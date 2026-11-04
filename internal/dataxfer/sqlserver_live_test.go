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
	"strings"
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
		sd, err := d.(selectExprDialect).ForColumns(ctx, db, tb.s, tb.n, cols, nil)
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

// A table without primary key nor unique index (the fixture's
// system-versioned history table) is copied in one batch: no key orders
// its rows the same way in two OFFSET SELECTs.
func TestSQLServerLiveHeap(t *testing.T) {
	port, _ := strconv.Atoi(liveEnv("SQUISHY_SQLSERVER_PORT", "1433"))
	p := connection.Params{
		Kind: "sqlserver", Host: liveEnv("SQUISHY_SQLSERVER_HOST", "sqlserver-sample"), Port: port,
		Database: "squishy_sample", Username: "sa",
		Password: liveEnv("SQUISHY_SQLSERVER_PASSWORD", "Squishy_2026x"), SSLMode: "disable",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := connection.OpenSQLServer(ctx, p)
	require.NoError(t, err)
	defer db.Close()
	d := SQLServerSource()
	plan, err := BuildPartitionPlan(ctx, db, d, "dbo", "EmployeesHistory", 1)
	require.NoError(t, err)
	require.Equal(t, "offset", plan.RangeKind)
	require.Len(t, plan.Ranges, 1)
	require.Equal(t, plan.TotalRows, plan.Ranges[0].High["limit"])
	key, err := sourceOrderKey(ctx, db, d, "dbo", "EmployeesHistory")
	require.NoError(t, err)
	require.Empty(t, key)
	// A table with a primary key is unchanged.
	key, err = sourceOrderKey(ctx, db, d, "dbo", "Employees")
	require.NoError(t, err)
	require.NotEmpty(t, key)
}

// sql_variant values are read as the lossless text of their base type.
func TestSQLServerLiveVariant(t *testing.T) {
	port, _ := strconv.Atoi(liveEnv("SQUISHY_SQLSERVER_PORT", "1433"))
	p := connection.Params{
		Kind: "sqlserver", Host: liveEnv("SQUISHY_SQLSERVER_HOST", "sqlserver-sample"), Port: port,
		Database: "master", Username: "sa",
		Password: liveEnv("SQUISHY_SQLSERVER_PASSWORD", "Squishy_2026x"), SSLMode: "disable",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := connection.OpenSQLServer(ctx, p)
	require.NoError(t, err)
	defer db.Close()
	cases := []struct{ value, want string }{
		{"CAST('2024-05-01 13:45:12.347' AS datetime)", "2024-05-01T13:45:12.347"},
		{"CAST('2024-05-01 13:45:12.1234567' AS datetime2(7))", "2024-05-01T13:45:12.1234567"},
		{"CAST('2024-05-01 13:45:12 +02:00' AS datetimeoffset(0))", "2024-05-01T13:45:12+02:00"},
		{"CAST('2024-05-01' AS date)", "2024-05-01"},
		{"CAST(3.14159265358979 AS float)", "3.1415926535897900e+000"},
		{"CAST(12.3456 AS money)", "12.3456"},
		{"CAST(0x4142 AS varbinary(10))", "0x4142"},
		{"CAST(REPLICATE(CAST('x' AS varchar(max)), 6000) AS varchar(8000))", strings.Repeat("x", 6000)},
		{"CAST(N'été' AS nvarchar(10))", "été"},
		{"CAST('ab' AS char(4))", "ab  "},
		{"CAST(12.50 AS decimal(5,2))", "12.50"},
	}
	for _, c := range cases {
		var got string
		// A table, not a constant: SQL Server folds constants through
		// every CASE branch, which a column value never meets.
		q := "SET NOCOUNT ON; DECLARE @t TABLE ([x] sql_variant); INSERT INTO @t VALUES (CAST(" + c.value + " AS sql_variant)); SELECT " + sqlserverVariantText("[x]") + " FROM @t"
		require.NoError(t, db.QueryRowContext(ctx, q).Scan(&got), c.value)
		require.Equal(t, len(c.want), len(got), c.value)
		require.True(t, c.want == got, "%s: got %.80q", c.value, got)
	}
}
