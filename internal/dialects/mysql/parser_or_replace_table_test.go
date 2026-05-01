package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestCreateOrReplaceTableParsesAndFlags(t *testing.T) {
	src := `CREATE OR REPLACE TABLE app.t (id INT PRIMARY KEY);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.True(t, ct.OrReplace)
	require.Equal(t, "app", ct.Schema)
	require.Equal(t, "t", ct.Name)
}

func TestCreateOrReplaceTemporaryTable(t *testing.T) {
	src := `CREATE OR REPLACE TEMPORARY TABLE t (id INT);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.True(t, ct.OrReplace)
	require.True(t, ct.Temporary)
}

func TestCreateTableWithoutOrReplace(t *testing.T) {
	src := `CREATE TABLE t (id INT);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.False(t, ct.OrReplace)
}
