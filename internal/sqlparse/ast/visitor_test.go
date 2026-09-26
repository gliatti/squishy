package ast

import "testing"

// identCollector records the first part of every *Ident Walk reaches.
type identCollector struct{ seen map[string]bool }

func (c *identCollector) Visit(n Node) Visitor {
	if id, ok := n.(*Ident); ok && len(id.Parts) > 0 {
		c.seen[id.Parts[0]] = true
	}
	return c
}

func collectIdents(n Node) map[string]bool {
	c := &identCollector{seen: map[string]bool{}}
	Walk(c, n)
	return c.seen
}

// TestWalk_PLCoverage builds a routine body that nests every procedural
// statement kind and checks Walk reaches an identifier placed in each
// expression / query slot.
func TestWalk_PLCoverage(t *testing.T) {
	body := &Block{
		Decls: []PLDecl{
			&DeclareVar{Name: "v", Default: BuildIdent("decl_default")},
			&DeclareCursor{Name: "c", Stmt: selectWhere("cursor_where")},
			&DeclareHandler{Kind: "CONTINUE", Condition: "NOT FOUND",
				Action: &AssignStmt{Target: "done", Expr: BuildIdent("handler_action")}},
		},
		Stmts: []PLStmt{
			&IfStmt{
				Branches: []IfBranch{{
					Cond: BuildIdent("if_cond"),
					Body: []PLStmt{&AssignStmt{Target: "x", Expr: BuildIdent("if_body")}},
				}},
				Else: []PLStmt{&ReturnStmt{Expr: BuildIdent("if_else")}},
			},
			&CaseStmt{
				Expr: BuildIdent("case_expr"),
				When: []CaseWhen{{Match: BuildIdent("case_match"),
					Body: []PLStmt{&CallStmt{Name: "p", Args: []Expr{BuildIdent("case_call")}}}}},
				Else: []PLStmt{&LeaveStmt{Label: "l", WhenCond: BuildIdent("leave_when")}},
			},
			&WhileStmt{Cond: BuildIdent("while_cond"),
				Body: []PLStmt{&IterateStmt{Label: "l", WhenCond: BuildIdent("iterate_when")}}},
			&LoopStmt{Body: []PLStmt{&SelectInto{Vars: []string{"n"}, Stmt: selectWhere("select_into")}}},
			&RepeatStmt{
				Body: []PLStmt{&AssignStmt{Target: "y", Expr: BuildIdent("repeat_body")}},
				Cond: BuildIdent("repeat_until"),
			},
		},
		Except: &ExceptionBlock{Handlers: []ExceptionHandler{{
			Names: []string{"OTHERS"},
			Body:  []PLStmt{&AssignStmt{Target: "err", Expr: BuildIdent("except_body")}},
		}}},
	}

	seen := collectIdents(body)
	for _, want := range []string{
		"decl_default", "cursor_where", "handler_action",
		"if_cond", "if_body", "if_else",
		"case_expr", "case_match", "case_call", "leave_when",
		"while_cond", "iterate_when",
		"select_into",
		"repeat_body", "repeat_until",
		"except_body",
	} {
		if !seen[want] {
			t.Errorf("Walk did not reach ident %q", want)
		}
	}
}

// TestWalk_DMLAndDDLSlots covers the DML / DDL slots added alongside the
// PL coverage: CTE bodies, aggregate ORDER BY / SEPARATOR, INTERVAL
// expressions, typed view bodies and event AT expressions.
func TestWalk_DMLAndDDLSlots(t *testing.T) {
	sel := &SelectStmt{
		With: &WithClause{CTEs: []CTE{{Name: "c", Body: selectWhere("cte_where")}}},
		Cols: []SelectItem{
			{Expr: &FuncCall{
				Name:         "GROUP_CONCAT",
				Args:         []Expr{BuildIdent("agg_arg")},
				AggOrderBy:   []OrderItem{{Expr: BuildIdent("agg_order")}},
				AggSeparator: BuildIdent("agg_sep"),
			}},
			{Expr: &IntervalLit{Unit: "DAY", Expr: BuildIdent("interval_expr")}},
		},
	}
	view := &CreateView{View: TableRef{Name: "v"}, Select: sel}
	event := &CreateEvent{Name: "e", ScheduleKind: "AT", AtExpr: BuildIdent("event_at")}

	seen := collectIdents(view)
	for k := range collectIdents(event) {
		seen[k] = true
	}
	for _, want := range []string{"cte_where", "agg_arg", "agg_order", "agg_sep", "interval_expr", "event_at"} {
		if !seen[want] {
			t.Errorf("Walk did not reach ident %q", want)
		}
	}
}

// TestWalk_NilSlots — typed-nil query pointers (a cursor or view whose
// body failed to parse) must not reach Visit or panic.
func TestWalk_NilSlots(t *testing.T) {
	var visited int
	v := visitFunc(func(n Node) { visited++ })
	Walk(v, &Block{Decls: []PLDecl{&DeclareCursor{Name: "c"}}})
	Walk(v, &CreateView{View: TableRef{Name: "v"}})
	Walk(v, &SelectInto{Vars: []string{"n"}})
	Walk(v, &SelectStmt{SetOps: []SetOp{{Op: "UNION"}}})
	// Block + DeclareCursor, CreateView, SelectInto, SelectStmt.
	if visited != 5 {
		t.Errorf("visited %d nodes, want 5", visited)
	}
}

// TestWalk_Prune — returning nil from Visit skips the subtree.
func TestWalk_Prune(t *testing.T) {
	body := &Block{Stmts: []PLStmt{&IfStmt{Branches: []IfBranch{{Cond: BuildIdent("hidden")}}}}}
	var seen []string
	Walk(pruneIf{seen: &seen}, body)
	for _, s := range seen {
		if s == "hidden" {
			t.Errorf("pruned subtree was visited")
		}
	}
}

type visitFunc func(n Node)

func (f visitFunc) Visit(n Node) Visitor {
	f(n)
	return f
}

type pruneIf struct{ seen *[]string }

func (p pruneIf) Visit(n Node) Visitor {
	if _, ok := n.(*IfStmt); ok {
		return nil
	}
	if id, ok := n.(*Ident); ok {
		*p.seen = append(*p.seen, id.Parts[0])
	}
	return p
}
