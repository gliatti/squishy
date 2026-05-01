package ast

// Rewriter is a function that may replace a node with another. It runs in
// post-order: every child is rewritten before its parent. Returning n
// itself (or its address) means "no change". Returning a different Node
// substitutes the value at the parent's slot.
//
// Rewriters compose by chaining — see Compose.
type Rewriter func(n Node) Node

// Rewrite traverses n in post-order, applying fn to every node and using
// the returned value at the parent's slot. The return value of Rewrite is
// the rewritten root. nil input returns nil.
//
// Rewrite is the AST-only counterpart to translator.rawExpr's text
// rendering: every translation pass that wants to substitute a typed
// node (FuncCall("decode", …) → CASE, BinaryExpr{Op:"MOD"} → mod(…))
// implements a Rewriter and lets Rewrite walk the tree once.
//
// Coverage targets the Expr / Stmt / PLStmt subtrees the Phase 3 visitors
// touch. PLDecl and DDL nodes are walked enough to descend into the
// expressions they hold (DEFAULT clauses, CHECK constraints, …) but the
// rewriter never replaces a PLDecl outright — those have no expression-
// position equivalent, and the visitors operate on whatever expressions
// they expose.
func Rewrite(n Node, fn Rewriter) Node {
	if n == nil {
		return nil
	}
	switch x := n.(type) {

	// -- Expressions --------------------------------------------------

	case *Literal, *Ident, *RawExpr, *CursorAttr, *SequenceRef:
		// Leaves — no children to descend into. Just hand to fn.
		return fn(x)
	case *BinaryExpr:
		x.Lhs = rewriteExpr(x.Lhs, fn)
		x.Rhs = rewriteExpr(x.Rhs, fn)
		return fn(x)
	case *UnaryExpr:
		x.Rhs = rewriteExpr(x.Rhs, fn)
		return fn(x)
	case *ParenExpr:
		x.Inner = rewriteExpr(x.Inner, fn)
		return fn(x)
	case *FuncCall:
		for i, a := range x.Args {
			x.Args[i] = rewriteExpr(a, fn)
		}
		return fn(x)
	case *CaseExpr:
		x.Operand = rewriteExpr(x.Operand, fn)
		for i, w := range x.Whens {
			x.Whens[i].Match = rewriteExpr(w.Match, fn)
			x.Whens[i].Then = rewriteExpr(w.Then, fn)
		}
		x.Else = rewriteExpr(x.Else, fn)
		return fn(x)
	case *BetweenExpr:
		x.Expr = rewriteExpr(x.Expr, fn)
		x.Low = rewriteExpr(x.Low, fn)
		x.High = rewriteExpr(x.High, fn)
		return fn(x)
	case *InExpr:
		x.Expr = rewriteExpr(x.Expr, fn)
		for i, e := range x.List {
			x.List[i] = rewriteExpr(e, fn)
		}
		if x.Subquery != nil {
			if rs, ok := Rewrite(x.Subquery, fn).(*SelectStmt); ok {
				x.Subquery = rs
			}
		}
		return fn(x)
	case *ExistsExpr:
		if x.Subquery != nil {
			if rs, ok := Rewrite(x.Subquery, fn).(*SelectStmt); ok {
				x.Subquery = rs
			}
		}
		return fn(x)
	case *SubqueryExpr:
		if x.Stmt != nil {
			if rs, ok := Rewrite(x.Stmt, fn).(*SelectStmt); ok {
				x.Stmt = rs
			}
		}
		return fn(x)
	case *WindowedAgg:
		if x.Func != nil {
			if rf, ok := Rewrite(x.Func, fn).(*FuncCall); ok {
				x.Func = rf
			}
		}
		for i := range x.Within {
			x.Within[i].Expr = rewriteExpr(x.Within[i].Expr, fn)
		}
		// Over (window spec) carries Idents and Exprs but we don't
		// recurse into it for now — the Phase 3 visitors that touch
		// windowing rebuild the spec from scratch when needed.
		return fn(x)
	case *OuterJoinHint:
		x.Inner = rewriteExpr(x.Inner, fn)
		return fn(x)
	case *CastExpr:
		x.Expr = rewriteExpr(x.Expr, fn)
		return fn(x)
	case *IntervalLit:
		return fn(x)

	// -- Statements ---------------------------------------------------

	case *SelectStmt:
		for i := range x.Cols {
			x.Cols[i].Expr = rewriteExpr(x.Cols[i].Expr, fn)
		}
		for i, f := range x.From {
			if rf, ok := Rewrite(f, fn).(FromItem); ok {
				x.From[i] = rf
			}
		}
		x.Where = rewriteExpr(x.Where, fn)
		for i, e := range x.GroupBy {
			x.GroupBy[i] = rewriteExpr(e, fn)
		}
		x.Having = rewriteExpr(x.Having, fn)
		for i := range x.OrderBy {
			x.OrderBy[i].Expr = rewriteExpr(x.OrderBy[i].Expr, fn)
		}
		x.Limit = rewriteExpr(x.Limit, fn)
		x.Offset = rewriteExpr(x.Offset, fn)
		for i, so := range x.SetOps {
			if rs, ok := Rewrite(so.Stmt, fn).(*SelectStmt); ok {
				x.SetOps[i].Stmt = rs
			}
		}
		return fn(x)
	case *FromSubquery:
		if x.Stmt != nil {
			if rs, ok := Rewrite(x.Stmt, fn).(*SelectStmt); ok {
				x.Stmt = rs
			}
		}
		return fn(x)
	case *FromJoin:
		if rl, ok := Rewrite(x.Left, fn).(FromItem); ok {
			x.Left = rl
		}
		if rr, ok := Rewrite(x.Right, fn).(FromItem); ok {
			x.Right = rr
		}
		x.On = rewriteExpr(x.On, fn)
		return fn(x)
	case *InsertStmt:
		for i, row := range x.Values {
			for j, e := range row {
				x.Values[i][j] = rewriteExpr(e, fn)
			}
		}
		if x.Select != nil {
			if rs, ok := Rewrite(x.Select, fn).(*SelectStmt); ok {
				x.Select = rs
			}
		}
		return fn(x)
	case *UpdateStmt:
		for i, a := range x.Sets {
			x.Sets[i].Expr = rewriteExpr(a.Expr, fn)
		}
		x.Where = rewriteExpr(x.Where, fn)
		return fn(x)
	case *DeleteStmt:
		x.Where = rewriteExpr(x.Where, fn)
		return fn(x)
	case *MergeStmt:
		if x.Source != nil {
			if rs, ok := Rewrite(x.Source, fn).(FromItem); ok {
				x.Source = rs
			}
		}
		x.On = rewriteExpr(x.On, fn)
		return fn(x)

	// -- PL/SQL statements (just the ones whose expression slots Phase
	// 3 visitors touch — Block.Stmts/Decls themselves get descended
	// into via the parent Block case below) -------------------------

	case *Block:
		for i, d := range x.Decls {
			if rd, ok := Rewrite(d, fn).(PLDecl); ok {
				x.Decls[i] = rd
			}
		}
		for i, s := range x.Stmts {
			if rs, ok := Rewrite(s, fn).(PLStmt); ok {
				x.Stmts[i] = rs
			}
		}
		if x.Except != nil {
			for i := range x.Except.Handlers {
				for j, s := range x.Except.Handlers[i].Body {
					if rs, ok := Rewrite(s, fn).(PLStmt); ok {
						x.Except.Handlers[i].Body[j] = rs
					}
				}
			}
		}
		return fn(x)
	case *DeclareVar:
		x.Default = rewriteExpr(x.Default, fn)
		return fn(x)
	case *AssignStmt:
		x.Expr = rewriteExpr(x.Expr, fn)
		return fn(x)
	case *IfStmt:
		for i, br := range x.Branches {
			x.Branches[i].Cond = rewriteExpr(br.Cond, fn)
			for j, s := range br.Body {
				if rs, ok := Rewrite(s, fn).(PLStmt); ok {
					x.Branches[i].Body[j] = rs
				}
			}
		}
		for i, s := range x.Else {
			if rs, ok := Rewrite(s, fn).(PLStmt); ok {
				x.Else[i] = rs
			}
		}
		return fn(x)
	case *CaseStmt:
		x.Expr = rewriteExpr(x.Expr, fn)
		for i, w := range x.When {
			x.When[i].Match = rewriteExpr(w.Match, fn)
			for j, s := range w.Body {
				if rs, ok := Rewrite(s, fn).(PLStmt); ok {
					x.When[i].Body[j] = rs
				}
			}
		}
		for i, s := range x.Else {
			if rs, ok := Rewrite(s, fn).(PLStmt); ok {
				x.Else[i] = rs
			}
		}
		return fn(x)
	case *WhileStmt:
		x.Cond = rewriteExpr(x.Cond, fn)
		for i, s := range x.Body {
			if rs, ok := Rewrite(s, fn).(PLStmt); ok {
				x.Body[i] = rs
			}
		}
		return fn(x)
	case *LoopStmt:
		for i, s := range x.Body {
			if rs, ok := Rewrite(s, fn).(PLStmt); ok {
				x.Body[i] = rs
			}
		}
		return fn(x)
	case *NumericForStmt:
		x.Low = rewriteExpr(x.Low, fn)
		x.High = rewriteExpr(x.High, fn)
		for i, s := range x.Body {
			if rs, ok := Rewrite(s, fn).(PLStmt); ok {
				x.Body[i] = rs
			}
		}
		return fn(x)
	case *CursorForStmt:
		for i, s := range x.Body {
			if rs, ok := Rewrite(s, fn).(PLStmt); ok {
				x.Body[i] = rs
			}
		}
		return fn(x)
	case *LeaveStmt:
		x.WhenCond = rewriteExpr(x.WhenCond, fn)
		return fn(x)
	case *IterateStmt:
		x.WhenCond = rewriteExpr(x.WhenCond, fn)
		return fn(x)
	case *ReturnStmt:
		x.Expr = rewriteExpr(x.Expr, fn)
		return fn(x)
	case *ExecuteImmediateStmt:
		x.SQL = rewriteExpr(x.SQL, fn)
		for i, e := range x.Using {
			x.Using[i] = rewriteExpr(e, fn)
		}
		return fn(x)
	case *SelectInto:
		if x.Stmt != nil {
			if rs, ok := Rewrite(x.Stmt, fn).(*SelectStmt); ok {
				x.Stmt = rs
			}
		}
		return fn(x)
	}

	// Unknown / leaf node — hand to fn unchanged.
	return fn(n)
}

// rewriteExpr is the type-narrowing helper for Expr-typed slots: it
// runs the rewriter and reasserts the result back to Expr, leaving the
// slot unchanged when the substitution returns a non-Expr (which the
// visitor library forbids but defensive code stays cheap).
func rewriteExpr(e Expr, fn Rewriter) Expr {
	if e == nil {
		return nil
	}
	r := Rewrite(e, fn)
	if r == nil {
		return nil
	}
	if re, ok := r.(Expr); ok {
		return re
	}
	return e
}

// Compose chains rewriters left-to-right: Compose(a, b, c)(n) is
// c(b(a(n))). Each rewriter sees the output of the previous one, which
// matches the natural pipeline shape the Phase 3 orchestrator wants:
// outer-join structural rewrite → name normalisation → match-and-
// replace passes → final-residual passes.
func Compose(rs ...Rewriter) Rewriter {
	return func(n Node) Node {
		for _, r := range rs {
			n = r(n)
		}
		return n
	}
}
