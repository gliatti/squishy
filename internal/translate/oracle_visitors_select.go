package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_visitors_select.go — Group C AST rewriters that operate at
// the SelectStmt / WindowedAgg level rather than on a single
// expression node. Composed by the Phase 3.6 orchestrator alongside
// the simpler Group A rewriters in oracle_visitors_funcs.go.

// VisitOracleRownumLimit detects the Oracle idiom
//
//	SELECT … FROM … WHERE ROWNUM <= N
//	SELECT … FROM … WHERE ROWNUM <  N
//	SELECT … FROM … WHERE ROWNUM =  1
//
// and rewrites the SelectStmt to use PG's LIMIT clause:
//
//	SELECT … FROM … LIMIT N
//
// Only single-clause WHEREs are matched (the predicate must be the
// entire Where, not part of a larger AND/OR tree). Multi-clause cases
// are left alone — the rewrite would have to lift the ROWNUM branch
// out of an AND and risk re-ordering side effects from other
// predicates. Phase 3.5 covers the dominant 80% case; the remaining
// shapes route to a follow-up commit.
//
// Replaces the legacy text pass `rewriteOracleRownumLimit`.
func VisitOracleRownumLimit(n ast.Node) ast.Node {
	sel, ok := n.(*ast.SelectStmt)
	if !ok {
		return n
	}
	if sel.Where == nil || sel.Limit != nil {
		return n
	}
	bin, ok := sel.Where.(*ast.BinaryExpr)
	if !ok {
		return n
	}
	// Identify the ROWNUM operand and the numeric bound regardless of
	// which side carries the pseudo-column.
	limit, op := matchRownumPredicate(bin)
	if limit == nil {
		return n
	}
	// `<` semantics: WHERE ROWNUM < N means strictly less than N rows.
	// PG LIMIT is inclusive, so the equivalent is N-1. We don't try to
	// constant-fold here — emit a `LIMIT N - 1` arithmetic expression
	// and let PG evaluate.
	if op == "<" {
		limit = ast.BuildBinary("-", limit, ast.BuildIntLit(1))
	}
	// Build a fresh SelectStmt mirroring the original but with the
	// ROWNUM predicate stripped and Limit populated. Don't mutate the
	// caller's node in place — the same node may be reachable from
	// multiple parents during a Compose chain.
	out := *sel
	out.Where = nil
	out.Limit = limit
	return &out
}

// matchRownumPredicate returns (limit, op) when bin is a
// `ROWNUM <op> N` or `N <op> ROWNUM` expression where:
//   - op is "=", "<", or "<="
//   - N is a Literal (kept as Expr to round-trip through the writer)
//
// Returns (nil, "") when the predicate doesn't match.
func matchRownumPredicate(bin *ast.BinaryExpr) (ast.Expr, string) {
	op := bin.Op
	if op != "=" && op != "<" && op != "<=" {
		return nil, ""
	}
	switch {
	case isRownumIdent(bin.Lhs):
		if _, ok := ast.IsLiteralKind(bin.Rhs, "number"); !ok {
			return nil, ""
		}
		return bin.Rhs, op
	case isRownumIdent(bin.Rhs):
		if _, ok := ast.IsLiteralKind(bin.Lhs, "number"); !ok {
			return nil, ""
		}
		// Flip operator for `N op ROWNUM` (PG LIMIT is always on the
		// right). For `1 = ROWNUM` the op stays "=".
		switch op {
		case "<":
			op = ">" // not handled — rare and ambiguous; bail out
			return nil, ""
		case "<=":
			op = ">="
			return nil, ""
		}
		return bin.Lhs, op
	}
	return nil, ""
}

// isRownumIdent returns true when e is the bare `ROWNUM` pseudo-column
// reference (which the Oracle parser produces as an Ident with a
// single uppercase part — Oracle reserves ROWNUM and the lexer keeps
// the canonical casing).
func isRownumIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok || len(id.Parts) != 1 {
		return false
	}
	return strings.EqualFold(id.Parts[0], "ROWNUM")
}

// VisitOracleListagg rewrites the LISTAGG aggregate to PG's
// string_agg with an ORDER BY clause derived from the WITHIN GROUP
// modifier:
//
//	LISTAGG(expr, sep) WITHIN GROUP (ORDER BY a, b DESC)
//	→ string_agg(expr::text, sep ORDER BY a, b DESC)
//
// The Oracle parser emits *WindowedAgg{Func: FuncCall{Name:"listagg",
// Args:[expr, sep]}, Within: [order_items]}. PG 16+ supports WITHIN
// GROUP on string_agg natively, but the canonical translation moves
// the ordering into string_agg's argument list — that form predates
// PG 16 and is what the legacy text pass produced.
//
// The expression operand is wrapped in an explicit ::text cast to
// match LISTAGG's implicit string conversion (Oracle accepts non-
// VARCHAR values; PG's string_agg requires text/varchar).
//
// Replaces the legacy text pass `rewriteListagg`.
func VisitOracleListagg(n ast.Node) ast.Node {
	wa, ok := n.(*ast.WindowedAgg)
	if !ok || wa.Func == nil {
		return n
	}
	if !strings.EqualFold(wa.Func.Name, "listagg") {
		return n
	}
	if len(wa.Func.Args) < 1 || len(wa.Func.Args) > 2 {
		return n
	}
	exprArg := wa.Func.Args[0]
	var sepArg ast.Expr = ast.BuildStringLit("")
	if len(wa.Func.Args) == 2 {
		sepArg = wa.Func.Args[1]
	}
	// expr::text — preserve LISTAGG's implicit conversion semantics.
	castExpr := &ast.CastExpr{
		Expr: exprArg,
		Type: &ast.UserDefinedType{Name: "text"},
	}
	newCall := &ast.FuncCall{
		Name: "string_agg",
		Args: []ast.Expr{castExpr, sepArg},
		P:    wa.Func.P,
	}
	// Move the WITHIN GROUP order into the inner FuncCall's argument
	// list as a synthetic ORDER BY. There's no first-class AST shape
	// for "agg(args ORDER BY items)"; surface it via a RawExpr appended
	// to Args so WriteExpr emits the literal `ORDER BY …` text. This
	// is the smallest change that produces correct PG; a future
	// enhancement adds an AggOrderBy field to FuncCall and a writer
	// branch.
	if len(wa.Within) > 0 {
		newCall.Args = append(newCall.Args, &ast.RawExpr{
			Text: "ORDER BY " + renderOrderItemsRaw(wa.Within),
		})
	}
	// Drop WITHIN — the ordering moved into the FuncCall.
	return &ast.WindowedAgg{
		Func:   newCall,
		Within: nil,
		Over:   wa.Over,
		P:      wa.P,
	}
}

// renderOrderItemsRaw flattens []OrderItem to the comma-separated PG
// `expr [DESC] [NULLS LAST]` text form.  Used by the listagg visitor
// to fold WITHIN GROUP ordering into the inner string_agg call.
func renderOrderItemsRaw(items []ast.OrderItem) string {
	parts := make([]string, len(items))
	for i, oi := range items {
		parts[i] = renderOrderItem(oi)
	}
	return strings.Join(parts, ", ")
}

// VisitOracleNestedAggregates rewrites SELECT statements that nest one
// aggregate inside another:
//
//	SELECT MIN(COUNT(*)) FROM t GROUP BY x
//	→ SELECT MIN(_nag) FROM (SELECT COUNT(*) AS _nag FROM t GROUP BY x) _nag_t
//
// PostgreSQL rejects nested aggregates ("aggregate function calls
// cannot be nested"); the canonical lift wraps the inner aggregate
// in a derived table so the outer aggregate operates over the
// resulting one-row-per-group set.
//
// Match shape:
//   - SelectStmt with exactly one column (Cols[0]) that is a FuncCall
//     of one of MIN/MAX/AVG/SUM/COUNT.
//   - That FuncCall has exactly one argument which is itself a
//     FuncCall of one of the same aggregate set.
//   - The SelectStmt has at least one FROM item (otherwise the
//     rewrite would produce SELECT … FROM (…) without a base table,
//     which is fine but the original wouldn't have parsed as a real
//     query).
//
// Multi-column projections, calls with extra arguments, set ops,
// and unsupported aggregate names fall through unchanged — the
// legacy text pass owned only the canonical single-column shape and
// the visitor mirrors that contract.
//
// Replaces the legacy text pass `rewriteOracleNestedAggregates`.
func VisitOracleNestedAggregates(n ast.Node) ast.Node {
	sel, ok := n.(*ast.SelectStmt)
	if !ok {
		return n
	}
	if len(sel.Cols) != 1 {
		return n
	}
	col := sel.Cols[0]
	if col.Star {
		return n
	}
	outer, ok := col.Expr.(*ast.FuncCall)
	if !ok || !isNestableAgg(outer.Name) {
		return n
	}
	if len(outer.Args) != 1 {
		return n
	}
	inner, ok := outer.Args[0].(*ast.FuncCall)
	if !ok || !isNestableAgg(inner.Name) {
		return n
	}
	if len(sel.From) == 0 {
		return n
	}
	// Build the inner SELECT — same FROM/WHERE/GROUP BY/HAVING but
	// projecting the inner aggregate aliased as _nag. The original
	// SelectStmt's clauses are reused as-is; the outer wrapper's only
	// job is to lift the inner aggregate into a derived row source.
	innerStmt := &ast.SelectStmt{
		Cols: []ast.SelectItem{
			{Expr: inner, Alias: "_nag"},
		},
		From:    sel.From,
		Where:   sel.Where,
		GroupBy: sel.GroupBy,
		Having:  sel.Having,
	}
	// Build the outer SELECT — single column applying outer agg to the
	// derived row source's _nag column.
	outerStmt := &ast.SelectStmt{
		Cols: []ast.SelectItem{
			{
				Expr: &ast.FuncCall{
					Name: outer.Name,
					Args: []ast.Expr{&ast.Ident{Parts: []string{"_nag"}}},
					P:    outer.P,
				},
			},
		},
		From: []ast.FromItem{
			&ast.FromSubquery{
				Stmt:  innerStmt,
				Alias: "_nag_t",
			},
		},
		// Preserve outer-only clauses on the wrapper so a caller that
		// added e.g. ORDER BY at the top level doesn't lose it.
		OrderBy: sel.OrderBy,
		Limit:   sel.Limit,
		Offset:  sel.Offset,
	}
	return outerStmt
}

// isNestableAgg returns true when name is one of the SQL aggregate
// function names the visitor matches on. Match is case-insensitive.
func isNestableAgg(name string) bool {
	switch strings.ToUpper(name) {
	case "MIN", "MAX", "SUM", "AVG", "COUNT":
		return true
	}
	return false
}
