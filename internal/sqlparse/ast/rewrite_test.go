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

// TestRewrite_ReachesTSQLSlots pins the descent into every expression /
// statement slot added for the T-SQL dialect (new nodes and new fields of
// existing nodes): one case per slot, read back through the root after the
// renaming rewriter ran.
func TestRewrite_ReachesTSQLSlots(t *testing.T) {
	type tc struct {
		name string
		root Node
		get  func() Expr
	}
	var cases []tc

	{ // a LIKE 'x' ESCAPE e
		root := &BinaryExpr{Op: "LIKE", Lhs: BuildIdent("a"), Rhs: BuildStringLit("x"), Escape: BuildIdent("e")}
		cases = append(cases, tc{"BinaryExpr.Escape", root, func() Expr { return root.Escape }})
	}
	{ // CONVERT(int, x, s)
		root := &CastExpr{Expr: BuildIdent("x"), Type: &IntType{Name: "INT"}, IsConvert: true, ConvertStyle: BuildIdent("s")}
		cases = append(cases,
			tc{"CastExpr.Expr", root, func() Expr { return root.Expr }},
			tc{"CastExpr.ConvertStyle", root, func() Expr { return root.ConvertStyle }},
		)
	}
	{ // c COLLATE x
		root := &CollateExpr{Expr: BuildIdent("c"), Collation: "Latin1_General_BIN"}
		cases = append(cases, tc{"CollateExpr.Expr", root, func() Expr { return root.Expr }})
	}
	{ // col.GetAncestor(n)
		root := &MethodCall{Target: BuildIdent("col"), Method: "GetAncestor", Args: []Expr{BuildIdent("n")}}
		cases = append(cases,
			tc{"MethodCall.Target", root, func() Expr { return root.Target }},
			tc{"MethodCall.Args", root, func() Expr { return root.Args[0] }},
		)
	}
	{ // EXEC p @x = v
		root := &CallStmt{Name: "p", Args: []Expr{&NamedArg{Name: "@x", Value: BuildIdent("v")}}}
		cases = append(cases, tc{"NamedArg.Value", root, func() Expr { return root.Args[0].(*NamedArg).Value }})
	}
	{ // SELECT TOP (n) * FROM f(x), (…) q PIVOT (SUM(g) …), (VALUES (v)) vv
		pivot := &FromPivot{Source: &FromSubquery{Stmt: selectWhere("s"), Alias: "q"}, Agg: BuildFuncCall("SUM", BuildIdent("g")), ForCol: "y", InCols: []string{"1"}}
		root := &SelectStmt{
			Top:  BuildIdent("n"),
			Cols: []SelectItem{{Star: true}},
			From: []FromItem{
				&FromFunc{Func: BuildFuncCall("f", BuildIdent("x")), Alias: "f"},
				pivot,
				&FromValues{Rows: [][]Expr{{BuildIdent("v")}}, Alias: "vv"},
			},
		}
		cases = append(cases,
			tc{"SelectStmt.Top", root, func() Expr { return root.Top }},
			tc{"FromFunc.Func", root, func() Expr { return root.From[0].(*FromFunc).Func.(*FuncCall).Args[0] }},
			tc{"FromPivot.Source", root, func() Expr { return whereLhs(root.From[1].(*FromPivot).Source.(*FromSubquery).Stmt) }},
			tc{"FromPivot.Agg", root, func() Expr { return root.From[1].(*FromPivot).Agg.(*FuncCall).Args[0] }},
			tc{"FromValues.Rows", root, func() Expr { return root.From[2].(*FromValues).Rows[0][0] }},
		)
	}
	{ // WITH c AS (…) INSERT TOP (n) INTO t EXEC p a
		root := &InsertStmt{
			Table: TableRef{Name: "t"},
			With:  &WithClause{CTEs: []CTE{{Name: "c", Body: selectWhere("w")}}},
			Top:   BuildIdent("n"),
			Exec:  &CallStmt{Name: "p", Args: []Expr{BuildIdent("a")}},
		}
		cases = append(cases,
			tc{"InsertStmt.With", root, func() Expr { return whereLhs(root.With.CTEs[0].Body) }},
			tc{"InsertStmt.Top", root, func() Expr { return root.Top }},
			tc{"InsertStmt.Exec", root, func() Expr { return root.Exec.(*CallStmt).Args[0] }},
		)
	}
	{ // WITH … UPDATE TOP (n) t SET c = 1 / WITH … DELETE TOP (m) t
		upd := &UpdateStmt{
			Table: TableRef{Name: "t"},
			With:  &WithClause{CTEs: []CTE{{Name: "c", Body: selectWhere("w")}}},
			Top:   BuildIdent("n"),
			Sets:  []Assign{{Col: "c", Expr: BuildIntLit(1)}},
		}
		del := &DeleteStmt{
			Table: TableRef{Name: "t"},
			With:  &WithClause{CTEs: []CTE{{Name: "c", Body: selectWhere("w")}}},
			Top:   BuildIdent("m"),
		}
		cases = append(cases,
			tc{"UpdateStmt.With", upd, func() Expr { return whereLhs(upd.With.CTEs[0].Body) }},
			tc{"UpdateStmt.Top", upd, func() Expr { return upd.Top }},
			tc{"DeleteStmt.With", del, func() Expr { return whereLhs(del.With.CTEs[0].Body) }},
			tc{"DeleteStmt.Top", del, func() Expr { return del.Top }},
		)
	}
	{ // MERGE … WHEN MATCHED AND c THEN UPDATE SET x = s … BY SOURCE AND d THEN DELETE
		root := &MergeStmt{
			Target: TableRef{Name: "t"},
			Source: &FromTable{Name: "s"},
			On:     BuildIdent("on"),
			With:   &WithClause{CTEs: []CTE{{Name: "c", Body: selectWhere("w")}}},
			Top:    BuildIdent("n"),
			WhenMatched: []MergeAction{{Kind: "UPDATE", Cond: BuildIdent("c"),
				Sets: []Assign{{Col: "x", Expr: BuildIdent("s")}}}},
			WhenNotMatched:         []MergeAction{{Kind: "INSERT", InsertCols: []string{"x"}, InsertValues: []Expr{BuildIdent("i")}}},
			WhenNotMatchedBySource: []MergeAction{{Kind: "DELETE", Cond: BuildIdent("d")}},
		}
		cases = append(cases,
			tc{"MergeStmt.With", root, func() Expr { return whereLhs(root.With.CTEs[0].Body) }},
			tc{"MergeStmt.Top", root, func() Expr { return root.Top }},
			tc{"MergeStmt.WhenMatched.Cond", root, func() Expr { return root.WhenMatched[0].Cond }},
			tc{"MergeStmt.WhenMatched.Sets", root, func() Expr { return root.WhenMatched[0].Sets[0].Expr }},
			tc{"MergeStmt.WhenNotMatched.InsertValues", root, func() Expr { return root.WhenNotMatched[0].InsertValues[0] }},
			tc{"MergeStmt.WhenNotMatchedBySource.Cond", root, func() Expr { return root.WhenNotMatchedBySource[0].Cond }},
		)
	}
	{ // EXEC sp_executesql @s, @params, @p
		root := &ExecuteImmediateStmt{SQL: BuildIdent("s"), ParamDecl: BuildIdent("params"), Using: []Expr{BuildIdent("p")}, SPExecuteSQL: true}
		cases = append(cases, tc{"ExecuteImmediateStmt.ParamDecl", root, func() Expr { return root.ParamDecl }})
	}
	{ // RAISERROR (m, sev, st, a) / THROW n, …
		root := &RaiseStmt{Kind: "RAISERROR", Message: BuildIdent("m"), ErrNumber: BuildIdent("n"),
			Severity: BuildIdent("sev"), State: BuildIdent("st"), Args: []Expr{BuildIdent("a")}}
		cases = append(cases,
			tc{"RaiseStmt.Message", root, func() Expr { return root.Message }},
			tc{"RaiseStmt.ErrNumber", root, func() Expr { return root.ErrNumber }},
			tc{"RaiseStmt.Severity", root, func() Expr { return root.Severity }},
			tc{"RaiseStmt.State", root, func() Expr { return root.State }},
			tc{"RaiseStmt.Args", root, func() Expr { return root.Args[0] }},
		)
	}
	{ // FETCH ABSOLUTE n FROM c
		root := &FetchStmt{Cursor: "c", Direction: "ABSOLUTE", Offset: BuildIdent("n")}
		cases = append(cases, tc{"FetchStmt.Offset", root, func() Expr { return root.Offset }})
	}
	{ // DECLARE @a int = d
		root := &DeclareStmt{Decls: []PLDecl{&DeclareVar{Name: "@a", Type: &IntType{Name: "INT"}, Default: BuildIdent("d")}}}
		cases = append(cases, tc{"DeclareStmt.Decls", root, func() Expr { return root.Decls[0].(*DeclareVar).Default }})
	}
	{ // CREATE INDEX … WHERE w (top level and inside a body);
		// CREATE FUNCTION … RETURN (SELECT …); ALTER TABLE … ADD DEFAULT d FOR c
		idx := &CreateIndex{Name: "ix", Where: BuildIdent("w")}
		emb := &EmbeddedDDL{Stmt: &CreateIndex{Name: "ix2", Where: BuildIdent("w2")}}
		fn := &CreateFunction{Name: "f", ReturnsInlineTable: true, Select: selectWhere("q")}
		alt := &AlterTable{Table: TableRef{Name: "t"}, Actions: []AlterAction{{Kind: "SET_DEFAULT", DropName: "c", Default: BuildIdent("d")}}}
		cases = append(cases,
			tc{"CreateIndex.Where", idx, func() Expr { return idx.Where }},
			tc{"EmbeddedDDL.Stmt", emb, func() Expr { return emb.Stmt.(*CreateIndex).Where }},
			tc{"CreateFunction.Select", fn, func() Expr { return whereLhs(fn.Select) }},
			tc{"AlterTable.Actions.Default", alt, func() Expr { return alt.Actions[0].Default }},
		)
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

// TestRewrite_TSQLLeaves checks that the T-SQL leaf nodes are handed to
// the rewriter without a dedicated case.
func TestRewrite_TSQLLeaves(t *testing.T) {
	for _, n := range []Node{
		&CommentStmt{ObjectKind: "TABLE"}, &CreateSchema{Name: "s"}, &TransactionStmt{Kind: "COMMIT"},
		&LabelStmt{Label: "l"}, &NullStmt{Source: "SET NOCOUNT ON"},
	} {
		seen := false
		Rewrite(n, func(m Node) Node {
			if m == n {
				seen = true
			}
			return m
		})
		if !seen {
			t.Errorf("%T not handed to the rewriter", n)
		}
	}
}
