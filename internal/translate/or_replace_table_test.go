package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateCreateOrReplaceTableEmitsDropPreaction(t *testing.T) {
	src := `CREATE OR REPLACE TABLE t (id INT PRIMARY KEY);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.Len(t, res.Plan.Tables, 1)

	var sawDrop bool
	for _, pre := range res.Plan.PreActions {
		if strings.Contains(pre, `DROP TABLE IF EXISTS "mig"."t" CASCADE`) {
			sawDrop = true
			break
		}
	}
	require.True(t, sawDrop, "OR REPLACE TABLE must emit DROP TABLE IF EXISTS … CASCADE pre-action")
}

func TestTranslateCreateTableNoOrReplaceNoDrop(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	for _, pre := range res.Plan.PreActions {
		require.NotContains(t, pre, "DROP TABLE")
	}
}
