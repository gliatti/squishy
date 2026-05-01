package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// rewriteOracleCollectionTokens — unit tests for the token-walker
// rewriter that handles `TABLE(<expr>)` (O-23) and Oracle collection-
// method postfix expressions (O-22a).
func TestRewriteCollectionTokens_TableUnnest(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"single table call",
			"SELECT * FROM TABLE(v_arr)",
			"SELECT * FROM unnest(v_arr)",
		},
		{
			"table call inside join",
			"SELECT * FROM orders o, TABLE(o.items) t",
			"SELECT * FROM orders o, unnest(o.items) t",
		},
		{
			"table inside string literal stays untouched",
			"SELECT 'TABLE(foo)' FROM dual",
			"SELECT 'TABLE(foo)' FROM dual",
		},
		{
			"create table prefix is not rewritten (no following paren)",
			// Note: this body is what would appear in an expression
			// context (view body) — `CREATE TABLE` headers don't reach
			// the body rewriter, so the keyword's standalone use stays.
			"WHERE 'TABLE foo' = 'x'",
			"WHERE 'TABLE foo' = 'x'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleCollectionTokens(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestRewriteCollectionTokens_Methods(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"COUNT in IF cond",
			"IF v_arr.COUNT > 0 THEN",
			"IF COALESCE(array_length(v_arr, 1), 0) > 0 THEN",
		},
		{
			"FIRST as integer",
			"i := v_arr.FIRST",
			"i := 1",
		},
		{
			"LAST in WHILE",
			"WHILE i <= v_arr.LAST LOOP",
			"WHILE i <= array_length(v_arr, 1) LOOP",
		},
		{
			"NEXT(i) on RHS",
			"i := v_arr.NEXT(i)",
			"i := (i + 1)",
		},
		{
			"PRIOR(i)",
			"i := v_arr.PRIOR(i)",
			"i := (i - 1)",
		},
		{
			"EXISTS check",
			"IF v_arr.EXISTS(i) THEN",
			"IF (v_arr[i] IS NOT NULL) THEN",
		},
		{
			"dotted-ident chain (pkg.coll.COUNT)",
			"x := pkg.coll.COUNT",
			"x := COALESCE(array_length(pkg.coll, 1), 0)",
		},
		{
			"method-name inside string is not rewritten",
			"RAISE NOTICE 'v_arr.COUNT'",
			"RAISE NOTICE 'v_arr.COUNT'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleCollectionTokens(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// End-to-end through a routine body: `IF v_arr.COUNT > 0 THEN ... END IF`
// must rewrite the COUNT expression in the IF condition. This is the
// O-22a expression-level case that was previously left as raw Oracle
// text.
func TestOracleCollectionMethodsInExpressionEndToEnd(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."CHECK_LEN" IS
		  v_arr SYS.ODCINUMBERLIST;
		BEGIN
		  IF v_arr.COUNT > 0 THEN
		    NULL;
		  END IF;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.True(t,
		strings.Contains(ddl, "array_length") || strings.Contains(ddl, "ARRAY_LENGTH"),
		"COUNT in IF condition must rewrite to array_length(...), got: %s", ddl)
	require.NotContains(t, strings.ToUpper(ddl), ".COUNT",
		"the .COUNT postfix must be rewritten away in the IF condition")
}

// O-23 end-to-end through a SELECT used inside a view body: `TABLE(coll)`
// in the FROM clause must rewrite to `unnest(coll)`.
func TestOracleTableCollectionInViewBodyEndToEnd(t *testing.T) {
	src := `
		CREATE OR REPLACE VIEW "MIG"."V_ITEMS" AS
		  SELECT t.column_value AS item
		  FROM TABLE(SYS.ODCINUMBERLIST(1,2,3)) t;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	require.Len(t, res.Plan.Views, 1)
	ddl := res.Plan.Views[0].DDL
	require.Contains(t, ddl, "unnest(",
		"TABLE(coll) in view FROM must rewrite to unnest(coll), got: %s", ddl)
	require.NotContains(t, ddl, "TABLE(SYS.ODCINUMBERLIST")
}
