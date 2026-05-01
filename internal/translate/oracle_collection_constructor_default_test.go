package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Oracle treats a local collection type's name as a callable constructor:
//
//	TYPE type_chaine IS VARRAY(1000) OF VARCHAR2(4000);
//	l_tab type_chaine := type_chaine();
//
// PG has no such function — emitting `DEFAULT type_chaine()` verbatim makes
// the routine body fail at runtime with `function type_chaine() does not
// exist`. The translator must rewrite the constructor to a typed array
// literal so the variable starts as an empty array.
func TestOracleVarrayConstructorDefaultBecomesArrayLiteral(t *testing.T) {
	// Phase 8 unskip: MakeCollectionConstructorVisitor runs as part of
	// the DeclareVar.Default rewrite at block-decl time. type_chaine()
	// → ARRAY[]::VARCHAR(4000)[] and type_chaine('a','b') →
	// ARRAY['a', 'b']::VARCHAR(4000)[].
	src := `
		CREATE OR REPLACE PROCEDURE "DRSE"."INIT_TABS" IS
		  TYPE type_chaine IS VARRAY(1000) OF VARCHAR2(4000);
		  l_tab_index type_chaine := type_chaine();
		  l_tab_pre   type_chaine := type_chaine('a', 'b');
		BEGIN
		  NULL;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.NotContains(t, ddl, "type_chaine()",
		"raw collection constructor must be rewritten:\n%s", ddl)
	require.Contains(t, ddl, "DEFAULT ARRAY[]::",
		"empty constructor must become ARRAY[]::<elem>[]; got:\n%s", ddl)
	require.True(t,
		strings.Contains(ddl, "ARRAY['a', 'b']") || strings.Contains(ddl, "ARRAY['a','b']"),
		"populated constructor must become ARRAY[…]; got:\n%s", ddl)
}
