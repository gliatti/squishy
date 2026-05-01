package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// COMPOUND TRIGGER must surface a blocking manual-review prereq with
// concrete splitting guidance. The skip + commented placeholder behaviour
// is preserved so the user can find the original body.
func TestOracleCompoundTriggerEmitsBlockingPrereq(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_COMPOUND"
		  FOR INSERT OR UPDATE ON "MIG"."ORDERS"
		COMPOUND TRIGGER
		  total NUMBER := 0;
		  BEFORE EACH ROW IS BEGIN total := total + 1; END BEFORE EACH ROW;
		  AFTER STATEMENT IS BEGIN NULL; END AFTER STATEMENT;
		END "TRG_COMPOUND";
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "COMPOUND TRIGGER") && strings.Contains(p.Title, "TRG_COMPOUND") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
			require.Equal(t, CatManualReview, p.Category)
			require.Contains(t, p.Remediation, "BEFORE EACH ROW")
			require.Contains(t, p.Remediation, "AFTER STATEMENT")
			require.Contains(t, p.Remediation, "transition table")
		}
	}
	require.True(t, sawPrereq, "expected blocking COMPOUND TRIGGER prereq")

	// Routine still appears with a commented placeholder so users can find
	// the original body.
	require.Len(t, res.Plan.Routines, 1)
	require.Contains(t, res.Plan.Routines[0].DDL, "Compound trigger")
}
