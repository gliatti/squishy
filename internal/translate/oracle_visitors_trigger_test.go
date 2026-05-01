package translate

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestMakeRowAliasVisitor_RenamesIdentFirstPart(t *testing.T) {
	v := MakeRowAliasVisitor("NR", "NEW")

	id := &ast.Ident{Parts: []string{"NR", "QTY"}}
	out := v(id)
	rep, ok := out.(*ast.Ident)
	if !ok {
		t.Fatalf("expected Ident, got %T", out)
	}
	if rep.Parts[0] != "NEW" || rep.Parts[1] != "QTY" {
		t.Errorf("Parts = %v, want [NEW QTY]", rep.Parts)
	}
	// Source must not be mutated in place — the original is shared.
	if id.Parts[0] != "NR" {
		t.Errorf("source Ident mutated: Parts=%v", id.Parts)
	}
}

func TestMakeRowAliasVisitor_CaseInsensitive(t *testing.T) {
	v := MakeRowAliasVisitor("NR", "NEW")
	id := &ast.Ident{Parts: []string{"nr", "qty"}}
	out := v(id)
	rep, _ := out.(*ast.Ident)
	if rep == nil || rep.Parts[0] != "NEW" {
		t.Errorf("case-insensitive match failed: %v", out)
	}
}

func TestMakeRowAliasVisitor_DoesNotTouchOtherIdents(t *testing.T) {
	v := MakeRowAliasVisitor("NR", "NEW")
	cases := []*ast.Ident{
		{Parts: []string{"NRC"}},          // NR is a substring, not a whole part
		{Parts: []string{"NREVENUE"}},     // NR is a prefix, not a whole part
		{Parts: []string{"a", "NR"}},      // NR is in second position, not first
		{Parts: []string{"someone_else"}}, // unrelated
	}
	for _, id := range cases {
		out := v(id)
		if out != ast.Node(id) {
			t.Errorf("ident %v should not be rewritten, got %v", id.Parts, out)
		}
	}
}

func TestMakeRowAliasVisitor_NoopForEmptyOrSelfRename(t *testing.T) {
	id := &ast.Ident{Parts: []string{"NEW", "qty"}}
	// Empty `from` → identity.
	if MakeRowAliasVisitor("", "NEW")(id) != ast.Node(id) {
		t.Errorf("empty from must be a no-op")
	}
	// Self-rename → identity.
	if MakeRowAliasVisitor("NEW", "NEW")(id) != ast.Node(id) {
		t.Errorf("self-rename must be a no-op")
	}
	if MakeRowAliasVisitor("new", "NEW")(id) != ast.Node(id) {
		t.Errorf("case-insensitive self-rename must be a no-op")
	}
}

func TestMakeRowAliasVisitor_RewriteIntegration(t *testing.T) {
	// Mimic the IF NR.QTY < OR1.QTY THEN NULL; END IF; case the
	// Phase 1.1 IfStmt typing was deferred for. After running the
	// trigger-alias composite, the body should have NEW/OLD in place.
	cond := &ast.BinaryExpr{
		Op:  "<",
		Lhs: &ast.Ident{Parts: []string{"NR", "QTY"}},
		Rhs: &ast.Ident{Parts: []string{"OR1", "QTY"}},
	}
	composite := MakeTriggerAliasComposite("NR", "OR1")
	out := ast.Rewrite(cond, composite)
	bin, ok := out.(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("root: want *BinaryExpr, got %T", out)
	}
	lhs, ok := bin.Lhs.(*ast.Ident)
	if !ok || lhs.Parts[0] != "NEW" {
		t.Errorf("Lhs: want NEW.<col>, got %v", bin.Lhs)
	}
	rhs, ok := bin.Rhs.(*ast.Ident)
	if !ok || rhs.Parts[0] != "OLD" {
		t.Errorf("Rhs: want OLD.<col>, got %v", bin.Rhs)
	}
}
