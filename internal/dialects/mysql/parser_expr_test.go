package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// parseExprFromCheck runs the expression parser by stuffing the input into a
// CHECK constraint of a CREATE TABLE statement, since the MySQL parser
// exposes parseExpr only via DDL contexts. Returns the captured expression.
func parseExprFromCheck(t *testing.T, expr string) ast.Expr {
	t.Helper()
	src := "CREATE TABLE t (x INT CHECK (" + expr + "));"
	stmts, errs := Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.Len(t, stmts, 1)
	ct, ok := stmts[0].(*ast.CreateTable)
	require.True(t, ok)
	require.NotEmpty(t, ct.Columns)
	require.NotNil(t, ct.Columns[0].Check, "expected a CHECK expression")
	return ct.Columns[0].Check
}

func TestParseExpr_Between(t *testing.T) {
	e := parseExprFromCheck(t, "x BETWEEN 1 AND 10")
	be, ok := e.(*ast.BetweenExpr)
	require.True(t, ok, "got %T", e)
	require.False(t, be.Not)
	require.NotNil(t, be.Low)
	require.NotNil(t, be.High)
}

func TestParseExpr_NotBetween(t *testing.T) {
	e := parseExprFromCheck(t, "x NOT BETWEEN 1 AND 10")
	be, ok := e.(*ast.BetweenExpr)
	require.True(t, ok, "got %T", e)
	require.True(t, be.Not)
}

func TestParseExpr_InList(t *testing.T) {
	e := parseExprFromCheck(t, "x IN (1, 2, 3)")
	in, ok := e.(*ast.InExpr)
	require.True(t, ok, "got %T", e)
	require.False(t, in.Not)
	require.Len(t, in.List, 3)
}

func TestParseExpr_NotInList(t *testing.T) {
	e := parseExprFromCheck(t, "x NOT IN ('a', 'b')")
	in, ok := e.(*ast.InExpr)
	require.True(t, ok, "got %T", e)
	require.True(t, in.Not)
	require.Len(t, in.List, 2)
}

func TestParseExpr_CaseSearched(t *testing.T) {
	e := parseExprFromCheck(t, "CASE WHEN x > 0 THEN 1 WHEN x < 0 THEN -1 ELSE 0 END = 1")
	// The CASE is the LHS of `=`.
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", e)
	ce, ok := be.Lhs.(*ast.CaseExpr)
	require.True(t, ok, "lhs got %T", be.Lhs)
	require.Nil(t, ce.Operand, "searched CASE has no operand")
	require.Len(t, ce.Whens, 2)
	require.NotNil(t, ce.Else)
}

func TestParseExpr_CaseSimple(t *testing.T) {
	e := parseExprFromCheck(t, "CASE x WHEN 1 THEN 'one' WHEN 2 THEN 'two' END = 'one'")
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok)
	ce, ok := be.Lhs.(*ast.CaseExpr)
	require.True(t, ok, "lhs got %T", be.Lhs)
	require.NotNil(t, ce.Operand, "simple CASE has an operand")
	require.Len(t, ce.Whens, 2)
	require.Nil(t, ce.Else)
}

func TestParseExpr_Cast(t *testing.T) {
	e := parseExprFromCheck(t, "CAST(x AS CHAR(8)) = 'abc'")
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok)
	ce, ok := be.Lhs.(*ast.CastExpr)
	require.True(t, ok, "lhs got %T", be.Lhs)
	require.NotNil(t, ce.Type)
	require.Equal(t, "CHAR", ce.Type.TypeName())
}

func TestParseExpr_NotLike(t *testing.T) {
	e := parseExprFromCheck(t, "x NOT LIKE '%foo%'")
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", e)
	require.Equal(t, "NOT LIKE", be.Op)
}
