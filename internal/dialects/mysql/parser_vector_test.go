package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestVectorTypeParsed(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, embedding VECTOR(3) NOT NULL);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	var col *ast.ColumnDef
	for _, c := range ct.Columns {
		if c.Name == "embedding" {
			col = c
		}
	}
	require.NotNil(t, col)
	v, ok := col.Type.(*ast.VectorType)
	require.True(t, ok, "expected VectorType, got %T", col.Type)
	require.True(t, v.HasDim)
	require.Equal(t, 3, v.Dim)
}

func TestVectorTypeWithoutDim(t *testing.T) {
	src := `CREATE TABLE t (e VECTOR);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	v := ct.Columns[0].Type.(*ast.VectorType)
	require.False(t, v.HasDim)
}

func TestVectorIndexInTableConstraint(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		embedding VECTOR(3) NOT NULL,
		VECTOR INDEX v_idx (embedding) M=8 DISTANCE=cosine
	);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.Len(t, ct.Indexes, 1)
	idx := ct.Indexes[0]
	require.Equal(t, "VECTOR", idx.Kind)
	require.Equal(t, "v_idx", idx.Name)
	require.Len(t, idx.Columns, 1)
	require.Equal(t, "embedding", idx.Columns[0].Name)
}
