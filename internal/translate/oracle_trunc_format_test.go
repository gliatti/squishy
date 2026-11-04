package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Oracle's `TRUNC(<date>)` truncates a date or timestamp to midnight. PG has
// `trunc()` for numerics only, so the literal Oracle form raises
// "function trunc(timestamp without time zone) does not exist" when used as
// a parameter default like `p_day date default trunc(sysdate)`. The
// rewriter must lift it to `date_trunc('day', <expr>)::date`.
func TestOracleTruncOnSysdateBecomesDateTrunc(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "APP"."DAILY_SUMMARY"(
		  p_include_drafts IN NUMBER DEFAULT 0,
		  p_day IN DATE DEFAULT TRUNC(SYSDATE)
		) IS BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.NotContains(t, strings.ToUpper(ddl), "TRUNC(SYSDATE)",
		"raw TRUNC(SYSDATE) must be rewritten:\n%s", ddl)
	// SYSDATE inside the arg is rewritten by the same pass to its PG idiom,
	// so the final shape is date_trunc('day', CURRENT_TIMESTAMP::timestamp(0))::date.
	require.Contains(t, ddl, "date_trunc('day', CURRENT_TIMESTAMP::timestamp(0))::date",
		"single-arg TRUNC of SYSDATE must lift to date_trunc:\n%s", ddl)
}

// TRUNC(d, 'YYYY') / TRUNC(d, 'MM') variants must map their format mask to
// the right PG date_trunc unit.
func TestOracleTruncWithFormatMaskMapsUnit(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "APP"."PERIOD_START"(
		  p_y IN DATE DEFAULT TRUNC(SYSDATE, 'YYYY'),
		  p_m IN DATE DEFAULT TRUNC(SYSDATE, 'MM')
		) IS BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "date_trunc('year', CURRENT_TIMESTAMP::timestamp(0))::date",
		"YYYY mask must map to year:\n%s", ddl)
	require.Contains(t, ddl, "date_trunc('month', CURRENT_TIMESTAMP::timestamp(0))::date",
		"MM mask must map to month:\n%s", ddl)
}

// Numeric TRUNC must remain untouched — PG's native trunc() handles it.
func TestOracleTruncOnNumericLeftAlone(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "APP"."ROUND_DOWN"(
		  p_n IN NUMBER DEFAULT TRUNC(123.456)
		) IS BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.Contains(t, ddl, "TRUNC(123.456)",
		"numeric TRUNC must pass through; got:\n%s", ddl)
	require.NotContains(t, ddl, "date_trunc",
		"numeric TRUNC must not be rewritten as date_trunc:\n%s", ddl)
}
