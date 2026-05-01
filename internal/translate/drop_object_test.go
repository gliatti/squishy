package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateDropIndexEmitsPGStmt(t *testing.T) {
	stmts, errs := mysql.Parse("DROP INDEX idx_x ON orders;")
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	pre := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, pre, `DROP INDEX "mig"."idx_x";`,
		"PG DROP INDEX has no ON clause; the per-table qualifier is dropped")
}

func TestTranslateDropViewIfExistsCascade(t *testing.T) {
	stmts, errs := mysql.Parse("DROP VIEW IF EXISTS a, b CASCADE;")
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	pre := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, pre, `DROP VIEW IF EXISTS "mig"."a", "mig"."b" CASCADE;`)
}

func TestTranslateDropProcedureAndFunction(t *testing.T) {
	stmts, errs := mysql.Parse("DROP PROCEDURE IF EXISTS p; DROP FUNCTION IF EXISTS f;")
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	pre := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, pre, `DROP PROCEDURE IF EXISTS "mig"."p";`)
	require.Contains(t, pre, `DROP FUNCTION IF EXISTS "mig"."f";`)
}

func TestTranslateDropTriggerWarnsAndEmitsTodo(t *testing.T) {
	stmts, errs := mysql.Parse("DROP TRIGGER IF EXISTS trg;")
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	pre := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, pre, "TODO: DROP TRIGGER", "TRIGGER emit must surface as a TODO comment")

	found := false
	for _, w := range res.Warnings {
		if w.Kind == "drop_trigger" {
			found = true
		}
	}
	require.True(t, found, "expected drop_trigger warning")
}

func TestTranslateDropEventWarnsOnly(t *testing.T) {
	stmts, errs := mysql.Parse("DROP EVENT IF EXISTS daily;")
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	require.Empty(t, res.Plan.PreActions, "DROP EVENT has no PG equivalent — no PG stmt emitted")
	found := false
	for _, w := range res.Warnings {
		if w.Kind == "drop_event" {
			found = true
		}
	}
	require.True(t, found)
}
