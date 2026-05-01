package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// CREATE IMMUTABLE / BLOCKCHAIN / SHARDED / DUPLICATED TABLE — table is
// emitted as a regular PG heap; per-qualifier explanation surfaces the
// dropped semantic.
func TestOracleTableQualifierExplanations(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// Substring expected in one of the emitted explanation Source fields.
		marker string
	}{
		{"immutable", `CREATE IMMUTABLE TABLE "MIG"."LEDGER" ("ID" NUMBER NOT NULL);`, "IMMUTABLE"},
		{"blockchain", `CREATE BLOCKCHAIN TABLE "MIG"."LEDGER" ("ID" NUMBER NOT NULL);`, "BLOCKCHAIN"},
		{"sharded", `CREATE SHARDED TABLE "MIG"."ORDERS" ("ID" NUMBER NOT NULL);`, "SHARDED"},
		{"duplicated", `CREATE DUPLICATED TABLE "MIG"."DIM_REGION" ("ID" NUMBER NOT NULL);`, "DUPLICATED"},
		{"private_temp", `CREATE PRIVATE TEMPORARY TABLE "MIG"."ORA$PTT_TMP" ("ID" NUMBER);`, "PRIVATE TEMPORARY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmts, errs := oracle.Parse(tc.src)
			require.Empty(t, errs)
			res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

			require.NotEmpty(t, res.Plan.Tables, "the table itself must still be emitted")
			var sawExpl bool
			for _, e := range res.Explanations {
				if strings.Contains(e.Source, tc.marker) {
					sawExpl = true
				}
			}
			require.True(t, sawExpl, "expected an explanation mentioning %q", tc.marker)
		})
	}
}
