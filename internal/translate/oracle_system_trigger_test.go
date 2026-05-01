package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// AFTER LOGON ON DATABASE — pure connection-event trigger, no PG
// counterpart. Must surface a blocking manual-review prereq.
func TestOracleSystemTriggerLogonRaisesPrereq(t *testing.T) {
	src := `
		CREATE OR REPLACE TRIGGER "MIG"."TRG_LOGON"
		  AFTER LOGON ON DATABASE
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "system trigger") && strings.Contains(p.Title, "TRG_LOGON") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
			require.Equal(t, CatManualReview, p.Category)
			require.Contains(t, p.Description, "LOGON")
		}
	}
	require.True(t, sawPrereq)
}

// BEFORE CREATE ON SCHEMA — DDL event trigger; the prereq's remediation
// must surface PG's CREATE EVENT TRIGGER as the closest fit.
func TestOracleSystemTriggerCreateOnSchemaRaisesPrereq(t *testing.T) {
	src := `
		CREATE OR REPLACE TRIGGER "MIG"."TRG_CREATE_AUDIT"
		  BEFORE CREATE ON "MIG".SCHEMA
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "system trigger") && strings.Contains(p.Title, "TRG_CREATE_AUDIT") {
			sawPrereq = true
			require.Contains(t, p.Remediation, "EVENT TRIGGER")
		}
	}
	require.True(t, sawPrereq)
}

// AFTER SHUTDOWN ON DATABASE — no PG equivalent; remediation must call
// that out and point to operational tooling.
func TestOracleSystemTriggerShutdownNoPGEquivalent(t *testing.T) {
	src := `
		CREATE OR REPLACE TRIGGER "MIG"."TRG_SHUTDOWN"
		  AFTER SHUTDOWN ON DATABASE
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "TRG_SHUTDOWN") {
			sawPrereq = true
			require.Contains(t, p.Remediation, "no PG counterpart")
		}
	}
	require.True(t, sawPrereq)
}

// Regression guard: a regular DML trigger must still translate normally
// (didn't get caught by the system-trigger detection).
func TestOracleRegularTriggerStillTranslates(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE TRIGGER "MIG"."TRG_DML"
		  BEFORE INSERT ON "MIG"."ORDERS"
		  FOR EACH ROW
		BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	require.Contains(t, res.Plan.Routines[0].DDL, "CREATE TRIGGER",
		"DML trigger must still emit a real CREATE TRIGGER")
}
