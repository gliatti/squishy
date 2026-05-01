package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestDropIndexOnTable(t *testing.T) {
	src := "DROP INDEX idx_x ON orders;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	d, ok := stmts[0].(*ast.DropObject)
	require.True(t, ok)
	require.Equal(t, "INDEX", d.Kind)
	require.Equal(t, "idx_x", d.Name)
	require.Equal(t, "orders", d.OnTable.Name)
}

func TestDropIndexAlgorithmLockTrailing(t *testing.T) {
	src := "DROP INDEX idx_x ON orders ALGORITHM=INPLACE LOCK=NONE;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	d := stmts[0].(*ast.DropObject)
	require.Equal(t, "INDEX", d.Kind)
	require.Equal(t, "idx_x", d.Name)
}

func TestDropViewMulti(t *testing.T) {
	src := "DROP VIEW IF EXISTS a, b, c CASCADE;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	d := stmts[0].(*ast.DropObject)
	require.Equal(t, "VIEW", d.Kind)
	require.True(t, d.IfExists)
	require.Len(t, d.Names, 3)
	require.True(t, d.Cascade)
}

func TestDropProcedureFunctionTriggerEvent(t *testing.T) {
	cases := map[string]string{
		"DROP PROCEDURE IF EXISTS p;":   "PROCEDURE",
		"DROP FUNCTION IF EXISTS f;":    "FUNCTION",
		"DROP TRIGGER IF EXISTS trg;":   "TRIGGER",
		"DROP EVENT IF EXISTS daily;":   "EVENT",
	}
	for src, kind := range cases {
		stmts, errs := Parse(src)
		require.Empty(t, errs, "src=%q errs=%v", src, errs)
		d, ok := stmts[0].(*ast.DropObject)
		require.True(t, ok, "src=%q", src)
		require.Equal(t, kind, d.Kind, "src=%q", src)
		require.True(t, d.IfExists, "src=%q", src)
	}
}

func TestDropTableTrailingCascade(t *testing.T) {
	// DROP TABLE supports a trailing CASCADE/RESTRICT in the grammar — must
	// be consumed even though the AST doesn't currently expose it.
	src := "DROP TABLE IF EXISTS a, b CASCADE;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	dt, ok := stmts[0].(*ast.DropTable)
	require.True(t, ok)
	require.True(t, dt.IfExists)
	require.Len(t, dt.Tables, 2)
}

func TestDropFunctionSchemaQualified(t *testing.T) {
	src := "DROP FUNCTION app.compute_total;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	d := stmts[0].(*ast.DropObject)
	require.Equal(t, "FUNCTION", d.Kind)
	require.Equal(t, "app", d.Schema)
	require.Equal(t, "compute_total", d.Name)
}
