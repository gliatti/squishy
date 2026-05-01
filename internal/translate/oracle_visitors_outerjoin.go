package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_visitors_outerjoin.go — Phase 3.7b structural visitor that
// rewrites Oracle's `(+)` outer-join hint into ANSI LEFT JOIN clauses.
//
// The Oracle parser produces *ast.OuterJoinHint{Inner Expr} on
// column refs marked with `(+)` in expression context (parser_expr.go
// L425). This visitor walks every *SelectStmt and:
//
//  1. Scans the WHERE clause for predicates that reference an
//     OuterJoinHint.
//  2. Splits the WHERE on top-level AND boundaries.
//  3. Classifies each predicate:
//     - Join predicate: ≥2 distinct alias references, at least one
//       carrying the (+) hint. The hinted side becomes the *outer*
//       (left/right) side of the JOIN; in PG canonical form the
//       hinted side joins as the RIGHT side of a LEFT JOIN — i.e.,
//       a `LEFT JOIN <hinted>` on the predicate.
//     - Outer-side filter: predicate against a single alias that
//       carries the hint and a literal/bind on the other side. PG
//       carries this into the JOIN's ON clause (otherwise the
//       filter would defeat the LEFT semantics by dropping NULL
//       extensions).
//     - Other predicate: stays in the rewritten WHERE.
//  4. Rebuilds From as a chain of LEFT JOIN + the leftover non-hinted
//     tables comma-joined.
//  5. Strips every remaining OuterJoinHint via a final Rewrite pass.
//
// Cases that surface a blocking-style failure (left as-is so the
// downstream emitter shows the broken query at apply time):
//   - `(+)` on both sides of a predicate (Oracle disallows; we don't
//     attempt to interpret).
//   - `(+)` in OR / nested boolean group (semantics not reducible to
//     a single LEFT JOIN).
//   - `(+)` on a function-call result (Oracle 12c+ allowance; the
//     legacy text path didn't translate it either).
//
// The visitor only fires on SelectStmts that contain at least one
// OuterJoinHint somewhere in WHERE — clean SelectStmts return
// unchanged so the cost is a single bool walk.

// VisitOracleOuterJoin rewrites the Oracle (+) outer-join hint into
// ANSI LEFT JOIN syntax. See the file-level comment for the algorithm
// and the unsupported-shape contract.
func VisitOracleOuterJoin(n ast.Node) ast.Node {
	sel, ok := n.(*ast.SelectStmt)
	if !ok {
		return n
	}
	if sel.Where == nil {
		return n
	}
	if !whereHasOuterJoinHint(sel.Where) {
		return n
	}

	// Top-level AND split. OR groups containing a (+) bail out early.
	preds := flattenAnd(sel.Where)
	if anyOrContainsHint(sel.Where) {
		return n
	}

	aliasIdx := indexFromAliases(sel.From)
	if len(aliasIdx) == 0 {
		return n
	}

	// Classification buckets.
	type joinPair struct {
		hinted   string // alias of the (+)-marked side
		other    string // alias of the un-hinted side
		on       ast.Expr
		isFilter bool
	}
	var joinPreds []joinPair
	var residuals []ast.Expr

	for _, p := range preds {
		hintCount := countOuterJoinHints(p)
		if hintCount == 0 {
			residuals = append(residuals, p)
			continue
		}
		if hintCount > 1 {
			// (+) on both sides of a single predicate — Oracle
			// disallows; bail out without rewriting.
			return n
		}
		hinted, other, ok := splitJoinAliases(p, aliasIdx)
		if !ok {
			// Pattern we don't model — keep the SelectStmt intact.
			return n
		}
		joinPreds = append(joinPreds, joinPair{
			hinted:   hinted,
			other:    other,
			on:       stripOuterJoinHints(p),
			isFilter: other == "",
		})
	}
	if len(joinPreds) == 0 {
		return n
	}

	// Build JOIN tree. Group predicates by hinted-alias; collapse
	// multiple predicates against the same hinted alias into one
	// LEFT JOIN with AND'd ON clauses.
	byHinted := map[string][]ast.Expr{}
	hintedOrder := []string{}
	for _, jp := range joinPreds {
		if _, seen := byHinted[jp.hinted]; !seen {
			hintedOrder = append(hintedOrder, jp.hinted)
		}
		byHinted[jp.hinted] = append(byHinted[jp.hinted], jp.on)
	}

	// Pick a base table — the first FROM item whose alias is NOT a
	// hinted alias. Falls back to the first FROM item if every alias
	// is hinted (degenerate but valid Oracle).
	hintedSet := map[string]bool{}
	for k := range byHinted {
		hintedSet[k] = true
	}
	var base ast.FromItem
	var baseIdx int
	for i, fi := range sel.From {
		alias := fromItemAlias(fi)
		if !hintedSet[alias] {
			base = fi
			baseIdx = i
			break
		}
	}
	if base == nil {
		base = sel.From[0]
		baseIdx = 0
	}

	// Build the LEFT JOIN chain on top of `base` for each hinted
	// alias in source order.
	tree := base
	consumed := map[int]bool{baseIdx: true}
	for _, h := range hintedOrder {
		ix, ok := aliasIdx[h]
		if !ok {
			return n
		}
		if consumed[ix] {
			// Same alias appearing in multiple FROM positions — bail.
			return n
		}
		on := andAll(byHinted[h])
		tree = &ast.FromJoin{
			Kind:  ast.LeftJoin,
			Left:  tree,
			Right: sel.From[ix],
			On:    on,
		}
		consumed[ix] = true
	}

	// Any remaining FROM items become CROSS JOIN partners (Oracle
	// comma-list semantics — no predicate ties them in).
	newFrom := []ast.FromItem{tree}
	for i, fi := range sel.From {
		if !consumed[i] {
			newFrom = append(newFrom, fi)
		}
	}

	// Final WHERE: AND of the residual non-hint predicates (nil if
	// none).
	var newWhere ast.Expr
	if len(residuals) > 0 {
		// Strip any leftover hints (defensive — shouldn't be there in
		// residuals, but the post-condition is "no OuterJoinHint
		// reaches the writer").
		stripped := make([]ast.Expr, len(residuals))
		for i, r := range residuals {
			stripped[i] = stripOuterJoinHints(r)
		}
		newWhere = andAll(stripped)
	}

	out := *sel
	out.From = newFrom
	out.Where = newWhere
	return &out
}

// whereHasOuterJoinHint walks e and returns true on the first
// OuterJoinHint encountered.
func whereHasOuterJoinHint(e ast.Expr) bool {
	found := false
	ast.Rewrite(e, func(n ast.Node) ast.Node {
		if _, ok := n.(*ast.OuterJoinHint); ok {
			found = true
		}
		return n
	})
	return found
}

// flattenAnd splits e on top-level `AND` boundaries. ParenExpr
// wrappers around AND-shaped subtrees are unwrapped.
func flattenAnd(e ast.Expr) []ast.Expr {
	var out []ast.Expr
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case *ast.BinaryExpr:
			if strings.EqualFold(x.Op, "AND") {
				walk(x.Lhs)
				walk(x.Rhs)
				return
			}
		case *ast.ParenExpr:
			walk(x.Inner)
			return
		}
		out = append(out, e)
	}
	walk(e)
	return out
}

// andAll re-folds a slice of predicates into a left-leaning AND
// chain. Empty input returns nil; single-element input returns the
// element as-is so the caller can avoid a useless paren wrapper.
func andAll(preds []ast.Expr) ast.Expr {
	if len(preds) == 0 {
		return nil
	}
	out := preds[0]
	for _, p := range preds[1:] {
		out = &ast.BinaryExpr{Op: "AND", Lhs: out, Rhs: p}
	}
	return out
}

// anyOrContainsHint detects if a top-level OR group contains a (+)
// — bail-out signal: PG can't reduce `cond1 OR (a.x = b.x(+))` to a
// LEFT JOIN since the OR semantics differ.
func anyOrContainsHint(e ast.Expr) bool {
	if e == nil {
		return false
	}
	if bin, ok := e.(*ast.BinaryExpr); ok && strings.EqualFold(bin.Op, "OR") {
		if whereHasOuterJoinHint(bin) {
			return true
		}
	}
	if bin, ok := e.(*ast.BinaryExpr); ok {
		return anyOrContainsHint(bin.Lhs) || anyOrContainsHint(bin.Rhs)
	}
	if par, ok := e.(*ast.ParenExpr); ok {
		return anyOrContainsHint(par.Inner)
	}
	return false
}

// countOuterJoinHints counts how many *OuterJoinHint nodes appear
// directly inside e (not recursing into sub-SelectStmt — those are
// independent contexts).
func countOuterJoinHints(e ast.Expr) int {
	count := 0
	ast.Rewrite(e, func(n ast.Node) ast.Node {
		if _, ok := n.(*ast.OuterJoinHint); ok {
			count++
		}
		return n
	})
	return count
}

// stripOuterJoinHints walks e and replaces every *OuterJoinHint with
// its Inner expression. Idempotent.
func stripOuterJoinHints(e ast.Expr) ast.Expr {
	r := ast.Rewrite(e, func(n ast.Node) ast.Node {
		if h, ok := n.(*ast.OuterJoinHint); ok {
			return h.Inner
		}
		return n
	})
	if rep, ok := r.(ast.Expr); ok {
		return rep
	}
	return e
}

// indexFromAliases returns alias → index map for sel.From. Aliases
// fall back to the table name when no AS clause is present.
func indexFromAliases(items []ast.FromItem) map[string]int {
	out := map[string]int{}
	for i, fi := range items {
		a := fromItemAlias(fi)
		if a != "" {
			out[strings.ToLower(a)] = i
		}
	}
	return out
}

// fromItemAlias returns the lowercase alias for fi (alias-when-set,
// table-name-otherwise). FromJoin / FromSubquery without alias
// return "".
func fromItemAlias(fi ast.FromItem) string {
	switch x := fi.(type) {
	case *ast.FromTable:
		if x.Alias != "" {
			return strings.ToLower(x.Alias)
		}
		return strings.ToLower(x.Name)
	case *ast.FromSubquery:
		return strings.ToLower(x.Alias)
	}
	return ""
}

// splitJoinAliases inspects predicate p and figures out which alias
// carries the (+) hint and which alias is the join partner. Returns
// (hinted, other, true) on a clean two-alias predicate; (hinted, "",
// true) when only one alias is present (an outer-side filter); and
// ("", "", false) for shapes the visitor doesn't model.
//
// The predicate is expected to be a top-level BinaryExpr — typical
// `a.x = b.y(+)` shape. More elaborate forms (function calls on
// either side) are bailed out by returning false.
func splitJoinAliases(p ast.Expr, aliasIdx map[string]int) (hinted, other string, ok bool) {
	bin, isBin := p.(*ast.BinaryExpr)
	if !isBin {
		return "", "", false
	}
	lhsAlias, lhsHinted := identAlias(bin.Lhs, aliasIdx)
	rhsAlias, rhsHinted := identAlias(bin.Rhs, aliasIdx)

	switch {
	case lhsHinted && rhsHinted:
		// Caller already detected double-hint via countOuterJoinHints
		// and returned early — defensive: return false here too.
		return "", "", false
	case lhsHinted && rhsAlias != "" && rhsAlias != lhsAlias:
		return lhsAlias, rhsAlias, true
	case rhsHinted && lhsAlias != "" && lhsAlias != rhsAlias:
		return rhsAlias, lhsAlias, true
	case lhsHinted && rhsAlias == "":
		// `a.x(+) = <literal>`  → outer-side filter on alias `a`.
		return lhsAlias, "", true
	case rhsHinted && lhsAlias == "":
		return rhsAlias, "", true
	}
	return "", "", false
}

// identAlias returns the lowercase alias of the FROM item the
// expression references, plus whether the reference is wrapped in
// an OuterJoinHint. Returns ("", false) when e isn't a recognised
// alias-bearing reference (function call, literal, etc.).
func identAlias(e ast.Expr, aliasIdx map[string]int) (alias string, hinted bool) {
	if e == nil {
		return "", false
	}
	if h, ok := e.(*ast.OuterJoinHint); ok {
		alias, _ = identAlias(h.Inner, aliasIdx)
		return alias, true
	}
	if id, ok := e.(*ast.Ident); ok && len(id.Parts) >= 1 {
		first := strings.ToLower(id.Parts[0])
		if _, in := aliasIdx[first]; in {
			return first, false
		}
	}
	return "", false
}
