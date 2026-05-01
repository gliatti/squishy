package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Oracle accepts defaults anywhere in the parameter list because callers can
// use named notation. PG requires every parameter after a defaulted one to
// also have a default — otherwise CREATE PROCEDURE raises 42P13 ("input
// parameters after one with a default value must also have defaults"). Pad
// the trailing IN params with DEFAULT NULL so the routine compiles.
func TestOracleProcedureMidListDefaultPadsTrailingParams(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "DRSE"."PRC_BARNIER_PDF"(
		  nv_ann_trt IN VARCHAR2,
		  nd_cod_com IN NUMBER,
		  nd_cod_tra IN NUMBER,
		  nv_tra_jur IN VARCHAR2 DEFAULT NULL,
		  nv_num_agc IN VARCHAR2,
		  p_cat_fac  IN VARCHAR2
		) IS BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	// Each trailing IN param must carry a synthesized DEFAULT NULL.
	require.Contains(t, ddl, `IN "nv_num_agc" VARCHAR DEFAULT NULL`,
		"trailing IN param without default must be padded; got:\n%s", ddl)
	require.Contains(t, ddl, `IN "p_cat_fac" VARCHAR DEFAULT NULL`,
		"trailing IN param without default must be padded; got:\n%s", ddl)
	// PG rule: every IN param after a defaulted one must be defaulted →
	// every IN line in the signature carries `DEFAULT NULL` here.
	require.Equal(t, 6, strings.Count(ddl, "DEFAULT NULL"),
		"every IN param must carry exactly one DEFAULT NULL; got:\n%s", ddl)
}

// Without any defaulted param in the list, no padding happens — leaves the
// MySQL/MariaDB and the no-default Oracle paths untouched.
func TestOracleProcedureNoDefaultsNoPadding(t *testing.T) {
	src := `
		CREATE OR REPLACE PROCEDURE "DRSE"."PRC_PLAIN"(
		  pv_a IN VARCHAR2,
		  pv_b IN VARCHAR2
		) IS BEGIN NULL; END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	require.NotContains(t, ddl, "DEFAULT NULL",
		"no padding when no param has a default; got:\n%s", ddl)
}
