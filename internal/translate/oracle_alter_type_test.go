package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `ALTER TYPE … COMPILE` drops with an info explanation.
func TestOracleAlterTypeCompileDropped(t *testing.T) {
	src := `ALTER TYPE "MIG"."T_ORDER" COMPILE BODY;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ALTER TYPE")
	}
	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "type.t_order" && strings.Contains(e.Source, "COMPILE") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected COMPILE explanation")
}

// `ALTER TYPE … ADD ATTRIBUTE` raises a blocking manual-review prereq —
// PG composite types don't round-trip Oracle's evolution semantics cleanly.
func TestOracleAlterTypeAddAttributeRaisesPrereq(t *testing.T) {
	src := `ALTER TYPE "MIG"."T_ORDER" ADD ATTRIBUTE "TS" TIMESTAMP CASCADE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "ALTER TYPE") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
			require.Equal(t, CatManualReview, p.Category)
		}
	}
	require.True(t, sawPrereq, "expected manual-review prereq for ALTER TYPE ADD ATTRIBUTE")
}

// `ALTER TYPE … DROP ATTRIBUTE` — same prereq emission as ADD.
func TestOracleAlterTypeDropAttributePrereq(t *testing.T) {
	src := `ALTER TYPE "MIG"."T_ORDER" DROP ATTRIBUTE "OBSOLETE_FLAG" CASCADE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "ALTER TYPE") {
			sawPrereq = true
		}
	}
	require.True(t, sawPrereq)
}
