package translate

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func mkIdent(parts ...string) *ast.Ident {
	return &ast.Ident{Parts: parts}
}

func TestVisitOracleOuterJoin_BasicLeftJoin(t *testing.T) {
	// SELECT a.x FROM A a, B b WHERE a.id = b.id(+)
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: mkIdent("a", "x")}},
		From: []ast.FromItem{
			&ast.FromTable{Name: "A", Alias: "a"},
			&ast.FromTable{Name: "B", Alias: "b"},
		},
		Where: &ast.BinaryExpr{
			Op:  "=",
			Lhs: mkIdent("a", "id"),
			Rhs: &ast.OuterJoinHint{Inner: mkIdent("b", "id")},
		},
	}
	out := VisitOracleOuterJoin(sel)
	rep, ok := out.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("want SelectStmt, got %T", out)
	}
	if rep.Where != nil {
		t.Errorf("WHERE should be cleared (only join predicate present): %#v", rep.Where)
	}
	if len(rep.From) != 1 {
		t.Fatalf("From should collapse to 1 item, got %d", len(rep.From))
	}
	join, ok := rep.From[0].(*ast.FromJoin)
	if !ok {
		t.Fatalf("From[0] want FromJoin, got %T", rep.From[0])
	}
	if join.Kind != ast.LeftJoin {
		t.Errorf("Kind want LeftJoin, got %v", join.Kind)
	}
	// ON clause must be hint-free.
	if whereHasOuterJoinHint(join.On) {
		t.Errorf("ON clause still contains OuterJoinHint: %#v", join.On)
	}
	// Left should be A, Right should be B (the hinted alias).
	if l, ok := join.Left.(*ast.FromTable); !ok || l.Alias != "a" {
		t.Errorf("Left want FromTable{a}, got %#v", join.Left)
	}
	if r, ok := join.Right.(*ast.FromTable); !ok || r.Alias != "b" {
		t.Errorf("Right want FromTable{b}, got %#v", join.Right)
	}
}

func TestVisitOracleOuterJoin_PreservesResidualPredicate(t *testing.T) {
	// SELECT a.x FROM A a, B b WHERE a.id = b.id(+) AND a.flag = 1
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: mkIdent("a", "x")}},
		From: []ast.FromItem{
			&ast.FromTable{Name: "A", Alias: "a"},
			&ast.FromTable{Name: "B", Alias: "b"},
		},
		Where: &ast.BinaryExpr{
			Op: "AND",
			Lhs: &ast.BinaryExpr{
				Op:  "=",
				Lhs: mkIdent("a", "id"),
				Rhs: &ast.OuterJoinHint{Inner: mkIdent("b", "id")},
			},
			Rhs: &ast.BinaryExpr{
				Op:  "=",
				Lhs: mkIdent("a", "flag"),
				Rhs: &ast.Literal{Kind: "number", Text: "1"},
			},
		},
	}
	out := VisitOracleOuterJoin(sel).(*ast.SelectStmt)
	if out.Where == nil {
		t.Fatal("residual WHERE clause should be preserved")
	}
	bin, ok := out.Where.(*ast.BinaryExpr)
	if !ok || bin.Op != "=" {
		t.Errorf("residual WHERE want BinaryExpr{=}, got %#v", out.Where)
	}
}

func TestVisitOracleOuterJoin_BailsOutOnDoubleHint(t *testing.T) {
	// (+) on both sides — invalid Oracle, defensive bail.
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: mkIdent("a", "x")}},
		From: []ast.FromItem{
			&ast.FromTable{Name: "A", Alias: "a"},
			&ast.FromTable{Name: "B", Alias: "b"},
		},
		Where: &ast.BinaryExpr{
			Op:  "=",
			Lhs: &ast.OuterJoinHint{Inner: mkIdent("a", "id")},
			Rhs: &ast.OuterJoinHint{Inner: mkIdent("b", "id")},
		},
	}
	out := VisitOracleOuterJoin(sel)
	if out != ast.Node(sel) {
		t.Errorf("double-hint must leave SelectStmt untouched")
	}
}

func TestVisitOracleOuterJoin_BailsOutOnOrWithHint(t *testing.T) {
	// `(+)` inside an OR group — semantics not reducible.
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: mkIdent("a", "x")}},
		From: []ast.FromItem{
			&ast.FromTable{Name: "A", Alias: "a"},
			&ast.FromTable{Name: "B", Alias: "b"},
		},
		Where: &ast.BinaryExpr{
			Op: "OR",
			Lhs: &ast.BinaryExpr{
				Op:  "=",
				Lhs: mkIdent("a", "id"),
				Rhs: &ast.OuterJoinHint{Inner: mkIdent("b", "id")},
			},
			Rhs: &ast.Literal{Kind: "bool", Text: "TRUE"},
		},
	}
	out := VisitOracleOuterJoin(sel)
	if out != ast.Node(sel) {
		t.Errorf("OR with hint must leave SelectStmt untouched")
	}
}

func TestVisitOracleOuterJoin_NoHintNoChange(t *testing.T) {
	sel := &ast.SelectStmt{
		Cols: []ast.SelectItem{{Expr: mkIdent("a", "x")}},
		From: []ast.FromItem{&ast.FromTable{Name: "a"}},
		Where: &ast.BinaryExpr{
			Op:  "=",
			Lhs: mkIdent("a", "id"),
			Rhs: &ast.Literal{Kind: "number", Text: "1"},
		},
	}
	if VisitOracleOuterJoin(sel) != ast.Node(sel) {
		t.Errorf("hint-free SelectStmt must round-trip unchanged")
	}
}

func TestVisitOracleOuterJoin_NoSelectStmtNoOp(t *testing.T) {
	id := mkIdent("x")
	if VisitOracleOuterJoin(id) != ast.Node(id) {
		t.Errorf("non-SelectStmt should not be touched")
	}
}

func TestStripOuterJoinHints_RemovesHints(t *testing.T) {
	in := &ast.BinaryExpr{
		Op:  "=",
		Lhs: mkIdent("a", "id"),
		Rhs: &ast.OuterJoinHint{Inner: mkIdent("b", "id")},
	}
	out := stripOuterJoinHints(in)
	if whereHasOuterJoinHint(out) {
		t.Errorf("stripOuterJoinHints left a hint behind: %#v", out)
	}
}

func TestFlattenAnd_SplitsTopLevelAndOnly(t *testing.T) {
	// (a OR b) AND c AND d → 3 leaves: (a OR b), c, d.
	in := &ast.BinaryExpr{
		Op: "AND",
		Lhs: &ast.BinaryExpr{
			Op: "AND",
			Lhs: &ast.BinaryExpr{
				Op:  "OR",
				Lhs: &ast.Literal{Kind: "bool", Text: "a"},
				Rhs: &ast.Literal{Kind: "bool", Text: "b"},
			},
			Rhs: &ast.Literal{Kind: "bool", Text: "c"},
		},
		Rhs: &ast.Literal{Kind: "bool", Text: "d"},
	}
	parts := flattenAnd(in)
	if len(parts) != 3 {
		t.Errorf("want 3 AND leaves, got %d", len(parts))
	}
}
