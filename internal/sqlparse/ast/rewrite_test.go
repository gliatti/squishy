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

// renameIdents returns a Rewriter that replaces every *Ident with a
// fresh *Ident whose parts carry the "r_" prefix. A fresh node (rather
// than an in-place mutation) proves that Rewrite stores the substituted
// value back into the parent's slot.
func renameIdents() Rewriter {
	return func(n Node) Node {
		id, ok := n.(*Ident)
		if !ok {
			return n
		}
		parts := make([]string, len(id.Parts))
		for i, p := range id.Parts {
			parts[i] = "r_" + p
		}
		return &Ident{Parts: parts, Backtick: id.Backtick, P: id.P}
	}
}

// identName returns the single part of a one-part *Ident and fails the
// test for anything else.
func identName(t *testing.T, e Expr) string {
	t.Helper()
	id, ok := e.(*Ident)
	if !ok || len(id.Parts) != 1 {
		t.Fatalf("want one-part *Ident, got %#v", e)
	}
	return id.Parts[0]
}

// selectWhere builds `SELECT <col> FROM t WHERE <col> = 1`; the Where
// Lhs is the identifier the rename tests look at.
func selectWhere(col string) *SelectStmt {
	return &SelectStmt{
		Cols:  []SelectItem{{Expr: BuildIdent(col)}},
		From:  []FromItem{&FromTable{Name: "t"}},
		Where: BuildBinary("=", BuildIdent(col), BuildIntLit(1)),
	}
}

func whereLhs(s *SelectStmt) Expr { return s.Where.(*BinaryExpr).Lhs }

// TestRewrite_ReachesAllSlots pins the descent into every slot the
// MySQL → PG visitors rely on. Each case builds a root, runs the
// renaming rewriter over it, and reads the slot back through the root.
func TestRewrite_ReachesAllSlots(t *testing.T) {
	type tc struct {
		name string
		root Node
		get  func() Expr
	}
	var cases []tc

	{ // WITH c AS (SELECT …) SELECT * FROM c
		root := &SelectStmt{
			With: &WithClause{CTEs: []CTE{{Name: "c", Body: selectWhere("a")}}},
			Cols: []SelectItem{{Star: true}},
			From: []FromItem{&FromTable{Name: "c"}},
		}
		cases = append(cases, tc{"CTE body", root, func() Expr { return whereLhs(root.With.CTEs[0].Body) }})
	}
	{ // INSERT … ON CONFLICT (id) DO UPDATE SET c = a WHERE b RETURNING d
		root := &InsertStmt{
			Table:  TableRef{Name: "t"},
			Values: [][]Expr{{BuildIntLit(1)}},
			OnConflict: &OnConflict{
				Target: []string{"id"},
				Sets:   []Assign{{Col: "c", Expr: BuildIdent("a")}},
				Where:  BuildIdent("b"),
			},
			Returning: []SelectItem{{Expr: BuildIdent("d")}},
		}
		cases = append(cases,
			tc{"OnConflict.Sets", root, func() Expr { return root.OnConflict.Sets[0].Expr }},
			tc{"OnConflict.Where", root, func() Expr { return root.OnConflict.Where }},
			tc{"Insert.Returning", root, func() Expr { return root.Returning[0].Expr }},
		)
	}
	{ // UPDATE t SET c = 1 FROM (SELECT …) s RETURNING e
		root := &UpdateStmt{
			Table:     TableRef{Name: "t"},
			Sets:      []Assign{{Col: "c", Expr: BuildIntLit(1)}},
			From:      []FromItem{&FromSubquery{Stmt: selectWhere("a"), Alias: "s"}},
			Returning: []SelectItem{{Expr: BuildIdent("e")}},
		}
		cases = append(cases,
			tc{"UPDATE … FROM subquery", root, func() Expr { return whereLhs(root.From[0].(*FromSubquery).Stmt) }},
			tc{"Update.Returning", root, func() Expr { return root.Returning[0].Expr }},
		)
	}
	{ // DELETE FROM t USING (SELECT …) s RETURNING f
		root := &DeleteStmt{
			Table:     TableRef{Name: "t"},
			Using:     []FromItem{&FromSubquery{Stmt: selectWhere("a"), Alias: "s"}},
			Returning: []SelectItem{{Expr: BuildIdent("f")}},
		}
		cases = append(cases,
			tc{"DELETE … USING subquery", root, func() Expr { return whereLhs(root.Using[0].(*FromSubquery).Stmt) }},
			tc{"Delete.Returning", root, func() Expr { return root.Returning[0].Expr }},
		)
	}
	{ // GROUP_CONCAT(x ORDER BY y DESC SEPARATOR z)
		fc := &FuncCall{
			Name:         "GROUP_CONCAT",
			Args:         []Expr{BuildIdent("x")},
			AggOrderBy:   []OrderItem{{Expr: BuildIdent("y"), Desc: true}},
			AggSeparator: BuildIdent("z"),
		}
		root := &SelectStmt{Cols: []SelectItem{{Expr: fc}}}
		cases = append(cases,
			tc{"FuncCall.AggOrderBy", root, func() Expr { return root.Cols[0].Expr.(*FuncCall).AggOrderBy[0].Expr }},
			tc{"FuncCall.AggSeparator", root, func() Expr { return root.Cols[0].Expr.(*FuncCall).AggSeparator }},
		)
	}
	{ // SUM(v) OVER (PARTITION BY p ORDER BY o)
		root := &WindowedAgg{
			Func: BuildFuncCall("SUM", BuildIdent("v")),
			Over: &WindowSpec{
				PartitionBy: []Expr{BuildIdent("p")},
				OrderBy:     []OrderItem{{Expr: BuildIdent("o")}},
			},
		}
		cases = append(cases,
			tc{"Over.PartitionBy", root, func() Expr { return root.Over.PartitionBy[0] }},
			tc{"Over.OrderBy", root, func() Expr { return root.Over.OrderBy[0].Expr }},
		)
	}
	{ // INTERVAL (n + 1) DAY
		root := &IntervalLit{Unit: "DAY", Expr: BuildBinary("+", BuildIdent("n"), BuildIntLit(1))}
		cases = append(cases, tc{"IntervalLit.Expr", root, func() Expr { return root.Expr.(*BinaryExpr).Lhs }})
	}
	{ // REPEAT SET i = k; UNTIL done END REPEAT
		root := &RepeatStmt{
			Body: []PLStmt{&AssignStmt{Target: "i", Expr: BuildIdent("k")}},
			Cond: BuildIdent("done"),
		}
		cases = append(cases,
			tc{"RepeatStmt.Cond", root, func() Expr { return root.Cond }},
			tc{"RepeatStmt.Body", root, func() Expr { return root.Body[0].(*AssignStmt).Expr }},
		)
	}
	{ // CALL p(a)
		root := &CallStmt{Name: "p", Args: []Expr{BuildIdent("a")}}
		cases = append(cases, tc{"CallStmt.Args", root, func() Expr { return root.Args[0] }})
	}
	{ // DECLARE c CURSOR FOR SELECT …; DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = h;
		root := &Block{Decls: []PLDecl{
			&DeclareCursor{Name: "c", Stmt: selectWhere("a")},
			&DeclareHandler{Kind: "CONTINUE", Condition: "NOT FOUND",
				Action: &AssignStmt{Target: "done", Expr: BuildIdent("h")}},
		}}
		cases = append(cases,
			tc{"DeclareCursor.Stmt", root, func() Expr { return whereLhs(root.Decls[0].(*DeclareCursor).Stmt) }},
			tc{"DeclareHandler.Action", root, func() Expr { return root.Decls[1].(*DeclareHandler).Action.(*AssignStmt).Expr }},
		)
	}
	{ // CREATE VIEW v AS SELECT …
		root := &CreateView{View: TableRef{Name: "v"}, Select: selectWhere("a")}
		cases = append(cases, tc{"CreateView.Select", root, func() Expr { return whereLhs(root.Select) }})
	}
	{ // CREATE EVENT e ON SCHEDULE AT ts DO …
		root := &CreateEvent{Name: "e", ScheduleKind: "AT", AtExpr: BuildIdent("ts")}
		cases = append(cases, tc{"CreateEvent.AtExpr", root, func() Expr { return root.AtExpr }})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := Rewrite(c.root, renameIdents()); out != c.root {
				t.Fatalf("root identity changed: got %T", out)
			}
			name := identName(t, c.get())
			if len(name) < 2 || name[:2] != "r_" {
				t.Errorf("ident not rewritten: got %q", name)
			}
		})
	}
}

// TestRewrite_AggOrderByPostOrder checks that the new FuncCall slots keep
// the post-order contract: arguments, then the aggregate ORDER BY, then
// the call itself.
func TestRewrite_AggOrderByPostOrder(t *testing.T) {
	fc := &FuncCall{
		Name:       "string_agg",
		Args:       []Expr{BuildIdent("x"), BuildStringLit(",")},
		AggOrderBy: []OrderItem{{Expr: BuildIdent("y")}},
	}
	var order []string
	Rewrite(fc, func(n Node) Node {
		switch x := n.(type) {
		case *Ident:
			order = append(order, x.Parts[0])
		case *FuncCall:
			order = append(order, "func")
		}
		return n
	})
	want := []string{"x", "y", "func"}
	if len(order) != len(want) {
		t.Fatalf("visit order: want %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("visit order: want %v, got %v", want, order)
		}
	}
}
