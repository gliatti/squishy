package translate

import (
	"fmt"
	"strings"

	oracledialect "gitlab.com/dalibo/squishy/internal/dialects/oracle"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// normalizeOracleIdent converts an Oracle identifier to its PG-friendly form.
//
// Oracle semantics: unquoted identifiers are case-folded to UPPERCASE and
// quoted identifiers preserve case. DBMS_METADATA always emits everything
// quoted + uppercase, so we lose the "was originally unquoted" signal. To
// restore PG compatibility we apply a two-rule heuristic:
//
//   • all-uppercase → lowercase (was an unquoted identifier; PG default
//     case is lowercase, so lowercasing lets CHECK/DEFAULT/rawSQL fragments
//     match without being quoted).
//   • mixed-case (or has lowercase already) → preserved verbatim so it
//     keeps round-tripping as a quoted identifier on both sides.
//
// Oracle also accepts `#` as a regular identifier byte (idiomatic in
// trace/journal infra: MVT#ABT, IDX#MVT#xxx). PG only allows `#` inside
// double-quoted identifiers, and procedures that build DDL via string
// concatenation can't be quoted in-place — so we replace `#` with `_`
// here. This must stay in lockstep with rewriteOracleHashInIdentLiterals
// (the body-string version) so both sides agree on the post-migration
// table names.
func normalizeOracleIdent(s string) string {
	if s == "" {
		return s
	}
	out := s
	if strings.ToUpper(out) == out && strings.ToLower(out) != out {
		out = strings.ToLower(out)
	}
	if strings.ContainsRune(out, '#') {
		out = strings.ReplaceAll(out, "#", "_")
	}
	return out
}

// normalizeOracleTable rewrites the table / column / constraint / index names
// of a CreateTable to their PG-normalized form. Also normalizes identifiers
// inside CHECK-expression raw text so they match the column names we emit.
func normalizeOracleTable(s *ast.CreateTable) {
	s.Name = normalizeOracleIdent(s.Name)
	s.Schema = normalizeOracleIdent(s.Schema)
	for _, c := range s.Columns {
		c.Name = normalizeOracleIdent(c.Name)
		if c.Check != nil {
			if r, ok := c.Check.(*ast.RawExpr); ok {
				r.Text = normalizeRawIdents(r.Text)
			}
		}
		if c.Generated != nil {
			if r, ok := c.Generated.Expr.(*ast.RawExpr); ok {
				r.Text = normalizeRawIdents(r.Text)
			}
		}
	}
	for _, cons := range s.Constraints {
		switch x := cons.(type) {
		case *ast.PKConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			for i := range x.Columns {
				x.Columns[i].Name = normalizeOracleIdent(x.Columns[i].Name)
			}
		case *ast.UQConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			for i := range x.Columns {
				x.Columns[i].Name = normalizeOracleIdent(x.Columns[i].Name)
			}
		case *ast.FKConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			for i := range x.Columns {
				x.Columns[i] = normalizeOracleIdent(x.Columns[i])
			}
			for i := range x.RefColumns {
				x.RefColumns[i] = normalizeOracleIdent(x.RefColumns[i])
			}
			x.RefSchema = normalizeOracleIdent(x.RefSchema)
			x.RefTable = normalizeOracleIdent(x.RefTable)
		case *ast.CheckConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			if r, ok := x.Expr.(*ast.RawExpr); ok {
				r.Text = normalizeRawIdents(r.Text)
			}
		}
	}
	for _, idx := range s.Indexes {
		idx.Name = normalizeOracleIdent(idx.Name)
		for i := range idx.Columns {
			idx.Columns[i].Name = normalizeOracleIdent(idx.Columns[i].Name)
		}
	}
}

// stripOracleViewTrailers removes Oracle-only trailing clauses from a captured
// view SELECT body — currently `WITH READ ONLY` and `WITH CHECK OPTION`. PG
// has no in-body equivalent for these (read-only is a privilege, and check
// option attaches to the CREATE VIEW statement itself, not the SELECT). Both
// cause syntax errors when the body is dropped verbatim into a PG view.
func stripOracleViewTrailers(body string) string {
	trimmed := strings.TrimRight(body, " \t\r\n;")
	upper := strings.ToUpper(trimmed)
	for _, suffix := range []string{
		"WITH READ ONLY",
		"WITH CHECK OPTION",
	} {
		if strings.HasSuffix(upper, suffix) {
			cut := len(trimmed) - len(suffix)
			trimmed = strings.TrimRight(trimmed[:cut], " \t\r\n")
			upper = strings.ToUpper(trimmed)
		}
	}
	return trimmed
}

// normalizeOracleAlterTable applies the same identifier folding rules as
// normalizeOracleTable to constraint references emitted by an
// `ALTER TABLE … ADD CONSTRAINT …` statement. DBMS_METADATA emits the table
// name and the constraint columns in their stored (uppercase) form, but the
// translator already lowercased the matching CREATE TABLE — without this
// pass the resulting PG DDL has e.g. PRIMARY KEY ("DEPARTMENT_ID") on a
// column named "department_id", which PG treats as a missing column.
func normalizeOracleAlterTable(s *ast.AlterTable) {
	s.Table.Schema = normalizeOracleIdent(s.Table.Schema)
	s.Table.Name = normalizeOracleIdent(s.Table.Name)
	for i := range s.Actions {
		switch x := s.Actions[i].Constraint.(type) {
		case *ast.PKConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			for j := range x.Columns {
				x.Columns[j].Name = normalizeOracleIdent(x.Columns[j].Name)
			}
		case *ast.UQConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			for j := range x.Columns {
				x.Columns[j].Name = normalizeOracleIdent(x.Columns[j].Name)
			}
		case *ast.FKConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			for j := range x.Columns {
				x.Columns[j] = normalizeOracleIdent(x.Columns[j])
			}
			for j := range x.RefColumns {
				x.RefColumns[j] = normalizeOracleIdent(x.RefColumns[j])
			}
			x.RefSchema = normalizeOracleIdent(x.RefSchema)
			x.RefTable = normalizeOracleIdent(x.RefTable)
		case *ast.CheckConstraint:
			x.Name = normalizeOracleIdent(x.Name)
			if r, ok := x.Expr.(*ast.RawExpr); ok {
				r.Text = normalizeRawIdents(r.Text)
			}
		}
	}
}

// normalizeRawIdents walks a raw SQL fragment and lowercases every sequence
// of ASCII letters/digits/underscores that is entirely uppercase. Quoted
// identifiers that are also all-uppercase are unwrapped to their lowercase
// unquoted equivalent (so they match the column names we emit unquoted);
// mixed-case quoted identifiers are left intact. String literals ('...')
// are preserved verbatim.
func normalizeRawIdents(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		r := s[i]
		switch {
		case r == '"':
			// Find the closing quote.
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			inside := ""
			if j < len(s) {
				inside = s[i+1 : j]
			}
			if inside != "" && strings.ToUpper(inside) == inside && strings.ToLower(inside) != inside {
				// All-caps quoted ident → emit lowercase, drop the quotes.
				// Also strip Oracle-style `#` from the name (PG rejects
				// it in unquoted identifiers).
				out.WriteString(strings.ReplaceAll(strings.ToLower(inside), "#", "_"))
			} else {
				// Keep verbatim (including quotes) to preserve mixed-case.
				out.WriteByte('"')
				out.WriteString(inside)
				if j < len(s) {
					out.WriteByte('"')
				}
			}
			i = j + 1
			continue
		case r == '\'':
			// String literal — copy verbatim, handling '' escape.
			out.WriteByte(r)
			i++
			for i < len(s) {
				if s[i] == '\'' {
					out.WriteByte(s[i])
					i++
					if i < len(s) && s[i] == '\'' {
						out.WriteByte(s[i])
						i++
						continue
					}
					break
				}
				out.WriteByte(s[i])
				i++
			}
		case isIdentStart(r):
			// Collect a word.
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			word := s[i:j]
			if strings.ToUpper(word) == word && strings.ToLower(word) != word {
				word = strings.ToLower(word)
			}
			// Replace Oracle-style `#` in identifiers (which PG rejects
			// unquoted) with `_`. Stays in lockstep with normalizeOracleIdent
			// and rewriteOracleHashInIdentLiterals.
			if strings.ContainsRune(word, '#') {
				word = strings.ReplaceAll(word, "#", "_")
			}
			out.WriteString(word)
			i = j
		default:
			out.WriteByte(r)
			i++
		}
	}
	return out.String()
}

func isIdentStart(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

// isIdentStartByte mirrors isIdentStart — exposed under a slightly
// different name because two passes in this file already use it.
// Restored after the dynamic-DDL revert (the original definition
// lived in the now-removed oracle_dynamic_ddl.go).
func isIdentStartByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9') || b == '$' || b == '#'
}

// rewriteOracleExpr normalizes an Oracle expression snippet into the
// PostgreSQL equivalent, focussing on the constructs that break when left
// literal (function names PG doesn't recognize, operator quirks, etc.).
//
// Scope (v1):
//   SYSDATE          → CURRENT_TIMESTAMP::timestamp(0)
//   SYSTIMESTAMP     → CURRENT_TIMESTAMP
//   LOCALTIMESTAMP   → LOCALTIMESTAMP              (same in PG)
//   CURRENT_DATE     → CURRENT_DATE                (same in PG)
//   NVL(a,b)         → COALESCE(a,b)
//   NVL2(a,b,c)      → CASE WHEN a IS NOT NULL THEN b ELSE c END  (shallow)
//   DECODE(e,k1,v1[,k2,v2]*[,def]) → CASE expression              (shallow)
//   SEQ.NEXTVAL      → nextval('seq')
//   SEQ.CURRVAL      → currval('seq')
//   (+)              → outer-join marker removed + warning tag
//
// The rewriter works on tokenized-ish text using simple regexes; it is
// deliberately conservative — anything it can't pattern-match is left as-is
// so the downstream SQL error surfaces in the plan rather than being masked.
// rewriteOracleExpr — DEFENSIVE TEXT FALLBACK after the AST orchestrator.
//
// Phase 7 closed every visitor coverage gap the notice.drse corpus
// surfaced — the AST visitors in internal/translate/oracle_visitors_*.go
// run BEFORE this pipeline (RewriteOracleAST, wired into
// TranslateRoutineBodyExtV) and substitute typed AST nodes for the
// canonical patterns. This text pipeline runs AFTER and is
// idempotent on AST-substituted input: each pass below is a no-op
// when the corresponding AST visitor already produced the PG form.
//
// The pipeline survives because the AST visitors ARE PARTIAL by
// design — they match the typed-node shapes parser_expr.go produces.
// Patterns embedded in *ast.RawExpr fragments (legacy code paths
// that haven't migrated to typed Expr yet, plus dialect-specific
// constructs like SDO Spatial that no AST visitor models) still
// need the text fallback. Phase 5 originally proposed a bulk-remove;
// the e2e revealed that SDO Spatial in oracle_collection_expr.go's
// rewriteOracleCollectionTokens has no AST equivalent yet, so the
// chain stays in place.
//
// Each pass is annotated with its AST-visitor counterpart (or
// "(no AST equivalent)" when only the text path covers it):
//
//	rewriteOracleStripSysPrefix          ↔ VisitOracleStripSysPrefix (FuncCall path)
//	rewriteOracleDbmsUtilityFormatCallStack ↔ VisitOracleDbmsUtilityFormatCallStack
//	rewriteOracleQuotedPackageCalls      ↔ VisitOracleQuotedPackageCalls
//	trimOracleDbmsSqlParseLanguageArg    ↔ VisitOracleTrimDbmsSqlParseLanguageArg
//	rewriteOracleCursorAttributes        ↔ rawExpr's CursorAttr case
//	rewriteOracleRownumLimit             ↔ VisitOracleRownumLimit (single-clause)
//	rewriteOracleHashInIdentLiterals     ↔ VisitOracleHashInIdentLiterals (Literal path)
//	qualifyOracleToCharSingleArg         ↔ VisitOracleQualifyToCharSingleArg
//	rewriteOracleNestedAggregates        ↔ VisitOracleNestedAggregates (single-col)
//	qualifyOracleDecode                  ↔ VisitOracleQualifyDecode (qualifier-only)
//	                                        + legacy CASE expansion for >7 args
//	rewriteOracleDictionaryViews         ↔ VisitOracleDictionaryViews (1-part Ident)
//	rewriteOracleTrunc                   ↔ VisitOracleTrunc (FuncCall path)
//	rewriteListagg                       ↔ VisitOracleListagg
//	rewriteNextvalCurrval                ↔ VisitOracleSequenceRef
//	rewriteTranslateUsing                ↔ VisitOracleTranslateUsing
//	rewriteOracleCollectionTokens        ↔ VisitOracleCollectionTokens
//	                                        (no AST equivalent for SDO Spatial)
//	rewriteUserenv                       ↔ VisitOracleUserenv
//	rewriteSysContext                    ↔ VisitOracleSysContext
//	rewriteOracleModOperator             ↔ VisitOracleModOperator (BinaryExpr path)
//	rewriteOracleOuterJoin               ↔ VisitOracleOuterJoin (canonical 2-table)
func rewriteOracleExpr(s string) string {
	if s == "" {
		return s
	}
	s = rewriteOracleStripSysPrefix(s)
	s = rewriteOracleDbmsUtilityFormatCallStack(s)
	s = rewriteOracleQuotedPackageCalls(s)
	s = trimOracleDbmsSqlParseLanguageArg(s)
	s = rewriteOracleCursorAttributes(s)
	s = rewriteOracleRownumLimit(s)
	s = rewriteOracleHashInIdentLiterals(s)
	s = qualifyOracleToCharSingleArg(s)
	s = rewriteOracleNestedAggregates(s)
	s = qualifyOracleDecode(s)
	s = rewriteOracleDictionaryViews(s)
	s = rewriteOracleTrunc(s)
	s = replaceWholeWordCI(s, "SYSDATE", "CURRENT_TIMESTAMP::timestamp(0)")
	s = replaceWholeWordCI(s, "SYSTIMESTAMP", "CURRENT_TIMESTAMP")
	s = replaceWholeWordCI(s, "MINUS", "EXCEPT")
	s = renameFuncCall(s, "NVL", "COALESCE(")
	s = rewriteListagg(s)
	s = rewriteNextvalCurrval(s)
	s = rewriteTranslateUsing(s)
	s = rewriteOracleCollectionTokens(s)
	s = rewriteUserenv(s)
	s = rewriteSysContext(s)
	s = rewriteOracleModOperator(s)
	s = replaceWholeWordCI(s, "$$PLSQL_UNIT_OWNER", "current_user")
	s = replaceWholeWordCI(s, "$$PLSQL_UNIT_TYPE", "'PROCEDURE'")
	s = replaceWholeWordCI(s, "$$PLSQL_UNIT", "'<plsql_unit>'")
	s = replaceWholeWordCI(s, "$$PLSQL_LINE", "0")
	s = rewriteOracleOuterJoin(s)
	return s
}

// rewriteOracleModOperator replaces every `<lhs> MOD <rhs>` infix
// operator with the `mod(<lhs>, <rhs>)` function call. PG plpgsql has no
// `MOD` infix operator (the keyword is parsed as a function name only),
// so an Oracle expression like `IF (x MOD 2) = 0 THEN …` raises a
// `syntax error at or near "MOD"` when applied verbatim.
//
// The rewrite operates token-by-token via the Oracle lexer so it
// honours string literals, comments, and paren nesting. For each `MOD`
// keyword (or upper/lower ident `mod`) found at top level, we walk
// backwards to find the lhs operand (a single ident, parenthesised
// expression, or numeric literal — possibly chained via `.` for
// qualified names) and forwards to find the rhs by the same rule. The
// pair is then re-emitted as `mod(<lhs>, <rhs>)`. Anything that doesn't
// match the expected operand shape is left alone, so a column or
// variable accidentally named `mod` (rare but legal) survives.
func rewriteOracleModOperator(s string) string {
	if s == "" || !containsKeywordCI(s, "MOD") {
		return s
	}
	// Find every whole-word MOD occurrence and try to rewrite.
	var b strings.Builder
	upper := strings.ToUpper(s)
	i := 0
	for i < len(s) {
		j := strings.Index(upper[i:], "MOD")
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		j += i
		// Boundary check.
		before := byte(' ')
		if j > 0 {
			before = s[j-1]
		}
		after := byte(' ')
		if j+3 < len(s) {
			after = s[j+3]
		}
		if isIdentByte(before) || isIdentByte(after) {
			// Not whole-word — skip past `M` and continue.
			b.WriteString(s[i : j+1])
			i = j + 1
			continue
		}
		// Skip if the next non-whitespace byte is `(` — that's the
		// PG-native `mod(x, y)` function-call form, already valid; we
		// only rewrite the Oracle `x MOD y` infix operator.
		next := j + 3
		for next < len(s) && (s[next] == ' ' || s[next] == '\t' || s[next] == '\n' || s[next] == '\r') {
			next++
		}
		if next < len(s) && s[next] == '(' {
			b.WriteString(s[i : j+3])
			i = j + 3
			continue
		}
		// Find LHS operand (walk left, skipping spaces, then capture
		// either a parenthesised expression or a dotted ident /
		// numeric token).
		lhsEnd := j
		// trim trailing whitespace before MOD
		for lhsEnd > 0 && (s[lhsEnd-1] == ' ' || s[lhsEnd-1] == '\t' || s[lhsEnd-1] == '\n' || s[lhsEnd-1] == '\r') {
			lhsEnd--
		}
		if lhsEnd == 0 {
			b.WriteString(s[i : j+3])
			i = j + 3
			continue
		}
		lhsStart := lhsEnd
		if s[lhsEnd-1] == ')' {
			// Walk left to matching `(`.
			depth := 1
			lhsStart = lhsEnd - 1
			for lhsStart > 0 {
				lhsStart--
				if s[lhsStart] == ')' {
					depth++
				} else if s[lhsStart] == '(' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
		} else {
			// Walk left over ident bytes / dots.
			for lhsStart > 0 && (isIdentByte(s[lhsStart-1]) || s[lhsStart-1] == '.') {
				lhsStart--
			}
		}
		if lhsStart >= lhsEnd {
			b.WriteString(s[i : j+3])
			i = j + 3
			continue
		}
		lhs := s[lhsStart:lhsEnd]
		// Find RHS operand (walk right, skipping spaces).
		rhsStart := j + 3
		for rhsStart < len(s) && (s[rhsStart] == ' ' || s[rhsStart] == '\t' || s[rhsStart] == '\n' || s[rhsStart] == '\r') {
			rhsStart++
		}
		if rhsStart >= len(s) {
			b.WriteString(s[i : j+3])
			i = j + 3
			continue
		}
		rhsEnd := rhsStart
		if s[rhsStart] == '(' {
			// Match closing paren.
			depth := 1
			rhsEnd = rhsStart + 1
			for rhsEnd < len(s) && depth > 0 {
				if s[rhsEnd] == '(' {
					depth++
				} else if s[rhsEnd] == ')' {
					depth--
				}
				rhsEnd++
			}
		} else {
			// Walk right over ident bytes / dots.
			for rhsEnd < len(s) && (isIdentByte(s[rhsEnd]) || s[rhsEnd] == '.') {
				rhsEnd++
			}
		}
		if rhsEnd <= rhsStart {
			b.WriteString(s[i : j+3])
			i = j + 3
			continue
		}
		rhs := s[rhsStart:rhsEnd]
		// Emit prefix (everything before lhs), then mod(lhs, rhs).
		b.WriteString(s[i:lhsStart])
		b.WriteString("mod(")
		b.WriteString(strings.TrimSpace(lhs))
		b.WriteString(", ")
		b.WriteString(strings.TrimSpace(rhs))
		b.WriteString(")")
		i = rhsEnd
	}
	return b.String()
}

// rewriteTranslateUsing replaces every TRANSLATE(<expr> USING NCHAR_CS|CHAR_CS)
// with just the inner expression. Replaces the regex
//   (?is)\bTRANSLATE\s*\(\s*([^()]+?)\s+USING\s+(?:NCHAR_CS|CHAR_CS)\s*\).
// Only the single-argument USING form is handled; the 3-arg call
// `TRANSLATE(str, from, to)` exists in PG and stays untouched.
func rewriteTranslateUsing(s string) string {
	const fname = "TRANSLATE"
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		j := skipWS(s, k+len(fname))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		inner := s[j+1 : end]
		// Look for the `USING` keyword at depth 0 in the inner body. The
		// original regex required no nested parens in the expression
		// (`[^()]+?`) — we honour the same shape constraint.
		if strings.ContainsAny(inner, "()") {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		usingOff := findKeywordCI(inner, 0, "USING")
		if usingOff < 0 {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		expr := strings.TrimSpace(inner[:usingOff])
		tail := strings.TrimSpace(inner[usingOff+len("USING"):])
		if !strings.EqualFold(tail, "NCHAR_CS") && !strings.EqualFold(tail, "CHAR_CS") {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		out.WriteString(s[i:k])
		out.WriteString(expr)
		i = end + 1
	}
	return out.String()
}

// rewriteSysContext maps `sys_context('NAMESPACE','PARAMETER')` to
// `current_setting('squishy.<param>', true)`. Replaces the regex
//   (?is)\bsys_context\s*\(\s*'[^']+'\s*,\s*'([^']+)'\s*\).
// rewriteUserenv maps the legacy single-arg `USERENV(<key>)` to
// `current_setting('squishy.<key>', true)`. Oracle deprecated USERENV in 9i in
// favour of `SYS_CONTEXT('USERENV', <key>)`, but mature schemas (notably
// column DEFAULTs persisted by DBMS_METADATA) still emit it. Mirrors the
// shape of `rewriteSysContext` so both functions surface in PG with the same
// `squishy.*` GUC convention.
func rewriteUserenv(s string) string {
	const fname = "userenv"
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		j := skipWS(s, k+len(fname))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		if len(args) != 1 {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		param := strings.TrimSpace(args[0])
		if len(param) < 2 || param[0] != '\'' || param[len(param)-1] != '\'' {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		key := strings.ToLower(strings.TrimSpace(param[1 : len(param)-1]))
		out.WriteString(s[i:k])
		// Map a handful of well-known USERENV keys to native PG equivalents
		// so column DEFAULTs of numeric / text type don't get a type-mismatch
		// error from a generic `current_setting` call. Generic keys still
		// fall back to the GUC convention, but those columns must already
		// be text for the source to compile in Oracle.
		switch key {
		case "sessionid", "session_id":
			// Oracle returns a NUMBER; PG's closest is the backend PID.
			// Cast to numeric so the value fits NUMERIC columns directly.
			out.WriteString("(pg_backend_pid())::numeric")
		case "terminal", "client_info":
			// Both return varchar in Oracle; PG's application_name is the
			// closest analogue.
			out.WriteString("current_setting('application_name', true)")
		default:
			out.WriteString("current_setting('squishy.")
			out.WriteString(key)
			out.WriteString("', true)")
		}
		i = end + 1
	}
	return out.String()
}

// rewriteOracleRownumLimit converts `WHERE ROWNUM <op> <num>` (with
// op ∈ {=, <=, <}) to a PG `LIMIT <num>` clause when ROWNUM is the
// sole filter. Common Oracle idioms it covers:
//
//	WHERE ROWNUM = 1   →  LIMIT 1
//	WHERE ROWNUM <= 10 →  LIMIT 10
//	WHERE ROWNUM < 5   →  LIMIT 4
//
// Quirk: Oracle's ROWNUM is assigned BEFORE row filtering, so
// `WHERE ROWNUM = N` (N>1) never matches anything. We don't try to
// emulate that pathology — `=` is treated as `<=` for translation
// purposes, matching the user's almost-certain intent.
//
// Combined predicates (e.g. `WHERE active = 1 AND ROWNUM <= 10`) and
// uses of ROWNUM in the SELECT list or ORDER BY require ROW_NUMBER()
// rewrites that aren't safe to do at text level — those are left
// untouched and surface as "column rownum does not exist" at compile
// time, prompting manual review.
func rewriteOracleRownumLimit(s string) string {
	if s == "" || !containsKeywordCI(s, "ROWNUM") {
		return s
	}
	var b strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, "WHERE")
		if k < 0 {
			b.WriteString(s[i:])
			break
		}
		// After WHERE: optional ws, ROWNUM, optional ws, op, optional ws, integer literal.
		j := skipWS(s, k+len("WHERE"))
		if !strings.EqualFold(s[j:min(j+len("ROWNUM"), len(s))], "ROWNUM") {
			b.WriteString(s[i : k+len("WHERE")])
			i = k + len("WHERE")
			continue
		}
		// Whole-word check on ROWNUM (no trailing ident byte).
		if j+len("ROWNUM") < len(s) && isIdentByte(s[j+len("ROWNUM")]) {
			b.WriteString(s[i : k+len("WHERE")])
			i = k + len("WHERE")
			continue
		}
		opStart := skipWS(s, j+len("ROWNUM"))
		var opLen int
		switch {
		case opStart+1 < len(s) && s[opStart] == '<' && s[opStart+1] == '=':
			opLen = 2
		case opStart < len(s) && (s[opStart] == '=' || s[opStart] == '<'):
			opLen = 1
		default:
			b.WriteString(s[i : k+len("WHERE")])
			i = k + len("WHERE")
			continue
		}
		op := s[opStart : opStart+opLen]
		numStart := skipWS(s, opStart+opLen)
		numEnd := numStart
		for numEnd < len(s) && s[numEnd] >= '0' && s[numEnd] <= '9' {
			numEnd++
		}
		if numEnd == numStart {
			b.WriteString(s[i : k+len("WHERE")])
			i = k + len("WHERE")
			continue
		}
		// Reject if the next token is AND/OR — combined filters need
		// manual review.
		afterNum := skipWS(s, numEnd)
		next := strings.ToUpper(s[afterNum:min(afterNum+3, len(s))])
		if strings.HasPrefix(next, "AND") &&
			(afterNum+3 == len(s) || !isIdentByte(s[afterNum+3])) {
			b.WriteString(s[i : k+len("WHERE")])
			i = k + len("WHERE")
			continue
		}
		if strings.HasPrefix(next, "OR ") || strings.HasPrefix(next, "OR\t") || strings.HasPrefix(next, "OR\n") {
			b.WriteString(s[i : k+len("WHERE")])
			i = k + len("WHERE")
			continue
		}
		// Compute LIMIT value: `<` cuts one off, `=` and `<=` keep N.
		nStr := s[numStart:numEnd]
		if op == "<" {
			n := 0
			for x := 0; x < len(nStr); x++ {
				n = n*10 + int(nStr[x]-'0')
			}
			if n > 0 {
				n--
			}
			nStr = ""
			if n == 0 {
				nStr = "0"
			} else {
				for n > 0 {
					nStr = string(rune('0'+(n%10))) + nStr
					n /= 10
				}
			}
		}
		// Emit prefix, then LIMIT, then everything after the number.
		b.WriteString(s[i:k])
		b.WriteString("LIMIT ")
		b.WriteString(nStr)
		i = numEnd
	}
	return b.String()
}

// rewriteOracleHashInIdentLiterals replaces `#` with `_` inside single-
// quoted string literals when the `#` appears between identifier-class
// bytes (alphanumeric or underscore). Free-text `#` (e.g. " #1 priority")
// is left alone. This sidesteps PG's rejection of `#` in unquoted
// identifiers when procedures build DDL via string concatenation.
//
// Tradeoff: tables originally named `MVT#ABT` in Oracle migrate to PG
// as `mvt#abt` (quoted). After this rewrite the procedure body will
// reference `mvt_abt`. The user must either rename existing tables
// (`ALTER TABLE … RENAME TO …`) or re-run squishy with a similar
// `# → _` rule on data-migration identifiers.
func rewriteOracleHashInIdentLiterals(s string) string {
	if !strings.ContainsRune(s, '#') {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		// Single-quoted string literal — scan and apply per-byte rule.
		if c == '\'' {
			b.WriteByte(c)
			i++
			for i < len(s) {
				if s[i] == '\'' {
					// Possible '' escape — copy both bytes verbatim.
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteByte('\'')
						b.WriteByte('\'')
						i += 2
						continue
					}
					b.WriteByte('\'')
					i++
					break
				}
				if s[i] == '#' && hashIsBetweenIdentBytes(s, i) {
					b.WriteByte('_')
					i++
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			continue
		}
		// Skip past SQL comments via the shared helper.
		if next := skipSQLComment(s, i); next != i {
			b.WriteString(s[i:next])
			i = next
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// hashIsBetweenIdentBytes returns true when src[at] == '#' looks like
// part of an identifier rather than free text. The rule: both adjacent
// bytes must be non-whitespace, AND at least one must be
// identifier-class (alphanumeric or another `#`). This catches
// `'MVT#'` (T then closing quote), `'#NAME'`, `'A#B'`, `'IDX#MVT#'`
// while leaving `'priority #1 task'` (space before #) untouched.
func hashIsBetweenIdentBytes(src string, at int) bool {
	prev := byte(0)
	if at > 0 {
		prev = src[at-1]
	}
	next := byte(0)
	if at+1 < len(src) {
		next = src[at+1]
	}
	if isWhitespaceByte(prev) || isWhitespaceByte(next) {
		return false
	}
	prevIdent := isIdentByte(prev) || prev == '#'
	nextIdent := isIdentByte(next) || next == '#'
	return prevIdent || nextIdent
}

func isWhitespaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// qualifyOracleToCharSingleArg prefixes every single-argument `to_char(x)`
// call with the orafce `oracle.` schema. PG's stock `pg_catalog.to_char`
// has 2-arg overloads only; orafce supplies the single-arg shape that
// Oracle code routinely uses (e.g. `TO_CHAR(DATA_PRECISION)` to render
// a number with the default Oracle format). 2-arg calls (with explicit
// format mask) match pg_catalog directly so they're left bare.
//
// Calls already schema-qualified — `oracle.to_char(...)`, `mig.to_char(...)`
// — are skipped. String literals and SQL comments are honoured via the
// shared findKeywordCI lexer.
func qualifyOracleToCharSingleArg(s string) string {
	if s == "" || !containsKeywordCI(s, "to_char") {
		return s
	}
	const fname = "to_char"
	var b strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			b.WriteString(s[i:])
			break
		}
		// Must be followed by `(` to be a call.
		j := k + len(fname)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		// Skip if already schema-qualified (preceded by `.` allowing ws).
		p := k
		for p > 0 && (s[p-1] == ' ' || s[p-1] == '\t') {
			p--
		}
		if p > 0 && s[p-1] == '.' {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		if len(args) != 1 {
			// Multi-arg call — pg_catalog handles it natively.
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		b.WriteString(s[i:k])
		b.WriteString("oracle.to_char")
		i = k + len(fname)
	}
	return b.String()
}

// rewriteOracleNestedAggregates lifts every `<OUTER_AGG>(<INNER_AGG>(arg))`
// expression that Oracle accepts directly (because Oracle interprets it
// as "aggregate over the per-group results when GROUP BY is present")
// into a PG-legal derived-table form. PG raises "aggregate function
// calls cannot be nested" otherwise.
//
// Transformation (single-column SELECT only):
//
//	SELECT <O>(<I>(arg)) [rest: FROM … [WHERE …] GROUP BY … [HAVING …]]
//	→
//	SELECT <O>(_nag) FROM (SELECT <I>(arg) AS _nag [rest]) _nag_t
//
// We only fire when:
//   - both <O> and <I> are recognised aggregate names (MIN/MAX/SUM/AVG/COUNT)
//   - the SELECT list contains exactly the nested-aggregate expression
//     (no other columns) — multi-column SELECTs need manual review since
//     non-aggregate cols would have to move inside the inner SELECT
//   - a `FROM` keyword follows at top level
//
// The SELECT's end is found by walking forward paren-aware until either
// we exit the enclosing parenthesis (depth -1) or hit a top-level `;`.
// String literals and SQL comments are skipped via the shared lexer
// helpers used elsewhere in this file.
func rewriteOracleNestedAggregates(s string) string {
	if s == "" {
		return s
	}
	const sentinel = "(" // hot-path: any nested call has at least two `(`s
	if !strings.Contains(s, sentinel) {
		return s
	}
	aggs := map[string]bool{
		"MIN": true, "MAX": true, "SUM": true, "AVG": true, "COUNT": true,
	}
	var b strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, "SELECT")
		if k < 0 {
			b.WriteString(s[i:])
			break
		}
		// Read the SELECT list head: skip whitespace, optional DISTINCT.
		listStart := skipWS(s, k+len("SELECT"))
		if listStart < len(s) && strings.EqualFold(s[listStart:min(listStart+8, len(s))], "DISTINCT") {
			listStart = skipWS(s, listStart+len("DISTINCT"))
		}
		// Outer aggregate name.
		outerEnd := listStart
		for outerEnd < len(s) && isIdentByte(s[outerEnd]) {
			outerEnd++
		}
		outerName := strings.ToUpper(s[listStart:outerEnd])
		if !aggs[outerName] {
			b.WriteString(s[i : k+len("SELECT")])
			i = k + len("SELECT")
			continue
		}
		// Outer `(`.
		op := skipWS(s, outerEnd)
		if op >= len(s) || s[op] != '(' {
			b.WriteString(s[i : k+len("SELECT")])
			i = k + len("SELECT")
			continue
		}
		// Inner aggregate name.
		innerStart := skipWS(s, op+1)
		innerEnd := innerStart
		for innerEnd < len(s) && isIdentByte(s[innerEnd]) {
			innerEnd++
		}
		innerName := strings.ToUpper(s[innerStart:innerEnd])
		if !aggs[innerName] {
			b.WriteString(s[i : k+len("SELECT")])
			i = k + len("SELECT")
			continue
		}
		ip := skipWS(s, innerEnd)
		if ip >= len(s) || s[ip] != '(' {
			b.WriteString(s[i : k+len("SELECT")])
			i = k + len("SELECT")
			continue
		}
		innerArgEnd, ok := findMatchingParen(s, ip)
		if !ok {
			b.WriteString(s[i : k+len("SELECT")])
			i = k + len("SELECT")
			continue
		}
		outerCloseExpect := skipWS(s, innerArgEnd+1)
		if outerCloseExpect >= len(s) || s[outerCloseExpect] != ')' {
			// Outer aggregate has more arguments after the inner —
			// not the simple shape we handle.
			b.WriteString(s[i : k+len("SELECT")])
			i = k + len("SELECT")
			continue
		}
		// Confirm the SELECT list ends here (single-column SELECT).
		afterOuter := skipWS(s, outerCloseExpect+1)
		if afterOuter >= len(s) || !strings.EqualFold(s[afterOuter:min(afterOuter+4, len(s))], "FROM") ||
			(afterOuter+4 < len(s) && isIdentByte(s[afterOuter+4])) {
			b.WriteString(s[i : k+len("SELECT")])
			i = k + len("SELECT")
			continue
		}
		// Find the end of this SELECT: walk paren-aware.
		end := findSelectScopeEnd(s, afterOuter)
		// Build the rewrite.
		innerArg := s[ip+1 : innerArgEnd]
		rest := s[afterOuter:end]
		b.WriteString(s[i:k])
		fmt.Fprintf(&b, "SELECT %s(_nag) FROM (SELECT %s(%s) AS _nag %s) _nag_t",
			strings.ToLower(outerName), strings.ToLower(innerName), innerArg, rest)
		i = end
	}
	return b.String()
}

// findSelectScopeEnd locates the end of a SELECT statement starting at off.
// "End" is the position of the first byte that closes the enclosing
// scope: a `)` at depth -1, a top-level `;`, or end-of-input. Tracks
// paren depth, single-quoted strings, and SQL comments.
func findSelectScopeEnd(s string, off int) int {
	depth := 0
	inStr := false
	j := off
	for j < len(s) {
		c := s[j]
		if inStr {
			if c == '\'' {
				if j+1 < len(s) && s[j+1] == '\'' {
					j += 2
					continue
				}
				inStr = false
			}
			j++
			continue
		}
		if next := skipSQLComment(s, j); next != j {
			j = next
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return j
			}
			depth--
		case ';':
			if depth == 0 {
				return j
			}
		}
		j++
	}
	return j
}

// rewriteOracleQuotedPackageCalls strips the double-quoting from
// `"PKG"."METHOD"` references where PKG names a known orafce-shipped
// package. Output is the lowercase, unquoted form `pkg.method` so PG
// resolves the call against orafce. Other quoted identifiers are
// preserved (the original sources may legitimately use mixed-case
// quoted names for application objects).
func rewriteOracleQuotedPackageCalls(s string) string {
	if s == "" || !strings.Contains(s, `"`) {
		return s
	}
	// Set of orafce-shipped package schemas. Add new entries when more
	// packages are wrapped on the target.
	pkgs := map[string]bool{
		"DBMS_SQL":     true,
		"DBMS_OUTPUT":  true,
		"DBMS_UTILITY": true,
		"DBMS_RANDOM":  true,
		"DBMS_LOCK":    true,
		"DBMS_PIPE":    true,
		"DBMS_ALERT":   true,
		"DBMS_ASSERT":  true,
		"UTL_FILE":     true,
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		// Skip single-quoted string literals (Oracle escape: '').
		if c == '\'' {
			b.WriteByte(c)
			i++
			for i < len(s) {
				if s[i] == '\'' {
					b.WriteByte(s[i])
					i++
					if i < len(s) && s[i] == '\'' {
						b.WriteByte(s[i])
						i++
						continue
					}
					break
				}
				b.WriteByte(s[i])
				i++
			}
			continue
		}
		// Skip past SQL line/block comments via the shared helper.
		if next := skipSQLComment(s, i); next != i {
			b.WriteString(s[i:next])
			i = next
			continue
		}
		if c != '"' {
			b.WriteByte(c)
			i++
			continue
		}
		// Read a quoted identifier.
		end := i + 1
		for end < len(s) && s[end] != '"' {
			end++
		}
		if end >= len(s) {
			b.WriteString(s[i:])
			break
		}
		ident := s[i+1 : end]
		if !pkgs[strings.ToUpper(ident)] {
			// Not a recognised package — pass the quoted run through.
			b.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		// Confirmed package qualifier. Emit lowercase unquoted.
		b.WriteString(strings.ToLower(ident))
		i = end + 1
		// Optionally consume a following `."<METHOD>"` and lowercase it
		// too — that's the typical shape DBMS_METADATA emits.
		if i < len(s) && s[i] == '.' && i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('.')
			mStart := i + 2
			mEnd := mStart
			for mEnd < len(s) && s[mEnd] != '"' {
				mEnd++
			}
			if mEnd < len(s) {
				b.WriteString(strings.ToLower(s[mStart:mEnd]))
				i = mEnd + 1
			}
		}
	}
	return b.String()
}

// trimOracleDbmsSqlParseLanguageArg drops the third argument (Oracle's
// language-flag, typically `DBMS_SQL.NATIVE`) from every
// `dbms_sql.parse(cur, stmt, lang)` call. orafce's signature is
// `dbms_sql.parse(c integer, stmt varchar2)` — passing a third arg
// raises "function dbms_sql.parse(integer, …, …) does not exist" and
// the Oracle-style language flag has no PG meaning.
func trimOracleDbmsSqlParseLanguageArg(s string) string {
	if s == "" || !containsKeywordCI(s, "dbms_sql") {
		return s
	}
	const fname = "parse"
	var b strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			b.WriteString(s[i:])
			break
		}
		// Confirm the preceding qualifier is `dbms_sql.` (allowing
		// whitespace around the dot).
		p := k
		for p > 0 && (s[p-1] == ' ' || s[p-1] == '\t') {
			p--
		}
		if p == 0 || s[p-1] != '.' {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		p--
		for p > 0 && (s[p-1] == ' ' || s[p-1] == '\t') {
			p--
		}
		const pkg = "DBMS_SQL"
		if p < len(pkg) || !strings.EqualFold(s[p-len(pkg):p], pkg) {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		// Boundary check: byte before DBMS_SQL must be non-ident.
		if p-len(pkg) > 0 && isIdentByte(s[p-len(pkg)-1]) {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		// Locate the opening `(`.
		j := k + len(fname)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			b.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		if len(args) <= 2 {
			b.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		// Emit the prefix unchanged, then the call with only the first
		// two args, then the closing paren.
		b.WriteString(s[i : j+1])
		b.WriteString(strings.TrimSpace(args[0]))
		b.WriteString(", ")
		b.WriteString(strings.TrimSpace(args[1]))
		b.WriteString(")")
		i = end + 1
	}
	return b.String()
}

// rewriteOracleCursorAttributes maps Oracle cursor-attribute postfixes
// onto their PG plpgsql equivalents:
//
//	<cur>%NOTFOUND  → NOT FOUND
//	<cur>%FOUND     → FOUND
//	<cur>%ISOPEN    → EXISTS(SELECT 1 FROM pg_cursors WHERE name = <cur>::text)
//	<cur>%ROWCOUNT  → 0 /* TODO: GET DIAGNOSTICS rc = ROW_COUNT */
//
// PG's FOUND / NOT FOUND are special booleans set by the most recent
// FETCH or `EXECUTE … INTO`; they don't carry a cursor name. The mapping
// preserves the per-routine semantic Oracle code expects when only one
// cursor is active at a time (the dominant pattern in PRC_*-style
// procedures). Multi-cursor routines that read attributes between FETCHes
// of different cursors will need manual review.
//
// %ROWTYPE and %TYPE are PG-valid postfix keywords for type anchoring
// — those are deliberately NOT in the keyword set so they pass through.
func rewriteOracleCursorAttributes(s string) string {
	if s == "" || !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		idx := strings.IndexByte(s[i:], '%')
		if idx < 0 {
			b.WriteString(s[i:])
			break
		}
		idx += i
		// Walk back over the preceding identifier (may be dotted, e.g.
		// `pkg.cur%NOTFOUND`).
		idEnd := idx
		idStart := idx
		for idStart > 0 {
			c := s[idStart-1]
			if isIdentByte(c) || c == '.' || c == '"' {
				idStart--
				continue
			}
			break
		}
		// Read the keyword after %.
		j := idx + 1
		kwStart := j
		for j < len(s) && isIdentByte(s[j]) {
			j++
		}
		kw := strings.ToUpper(s[kwStart:j])
		// Pass-through for %TYPE / %ROWTYPE / unknown attributes.
		if kw != "NOTFOUND" && kw != "FOUND" && kw != "ISOPEN" && kw != "ROWCOUNT" {
			b.WriteString(s[i : idx+1])
			i = idx + 1
			continue
		}
		// Reject if the LHS is empty (e.g. `%NOTFOUND` with nothing
		// before — invalid Oracle anyway, but defensive).
		if idStart >= idEnd {
			b.WriteString(s[i : idx+1])
			i = idx + 1
			continue
		}
		cur := s[idStart:idEnd]
		// Emit the prefix up to the start of the cursor identifier.
		b.WriteString(s[i:idStart])
		switch kw {
		case "NOTFOUND":
			b.WriteString("NOT FOUND")
		case "FOUND":
			b.WriteString("FOUND")
		case "ISOPEN":
			b.WriteString("EXISTS (SELECT 1 FROM pg_cursors WHERE name = ")
			b.WriteString(cur)
			b.WriteString("::text)")
		case "ROWCOUNT":
			b.WriteString("0 /* TODO: replace with `GET DIAGNOSTICS x = ROW_COUNT` after the FETCH/EXECUTE referencing ")
			b.WriteString(cur)
			b.WriteString(" */")
		}
		i = j
	}
	return b.String()
}

// qualifyOracleDecode expands every Oracle `DECODE(expr, k1, v1[, k2,
// v2, …][, default])` call into a PG `CASE` expression. orafce ships
// only a handful of polymorphic overloads (3..7 args), so calls with
// more than three search/result pairs (the common metadata-introspection
// shape with type matrices) raise "function oracle.decode(…) does not
// exist". Hand-rolling a CASE has two further benefits: it's
// independent of orafce, and `IS NOT DISTINCT FROM` faithfully
// reproduces Oracle's "two NULLs match" semantics — the same rule
// orafce implements internally.
//
// Form:
//
//	DECODE(e, k1, v1, k2, v2, ..., kN, vN, default)   (odd  arg count)
//	DECODE(e, k1, v1, k2, v2, ..., kN, vN)            (even arg count)
//	→
//	(CASE
//	   WHEN e IS NOT DISTINCT FROM k1 THEN v1
//	   WHEN e IS NOT DISTINCT FROM k2 THEN v2 ...
//	   [ELSE default]
//	 END)
//
// The pass is paren-aware so nested `DECODE` / sub-SELECTs in arguments
// keep their grouping. Calls already schema-qualified to a non-default
// schema (`mig.decode`, `myapp.decode`, …) are left alone — only bare
// decode and `oracle.decode` (orafce) are rewritten.
func qualifyOracleDecode(s string) string {
	if s == "" || !containsKeywordCI(s, "decode") {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		k := findKeywordCI(s, i, "decode")
		if k < 0 {
			b.WriteString(s[i:])
			break
		}
		// Determine where the function-name region starts (so we can
		// drop a leading `oracle.` qualifier when present and rewrite
		// the whole expression). Walk back over whitespace + dot +
		// schema name.
		nameStart := k
		p := k
		for p > 0 && (s[p-1] == ' ' || s[p-1] == '\t') {
			p--
		}
		if p > 0 && s[p-1] == '.' {
			schemaEnd := p - 1
			schemaStart := schemaEnd
			for schemaStart > 0 && isIdentByte(s[schemaStart-1]) {
				schemaStart--
			}
			schema := strings.ToLower(s[schemaStart:schemaEnd])
			if schema == "oracle" {
				// Absorb the `oracle.` qualifier so we replace the
				// whole reference with our CASE.
				nameStart = schemaStart
			} else if schema != "" {
				// User-namespaced decode (mig.decode, etc.) — leave alone.
				b.WriteString(s[i : k+len("decode")])
				i = k + len("decode")
				continue
			}
		}
		// Must be followed (after optional whitespace) by `(` to be a call.
		j := k + len("decode")
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			b.WriteString(s[i : k+len("decode")])
			i = k + len("decode")
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			b.WriteString(s[i : k+len("decode")])
			i = k + len("decode")
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		if len(args) < 3 {
			// Not enough args to be a real DECODE — pass through.
			b.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		// Build the CASE. Recurse into each argument so nested
		// DECODE(...) calls get expanded too — without this, only the
		// outermost call would be rewritten and PG would still see the
		// inner literal `decode(...)`.
		expr := qualifyOracleDecode(strings.TrimSpace(args[0]))
		var caseB strings.Builder
		caseB.WriteString("(CASE")
		idx := 1
		for idx+1 < len(args) {
			search := qualifyOracleDecode(strings.TrimSpace(args[idx]))
			result := qualifyOracleDecode(strings.TrimSpace(args[idx+1]))
			fmt.Fprintf(&caseB, " WHEN %s IS NOT DISTINCT FROM %s THEN %s", expr, search, result)
			idx += 2
		}
		if idx < len(args) {
			fmt.Fprintf(&caseB, " ELSE %s", qualifyOracleDecode(strings.TrimSpace(args[idx])))
		}
		caseB.WriteString(" END)")
		b.WriteString(s[i:nameStart])
		b.WriteString(caseB.String())
		i = end + 1
	}
	return b.String()
}

// rewriteOracleDictionaryViews replaces every bare reference to a
// supported Oracle data-dictionary view with an inline sub-SELECT that
// reads from pg_catalog directly. orafce only ships the USER_* family
// (current-user scope, no OWNER column) under schema `oracle`, which
// isn't enough for code that filters on OWNER. Rather than provisioning
// a compat view on the target, we expand the reference in-place so the
// emitted SQL is self-contained.
//
// The expansion strategy:
//
//   - For each known dictionary-view name (ALL_OBJECTS, ALL_TABLES, …),
//     find every whole-word, case-insensitive occurrence not preceded
//     by a dot (i.e. not already schema-qualified).
//   - Substitute the matched name with the appropriate parenthesised
//     sub-SELECT.
//   - Decide the trailing alias: if the next non-whitespace token after
//     the original name looks like a user-supplied table alias (a bare
//     identifier that isn't a SQL keyword), leave the substitution
//     un-aliased and let the user's alias bind to the sub-SELECT.
//     Otherwise emit `AS <view_lower>` so PG accepts the derived table
//     in `FROM (subquery)` contexts.
func rewriteOracleDictionaryViews(s string) string {
	if s == "" {
		return s
	}
	out := s
	for _, name := range dictViewOrder {
		out = expandDictView(out, name, dictViewSubquery[name])
	}
	return out
}

// dictViewOrder fixes the iteration order so longer names match before
// any shorter prefix-overlapping name (e.g. ALL_IND_COLUMNS before
// ALL_INDEXES, ALL_TAB_COLUMNS before ALL_TABLES).
var dictViewOrder = []string{
	"ALL_TAB_COLUMNS",
	"ALL_IND_COLUMNS",
	"ALL_COL_COMMENTS",
	"ALL_TAB_COMMENTS",
	"ALL_INDEXES",
	"ALL_OBJECTS",
	"ALL_TABLES",
}

// dictViewSubquery maps each supported view to the PG sub-SELECT that
// reproduces its observable shape (the columns Oracle code typically
// filters on: OWNER, OBJECT_NAME, OBJECT_TYPE, STATUS for ALL_OBJECTS;
// OWNER, TABLE_NAME, TABLESPACE_NAME, NUM_ROWS, … for ALL_TABLES). All
// names are uppercased to match Oracle's case-folded data-dictionary
// columns; callers compare with literals like 'PKG_ITV', 'TABLE'.
var dictViewSubquery = map[string]string{
	"ALL_OBJECTS": `(SELECT upper(n.nspname)::text AS owner, ` +
		`upper(c.relname)::text AS object_name, ` +
		`NULL::text AS subobject_name, ` +
		`c.oid AS object_id, NULL::oid AS data_object_id, ` +
		`(CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'p' THEN 'TABLE' ` +
		`WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ` +
		`WHEN 'i' THEN 'INDEX' WHEN 'I' THEN 'INDEX' ` +
		`WHEN 'S' THEN 'SEQUENCE' WHEN 'f' THEN 'FOREIGN TABLE' ` +
		`WHEN 'c' THEN 'TYPE' END)::text AS object_type, ` +
		`NULL::timestamp AS created, NULL::timestamp AS last_ddl_time, ` +
		`'VALID'::text AS status ` +
		`FROM pg_catalog.pg_class c ` +
		`JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace ` +
		`WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast') ` +
		`AND c.relkind IN ('r','p','v','m','i','I','S','f','c') ` +
		`UNION ALL ` +
		`SELECT upper(n.nspname)::text, upper(p.proname)::text, NULL::text, ` +
		`p.oid, NULL::oid, ` +
		`(CASE p.prokind WHEN 'p' THEN 'PROCEDURE' WHEN 'f' THEN 'FUNCTION' ` +
		`WHEN 'a' THEN 'FUNCTION' WHEN 'w' THEN 'FUNCTION' END)::text, ` +
		`NULL::timestamp, NULL::timestamp, 'VALID'::text ` +
		`FROM pg_catalog.pg_proc p ` +
		`JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace ` +
		`WHERE n.nspname NOT IN ('pg_catalog','information_schema'))`,

	"ALL_TABLES": `(SELECT upper(n.nspname)::text AS owner, ` +
		`upper(c.relname)::text AS table_name, ` +
		`upper(t.spcname)::text AS tablespace_name, ` +
		`'VALID'::text AS status, ` +
		`c.reltuples::numeric AS num_rows, ` +
		`c.relpages::numeric AS blocks, ` +
		`'NO'::text AS partitioned, ` +
		`'N'::text AS temporary, 'N'::text AS secondary ` +
		`FROM pg_catalog.pg_class c ` +
		`JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace ` +
		`LEFT JOIN pg_catalog.pg_tablespace t ON t.oid = c.reltablespace ` +
		`WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast') ` +
		`AND c.relkind IN ('r','p'))`,

	// ALL_TAB_COLUMNS — column metadata for every table/view the role
	// can see. data_type is mapped from PG's pg_type.typname back to
	// the Oracle name so callers that filter on 'BLOB', 'NUMBER',
	// 'VARCHAR2', etc. still match. Mappings cover the common cases;
	// uncommon PG types fall through as the uppercased PG type name
	// (which any Oracle filter on `DATA_TYPE = 'foo'` will simply miss).
	"ALL_TAB_COLUMNS": `(SELECT upper(n.nspname)::text AS owner, ` +
		`upper(c.relname)::text AS table_name, ` +
		`upper(a.attname)::text AS column_name, ` +
		`(CASE ` +
		`WHEN t.typname = 'bytea' THEN 'BLOB' ` +
		`WHEN t.typname IN ('text','citext') THEN 'CLOB' ` +
		`WHEN t.typname IN ('varchar','character varying','bpchar','character') THEN 'VARCHAR2' ` +
		`WHEN t.typname = 'numeric' THEN 'NUMBER' ` +
		`WHEN t.typname IN ('int2','int4','int8') THEN 'NUMBER' ` +
		`WHEN t.typname = 'float8' THEN 'BINARY_DOUBLE' ` +
		`WHEN t.typname = 'float4' THEN 'BINARY_FLOAT' ` +
		`WHEN t.typname = 'date' THEN 'DATE' ` +
		`WHEN t.typname IN ('timestamp','timestamptz') THEN 'TIMESTAMP' ` +
		`WHEN t.typname = 'geometry' THEN 'SDO_GEOMETRY' ` +
		`ELSE upper(t.typname) END)::text AS data_type, ` +
		`COALESCE(a.atttypmod - 4, a.attlen)::numeric AS data_length, ` +
		`information_schema._pg_numeric_precision(a.atttypid, a.atttypmod)::numeric AS data_precision, ` +
		`information_schema._pg_numeric_scale(a.atttypid, a.atttypmod)::numeric AS data_scale, ` +
		`(CASE WHEN a.attnotnull THEN 'N' ELSE 'Y' END)::text AS nullable, ` +
		`a.attnum::numeric AS column_id ` +
		`FROM pg_catalog.pg_class c ` +
		`JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace ` +
		`JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid ` +
		`JOIN pg_catalog.pg_type t ON t.oid = a.atttypid ` +
		`WHERE a.attnum > 0 AND NOT a.attisdropped ` +
		`AND c.relkind IN ('r','p','v','m','f') ` +
		`AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast'))`,

	// ALL_INDEXES — every index visible to the role. UNIQUENESS is
	// 'UNIQUE' / 'NONUNIQUE' to match Oracle's literal values.
	"ALL_INDEXES": `(SELECT upper(idn.nspname)::text AS owner, ` +
		`upper(ic.relname)::text AS index_name, ` +
		`upper(tn.nspname)::text AS table_owner, ` +
		`upper(tc.relname)::text AS table_name, ` +
		`(CASE WHEN i.indisunique THEN 'UNIQUE' ELSE 'NONUNIQUE' END)::text AS uniqueness, ` +
		`'NORMAL'::text AS index_type, ` +
		`'VALID'::text AS status ` +
		`FROM pg_catalog.pg_index i ` +
		`JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid ` +
		`JOIN pg_catalog.pg_namespace idn ON idn.oid = ic.relnamespace ` +
		`JOIN pg_catalog.pg_class tc ON tc.oid = i.indrelid ` +
		`JOIN pg_catalog.pg_namespace tn ON tn.oid = tc.relnamespace ` +
		`WHERE tn.nspname NOT IN ('pg_catalog','information_schema','pg_toast'))`,

	// ALL_COL_COMMENTS — column-level comments. PG stores them in
	// pg_description, keyed by (objoid=relation oid, objsubid=attnum).
	"ALL_COL_COMMENTS": `(SELECT upper(n.nspname)::text AS owner, ` +
		`upper(c.relname)::text AS table_name, ` +
		`upper(a.attname)::text AS column_name, ` +
		`d.description::text AS comments ` +
		`FROM pg_catalog.pg_class c ` +
		`JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace ` +
		`JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid ` +
		`LEFT JOIN pg_catalog.pg_description d ON d.objoid = c.oid ` +
		`AND d.classoid = 'pg_catalog.pg_class'::regclass AND d.objsubid = a.attnum ` +
		`WHERE a.attnum > 0 AND NOT a.attisdropped ` +
		`AND c.relkind IN ('r','p','v','m','f') ` +
		`AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast'))`,

	// ALL_TAB_COMMENTS — table-level comments. Same source, but
	// objsubid = 0 means "the relation itself, not a specific column".
	"ALL_TAB_COMMENTS": `(SELECT upper(n.nspname)::text AS owner, ` +
		`upper(c.relname)::text AS table_name, ` +
		`(CASE c.relkind ` +
		`WHEN 'r' THEN 'TABLE' WHEN 'p' THEN 'TABLE' ` +
		`WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ` +
		`WHEN 'f' THEN 'FOREIGN TABLE' END)::text AS table_type, ` +
		`d.description::text AS comments ` +
		`FROM pg_catalog.pg_class c ` +
		`JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace ` +
		`LEFT JOIN pg_catalog.pg_description d ON d.objoid = c.oid ` +
		`AND d.classoid = 'pg_catalog.pg_class'::regclass AND d.objsubid = 0 ` +
		`WHERE c.relkind IN ('r','p','v','m','f') ` +
		`AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast'))`,

	// ALL_IND_COLUMNS — index→column mapping with positional ordering.
	// Uses unnest(indkey) WITH ORDINALITY to expose Oracle's
	// COLUMN_POSITION; expressional indexes (where attnum is 0) are
	// skipped because they have no column_name in the Oracle sense.
	"ALL_IND_COLUMNS": `(SELECT upper(idn.nspname)::text AS index_owner, ` +
		`upper(ic.relname)::text AS index_name, ` +
		`upper(tn.nspname)::text AS table_owner, ` +
		`upper(tc.relname)::text AS table_name, ` +
		`upper(a.attname)::text AS column_name, ` +
		`col.ord::numeric AS column_position ` +
		`FROM pg_catalog.pg_index i ` +
		`JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid ` +
		`JOIN pg_catalog.pg_namespace idn ON idn.oid = ic.relnamespace ` +
		`JOIN pg_catalog.pg_class tc ON tc.oid = i.indrelid ` +
		`JOIN pg_catalog.pg_namespace tn ON tn.oid = tc.relnamespace ` +
		`CROSS JOIN LATERAL unnest(i.indkey::int[]) WITH ORDINALITY AS col(attnum, ord) ` +
		`JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = col.attnum ` +
		`WHERE col.attnum > 0 ` +
		`AND tn.nspname NOT IN ('pg_catalog','information_schema','pg_toast'))`,
}

// expandDictView replaces every whole-word, case-insensitive occurrence
// of view in src with subquery, choosing whether to append `AS <view>`
// based on what follows the original name. References already
// schema-qualified (preceded by `.`) are skipped.
func expandDictView(src, view, subquery string) string {
	if !containsKeywordCI(src, view) {
		return src
	}
	lower := strings.ToLower(view)
	var b strings.Builder
	i := 0
	for {
		k := findKeywordCI(src, i, view)
		if k < 0 {
			b.WriteString(src[i:])
			break
		}
		// Skip if already schema-qualified (e.g. `mig.all_objects`).
		j := k
		for j > 0 && (src[j-1] == ' ' || src[j-1] == '\t' || src[j-1] == '\n' || src[j-1] == '\r') {
			j--
		}
		if j > 0 && src[j-1] == '.' {
			b.WriteString(src[i : k+len(view)])
			i = k + len(view)
			continue
		}
		// Skip if the view name is being used as a derived-table alias
		// — i.e. it sits right after a `)` (with optional `AS` keyword).
		// Without this guard, a second pass over already-rewritten body
		// text would re-expand the trailing `AS all_tab_columns` alias
		// we emitted on the first pass and produce a malformed
		// `(sub1) AS (sub2) AS all_tab_columns` shape.
		if j >= 2 && strings.EqualFold(src[j-2:j], "AS") &&
			(j == 2 || !isIdentByte(src[j-3])) {
			b.WriteString(src[i : k+len(view)])
			i = k + len(view)
			continue
		}
		if j > 0 && src[j-1] == ')' {
			b.WriteString(src[i : k+len(view)])
			i = k + len(view)
			continue
		}
		b.WriteString(src[i:k])
		b.WriteString(subquery)
		// Decide whether to add an alias: PG requires an alias for any
		// derived table in FROM, except when the user already supplied
		// one (e.g. `FROM ALL_TABLES A`).
		end := k + len(view)
		if !nextLooksLikeAlias(src, end) {
			b.WriteString(" AS ")
			b.WriteString(lower)
		}
		i = end
	}
	return b.String()
}

// nextLooksLikeAlias reports whether the bytes immediately following
// position `at` in src look like a user-supplied table alias. An alias
// is a bare identifier — letter or underscore start, alphanumerics
// thereafter — that is NOT one of the SQL clause keywords that signal
// "no alias here, the FROM target ends" (WHERE, ON, USING, JOIN, …).
//
// Used by expandDictView to decide between two emission shapes:
//
//	FROM ALL_TABLES A ON …  → FROM (subquery) A ON …      (no alias added)
//	FROM ALL_OBJECTS WHERE … → FROM (subquery) AS all_objects WHERE …
func nextLooksLikeAlias(src string, at int) bool {
	j := at
	for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
		j++
	}
	if j >= len(src) {
		return false
	}
	c := src[j]
	// Quoted identifier counts as an alias.
	if c == '"' {
		return true
	}
	if !isIdentStartByte(c) {
		return false
	}
	end := j
	for end < len(src) && isIdentByte(src[end]) {
		end++
	}
	tok := strings.ToUpper(src[j:end])
	switch tok {
	case "WHERE", "ON", "USING", "JOIN", "INNER", "LEFT", "RIGHT",
		"FULL", "OUTER", "CROSS", "NATURAL",
		"GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH",
		"FOR", "AND", "OR", "NOT",
		"UNION", "INTERSECT", "EXCEPT", "MINUS",
		"INTO", "RETURNING", "WITH", "AS":
		return false
	}
	return true
}

// rewriteOracleStripSysPrefix removes a leading `SYS.` from any dotted
// identifier sequence (e.g. `SYS.DBMS_UTILITY.format_call_stack`,
// `SYS.ALL_OBJECTS`, `SYS.STANDARD.TO_NUMBER`). PG has no `sys` schema,
// so the dotted form is parsed as a multi-part column reference and
// raises "missing FROM-clause entry for table …" or "schema sys does
// not exist". Stripping the prefix lets the remaining identifier resolve
// against the search_path (orafce schemas, application views, etc.).
//
// Match conditions (all required to fire):
//   - The `SYS` token is whole-word (preceded by start-of-string or a
//     non-ident byte; followed by a `.` after optional whitespace).
//   - The byte after the `.` (skipping whitespace) is an identifier
//     start byte, so we don't strip a bare `SYS` that doesn't qualify
//     anything (defensive — Oracle sometimes uses `SYS` as a column
//     value in literal contexts).
//
// String literals and SQL comments are honoured via findKeywordCI.
func rewriteOracleStripSysPrefix(s string) string {
	if s == "" {
		return s
	}
	if !containsKeywordCI(s, "SYS") {
		return s
	}
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, "SYS")
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		// After SYS, expect optional whitespace, then `.`, then optional
		// whitespace, then an identifier-start byte.
		j := k + 3
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) || s[j] != '.' {
			out.WriteString(s[i : k+3])
			i = k + 3
			continue
		}
		j++
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) || !isIdentStartByte(s[j]) {
			out.WriteString(s[i : k+3])
			i = k + 3
			continue
		}
		// Drop everything from the start of `SYS` through the `.` (and
		// the whitespace around it) — i.e. emit s[i:k] then resume at j.
		out.WriteString(s[i:k])
		i = j
	}
	return out.String()
}

// rewriteOracleDbmsUtilityFormatCallStack rewrites every reference to
// the Oracle `DBMS_UTILITY.FORMAT_*` family to its closest PG-native
// equivalent. The `SYS.` qualifier has already been stripped earlier
// in the rewrite pipeline, so we only match the `DBMS_UTILITY.<name>`
// form here.
//
// Mapping (no runtime helper required — every replacement is either an
// orafce-shipped function or a PG built-in):
//
//	FORMAT_CALL_STACK      → dbms_utility.format_call_stack()  (orafce, native)
//	FORMAT_ERROR_BACKTRACE → ''                                (no inline PG analog;
//	                                                            use GET STACKED
//	                                                            DIAGNOSTICS … =
//	                                                            PG_EXCEPTION_CONTEXT
//	                                                            inside an EXCEPTION
//	                                                            handler if you need
//	                                                            the real value)
//	FORMAT_ERROR_STACK     → SQLERRM                           (PG built-in variable)
//
// Why a custom pass instead of replaceWholeWordCI: the source is a
// dotted identifier (`DBMS_UTILITY.FORMAT_CALL_STACK`) that the generic
// word-boundary helper can't match because `.` is treated as a separator.
// We walk byte-by-byte, recognise the literal sequence
// (case-insensitive), and emit the right replacement. Whitespace
// between the dot segments is tolerated so multi-line PL/SQL keeps
// translating cleanly.
func rewriteOracleDbmsUtilityFormatCallStack(s string) string {
	if s == "" {
		return s
	}
	upper := strings.ToUpper(s)
	// Hot-path skip: only run the slow scanner when at least one target
	// suffix is present.
	if !strings.Contains(upper, "FORMAT_CALL_STACK") &&
		!strings.Contains(upper, "FORMAT_ERROR_BACKTRACE") &&
		!strings.Contains(upper, "FORMAT_ERROR_STACK") {
		return s
	}
	// Targets are tried longest-first so FORMAT_ERROR_STACK doesn't
	// short-circuit FORMAT_ERROR_BACKTRACE matches. Each target carries
	// the literal replacement text and a flag telling us whether the
	// replacement is a function call (orafce) — only function-call
	// targets need the trailing `()` normalisation that PG demands.
	targets := []struct {
		upper       string
		replacement string
		isCall      bool
	}{
		{"FORMAT_ERROR_BACKTRACE", "''", false},                    // no PG analog
		{"FORMAT_ERROR_STACK", "SQLERRM", false},                   // PG built-in
		{"FORMAT_CALL_STACK", "dbms_utility.format_call_stack", true}, // orafce native
	}
	var out strings.Builder
	i := 0
	for i < len(s) {
		// Find the earliest match of any target suffix at i.
		bestIdx := -1
		var bestT struct {
			upper       string
			replacement string
			isCall      bool
		}
		for _, t := range targets {
			idx := strings.Index(upper[i:], t.upper)
			if idx < 0 {
				continue
			}
			idx += i
			if bestIdx < 0 || idx < bestIdx {
				bestIdx = idx
				bestT = t
			}
		}
		if bestIdx < 0 {
			out.WriteString(s[i:])
			break
		}
		// Confirm the preceding token is DBMS_UTILITY (possibly with
		// whitespace around the dot) — otherwise pass the suffix through
		// untouched.
		matchStart, ok := matchDbmsUtilityPrefix(s, upper, bestIdx)
		if !ok {
			out.WriteString(s[i : bestIdx+len(bestT.upper)])
			i = bestIdx + len(bestT.upper)
			continue
		}
		// Right boundary: must end at a non-ident byte.
		end := bestIdx + len(bestT.upper)
		if end < len(s) && isIdentByte(s[end]) {
			out.WriteString(s[i:end])
			i = end
			continue
		}
		// Consume optional whitespace + `()` left over from Oracle's
		// "may call zero-arg routines without parens" form.
		afterParens := end
		j := end
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j+1 < len(s) && s[j] == '(' && s[j+1] == ')' {
			afterParens = j + 2
		}
		out.WriteString(s[i:matchStart])
		out.WriteString(bestT.replacement)
		if bestT.isCall {
			// PG insists on the `()` form for function calls.
			out.WriteString("()")
		}
		i = afterParens
	}
	return out.String()
}

// matchDbmsUtilityPrefix verifies that the bytes immediately before
// position `at` form `[SYS.]DBMS_UTILITY.` (case-insensitive, with
// optional whitespace around the dots). Returns the start offset of the
// matched prefix and true on hit. Used to confirm that a
// `FORMAT_CALL_STACK` substring is qualified by the Oracle package name
// rather than appearing as a bare identifier in unrelated code.
func matchDbmsUtilityPrefix(s, upper string, at int) (int, bool) {
	// Walk back over whitespace + `.` + whitespace.
	k := at
	if k == 0 || s[k-1] != '.' {
		// Whitespace between segments? Allow `DBMS_UTILITY . FORMAT_CALL_STACK`.
		j := k
		for j > 0 && (s[j-1] == ' ' || s[j-1] == '\t') {
			j--
		}
		if j == 0 || s[j-1] != '.' {
			return 0, false
		}
		k = j
	}
	k-- // consumed `.`
	// Whitespace before the dot.
	for k > 0 && (s[k-1] == ' ' || s[k-1] == '\t') {
		k--
	}
	const pkg = "DBMS_UTILITY"
	if k < len(pkg) {
		return 0, false
	}
	if upper[k-len(pkg):k] != pkg {
		return 0, false
	}
	// Whole-word: byte before DBMS_UTILITY must not be an ident byte
	// (allow `.`, whitespace, start-of-string).
	pkgStart := k - len(pkg)
	if pkgStart > 0 && isIdentByte(s[pkgStart-1]) {
		return 0, false
	}
	matchStart := pkgStart
	// Optional `SYS.` prefix.
	m := pkgStart
	if m > 0 && s[m-1] == '.' {
		m--
		for m > 0 && (s[m-1] == ' ' || s[m-1] == '\t') {
			m--
		}
		const sysKw = "SYS"
		if m >= len(sysKw) && upper[m-len(sysKw):m] == sysKw {
			sysStart := m - len(sysKw)
			if sysStart == 0 || !isIdentByte(s[sysStart-1]) {
				matchStart = sysStart
			}
		}
	}
	return matchStart, true
}

// rewriteOracleTrunc rewrites Oracle's TRUNC applied to a date or timestamp
// expression to PG's `date_trunc(<unit>, <expr>)::date`. PG's trunc() exists
// only for numeric types, so `TRUNC(<date_expr>)` raises
// "function trunc(timestamp without time zone) does not exist" when emitted
// verbatim.
//
// Two forms are handled:
//
//   - `TRUNC(<date_expr>)`        → `date_trunc('day', <date_expr>)::date`
//   - `TRUNC(<date_expr>, 'fmt')` → `date_trunc('<unit>', <date_expr>)::date`
//
// `<date_expr>` is recognised heuristically: the trimmed argument's first
// token is one of {SYSDATE, SYSTIMESTAMP, CURRENT_DATE, CURRENT_TIMESTAMP,
// LOCALTIMESTAMP} or an opening call to TO_DATE / TO_TIMESTAMP. Numeric
// uses of TRUNC (`TRUNC(123.45)`, `TRUNC(x, 2)`) are left alone so PG's
// native trunc() handles them.
//
// Format masks are mapped from Oracle to PG: 'YYYY'/'YEAR' → 'year',
// 'Q' → 'quarter', 'MM'/'MON'/'MONTH' → 'month', 'IW'/'WW'/'W' → 'week',
// 'DD'/'DDD'/'J' → 'day', 'HH'/'HH24'/'HH12' → 'hour', 'MI' → 'minute'.
// Unknown masks fall back to 'day' so the routine still compiles.
func rewriteOracleTrunc(s string) string {
	const fname = "trunc"
	if !containsKeywordCI(s, fname) {
		return s
	}
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		j := skipWS(s, k+len(fname))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		if len(args) < 1 || len(args) > 2 {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		dateArg := strings.TrimSpace(args[0])
		if !looksLikeDateExpr(dateArg) {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		unit := "day"
		if len(args) == 2 {
			fmtArg := strings.TrimSpace(args[1])
			if len(fmtArg) >= 2 && fmtArg[0] == '\'' && fmtArg[len(fmtArg)-1] == '\'' {
				unit = oracleFmtMaskToPGUnit(strings.ToUpper(fmtArg[1 : len(fmtArg)-1]))
			}
		}
		out.WriteString(s[i:k])
		fmt.Fprintf(&out, "date_trunc('%s', %s)::date", unit, dateArg)
		i = end + 1
	}
	return out.String()
}

// looksLikeDateExpr returns true when expr's leading identifier matches a
// known Oracle date source. Keeps the TRUNC rewrite conservative: arbitrary
// numeric expressions are left alone so PG's native trunc() handles them.
func looksLikeDateExpr(expr string) bool {
	t := strings.TrimSpace(expr)
	if t == "" {
		return false
	}
	// Strip a leading `(` if the whole expression is parenthesised; the
	// content inside still needs to start with a date keyword for us to be
	// confident.
	for len(t) > 0 && t[0] == '(' {
		t = strings.TrimSpace(t[1:])
	}
	upper := strings.ToUpper(t)
	for _, kw := range []string{
		"SYSDATE", "SYSTIMESTAMP", "CURRENT_DATE", "CURRENT_TIMESTAMP", "LOCALTIMESTAMP",
		"TO_DATE", "TO_TIMESTAMP", "ADD_MONTHS", "TRUNC",
	} {
		if strings.HasPrefix(upper, kw) {
			rest := upper[len(kw):]
			if rest == "" {
				return true
			}
			c := rest[0]
			// Whole-word boundary: keyword must end at a non-ident byte
			// (`(`, space, `+`, `-`, …) so SYSDATE_X / TO_DATEISH don't
			// trigger the rewrite.
			if !isIdentByte(c) {
				return true
			}
		}
	}
	return false
}

// oracleFmtMaskToPGUnit maps an uppercased Oracle format mask passed to
// TRUNC(date, fmt) to the equivalent date_trunc unit.
func oracleFmtMaskToPGUnit(mask string) string {
	switch mask {
	case "YYYY", "YEAR", "SYYYY", "YYY", "YY", "Y":
		return "year"
	case "Q":
		return "quarter"
	case "MM", "MON", "MONTH", "RM":
		return "month"
	case "WW", "IW", "W":
		return "week"
	case "DD", "DDD", "J":
		return "day"
	case "HH", "HH12", "HH24":
		return "hour"
	case "MI":
		return "minute"
	}
	return "day"
}

func rewriteSysContext(s string) string {
	const fname = "sys_context"
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		j := skipWS(s, k+len(fname))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		if len(args) != 2 {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		ns := strings.TrimSpace(args[0])
		param := strings.TrimSpace(args[1])
		if len(ns) < 2 || ns[0] != '\'' || ns[len(ns)-1] != '\'' ||
			len(param) < 2 || param[0] != '\'' || param[len(param)-1] != '\'' {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		key := strings.ToLower(strings.TrimSpace(param[1 : len(param)-1]))
		out.WriteString(s[i:k])
		out.WriteString("current_setting('squishy.")
		out.WriteString(key)
		out.WriteString("', true)")
		i = end + 1
	}
	return out.String()
}

// hasOuterJoinPlus reports whether s contains Oracle's `(+)` outer-join
// marker outside string literals. Replaces the regex \(\s*\+\s*\).
func hasOuterJoinPlus(s string) bool {
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if c == '\'' {
			inStr = true
			continue
		}
		if c == '(' {
			j := skipWS(s, i+1)
			if j < len(s) && s[j] == '+' {
				j = skipWS(s, j+1)
				if j < len(s) && s[j] == ')' {
					return true
				}
			}
		}
	}
	return false
}

// stripOuterJoinPlus removes every `(+)` marker (with internal whitespace)
// from s. Replaces reOuterJoinPlus.ReplaceAllString(s, "").
func stripOuterJoinPlus(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			out.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					out.WriteByte(s[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if c == '\'' {
			inStr = true
			out.WriteByte(c)
			continue
		}
		if c == '(' {
			j := skipWS(s, i+1)
			if j < len(s) && s[j] == '+' {
				k := skipWS(s, j+1)
				if k < len(s) && s[k] == ')' {
					i = k
					continue
				}
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

// collapseHorizontalWS collapses runs of spaces and tabs to a single space.
// Replaces reMultiSpace.ReplaceAllString(s, " ").
func collapseHorizontalWS(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	prev := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !prev {
				out.WriteByte(' ')
				prev = true
			}
			continue
		}
		out.WriteByte(c)
		prev = false
	}
	return out.String()
}

// rewriteOraclePlusOuterJoins converts Oracle's classic `tbl.col(+)` outer-join
// syntax into ANSI LEFT JOIN form. It rewrites the FROM/WHERE pair in place
// for the simple — but very common — pattern emitted by DBMS_METADATA:
//
//	FROM   t1 a1, t2 a2 [, t3 a3, ...]
//	WHERE  a2.col1 (+) = a1.col1
//	  AND  a2.col2 (+) = <expr without (+)>
//	  AND  <inner conditions...>
//
// Each table aliased on the `(+)` side becomes the right operand of a
// LEFT JOIN with all of its tagged conditions moved into the join's ON clause.
// Inner (untagged) conditions stay in WHERE. If any structural assumption is
// violated (subqueries with their own FROMs, mixed left/right (+) on the same
// pair, etc.), the function returns the input unchanged so the downstream
// parse error surfaces rather than producing a silently-wrong rewrite.
func rewriteOraclePlusOuterJoins(body string) string {
	fromIdx := findKeyword(body, 0, "FROM")
	if fromIdx < 0 {
		return body
	}
	whereIdx := findKeyword(body, fromIdx+4, "WHERE")
	if whereIdx < 0 {
		return body
	}
	endIdx := findAnyKeyword(body, whereIdx+5,
		"GROUP", "HAVING", "ORDER", "WINDOW",
		"UNION", "INTERSECT", "MINUS", "EXCEPT", "FETCH", "OFFSET", "CONNECT", "START")
	if endIdx < 0 {
		endIdx = len(body)
	}

	fromClause := strings.TrimSpace(body[fromIdx+4 : whereIdx])
	whereClause := strings.TrimSpace(body[whereIdx+5 : endIdx])

	// Parse FROM into [(rawText, alias)] entries. We only handle the simple
	// comma-separated form — any explicit JOIN keyword means the FROM was
	// already partially ANSI-fied, in which case `(+)` mixed with JOIN is
	// undefined, so we bail out.
	if hasExplicitJoinKeyword(fromClause) {
		return body
	}
	tables := splitTopLevelByComma(fromClause)
	if len(tables) < 2 {
		return body
	}
	type tabRef struct {
		raw, alias string
	}
	refs := make([]tabRef, 0, len(tables))
	aliasIdx := map[string]int{}
	for _, t := range tables {
		t = strings.TrimSpace(t)
		alias := lastWord(t)
		if alias == "" {
			return body
		}
		aliasIdx[strings.ToLower(alias)] = len(refs)
		refs = append(refs, tabRef{raw: t, alias: alias})
	}

	// Split WHERE into top-level AND-separated conditions, classify each
	// as inner / LEFT-outer(alias) / FULL-outer(pair). Outer aliases are
	// tracked in FROM declaration order so the final JOIN sequence is
	// deterministic.
	type joinSpec struct {
		kind string // "LEFT" or "FULL"
		on   []string
	}
	conds := splitTopLevelAnd(whereClause)
	joins := map[string]*joinSpec{} // outer alias (lowercased) → join spec
	var outerOrder []string         // first-appearance order
	var innerConds []string

	addOuter := func(alias, kind, condStripped string) bool {
		key := strings.ToLower(alias)
		if _, ok := aliasIdx[key]; !ok {
			// Tag references an alias that isn't in our FROM.
			return false
		}
		js, seen := joins[key]
		if !seen {
			joins[key] = &joinSpec{kind: kind, on: []string{condStripped}}
			outerOrder = append(outerOrder, key)
			return true
		}
		// Same alias already classified — its kind must agree across
		// conditions. Mixed LEFT+FULL on a single table is undefined
		// in Oracle's (+) syntax, so bail out at the caller.
		if js.kind != kind {
			return false
		}
		js.on = append(js.on, condStripped)
		return true
	}

	for _, c := range conds {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		tagged := outerJoinAliases(c)
		if len(tagged) == 0 {
			innerConds = append(innerConds, c)
			continue
		}
		// Strip the `(+)` markers and collapse the double whitespace they
		// leave behind so the rendered ON clause is tidy.
		stripped := stripOuterJoinPlus(c)
		stripped = strings.TrimSpace(collapseHorizontalWS(stripped))

		switch len(tagged) {
		case 1:
			// LEFT JOIN: the tagged alias is the outer (right) side.
			if !addOuter(tagged[0], "LEFT", stripped) {
				return body
			}
		case 2:
			// FULL OUTER JOIN: both sides tagged. The alias that appears
			// LATER in FROM becomes the joined relation; the earlier one
			// stays as an anchor we'll FULL JOIN against.
			a, b := strings.ToLower(tagged[0]), strings.ToLower(tagged[1])
			ai, aok := aliasIdx[a]
			bi, bok := aliasIdx[b]
			if !aok || !bok {
				return body
			}
			joinedAlias := tagged[0]
			if bi > ai {
				joinedAlias = tagged[1]
			}
			if !addOuter(joinedAlias, "FULL", stripped) {
				return body
			}
		default:
			// 3+ tagged aliases on a single condition is structurally
			// non-trivial — bail out and let the parse error surface.
			return body
		}
	}
	if len(outerOrder) == 0 {
		return body
	}

	// Build the new FROM: anchor tables first (any FROM table not in the
	// outer set), then each outer table joined with its grouped ON in the
	// order it first appeared.
	used := make([]bool, len(refs))
	for _, key := range outerOrder {
		used[aliasIdx[key]] = true
	}
	var fromParts []string
	for i, r := range refs {
		if !used[i] {
			fromParts = append(fromParts, r.raw)
		}
	}
	if len(fromParts) == 0 {
		// All tables tagged outer — nothing to anchor on. Bail out.
		return body
	}
	newFrom := strings.Join(fromParts, ", ")
	for _, key := range outerOrder {
		ref := refs[aliasIdx[key]]
		js := joins[key]
		on := strings.Join(js.on, "\n   AND ")
		newFrom += "\n" + js.kind + " JOIN " + ref.raw + "\n  ON " + on
	}

	var newWhere string
	if len(innerConds) > 0 {
		newWhere = "WHERE " + strings.Join(innerConds, "\n  AND ") + "\n"
	}

	return body[:fromIdx] + "FROM " + newFrom + "\n" + newWhere + body[endIdx:]
}

// hasExplicitJoinKeyword detects an explicit JOIN keyword in a FROM clause:
// any of INNER / LEFT / RIGHT / FULL / CROSS / NATURAL / JOIN as a whole word.
// Replaces the regex (?i)\b(INNER|LEFT|RIGHT|FULL|CROSS|NATURAL|JOIN)\b.
func hasExplicitJoinKeyword(s string) bool {
	for _, kw := range []string{"INNER", "LEFT", "RIGHT", "FULL", "CROSS", "NATURAL", "JOIN"} {
		if findKeywordCI(s, 0, kw) >= 0 {
			return true
		}
	}
	return false
}

// outerJoinAliases returns the aliases tagged with `(+)` in the given
// condition, in left-to-right order. Each occurrence is included exactly
// once even if the same alias has multiple tagged columns. The tag must be
// attached to a `<alias>.<col>(+)` reference; (+) on literals or unqualified
// columns is ignored.
func outerJoinAliases(cond string) []string {
	if !hasOuterJoinPlus(cond) {
		return nil
	}
	matches := findAliasPlusMarkers(cond)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(matches))
	for _, alias := range matches {
		k := strings.ToLower(alias)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, alias)
	}
	return out
}

// findAliasPlusMarkers returns the alias preceding every `<alias>.<col>(+)`
// reference in cond, in left-to-right order. Replaces the regex
//   (?i)([A-Za-z_][A-Za-z_0-9$#]*)\s*\.\s*[A-Za-z_][A-Za-z_0-9$#]*\s*\(\s*\+\s*\).
// The walk honours single-quoted strings.
func findAliasPlusMarkers(cond string) []string {
	var out []string
	inStr := false
	for i := 0; i < len(cond); i++ {
		c := cond[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(cond) && cond[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if c == '\'' {
			inStr = true
			continue
		}
		if !isIdentStart(c) {
			continue
		}
		// Match an identifier
		idStart := i
		for i < len(cond) && isIdentChar(cond[i]) {
			i++
		}
		idEnd := i
		j := skipWS(cond, i)
		if j >= len(cond) || cond[j] != '.' {
			i = idEnd - 1
			continue
		}
		j = skipWS(cond, j+1)
		if j >= len(cond) || !isIdentStart(cond[j]) {
			i = idEnd - 1
			continue
		}
		colEnd := j
		for colEnd < len(cond) && isIdentChar(cond[colEnd]) {
			colEnd++
		}
		j = skipWS(cond, colEnd)
		if j >= len(cond) || cond[j] != '(' {
			i = idEnd - 1
			continue
		}
		k := skipWS(cond, j+1)
		if k >= len(cond) || cond[k] != '+' {
			i = idEnd - 1
			continue
		}
		k = skipWS(cond, k+1)
		if k >= len(cond) || cond[k] != ')' {
			i = idEnd - 1
			continue
		}
		out = append(out, cond[idStart:idEnd])
		i = k
	}
	return out
}

// (alias-plus markers are now extracted by findAliasPlusMarkers, see above.)

// findKeyword locates the first occurrence of `kw` (case-insensitive) at a
// word boundary, outside of quoted strings or parentheses, starting at offset
// `from`. Returns -1 if absent.
func findKeyword(body string, from int, kw string) int {
	return findAnyKeyword(body, from, kw)
}

// findAnyKeyword is like findKeyword but matches the first of several
// alternative keywords.
func findAnyKeyword(body string, from int, kws ...string) int {
	depth := 0
	inStr := false
	upperKws := make([]string, len(kws))
	for i, k := range kws {
		upperKws[i] = strings.ToUpper(k)
	}
	for i := from; i < len(body); i++ {
		c := body[i]
		switch c {
		case '\'':
			if inStr && i+1 < len(body) && body[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		case '(':
			if !inStr {
				depth++
			}
			continue
		case ')':
			if !inStr {
				depth--
			}
			continue
		}
		if inStr || depth != 0 {
			continue
		}
		if i > from && isIdentChar(body[i-1]) {
			continue
		}
		for _, kw := range upperKws {
			if i+len(kw) > len(body) {
				continue
			}
			seg := body[i : i+len(kw)]
			if !strings.EqualFold(seg, kw) {
				continue
			}
			if i+len(kw) < len(body) && isIdentChar(body[i+len(kw)]) {
				continue
			}
			return i
		}
	}
	return -1
}

func isIdentChar(b byte) bool {
	return b == '_' || b == '$' || b == '#' ||
		(b >= '0' && b <= '9') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z')
}

// splitTopLevelByComma splits `s` at commas that are at parenthesis depth 0
// and outside quoted strings.
func splitTopLevelByComma(s string) []string {
	var parts []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
		case '(':
			if !inStr {
				depth++
			}
		case ')':
			if !inStr {
				depth--
			}
		case ',':
			if !inStr && depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// splitTopLevelAnd splits `s` at the keyword AND when it sits at parenthesis
// depth 0 and outside quoted strings.
func splitTopLevelAnd(s string) []string {
	var parts []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		case '(':
			if !inStr {
				depth++
			}
			continue
		case ')':
			if !inStr {
				depth--
			}
			continue
		}
		if inStr || depth != 0 {
			continue
		}
		if i > start && i+3 <= len(s) && strings.EqualFold(s[i:i+3], "AND") {
			before := s[i-1]
			var after byte
			if i+3 < len(s) {
				after = s[i+3]
			}
			if (before == ' ' || before == '\t' || before == '\n' || before == '\r') &&
				(after == 0 || after == ' ' || after == '\t' || after == '\n' || after == '\r' || after == '(') {
				parts = append(parts, s[start:i])
				start = i + 3
				i += 2
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// lastWord returns the trailing identifier word of `s`, used to extract the
// alias from a FROM-clause table reference like `product_information  i`.
// Returns "" when the trailing token isn't an identifier.
func lastWord(s string) string {
	s = strings.TrimRight(s, " \t\n\r")
	end := len(s)
	for end > 0 && isIdentChar(s[end-1]) {
		end--
	}
	if end == len(s) {
		return ""
	}
	return s[end:]
}

// replaceWholeWordCI substitutes every whole-word, case-insensitive
// occurrence of word in s with replacement. String literals are honoured.
// Replaces calls of the form `reWord(word).ReplaceAllString(s, repl)`.
func replaceWholeWordCI(s, word, repl string) string {
	if word == "" {
		return s
	}
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, word)
		if k < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		out.WriteString(s[i:k])
		out.WriteString(repl)
		i = k + len(word)
	}
}

// renameFuncCall substitutes every whole-word case-insensitive function-call
// opener `<name> (` (whitespace-tolerant) in s with `newOpener`. Replaces
// `reFunc(name).ReplaceAllString(s, newOpener)`.
func renameFuncCall(s, name, newOpener string) string {
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, name)
		if k < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		j := skipWS(s, k+len(name))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(name)])
			i = k + len(name)
			continue
		}
		out.WriteString(s[i:k])
		out.WriteString(newOpener)
		// Skip past the original `<name> <ws> (`. The new opener already
		// includes its own '(' (e.g. "COALESCE("), so advance past the source
		// `(` too.
		i = j + 1
	}
}

// rewriteNextvalCurrval converts SEQ.NEXTVAL / "SEQ".NEXTVAL / SEQ.CURRVAL
// into nextval('seq') / currval('seq'). Replaces the regex
//   (?i)(?:"([^"]+)"|([A-Za-z_][A-Za-z_0-9$#]*))\.(NEXTVAL|CURRVAL)\b.
func rewriteNextvalCurrval(s string) string {
	// readIdent reads either a quoted or bare identifier starting at position
	// p; returns (text, end-of-token, ok). Bare identifiers are lowercased to
	// match PG's case-folding convention; quoted identifiers preserve case.
	readIdent := func(p int) (string, int, bool) {
		if p >= len(s) {
			return "", p, false
		}
		if s[p] == '"' {
			endQ := strings.IndexByte(s[p+1:], '"')
			if endQ < 0 {
				return "", p, false
			}
			return s[p+1 : p+1+endQ], p + 1 + endQ + 1, true
		}
		if !isIdentStart(s[p]) {
			return "", p, false
		}
		j := p
		for j < len(s) && isIdentChar(s[j]) {
			j++
		}
		return strings.ToLower(s[p:j]), j, true
	}

	// matchSeqCall parses [schema.]seq.{NEXTVAL|CURRVAL} starting at p.
	// On success returns (fn, seq, end, ok). Handles 1-, 2-, and 3-part
	// references where the trailing pseudocolumn may be quoted (`"NEXTVAL"`)
	// or bare (NEXTVAL).
	matchSeqCall := func(p int) (string, string, int, bool) {
		first, q, ok := readIdent(p)
		if !ok {
			return "", "", 0, false
		}
		if q >= len(s) || s[q] != '.' {
			return "", "", 0, false
		}
		// Quoted form for the trailing pseudocolumn: `"NEXTVAL"` /
		// `"CURRVAL"` — accept both. DBMS_METADATA emits this form for
		// 3-part column DEFAULT references like `"S"."SEQ"."NEXTVAL"`.
		isPseudo := func(x string) (string, bool) {
			switch strings.ToUpper(x) {
			case "NEXTVAL":
				return "nextval", true
			case "CURRVAL":
				return "currval", true
			}
			return "", false
		}
		// Try 2-part: first . pseudo
		second, q2, ok := readIdent(q + 1)
		if !ok {
			return "", "", 0, false
		}
		if fn, ok := isPseudo(second); ok {
			return fn, first, q2, true
		}
		// Try 3-part: first . second . pseudo
		if q2 >= len(s) || s[q2] != '.' {
			return "", "", 0, false
		}
		third, q3, ok := readIdent(q2 + 1)
		if !ok {
			return "", "", 0, false
		}
		if fn, ok := isPseudo(third); ok {
			// Schema-qualified: emit the bare sequence name. PG sequences
			// resolve via search_path; the schema is set at the top of
			// the DDL script (`SET search_path TO <schema>, public;`).
			_ = first
			return fn, second, q3, true
		}
		return "", "", 0, false
	}

	var out strings.Builder
	i := 0
	for i < len(s) {
		// Word boundary check on the left so we don't rewrite things like
		// `tabular_seq.nextval` where the leading char is mid-identifier.
		startCh := s[i]
		if startCh != '"' && (startCh < 'A' || (startCh > 'Z' && startCh < 'a') || startCh > 'z') && startCh != '_' {
			out.WriteByte(startCh)
			i++
			continue
		}
		if i > 0 && startCh != '"' && isIdentChar(s[i-1]) {
			out.WriteByte(startCh)
			i++
			continue
		}
		fn, seq, end, ok := matchSeqCall(i)
		if !ok {
			out.WriteByte(startCh)
			i++
			continue
		}
		// Embed the sequence name as a quoted identifier inside the
		// nextval string literal: PG's regclass parser would otherwise
		// case-fold an unquoted name. Oracle preserves the original case
		// (and DBMS_METADATA emits SEQ#X$Y-style names with #/$ chars
		// which must be quoted on PG to avoid syntax errors).
		out.WriteString(fn)
		out.WriteString("('\"")
		out.WriteString(strings.ReplaceAll(seq, `"`, `""`))
		out.WriteString("\"')")
		i = end
	}
	return out.String()
}

// rewriteListagg converts Oracle's LISTAGG(expr [, sep]) WITHIN GROUP (ORDER
// BY ord) aggregate into PG's string_agg((expr)::text, sep [ORDER BY ord]).
// Replaces the regex
//   (?is)\bLISTAGG\s*\(([^()]*)\)(\s*WITHIN\s+GROUP\s*\(\s*ORDER\s+BY\s+([^()]+)\))?
// with the same shape constraint (no nested parens inside the LISTAGG and
// the WITHIN-GROUP arglist).
func rewriteListagg(s string) string {
	const fname = "LISTAGG"
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		j := skipWS(s, k+len(fname))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		// Inner of LISTAGG(...) — must contain no nested parens.
		argsEnd := strings.IndexByte(s[j:], ')')
		if argsEnd < 0 {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		argsEnd += j
		args := s[j+1 : argsEnd]
		if strings.ContainsAny(args, "()") {
			out.WriteString(s[i : argsEnd+1])
			i = argsEnd + 1
			continue
		}
		// Optional WITHIN GROUP (ORDER BY <ord>)
		order := ""
		consumedTo := argsEnd + 1
		w := skipWS(s, argsEnd+1)
		if startsKeywordCI(s, w, "WITHIN") {
			w2 := skipWS(s, w+len("WITHIN"))
			if startsKeywordCI(s, w2, "GROUP") {
				w3 := skipWS(s, w2+len("GROUP"))
				if w3 < len(s) && s[w3] == '(' {
					ordEnd := strings.IndexByte(s[w3:], ')')
					if ordEnd > 0 {
						ordEnd += w3
						body := s[w3+1 : ordEnd]
						if !strings.ContainsAny(body, "()") {
							ob := skipWS(body, 0)
							if startsKeywordCI(body, ob, "ORDER") {
								ob = skipWS(body, ob+len("ORDER"))
								if startsKeywordCI(body, ob, "BY") {
									order = strings.TrimSpace(body[ob+len("BY"):])
									consumedTo = ordEnd + 1
								}
							}
						}
					}
				}
			}
		}
		expr, sep := splitLastTopLevelComma(strings.TrimSpace(args))
		if sep == "" {
			sep = "''"
		}
		out.WriteString(s[i:k])
		if order != "" {
			out.WriteString("string_agg((")
			out.WriteString(expr)
			out.WriteString(")::text, ")
			out.WriteString(sep)
			out.WriteString(" ORDER BY ")
			out.WriteString(order)
			out.WriteString(")")
		} else {
			out.WriteString("string_agg((")
			out.WriteString(expr)
			out.WriteString(")::text, ")
			out.WriteString(sep)
			out.WriteString(")")
		}
		i = consumedTo
	}
}

// splitLastTopLevelComma returns (leftOfLastComma, rightOfLastComma),
// skipping commas inside parentheses or single-quoted strings so a nested
// function call's comma doesn't confuse the split. Returns (s, "") when
// no top-level comma is found.
func splitLastTopLevelComma(s string) (string, string) {
	depth := 0
	inStr := false
	last := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			// Handle '' escape.
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
		case '(':
			if !inStr {
				depth++
			}
		case ')':
			if !inStr {
				depth--
			}
		case ',':
			if !inStr && depth == 0 {
				last = i
			}
		}
	}
	if last < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:last]), strings.TrimSpace(s[last+1:])
}

// ---------------------------------------------------------------------------
// Token-based rewriter for `TABLE(<expr>)` (O-23) and Oracle collection
// methods at expression-level (O-22a).
//
// We can't lean on a structural expression AST because the codebase
// captures all expression-level content as `RawExpr.Text` / `RawSQL.Text`.
// Instead of regex-walking the raw string (fragile w.r.t. literals,
// nested parens, identifier boundaries), we feed the body back through
// the Oracle lexer and walk tokens — that re-uses the same string-literal
// / paren / keyword classification the rest of the parser depends on, and
// keeps the rewrite "structural" without a full expression-parser refactor.
//
// Patterns rewritten (one pass, left-to-right):
//
//   `TABLE`  `(` …  `)`             → `unnest` `(` … `)`           (O-23)
//   <chain>  `.`  `COUNT`           → `COALESCE(array_length(<chain>, 1), 0)`
//   <chain>  `.`  `FIRST`           → `1`
//   <chain>  `.`  `LAST`            → `array_length(<chain>, 1)`
//   <chain>  `.`  `NEXT`  `(` <i> `)` → `(<i> + 1)`
//   <chain>  `.`  `PRIOR` `(` <i> `)` → `(<i> - 1)`
//   <chain>  `.`  `EXISTS``(` <i> `)` → `(<chain>[<i>] IS NOT NULL)`
//
// where <chain> is a contiguous IDENT (`.` IDENT)* sequence.
// ---------------------------------------------------------------------------

func rewriteOracleCollectionTokens(body string) string {
	if body == "" {
		return body
	}
	tokens := lexOracleBody(body)
	if len(tokens) == 0 {
		return body
	}
	runes := []rune(body)

	type rep struct {
		startRune int // inclusive
		endRune   int // exclusive
		text      string
	}
	var reps []rep

	tokRunes := func(t oracledialect.Token) (start, end int) {
		start = t.Pos.Offset
		// PUNCT / ASSIGN / RANGE tokens have empty Raw — the literal text
		// lives in t.Lit. Fall back to Lit length so end-positions are
		// correct for paren / dot / `:=` / `..` tokens.
		w := len([]rune(t.Raw))
		if w == 0 {
			w = len([]rune(t.Lit))
		}
		end = start + w
		return
	}
	chainText := func(chain []oracledialect.Token) string {
		// Re-render the chain by slicing the original rune buffer between
		// the first token's start and the last token's end.
		s, _ := tokRunes(chain[0])
		_, e := tokRunes(chain[len(chain)-1])
		return string(runes[s:e])
	}

	i := 0
	for i < len(tokens) {
		cur := tokens[i]

		// O-23: `TABLE(<expr>)`. Recognize TABLE keyword (or IDENT lit) at
		// position i, with a `(` immediately following at i+1. The argument
		// list itself is left untouched — only the leading `TABLE` token is
		// rewritten to `unnest`. Nested `TABLE(...)` inside the inner expr
		// are caught on subsequent walker passes through their own tokens.
		if isWordToken(cur, "TABLE") &&
			i+1 < len(tokens) && isPunct(tokens[i+1], "(") {
			s, e := tokRunes(cur)
			reps = append(reps, rep{startRune: s, endRune: e, text: "unnest"})
			i++
			continue
		}

		// O-37: Oracle Spatial functions — `SDO_<fn>(...)` and
		// `SDO_GEOM.SDO_<fn>(...)` rewrite to their PostGIS counterparts.
		// We detect call shape (ident or pkg.ident followed by `(`),
		// then match against the SDO function table; on a hit we capture
		// the args list (top-level comma split, paren-depth aware) and
		// reassemble as ST_<fn>(...).
		if isIdentToken(cur) && i+1 < len(tokens) {
			// Find the call's name and how many tokens it spans
			// (1 for plain ident, 3 for pkg.ident).
			nameTokens := 1
			fullName := strings.ToUpper(cur.Lit)
			if i+2 < len(tokens) && isPunct(tokens[i+1], ".") &&
				isIdentToken(tokens[i+2]) {
				fullName = strings.ToUpper(cur.Lit) + "." + strings.ToUpper(tokens[i+2].Lit)
				nameTokens = 3
			}
			callOpenIdx := i + nameTokens
			if callOpenIdx < len(tokens) && isPunct(tokens[callOpenIdx], "(") {
				if rewriter, ok := sdoRewriteFor(fullName); ok {
					// Find matching `)` at depth 0.
					depth := 0
					closeIdx := -1
					for k := callOpenIdx; k < len(tokens); k++ {
						if isPunct(tokens[k], "(") {
							depth++
						} else if isPunct(tokens[k], ")") {
							depth--
							if depth == 0 {
								closeIdx = k
								break
							}
						}
					}
					if closeIdx > callOpenIdx {
						openS, _ := tokRunes(tokens[callOpenIdx])
						_, closeE := tokRunes(tokens[closeIdx])
						argsRaw := string(runes[openS+1 : closeE-1])
						if newCall, rok := rewriter(argsRaw); rok {
							nameStartRune, _ := tokRunes(cur)
							reps = append(reps, rep{
								startRune: nameStartRune,
								endRune:   closeE,
								text:      newCall,
							})
							i = closeIdx + 1
							continue
						}
					}
				}
			}
		}

		// O-22a: <chain> `.` <method>[`(`<arg>`)`] — walk forward to
		// collect a maximal IDENT chain ending at the current position,
		// then check the lookahead for `.METHOD`.
		// We trigger on a `.` token that's preceded by an ident chain and
		// followed by one of the method keywords.
		if isPunct(cur, ".") && i > 0 && i+1 < len(tokens) {
			methodTok := tokens[i+1]
			method := strings.ToUpper(methodTok.Lit)
			switch method {
			case "COUNT", "FIRST", "LAST", "NEXT", "PRIOR", "EXISTS":
				// only treat as collection-method if the suffix is a name
				// token, not a punct/literal.
				if methodTok.Kind != oracledialect.TOK_IDENT &&
					methodTok.Kind != oracledialect.TOK_KEYWORD &&
					methodTok.Kind != oracledialect.TOK_QUOTED_IDENT {
					i++
					continue
				}
				// Walk back to find the start of the dotted-ident chain.
				// The token immediately before the `.` (i.e. tokens[i-1])
				// is always the rightmost chain ident. We then extend left
				// as long as the next-left pair is `.<IDENT>`.
				if i < 1 || !isIdentToken(tokens[i-1]) {
					i++
					continue
				}
				chain := []oracledialect.Token{tokens[i-1]}
				left := i - 1
				for left-2 >= 0 &&
					isPunct(tokens[left-1], ".") &&
					isIdentToken(tokens[left-2]) {
					chain = append([]oracledialect.Token{tokens[left-2]}, chain...)
					left -= 2
				}
				chStr := chainText(chain)

				// Has `(arg)` after the method?
				hasArgs := i+2 < len(tokens) && isPunct(tokens[i+2], "(")
				var argText string
				var argEnd int // rune index of the `)`+1 when args present
				if hasArgs {
					// find matching `)` at depth 0 starting from i+2
					depth := 0
					argClose := -1
					for k := i + 2; k < len(tokens); k++ {
						if isPunct(tokens[k], "(") {
							depth++
						} else if isPunct(tokens[k], ")") {
							depth--
							if depth == 0 {
								argClose = k
								break
							}
						}
					}
					if argClose < 0 {
						i++
						continue
					}
					openS, _ := tokRunes(tokens[i+2])
					_, closeE := tokRunes(tokens[argClose])
					inner := string(runes[openS+1 : closeE-1])
					argText = strings.TrimSpace(inner)
					argEnd = closeE
				}

				// Build replacement text per method.
				chainStartRune, _ := tokRunes(chain[0])
				_, methodEndRune := tokRunes(methodTok)
				replStart := chainStartRune
				replEnd := methodEndRune
				if hasArgs {
					replEnd = argEnd
				}
				var replText string
				switch method {
				case "COUNT":
					replText = "COALESCE(array_length(" + chStr + ", 1), 0)"
				case "FIRST":
					replText = "1"
				case "LAST":
					replText = "array_length(" + chStr + ", 1)"
				case "NEXT":
					if !hasArgs {
						i++
						continue
					}
					replText = "(" + argText + " + 1)"
				case "PRIOR":
					if !hasArgs {
						i++
						continue
					}
					replText = "(" + argText + " - 1)"
				case "EXISTS":
					if !hasArgs {
						i++
						continue
					}
					replText = "(" + chStr + "[" + argText + "] IS NOT NULL)"
				}
				if replText != "" {
					reps = append(reps, rep{
						startRune: replStart,
						endRune:   replEnd,
						text:      replText,
					})
					// Skip past the consumed tokens.
					if hasArgs {
						// jump to token AFTER the `)`
						jump := i + 2
						depth := 0
						for k := i + 2; k < len(tokens); k++ {
							if isPunct(tokens[k], "(") {
								depth++
							} else if isPunct(tokens[k], ")") {
								depth--
								if depth == 0 {
									jump = k + 1
									break
								}
							}
						}
						i = jump
					} else {
						i += 2
					}
					continue
				}
			}
		}
		i++
	}

	if len(reps) == 0 {
		return body
	}
	// Apply replacements left-to-right (they're already in source order).
	var b strings.Builder
	cursor := 0
	for _, r := range reps {
		if r.startRune < cursor {
			// overlapping rep — skip (defensive)
			continue
		}
		b.WriteString(string(runes[cursor:r.startRune]))
		b.WriteString(r.text)
		cursor = r.endRune
	}
	b.WriteString(string(runes[cursor:]))
	return b.String()
}

// lexOracleBody runs the Oracle lexer over body and returns the non-comment,
// non-EOF token stream. Errors from the lexer are tolerated — we return
// what we have so the caller can fall back gracefully.
func lexOracleBody(body string) []oracledialect.Token {
	l := oracledialect.NewLexer(body)
	var out []oracledialect.Token
	for {
		t := l.Next()
		if t.Kind == oracledialect.TOK_EOF {
			return out
		}
		if t.Kind == oracledialect.TOK_COMMENT {
			continue
		}
		out = append(out, t)
	}
}

func isWordToken(t oracledialect.Token, word string) bool {
	if t.Kind != oracledialect.TOK_KEYWORD &&
		t.Kind != oracledialect.TOK_IDENT {
		return false
	}
	return strings.EqualFold(t.Lit, word)
}

func isIdentToken(t oracledialect.Token) bool {
	return t.Kind == oracledialect.TOK_IDENT ||
		t.Kind == oracledialect.TOK_QUOTED_IDENT ||
		t.Kind == oracledialect.TOK_KEYWORD
}

func isPunct(t oracledialect.Token, lit string) bool {
	return t.Kind == oracledialect.TOK_PUNCT && t.Lit == lit
}

// ---------------------------------------------------------------------------
// O-37 — Oracle Spatial (SDO_*) function rewrites to PostGIS equivalents.
// ---------------------------------------------------------------------------

// sdoCallRewriter takes the raw inner-args text of a call and returns the
// FULL rewritten call (including the new function name and outer parens),
// plus an ok flag. False → leave the call untouched so PG surfaces the
// original error. The rewriter splits top-level commas itself via
// splitTopLevel (paren / quote aware).
type sdoCallRewriter func(rawArgs string) (rewrittenCall string, ok bool)

// sdoRewriteFor returns the rewriter for a recognised Oracle Spatial
// function call, or ok=false. Names are uppercased by the lexer; the
// fullName comes through as `SDO_FN` or `PKG.SDO_FN`.
func sdoRewriteFor(name string) (sdoCallRewriter, bool) {
	switch name {
	case "SDO_DISTANCE", "SDO_GEOM.SDO_DISTANCE":
		// SDO_DISTANCE(g1, g2 [, tol [, unit]]) → ST_Distance(g1, g2)
		return sdoFixedCall("ST_Distance", 2), true
	case "SDO_GEOM.SDO_AREA":
		return sdoFixedCall("ST_Area", 1), true
	case "SDO_GEOM.SDO_LENGTH":
		return sdoFixedCall("ST_Length", 1), true
	case "SDO_GEOM.SDO_INTERSECTION":
		return sdoFixedCall("ST_Intersection", 2), true
	case "SDO_GEOM.SDO_UNION":
		return sdoFixedCall("ST_Union", 2), true
	case "SDO_GEOM.SDO_DIFFERENCE":
		return sdoFixedCall("ST_Difference", 2), true
	case "SDO_RELATE":
		// Predicate selection depends on the `mask` arg.
		return sdoRelateCall, true
	case "SDO_WITHIN_DISTANCE":
		// `'distance=N'` parsing.
		return sdoWithinDistanceCall, true
	}
	return nil, false
}

// sdoFixedCall returns a rewriter that emits `<pgName>(<first N args>)`
// — drops the trailing tolerance/unit args Oracle Spatial accepts.
func sdoFixedCall(pgName string, n int) sdoCallRewriter {
	return func(raw string) (string, bool) {
		parts, err := splitTopLevel(raw, ',')
		if err != nil || len(parts) < n {
			return "", false
		}
		out := make([]string, n)
		for i := 0; i < n; i++ {
			out[i] = strings.TrimSpace(parts[i])
		}
		return pgName + "(" + strings.Join(out, ", ") + ")", true
	}
}

// sdoRelateCall maps SDO_RELATE → the appropriate ST_* predicate based
// on the `mask=` value in the third arg.
//
//	ANYINTERACT → ST_Intersects(g1, g2)
//	CONTAINS    → ST_Contains(g1, g2)
//	INSIDE      → ST_Within(g1, g2)
//	COVERS      → ST_Covers(g1, g2)
//	COVEREDBY   → ST_CoveredBy(g1, g2)
//	EQUAL       → ST_Equals(g1, g2)
//	TOUCH       → ST_Touches(g1, g2)
//	OVERLAPBDYINTERSECT, OVERLAPBDYDISJOINT → ST_Overlaps(g1, g2)
//
// Unknown masks return ok=false so the call is left as-is and PG surfaces
// the original SDO_RELATE error.
func sdoRelateCall(raw string) (string, bool) {
	parts, err := splitTopLevel(raw, ',')
	if err != nil || len(parts) < 3 {
		return "", false
	}
	g1 := strings.TrimSpace(parts[0])
	g2 := strings.TrimSpace(parts[1])
	maskExpr := strings.TrimSpace(parts[2])
	maskExpr = strings.Trim(maskExpr, "'\"")
	mask := strings.ToUpper(maskExpr)
	if idx := strings.Index(mask, "MASK="); idx >= 0 {
		mask = mask[idx+len("MASK="):]
	}
	mask = strings.TrimSpace(strings.SplitN(mask, " ", 2)[0])
	mask = strings.Trim(mask, "'\"")
	pred := ""
	switch mask {
	case "ANYINTERACT":
		pred = "ST_Intersects"
	case "CONTAINS":
		pred = "ST_Contains"
	case "INSIDE":
		pred = "ST_Within"
	case "COVERS":
		pred = "ST_Covers"
	case "COVEREDBY":
		pred = "ST_CoveredBy"
	case "EQUAL":
		pred = "ST_Equals"
	case "TOUCH":
		pred = "ST_Touches"
	case "OVERLAPBDYINTERSECT", "OVERLAPBDYDISJOINT":
		pred = "ST_Overlaps"
	}
	if pred == "" {
		return "", false
	}
	return pred + "(" + g1 + ", " + g2 + ")", true
}

// sdoWithinDistanceCall maps `SDO_WITHIN_DISTANCE(g1, g2, 'distance=N')`
// to PG's `ST_DWithin(g1, g2, N)`. The `unit=` modifier is dropped (PG
// uses the geometry's SRID-defined unit; for unit changes apply
// ST_Transform first).
func sdoWithinDistanceCall(raw string) (string, bool) {
	parts, err := splitTopLevel(raw, ',')
	if err != nil || len(parts) < 3 {
		return "", false
	}
	g1 := strings.TrimSpace(parts[0])
	g2 := strings.TrimSpace(parts[1])
	param := strings.TrimSpace(parts[2])
	param = strings.Trim(param, "'\"")
	dist := param
	if idx := strings.Index(strings.ToUpper(param), "DISTANCE="); idx >= 0 {
		dist = strings.TrimSpace(param[idx+len("DISTANCE="):])
		if sp := strings.Index(dist, " "); sp >= 0 {
			dist = dist[:sp]
		}
	}
	if dist == "" {
		return "", false
	}
	return "ST_DWithin(" + g1 + ", " + g2 + ", " + dist + ")", true
}
