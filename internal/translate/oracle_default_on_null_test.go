package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `DEFAULT ON NULL` must produce a trigger that COALESCEs explicit-NULL
// inserts/updates back to the default expression. PG's plain DEFAULT
// only fires when the column is omitted.
func TestOracleDefaultOnNullEmitsTrigger(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" (
		  "ID" NUMBER NOT NULL,
		  "STATUS" VARCHAR2(10) DEFAULT ON NULL 'PENDING' NOT NULL
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawFn, sawTrg bool
	for _, a := range res.Plan.PostActions {
		if strings.Contains(a, "default_on_null_orders_status") {
			if strings.Contains(a, "FUNCTION") || strings.Contains(a, "BEGIN") {
				sawFn = true
			}
			if strings.Contains(a, "BEFORE INSERT OR UPDATE") {
				sawTrg = true
			}
		}
	}
	require.True(t, sawFn, "expected the DEFAULT ON NULL trigger function in PostActions")
	require.True(t, sawTrg, "expected the DEFAULT ON NULL trigger declaration in PostActions")

	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "DEFAULT ON NULL") {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected an explanation for DEFAULT ON NULL")
}

// Plain DEFAULT (without ON NULL) must NOT generate the trigger — it's
// handled by PG's normal column-default semantics.
func TestOraclePlainDefaultNoTrigger(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."ORDERS" (
		  "ID" NUMBER NOT NULL,
		  "STATUS" VARCHAR2(10) DEFAULT 'PENDING' NOT NULL
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, a := range res.Plan.PostActions {
		require.NotContains(t, a, "default_on_null_",
			"plain DEFAULT must not generate a coalesce trigger")
	}
}
