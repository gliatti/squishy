package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestCreateTableLikeEmitsPGEquivalent(t *testing.T) {
	src := "CREATE TABLE dst LIKE src;"
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	require.Empty(t, res.Plan.Tables, "LIKE form should not register a structured table — it inherits from src")
	// PreActions should carry the PG CREATE TABLE … (LIKE …) statement.
	joined := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, joined, `CREATE TABLE "mig"."dst" (LIKE "mig"."src" INCLUDING ALL);`,
		"PreActions=%v", res.Plan.PreActions)

	// And the explanation must surface in the wizard.
	found := false
	for _, e := range res.Explanations {
		if strings.Contains(e.Reason, "LIKE source INCLUDING ALL") {
			found = true
		}
	}
	require.True(t, found, "expected LIKE-emission explanation; got %#v", res.Explanations)
}

func TestCreateTableLikeIfNotExistsRoundtrips(t *testing.T) {
	src := "CREATE TABLE IF NOT EXISTS dst LIKE src;"
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	joined := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, joined, `CREATE TABLE IF NOT EXISTS "mig"."dst"`)
}

func TestCreateTableAsSelectQueuedAsManualReview(t *testing.T) {
	src := "CREATE TABLE summary AS SELECT id, COUNT(*) AS n FROM t GROUP BY id;"
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	require.Empty(t, res.Plan.Tables, "AS SELECT bodies are out of scope for the autogen path")
	require.NotEmpty(t, res.Warnings)

	var prereq *Prerequisite
	for i, p := range res.Prerequisites {
		if p.Object == "table.summary" {
			prereq = &res.Prerequisites[i]
		}
	}
	require.NotNil(t, prereq, "expected a manual-review prerequisite for the AS SELECT body")
	require.Equal(t, SeverityBlocking, prereq.Severity)
	require.Equal(t, CatManualReview, prereq.Category)
	require.Contains(t, prereq.Remediation, "SELECT id, COUNT(*)")
}
