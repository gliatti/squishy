package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// FORALL i IN lo..hi <dml> (no SAVE EXCEPTIONS) — plain FOR loop, no
// per-iteration exception wrapper.
func TestOracleForallPlainEmitsBareLoop(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."BULK_UP" IS
		  ids SYS.ODCINUMBERLIST;
		BEGIN
		  FORALL i IN 1..ids.COUNT
		    UPDATE orders SET qty = qty + 1 WHERE id = ids(i);
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	// Identifier case is normalised to upper by the Oracle pipeline.
	require.Contains(t, strings.ToUpper(ddl), "FOR I IN")
	require.NotContains(t, ddl, "EXCEPTION WHEN OTHERS",
		"plain FORALL must not wrap each iteration in BEGIN/EXCEPTION/END")
}

// FORALL i IN lo..hi SAVE EXCEPTIONS <dml> — Phase 6.5 fully translates
// the Oracle bulk-exceptions semantics: each iteration is wrapped in
// BEGIN/EXCEPTION/END so individual failures don't abort the loop, and
// every failure pushes a JSONB record onto the routine-scope
// `_bulk_exceptions` accumulator. References to SQL%BULK_EXCEPTIONS in
// the post-loop diagnostic code translate to cardinality(_bulk_exceptions)
// and JSONB extracts.
func TestOracleForallSaveExceptionsTranslatedFully(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."BULK_BEST_EFFORT" IS
		  ids SYS.ODCINUMBERLIST;
		BEGIN
		  FORALL i IN 1..ids.COUNT SAVE EXCEPTIONS
		    UPDATE orders SET qty = qty + 1 WHERE id = ids(i);
		  -- Post-loop diagnostic: report each failure.
		  FOR i IN 1..SQL%BULK_EXCEPTIONS.COUNT LOOP
		    DBMS_OUTPUT.PUT_LINE('row ' || SQL%BULK_EXCEPTIONS(i).ERROR_INDEX);
		  END LOOP;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "EXCEPTION WHEN OTHERS",
		"SAVE EXCEPTIONS must wrap each iteration in BEGIN/EXCEPTION/END")
	require.Contains(t, ddl, `"_bulk_exceptions" := array_append("_bulk_exceptions"`,
		"SAVE EXCEPTIONS must populate the _bulk_exceptions accumulator")
	require.Contains(t, ddl, `"_bulk_exceptions" jsonb[]`,
		"the routine's outermost DECLARE must declare _bulk_exceptions")
	require.Regexp(t, `(?i)cardinality\("_bulk_exceptions"\)`, ddl,
		"SQL%BULK_EXCEPTIONS.COUNT must translate to cardinality(\"_bulk_exceptions\")")
	require.Contains(t, ddl, `"_bulk_exceptions"[i]->>'error_index'`,
		"SQL%BULK_EXCEPTIONS(i).ERROR_INDEX must translate to JSONB extract")

	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "SAVE EXCEPTIONS") {
			t.Errorf("FORALL SAVE EXCEPTIONS should no longer surface a warning; got %q", w.Message)
		}
	}
}
