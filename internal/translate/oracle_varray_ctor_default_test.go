package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Oracle treats a local collection type's name as a callable constructor:
//
//	TYPE t_str_list IS VARRAY(1000) OF VARCHAR2(4000);
//	l_tab t_str_list := t_str_list();
//
// PG has no such function — emitting `DEFAULT t_str_list()` verbatim makes
// the routine body fail at runtime with `function t_str_list() does not
// exist`. The translator must rewrite the constructor to a typed array
// literal so the variable starts as an empty array.
func TestOracleVarrayConstructorDefaultBecomesArrayLiteral(t *testing.T) {
	// Phase 8 unskip: MakeCollectionConstructorVisitor runs as part of
	// the DeclareVar.Default rewrite at block-decl time. t_str_list()
	// → ARRAY[]::VARCHAR(4000)[] and t_str_list('a','b') →
	// ARRAY['a', 'b']::VARCHAR(4000)[].
	src := `
		CREATE OR REPLACE PROCEDURE "APP"."INIT_LISTS" IS
		  TYPE t_str_list IS VARRAY(1000) OF VARCHAR2(4000);
		  l_names t_str_list := t_str_list();
		  l_codes     t_str_list := t_str_list('a', 'b');
		BEGIN
		  NULL;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.NotContains(t, ddl, "t_str_list()",
		"raw collection constructor must be rewritten:\n%s", ddl)
	require.Contains(t, ddl, "DEFAULT ARRAY[]::",
		"empty constructor must become ARRAY[]::<elem>[]; got:\n%s", ddl)
	require.True(t,
		strings.Contains(ddl, "ARRAY['a', 'b']") || strings.Contains(ddl, "ARRAY['a','b']"),
		"populated constructor must become ARRAY[…]; got:\n%s", ddl)
}
