package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `OPEN cur FOR SELECT …;` (literal SELECT body) maps to PG's identical
// `OPEN cur FOR SELECT …;` form.
func TestOracleOpenForStaticSelect(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."RUN_QUERY" IS
		  cur SYS_REFCURSOR;
		BEGIN
		  OPEN cur FOR SELECT id, qty FROM orders WHERE qty > 0;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "OPEN cur FOR SELECT",
		"static OPEN FOR SELECT must round-trip into PG, got: %s", ddl)
	require.NotContains(t, ddl, "EXECUTE",
		"a literal SELECT body must not be wrapped in EXECUTE")
}

// `OPEN cur FOR <dynamic-string> USING args;` maps to PG's
// `OPEN cur FOR EXECUTE <dyn-string> USING args;` form.
func TestOracleOpenForDynamicWithUsing(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."RUN_DYNAMIC"(p_filter VARCHAR2) IS
		  cur SYS_REFCURSOR;
		  v_sql VARCHAR2(200);
		BEGIN
		  v_sql := 'SELECT id FROM orders WHERE status = $1';
		  OPEN cur FOR v_sql USING p_filter;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "FOR EXECUTE",
		"dynamic OPEN FOR <expr> must rewrite to PG's OPEN … FOR EXECUTE, got: %s", ddl)
	require.True(t, strings.Contains(ddl, "USING p_filter") || strings.Contains(ddl, "USING P_FILTER"),
		"USING args must propagate to PG, got: %s", ddl)
}

// Static `OPEN cur;` (cursor declared with body in DECLARE) keeps the
// existing simple-OPEN behaviour — no FOR clause emitted.
func TestOracleOpenStaticCursor(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."RUN_DECLARED" IS
		  CURSOR cur IS SELECT id FROM orders;
		BEGIN
		  OPEN cur;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "OPEN cur;")
	require.NotContains(t, ddl, "OPEN cur FOR")
}
