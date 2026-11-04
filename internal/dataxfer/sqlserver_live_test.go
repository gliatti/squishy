//go:build sqlserver_live

package dataxfer

// Live check of the SQL Server copy-side queries and value coercions
// against the compose sample (`make sqlserver-up`). Opt-in, never part
// of `make test`:
//
//	docker compose --profile test run --rm unit-tests \
//	  go test -tags sqlserver_live ./internal/dataxfer/ -run TestSQLServerLive -v
//
// SQUISHY_SQLSERVER_HOST / _PORT / _PASSWORD override the sample defaults;
// SQUISHY_SQLSERVER_DB picks the database (default AdventureWorks).

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/connection"
)

func liveEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestSQLServerLive(t *testing.T) {
	port, _ := strconv.Atoi(liveEnv("SQUISHY_SQLSERVER_PORT", "1433"))
	p := connection.Params{
		Kind: "sqlserver", Host: liveEnv("SQUISHY_SQLSERVER_HOST", "sqlserver-sample"), Port: port,
		Database: liveEnv("SQUISHY_SQLSERVER_DB", "AdventureWorks"), Username: "sa",
		Password: liveEnv("SQUISHY_SQLSERVER_PASSWORD", "Squishy_2026x"), SSLMode: "disable",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := connection.OpenSQLServer(ctx, p)
	require.NoError(t, err)
	defer db.Close()

	// Driver values → coerce(): what go-mssqldb really hands back.
	var g, m, dto, bt, tm, dt2 any
	require.NoError(t, db.QueryRowContext(ctx, `SELECT
		CAST('6F9619FF-8B86-D011-B42D-00C04FC964FF' AS uniqueidentifier),
		CAST(12.34 AS money),
		CAST('2026-11-04 10:30:00 +02:00' AS datetimeoffset),
		CAST(1 AS bit),
		CAST('13:45:10.1234567' AS time(7)),
		CAST('2026-11-04 10:30:00.1234567' AS datetime2(7))`).Scan(&g, &m, &dto, &bt, &tm, &dt2))
	t.Logf("driver types: guid %T, money %T(%v), dto %T(%v), bit %T, time %T(%v), datetime2 %T(%v)",
		g, m, m, dto, dto, bt, tm, tm, dt2, dt2)
	require.Equal(t, "6f9619ff-8b86-d011-b42d-00c04fc964ff", coerceFrom("sqlserver", g, "uuid"))
	require.Equal(t, "12.3400", coerceFrom("sqlserver", m, "numeric"))
	require.Equal(t, true, coerceFrom("sqlserver", bt, "bool"))
	ts, ok := dto.(time.Time)
	require.True(t, ok)
	require.Equal(t, time.Date(2026, 11, 4, 8, 30, 0, 0, time.UTC), ts.UTC())

	// Every table's first batch SELECT runs as the copier builds it.
	rows, err := db.QueryContext(ctx, `
		SELECT TABLE_SCHEMA, TABLE_NAME FROM INFORMATION_SCHEMA.TABLES
		 WHERE TABLE_TYPE = 'BASE TABLE' ORDER BY 1, 2`)
	require.NoError(t, err)
	type tbl struct{ s, n string }
	var tables []tbl
	for rows.Next() {
		var x tbl
		require.NoError(t, rows.Scan(&x.s, &x.n))
		tables = append(tables, x)
	}
	rows.Close()
	d := SQLServerSource()
	for _, tb := range tables {
		plan, err := BuildPartitionPlan(ctx, db, d, tb.s, tb.n, 500)
		require.NoError(t, err, tb.s+"."+tb.n)
		if len(plan.Ranges) == 0 {
			continue
		}
		cols, err := listSourceColumns(ctx, db, d, tb.s, tb.n)
		require.NoError(t, err)
		o := CopyOpts{SourceDB: db, SourceDialect: d, SrcSchema: tb.s, SrcTable: tb.n,
			Columns: cols, RangeKind: plan.RangeKind, Low: plan.Ranges[0].Low, High: plan.Ranges[0].High}
		sd, err := d.(selectExprDialect).ForColumns(ctx, db, tb.s, tb.n, cols)
		require.NoError(t, err)
		o.SourceDialect = sd
		r, err := o.selectSource(ctx)
		require.NoError(t, err, tb.s+"."+tb.n)
		n := 0
		for r.Next() {
			n++
		}
		require.NoError(t, r.Err())
		r.Close()
		require.Positive(t, n, tb.s+"."+tb.n)
		t.Logf("%s.%s: %s, first batch %d rows, %d cols", tb.s, tb.n, plan.RangeKind, n, len(cols))
	}
}
