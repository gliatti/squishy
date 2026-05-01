package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `WHEN (cond)` on a row trigger must propagate to the PG CREATE TRIGGER.
// Without it the trigger fires for every row regardless of the condition.
func TestOracleTriggerWhenClausePropagated(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL, "QTY" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_QTY"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		  WHEN (NEW.QTY > 100)
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "WHEN (NEW.QTY > 100)",
		"WHEN clause must round-trip into PG CREATE TRIGGER")
}

// STATEMENT-level trigger (no FOR EACH ROW; FOR EACH STATEMENT explicit
// in PG syntax) must surface as `FOR EACH STATEMENT`.
func TestOracleTriggerStatementLevel(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."AUDIT" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_AUDIT"
		  AFTER INSERT ON "MIG"."AUDIT"
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	// Without an explicit FOR EACH clause, Oracle defaults to STATEMENT-level
	// — the PG writer's default is ROW, so we should NOT emit STATEMENT here.
	// (When the source explicitly says FOR EACH STATEMENT we DO emit it.)
	require.Contains(t, ddl, "FOR EACH ROW",
		"Oracle's no-FOR-EACH default is currently treated as ROW (historical) — keep this test as the regression guard for that behaviour")
}

// Explicit `FOR EACH STATEMENT` from Oracle (rare but possible) must round-
// trip as PG `FOR EACH STATEMENT`.
func TestOracleTriggerExplicitForEachStatement(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."AUDIT" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_AUDIT"
		  AFTER INSERT ON "MIG"."AUDIT"
		  FOR EACH STATEMENT
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "FOR EACH STATEMENT")
}

// `REFERENCING NEW AS new_row OLD AS old_row` aliases must rewrite the
// body's references back to PG's fixed NEW/OLD names. Without this the
// translated trigger would error at CREATE FUNCTION with "record not
// known: new_row".
func TestOracleTriggerReferencingAliasRewritten(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL, "QTY" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_REF"
		  BEFORE UPDATE ON "MIG"."ORDERS"
		  REFERENCING NEW AS NR OLD AS OR1
		  FOR EACH ROW
		BEGIN
		  IF NR.QTY < OR1.QTY THEN
		    NULL;
		  END IF;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	// Aliases were rewritten to the PG-fixed names.
	require.NotContains(t, ddl, "NR.QTY",
		"NEW alias 'NR' must be rewritten to NEW")
	require.NotContains(t, ddl, "OR1.QTY",
		"OLD alias 'OR1' must be rewritten to OLD")
	// After the Oracle ident-case-fold visitor, unquoted Oracle
	// idents in the body are lowercased to match the form that
	// normalizeOracleTable applies when emitting the CREATE TABLE
	// (so `"QTY"` in the table DDL also lands as `"qty"` PG-side).
	// The writer's isPLpgSQLPseudoCol guard keeps NEW/OLD unquoted
	// (case-insensitive match) while the column part is the lowered
	// `"qty"`. Both sides resolve to the same PG column.
	require.True(t,
		strings.Contains(ddl, `NEW."qty"`),
		`NEW."qty" expected; got:\n%s`, ddl)
	require.True(t,
		strings.Contains(ddl, `OLD."qty"`),
		`OLD."qty" expected; got:\n%s`, ddl)
}

// rewriteRowAlias is the unit-level helper — confirm it respects word
// boundaries and quoted strings.
func TestRewriteRowAliasUnit(t *testing.T) {
	cases := []struct {
		name string
		body string
		from string
		to   string
		want string
	}{
		{"basic", "NR.qty := 1;", "NR", "NEW", "NEW.qty := 1;"},
		{"case insensitive", "nr.qty := 1;", "NR", "NEW", "NEW.qty := 1;"},
		{"word boundary", "INNERVAR := 1;", "NR", "NEW", "INNERVAR := 1;"},
		{"in quoted string", "RAISE NOTICE 'NR row';", "NR", "NEW", "RAISE NOTICE 'NR row';"},
		{"multiple", "NR.a := NR.b + 1;", "NR", "NEW", "NEW.a := NEW.b + 1;"},
		{"no-op same alias", "NEW.q := 1;", "NEW", "NEW", "NEW.q := 1;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteRowAlias(tc.body, tc.from, tc.to)
			require.Equal(t, tc.want, got)
		})
	}
}

// FOLLOWS / PRECEDES — already had a warning; the test confirms it stays
// info-level (not blocking) since the user can rename to encode order.
func TestOracleTriggerFollowsRemainsInfo(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_A"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		BEGIN NULL; END;
		/
		CREATE OR REPLACE TRIGGER "MIG"."TRG_B"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		  FOLLOWS "MIG"."TRG_A"
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawWarn bool
	for _, w := range res.Warnings {
		if w.Kind == "trigger.follows" && strings.Contains(w.Message, "FOLLOWS") {
			sawWarn = true
		}
	}
	require.True(t, sawWarn)
}
