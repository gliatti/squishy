package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `coll.DELETE;` (no args) — clear the collection. PG: assign empty array.
func TestOracleCollectionDeleteRewritten(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."RESET_COLL" IS
		  v_arr SYS.ODCINUMBERLIST;
		BEGIN
		  v_arr.DELETE;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "ARRAY[]",
		"v_arr.DELETE must rewrite to an empty-array assignment, got: %s", ddl)
}

// `coll.EXTEND;` (no args) — append a NULL.
func TestOracleCollectionExtendRewritten(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."GROW" IS
		  v_arr SYS.ODCINUMBERLIST;
		BEGIN
		  v_arr.EXTEND;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "array_append")
}

// `coll.TRIM;` (no args) — drop the last element.
func TestOracleCollectionTrimRewritten(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."SHRINK" IS
		  v_arr SYS.ODCINUMBERLIST;
		BEGIN
		  v_arr.TRIM;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "array_length")
}

// `coll.DELETE(i)` — element removal. PG arrays have no gap semantics, so
// we surface a TODO comment + a routine.untranslated_construct warning
// rather than silently miscompiling.
func TestOracleCollectionDeleteByIndexWarns(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."ZAP" IS
		  v_arr SYS.ODCINUMBERLIST;
		BEGIN
		  v_arr.DELETE(2);
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawWarn bool
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "DELETE(i)") {
			sawWarn = true
		}
	}
	require.True(t, sawWarn, "expected a DELETE(i) warning")

	ddl := res.Plan.Routines[0].DDL
	// Identifier case is normalised to upper by the Oracle pipeline; match
	// case-insensitively.
	require.Contains(t, strings.ToUpper(ddl), "TODO: V_ARR.DELETE")
}
