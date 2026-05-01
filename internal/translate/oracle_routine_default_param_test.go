package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Oracle allows DEFAULT (and the := alias) on IN parameters of procedures and
// functions. PG accepts the same syntax for both, so we must round-trip the
// default expression into the emitted signature — otherwise callers that
// relied on positional defaults break with "function … does not exist".
func TestOracleProcedureParamDefaultsAreEmitted(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "DRSE"."PRC_MAJ_TRC"(
		  p_bo_verbose     IN BOOLEAN  DEFAULT FALSE,
		  p_bo_version     IN BOOLEAN  := TRUE,
		  pv_owner         IN VARCHAR2 DEFAULT 'NONE',
		  pv_table_name    IN VARCHAR2 DEFAULT NULL
		) IS BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "DEFAULT FALSE", "boolean DEFAULT must round-trip; got:\n%s", ddl)
	require.Contains(t, ddl, "DEFAULT TRUE", "`:=` shorthand must be normalised to DEFAULT; got:\n%s", ddl)
	require.Contains(t, ddl, "DEFAULT 'NONE'", "string DEFAULT must round-trip; got:\n%s", ddl)
	require.Contains(t, ddl, "DEFAULT NULL", "NULL DEFAULT must round-trip; got:\n%s", ddl)
}

// Oracle-specific defaults like SYSDATE must be rewritten to the PG idiom so
// the emitted signature actually parses on PG (SYSDATE is not a PG function).
func TestOracleProcedureParamDefaultRewritesSysdate(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "DRSE"."LOG_AT"(
		  p_when IN DATE DEFAULT SYSDATE
		) IS BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "DEFAULT CURRENT_TIMESTAMP::timestamp(0)",
		"SYSDATE in a DEFAULT must be rewritten to the PG idiom; got:\n%s", ddl)
	require.False(t, strings.Contains(strings.ToUpper(ddl[strings.Index(ddl, "("):strings.Index(ddl, ")")+1]), "SYSDATE"),
		"raw SYSDATE must not appear in the param list; got:\n%s", ddl)
}

// A function (returning a value) with DEFAULT params must also round-trip:
// the function branch of pgProcSignature has a different format string from
// procedures, so it needs its own coverage.
func TestOracleFunctionParamDefaultsAreEmitted(t *testing.T) {
	src := `
		CREATE OR REPLACE FUNCTION "DRSE"."FN_GREET"(
		  pv_name VARCHAR2 DEFAULT 'world'
		) RETURN VARCHAR2 IS BEGIN RETURN 'hello ' || pv_name; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "DEFAULT 'world'",
		"function param DEFAULT must round-trip; got:\n%s", ddl)
}
