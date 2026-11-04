package translate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
)

// lf normalises line endings (the checkout may convert both files).
func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// postgres-init/03-tsql-shim.sql is the concatenation of the embedded
// pieces. Regenerate it with:
//
//	cat internal/translate/tsql_shim/*.sql > postgres-init/03-tsql-shim.sql
func TestTSQLShim_InitFileMatchesPieces(t *testing.T) {
	b, err := os.ReadFile("../../postgres-init/03-tsql-shim.sql")
	require.NoError(t, err)
	require.Equal(t, lf(tsqlShimScript()), lf(string(b)),
		"postgres-init/03-tsql-shim.sql is out of date: cat internal/translate/tsql_shim/*.sql > postgres-init/03-tsql-shim.sql")
}

// Every function the rewriters may emit has a shim piece that creates it,
// and every piece is reachable.
func TestTSQLShim_FunctionsHavePieces(t *testing.T) {
	names := map[string]bool{}
	for _, f := range tsqlShimFileNames() {
		names[f] = true
	}
	used := map[string]bool{tsqlShimSchemaFile: true}
	for fn, files := range tsqlShimFuncs {
		require.NotEmpty(t, files, fn)
		own := files[len(files)-1]
		require.True(t, names[own], "%s: missing piece %s", fn, own)
		require.Contains(t, tsqlShimFile(own), "FUNCTION "+fn+"(", "%s is not created by %s", fn, own)
		for _, f := range files {
			require.True(t, names[f], "%s: missing piece %s", fn, f)
			used[f] = true
		}
	}
	for f := range names {
		require.True(t, used[f], "piece %s is never emitted", f)
	}
}

// The shim honours CLAUDE.md: no regex function, operator or SIMILAR TO.
func TestTSQLShim_NoRegex(t *testing.T) {
	script := strings.ToLower(tsqlShimScript())
	for _, bad := range []string{"regexp_", "similar to", "~"} {
		require.NotContains(t, script, bad)
	}
}

func TestTSQLShim_PreActions(t *testing.T) {
	require.Nil(t, tsqlShimPreActions(&HookRun{}))

	run := &HookRun{}
	run.Use("tsql.datename")
	run.Use("note:something else")
	pre := tsqlShimPreActions(run)
	require.Len(t, pre, 4)
	require.Contains(t, pre[0], "CREATE SCHEMA IF NOT EXISTS tsql;")
	require.Contains(t, pre[1], "FUNCTION tsql.datepart_canon(")
	require.Contains(t, pre[2], "FUNCTION tsql.datepart(")
	require.Contains(t, pre[3], "FUNCTION tsql.datename(")

	run = &HookRun{}
	run.Use("tsql.like")
	run.Use("tsql.convert_style")
	pre = tsqlShimPreActions(run)
	require.Len(t, pre, 3)
	require.Contains(t, pre[1], "FUNCTION tsql.like(")
	require.Contains(t, pre[2], "FUNCTION tsql.convert_style(")
}

// TestTSQLShim_PG loads the shim into the PostgreSQL named by
// SQUISHY_PG_TEST_DSN (skipped when unset) and checks its functions, and
// the PG text the rewriters produce for T-SQL expressions, against the
// values SQL Server returns. The shim is created in a transaction that is
// rolled back.
func TestTSQLShim_PG(t *testing.T) {
	dsn := os.Getenv("SQUISHY_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("SQUISHY_PG_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, "SET LOCAL TimeZone = 'UTC'")
	require.NoError(t, err)
	// The whole script at once (simple protocol: several statements).
	_, err = tx.Conn().PgConn().Exec(ctx, tsqlShimScript()).ReadAll()
	require.NoError(t, err)

	check := func(t *testing.T, expr, want string) {
		t.Helper()
		var got *string
		err := tx.QueryRow(ctx, "SELECT ("+expr+")::text").Scan(&got)
		require.NoError(t, err, expr)
		if want == "<null>" {
			require.Nil(t, got, expr)
			return
		}
		require.NotNil(t, got, expr)
		require.Equal(t, want, *got, expr)
	}

	shim := []struct{ expr, want string }{
		// like
		{`tsql.like('Abc', '[a-c]%')`, "true"},
		{`tsql.like('abc', '[^a]%')`, "false"},
		{`tsql.like('dbc', '[^a]%')`, "true"},
		{`tsql.like('b', '[a-c]')`, "true"},
		{`tsql.like('-', '[a-]')`, "true"},
		{`tsql.like('ab', 'a_')`, "true"},
		{`tsql.like('a', 'a_')`, "false"},
		{`tsql.like('a%b', 'a!%b', '!')`, "true"},
		{`tsql.like('axb', 'a!%b', '!')`, "false"},
		{`tsql.like('a_b', 'a\_b', '\')`, "true"},
		{`tsql.like('abc   ', 'abc')`, "true"},
		{`tsql.like('ABC', 'abc')`, "true"},
		{`tsql.like('a[b', 'a[b')`, "true"},
		{`tsql.like(']', '[]]')`, "true"},
		{`tsql.like('', '%')`, "true"},
		{`tsql.like('x', '')`, "false"},
		{`tsql.like('xay', '%a%')`, "true"},
		{`tsql.like('xyz', '%a%')`, "false"},
		{`tsql.like('aaab', '%ab')`, "true"},
		{`tsql.like('C:\dir', 'C:\d%')`, "true"},
		{`tsql.like(NULL, 'a')`, "<null>"},
		// dateadd
		{`tsql.dateadd('month', 1, timestamp '2024-01-31')`, "2024-02-29 00:00:00"},
		{`tsql.dateadd('yy', 1, date '2024-02-29')`, "2025-02-28"},
		{`tsql.dateadd('qq', 1, timestamp '2024-11-30 10:00')`, "2025-02-28 10:00:00"},
		{`tsql.dateadd('wk', -1, date '2024-01-08')`, "2024-01-01"},
		{`tsql.dateadd('ms', 1500, timestamp '2024-01-01 00:00:00')`, "2024-01-01 00:00:01.5"},
		{`tsql.dateadd('day', 1, timestamptz '2024-01-01 23:00+00')`, "2024-01-02 23:00:00+00"},
		// datediff: boundaries crossed
		{`tsql.datediff('year', timestamp '2024-12-31', timestamp '2025-01-01')`, "1"},
		{`tsql.datediff('year', timestamp '2024-01-01', timestamp '2024-12-31')`, "0"},
		{`tsql.datediff('quarter', date '2024-03-31', date '2024-04-01')`, "1"},
		{`tsql.datediff('month', date '2024-01-31', date '2024-02-01')`, "1"},
		{`tsql.datediff('month', date '2024-03-01', date '2024-01-31')`, "-2"},
		{`tsql.datediff('day', timestamp '2024-01-01 23:59', timestamp '2024-01-02 00:01')`, "1"},
		{`tsql.datediff('week', date '2024-01-06', date '2024-01-07')`, "1"},
		{`tsql.datediff('week', date '2024-01-07', date '2024-01-13')`, "0"},
		{`tsql.datediff('hour', timestamp '2024-01-01 10:59', timestamp '2024-01-01 11:00')`, "1"},
		{`tsql.datediff('minute', timestamp '2024-01-01 10:00:59', timestamp '2024-01-01 10:01:00')`, "1"},
		{`tsql.datediff('second', timestamp '2024-01-01 10:00:00.999', timestamp '2024-01-01 10:00:01')`, "1"},
		{`tsql.datediff('ms', timestamp '2024-01-01 10:00:00.0009', timestamp '2024-01-01 10:00:00.0011')`, "1"},
		{`tsql.datediff('day', timestamptz '2024-01-01 10:00+00', timestamptz '2024-01-03 09:00+00')`, "2"},
		// datepart / datename
		{`tsql.datepart('weekday', date '2024-01-07')`, "1"},
		{`tsql.datepart('dw', date '2024-01-06')`, "7"},
		{`tsql.datepart('week', date '2024-01-06')`, "1"},
		{`tsql.datepart('week', date '2024-01-07')`, "2"},
		{`tsql.datepart('wk', date '2023-12-31')`, "53"},
		{`tsql.datepart('dy', date '2024-12-31')`, "366"},
		{`tsql.datepart('iso_week', date '2024-12-30')`, "1"},
		{`tsql.datepart('ms', timestamp '2024-01-01 10:00:01.123')`, "123"},
		{`tsql.datepart('second', timestamp '2024-01-01 10:00:59.9')`, "59"},
		{`tsql.datepart('q', date '2024-08-01')`, "3"},
		{`tsql.datename('month', date '2024-03-05')`, "March"},
		{`tsql.datename('dw', date '2024-03-05')`, "Tuesday"},
		{`tsql.datename('year', timestamp '2024-03-05')`, "2024"},
		// isnumeric / isdate
		{`tsql.isnumeric('12.5')`, "1"},
		{`tsql.isnumeric(' -12 ')`, "1"},
		{`tsql.isnumeric('1e5')`, "1"},
		{`tsql.isnumeric('$12')`, "1"},
		{`tsql.isnumeric('1,000.25')`, "1"},
		{`tsql.isnumeric('-')`, "1"},
		{`tsql.isnumeric('abc')`, "0"},
		{`tsql.isnumeric('1.2.3')`, "0"},
		{`tsql.isnumeric('12a')`, "0"},
		{`tsql.isnumeric('')`, "0"},
		{`tsql.isnumeric(NULL)`, "0"},
		{`tsql.isdate('2024-02-29')`, "1"},
		{`tsql.isdate('2024-02-30')`, "0"},
		{`tsql.isdate('now')`, "0"},
		{`tsql.isdate('hello')`, "0"},
		{`tsql.isdate(NULL)`, "0"},
		// convert_style
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 101)`, "03/05/2024"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 103)`, "05/03/2024"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 104)`, "05.03.2024"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 105)`, "05-03-2024"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 110)`, "03-05-2024"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 111)`, "2024/03/05"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 112)`, "20240305"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 120)`, "2024-03-05 14:07:09"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 121)`, "2024-03-05 14:07:09.123"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09.123', 126)`, "2024-03-05T14:07:09.123"},
		{`tsql.convert_style(timestamp '2024-03-05 14:07:09', 126)`, "2024-03-05T14:07:09"},
		{`tsql.convert_style(date '2024-03-05', 112)`, "20240305"},
		{`tsql.convert_style(timestamptz '2024-03-05 14:07:09+00', 120)`, "2024-03-05 14:07:09"},
		// hierarchyid over its canonical path
		{`tsql.hierarchyid_getlevel('/')`, "0"},
		{`tsql.hierarchyid_getlevel('/1/3/')`, "2"},
		{`tsql.hierarchyid_getlevel('/2.1/')`, "1"},
		{`tsql.hierarchyid_getancestor('/1/3/', 1)`, "/1/"},
		{`tsql.hierarchyid_getancestor('/1/3/', 2)`, "/"},
		{`coalesce(tsql.hierarchyid_getancestor('/1/3/', 3), 'null')`, "null"},
		{`tsql.hierarchyid_isdescendantof('/1/3/', '/1/')`, "true"},
		{`tsql.hierarchyid_isdescendantof('/1/', '/1/')`, "true"},
		{`tsql.hierarchyid_isdescendantof('/10/', '/1/')`, "false"},
		{`tsql.hierarchyid_getreparentedvalue('/1/3/4/', '/1/3/', '/2/')`, "/2/4/"},
		{`tsql.hierarchyid_getdescendant('/1/', NULL, NULL)`, "/1/1/"},
		{`tsql.hierarchyid_getdescendant('/1/', '/1/3/', NULL)`, "/1/4/"},
		{`tsql.hierarchyid_getdescendant('/1/', '/1/3/', '/1/7/')`, "/1/4/"},
		// error context of a CATCH block (published, read, restored)
		{`coalesce(tsql.error_field('message'), 'null')`, "null"},
		{`(SELECT tsql.error_field('message') FROM (SELECT tsql.error_enter('23505', 'dup key') AS prev) p)`, "dup key"},
		{`tsql.error_number_of('23505')`, "2627"},
		{`tsql.error_number_of('P0001')`, "50000"},
		// CONVERT(datetime, s, style): DateStyle-ordered parsing
		{`tsql.str_to_timestamp_mdy('02/03/2004')`, "2004-02-03 00:00:00"},
		{`tsql.str_to_timestamp_dmy('02/03/2004')`, "2004-03-02 00:00:00"},
		{`tsql.str_to_timestamp_mdy('2004-02-21')`, "2004-02-21 00:00:00"},
	}
	for _, c := range shim {
		check(t, c.expr, c.want)
	}

	// T-SQL expressions through the rewriters: the PG text runs and
	// gives SQL Server's result.
	exprs := []struct{ tsql, want string }{
		{"CHARINDEX('b', 'abcb')", "2"},
		{"CHARINDEX('b', 'abcb', 3)", "4"},
		{"CHARINDEX('z', 'abcb', 3)", "0"},
		{"STUFF('abcdef', 2, 3, 'XY')", "aXYef"},
		{"STUFF('abcdef', 2, 3, NULL)", "aef"},
		{"LEN('ab  ')", "2"},
		{"LEN(12345)", "5"},
		{"DATALENGTH('abc')", "3"},
		{"REPLICATE('ab', 3)", "ababab"},
		{"'[' + SPACE(2) + ']'", "[  ]"},
		{"DATEDIFF(day, '2024-01-01', '2024-03-01')", "60"},
		{"DATEADD(month, 1, CAST('2024-01-31' AS date))", "2024-02-29"},
		{"DATEPART(weekday, '2024-01-07')", "1"},
		{"DATENAME(month, '2024-03-05')", "March"},
		{"YEAR('2024-03-05')", "2024"},
		{"EOMONTH('2024-02-10')", "2024-02-29"},
		{"EOMONTH('2024-01-31', 1)", "2024-02-29"},
		{"IIF(1 > 0, 'y', 'n')", "y"},
		{"CHOOSE(2, 'a', 'b', 'c')", "b"},
		{"'a' + 'b' + N'c'", "abc"},
		{"ISNULL(NULL, 5)", "5"},
		{"CONVERT(varchar(10), CAST('2024-03-05 10:11:12' AS datetime), 103)", "05/03/2024"},
		{"CONVERT(varchar(8), CAST('2024-03-05' AS datetime), 112)", "20240305"},
		{"CONVERT(datetime, '05/03/2024', 103)", "2024-03-05 00:00:00"},
		{"CONVERT(datetime, '2004-02-21', 101)", "2004-02-21 00:00:00"},
		{"CONVERT(datetime, '02/03/2004', 101)", "2004-02-03 00:00:00"},
		{"CONVERT(datetime, '2/3/2004 10:30', 101)", "2004-02-03 10:30:00"},
		{"CONVERT(datetime, '21.02.2004', 104)", "2004-02-21 00:00:00"},
		{"CONVERT(datetime, '20040203', 103)", "2004-02-03 00:00:00"},
		{"CAST(12.5 AS nvarchar)", "12.5"},
		{"5 ^ 3", "6"},
		{"~5", "-6"},
		{"0x0F & 7", "7"},
		{"STR(3.14159, 6, 2)", "  3.14"},
		{"ROUND(3.789, 1, 1)", "3.7"},
		{"SQUARE(3)", "9"},
		{"ISNUMERIC('12')", "1"},
		{"ISDATE('2024-13-01')", "0"},
		{"'x' LIKE '[a-z]'", "true"},
		{"'X' NOT LIKE '[^a-z]'", "true"},
		{"SUSER_SNAME() IS NOT NULL", "true"},
		{"SESSION_USER IS NOT NULL", "true"},
		{"HOST_NAME() IS NULL OR HOST_NAME() IS NOT NULL", "true"},
		{"DB_NAME() IS NOT NULL", "true"},
		{"GETUTCDATE() IS NOT NULL", "true"},
		{"LEFT(12345, 2)", "12"},
		{"UPPER(N'abc') + 'd'", "ABCd"},
		{"QUOTENAME('a b')", `"a b"`},
		{"hierarchyid::GetRoot()", "/"},
		{"CAST('/1/2/' AS varchar(20))", "/1/2/"},
		{`CAST('<r xmlns="urn:x"><a>7</a></r>' AS xml).value('declare default element namespace "urn:x"; (/r/a)[1]', 'int') + 1`, "8"},
		{`CAST('<r><a b="x"/></r>' AS xml).exist('/r/a[@b="x"]')`, "true"},
		{`CAST('<r><a b="x"/></r>' AS xml).exist('/r/c')`, "false"},
		// value() truncates to the declared length, as SQL Server does.
		{`CAST('<a>abcdef</a>' AS xml).value('(/a)[1]', 'nvarchar(3)')`, "abc"},
	}
	for _, c := range exprs {
		e, errs := tsqldialect.ParseExpr(c.tsql)
		require.Empty(t, errs, c.tsql)
		out, _ := rewriteTSQLExpr(e, nil)
		check(t, pgast.WriteExpr(out), c.want)
	}
}
