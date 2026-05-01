package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Oracle MERGE INTO ... must round-trip into PG MERGE (PG 15+ supports the
// same shape natively). Oracle's trailing WHERE on a WHEN clause maps to
// PG's `AND <cond>` AND-guard.
func TestOracleMergeMatchedWhereRewritesToAndGuard(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."UPSERT" IS
		BEGIN
		  MERGE INTO orders t
		  USING staging s ON (t.id = s.id)
		  WHEN MATCHED THEN UPDATE SET t.qty = s.qty WHERE s.qty > 0
		  WHEN NOT MATCHED THEN INSERT (id, qty) VALUES (s.id, s.qty) WHERE s.qty > 0;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "WHEN MATCHED AND",
		"trailing WHERE on WHEN MATCHED must rewrite into AND-guard, got: %s", ddl)
	require.Contains(t, ddl, "WHEN NOT MATCHED AND",
		"trailing WHERE on WHEN NOT MATCHED must rewrite into AND-guard, got: %s", ddl)
}

// LOG ERRORS [INTO …] [REJECT LIMIT …] has no PG counterpart — must be
// stripped + a warning emitted.
func TestOracleMergeLogErrorsStripped(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."UPSERT" IS
		BEGIN
		  MERGE INTO orders t
		  USING staging s ON (t.id = s.id)
		  WHEN MATCHED THEN UPDATE SET t.qty = s.qty
		  WHEN NOT MATCHED THEN INSERT (id, qty) VALUES (s.id, s.qty)
		  LOG ERRORS INTO err_log REJECT LIMIT UNLIMITED;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.NotContains(t, ddl, "LOG ERRORS")
	var sawWarn bool
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "LOG ERRORS") {
			sawWarn = true
		}
	}
	require.True(t, sawWarn, "expected a routine.untranslated_construct warning for LOG ERRORS")
}

// DELETE WHERE inside WHEN MATCHED THEN UPDATE → can't be safely auto-
// split into two WHEN branches; surface a warning so the user splits it.
func TestOracleMergeDeleteWhereWarns(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."UPSERT" IS
		BEGIN
		  MERGE INTO orders t
		  USING staging s ON (t.id = s.id)
		  WHEN MATCHED THEN UPDATE SET t.qty = s.qty
		    DELETE WHERE s.qty = 0;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawWarn bool
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "DELETE WHERE") {
			sawWarn = true
		}
	}
	require.True(t, sawWarn, "expected a routine.untranslated_construct warning for DELETE WHERE")
}
