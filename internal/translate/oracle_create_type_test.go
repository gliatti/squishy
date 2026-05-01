package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// CREATE TYPE … AS OBJECT with attributes only (no methods) maps to PG
// composite type via the column-def parser.
func TestOracleCreateObjectTypeAttributesOnly(t *testing.T) {
	src := `
		CREATE OR REPLACE TYPE "MIG"."ADDR" AS OBJECT (
		  street VARCHAR2(80),
		  city   VARCHAR2(40),
		  zip    VARCHAR2(10)
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "CREATE TYPE") && strings.Contains(s, "addr") {
			found = s
		}
	}
	require.NotEmpty(t, found, "expected a CREATE TYPE … AS (...) pre-action")
	require.Contains(t, found, "AS (")
	require.Contains(t, found, `"street"`)
	require.Contains(t, found, `"city"`)
	require.Contains(t, found, `"zip"`)
}

// CREATE TYPE OBJECT with methods → composite emitted with the data
// attributes (methods are dropped and re-emitted by TYPE BODY translation
// as standalone functions). The previous behaviour was to emit nothing
// + a blocking prereq; we now flatten data + drop methods.
func TestOracleCreateObjectTypeWithMethodsRaisesPrereq(t *testing.T) {
	src := `
		CREATE OR REPLACE TYPE "MIG"."ORDER_T" AS OBJECT (
		  id   NUMBER,
		  qty  NUMBER,
		  MEMBER FUNCTION total RETURN NUMBER
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawComposite string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "CREATE TYPE") && strings.Contains(s, "order_t") {
			sawComposite = s
		}
	}
	require.NotEmpty(t, sawComposite,
		"OBJECT with methods must auto-emit a data-only composite type")
	require.Contains(t, sawComposite, `"id"`)
	require.Contains(t, sawComposite, `"qty"`)
	require.NotContains(t, strings.ToUpper(sawComposite), "MEMBER FUNCTION")

	// No blocking prereq anymore — the composite emission + matching TYPE
	// BODY translation cover the methods automatically.
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "OBJECT type") && strings.Contains(p.Title, "ORDER_T") {
			require.NotEqual(t, SeverityBlocking, p.Severity,
				"OBJECT-with-methods prereq must not be blocking now that composite is auto-emitted")
		}
	}
}

// CREATE TYPE … AS VARRAY(N) OF elem → auto-resolved: emitted as an info
// Explanation (NOT a Prerequisite) since the rewrite is already done at
// every use site and there is no user action required.
func TestOracleCreateVarrayTypeRaisesPrereq(t *testing.T) {
	src := `CREATE OR REPLACE TYPE "MIG"."NUM_LIST" AS VARRAY(100) OF NUMBER;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, p := range res.Prerequisites {
		require.NotContains(t, p.Object, "type.num_list",
			"auto-resolved VARRAY rewrite must NOT appear in prerequisites checklist")
	}

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "type.num_list" && e.Level == "info" {
			sawExpl = true
			require.Contains(t, e.Target, "numeric[]") // NUMBER (no precision) → numeric default
			require.Contains(t, e.Reason, "aucune action manuelle requise")
		}
	}
	require.True(t, sawExpl, "expected an info Explanation for the auto-resolved VARRAY rewrite")
}

// CREATE TYPE BODY → each MEMBER FUNCTION/PROCEDURE auto-promoted to a
// standalone PG function. Replaces the old blocking-prereq-only behaviour.
func TestOracleCreateTypeBodyRaisesPrereq(t *testing.T) {
	src := `
		CREATE OR REPLACE TYPE BODY "MIG"."ORDER_T" IS
		  MEMBER FUNCTION total RETURN NUMBER IS
		  BEGIN
		    RETURN 0;
		  END;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	// TYPE BODY auto-promotion: the methods become standalone PG functions.
	// No prerequisite is raised — the user has nothing to do — so we surface
	// the summary as an info-level Explanation instead.
	for _, p := range res.Prerequisites {
		require.NotContains(t, p.Object, "type_body.order_t",
			"auto-resolved TYPE BODY promotion must NOT appear in prerequisites checklist")
	}
	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "type_body.order_t" && e.Level == "info" {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected an info Explanation for the auto-promoted TYPE BODY methods")

	// The MEMBER FUNCTION should have been emitted as a standalone PG
	// function whose first parameter is the receiver composite.
	var sawRoutine bool
	for _, r := range res.Plan.Routines {
		if strings.EqualFold(r.Name, "order_t_total") {
			sawRoutine = true
			require.Contains(t, r.DDL, `"mig"."order_t_total"`)
			require.Contains(t, r.DDL, `self "mig"."order_t"`)
			require.Contains(t, r.DDL, "RETURNS numeric")
			require.Contains(t, r.DDL, "RETURN 0")
		}
	}
	require.True(t, sawRoutine, "expected order_t_total function to be emitted from TYPE BODY")
}
