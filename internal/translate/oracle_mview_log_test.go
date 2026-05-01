package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// CREATE MATERIALIZED VIEW LOG → no PG counterpart, dropped with info.
func TestOracleCreateMaterializedViewLogDropped(t *testing.T) {
	src := `CREATE MATERIALIZED VIEW LOG ON "MIG"."ORDERS"
		WITH ROWID, SEQUENCE ("ID","STATUS") INCLUDING NEW VALUES;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Empty(t, res.Plan.PreActions)
	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "MATERIALIZED VIEW LOG") {
			sawExpl = true
			require.Contains(t, e.Reason, "incremental refresh")
		}
	}
	require.True(t, sawExpl)
}

// ALTER MATERIALIZED VIEW (refresh policy) → dropped with info.
func TestOracleAlterMaterializedViewDropped(t *testing.T) {
	src := `ALTER MATERIALIZED VIEW "MIG"."MV_SUMMARY" REFRESH FAST ON COMMIT;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Empty(t, res.Plan.PreActions)
	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "ALTER MATERIALIZED VIEW") {
			sawExpl = true
			require.Contains(t, e.Reason, "REFRESH")
		}
	}
	require.True(t, sawExpl)
}

// ALTER MATERIALIZED VIEW LOG → same dropped path.
func TestOracleAlterMaterializedViewLogDropped(t *testing.T) {
	src := `ALTER MATERIALIZED VIEW LOG ON "MIG"."ORDERS" PARALLEL 4;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Empty(t, res.Plan.PreActions)
	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "ALTER MATERIALIZED VIEW LOG") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl)
}
