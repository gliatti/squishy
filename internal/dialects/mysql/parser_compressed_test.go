package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestCompressedColumnFlag(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, body TEXT COMPRESSED);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	var col *ast.ColumnDef
	for _, c := range ct.Columns {
		if c.Name == "body" {
			col = c
		}
	}
	require.NotNil(t, col)
	require.True(t, col.Compressed)
}

func TestCompressedColumnWithMethod(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, body LONGBLOB COMPRESSED=zlib);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.True(t, ct.Columns[1].Compressed)
}
