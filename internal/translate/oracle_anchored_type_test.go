package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// %TYPE / %ROWTYPE in declarations now pass through to PG verbatim — PG
// supports the same syntax natively in PL/pgSQL.
func TestOracleAnchoredTypeMapsToPGAnchored(t *testing.T) {
	cases := []struct {
		name string
		in   *ast.UserDefinedType
		want string
	}{
		{"col TYPE", &ast.UserDefinedType{Name: "ORDERS.ID", Anchored: "TYPE"}, "orders.id%TYPE"},
		{"row TYPE", &ast.UserDefinedType{Name: "ORDERS", Anchored: "ROWTYPE"}, "orders%ROWTYPE"},
		{"qualified col", &ast.UserDefinedType{Name: "MIG.ORDERS.ID", Anchored: "TYPE"}, "mig.orders.id%TYPE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapOracleType(tc.in, "v", Caps{})
			require.Equal(t, tc.want, got.PG)
			require.NotEmpty(t, got.Note, "expected an explanatory note for anchored types")
		})
	}
}

// End-to-end: a function with a parameter typed `tab.col%TYPE` and a local
// var typed `tab%ROWTYPE` must emit the anchored syntax in both places.
func TestOracleAnchoredTypeRoundTripsInRoutine(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);
		CREATE OR REPLACE PROCEDURE "MIG"."BUMP"(p_id IN "ORDERS"."ID"%TYPE) IS
		  v_row "ORDERS"%ROWTYPE;
		BEGIN
		  NULL;
		END;
		/`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	// The exact identifier casing is normalised to lowercase by the Oracle
	// pipeline; verify the anchored shape is preserved.
	require.True(t, strings.Contains(ddl, "%TYPE") || strings.Contains(ddl, "%type"),
		"parameter %TYPE must round-trip into the PG signature, got: %s", ddl)
	require.True(t, strings.Contains(ddl, "%ROWTYPE") || strings.Contains(ddl, "%rowtype"),
		"local %ROWTYPE must round-trip into the PG DECLARE, got: %s", ddl)
}
