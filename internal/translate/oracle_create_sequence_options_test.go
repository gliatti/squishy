package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// CREATE SEQUENCE with Oracle-spec options (ORDER, KEEP, SCALE, SHARING)
// must surface them via an info-level explanation so the user knows what
// was dropped.
func TestOracleCreateSequenceOracleOnlyOptionsExplained(t *testing.T) {
	src := `
		CREATE SEQUENCE "MIG"."S_X"
		  START WITH 1 INCREMENT BY 1
		  ORDER NOKEEP SCALE EXTEND
		  SHARING METADATA;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var sawCreate string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "CREATE SEQUENCE") {
			sawCreate = s
		}
	}
	require.NotEmpty(t, sawCreate, "expected a CREATE SEQUENCE pre-action")
	for _, dropped := range []string{"ORDER", "NOKEEP", "SCALE", "EXTEND", "SHARING"} {
		require.NotContains(t, sawCreate, dropped,
			"%s has no PG counterpart and must not be in the emitted CREATE SEQUENCE", dropped)
	}

	var sawDrop bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "ORDER") && strings.Contains(e.Source, "SHARING") {
			sawDrop = true
		}
	}
	require.True(t, sawDrop, "expected an explanation listing the dropped Oracle-only options")
}
