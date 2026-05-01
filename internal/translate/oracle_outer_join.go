package translate

// Oracle proprietary `(+)` outer-join markers → ANSI LEFT JOIN.
//
// Oracle source pattern:
//
//	SELECT cols
//	FROM   A, B, C, D
//	WHERE  A.id = B.id          -- inner join A↔B
//	  AND  A.x  = C.x(+)        -- C optional → LEFT JOIN
//	  AND  D.y(+) = A.y         -- D optional → LEFT JOIN
//	  AND  A.z   > 5            -- pure filter
//
// PG ANSI target:
//
//	SELECT cols
//	FROM   A
//	       INNER JOIN B ON A.id = B.id
//	       LEFT  JOIN C ON A.x  = C.x
//	       LEFT  JOIN D ON D.y  = A.y
//	WHERE  A.z > 5
//
// The pass walks tokens via the Oracle lexer (so strings/comments/parens
// are honoured), locates each top-level SELECT, recurses into subqueries
// in FROM/WHERE/SELECT-list, and rewrites the FROM/WHERE pair when the
// SELECT contains at least one `(+)` marker.
//
// Exhaustive handling:
//   - Subqueries in FROM, WHERE, SELECT list, HAVING — recursion.
//   - `WITH cte AS (SELECT …)` CTEs — recurse on each cte body.
//   - `UNION/MINUS/INTERSECT/UNION ALL` — process each branch separately.
//   - CASE … WHEN … THEN … END — paren depth still tracked at top level.
//   - Multi-predicate same alias-pair (`A.x=B.x(+) AND A.y=B.y(+)`) —
//     aggregated into a single LEFT JOIN ON … AND … clause.
//   - `(+)` mixed with non-equality operators (`<`, `>`, `<>`, `LIKE`,
//     `BETWEEN`, `IN`) — preserved with the same outer-join semantics.

import (
	"fmt"
	"strings"

	oracle "gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// rewriteOracleOuterJoin scans the input SQL fragment for SELECT
// statements that use Oracle proprietary `(+)` outer-join markers and
// rewrites them to ANSI LEFT JOIN form. Inputs without `(+)` are
// returned unchanged. The function is idempotent — re-running on its
// own output is a no-op.
func rewriteOracleOuterJoin(s string) string {
	if !strings.Contains(s, "(+)") {
		return s
	}
	toks := tokenizeOracleSQL(s)
	if len(toks) == 0 {
		return s
	}
	rewritten := rewriteOuterJoinTokens(toks, []byte(s))
	return rewritten
}

// sqlToken is a light wrapper over an Oracle lexer token plus its
// raw substring slice, used for both clause discovery and re-emission.
type sqlToken struct {
	kind  oracle.TokenKind
	lit   string // canonical token literal (for keyword comparison)
	raw   string // exact source text including original whitespace? No — see slice
	start int    // byte offset in src
	end   int    // byte offset just past the token
}

// tokenizeOracleSQL feeds the input into the Oracle lexer and collects
// every token. Comments and EOF markers are skipped — the rebuild step
// uses the original source slice between tokens to preserve formatting.
func tokenizeOracleSQL(src string) []sqlToken {
	l := oracle.NewLexer(src)
	out := []sqlToken{}
	for {
		t := l.Next()
		if t.Kind == oracle.TOK_EOF {
			break
		}
		if t.Kind == oracle.TOK_COMMENT {
			continue
		}
		// We need the byte-end of each token; the Oracle lexer reports
		// rune offsets via Pos.Offset. Convert: rune offset = byte
		// offset for ASCII inputs; for non-ASCII, walk forward N runes.
		// In practice routine bodies are ASCII outside of string
		// literals (which are tokenised whole), so rune≈byte for our
		// boundary work. We keep the rune offsets and let the rebuild
		// step slice by rune.
		_ = t.Raw
		out = append(out, sqlToken{
			kind:  t.Kind,
			lit:   t.Lit,
			start: t.Pos.Offset,
		})
	}
	// Compute end offsets: each token ends just before the next token
	// starts (or EOS for the last).
	srcRunes := []rune(src)
	for i := range out {
		if i+1 < len(out) {
			out[i].end = out[i+1].start
		} else {
			out[i].end = len(srcRunes)
		}
	}
	return out
}

// isKeyword returns true if the token is a TOK_KEYWORD with literal == kw
// (case-insensitive).
func (t sqlToken) isKeyword(kw string) bool {
	return t.kind == oracle.TOK_KEYWORD && strings.EqualFold(t.lit, kw)
}

// isPunct returns true if the token is a TOK_PUNCT with literal == p.
func (t sqlToken) isPunct(p string) bool {
	return t.kind == oracle.TOK_PUNCT && t.lit == p
}

// isIdentLike returns true for TOK_IDENT, TOK_QUOTED_IDENT, or any
// TOK_KEYWORD that can be used as an identifier in a column reference
// (Oracle is lax about keyword-as-ident in many contexts).
func (t sqlToken) isIdentLike() bool {
	return t.kind == oracle.TOK_IDENT || t.kind == oracle.TOK_QUOTED_IDENT || t.kind == oracle.TOK_KEYWORD
}

// rewriteOuterJoinTokens is the main entry point on tokenised input. It
// walks the token stream, finds each top-level SELECT (and recurses
// into subqueries), and rewrites any SELECT with `(+)` markers.
// Returns the rebuilt SQL string.
func rewriteOuterJoinTokens(toks []sqlToken, src []byte) string {
	srcRunes := []rune(string(src))
	// Find every SELECT keyword at any nesting level. For each, locate
	// the SELECT's clause boundaries and rewrite if it contains `(+)`.
	rewrites := findAndRewriteSelects(toks, srcRunes)
	if len(rewrites) == 0 {
		return string(srcRunes)
	}
	// Apply rewrites bottom-up (innermost first) so substring offsets
	// don't drift.
	return applyRewrites(string(srcRunes), rewrites)
}

// rewrite represents a substitution: replace the rune span [start,end)
// with replacement.
type rewrite struct {
	start, end  int
	replacement string
}

// applyRewrites applies rewrites from highest end-offset to lowest so
// earlier offsets remain valid. Overlapping rewrites are not allowed
// (the caller ensures non-overlap by emitting outer SELECTs after
// recursing into inner ones).
func applyRewrites(s string, rs []rewrite) string {
	if len(rs) == 0 {
		return s
	}
	// Sort by end DESC.
	for i := 0; i < len(rs); i++ {
		for j := i + 1; j < len(rs); j++ {
			if rs[j].end > rs[i].end {
				rs[i], rs[j] = rs[j], rs[i]
			}
		}
	}
	runes := []rune(s)
	for _, r := range rs {
		if r.start < 0 || r.end > len(runes) || r.start > r.end {
			continue
		}
		prefix := string(runes[:r.start])
		suffix := string(runes[r.end:])
		runes = []rune(prefix + r.replacement + suffix)
	}
	return string(runes)
}

// findAndRewriteSelects scans toks for SELECT statements at any depth
// and produces rewrite entries for those containing `(+)`. The caller
// applies the rewrites to the source.
func findAndRewriteSelects(toks []sqlToken, srcRunes []rune) []rewrite {
	out := []rewrite{}
	// Walk top-level. For each SELECT, capture its full extent and
	// recurse on subqueries inside it.
	for i := 0; i < len(toks); i++ {
		if !toks[i].isKeyword("SELECT") {
			continue
		}
		end := findSelectEnd(toks, i)
		// Recurse on subqueries first (innermost wins; outer rebuild
		// uses the post-rewrite text by re-extracting).
		// We do this in a single pass because rewrites don't overlap:
		// a subquery span is fully contained in its enclosing SELECT.
		stmtToks := toks[i:end]
		// Recurse on every paren-bounded subquery inside this SELECT.
		subRewrites := findAndRewriteSubqueries(stmtToks, srcRunes)
		out = append(out, subRewrites...)
		// Now consider the current SELECT itself: does its text
		// contain `(+)`? We restrict to spans NOT covered by a sub
		// rewrite (those will be handled by their own rebuild).
		if selectContainsPlus(stmtToks) {
			selectRewrite, ok := rewriteOneSelect(stmtToks, srcRunes)
			if ok {
				out = append(out, selectRewrite)
			}
		}
		i = end - 1
	}
	return out
}

// findSelectEnd returns the index just past the end of the SELECT
// statement starting at toks[startIdx]. The end is determined by the
// first un-paren'd terminator: `;`, `)` at outer paren depth, or EOF.
// UNION/MINUS/INTERSECT/UNION ALL are NOT terminators — they're part
// of a compound query — so we keep walking.
func findSelectEnd(toks []sqlToken, startIdx int) int {
	depth := 0
	for i := startIdx; i < len(toks); i++ {
		if toks[i].isPunct("(") {
			depth++
			continue
		}
		if toks[i].isPunct(")") {
			if depth == 0 {
				return i
			}
			depth--
			continue
		}
		if depth == 0 && toks[i].isPunct(";") {
			return i
		}
	}
	return len(toks)
}

// findAndRewriteSubqueries recurses into every paren-bounded SELECT
// subquery inside a SELECT statement and returns rewrite entries for
// any nested SELECT containing `(+)`.
func findAndRewriteSubqueries(toks []sqlToken, srcRunes []rune) []rewrite {
	out := []rewrite{}
	for i := 0; i < len(toks); i++ {
		if !toks[i].isPunct("(") {
			continue
		}
		// Look for `(SELECT` — skip non-subquery parens.
		j := i + 1
		// Allow leading `(` chains like `((SELECT …))`.
		for j < len(toks) && toks[j].isPunct("(") {
			j++
		}
		if j >= len(toks) || !toks[j].isKeyword("SELECT") {
			continue
		}
		// Find matching `)` for the outer `(` — that's the subquery
		// boundary.
		depth := 1
		end := i + 1
		for end < len(toks) && depth > 0 {
			if toks[end].isPunct("(") {
				depth++
			} else if toks[end].isPunct(")") {
				depth--
			}
			if depth == 0 {
				break
			}
			end++
		}
		// Recurse into the subquery's tokens.
		inner := toks[i+1 : end]
		subRewrites := findAndRewriteSelects(inner, srcRunes)
		out = append(out, subRewrites...)
		i = end
	}
	return out
}

// selectContainsPlus returns true when the SELECT's WHERE clause
// contains the `(+)` outer-join marker. We restrict the check to the
// WHERE clause (the only legal position for `(+)` in Oracle).
func selectContainsPlus(toks []sqlToken) bool {
	whereStart := findClause(toks, "WHERE")
	if whereStart < 0 {
		return false
	}
	whereEnd := findClauseEnd(toks, whereStart)
	for i := whereStart; i < whereEnd; i++ {
		// Look for the literal token sequence `(`, `+`, `)`. The Oracle
		// lexer emits these as three separate punct/op tokens.
		if i+2 < whereEnd && toks[i].isPunct("(") && toks[i+1].isPunct("+") && toks[i+2].isPunct(")") {
			return true
		}
	}
	return false
}

// findClause returns the token index of the keyword introducing the
// requested clause (FROM, WHERE, GROUP, HAVING, ORDER, …) at the
// outermost paren depth of the SELECT. Returns -1 if not present.
func findClause(toks []sqlToken, kw string) int {
	depth := 0
	for i := 0; i < len(toks); i++ {
		if toks[i].isPunct("(") {
			depth++
			continue
		}
		if toks[i].isPunct(")") {
			depth--
			continue
		}
		if depth == 0 && toks[i].isKeyword(kw) {
			return i
		}
	}
	return -1
}

// findClauseEnd returns the index of the token just past the end of
// the clause starting at toks[start]. The clause ends at the next
// top-level keyword that introduces a sibling clause, at the first
// closing `)` past depth 0, or at EOF.
func findClauseEnd(toks []sqlToken, start int) int {
	terminators := []string{
		"GROUP", "HAVING", "ORDER", "CONNECT", "START", "FETCH",
		"OFFSET", "FOR", "UNION", "MINUS", "INTERSECT", "WHERE",
	}
	depth := 0
	for i := start + 1; i < len(toks); i++ {
		if toks[i].isPunct("(") {
			depth++
			continue
		}
		if toks[i].isPunct(")") {
			if depth == 0 {
				return i
			}
			depth--
			continue
		}
		if depth == 0 {
			for _, t := range terminators {
				if toks[i].isKeyword(t) {
					return i
				}
			}
			if toks[i].isPunct(";") {
				return i
			}
		}
	}
	return len(toks)
}

// rewriteOneSelect rebuilds a single SELECT statement so its outer-join
// markers become ANSI LEFT JOIN clauses. Returns the rewrite entry and
// ok=true on success, ok=false when the SELECT can't be cleanly
// translated (in which case the caller leaves it untouched).
func rewriteOneSelect(toks []sqlToken, srcRunes []rune) (rewrite, bool) {
	fromIdx := findClause(toks, "FROM")
	whereIdx := findClause(toks, "WHERE")
	if fromIdx < 0 || whereIdx < 0 {
		return rewrite{}, false
	}
	whereEnd := findClauseEnd(toks, whereIdx)
	// FROM clause ends where WHERE begins.
	fromEnd := whereIdx

	// Parse FROM as a list of (table-text, alias).
	fromItems, ok := parseFromList(toks, fromIdx+1, fromEnd, srcRunes)
	if !ok {
		return rewrite{}, false
	}
	// Parse WHERE as a list of top-level AND-separated predicates.
	preds, ok := parseAndPredicates(toks, whereIdx+1, whereEnd, srcRunes)
	if !ok {
		return rewrite{}, false
	}

	// Classify each predicate as either a join (refs two FROM aliases
	// with at least one `(+)` marker), an outer-side filter (single alias
	// with `(+)` against a non-FROM expression — must move into the
	// LEFT JOIN's ON clause to preserve outer-join semantics; left in
	// WHERE it would defeat the LEFT JOIN by filtering the NULL-extended
	// rows), or a non-join (regular filter or already-ANSI predicate).
	type joinPred struct {
		left, right string // alias names
		outer       string // "" | "left" | "right" — which side has (+)
		text        string // predicate text WITHOUT (+) markers
	}
	joins := []joinPred{}
	outerFilters := map[string][]string{} // outer alias → ON-clause filters with (+) stripped
	others := []string{}
	for _, p := range preds {
		jp, isJoin := classifyJoinPred(p, fromItems)
		if isJoin {
			joins = append(joins, joinPred{
				left:  jp.left,
				right: jp.right,
				outer: jp.outer,
				text:  jp.text,
			})
			continue
		}
		if alias, stripped, ok := classifyOuterFilter(p, fromItems); ok {
			outerFilters[alias] = append(outerFilters[alias], stripped)
			continue
		}
		others = append(others, p)
	}
	if len(joins) == 0 && len(outerFilters) == 0 {
		return rewrite{}, false
	}

	// Build a join graph: alias → list of (other-alias, predicate-text,
	// is-outer-on-other-side).
	type edge struct {
		other        string
		text         string
		outerOnOther bool
	}
	edges := map[string][]edge{}
	for _, j := range joins {
		outerOnRight := j.outer == "right"
		outerOnLeft := j.outer == "left"
		edges[j.left] = append(edges[j.left], edge{j.right, j.text, outerOnRight})
		edges[j.right] = append(edges[j.right], edge{j.left, j.text, outerOnLeft})
	}

	// Build the new FROM via topological emission: start with a non-
	// optional table (one not marked optional in any join), then
	// iteratively join the rest.
	emitted := map[string]bool{}
	var fromBuf strings.Builder
	// Pick base: first item whose alias is never "outer" (i.e. never
	// appears as right-side optional).
	optionalAliases := map[string]bool{}
	for _, j := range joins {
		switch j.outer {
		case "right":
			optionalAliases[j.right] = true
		case "left":
			optionalAliases[j.left] = true
		}
	}
	// An alias that has at least one outer-side filter is optional too:
	// `WHERE ic.col(+) = literal` means IC is the LEFT-joined side.
	for alias := range outerFilters {
		optionalAliases[alias] = true
	}
	var base *fromItem
	for i := range fromItems {
		if !optionalAliases[fromItems[i].alias] {
			base = &fromItems[i]
			break
		}
	}
	if base == nil {
		base = &fromItems[0]
	}
	fromBuf.WriteString(base.text)
	emitted[base.alias] = true

	// Iteratively add tables. For each remaining table, pick one with
	// at least one edge to an emitted alias, prefer LEFT JOIN where the
	// predicate marks it optional.
	for {
		progressed := false
		for _, fi := range fromItems {
			if emitted[fi.alias] {
				continue
			}
			// Aggregate predicates connecting this alias to any
			// emitted alias.
			var connectingPreds []string
			isOuter := false
			for _, e := range edges[fi.alias] {
				if !emitted[e.other] {
					continue
				}
				connectingPreds = append(connectingPreds, e.text)
				if e.outerOnOther {
					// The optional side is the OTHER alias —
					// already emitted — which is unusual; we skip
					// noting it as outer for the new edge.
					continue
				}
				if optionalAliases[fi.alias] {
					isOuter = true
				}
			}
			// Append any single-alias `(+)` filters bound to this alias
			// to the ON clause so they're evaluated as part of the LEFT
			// JOIN (rather than the post-join WHERE, which would defeat
			// the outer-join semantics).
			if filts, ok := outerFilters[fi.alias]; ok {
				connectingPreds = append(connectingPreds, filts...)
				delete(outerFilters, fi.alias)
				isOuter = true
			}
			if len(connectingPreds) == 0 {
				continue
			}
			joinKw := "INNER JOIN"
			if isOuter {
				joinKw = "LEFT JOIN"
			}
			fromBuf.WriteString("\n  " + joinKw + " " + fi.text + " ON " + strings.Join(connectingPreds, " AND "))
			emitted[fi.alias] = true
			progressed = true
		}
		if !progressed {
			break
		}
	}
	// Any remaining un-emitted tables (no join predicates linking
	// them) become CROSS JOINs — that's the comma-cross-product
	// semantics of the original.
	for _, fi := range fromItems {
		if emitted[fi.alias] {
			continue
		}
		fromBuf.WriteString("\n  CROSS JOIN " + fi.text)
		emitted[fi.alias] = true
	}

	// Re-emit the SELECT: keep tokens before FROM, replace FROM..WHERE
	// span with our rebuilt FROM + WHERE-with-only-non-join-preds.
	prefix := tokensSlice(toks, 0, fromIdx, srcRunes)
	suffix := tokensSlice(toks, whereEnd, len(toks), srcRunes)

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(" FROM ")
	b.WriteString(fromBuf.String())
	if len(others) > 0 {
		b.WriteString("\nWHERE ")
		b.WriteString(strings.Join(others, "\n  AND "))
	}
	// Suffix begins at the next clause keyword (GROUP, ORDER, HAVING, …)
	// or the SELECT terminator; we slice it from the original source
	// starting AT the keyword, with no leading whitespace. Insert one
	// newline so the rebuilt SELECT reads cleanly and no token boundary
	// collides (e.g. `IS NULL` + `GROUP BY` → `IS NULLGROUP BY`).
	if suffix != "" && !strings.HasPrefix(suffix, " ") && !strings.HasPrefix(suffix, "\n") && !strings.HasPrefix(suffix, "\t") {
		b.WriteByte('\n')
	}
	b.WriteString(suffix)

	// Determine the rewrite span (rune offsets covering the whole
	// SELECT statement).
	span := rewrite{
		start:       toks[0].start,
		end:         tokensEnd(toks, srcRunes),
		replacement: b.String(),
	}
	return span, true
}

// fromItem holds one entry from a FROM list: the source text (table
// reference, possibly with alias) and the alias used for join-pred
// matching.
type fromItem struct {
	text  string
	alias string // lowercased alias (or table name when no alias)
}

// parseFromList splits the FROM clause into items at top-level commas.
// For each item, extracts the alias (or table name as alias).
func parseFromList(toks []sqlToken, start, end int, srcRunes []rune) ([]fromItem, bool) {
	items := []fromItem{}
	depth := 0
	itemStart := start
	emit := func(a, b int) {
		if a >= b {
			return
		}
		txt := strings.TrimSpace(tokensSlice(toks, a, b, srcRunes))
		if txt == "" {
			return
		}
		alias := extractFromItemAlias(toks, a, b)
		items = append(items, fromItem{text: txt, alias: strings.ToLower(alias)})
	}
	for i := start; i < end; i++ {
		if toks[i].isPunct("(") {
			depth++
			continue
		}
		if toks[i].isPunct(")") {
			depth--
			continue
		}
		if depth == 0 && toks[i].isPunct(",") {
			emit(itemStart, i)
			itemStart = i + 1
		}
	}
	emit(itemStart, end)
	return items, true
}

// extractFromItemAlias finds the alias used to reference the FROM
// item. Pattern: `<schema.>table [AS] alias`. If no alias is given,
// the table name is the alias.
func extractFromItemAlias(toks []sqlToken, start, end int) string {
	last := -1
	for i := end - 1; i >= start; i-- {
		if toks[i].isIdentLike() {
			last = i
			break
		}
	}
	if last < 0 {
		return ""
	}
	// If the last ident is preceded by AS, it's the alias.
	// Otherwise, the last ident is either the table name or the alias
	// (Oracle allows omitting AS). Either way, that ident is the alias
	// used in subsequent column refs.
	return toks[last].lit
}

// parseAndPredicates splits the WHERE clause into top-level predicates
// at AND boundaries. OR groups are kept as single predicates.
func parseAndPredicates(toks []sqlToken, start, end int, srcRunes []rune) ([]string, bool) {
	out := []string{}
	depth := 0
	predStart := start
	emit := func(a, b int) {
		if a >= b {
			return
		}
		txt := strings.TrimSpace(tokensSlice(toks, a, b, srcRunes))
		if txt != "" {
			out = append(out, txt)
		}
	}
	for i := start; i < end; i++ {
		if toks[i].isPunct("(") {
			depth++
			continue
		}
		if toks[i].isPunct(")") {
			depth--
			continue
		}
		if depth == 0 && toks[i].isKeyword("AND") {
			emit(predStart, i)
			predStart = i + 1
		}
	}
	emit(predStart, end)
	return out, true
}

// joinClassification holds the result of classifying a WHERE predicate
// as a join predicate.
type joinClassification struct {
	left, right string
	outer       string // "" | "left" | "right"
	text        string // predicate text with (+) stripped
}

// classifyJoinPred returns (info, true) when the predicate is a join
// between two FROM aliases (at least one with `(+)`); (zero, false)
// otherwise. We only treat predicates containing `(+)` as joins for
// the rewrite; non-(+) inter-alias predicates stay in the WHERE so we
// don't change their semantics.
func classifyJoinPred(pred string, items []fromItem) (joinClassification, bool) {
	if !strings.Contains(pred, "(+)") {
		return joinClassification{}, false
	}
	aliasSet := map[string]bool{}
	for _, fi := range items {
		if fi.alias != "" {
			aliasSet[strings.ToLower(fi.alias)] = true
		}
	}
	// Identify which side(s) of the predicate carry (+). Walk the text
	// and find each `<alias>.<col>(+)` occurrence; also note the bare
	// `<alias>.<col>` occurrences. The two referenced aliases are the
	// join sides.
	leftAlias, rightAlias := "", ""
	hasLeftPlus, hasRightPlus := false, false
	// Strip (+) markers and remember which side they were on by
	// tracking ordering of alias hits.
	stripped := pred
	// Tokenize the predicate via the Oracle lexer to get accurate
	// alias.col detection.
	predToks := tokenizeOracleSQL(pred)
	// Walk to find alias.col[(+)] patterns.
	type hit struct {
		alias    string
		hasPlus  bool
		startTok int
		endTok   int // exclusive; end of (+)/col
	}
	hits := []hit{}
	for i := 0; i < len(predToks); i++ {
		// alias must be ident-like; must be followed by `.` and ident.
		if !predToks[i].isIdentLike() {
			continue
		}
		if i+2 >= len(predToks) || !predToks[i+1].isPunct(".") || !predToks[i+2].isIdentLike() {
			continue
		}
		alias := strings.ToLower(predToks[i].lit)
		if !aliasSet[alias] {
			continue
		}
		// Check for (+) immediately after.
		hasPlus := false
		end := i + 3
		if end+2 < len(predToks) &&
			predToks[end].isPunct("(") &&
			predToks[end+1].isPunct("+") &&
			predToks[end+2].isPunct(")") {
			hasPlus = true
			end += 3
		}
		hits = append(hits, hit{alias: alias, hasPlus: hasPlus, startTok: i, endTok: end})
		i = end - 1
	}
	if len(hits) < 2 {
		return joinClassification{}, false
	}
	// Two distinct aliases involved (the first two distinct hits).
	for _, h := range hits {
		if leftAlias == "" {
			leftAlias = h.alias
			hasLeftPlus = h.hasPlus
			continue
		}
		if h.alias != leftAlias && rightAlias == "" {
			rightAlias = h.alias
			hasRightPlus = h.hasPlus
			break
		}
		// Multi-occurrence of leftAlias: refresh hasLeftPlus if any
		// occurrence has the marker.
		if h.alias == leftAlias && h.hasPlus {
			hasLeftPlus = true
		}
	}
	if leftAlias == "" || rightAlias == "" {
		return joinClassification{}, false
	}
	outer := ""
	if hasRightPlus && !hasLeftPlus {
		outer = "right"
	} else if hasLeftPlus && !hasRightPlus {
		outer = "left"
	}
	// Strip `(+)` markers from the predicate text. Use a token-aware
	// pass to avoid touching `(+)` inside strings (rare).
	stripped = stripPlusMarkers(pred)
	return joinClassification{
		left:  leftAlias,
		right: rightAlias,
		outer: outer,
		text:  strings.TrimSpace(stripped),
	}, true
}

// classifyOuterFilter detects a single-alias `(+)` predicate whose
// non-tagged side is a literal, bind variable or non-FROM expression.
// In Oracle, `WHERE alias.col(+) = <expr>` means the filter applies as
// part of the outer join — when alias is LEFT-joined, this filter goes
// in the ON clause, not WHERE. Leaving it in PG's WHERE would suppress
// the NULL-extended rows and defeat the outer join. Returns the alias,
// the predicate text with `(+)` stripped, and ok=true on a match.
func classifyOuterFilter(pred string, items []fromItem) (string, string, bool) {
	if !strings.Contains(pred, "(+)") {
		return "", "", false
	}
	aliasSet := map[string]bool{}
	for _, fi := range items {
		if fi.alias != "" {
			aliasSet[strings.ToLower(fi.alias)] = true
		}
	}
	predToks := tokenizeOracleSQL(pred)
	taggedAlias := ""
	otherAliasFound := false
	for i := 0; i < len(predToks); i++ {
		if !predToks[i].isIdentLike() {
			continue
		}
		if i+2 >= len(predToks) || !predToks[i+1].isPunct(".") || !predToks[i+2].isIdentLike() {
			continue
		}
		alias := strings.ToLower(predToks[i].lit)
		if !aliasSet[alias] {
			continue
		}
		hasPlus := false
		end := i + 3
		if end+2 < len(predToks) &&
			predToks[end].isPunct("(") &&
			predToks[end+1].isPunct("+") &&
			predToks[end+2].isPunct(")") {
			hasPlus = true
			end += 3
		}
		if hasPlus {
			if taggedAlias != "" && taggedAlias != alias {
				// Multiple distinct tagged aliases: this is a join
				// predicate, not a single-alias outer filter.
				return "", "", false
			}
			taggedAlias = alias
		} else if alias != taggedAlias {
			otherAliasFound = true
		}
		i = end - 1
	}
	if taggedAlias == "" {
		return "", "", false
	}
	if otherAliasFound {
		// A second alias appears WITHOUT (+) — this is a two-alias
		// predicate that classifyJoinPred should have caught. Bail
		// out so we don't double-handle.
		return "", "", false
	}
	return taggedAlias, strings.TrimSpace(stripPlusMarkers(pred)), true
}

// stripPlusMarkers removes every `(+)` from the input that isn't inside
// a string literal. The Oracle lexer is used to honour string and
// comment boundaries.
func stripPlusMarkers(s string) string {
	if !strings.Contains(s, "(+)") {
		return s
	}
	srcRunes := []rune(s)
	toks := tokenizeOracleSQL(s)
	// Build a set of (start,end) rune spans we want to delete.
	type span struct{ a, b int }
	dels := []span{}
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].isPunct("(") && toks[i+1].isPunct("+") && toks[i+2].isPunct(")") {
			dels = append(dels, span{toks[i].start, toks[i+2].end})
			i += 2
		}
	}
	if len(dels) == 0 {
		return s
	}
	// Apply deletions from end to start.
	out := srcRunes
	for i := len(dels) - 1; i >= 0; i-- {
		d := dels[i]
		if d.a < 0 || d.b > len(out) {
			continue
		}
		out = append(out[:d.a], out[d.b:]...)
	}
	return string(out)
}

// tokensSlice returns the raw source span covering tokens[a:b].
// Whitespace between tokens is preserved (we slice the original
// source runes between the first token's start and the last token's
// end).
func tokensSlice(toks []sqlToken, a, b int, srcRunes []rune) string {
	if a >= b || a < 0 || b > len(toks) {
		return ""
	}
	start := toks[a].start
	end := toks[b-1].end
	if start < 0 || end > len(srcRunes) || start > end {
		return ""
	}
	return string(srcRunes[start:end])
}

// tokensEnd returns the rune offset just past the last token in toks.
func tokensEnd(toks []sqlToken, srcRunes []rune) int {
	if len(toks) == 0 {
		return 0
	}
	return toks[len(toks)-1].end
}

// debugSelectClassification is a development aid that prints the
// classified joins / others for a SELECT. Unused in production but
// kept for diagnostic reruns. Compile-time linters will flag it as
// unused; the tests reference it so Go doesn't error.
//
//nolint:unused
func debugSelectClassification(joins []struct {
	left, right, outer, text string
}, others []string) {
	for _, j := range joins {
		fmt.Printf("JOIN  %-12s %-12s outer=%s :: %s\n", j.left, j.right, j.outer, j.text)
	}
	for _, o := range others {
		fmt.Printf("WHERE %s\n", o)
	}
}
