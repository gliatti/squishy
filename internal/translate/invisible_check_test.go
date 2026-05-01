package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateInvisibleColumnEmitsExplanation(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		secret_count INT INVISIBLE
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.Len(t, res.Plan.Tables, 1)

	// Column must still be present on the target — INVISIBLE is just a hint.
	tbl := res.Plan.Tables[0]
	var found bool
	for _, c := range tbl.Columns {
		if c.Name == "secret_count" {
			found = true
		}
	}
	require.True(t, found, "INVISIBLE column must still be migrated")

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Source == "INVISIBLE column" && e.Object == "t.secret_count" {
			sawExpl = true
		}
	}
	require.True(t, sawExpl, "expected an INVISIBLE column explanation")
}

func TestTranslateNonInvisibleColumnNoInvisibleExpl(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(50));`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	for _, e := range res.Explanations {
		require.NotEqual(t, "INVISIBLE column", e.Source)
	}
}

func TestTranslateNotEnforcedCheckEmitsWarning(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		v INT,
		CONSTRAINT v_pos CHECK (v > 0) NOT ENFORCED
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.Len(t, res.Plan.Tables, 1)
	require.NotEmpty(t, res.Plan.Tables[0].Checks, "CHECK must still be emitted (PG enforces it)")

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Source == "CHECK ... NOT ENFORCED" {
			sawExpl = true
			require.Equal(t, "warn", e.Level)
		}
	}
	require.True(t, sawExpl, "expected a NOT ENFORCED CHECK warning")
}

func TestTranslateEnforcedCheckHasNoNotEnforcedWarning(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		v INT,
		CONSTRAINT v_pos CHECK (v > 0)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	for _, e := range res.Explanations {
		require.NotEqual(t, "CHECK ... NOT ENFORCED", e.Source)
	}
}
