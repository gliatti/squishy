package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

func dropPreActions(res *Result, kind string) []string {
	var out []string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "DROP "+kind) {
			out = append(out, s)
		}
	}
	return out
}

func TestOracleDropIndex(t *testing.T) {
	src := `DROP INDEX "MIG"."IX_FOO";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	got := dropPreActions(res, "INDEX")
	require.Len(t, got, 1)
	require.Contains(t, got[0], `"mig"."ix_foo"`)
}

func TestOracleDropView(t *testing.T) {
	src := `DROP VIEW "MIG"."V_ORDERS";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	got := dropPreActions(res, "VIEW")
	require.Len(t, got, 1)
	require.Contains(t, got[0], `"mig"."v_orders"`)
}

func TestOracleDropMaterializedView(t *testing.T) {
	src := `DROP MATERIALIZED VIEW "MIG"."MV_SUMMARY";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	got := dropPreActions(res, "MATERIALIZED VIEW")
	require.Len(t, got, 1)
	require.Contains(t, got[0], `"mig"."mv_summary"`)
}

func TestOracleDropSequence(t *testing.T) {
	src := `DROP SEQUENCE "MIG"."S_ORDERS";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	got := dropPreActions(res, "SEQUENCE")
	require.Len(t, got, 1)
}

func TestOracleDropProcedure(t *testing.T) {
	src := `DROP PROCEDURE "MIG"."P_REFRESH";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	got := dropPreActions(res, "PROCEDURE")
	require.Len(t, got, 1)
}

func TestOracleDropFunction(t *testing.T) {
	src := `DROP FUNCTION "MIG"."FN_TOTAL";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	got := dropPreActions(res, "FUNCTION")
	require.Len(t, got, 1)
}

func TestOracleDropType(t *testing.T) {
	src := `DROP TYPE "MIG"."T_ORDER" FORCE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	got := dropPreActions(res, "TYPE")
	require.Len(t, got, 1)
	require.Contains(t, got[0], `"mig"."t_order"`)
}

func TestOracleDropTypeBodyExplained(t *testing.T) {
	src := `DROP TYPE BODY "MIG"."T_ORDER";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	require.Empty(t, dropPreActions(res, "TYPE BODY"),
		"PG composite types have no body — nothing to emit")
	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "DROP TYPE BODY") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl)
}

func TestOraclePackageDropExplained(t *testing.T) {
	src := `DROP PACKAGE "MIG"."PKG_ORDERS";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})
	require.Empty(t, dropPreActions(res, "PACKAGE"))
	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "DROP PACKAGE") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl)
}

// DROP TRIGGER must use the table-aware PG syntax when the owning table
// is in the migration plan.
func TestOracleDropTriggerWithKnownTable(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_ORDERS"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		BEGIN NULL; END;
		/
		DROP TRIGGER "MIG"."TRG_ORDERS";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.HasPrefix(strings.TrimSpace(s), "DROP TRIGGER") {
			found = s
		}
	}
	require.NotEmpty(t, found, "expected a real DROP TRIGGER statement")
	require.Contains(t, found, `"trg_orders"`)
	require.Contains(t, found, `"mig"."orders"`)
}

// DROP TRIGGER without a known owning table falls back to the TODO comment
// + warning (matches the MySQL behaviour for the same shape).
func TestOracleDropTriggerWithUnknownTable(t *testing.T) {
	src := `DROP TRIGGER "MIG"."TRG_ORPHAN";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var todo string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "TODO: DROP TRIGGER") {
			todo = s
		}
	}
	require.NotEmpty(t, todo, "expected a TODO placeholder when the table is unknown")
	var sawWarn bool
	for _, w := range res.Warnings {
		if w.Kind == "drop_trigger" {
			sawWarn = true
		}
	}
	require.True(t, sawWarn)
}
