//go:build e2e

package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestDB2E2E exercises the full DB2 → PG migration pipeline. It is opt-in
// because the DB2 sample container is on a docker-compose profile (db2),
// boots in ~4 min, and is gated behind IBM's Community License — running
// it in CI requires explicit acceptance of the EULA.
//
// Activate by setting SQUISHY_E2E_DB2=1 in the environment and starting
// the docker stack with the db2 profile (see scripts/run_e2e.sh).
func TestDB2E2E(t *testing.T) {
	if os.Getenv("SQUISHY_E2E_DB2") != "1" {
		t.Skip("SQUISHY_E2E_DB2 not set — DB2 e2e is opt-in (gated behind ICR EULA + 4min boot)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	waitReady(t, ctx)

	// Step 1: create project.
	project := postJSON(t, "/api/v1/projects", map[string]any{
		"name":        fmt.Sprintf("e2e-db2-%d", time.Now().Unix()),
		"description": "DB2 integration test",
	})
	projectID := project["id"].(string)
	t.Logf("project=%s", projectID)

	// Step 2: set target PG admin connection.
	putJSON(t, fmt.Sprintf("/api/v1/projects/%s/connection", projectID), map[string]any{
		"host": "postgres", "port": 5432,
		"database": "squishy", "username": "squishy", "password": "squishy", "ssl_mode": "disable",
	})
	tt := postJSON(t, fmt.Sprintf("/api/v1/projects/%s/connection/test", projectID), nil)
	require.True(t, tt["ok"].(bool), "target test: %v", tt["message"])

	// Pre-clean any leftover schema from a previous run.
	pool, err := pgxpool.New(ctx, pgAdminDSN)
	require.NoError(t, err)
	defer pool.Close()
	_, _ = pool.Exec(ctx, `DROP SCHEMA IF EXISTS squishy CASCADE`)

	// Step 3: create a DB2 source instance + run discovery.
	created := postJSON(t, fmt.Sprintf("/api/v1/projects/%s/instances", projectID), map[string]any{
		"name":              "db2-sample",
		"kind":              "db2",
		"host":              "db2-sample",
		"port":              50000,
		"database":          "SAMPLE",
		"username":          "db2inst1",
		"password":          "password",
		"ssl_mode":          "disable",
		"target_strategy":   "dedicated_schema",
		"target_default_db": "squishy",
	})
	if e, ok := created["discover_error"].(string); ok && e != "" {
		t.Fatalf("instance discovery failed: %s", e)
	}
	migrations := asSlice(created["migrations"])
	require.NotEmpty(t, migrations, "no draft migrations created — discovery returned empty schema list")

	var migrationID string
	for _, m := range migrations {
		mm := m.(map[string]any)
		if mm["source_schema_name"] == "SQUISHY" {
			migrationID = mm["id"].(string)
			break
		}
	}
	require.NotEmpty(t, migrationID, "no draft migration for SQUISHY schema in: %+v", migrations)
	t.Logf("migration=%s", migrationID)

	// Step 4: plan + inspect.
	insp := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/inspect", migrationID), nil)
	require.NotEmpty(t, insp["tables"])

	plan := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/plan", migrationID), map[string]any{
		"options": map[string]any{"batch_size": 10000},
	})
	require.NotEmpty(t, plan["ddl_script"])
	t.Logf("ddl_bytes=%d explanations=%d warnings=%d",
		len(plan["ddl_script"].(string)),
		countSlice(plan["explanations"]), countSlice(plan["warnings"]))

	// Step 5: start a run.
	start := postJSON(t, fmt.Sprintf("/api/v1/migrations/%s/runs", migrationID), nil)
	runID := start["run_id"].(string)
	t.Logf("run=%s", runID)

	// Step 6: poll until terminal status.
	var finalStatus string
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		r := getJSON(t, fmt.Sprintf("/api/v1/runs/%s", runID))
		finalStatus, _ = r["status"].(string)
		if finalStatus == "succeeded" || finalStatus == "failed" || finalStatus == "cancelled" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.Equal(t, "succeeded", finalStatus, "run did not succeed")

	// Step 7: row-count validation. The fixture seeds 5 customers, 5 orders,
	// 5 order lines, 5 countries — counts should match end-to-end.
	expectedCounts := map[string]int64{
		"countries":    5,
		"customers":    5,
		"orders":       5,
		"order_lines":  5,
	}
	for tbl, want := range expectedCounts {
		var got int64
		err := pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM squishy.%s`, tbl)).Scan(&got)
		require.NoError(t, err, "count squishy.%s", tbl)
		require.Equal(t, want, got, "row count mismatch for %s", tbl)
	}
}
