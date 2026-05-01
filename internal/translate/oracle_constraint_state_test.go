package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// FK NOVALIDATE → PG NOT VALID, with no follow-up VALIDATE step (Oracle
// user explicitly opted out of validation).
func TestOracleFKNoValidateEmitsNotValid(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL, CONSTRAINT "PK_O" PRIMARY KEY ("ID"));
		CREATE TABLE "MIG"."ITEMS" (
		  "ID" NUMBER NOT NULL,
		  "ORDER_ID" NUMBER NOT NULL,
		  CONSTRAINT "PK_I" PRIMARY KEY ("ID"),
		  CONSTRAINT "FK_I_O" FOREIGN KEY ("ORDER_ID") REFERENCES "ORDERS" ("ID") NOVALIDATE
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.ForeignKeys, 1)
	require.True(t, res.Plan.ForeignKeys[0].NotValid)

	// FK pre-action is in the post-copy DDL.
	require.Contains(t, res.DDLPostCopy, "NOT VALID",
		"NOVALIDATE must surface as NOT VALID in the FK ALTER")
	// Oracle NOVALIDATE means the user opted out — no follow-up VALIDATE step.
	require.NotContains(t, res.DDLPostCopy, "VALIDATE CONSTRAINT")
}

// FK NOVALIDATE on a partitioned source table → must NOT emit `NOT VALID`,
// because PG rejects it with SQLSTATE 42809 ("cannot add NOT VALID foreign
// key on partitioned table"). A warn-level explanation surfaces the change.
func TestOracleFKNoValidateOnPartitionedTableSkipsNotValid(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL, CONSTRAINT "PK_O" PRIMARY KEY ("ID"));
		CREATE TABLE "MIG"."ITEMS" (
		  "ID" NUMBER NOT NULL,
		  "ORDER_ID" NUMBER NOT NULL,
		  "TIME_ID" DATE NOT NULL,
		  CONSTRAINT "FK_I_O" FOREIGN KEY ("ORDER_ID") REFERENCES "ORDERS" ("ID") NOVALIDATE
		)
		PARTITION BY RANGE ("TIME_ID") (
		  PARTITION "P1" VALUES LESS THAN (TO_DATE('2024-01-01','YYYY-MM-DD'))
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.ForeignKeys, 1)
	require.NotContains(t, res.DDLPostCopy, "NOT VALID",
		"NOT VALID must be suppressed on FK targeting a partitioned child")
	require.NotContains(t, res.DDLPostCopy, "VALIDATE CONSTRAINT",
		"no separate VALIDATE step — the ADD already validated")

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Level == "warn" && strings.Contains(e.Source, "NOVALIDATE") &&
			strings.Contains(e.Object, "fk_i_o") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected a warn explanation about NOVALIDATE downgrade on partitioned FK")
}

// FK DEFERRABLE INITIALLY DEFERRED rolls through to PG's identical clause.
func TestOracleFKDeferrableEmitsPGDeferrable(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL, CONSTRAINT "PK_O" PRIMARY KEY ("ID"));
		CREATE TABLE "MIG"."ITEMS" (
		  "ID" NUMBER NOT NULL,
		  "ORDER_ID" NUMBER NOT NULL,
		  CONSTRAINT "FK_I_O" FOREIGN KEY ("ORDER_ID") REFERENCES "ORDERS" ("ID")
		    DEFERRABLE INITIALLY DEFERRED
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.ForeignKeys, 1)
	fk := res.Plan.ForeignKeys[0]
	require.True(t, fk.Deferrable)
	require.True(t, fk.InitiallyDeferred)
	require.Contains(t, res.DDLPostCopy, "DEFERRABLE INITIALLY DEFERRED")
}

// DISABLE drops the constraint entirely (PG has no equivalent state) +
// info-level explanation surfaces the drop.
func TestOracleConstraintDisableSkipped(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" (
		  "ID" NUMBER NOT NULL,
		  "QTY" NUMBER NOT NULL,
		  CONSTRAINT "PK_O" PRIMARY KEY ("ID"),
		  CONSTRAINT "CK_QTY" CHECK ("QTY" > 0) DISABLE
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.Empty(t, tbl.Checks, "DISABLE'd CHECK must not appear in the table's check list")

	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "DISABLE") && strings.Contains(e.Object, "ck_qty") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected an explanation for the DISABLE'd CHECK")
}

// PK NOVALIDATE → warning (PG can't NOT VALID a PK).
func TestOraclePKNoValidateWarns(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" (
		  "ID" NUMBER NOT NULL,
		  CONSTRAINT "PK_O" PRIMARY KEY ("ID") NOVALIDATE
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Level == "warn" && strings.Contains(e.Source, "NOVALIDATE") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected a NOVALIDATE warn explanation on the PK")
}

// USING INDEX (...) on a PK is dropped — PG always synthesises the index.
// Surfaced as an info-level explanation so the user knows the spec was
// silently lost.
func TestOraclePKUsingIndexNote(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" (
		  "ID" NUMBER NOT NULL,
		  CONSTRAINT "PK_O" PRIMARY KEY ("ID")
		    USING INDEX (CREATE UNIQUE INDEX "IX_PK_O" ON "ORDERS" ("ID") TABLESPACE "USERS")
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "USING INDEX") {
			sawExpl = true
			require.Contains(t, e.Reason, "synthesises")
		}
	}
	require.True(t, sawExpl, "expected a USING INDEX explanation on the PK")
}

// USING INDEX with just the bare keyword (DBMS_METADATA's most common form
// after a non-IOT PK) also surfaces an explanation — there's no spec to
// drop but the user benefits from knowing PG handled it implicitly.
func TestOracleUQUsingIndexBareNote(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" (
		  "ID" NUMBER NOT NULL,
		  "CODE" VARCHAR2(20) NOT NULL,
		  CONSTRAINT "UQ_CODE" UNIQUE ("CODE") USING INDEX
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "USING INDEX") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl)
}

// RELY on FK → info note (PG planner has no equivalent hint).
func TestOracleFKRelyNotes(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL, CONSTRAINT "PK_O" PRIMARY KEY ("ID"));
		CREATE TABLE "MIG"."ITEMS" (
		  "ID" NUMBER NOT NULL,
		  "ORDER_ID" NUMBER NOT NULL,
		  CONSTRAINT "FK_I_O" FOREIGN KEY ("ORDER_ID") REFERENCES "ORDERS" ("ID") RELY
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "RELY") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl)
}
