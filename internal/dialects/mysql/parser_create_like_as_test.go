package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestCreateTableLike(t *testing.T) {
	src := "CREATE TABLE dst LIKE src;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableLike)
	require.True(t, ok)
	require.Equal(t, "dst", ct.Name)
	require.Equal(t, "src", ct.LikeName)
}

func TestCreateTableLikeIfNotExists(t *testing.T) {
	src := "CREATE TABLE IF NOT EXISTS dst LIKE other.src;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableLike)
	require.True(t, ok)
	require.True(t, ct.IfNotExists)
	require.Equal(t, "dst", ct.Name)
	require.Equal(t, "other", ct.LikeSchema)
	require.Equal(t, "src", ct.LikeName)
}

func TestCreateTableLikeParenForm(t *testing.T) {
	src := "CREATE TABLE dst (LIKE src);"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableLike)
	require.True(t, ok)
	require.Equal(t, "dst", ct.Name)
	require.Equal(t, "src", ct.LikeName)
}

func TestCreateTableTemporaryLike(t *testing.T) {
	src := "CREATE TEMPORARY TABLE tmp_x LIKE base;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableLike)
	require.True(t, ok)
	require.True(t, ct.Temporary)
	require.Equal(t, "tmp_x", ct.Name)
}

func TestCreateTableAsSelect(t *testing.T) {
	src := "CREATE TABLE summary AS SELECT id, COUNT(*) FROM t GROUP BY id;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableAs)
	require.True(t, ok)
	require.Equal(t, "summary", ct.Name)
	require.Contains(t, ct.SelectBody, "SELECT id")
	require.Contains(t, ct.SelectBody, "GROUP BY id")
}

func TestCreateTableAsSelectNoAsKw(t *testing.T) {
	// MySQL allows omitting AS.
	src := "CREATE TABLE summary SELECT id FROM t;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableAs)
	require.True(t, ok)
	require.Equal(t, "summary", ct.Name)
	require.Contains(t, ct.SelectBody, "SELECT id FROM t")
}

func TestCreateTableAsSelectIgnore(t *testing.T) {
	src := "CREATE TABLE dst IGNORE AS SELECT * FROM t;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTableAs)
	require.Equal(t, "IGNORE", ct.KeyConflict)
}

func TestCreateTableWithColumnsAndAs(t *testing.T) {
	// queryCreateTable also allows column defs + AS SELECT (MySQL applies
	// the col list to the SELECT result).
	src := "CREATE TABLE summary (id INT, total BIGINT) ENGINE=InnoDB AS SELECT id, COUNT(*) FROM t GROUP BY id;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableAs)
	require.True(t, ok)
	require.Equal(t, "summary", ct.Name)
	require.Len(t, ct.Columns, 2)
	require.Equal(t, "InnoDB", ct.Options.Engine)
	require.Contains(t, ct.SelectBody, "SELECT id")
}

func TestCreateTableLikeBacktickedNames(t *testing.T) {
	src := "CREATE TABLE `dst` LIKE `src`;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct, ok := stmts[0].(*ast.CreateTableLike)
	require.True(t, ok)
	require.Equal(t, "dst", ct.Name)
	require.Equal(t, "src", ct.LikeName)
}
