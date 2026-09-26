package ast

// Visitor implements a pre-order traversal over the AST. The semantics follow
// the classic Go ast package: if Visit returns nil, sub-nodes are skipped;
// otherwise the returned visitor is used to walk sub-nodes.
type Visitor interface {
	Visit(n Node) Visitor
}

// Walk traverses the tree rooted at n in depth-first order, calling v.Visit on
// every node. A nil return from Visit prunes the subtree.
func Walk(v Visitor, n Node) {
	if n == nil {
		return
	}
	v = v.Visit(n)
	if v == nil {
		return
	}
	switch x := n.(type) {
	case *CreateTable:
		for _, c := range x.Columns {
			Walk(v, c)
		}
		for _, c := range x.Constraints {
			Walk(v, c)
		}
		for _, i := range x.Indexes {
			Walk(v, i)
		}
	case *ColumnDef:
		if x.Type != nil {
			Walk(v, x.Type)
		}
		if x.Default != nil {
			Walk(v, x.Default)
		}
		if x.OnUpdate != nil {
			Walk(v, x.OnUpdate)
		}
		if x.Generated != nil && x.Generated.Expr != nil {
			Walk(v, x.Generated.Expr)
		}
		if x.Check != nil {
			Walk(v, x.Check)
		}
	case *CheckConstraint:
		if x.Expr != nil {
			Walk(v, x.Expr)
		}
	case *BinaryExpr:
		Walk(v, x.Lhs)
		Walk(v, x.Rhs)
	case *UnaryExpr:
		Walk(v, x.Rhs)
	case *ParenExpr:
		Walk(v, x.Inner)
	case *FuncCall:
		for _, a := range x.Args {
			Walk(v, a)
		}
		for _, oi := range x.AggOrderBy {
			Walk(v, oi.Expr)
		}
		Walk(v, x.AggSeparator)
	case *IntervalLit:
		Walk(v, x.Expr)
	case *CaseExpr:
		Walk(v, x.Operand)
		for _, w := range x.Whens {
			Walk(v, w.Match)
			Walk(v, w.Then)
		}
		Walk(v, x.Else)
	case *BetweenExpr:
		Walk(v, x.Expr)
		Walk(v, x.Low)
		Walk(v, x.High)
	case *InExpr:
		Walk(v, x.Expr)
		for _, e := range x.List {
			Walk(v, e)
		}
		if x.Subquery != nil {
			Walk(v, x.Subquery)
		}
	case *ExistsExpr:
		if x.Subquery != nil {
			Walk(v, x.Subquery)
		}
	case *SubqueryExpr:
		if x.Stmt != nil {
			Walk(v, x.Stmt)
		}
	case *WindowedAgg:
		if x.Func != nil {
			Walk(v, x.Func)
		}
		for _, oi := range x.Within {
			Walk(v, oi.Expr)
		}
		if x.Over != nil {
			for _, e := range x.Over.PartitionBy {
				Walk(v, e)
			}
			for _, oi := range x.Over.OrderBy {
				Walk(v, oi.Expr)
			}
		}
	case *OuterJoinHint:
		Walk(v, x.Inner)
	case *CastExpr:
		Walk(v, x.Expr)
		if x.Type != nil {
			Walk(v, x.Type)
		}
	// --- DML statements ---
	case *SelectStmt:
		if x.With != nil {
			for _, c := range x.With.CTEs {
				walkSelect(v, c.Body)
			}
		}
		for _, c := range x.Cols {
			Walk(v, c.Expr)
		}
		for _, f := range x.From {
			Walk(v, f)
		}
		Walk(v, x.Where)
		for _, e := range x.GroupBy {
			Walk(v, e)
		}
		Walk(v, x.Having)
		for _, oi := range x.OrderBy {
			Walk(v, oi.Expr)
		}
		Walk(v, x.Limit)
		Walk(v, x.Offset)
		for _, so := range x.SetOps {
			walkSelect(v, so.Stmt)
		}
	case *FromSubquery:
		if x.Stmt != nil {
			Walk(v, x.Stmt)
		}
	case *FromJoin:
		Walk(v, x.Left)
		Walk(v, x.Right)
		Walk(v, x.On)
	case *InsertStmt:
		for _, row := range x.Values {
			for _, e := range row {
				Walk(v, e)
			}
		}
		if x.Select != nil {
			Walk(v, x.Select)
		}
		if x.OnConflict != nil {
			for _, a := range x.OnConflict.Sets {
				Walk(v, a.Expr)
			}
			Walk(v, x.OnConflict.Where)
		}
		for _, ri := range x.Returning {
			Walk(v, ri.Expr)
		}
	case *UpdateStmt:
		for _, a := range x.Sets {
			Walk(v, a.Expr)
		}
		for _, f := range x.From {
			Walk(v, f)
		}
		Walk(v, x.Where)
		for _, ri := range x.Returning {
			Walk(v, ri.Expr)
		}
	case *DeleteStmt:
		for _, f := range x.Using {
			Walk(v, f)
		}
		Walk(v, x.Where)
		for _, ri := range x.Returning {
			Walk(v, ri.Expr)
		}
	case *MergeStmt:
		if x.Source != nil {
			Walk(v, x.Source)
		}
		Walk(v, x.On)
		for _, ma := range x.WhenMatched {
			walkMergeAction(v, ma)
		}
		for _, ma := range x.WhenNotMatched {
			walkMergeAction(v, ma)
		}

	// --- DDL nodes carrying typed bodies ---
	case *CreateView:
		walkSelect(v, x.Select)
	case *CreateEvent:
		Walk(v, x.AtExpr)

	// --- PL (procedural) declarations and statements ---
	case *Block:
		for _, d := range x.Decls {
			Walk(v, d)
		}
		walkPLStmts(v, x.Stmts)
		if x.Except != nil {
			for _, h := range x.Except.Handlers {
				walkPLStmts(v, h.Body)
			}
		}
	case *DeclareVar:
		Walk(v, x.Default)
	case *DeclareCursor:
		walkSelect(v, x.Stmt)
	case *DeclareHandler:
		Walk(v, x.Action)
	case *AssignStmt:
		Walk(v, x.Expr)
	case *IfStmt:
		for _, br := range x.Branches {
			Walk(v, br.Cond)
			walkPLStmts(v, br.Body)
		}
		walkPLStmts(v, x.Else)
	case *CaseStmt:
		Walk(v, x.Expr)
		for _, w := range x.When {
			Walk(v, w.Match)
			walkPLStmts(v, w.Body)
		}
		walkPLStmts(v, x.Else)
	case *WhileStmt:
		Walk(v, x.Cond)
		walkPLStmts(v, x.Body)
	case *LoopStmt:
		walkPLStmts(v, x.Body)
	case *RepeatStmt:
		walkPLStmts(v, x.Body)
		Walk(v, x.Cond)
	case *LeaveStmt:
		Walk(v, x.WhenCond)
	case *IterateStmt:
		Walk(v, x.WhenCond)
	case *ReturnStmt:
		Walk(v, x.Expr)
	case *CallStmt:
		for _, a := range x.Args {
			Walk(v, a)
		}
	case *SelectInto:
		walkSelect(v, x.Stmt)
	}
}

// walkSelect walks a *SelectStmt slot, skipping a typed-nil pointer
// (which would otherwise reach Walk as a non-nil Node interface).
func walkSelect(v Visitor, s *SelectStmt) {
	if s != nil {
		Walk(v, s)
	}
}

// walkPLStmts walks a procedural statement list in order.
func walkPLStmts(v Visitor, stmts []PLStmt) {
	for _, s := range stmts {
		Walk(v, s)
	}
}

func walkMergeAction(v Visitor, ma MergeAction) {
	Walk(v, ma.Cond)
	for _, a := range ma.Sets {
		Walk(v, a.Expr)
	}
	for _, e := range ma.InsertValues {
		Walk(v, e)
	}
}
