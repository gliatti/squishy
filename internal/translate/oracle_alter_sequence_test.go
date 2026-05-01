package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Plain `ALTER SEQUENCE name RESTART WITH N` lifts to PG's identical
// statement. The pre-action list must carry the rewrite so it runs before
// the data copy starts.
func TestOracleAlterSequenceRestartWith(t *testing.T) {
	src := `ALTER SEQUENCE "MIG"."S_ORDERS" RESTART WITH 5000;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "ALTER SEQUENCE") {
			found = s
		}
	}
	require.NotEmpty(t, found, "expected an ALTER SEQUENCE pre-action")
	require.Contains(t, found, `"mig"."s_orders"`,
		"Oracle uppercase identifiers must be lowercased like the rest of the pipeline")
	require.Contains(t, found, "RESTART WITH 5000")
}

// Oracle 12.2+ permits `ALTER SEQUENCE name START WITH N` — semantically
// identical to RESTART WITH N. PG's ALTER SEQUENCE has no plain START, so
// we rewrite it as RESTART WITH N.
func TestOracleAlterSequenceStartWithIsRewrittenToRestart(t *testing.T) {
	src := `ALTER SEQUENCE "MIG"."S_ITEMS" START WITH 1000;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "ALTER SEQUENCE") {
			found = s
		}
	}
	require.NotEmpty(t, found)
	require.Contains(t, found, "RESTART WITH 1000",
		"plain START WITH on ALTER must rewrite to PG's RESTART WITH")
	require.NotContains(t, found, " START WITH ",
		"PG ALTER SEQUENCE rejects START — the rewrite must drop it")
}

// Multi-spec ALTER: INCREMENT BY + MAX + CYCLE + a bunch of Oracle-only
// knobs. The translator should render a single PG ALTER carrying the
// supported specs and surface the dropped knobs in an explanation rather
// than emitting them.
func TestOracleAlterSequenceMultiSpec(t *testing.T) {
	src := `ALTER SEQUENCE "MIG"."S_X"
		INCREMENT BY 5
		MAXVALUE 99999
		CYCLE
		ORDER NOKEEP SCALE EXTEND;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "ALTER SEQUENCE") {
			found = s
		}
	}
	require.NotEmpty(t, found)
	require.Contains(t, found, "INCREMENT BY 5")
	require.Contains(t, found, "MAXVALUE 99999")
	require.Contains(t, found, "CYCLE")
	for _, dropped := range []string{"ORDER", "NOKEEP", "SCALE", "EXTEND"} {
		require.NotContains(t, found, dropped,
			"%s has no PG equivalent and must not be emitted in the ALTER", dropped)
	}

	// A single explanation should list the dropped Oracle-only specs.
	var sawDrop bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "ORDER") && strings.Contains(e.Source, "NOKEEP") {
			sawDrop = true
		}
	}
	require.True(t, sawDrop, "expected an explanation listing the dropped Oracle-only specs")
}

// `ALTER SEQUENCE name NOMAXVALUE` → PG `NO MAXVALUE` (with a space).
func TestOracleAlterSequenceNoMaxValue(t *testing.T) {
	src := `ALTER SEQUENCE "MIG"."S" NOMAXVALUE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "ALTER SEQUENCE") {
			found = s
		}
	}
	require.NotEmpty(t, found)
	require.Contains(t, found, "NO MAXVALUE")
}

// `NOCACHE` must rewrite to `CACHE 1` (PG has no NO CACHE; CACHE 1 is the
// closest equivalent).
func TestOracleAlterSequenceNoCacheRewrites(t *testing.T) {
	src := `ALTER SEQUENCE "MIG"."S" NOCACHE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "ALTER SEQUENCE") {
			found = s
		}
	}
	require.NotEmpty(t, found)
	require.Contains(t, found, "CACHE 1")
}

// A bare `ALTER SEQUENCE name;` (Oracle-only "recompile" semantics) must
// NOT produce a malformed PG statement; it's dropped with an explanation.
func TestOracleAlterSequenceBareIsDropped(t *testing.T) {
	src := `ALTER SEQUENCE "MIG"."S";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ALTER SEQUENCE",
			"bare ALTER SEQUENCE has no PG equivalent and must not be emitted")
	}
}
