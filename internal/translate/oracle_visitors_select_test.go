package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestVisitOracleRownumLimit_Equals(t *testing.T) {
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Star: true}},
		From: []ast.FromItem{&ast.FromTable{Name: "t"}},
		Where: &ast.BinaryExpr{
			Op:  "=",
			Lhs: &ast.Ident{Parts: []string{"ROWNUM"}},
			Rhs: &ast.Literal{Kind: "number", Text: "1"},
		},
	}
	out := VisitOracleRownumLimit(sel)
	rep, ok := out.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("want SelectStmt, got %T", out)
	}
	if rep.Where != nil {
		t.Errorf("Where must be cleared, got %#v", rep.Where)
	}
	if rep.Limit == nil {
		t.Fatal("Limit must be populated")
	}
	lit, _ := rep.Limit.(*ast.Literal)
	if lit == nil || lit.Text != "1" {
		t.Errorf("Limit want 1, got %#v", rep.Limit)
	}
}

func TestVisitOracleRownumLimit_LessOrEqual(t *testing.T) {
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Star: true}},
		Where: &ast.BinaryExpr{
			Op:  "<=",
			Lhs: &ast.Ident{Parts: []string{"rownum"}},
			Rhs: &ast.Literal{Kind: "number", Text: "10"},
		},
	}
	out := VisitOracleRownumLimit(sel)
	rep := out.(*ast.SelectStmt)
	lit, _ := rep.Limit.(*ast.Literal)
	if lit == nil || lit.Text != "10" {
		t.Errorf("LIMIT 10 expected, got %#v", rep.Limit)
	}
}

func TestVisitOracleRownumLimit_StrictLess(t *testing.T) {
	// `WHERE ROWNUM < 5` → `LIMIT 5 - 1`
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Star: true}},
		Where: &ast.BinaryExpr{
			Op:  "<",
			Lhs: &ast.Ident{Parts: []string{"ROWNUM"}},
			Rhs: &ast.Literal{Kind: "number", Text: "5"},
		},
	}
	out := VisitOracleRownumLimit(sel)
	rep := out.(*ast.SelectStmt)
	if rep.Where != nil {
		t.Errorf("Where must be cleared")
	}
	bin, ok := rep.Limit.(*ast.BinaryExpr)
	if !ok || bin.Op != "-" {
		t.Errorf("Limit want BinaryExpr{-}, got %T %v", rep.Limit, rep.Limit)
	}
}

func TestVisitOracleRownumLimit_NotMatchedShapes(t *testing.T) {
	cases := []*ast.SelectStmt{
		// No WHERE.
		{Cols: []ast.SelectItem{{Star: true}}, Where: nil},
		// Where is not a BinaryExpr.
		{Cols: []ast.SelectItem{{Star: true}}, Where: &ast.Literal{Kind: "bool", Text: "TRUE"}},
		// ROWNUM compared with a non-literal.
		{
			Cols: []ast.SelectItem{{Star: true}},
			Where: &ast.BinaryExpr{
				Op:  "=",
				Lhs: &ast.Ident{Parts: []string{"ROWNUM"}},
				Rhs: &ast.Ident{Parts: []string{"x"}},
			},
		},
		// Already has a Limit — leave alone.
		{
			Cols:  []ast.SelectItem{{Star: true}},
			Where: &ast.BinaryExpr{Op: "=", Lhs: &ast.Ident{Parts: []string{"ROWNUM"}}, Rhs: &ast.Literal{Kind: "number", Text: "1"}},
			Limit: &ast.Literal{Kind: "number", Text: "5"},
		},
		// Other column named ROWNUM-ish but not exact.
		{
			Cols: []ast.SelectItem{{Star: true}},
			Where: &ast.BinaryExpr{
				Op:  "=",
				Lhs: &ast.Ident{Parts: []string{"ROWNUMBER"}},
				Rhs: &ast.Literal{Kind: "number", Text: "1"},
			},
		},
	}
	for i, c := range cases {
		out := VisitOracleRownumLimit(c)
		if out != ast.Node(c) {
			t.Errorf("case[%d]: want unchanged, got %#v", i, out)
		}
	}
}

func TestVisitOracleListagg_BasicWithOrder(t *testing.T) {
	in := &ast.WindowedAgg{
		Func: &ast.FuncCall{
			Name: "listagg",
			Args: []ast.Expr{
				&ast.Ident{Parts: []string{"name"}},
				&ast.Literal{Kind: "string", Text: ", "},
			},
		},
		Within: []ast.OrderItem{
			{Expr: &ast.Ident{Parts: []string{"name"}}},
		},
	}
	out := VisitOracleListagg(in)
	wa, ok := out.(*ast.WindowedAgg)
	if !ok {
		t.Fatalf("want WindowedAgg, got %T", out)
	}
	if wa.Within != nil {
		t.Errorf("Within must be drained, got %v", wa.Within)
	}
	if wa.Func == nil || !strings.EqualFold(wa.Func.Name, "string_agg") {
		t.Fatalf("inner Func want string_agg, got %#v", wa.Func)
	}
	if len(wa.Func.Args) != 3 {
		t.Fatalf("string_agg args want 3 (expr::text, sep, ORDER BY), got %d", len(wa.Func.Args))
	}
	// arg[0] must be a CastExpr to text.
	if _, ok := wa.Func.Args[0].(*ast.CastExpr); !ok {
		t.Errorf("arg[0] want CastExpr, got %T", wa.Func.Args[0])
	}
}

func TestVisitOracleListagg_NoSeparator(t *testing.T) {
	// Single-arg listagg uses '' as default separator.
	in := &ast.WindowedAgg{
		Func: &ast.FuncCall{
			Name: "LISTAGG",
			Args: []ast.Expr{&ast.Ident{Parts: []string{"name"}}},
		},
	}
	out := VisitOracleListagg(in)
	wa := out.(*ast.WindowedAgg)
	if len(wa.Func.Args) < 2 {
		t.Fatalf("missing default sep arg")
	}
	sep, ok := wa.Func.Args[1].(*ast.Literal)
	if !ok || sep.Text != "" {
		t.Errorf("default sep want '', got %#v", wa.Func.Args[1])
	}
}

func TestVisitOracleListagg_NotMatched(t *testing.T) {
	other := &ast.WindowedAgg{
		Func: &ast.FuncCall{Name: "sum", Args: []ast.Expr{&ast.Ident{Parts: []string{"x"}}}},
	}
	if VisitOracleListagg(other) != ast.Node(other) {
		t.Errorf("non-listagg WindowedAgg should not be rewritten")
	}
}

func TestVisitOracleNestedAggregates_LiftsToDerivedTable(t *testing.T) {
	// SELECT MIN(COUNT(*)) FROM t GROUP BY x
	innerCount := &ast.FuncCall{
		Name: "COUNT",
		Args: []ast.Expr{&ast.Ident{Parts: []string{"*"}}},
	}
	outerMin := &ast.FuncCall{
		Name: "MIN",
		Args: []ast.Expr{innerCount},
	}
	in := &ast.SelectStmt{
		Cols:    []ast.SelectItem{{Expr: outerMin}},
		From:    []ast.FromItem{&ast.FromTable{Name: "t"}},
		GroupBy: []ast.Expr{&ast.Ident{Parts: []string{"x"}}},
	}
	out := VisitOracleNestedAggregates(in)
	rep, ok := out.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("want SelectStmt, got %T", out)
	}
	// Outer wrapper: SELECT MIN(_nag) FROM (SELECT COUNT(*) AS _nag … ) _nag_t.
	if len(rep.Cols) != 1 {
		t.Fatalf("outer wrapper want 1 col, got %d", len(rep.Cols))
	}
	outerCall, ok := rep.Cols[0].Expr.(*ast.FuncCall)
	if !ok || outerCall.Name != "MIN" {
		t.Errorf("outer call: want MIN(...), got %#v", rep.Cols[0].Expr)
	}
	if len(outerCall.Args) != 1 {
		t.Errorf("outer call args: want 1, got %d", len(outerCall.Args))
	}
	id, ok := outerCall.Args[0].(*ast.Ident)
	if !ok || id.Parts[0] != "_nag" {
		t.Errorf("outer arg: want Ident{_nag}, got %#v", outerCall.Args[0])
	}
	// Outer FROM: derived table with the inner aggregate.
	if len(rep.From) != 1 {
		t.Fatalf("outer From: want 1 item, got %d", len(rep.From))
	}
	sub, ok := rep.From[0].(*ast.FromSubquery)
	if !ok {
		t.Fatalf("outer From[0]: want FromSubquery, got %T", rep.From[0])
	}
	if sub.Alias != "_nag_t" {
		t.Errorf("subquery alias: want _nag_t, got %q", sub.Alias)
	}
	if sub.Stmt == nil || len(sub.Stmt.Cols) != 1 {
		t.Fatalf("inner subquery want 1 col, got %#v", sub.Stmt)
	}
	if sub.Stmt.Cols[0].Alias != "_nag" {
		t.Errorf("inner col alias: want _nag, got %q", sub.Stmt.Cols[0].Alias)
	}
	// Inner GROUP BY preserved.
	if len(sub.Stmt.GroupBy) != 1 {
		t.Errorf("inner GROUP BY: want 1 item, got %d", len(sub.Stmt.GroupBy))
	}
}

func TestVisitOracleNestedAggregates_NotMatchedShapes(t *testing.T) {
	cases := []*ast.SelectStmt{
		// No FROM.
		{Cols: []ast.SelectItem{{Expr: &ast.FuncCall{Name: "MIN", Args: []ast.Expr{&ast.FuncCall{Name: "COUNT"}}}}}},
		// Multi-column.
		{
			Cols: []ast.SelectItem{
				{Expr: &ast.FuncCall{Name: "MIN", Args: []ast.Expr{&ast.FuncCall{Name: "COUNT"}}}},
				{Expr: &ast.Ident{Parts: []string{"y"}}},
			},
			From: []ast.FromItem{&ast.FromTable{Name: "t"}},
		},
		// Single agg, not nested.
		{
			Cols: []ast.SelectItem{{Expr: &ast.FuncCall{Name: "COUNT"}}},
			From: []ast.FromItem{&ast.FromTable{Name: "t"}},
		},
		// Outer is non-aggregate.
		{
			Cols: []ast.SelectItem{{Expr: &ast.FuncCall{Name: "lower", Args: []ast.Expr{&ast.FuncCall{Name: "COUNT"}}}}},
			From: []ast.FromItem{&ast.FromTable{Name: "t"}},
		},
	}
	for i, c := range cases {
		out := VisitOracleNestedAggregates(c)
		if out != ast.Node(c) {
			t.Errorf("case[%d]: want unchanged, got %#v", i, out)
		}
	}
}
