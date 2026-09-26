//go:build e2e

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

var (
	apiURL     = getenv("SQUISHY_API_URL", "http://api:8080")
	apiToken   = os.Getenv("SQUISHY_API_TOKEN")
	pgAdminDSN = getenv("TARGET_PG_DSN", "postgres://squishy:squishy@postgres:5432/squishy?sslmode=disable")
	mysqlDSN   = getenv("SQUISHY_MYSQL_DSN", "sakila:sakila@tcp(mysql-sample:3306)/sakila?parseTime=true&multiStatements=true")
	fixtures   = []string{"customers", "orders", "order_items", "t_numeric", "t_string", "t_temporal", "t_defaults", "t_identity", "t_check"}
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestFullMigrationE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	waitReady(t, ctx)

	// The migrated schema lands in its own PG database (created by squishy
	// through target_create_db) under a schema named after the source, so
	// the run never shares a database with the app schema.
	const targetDB = "e2e_mysql"
	resetTargetDB(t, ctx, targetDB)

	migrationID := setupMigration(t, sourceSpec{
		ProjectName:  fmt.Sprintf("e2e-%d", time.Now().Unix()),
		InstanceName: "mysql-sample",
		Kind:         "mysql",
		Host:         "mysql-sample",
		Port:         3306,
		Database:     "sakila",
		Username:     "sakila",
		Password:     "sakila",
		SourceSchema: "sakila",
		TargetDB:     targetDB,
	})

	plan := planAndAck(t, migrationID)
	require.NotEmpty(t, plan["explanations"])

	runID := runToCompletion(t, migrationID, 5*time.Minute)
	checkBatchesEndpoint(t, ctx, runID)

	// Validate row counts for the fixture tables in the target schema.
	mydb, err := sql.Open("mysql", mysqlDSN)
	require.NoError(t, err)
	defer mydb.Close()

	pool := openTargetPool(t, ctx, targetDB)
	defer pool.Close()

	for _, tbl := range fixtures {
		var src, tgt int64
		require.NoError(t, mydb.QueryRowContext(ctx,
			fmt.Sprintf("SELECT count(*) FROM `%s`", tbl)).Scan(&src))
		err := pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM sakila.%q`, tbl)).Scan(&tgt)
		if err != nil {
			t.Errorf("%s: target query failed: %v", tbl, err)
			continue
		}
		require.Equal(t, src, tgt, "row count mismatch on %s", tbl)
		t.Logf("%s: %d rows OK", tbl, src)
	}
}

// ----- scenario helpers (shared with the opt-in DB2 scenario) -----

// sourceSpec describes one source instance and the PG database its
// migration targets (dedicated_schema strategy: one PG schema per source
// schema inside TargetDB).
type sourceSpec struct {
	ProjectName  string
	InstanceName string
	Kind         string
	Host         string
	Port         int
	Database     string
	Username     string
	Password     string
	SourceSchema string // discovered schema to migrate
	TargetDB     string
}

// setupMigration mirrors the wizard / MCP flow: create a project, create
// an instance carrying both the source DSN and the target PG DSN (discovery
// drafts one migration per source schema), probe both connections, and
// return the draft migration of spec.SourceSchema.
func setupMigration(t *testing.T, spec sourceSpec) string {
	t.Helper()
	project := postJSON(t, "/api/v1/projects", map[string]any{
		"name":        spec.ProjectName,
		"description": "integration test",
	})
	projectID := project["id"].(string)
	t.Logf("project=%s", projectID)

	created := postJSON(t, fmt.Sprintf("/api/v1/projects/%s/instances", projectID), map[string]any{
		"name":             spec.InstanceName,
		"kind":             spec.Kind,
		"host":             spec.Host,
		"port":             spec.Port,
		"database":         spec.Database,
		"username":         spec.Username,
		"password":         spec.Password,
		"ssl_mode":         "disable",
		"target_strategy":  "dedicated_schema",
		"target_host":      "postgres",
		"target_port":      5432,
		"target_database":  spec.TargetDB,
		"target_username":  "squishy",
		"target_password":  "squishy",
		"target_ssl_mode":  "disable",
		"target_create_db": true,
	})
	if e, ok := created["target_create_db_error"].(string); ok && e != "" {
		t.Fatalf("target database creation failed: %s", e)
	}
	if e, ok := created["discover_error"].(string); ok && e != "" {
		t.Fatalf("instance discovery failed: %s", e)
	}
	instanceID := created["instance"].(map[string]any)["id"].(string)
	t.Logf("instance=%s", instanceID)

	src := postJSON(t, fmt.Sprintf("/api/v1/instances/%s/test-connection", instanceID), nil)
	require.Equal(t, true, src["ok"], "source test: %v", src["message"])
	tgt := postJSON(t, fmt.Sprintf("/api/v1/instances/%s/test-target-connection", instanceID), nil)
	require.Equal(t, true, tgt["ok"], "target test: %v", tgt["message"])

	migrations := asSlice(created["migrations"])
	require.NotEmpty(t, migrations, "no draft migrations created")
	for _, m := range migrations {
		mm := m.(map[string]any)
		if mm["source_schema_name"] == spec.SourceSchema {
			id := mm["id"].(string)
			t.Logf("migration=%s (%s -> %v.%v)", id, spec.SourceSchema, mm["target_db_name"], mm["target_schema_name"])
			return id
		}
	}
	t.Fatalf("no draft migration for source schema %q in: %+v", spec.SourceSchema, migrations)
	return ""
}

// planAndAck inspects + plans the migration and acknowledges every
// prerequisite the plan reports, as the wizard checklist step does. It
// returns the plan response.
func planAndAck(t *testing.T, migrationID string) map[string]any {
	t.Helper()
	insp := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/inspect", migrationID), nil)
	require.NotEmpty(t, insp["tables"])

	plan := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/plan", migrationID), map[string]any{
		"options": map[string]any{"batch_size": 10000},
	})
	require.NotEmpty(t, plan["ddl_script"])
	t.Logf("ddl_bytes=%d explanations=%d warnings=%d prerequisites=%d",
		len(plan["ddl_script"].(string)),
		countSlice(plan["explanations"]), countSlice(plan["warnings"]),
		countSlice(plan["prerequisites"]))

	pre := getJSON(t, fmt.Sprintf("/api/v1/migrations/%s/prerequisites", migrationID))
	acked := []string{}
	for _, p := range asSlice(pre["prerequisites"]) {
		pm := p.(map[string]any)
		t.Logf("prerequisite %v [%v]: %v", pm["id"], pm["severity"], pm["title"])
		acked = append(acked, pm["id"].(string))
	}
	postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/prerequisites/ack", migrationID), map[string]any{"acked": acked})
	pre = getJSON(t, fmt.Sprintf("/api/v1/migrations/%s/prerequisites", migrationID))
	require.Equal(t, true, pre["can_launch"], "prerequisites still blocking: %+v", pre)
	return plan
}

// runToCompletion starts an auto-mode run and waits for a terminal status,
// logging the non-succeeded steps when the run does not succeed.
func runToCompletion(t *testing.T, migrationID string, timeout time.Duration) string {
	t.Helper()
	start := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/runs", migrationID), nil)
	runID := start["run_id"].(string)
	t.Logf("run=%s", runID)

	var finalStatus string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := getJSON(t, fmt.Sprintf("/api/v1/runs/%s", runID))
		finalStatus, _ = r["status"].(string)
		if finalStatus == "succeeded" || finalStatus == "failed" || finalStatus == "cancelled" {
			t.Logf("run %s: %v", finalStatus, r)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if finalStatus != "succeeded" {
		steps := getJSON(t, fmt.Sprintf("/api/v1/runs/%s/steps", runID))
		for _, s := range asSlice(steps["steps"]) {
			sm := s.(map[string]any)
			if sm["status"] != "succeeded" {
				t.Logf("step %v %v [%v]: %v %v", sm["kind"], sm["target"], sm["status"], sm["error"], sm["last_error"])
			}
		}
	}
	require.Equal(t, "succeeded", finalStatus, "run did not succeed")
	return runID
}

// checkBatchesEndpoint lists the batches of every copy_table step, then
// puts one batch back in its pending shape (row_count / row_count_est
// NULL, as before the copy sizes and fills it) and checks the endpoint
// still answers 200 with 0 instead of failing to scan the NULLs.
func checkBatchesEndpoint(t *testing.T, ctx context.Context, runID string) {
	t.Helper()
	var batchStep string
	for _, s := range asSlice(getJSON(t, fmt.Sprintf("/api/v1/runs/%s/steps", runID))["steps"]) {
		sm := s.(map[string]any)
		if sm["kind"] != "copy_table" {
			continue
		}
		stepID := sm["id"].(string)
		if countSlice(getJSON(t, fmt.Sprintf("/api/v1/runs/%s/steps/%s/batches", runID, stepID))["batches"]) > 0 {
			batchStep = stepID
		}
	}
	require.NotEmpty(t, batchStep, "no copy_table step with batches")

	pool, err := pgxpool.New(ctx, pgAdminDSN)
	require.NoError(t, err)
	defer pool.Close()
	var batchID string
	var rowCount, rowEst *int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT id::text, row_count, row_count_est FROM squishy.step_batches
		 WHERE step_id=$1 ORDER BY seq LIMIT 1`, batchStep).Scan(&batchID, &rowCount, &rowEst))
	_, err = pool.Exec(ctx, `UPDATE squishy.step_batches SET row_count=NULL, row_count_est=NULL WHERE id=$1`, batchID)
	require.NoError(t, err)
	defer func() {
		_, _ = pool.Exec(ctx, `UPDATE squishy.step_batches SET row_count=$2, row_count_est=$3 WHERE id=$1`, batchID, rowCount, rowEst)
	}()

	for _, b := range asSlice(getJSON(t, fmt.Sprintf("/api/v1/runs/%s/steps/%s/batches", runID, batchStep))["batches"]) {
		bm := b.(map[string]any)
		if bm["id"] == batchID {
			require.EqualValues(t, 0, bm["row_count"])
			require.EqualValues(t, 0, bm["row_count_est"])
			return
		}
	}
	t.Fatalf("batch %s missing from listBatches", batchID)
}

// resetTargetDB drops the target database left by a previous run so
// squishy recreates it from scratch (target_create_db).
func resetTargetDB(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, pgAdminDSN)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	require.NoError(t, err)
}

// openTargetPool connects to the migrated database with the admin creds.
func openTargetPool(t *testing.T, ctx context.Context, name string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pgAdminDSN)
	require.NoError(t, err)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	return pool
}

// ----- HTTP helpers -----

func waitReady(t *testing.T, ctx context.Context) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(apiURL + "/readyz")
		if err == nil && res.StatusCode == 200 {
			res.Body.Close()
			return
		}
		if res != nil {
			res.Body.Close()
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatal("api never became ready")
}

func postJSON(t *testing.T, path string, body any) map[string]any {
	return doJSON(t, http.MethodPost, path, body)
}
func getJSON(t *testing.T, path string) map[string]any {
	return doJSON(t, http.MethodGet, path, nil)
}

func doJSON(t *testing.T, method, path string, body any) map[string]any {
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, apiURL+path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+apiToken)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "%s %s", method, path)
	defer res.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		t.Fatalf("%s %s → %d: %s", method, path, res.StatusCode, string(raw))
	}
	if strings.TrimSpace(string(raw)) == "" {
		return out
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func countSlice(v any) int {
	if s, ok := v.([]any); ok {
		return len(s)
	}
	return 0
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}
