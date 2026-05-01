package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `ALTER VIEW … COMPILE` has no PG counterpart (PG re-resolves view bodies
// on every CREATE OR REPLACE) — dropped with an info explanation.
func TestOracleAlterViewCompileDropped(t *testing.T) {
	src := `ALTER VIEW "MIG"."V_ORDERS" COMPILE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ALTER VIEW",
			"COMPILE has no PG equivalent and must not be emitted as DDL")
	}
	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "view.v_orders" && strings.Contains(e.Source, "COMPILE") {
			sawExpl = true
			require.Contains(t, e.Reason, "re-resolves")
		}
	}
	require.True(t, sawExpl, "expected a COMPILE explanation")
}

// EDITIONABLE / NONEDITIONABLE — Oracle edition-based redefinition. No PG
// counterpart.
func TestOracleAlterViewEditionableDropped(t *testing.T) {
	for _, kw := range []string{"EDITIONABLE", "NONEDITIONABLE"} {
		t.Run(kw, func(t *testing.T) {
			src := `ALTER VIEW "MIG"."V_ORDERS" ` + kw + `;`
			stmts, errs := oracle.Parse(src)
			require.Empty(t, errs)
			res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

			for _, s := range res.Plan.PreActions {
				require.NotContains(t, s, kw)
			}
			var sawExpl bool
			for _, e := range res.Explanations {
				if strings.Contains(e.Source, kw) {
					sawExpl = true
				}
			}
			require.True(t, sawExpl, "expected explanation for "+kw)
		})
	}
}

// ADD/MODIFY/DROP CONSTRAINT on a view (Oracle's declarative-but-unenforced
// constraints) — no PG counterpart.
func TestOracleAlterViewConstraintDropped(t *testing.T) {
	src := `ALTER VIEW "MIG"."V_ORDERS" ADD CONSTRAINT "PK_V_ORDERS" PRIMARY KEY ("ID") DISABLE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ALTER VIEW")
	}
	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "view.v_orders" && strings.Contains(e.Source, "CONSTRAINT") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected CONSTRAINT explanation")
}
