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
	apiURL    = getenv("SQUISHY_API_URL", "http://api:8080")
	pgAdminDSN = getenv("TARGET_PG_DSN", "postgres://squishy:squishy@postgres:5432/squishy?sslmode=disable")
	mysqlDSN  = getenv("SQUISHY_MYSQL_DSN", "sakila:sakila@tcp(mysql-sample:3306)/sakila?parseTime=true&multiStatements=true")
	fixtures  = []string{"customers", "orders", "order_items", "t_numeric", "t_string", "t_temporal", "t_defaults", "t_identity", "t_check"}
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

	// Step 1: create project.
	project := postJSON(t, "/api/v1/projects", map[string]any{
		"name":        fmt.Sprintf("e2e-%d", time.Now().Unix()),
		"description": "integration test",
	})
	projectID := project["id"].(string)
	t.Logf("project=%s", projectID)

	// Step 2: set the project's target PG admin connection. We point at the
	// app DB ("squishy") and the instance below uses dedicated_schema, so all
	// tables land in squishy.<source_schema_name>.
	putJSON(t, fmt.Sprintf("/api/v1/projects/%s/connection", projectID), map[string]any{
		"host": "postgres", "port": 5432,
		"database": "squishy", "username": "squishy", "password": "squishy", "ssl_mode": "disable",
	})
	tt := postJSON(t, fmt.Sprintf("/api/v1/projects/%s/connection/test", projectID), nil)
	require.True(t, tt["ok"].(bool), "target test: %v", tt["message"])

	// Pre-clean: drop the target schema if it lingered from a previous run.
	pool, err := pgxpool.New(ctx, pgAdminDSN)
	require.NoError(t, err)
	defer pool.Close()
	_, _ = pool.Exec(ctx, `DROP SCHEMA IF EXISTS sakila CASCADE`)

	// Step 3: create a source instance pointing at mysql-sample. Discovery
	// runs synchronously and returns one draft migration per non-system
	// schema. We pick the strategy "dedicated_schema" so all schemas land in
	// the project's target_default_db (squishy here) under a PG schema named
	// after the source.
	created := postJSON(t, fmt.Sprintf("/api/v1/projects/%s/instances", projectID), map[string]any{
		"name":              "mysql-sample",
		"kind":              "mysql",
		"host":              "mysql-sample",
		"port":              3306,
		"database":          "sakila",
		"username":          "sakila",
		"password":          "sakila",
		"ssl_mode":          "disable",
		"target_strategy":   "dedicated_schema",
		"target_default_db": "squishy",
	})
	if e, ok := created["discover_error"].(string); ok && e != "" {
		t.Fatalf("instance discovery failed: %s", e)
	}
	migrations := asSlice(created["migrations"])
	require.NotEmpty(t, migrations, "no draft migrations created")

	// Pick the migration whose source_schema_name == "sakila".
	var migrationID string
	for _, m := range migrations {
		mm := m.(map[string]any)
		if mm["source_schema_name"] == "sakila" {
			migrationID = mm["id"].(string)
			break
		}
	}
	require.NotEmpty(t, migrationID, "no draft migration for sakila found in: %+v", migrations)
	t.Logf("migration=%s", migrationID)

	// Step 4: plan + inspect.
	insp := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/inspect", migrationID), nil)
	require.NotEmpty(t, insp["tables"])

	plan := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/plan", migrationID), map[string]any{
		"options": map[string]any{"batch_size": 10000},
	})
	require.NotEmpty(t, plan["ddl_script"])
	require.NotEmpty(t, plan["explanations"])
	t.Logf("ddl_bytes=%d explanations=%d warnings=%d",
		len(plan["ddl_script"].(string)),
		countSlice(plan["explanations"]), countSlice(plan["warnings"]))

	// Step 5: start a run.
	start := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/runs", migrationID), nil)
	runID := start["run_id"].(string)
	t.Logf("run=%s", runID)

	// Step 6: poll until the run terminates.
	var finalStatus string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		r := getJSON(t, fmt.Sprintf("/api/v1/runs/%s", runID))
		finalStatus, _ = r["status"].(string)
		if finalStatus == "succeeded" || finalStatus == "failed" || finalStatus == "cancelled" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	require.Equal(t, "succeeded", finalStatus, "run did not succeed")

	// Step 7: validate row counts for a few tables in the target schema.
	mydb, err := sql.Open("mysql", mysqlDSN)
	require.NoError(t, err)
	defer mydb.Close()

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
func putJSON(t *testing.T, path string, body any) map[string]any {
	return doJSON(t, http.MethodPut, path, body)
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
