package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `ALTER PROCEDURE name COMPILE` — no PG counterpart, dropped with info.
func TestOracleAlterProcedureCompileDropped(t *testing.T) {
	src := `ALTER PROCEDURE "MIG"."P_REFRESH" COMPILE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ALTER PROCEDURE")
	}
	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "procedure.p_refresh" && strings.Contains(e.Source, "COMPILE") {
			sawExpl = true
			require.Contains(t, e.Reason, "re-parses")
		}
	}
	require.True(t, sawExpl, "expected a COMPILE explanation for the procedure")
}

// `ALTER FUNCTION name COMPILE REUSE SETTINGS` — same, but FUNCTION kind.
func TestOracleAlterFunctionCompileDropped(t *testing.T) {
	src := `ALTER FUNCTION "MIG"."FN_TOTAL" COMPILE REUSE SETTINGS;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "function.fn_total" {
			sawExpl = true
		}
	}
	require.True(t, sawExpl)
}

// `ALTER PACKAGE name COMPILE BODY` — same, but PACKAGE kind.
func TestOracleAlterPackageCompileBodyDropped(t *testing.T) {
	src := `ALTER PACKAGE "MIG"."PKG_ORDERS" COMPILE BODY;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "package.pkg_orders" {
			sawExpl = true
		}
	}
	require.True(t, sawExpl)
}
