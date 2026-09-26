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

// ---------------------------------------------------------------------------
// ParseExpr + typed constructs formerly handled by the text body rewriter
// ---------------------------------------------------------------------------

func parseExprFor(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, errs := ParseExpr(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotNil(t, e)
	return e
}

func TestParseExpr_RoundTripAndTrailingTokens(t *testing.T) {
	e := parseExprFor(t, "a + 1 = b")
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", e)
	require.Equal(t, "=", be.Op)
	lhs, ok := be.Lhs.(*ast.BinaryExpr)
	require.True(t, ok, "lhs got %T", be.Lhs)
	require.Equal(t, "+", lhs.Op)

	_, errs := ParseExpr("a + 1 b")
	require.NotEmpty(t, errs)
	require.Contains(t, errs.Error(), "trailing tokens after expression")

	_, errs = ParseExpr("")
	require.NotEmpty(t, errs, "empty input is not an expression")
}

func TestParseExpr_GroupConcatSeparator(t *testing.T) {
	e := parseExprFor(t, "GROUP_CONCAT(o.status SEPARATOR ',')")
	fc, ok := e.(*ast.FuncCall)
	require.True(t, ok, "got %T", e)
	require.Equal(t, "GROUP_CONCAT", fc.Name)
	require.Len(t, fc.Args, 1)
	id, ok := fc.Args[0].(*ast.Ident)
	require.True(t, ok, "arg got %T", fc.Args[0])
	require.Equal(t, []string{"o", "status"}, id.Parts)
	require.False(t, fc.Distinct)
	require.Empty(t, fc.AggOrderBy)
	sep, ok := fc.AggSeparator.(*ast.Literal)
	require.True(t, ok, "separator got %T", fc.AggSeparator)
	require.Equal(t, "string", sep.Kind)
	require.Equal(t, ",", sep.Text)
}

func TestParseExpr_GroupConcatDistinctOrderSeparator(t *testing.T) {
	e := parseExprFor(t, "GROUP_CONCAT(DISTINCT name ORDER BY name DESC SEPARATOR '; ')")
	fc, ok := e.(*ast.FuncCall)
	require.True(t, ok, "got %T", e)
	require.Equal(t, "GROUP_CONCAT", fc.Name)
	require.True(t, fc.Distinct)
	require.Len(t, fc.Args, 1)
	require.Len(t, fc.AggOrderBy, 1)
	require.True(t, fc.AggOrderBy[0].Desc)
	sep, ok := fc.AggSeparator.(*ast.Literal)
	require.True(t, ok)
	require.Equal(t, "; ", sep.Text)
}

func TestParseExpr_GroupConcatSeparatorNeedsString(t *testing.T) {
	_, errs := ParseExpr("GROUP_CONCAT(x SEPARATOR y)")
	require.NotEmpty(t, errs)
}

func TestParseExpr_CountDistinct(t *testing.T) {
	e := parseExprFor(t, "COUNT(DISTINCT a)")
	fc, ok := e.(*ast.FuncCall)
	require.True(t, ok)
	require.True(t, fc.Distinct)
	e = parseExprFor(t, "COUNT(ALL a)")
	fc, ok = e.(*ast.FuncCall)
	require.True(t, ok)
	require.False(t, fc.Distinct)
}

func TestParseExpr_NullSafeEqual(t *testing.T) {
	e := parseExprFor(t, "a <=> b")
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", e)
	require.Equal(t, "<=>", be.Op)
}

func TestParseExpr_WindowFunction(t *testing.T) {
	e := parseExprFor(t, "SUM(x) OVER (PARTITION BY g ORDER BY d ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)")
	wa, ok := e.(*ast.WindowedAgg)
	require.True(t, ok, "got %T", e)
	require.NotNil(t, wa.Func)
	require.Equal(t, "SUM", wa.Func.Name)
	require.NotNil(t, wa.Over)
	require.Len(t, wa.Over.PartitionBy, 1)
	require.Len(t, wa.Over.OrderBy, 1)
	require.Equal(t, "ROWS BETWEEN 1 PRECEDING AND CURRENT ROW", wa.Over.Frame)
	require.Empty(t, wa.Over.RawSpec)
}

func TestParseExpr_WindowFunctionEmptyAndOrderOnly(t *testing.T) {
	e := parseExprFor(t, "ROW_NUMBER() OVER ()")
	wa, ok := e.(*ast.WindowedAgg)
	require.True(t, ok, "got %T", e)
	require.Empty(t, wa.Func.Args)
	require.Empty(t, wa.Over.PartitionBy)
	require.Empty(t, wa.Over.OrderBy)
	require.Empty(t, wa.Over.Frame)

	e = parseExprFor(t, "RANK() OVER (ORDER BY score DESC, id) + 1")
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", e)
	wa, ok = be.Lhs.(*ast.WindowedAgg)
	require.True(t, ok, "lhs got %T", be.Lhs)
	require.Len(t, wa.Over.OrderBy, 2)
	require.True(t, wa.Over.OrderBy[0].Desc)
}

func TestParseExpr_WindowNamedIsError(t *testing.T) {
	_, errs := ParseExpr("SUM(x) OVER w")
	require.NotEmpty(t, errs)
}

func TestParseExpr_Interval(t *testing.T) {
	cases := []struct {
		src, value, unit string
		typed            bool
	}{
		{"INTERVAL 1 HOUR", "1", "HOUR", false},
		{"INTERVAL '1' DAY", "1", "DAY", false},
		{"INTERVAL n DAY", "", "DAY", true},
		{"INTERVAL t.n DAY", "", "DAY", true},
		{"INTERVAL -1 DAY", "-1", "DAY", false},
		{"INTERVAL '1-2' YEAR_MONTH", "1-2", "YEAR_MONTH", false},
		{"INTERVAL (x+1) DAY", "", "DAY", true},
		{"INTERVAL n * 7 DAY", "", "DAY", true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e := parseExprFor(t, c.src)
			iv, ok := e.(*ast.IntervalLit)
			require.True(t, ok, "got %T", e)
			require.Equal(t, c.value, iv.Value)
			require.Equal(t, c.unit, iv.Unit)
			if c.typed {
				require.NotNil(t, iv.Expr)
			} else {
				require.Nil(t, iv.Expr)
			}
		})
	}

	e := parseExprFor(t, "INTERVAL (x+1) DAY")
	iv := e.(*ast.IntervalLit)
	_, ok := iv.Expr.(*ast.ParenExpr)
	require.True(t, ok, "expr got %T", iv.Expr)

	// An identifier quantity stays a typed Ident (never folded into
	// Value), so consumers do not have to guess identifier-ness.
	e = parseExprFor(t, "INTERVAL t.n DAY")
	iv = e.(*ast.IntervalLit)
	id, ok := iv.Expr.(*ast.Ident)
	require.True(t, ok, "expr got %T", iv.Expr)
	require.Equal(t, []string{"t", "n"}, id.Parts)

	// A string quantity stays literal text in Value.
	e = parseExprFor(t, "INTERVAL 'abc' DAY")
	iv = e.(*ast.IntervalLit)
	require.Equal(t, "abc", iv.Value)
	require.Nil(t, iv.Expr)
}

func TestParseExpr_IntervalInArithmetic(t *testing.T) {
	e := parseExprFor(t, "NOW() - INTERVAL 1 DAY + x")
	be, ok := e.(*ast.BinaryExpr)
	require.True(t, ok, "got %T", e)
	require.Equal(t, "+", be.Op)
	inner, ok := be.Lhs.(*ast.BinaryExpr)
	require.True(t, ok, "lhs got %T", be.Lhs)
	require.Equal(t, "-", inner.Op)
	iv, ok := inner.Rhs.(*ast.IntervalLit)
	require.True(t, ok, "rhs got %T", inner.Rhs)
	require.Equal(t, "1", iv.Value)
	require.Equal(t, "DAY", iv.Unit)
}

func TestParseExpr_CastSignedUnsigned(t *testing.T) {
	cases := []struct {
		src      string
		unsigned bool
	}{
		{"CAST(x AS UNSIGNED)", true},
		{"CAST(x AS UNSIGNED INTEGER)", true},
		{"CAST(x AS SIGNED)", false},
		{"CAST(x AS SIGNED INT)", false},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			e := parseExprFor(t, c.src)
			ce, ok := e.(*ast.CastExpr)
			require.True(t, ok, "got %T", e)
			it, ok := ce.Type.(*ast.IntType)
			require.True(t, ok, "type got %T", ce.Type)
			require.Equal(t, "BIGINT", it.Name)
			require.Equal(t, c.unsigned, it.Unsigned)
		})
	}
}

func TestParseExpr_NiladicDateKeywords(t *testing.T) {
	for _, src := range []string{"CURRENT_DATE", "CURRENT_TIME", "CURRENT_DATE()", "current_date"} {
		t.Run(src, func(t *testing.T) {
			e := parseExprFor(t, src)
			fc, ok := e.(*ast.FuncCall)
			require.True(t, ok, "got %T", e)
			require.Empty(t, fc.Args)
		})
	}
	e := parseExprFor(t, "CURRENT_TIME(3)")
	fc, ok := e.(*ast.FuncCall)
	require.True(t, ok)
	require.Equal(t, "CURRENT_TIME", fc.Name)
	require.Len(t, fc.Args, 1)
}
