//go:build e2e

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/require"
)

// sqlserverDSN is the go-mssqldb DSN of the fixture database the
// sqlserver-init service loads (test/fixtures/sqlserver_{sample,seed}.sql).
var sqlserverDSN = getenv("SQUISHY_SQLSERVER_DSN", "sqlserver://sa:Squishy_2026x@sqlserver-sample:1433?database=squishy_sample")

// sqlserverResidualWarnings are the non-info warning kinds the fixture
// still raises, each a construct without a faithful automatic
// translation (the plan acknowledges them, like the wizard checklist):
//   - routine.untranslated_construct: usp_count_customers runs EXEC
//     (@msg), a string built at run time;
//   - table.computed_not_generated: [Order Details].line_total converts
//     float arithmetic to money, not proven identical on PostgreSQL;
//   - type.spatial_no_postgis: only when the target cluster has no
//     PostGIS to install in the target database.
var sqlserverResidualWarnings = map[string]bool{
	"routine.untranslated_construct": true,
	"table.computed_not_generated":   true,
	"type.spatial_no_postgis":        true,
}

// TestSQLServerE2E migrates the squishy_sample fixture (schemas dbo and
// sales, every T-SQL feature the dialect covers) end to end through the
// API — instance kind sqlserver, dedicated_db strategy, plan, ack, run —
// then compares the row count of every fixture table between SQL Server
// and PostgreSQL, exercises the emulated temporal table and the exact
// UPDATE(col) trigger on the target, and runs the T-SQL shim test
// (internal/translate TestTSQLShim_PG) against the e2e PostgreSQL.
//
// Opt-in like the DB2 scenario: SQUISHY_E2E_SQLSERVER=1, with the
// sqlserver profile started (scripts/run_e2e.sh does both).
func TestSQLServerE2E(t *testing.T) {
	if os.Getenv("SQUISHY_E2E_SQLSERVER") != "1" {
		t.Skip("SQUISHY_E2E_SQLSERVER not set — SQL Server e2e is opt-in (2.3 GB image, sqlserver profile)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	waitReady(t, ctx)

	// dedicated_db: the database squishy_sample gets the T-SQL schemas
	// dbo and sales. Recreate it, with PostGIS when the cluster has it
	// (the remediation of the spatial prerequisite, done before the plan
	// so its extension probe sees it).
	const targetDB = "squishy_sample"
	resetTargetDB(t, ctx, targetDB)
	admin := openTargetPool(t, ctx, "squishy")
	defer admin.Close()
	var postgis bool
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'postgis')`).Scan(&postgis))
	if postgis {
		_, err := admin.Exec(ctx, `CREATE DATABASE `+targetDB)
		require.NoError(t, err)
		tp := openTargetPool(t, ctx, targetDB)
		_, err = tp.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS postgis`)
		tp.Close()
		require.NoError(t, err)
	}
	t.Logf("postgis available: %v", postgis)

	migrationID := setupMigration(t, sourceSpec{
		ProjectName:  fmt.Sprintf("e2e-sqlserver-%d", time.Now().Unix()),
		InstanceName: "sqlserver-sample",
		Kind:         "sqlserver",
		Host:         "sqlserver-sample",
		Port:         1433,
		Database:     "master",
		Username:     "sa",
		Password:     "Squishy_2026x",
		SourceSchema: "squishy_sample",
		TargetDB:     "squishy",
		Strategy:     "dedicated_db",
	})

	plan := planAndAck(t, migrationID)
	for _, w := range asSlice(plan["warnings"]) {
		wm := w.(map[string]any)
		if wm["severity"] == "info" {
			continue
		}
		kind, _ := wm["kind"].(string)
		t.Logf("warning %v on %v: %v", kind, wm["object"], wm["message"])
		require.True(t, sqlserverResidualWarnings[kind], "unexpected non-info warning %s on %v: %v", kind, wm["object"], wm["message"])
		if postgis {
			require.NotEqual(t, "type.spatial_no_postgis", kind, "PostGIS is installed in the target database")
		}
	}

	runToCompletion(t, migrationID, 6*time.Minute)

	ms, err := sql.Open("sqlserver", sqlserverDSN)
	require.NoError(t, err)
	defer ms.Close()
	pool := openTargetPool(t, ctx, targetDB)
	defer pool.Close()

	// Every base table of the fixture, the system-versioned history
	// table included, lands in its (lower-cased) schema with the same
	// rows.
	rows, err := ms.QueryContext(ctx, `
		SELECT TABLE_SCHEMA, TABLE_NAME FROM INFORMATION_SCHEMA.TABLES
		 WHERE TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_SCHEMA, TABLE_NAME`)
	require.NoError(t, err)
	type table struct{ schema, name string }
	var tables []table
	for rows.Next() {
		var tb table
		require.NoError(t, rows.Scan(&tb.schema, &tb.name))
		tables = append(tables, tb)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.GreaterOrEqual(t, len(tables), 13, "fixture tables: %v", tables)
	for _, tb := range tables {
		var src, tgt int64
		require.NoError(t, ms.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT_BIG(*) FROM [%s].[%s]", tb.schema, tb.name)).Scan(&src))
		err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q.%q`,
			strings.ToLower(tb.schema), strings.ToLower(tb.name))).Scan(&tgt)
		require.NoError(t, err, "%s.%s on PG", tb.schema, tb.name)
		require.Equal(t, src, tgt, "row count mismatch on %s.%s", tb.schema, tb.name)
		t.Logf("%s.%s: %d rows OK", tb.schema, tb.name, src)
	}

	// Behaviour on the target, in a transaction rolled back at the end.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	count := func(q string) int64 {
		var n int64
		require.NoError(t, tx.QueryRow(ctx, q).Scan(&n), q)
		return n
	}
	run := func(q string) {
		_, err := tx.Exec(ctx, q)
		require.NoError(t, err, q)
	}
	// System versioning: an UPDATE archives the previous version.
	hist := count(`SELECT count(*) FROM dbo.employeeshistory`)
	run(`UPDATE dbo.employees SET salary = salary + 1 WHERE employee_id = (SELECT min(employee_id) FROM dbo.employees)`)
	require.Equal(t, hist+1, count(`SELECT count(*) FROM dbo.employeeshistory`))
	// UPDATE(status): only an UPDATE whose SET list names status runs
	// the audit branch, which logs changed values.
	changes := func() int64 {
		return count(`SELECT count(*) FROM dbo.audit_log WHERE event_name = 'status_changed'`)
	}
	before := changes()
	run(`UPDATE sales.orders SET total = total WHERE order_id = (SELECT min(order_id) FROM sales.orders)`)
	require.Equal(t, before, changes(), "status not in the SET list")
	run(`UPDATE sales.orders SET status = CASE status WHEN 'new' THEN 'paid' ELSE 'new' END WHERE order_id = (SELECT min(order_id) FROM sales.orders)`)
	require.Equal(t, before+1, changes(), "status changed")

	// The T-SQL shim test (internal/translate, skipped without a DSN)
	// against this PostgreSQL: every shim function and rewritten
	// expression compared with SQL Server's results. It creates the shim
	// in a transaction it rolls back.
	cmd := exec.CommandContext(ctx, "go", "test", "./internal/translate/", "-run", "TestTSQLShim_PG", "-count=1", "-v")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "SQUISHY_PG_TEST_DSN="+pgAdminDSN)
	out, err := cmd.CombinedOutput()
	t.Logf("shim test:\n%s", out)
	require.NoError(t, err, "TestTSQLShim_PG")
	require.Contains(t, string(out), "--- PASS: TestTSQLShim_PG")
}
