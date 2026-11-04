package translate

import (
	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_register.go — plugs the T-SQL expression / query / body
// rewriters (tsql_visitors.go) and the shim pre-actions (tsql_shim.go)
// into the translator core through the dialect_hooks.go registries. The
// routine-body parser and translator are registered by
// tsql_routines_register.go.
func init() {
	RegisterExprRewriter(dialects.KindSQLServer, tsqlExprHook)
	RegisterSelectRewriter(dialects.KindSQLServer, tsqlSelectHook)
	RegisterBodyRewriter(dialects.KindSQLServer, tsqlBodyHook)
	RegisterPreActionsProvider(dialects.KindSQLServer, tsqlShimPreActions)
}

// Warning kinds of the T-SQL rewriters.
const (
	// tsqlWarnUntranslatedExpr: a DDL expression (DEFAULT, CHECK,
	// computed column, index filter) kept a T-SQL construct.
	tsqlWarnUntranslatedExpr = "tsql.untranslated_expression"
	// tsqlWarnNote: an informational remark on a translated construct
	// (semantic difference the user should know about).
	tsqlWarnNote = "tsql.note"
)

// tsqlExprHook is the SQL Server ExprRewriter.
func tsqlExprHook(e ast.Expr, ctx *ExprCtx) ast.Expr {
	out, res := rewriteTSQLExpr(e, ctx)
	reportTSQLRewrite(ctx, res, tsqlWarnUntranslatedExpr)
	return out
}

// tsqlSelectHook is the SQL Server SelectRewriter. The labels it returns
// become the query's blocking warning; they include FOR XML / JSON and
// TOP … PERCENT at any depth of the query (the residual walk).
func tsqlSelectHook(s *ast.SelectStmt, ctx *ExprCtx) (*ast.SelectStmt, []string) {
	if s == nil {
		return nil, nil
	}
	rw := newTSQLRW(ctx)
	rw.prepare(s)
	out, ok := ast.Rewrite(s, rw.chain()).(*ast.SelectStmt)
	if !ok || out == nil {
		out = s
	}
	res := rw.finish(out)
	reportTSQLRewrite(ctx, tsqlRewriteResult{Notes: res.Notes}, "")
	return out, res.Labels
}

// tsqlBodyHook is the SQL Server BodyRewriter (generic routine-body
// path).
func tsqlBodyHook(stmts []ast.PLStmt, ctx *ExprCtx) []ast.PLStmt {
	out, res := rewriteTSQLBody(stmts, ctx)
	reportTSQLRewrite(ctx, res, "routine.untranslated_construct")
	return out
}

// reportTSQLRewrite turns a rewrite's labels into blocking warnings of
// kind labelKind ("" to drop them: the caller reports them) and its notes
// into info warnings, each note once per object and run.
func reportTSQLRewrite(ctx *ExprCtx, res tsqlRewriteResult, labelKind string) {
	if ctx == nil || ctx.Run == nil {
		return
	}
	if labelKind != "" {
		for _, l := range res.Labels {
			ctx.Run.Warn(ctx.Object, labelKind, "T-SQL construct not translated: "+l+"; manual review required", SeverityBlocking)
		}
	}
	for _, n := range res.Notes {
		key := "note:" + ctx.Object + "\x00" + n
		if !ctx.Run.firstTime(key) {
			continue
		}
		ctx.Run.Warn(ctx.Object, tsqlWarnNote, n, SeverityInfo)
	}
}

// tsqlWarnFoldCollision: two source objects (tables, columns of one
// table) whose names fold to the same PG name — they differ by case in a
// case-sensitive database, or only past the 63 bytes PG keeps.
const tsqlWarnFoldCollision = "name.fold_collision"
