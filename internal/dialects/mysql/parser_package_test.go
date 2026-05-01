package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestCreatePackageNoop(t *testing.T) {
	src := "DELIMITER //\n" +
		"CREATE PACKAGE my_pkg AS\n" +
		"  PROCEDURE p1(x INT);\n" +
		"  FUNCTION f1 RETURN INT;\n" +
		"END;//\n"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	require.Len(t, stmts, 1)
	noop := stmts[0].(*ast.NoopStmt)
	require.Equal(t, "CREATE PACKAGE", noop.Kind)
	require.Contains(t, noop.Text, "my_pkg")
}

func TestCreatePackageBodyNoop(t *testing.T) {
	src := "DELIMITER //\n" +
		"CREATE PACKAGE BODY my_pkg AS\n" +
		"  PROCEDURE p1(x INT) AS BEGIN NULL; END;\n" +
		"END;//\n"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	require.Len(t, stmts, 1)
	noop := stmts[0].(*ast.NoopStmt)
	require.Equal(t, "CREATE PACKAGE BODY", noop.Kind)
	require.Contains(t, noop.Text, "my_pkg")
}
