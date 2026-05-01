package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// findTriggerRoutineDDL returns the rendered DDL for a CREATE TRIGGER
// routine with the given (case-insensitive) name, or an empty string.
func findTriggerRoutineDDL(res *Result, name string) string {
	for _, r := range res.Plan.Routines {
		if r.Kind == "trigger" && strings.EqualFold(r.Name, name) {
			return r.DDL
		}
	}
	return ""
}

// `ALTER TRIGGER name DISABLE` rewrites to PG's table-aware
// `ALTER TABLE tbl DISABLE TRIGGER name` and is appended to the matching
// CREATE TRIGGER routine's DDL — that ordering ensures the ALTER runs
// after the owning table is created and after the data is copied (level 5
// of the planner DAG, after copy_table at level 2).
func TestOracleAlterTriggerDisableUsesOwningTable(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_ORDERS_AI"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		BEGIN NULL; END;
		/
		ALTER TRIGGER "MIG"."TRG_ORDERS_AI" DISABLE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	ddl := findTriggerRoutineDDL(res, "TRG_ORDERS_AI")
	require.NotEmpty(t, ddl, "expected the trigger routine DDL to exist")
	require.Contains(t, ddl, "DISABLE TRIGGER",
		"ALTER TABLE … DISABLE TRIGGER must be appended to the trigger routine DDL")
	require.Contains(t, ddl, `"mig"."orders"`,
		"owning table must be resolved from the prior CREATE TRIGGER")
	// The ALTER must quote the trigger name with the same case as the
	// preceding CREATE TRIGGER (PG quoted idents are case-sensitive — a
	// mismatch raises "trigger does not exist"). The Oracle parser keeps
	// `s.Name` as-is from the source dump (uppercase here).
	require.Contains(t, ddl, `DISABLE TRIGGER "TRG_ORDERS_AI"`)

	// Belt-and-braces: PreActions must NOT contain a stray DISABLE TRIGGER
	// — that was the previous (broken) emission point.
	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "DISABLE TRIGGER",
			"ALTER TABLE … DISABLE TRIGGER must not appear in PreActions (would run before CREATE TABLE)")
	}
}

// `ALTER TRIGGER name ENABLE` — same as DISABLE, mirrored.
func TestOracleAlterTriggerEnable(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_ORDERS_AI"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		BEGIN NULL; END;
		/
		ALTER TRIGGER "MIG"."TRG_ORDERS_AI" ENABLE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	ddl := findTriggerRoutineDDL(res, "TRG_ORDERS_AI")
	require.NotEmpty(t, ddl)
	require.Contains(t, ddl, `ENABLE TRIGGER "TRG_ORDERS_AI"`)
	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ENABLE TRIGGER",
			"ALTER TABLE … ENABLE TRIGGER must not appear in PreActions")
	}
}

// INSTEAD OF triggers on a view: PG accepts CREATE TRIGGER but rejects
// ALTER TABLE … ENABLE/DISABLE TRIGGER on a view (SQLSTATE 42809).
// The ALTER must be silently dropped with an info-level explanation —
// PG triggers on views are always active, so this is semantically lossless.
func TestOracleAlterTriggerOnViewDropsEnable(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL, CONSTRAINT "PK_O" PRIMARY KEY ("ID"));
		CREATE OR REPLACE VIEW "MIG"."OC_ORDERS" AS SELECT "ID" FROM "MIG"."ORDERS";
		CREATE OR REPLACE TRIGGER "MIG"."ORDERS_TRG"
		  INSTEAD OF INSERT ON "MIG"."OC_ORDERS"
		  FOR EACH ROW
		BEGIN
		  INSERT INTO "MIG"."ORDERS" ("ID") VALUES (:NEW."ID");
		END;
		/
		ALTER TRIGGER "MIG"."ORDERS_TRG" ENABLE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	ddl := findTriggerRoutineDDL(res, "ORDERS_TRG")
	require.NotEmpty(t, ddl, "the CREATE TRIGGER itself must still be emitted")
	require.NotContains(t, ddl, "ENABLE TRIGGER",
		"ENABLE TRIGGER must NOT be emitted when the trigger is on a view")
	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ENABLE TRIGGER")
	}

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Level == "info" && strings.Contains(e.Source, "ENABLE") &&
			strings.Contains(e.Object, "orders_trg") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected an info explanation about the dropped ENABLE on a view")
}

// `ALTER TRIGGER name COMPILE` has no PG equivalent (PG re-parses on
// CREATE FUNCTION) — dropped entirely with an info-level explanation.
func TestOracleAlterTriggerCompileDropped(t *testing.T) {
	src := `ALTER TRIGGER "MIG"."TRG_ORDERS_AI" COMPILE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "TRIGGER",
			"COMPILE has no PG counterpart and must not generate DDL")
	}
	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "COMPILE") {
			sawExpl = true
			require.Contains(t, e.Reason, "re-parses")
		}
	}
	require.True(t, sawExpl, "expected a COMPILE explanation")
}

// When the owning table can't be resolved (no CREATE TRIGGER for this name
// in the dump), we emit a self-contained DO block that resolves the
// table at apply time via pg_trigger. No blocking prerequisite — the
// migration proceeds. If the trigger doesn't exist on the target either,
// the DO block raises a NOTICE and exits cleanly.
func TestOracleAlterTriggerUnknownTableRaisesPrereq(t *testing.T) {
	src := `ALTER TRIGGER "MIG"."TRG_ORPHAN" DISABLE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	// The runtime DO block lands in PostActions (target schema + alter
	// at apply time after CREATEs have completed).
	var sawDoBlock bool
	for _, s := range res.Plan.PostActions {
		if strings.Contains(s, "DO $$") &&
			strings.Contains(s, "FROM pg_trigger") &&
			strings.Contains(strings.ToLower(s), "trg_orphan") &&
			strings.Contains(s, "DISABLE TRIGGER") {
			sawDoBlock = true
		}
	}
	require.True(t, sawDoBlock,
		"expected a runtime DO-block fallback for the orphan ALTER, got PostActions:\n%v",
		res.Plan.PostActions)

	// Must NOT raise a blocking manual-review prereq for this trigger.
	for _, p := range res.Prerequisites {
		if strings.Contains(strings.ToLower(p.Title), "trg_orphan") {
			t.Fatalf("expected NO blocking prereq, got: %+v", p)
		}
	}
}

// RENAME TO new must round-trip through the PG-aware syntax.
func TestOracleAlterTriggerRename(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_OLD"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		BEGIN NULL; END;
		/
		ALTER TRIGGER "MIG"."TRG_OLD" RENAME TO "TRG_NEW";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	ddl := findTriggerRoutineDDL(res, "TRG_OLD")
	require.NotEmpty(t, ddl, "expected the trigger routine DDL to carry the RENAME")
	require.Contains(t, ddl, "RENAME TO")
	// Source-side trigger name is preserved verbatim (case-sensitive match
	// against the prior CREATE TRIGGER); the new name follows the
	// `normalizeOracleIdent` lowercase convention since it's a fresh
	// identifier without an existing CREATE.
	require.Contains(t, ddl, `"TRG_OLD"`)
	require.Contains(t, ddl, `"mig"."orders"`)
	require.Contains(t, ddl, `"trg_new"`)
}
