package translate

// tsql_rowcount.go — T-SQL SET ROWCOUNT in routine bodies.
//
// `SET ROWCOUNT n` makes the statements that follow stop after n rows
// until `SET ROWCOUNT 0` (and SQL Server restores the setting when the
// procedure returns). For a SELECT this is TOP n — the smaller of the two
// when the SELECT has its own TOP — which the visitors then turn into
// LIMIT. The pass runs on the source AST before the rewrites and only
// when the whole body is covered: every SET ROWCOUNT is in the body's
// main sequence (not inside IF / WHILE / TRY, where the limit would
// depend on the path taken) with a literal count, and nothing the limit
// applies to lacks a PostgreSQL row limit (UPDATE, DELETE, MERGE, an
// INSERT … SELECT, a procedure call or dynamic SQL, which inherit the
// setting). Otherwise nothing is changed and every SET ROWCOUNT is
// reported.

import (
	"strconv"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsqlRowcountPass is the state of one SET ROWCOUNT analysis.
type tsqlRowcountPass struct {
	limit int64 // 0: no limit
	fail  string
	sets  []*ast.SetRowcountStmt
	// tops are the SELECTs to limit, with their new TOP count.
	tops []tsqlRowcountTop
}

type tsqlRowcountTop struct {
	sel   *ast.SelectStmt
	count int64
}

// applySetRowcount applies the SET ROWCOUNT statements of a body (see
// the file comment): on success the limited SELECTs get their TOP and
// b.rowcountDone lists the translated SET ROWCOUNT statements (dropped
// by the renderer); otherwise b.rowcountWhy says why they are reported.
func (b *tsqlBody) applySetRowcount(stmts []ast.PLStmt) {
	rc := &tsqlRowcountPass{}
	rc.seq(stmts, true)
	if len(rc.sets) == 0 {
		return
	}
	if rc.fail != "" {
		b.rowcountWhy = rc.fail
		return
	}
	for _, t := range rc.tops {
		t.sel.Top = &ast.Literal{Kind: "number", Text: strconv.FormatInt(t.count, 10), P: t.sel.P}
	}
	b.rowcountDone = map[*ast.SetRowcountStmt]bool{}
	for _, s := range rc.sets {
		b.rowcountDone[s] = true
	}
	b.note("SET ROWCOUNT n is applied as TOP n (LIMIT n) to the SELECT statements that follow it")
}

func (rc *tsqlRowcountPass) failf(why string) {
	if rc.fail == "" {
		rc.fail = why
	}
}

// seq walks a statement sequence; top is true for the body's main
// sequence (and the BEGIN … END groups it runs straight through).
func (rc *tsqlRowcountPass) seq(stmts []ast.PLStmt, top bool) {
	for _, s := range stmts {
		rc.stmt(s, top)
	}
}

func (rc *tsqlRowcountPass) stmt(s ast.PLStmt, top bool) {
	switch x := s.(type) {
	case *ast.SetRowcountStmt:
		rc.sets = append(rc.sets, x)
		if !top {
			rc.failf("SET ROWCOUNT inside IF / WHILE / TRY: the limit of the statements that follow depends on the path taken")
			return
		}
		lit, ok := tsqlUnparen(x.Count).(*ast.Literal)
		if !ok || lit.Kind != "number" || !tsqlIsIntegerToken(lit.Text) {
			rc.failf("SET ROWCOUNT with a count that is not an integer literal")
			return
		}
		n, err := strconv.ParseInt(lit.Text, 10, 64)
		if err != nil {
			rc.failf("SET ROWCOUNT count out of range")
			return
		}
		rc.limit = n
	case *ast.Block:
		rc.seq(x.Stmts, top && !x.TryCatch)
		if x.Except != nil {
			for _, h := range x.Except.Handlers {
				rc.seq(h.Body, false)
			}
		}
	case *ast.IfStmt:
		for _, br := range x.Branches {
			rc.seq(br.Body, false)
		}
		rc.seq(x.Else, false)
	case *ast.WhileStmt:
		rc.seq(x.Body, false)
	case *ast.SelectStmt:
		rc.limitSelect(x)
	case *ast.SelectInto:
		if x.Stmt != nil {
			rc.limitSelect(x.Stmt)
		}
	case *ast.InsertStmt:
		if rc.limit > 0 && (x.Select != nil || x.Exec != nil || int64(len(x.Values)) > rc.limit) {
			rc.failf("INSERT under SET ROWCOUNT (PostgreSQL INSERT has no row limit)")
		}
	case *ast.UpdateStmt, *ast.DeleteStmt, *ast.MergeStmt:
		if rc.limit > 0 {
			rc.failf("UPDATE / DELETE / MERGE under SET ROWCOUNT (PostgreSQL has no row limit for them)")
		}
	case *ast.CallStmt, *ast.ExecuteImmediateStmt:
		if rc.limit > 0 {
			rc.failf("procedure call or dynamic SQL under SET ROWCOUNT (the callee inherits the limit)")
		}
	}
}

// limitSelect records the TOP a SELECT gets under the current limit.
func (rc *tsqlRowcountPass) limitSelect(sel *ast.SelectStmt) {
	if rc.limit == 0 {
		return
	}
	count := rc.limit
	if sel.Top != nil {
		lit, ok := tsqlUnparen(sel.Top).(*ast.Literal)
		if !ok || sel.TopPercent || sel.TopWithTies || lit.Kind != "number" || !tsqlIsIntegerToken(lit.Text) {
			rc.failf("SELECT TOP with a non-literal count, PERCENT or WITH TIES under SET ROWCOUNT")
			return
		}
		n, err := strconv.ParseInt(lit.Text, 10, 64)
		if err != nil {
			rc.failf("SELECT TOP count out of range")
			return
		}
		if n < count {
			count = n
		}
	}
	if sel.Limit != nil || len(sel.SetOps) > 0 {
		rc.failf("SELECT with OFFSET / FETCH or UNION under SET ROWCOUNT")
		return
	}
	rc.tops = append(rc.tops, tsqlRowcountTop{sel: sel, count: count})
}
