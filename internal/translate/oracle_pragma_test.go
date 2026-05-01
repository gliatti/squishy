package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// PRAGMA AUTONOMOUS_TRANSACTION must be dropped (PG can't parse it) and
// surfaced as a TODO + warning so the user reaches for the right
// remediation (autonomous-transaction extension, pg_background, dblink).
func TestOraclePragmaAutonomousTransactionWarns(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."AUDIT_INSERT" IS
		  PRAGMA AUTONOMOUS_TRANSACTION;
		BEGIN
		  NULL;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.NotContains(t, ddl, "AUTONOMOUS_TRANSACTION;",
		"AUTONOMOUS_TRANSACTION line must be replaced by a TODO comment")
	require.Contains(t, ddl, "TODO: Oracle PRAGMA AUTONOMOUS_TRANSACTION",
		"TODO comment must surface the pragma so users see it")

	var sawWarn bool
	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "AUTONOMOUS_TRANSACTION") {
			sawWarn = true
			require.Contains(t, w.Message, "dblink")
		}
	}
	require.True(t, sawWarn, "expected a routine.untranslated_construct warn for AUTONOMOUS_TRANSACTION")
}

// PRAGMA EXCEPTION_INIT(my_excp, -2291) — Phase 6.2 translates this
// at AST level: the visitor harvests the binding and stamps the
// resolved SQLSTATE on every matching RAISE site. No more "PRAGMA
// dropped" warning, no more TODO comment in the body.
func TestOraclePragmaExceptionInitTranslated(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "MIG"."MAP_FK" IS
		  fk_violation EXCEPTION;
		  PRAGMA EXCEPTION_INIT(fk_violation, -2291);
		BEGIN
		  IF 1 = 1 THEN
		    RAISE fk_violation;
		  END IF;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	// The translated rewrite is visible: a matching RAISE was stamped
	// with a 45XXX SQLSTATE.
	require.Contains(t, ddl, "RAISE EXCEPTION")
	require.Regexp(t, `ERRCODE\s*=\s*'45\d{3}'`, ddl,
		"matching RAISE must carry the resolved 45XXX SQLSTATE")
	// The dropped-pragma TODO should no longer appear for EXCEPTION_INIT.
	require.NotContains(t, ddl, "TODO: Oracle PRAGMA EXCEPTION_INIT dropped")

	for _, w := range res.Warnings {
		if strings.Contains(w.Message, "EXCEPTION_INIT") {
			t.Errorf("unexpected EXCEPTION_INIT warning still emitted: %q", w.Message)
		}
	}
}
