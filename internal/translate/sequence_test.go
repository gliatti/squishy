package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateMariaDBCreateSequenceFull(t *testing.T) {
	src := `CREATE SEQUENCE app.counter
		START WITH 100
		INCREMENT BY 2
		MINVALUE 1
		MAXVALUE 1000
		CACHE 50
		CYCLE;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.NotEmpty(t, res.Plan.PreActions)

	var got string
	for _, pre := range res.Plan.PreActions {
		if strings.Contains(pre, "CREATE SEQUENCE") {
			got = pre
			break
		}
	}
	require.NotEmpty(t, got, "expected a CREATE SEQUENCE pre-action")
	// The target schema wins over the source-side qualifier — keeping the
	// source schema (here "app") would mean emitting `CREATE SEQUENCE
	// "app".… ` against a Postgres database that has no "app" schema, which
	// is exactly what made the Oracle HR smoke fail with `schema "HR" does
	// not exist`. Sequences are uniquely named within the target schema.
	require.Contains(t, got, `"mig"."counter"`)
	require.NotContains(t, got, `"app".`)
	require.Contains(t, got, "INCREMENT BY 2")
	require.Contains(t, got, "MINVALUE 1")
	require.Contains(t, got, "MAXVALUE 1000")
	require.Contains(t, got, "START WITH 100")
	require.Contains(t, got, "CACHE 50")
	require.Contains(t, got, " CYCLE")
}

func TestTranslateMariaDBCreateSequenceNoVariants(t *testing.T) {
	src := `CREATE SEQUENCE s NOMINVALUE NOMAXVALUE NOCACHE NOCYCLE;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	var got string
	for _, pre := range res.Plan.PreActions {
		if strings.Contains(pre, "CREATE SEQUENCE") {
			got = pre
			break
		}
	}
	require.NotEmpty(t, got)
	require.Contains(t, got, "NO MINVALUE")
	require.Contains(t, got, "NO MAXVALUE")
	require.Contains(t, got, "CACHE 1") // NOCACHE → CACHE 1 (PG has no NO CACHE)
	require.Contains(t, got, "NO CYCLE")
}

func TestTranslateMariaDBCreateOrReplaceSequenceEmitsDrop(t *testing.T) {
	src := `CREATE OR REPLACE SEQUENCE s START WITH 1;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.GreaterOrEqual(t, len(res.Plan.PreActions), 2)

	var sawDrop, sawCreate bool
	for _, pre := range res.Plan.PreActions {
		if strings.Contains(pre, "DROP SEQUENCE IF EXISTS") {
			sawDrop = true
		}
		if strings.HasPrefix(pre, "CREATE SEQUENCE") {
			sawCreate = true
			require.Contains(t, pre, "IF NOT EXISTS",
				"OR REPLACE should defensively keep IF NOT EXISTS on the CREATE")
		}
	}
	require.True(t, sawDrop, "OR REPLACE must emit DROP SEQUENCE IF EXISTS first")
	require.True(t, sawCreate)
}

func TestTranslateMariaDBCreateSequenceUnschemaedFallsBackToTargetSchema(t *testing.T) {
	src := `CREATE SEQUENCE counter;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	var got string
	for _, pre := range res.Plan.PreActions {
		if strings.Contains(pre, "CREATE SEQUENCE") {
			got = pre
			break
		}
	}
	require.NotEmpty(t, got)
	require.Contains(t, got, `"mig"."counter"`)
}
