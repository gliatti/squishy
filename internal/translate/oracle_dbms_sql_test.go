package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// DBMS_METADATA emits orafce-shipped package calls with double-quoted,
// uppercase identifiers (`"DBMS_SQL"."PARSE"`). PG matches quoted names
// case-sensitively, so the lowercase orafce schema is invisible to those
// calls. Strip the quotes and lowercase the package + method so PG
// resolves the orafce overload.
func TestRewriteOracleQuotedPackageCalls(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "DBMS_SQL.PARSE quoted upper",
			in:   `CALL "DBMS_SQL"."PARSE"(c, stmt);`,
			want: `CALL dbms_sql.parse(c, stmt);`,
		},
		{
			name: "DBMS_SQL.CLOSE_CURSOR quoted upper",
			in:   `CALL "DBMS_SQL"."CLOSE_CURSOR"(c);`,
			want: `CALL dbms_sql.close_cursor(c);`,
		},
		{
			name: "DBMS_OUTPUT.PUT_LINE quoted upper",
			in:   `CALL "DBMS_OUTPUT"."PUT_LINE"('hello');`,
			want: `CALL dbms_output.put_line('hello');`,
		},
		{
			name: "user-quoted identifiers preserved",
			in:   `SELECT "MIG"."CUSTOM_TABLE" FROM dual;`,
			want: `SELECT "MIG"."CUSTOM_TABLE" FROM dual;`,
		},
		{
			name: "string literal preserved verbatim",
			in:   `msg := 'see "DBMS_SQL"."PARSE"';`,
			want: `msg := 'see "DBMS_SQL"."PARSE"';`,
		},
		{
			name: "package only, no method",
			in:   `IF "DBMS_LOCK" IS NULL THEN NULL; END IF;`,
			want: `IF dbms_lock IS NULL THEN NULL; END IF;`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleQuotedPackageCalls(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// orafce's `dbms_sql.parse(c integer, stmt varchar2)` is a 2-arg
// procedure; Oracle passes a third arg (the language flag, typically
// DBMS_SQL.NATIVE). The translator must drop the extra arg so the
// orafce signature matches.
func TestTrimOracleDbmsSqlParseLanguageArg(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "drops the third argument",
			in:   `CALL dbms_sql.parse(cur, stmt, DBMS_SQL.NATIVE);`,
			want: `CALL dbms_sql.parse(cur, stmt);`,
		},
		{
			name: "two-arg call left alone",
			in:   `CALL dbms_sql.parse(cur, stmt);`,
			want: `CALL dbms_sql.parse(cur, stmt);`,
		},
		{
			name: "third arg with whitespace",
			in:   `CALL dbms_sql.parse(cur, stmt,  DBMS_SQL.NATIVE );`,
			want: `CALL dbms_sql.parse(cur, stmt);`,
		},
		{
			name: "unrelated parse call left alone",
			in:   `v := my_pkg.parse(t, x, y);`,
			want: `v := my_pkg.parse(t, x, y);`,
		},
		{
			name: "parse with parenthesised arg keeps grouping",
			in:   `CALL dbms_sql.parse(cur, (sql || ' WHERE 1=1'), DBMS_SQL.NATIVE);`,
			want: `CALL dbms_sql.parse(cur, (sql || ' WHERE 1=1'));`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trimOracleDbmsSqlParseLanguageArg(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// End-to-end: combination of both passes through rewriteOracleExpr —
// quoted-uppercase form must land as `CALL dbms_sql.parse(cur, stmt);`
// with no third argument.
func TestRewriteOracleExprDbmsSqlParseEndToEnd(t *testing.T) {
	in := `CALL "DBMS_SQL"."PARSE"(cur, stmt, DBMS_SQL.NATIVE);`
	got := rewriteOracleExpr(in)
	require.Equal(t, `CALL dbms_sql.parse(cur, stmt);`, got)
}
