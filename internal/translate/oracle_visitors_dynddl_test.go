package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestVisitOracleDynamicDDL_FoldsAllLiteralConcat(t *testing.T) {
	// 'CREATE TABLE ' || 'foo' || ' (id NUMBER)'
	in := &ast.ExecuteImmediateStmt{
		SQL: &ast.BinaryExpr{
			Op: "||",
			Lhs: &ast.BinaryExpr{
				Op:  "||",
				Lhs: &ast.Literal{Kind: "string", Text: "CREATE TABLE "},
				Rhs: &ast.Literal{Kind: "string", Text: "foo"},
			},
			Rhs: &ast.Literal{Kind: "string", Text: " (id NUMBER)"},
		},
	}
	out := VisitOracleDynamicDDL(in)
	rep, ok := out.(*ast.ExecuteImmediateStmt)
	if !ok {
		t.Fatalf("want ExecuteImmediateStmt, got %T", out)
	}
	lit, ok := rep.SQL.(*ast.Literal)
	if !ok {
		t.Fatalf("folded SQL want *Literal, got %T", rep.SQL)
	}
	want := "CREATE TABLE foo (id NUMBER)"
	if lit.Text != want {
		t.Errorf("folded text: got %q, want %q", lit.Text, want)
	}
	if lit.Kind != "string" {
		t.Errorf("folded kind: got %q, want string", lit.Kind)
	}
}

func TestVisitOracleDynamicDDL_LeavesMixedConcatAlone(t *testing.T) {
	// 'CREATE TABLE ' || v_name || ' (id NUMBER)'
	mixed := &ast.BinaryExpr{
		Op: "||",
		Lhs: &ast.BinaryExpr{
			Op:  "||",
			Lhs: &ast.Literal{Kind: "string", Text: "CREATE TABLE "},
			Rhs: &ast.Ident{Parts: []string{"v_name"}},
		},
		Rhs: &ast.Literal{Kind: "string", Text: " (id NUMBER)"},
	}
	in := &ast.ExecuteImmediateStmt{SQL: mixed}
	out := VisitOracleDynamicDDL(in)
	rep := out.(*ast.ExecuteImmediateStmt)
	// Source must round-trip identically — visitor bails out on
	// non-Literal operands, leaving the runtime concat path in place.
	if rep.SQL != ast.Expr(mixed) {
		t.Errorf("mixed concat tree should not be folded; got %#v", rep.SQL)
	}
}

func TestVisitOracleDynamicDDL_LeavesSingleLiteralAlone(t *testing.T) {
	// EXECUTE IMMEDIATE 'TRUNCATE TABLE foo'
	lit := &ast.Literal{Kind: "string", Text: "TRUNCATE TABLE foo"}
	in := &ast.ExecuteImmediateStmt{SQL: lit}
	out := VisitOracleDynamicDDL(in)
	rep := out.(*ast.ExecuteImmediateStmt)
	// flattenConcat returns []{lit}; allLiteral is true; the fold is
	// a single-Literal copy. The Text round-trips exactly.
	repLit, ok := rep.SQL.(*ast.Literal)
	if !ok {
		t.Fatalf("want *Literal, got %T", rep.SQL)
	}
	if repLit.Text != lit.Text {
		t.Errorf("single-literal round-trip: got %q, want %q", repLit.Text, lit.Text)
	}
}

func TestVisitOracleDynamicDDL_NonExecuteImmediate_NoOp(t *testing.T) {
	id := &ast.Ident{Parts: []string{"x"}}
	if VisitOracleDynamicDDL(id) != ast.Node(id) {
		t.Errorf("non-ExecuteImmediateStmt should not be touched")
	}
}

// TestVisitOracleDynamicDDL_ReparseAndTranslate verifies the full
// Phase 4 path: an all-literal SELECT folds, re-parses via the
// Oracle parser, and renders via the PG writers — producing the
// translated PG SQL inside the Literal.
func TestVisitOracleDynamicDDL_ReparseAndTranslate(t *testing.T) {
	src := &ast.ExecuteImmediateStmt{
		SQL: &ast.BinaryExpr{
			Op:  "||",
			Lhs: &ast.Literal{Kind: "string", Text: "SELECT id FROM "},
			Rhs: &ast.Literal{Kind: "string", Text: "users"},
		},
	}
	out := MakeOracleDynamicDDLVisitor(nil)(src)
	rep, ok := out.(*ast.ExecuteImmediateStmt)
	if !ok {
		t.Fatalf("want ExecuteImmediateStmt, got %T", out)
	}
	lit, ok := rep.SQL.(*ast.Literal)
	if !ok {
		t.Fatalf("want folded Literal, got %T", rep.SQL)
	}
	// The Oracle parser uppercases unquoted idents; the writer emits
	// quoted form. Either way the result must be PG-shaped SELECT.
	if !strings.Contains(lit.Text, "SELECT") {
		t.Errorf("translated SQL missing SELECT: %q", lit.Text)
	}
}

func TestVisitOracleDynamicDDL_PrereqOnMixedConcat(t *testing.T) {
	mixed := &ast.ExecuteImmediateStmt{
		SQL: &ast.BinaryExpr{
			Op:  "||",
			Lhs: &ast.Literal{Kind: "string", Text: "CREATE TABLE "},
			Rhs: &ast.Ident{Parts: []string{"v_name"}},
		},
	}
	var emitted []Prerequisite
	v := MakeOracleDynamicDDLVisitor(func(p Prerequisite) {
		emitted = append(emitted, p)
	})
	out := v(mixed)
	// Mixed concat is left untouched.
	if out != ast.Node(mixed) {
		t.Errorf("mixed concat should not be folded; got %#v", out)
	}
	if len(emitted) != 1 {
		t.Fatalf("want 1 prereq emitted, got %d", len(emitted))
	}
	pq := emitted[0]
	if pq.Severity != SeverityBlocking {
		t.Errorf("Severity want blocking, got %q", pq.Severity)
	}
	if pq.Category != CatManualReview {
		t.Errorf("Category want manual_review, got %q", pq.Category)
	}
	if !strings.Contains(pq.Title, "Dynamic DDL") {
		t.Errorf("Title missing 'Dynamic DDL': %q", pq.Title)
	}
}

// TestVisitOracleDynamicDDL_NoPrereqOnAllLiteral confirms the
// emitter is NOT called when the visitor successfully folds.
func TestVisitOracleDynamicDDL_NoPrereqOnAllLiteral(t *testing.T) {
	in := &ast.ExecuteImmediateStmt{
		SQL: &ast.BinaryExpr{
			Op:  "||",
			Lhs: &ast.Literal{Kind: "string", Text: "DROP TABLE "},
			Rhs: &ast.Literal{Kind: "string", Text: "tmp"},
		},
	}
	var called bool
	v := MakeOracleDynamicDDLVisitor(func(_ Prerequisite) { called = true })
	v(in)
	if called {
		t.Errorf("PrereqEmitter must not fire on all-literal fold")
	}
}

func TestFlattenConcat_BasicShapes(t *testing.T) {
	cases := []struct {
		name string
		in   ast.Expr
		want int // number of leaf operands expected
	}{
		{"nil", nil, 0},
		{"single Literal", &ast.Literal{Kind: "string", Text: "x"}, 1},
		{"single Ident", &ast.Ident{Parts: []string{"x"}}, 1},
		{"two-leaf concat", &ast.BinaryExpr{
			Op:  "||",
			Lhs: &ast.Literal{Kind: "string", Text: "a"},
			Rhs: &ast.Literal{Kind: "string", Text: "b"},
		}, 2},
		{"left-deep concat", &ast.BinaryExpr{
			Op: "||",
			Lhs: &ast.BinaryExpr{
				Op:  "||",
				Lhs: &ast.Literal{Kind: "string", Text: "a"},
				Rhs: &ast.Literal{Kind: "string", Text: "b"},
			},
			Rhs: &ast.Literal{Kind: "string", Text: "c"},
		}, 3},
		{"right-deep with paren", &ast.BinaryExpr{
			Op:  "||",
			Lhs: &ast.Literal{Kind: "string", Text: "a"},
			Rhs: &ast.ParenExpr{
				Inner: &ast.BinaryExpr{
					Op:  "||",
					Lhs: &ast.Literal{Kind: "string", Text: "b"},
					Rhs: &ast.Literal{Kind: "string", Text: "c"},
				},
			},
		}, 3},
		{"non-concat binary", &ast.BinaryExpr{
			Op:  "+",
			Lhs: &ast.Literal{Kind: "number", Text: "1"},
			Rhs: &ast.Literal{Kind: "number", Text: "2"},
		}, 1}, // not concat — root passes through as a single leaf
	}
	for _, c := range cases {
		got := flattenConcat(c.in)
		if len(got) != c.want {
			t.Errorf("%s: got %d leaves, want %d", c.name, len(got), c.want)
		}
	}
}
