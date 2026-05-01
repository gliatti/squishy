package ast

import "testing"

// TestRewrite_ReplacesFuncCall pins the basic post-order substitution
// behaviour: a Rewriter that rewrites every `decode(...)` to
// `oracle.decode(...)` runs once and the parent expression sees the
// substituted child.
func TestRewrite_ReplacesFuncCall(t *testing.T) {
	root := BuildBinary("+",
		BuildFuncCall("decode", BuildIdent("x"), BuildIntLit(1), BuildStringLit("a")),
		BuildIntLit(2),
	)

	out := Rewrite(root, func(n Node) Node {
		if fc, ok := IsFuncCallNamed(n, "decode"); ok {
			return BuildFuncCall("oracle.decode", fc.Args...)
		}
		return n
	})

	bin, ok := out.(*BinaryExpr)
	if !ok {
		t.Fatalf("root want *BinaryExpr, got %T", out)
	}
	fc, ok := bin.Lhs.(*FuncCall)
	if !ok {
		t.Fatalf("Lhs want *FuncCall, got %T", bin.Lhs)
	}
	if fc.Name != "oracle.decode" {
		t.Errorf("decode rewrite: Name want oracle.decode, got %q", fc.Name)
	}
	// Args preserved verbatim — the rewriter just renamed.
	if len(fc.Args) != 3 {
		t.Errorf("Args length want 3, got %d", len(fc.Args))
	}
}

// TestRewrite_PostOrder confirms children are visited before parents.
// We count the call order against a known tree shape — if the parent
// is visited before its child, the test fails.
func TestRewrite_PostOrder(t *testing.T) {
	root := BuildBinary("+",
		BuildFuncCall("inner"),
		BuildIdent("x"),
	)
	var order []string
	Rewrite(root, func(n Node) Node {
		switch n.(type) {
		case *FuncCall:
			order = append(order, "func")
		case *Ident:
			order = append(order, "ident")
		case *BinaryExpr:
			order = append(order, "binary")
		}
		return n
	})
	// children first, parent last
	want := []string{"func", "ident", "binary"}
	if len(order) != len(want) {
		t.Fatalf("visit count: want %d, got %d (%v)", len(want), len(order), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("visit[%d]: want %s, got %s (full %v)", i, want[i], order[i], order)
		}
	}
}

// TestRewrite_NilSafe — Rewrite returns nil for nil input and a
// Rewriter applied to a node with nil sub-slots leaves them nil.
func TestRewrite_NilSafe(t *testing.T) {
	if Rewrite(nil, func(n Node) Node { return n }) != nil {
		t.Errorf("Rewrite(nil) must return nil")
	}
	// BinaryExpr with nil Lhs (defensive — parser doesn't produce this
	// but visitors during a multi-pass run might).
	bin := &BinaryExpr{Op: "+", Lhs: nil, Rhs: BuildIntLit(1)}
	out := Rewrite(bin, func(n Node) Node { return n })
	if rb, ok := out.(*BinaryExpr); !ok || rb.Lhs != nil {
		t.Errorf("nil Lhs preserved: got %#v", out)
	}
}

// TestCompose_ChainsRewriters — Compose applies its arguments
// left-to-right on each node visit.
func TestCompose_ChainsRewriters(t *testing.T) {
	root := BuildFuncCall("decode")
	r1 := func(n Node) Node {
		if fc, ok := IsFuncCallNamed(n, "decode"); ok {
			return BuildFuncCall("step1.decode", fc.Args...)
		}
		return n
	}
	r2 := func(n Node) Node {
		if fc, ok := IsFuncCallNamed(n, "step1.decode"); ok {
			return BuildFuncCall("step2.decode", fc.Args...)
		}
		return n
	}
	chain := Compose(r1, r2)
	out := Rewrite(root, chain)
	fc, ok := out.(*FuncCall)
	if !ok || fc.Name != "step2.decode" {
		t.Errorf("compose chain: want step2.decode, got %#v", out)
	}
}

// TestIsFuncCallNamed — case-insensitive name match.
func TestIsFuncCallNamed(t *testing.T) {
	fc := BuildFuncCall("Decode")
	if _, ok := IsFuncCallNamed(fc, "decode"); !ok {
		t.Errorf("case-insensitive match failed")
	}
	if _, ok := IsFuncCallNamed(fc, "encode"); ok {
		t.Errorf("name mismatch should not match")
	}
	if _, ok := IsFuncCallNamed(BuildIdent("decode"), "decode"); ok {
		t.Errorf("ident must not match FuncCall pattern")
	}
}
