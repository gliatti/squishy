package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// RewriteRoutineBody translates a MySQL PL/SQL body (procedure / function /
// trigger) into PL/pgSQL. It walks the MySQL lexer's token stream and applies
// statement-level transforms that are structurally close between the two
// languages:
//
//	SET  x = y;              →  x := y;
//	WHILE cond DO ... END WHILE    →  WHILE cond LOOP ... END LOOP
//	REPEAT ... UNTIL c END REPEAT  →  LOOP ... EXIT WHEN c; END LOOP
//	LEAVE label                    →  EXIT label
//	ITERATE label                  →  CONTINUE label
//	CALL p(x);               →  CALL p(x);                 (PG 11+)
//	DECLARE HANDLER ...      →  best-effort EXCEPTION mapping
//	IFNULL / NOW / UUID / …  →  COALESCE / now / gen_random_uuid / …
//	JSON_EXTRACT(e,'$.a.b')  →  (e -> 'a' -> 'b')
//	JSON_UNQUOTE(JSON_EXTRACT(e,'$.a'))  →  (e ->> 'a')
//	`backticked`             →  "double-quoted"
//	<=>                      →  IS NOT DISTINCT FROM
//
// The result is returned as translated PL/pgSQL text. Caller wraps it in a
// CREATE FUNCTION/PROCEDURE/TRIGGER skeleton. Untranslatable constructs are
// left as-is; callers can diff the output vs. the original to confirm.
//
// It is a pragmatic rewriter, not a full parser. The grammar-v4
// MySqlParser.g4 file in dialects/mysql/reference/ is the canonical spec —
// whenever a new construct needs support we extend this function rather than
// adding ad-hoc regexes elsewhere.
func RewriteRoutineBody(body string) (translated string, untranslated []string) {
	if strings.TrimSpace(body) == "" {
		return "", nil
	}

	// Phase 1 — lexical normalization: backticks → double quotes, string
	// literals preserved, keyword-level keyword substitutions.
	text := rewriteMySQLBody(body) // already handles backticks, <=>, GROUP_CONCAT, fn renames.
	// Phase 2 — procedural statement-level transforms via a small state
	// machine over MySQL tokens.
	text = rewriteControlFlow(text)
	// Phase 3 — domain-specific rewrites requiring structural knowledge.
	text = rewriteJSONExtract(text)
	text = rewriteJSONUnquoteExtract(text)
	text = rewriteSetAssign(text)
	text = rewriteELT(text)
	text = rewriteHandlers(text)
	text = rewriteRoutineLabels(text)
	text = rewriteResignal(text)
	// Phase 4 — untranslated-construct detection.
	untranslated = detectUntranslated(text, body)
	return text, untranslated
}

// rewriteControlFlow rewrites MySQL loop/branch keywords. Token-aware so
// keywords inside string literals are untouched.
func rewriteControlFlow(src string) string {
	l := mysql.NewLexer(src)
	var out strings.Builder
	srcBytes := []byte(src)
	prevEnd := 0
	// Look-ahead buffer so we can replace multi-token patterns like
	// "WHILE ... DO" (→ "WHILE ... LOOP") or "END WHILE" (→ "END LOOP").
	for {
		tok := l.Next()
		if tok.Kind == mysql.TOK_EOF {
			if prevEnd < len(srcBytes) {
				out.WriteString(string(srcBytes[prevEnd:]))
			}
			break
		}
		if tok.Pos.Offset > prevEnd {
			out.WriteString(string(srcBytes[prevEnd:tok.Pos.Offset]))
		}
		tokLen := len(tok.Raw)
		if tokLen == 0 {
			tokLen = len(tok.Lit)
		}
		prevEnd = tok.Pos.Offset + tokLen

		switch {
		case tok.Kind == mysql.TOK_KEYWORD && tok.Lit == "DO":
			// DO after a WHILE/FOR is a loop opener; leave DO in an event
			// (DO stmt) alone by checking preceding context via heuristic
			// — we look at whether "LOOP" or procedural control is around.
			// Simplest: always translate DO→LOOP here, and let the rewriter
			// for events (translateEvent) build its own "DO $$" blocks
			// (they don't go through this function).
			out.WriteString("LOOP")
		case tok.Kind == mysql.TOK_KEYWORD && tok.Lit == "END":
			// Peek the next token to handle END WHILE / END REPEAT.
			next := l.Peek()
			if next.Kind == mysql.TOK_KEYWORD {
				switch next.Lit {
				case "WHILE":
					out.WriteString("END LOOP")
					// consume the WHILE
					adv := l.Next()
					prevEnd = adv.Pos.Offset + len(adv.Raw)
					continue
				case "REPEAT":
					out.WriteString("END LOOP")
					adv := l.Next()
					prevEnd = adv.Pos.Offset + len(adv.Raw)
					continue
				}
			}
			out.WriteString(tok.Raw)
		case tok.Kind == mysql.TOK_KEYWORD && tok.Lit == "LEAVE":
			out.WriteString("EXIT")
		case tok.Kind == mysql.TOK_KEYWORD && tok.Lit == "ITERATE":
			out.WriteString("CONTINUE")
		case tok.Kind == mysql.TOK_KEYWORD && tok.Lit == "REPEAT":
			// REPEAT ... UNTIL cond END REPEAT → LOOP ... EXIT WHEN cond; END LOOP
			// We can't easily restructure here; emit a marker that a second
			// pass handles. For now, translate REPEAT → LOOP and let the
			// UNTIL handler rewrite (see below) turn `UNTIL c` into
			// `EXIT WHEN c;`.
			out.WriteString("LOOP")
		case tok.Kind == mysql.TOK_KEYWORD && tok.Lit == "UNTIL":
			out.WriteString("EXIT WHEN")
		case tok.Kind == mysql.TOK_QUOTED_IDENT:
			out.WriteString(`"`)
			out.WriteString(strings.ReplaceAll(tok.Lit, `"`, `""`))
			out.WriteString(`"`)
		default:
			out.WriteString(string(srcBytes[tok.Pos.Offset:prevEnd]))
		}
	}
	return out.String()
}

// rewriteSetAssign translates procedural `SET <lvalue> = <rvalue>;` into
// PL/pgSQL `<lvalue> := <rvalue>;`. We match on the full statement span
// (SET ... ;) so nested `SET` keywords inside expressions are not touched.
// Note: session-level `SET NAMES 'utf8'` is not part of routine bodies, so
// we don't special-case it here.
//
// Implementation: byte-walk for whole-word `SET` (so OFFSET / RESET are
// untouched), capture the target up to a top-level `=`, then capture the
// rvalue up to the next top-level `;`. Single-quoted strings and nested
// parens are honoured by the helpers in body_walk.go. Replaces the previous
// regex `(?is)\bSET\s+...\s*=\s*([^;]+);`.
func rewriteSetAssign(s string) string {
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, "SET")
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		// Emit everything up to (and including) any leading whitespace; we
		// will rewrite the SET..; span by replacing it with `target := rvalue;`.
		out.WriteString(s[i:k])
		// `SET` followed by the target, then `=` at depth 0.
		afterSet := skipWS(s, k+3)
		eqOff, eqLen := findTopLevelEquals(s, afterSet)
		if eqOff < 0 {
			// No top-level '=' before ';' or EOF — emit the keyword verbatim
			// and continue scanning past it.
			out.WriteString(s[k : k+3])
			i = k + 3
			continue
		}
		target := strings.TrimSpace(s[afterSet:eqOff])
		valStart := skipWS(s, eqOff+eqLen)
		semi := findStmtTerminator(s, valStart)
		val := strings.TrimSpace(s[valStart:semi])
		out.WriteString(target)
		out.WriteString(" := ")
		out.WriteString(val)
		if semi < len(s) {
			out.WriteByte(';')
			i = semi + 1
		} else {
			i = semi
		}
	}
	return out.String()
}

// rewriteJSONExtract converts MySQL `JSON_EXTRACT(expr, '$.a.b')` (after
// function name rewrite landed it as `jsonb_extract_path(expr, '$.a.b')`)
// back into PG path operators: `(expr -> 'a' -> 'b')`. Also handles numeric
// array indices: `$[0]` → `-> 0`.
//
// Byte-walk: locate whole-word `jsonb_extract_path(`, find the matching ')',
// split arguments at the top level, validate that the second arg is a single
// quoted path literal, and emit the arrow chain. Replaces the previous
// regex (?is)\bjsonb_extract_path\s*\(\s*([^,]+?)\s*,\s*'([^']*)'\s*\).
func rewriteJSONExtract(s string) string {
	const fname = "jsonb_extract_path"
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		// Require an immediate '(' after the function name (whitespace-tolerant).
		j := skipWS(s, k+len(fname))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := matchParen(s, j)
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
		path := strings.TrimSpace(args[1])
		if len(path) < 2 || path[0] != '\'' || path[len(path)-1] != '\'' {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		expr := strings.TrimSpace(args[0])
		out.WriteString(s[i:k])
		out.WriteString("(")
		out.WriteString(expr)
		out.WriteString(mysqlJSONPathToPG(path[1 : len(path)-1]))
		out.WriteString(")")
		i = end + 1
	}
	return out.String()
}

// rewriteJSONUnquoteExtract rewrites the common combo
// `jsonb_extract_path_text(jsonb_extract_path(e,'$.a'))` to `(e ->> 'a')`.
// After rewriteJSONExtract runs, this becomes
// `jsonb_extract_path_text((e -> 'a'))` — we catch that shape too.
//
// Byte-walk: find whole-word `jsonb_extract_path_text(`, then look for the
// inner `(<expr> -> '<key>')` shape that rewriteJSONExtract leaves behind,
// and produce `(<expr> ->> '<key>')`. Replaces the previous regex.
func rewriteJSONUnquoteExtract(s string) string {
	const fname = "jsonb_extract_path_text"
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
		end, ok := matchParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		// inner is everything between the outer parens; we expect it to be
		// `(<expr> -> '<key>')` — a single nested paren group whose body
		// holds a `->` operator and a quoted key.
		inner := strings.TrimSpace(s[j+1 : end])
		expr, key, ok := splitArrowKey(inner)
		if !ok {
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		out.WriteString(s[i:k])
		out.WriteString("(")
		out.WriteString(strings.TrimSpace(expr))
		out.WriteString(" ->> '")
		out.WriteString(key)
		out.WriteString("')")
		i = end + 1
	}
	return out.String()
}

// splitArrowKey expects an input shaped like "(<expr> -> '<key>')" — a single
// outer paren wrapping an expression, a `->` operator, and a single-quoted
// key. Returns expr, key when the shape matches.
func splitArrowKey(inner string) (string, string, bool) {
	if len(inner) < 2 || inner[0] != '(' || inner[len(inner)-1] != ')' {
		return "", "", false
	}
	body := strings.TrimSpace(inner[1 : len(inner)-1])
	// Find ` -> ` (or `->`) at depth 0 outside string literals.
	depth := 0
	inStr := false
	arrowIdx := -1
	for j := 0; j < len(body); j++ {
		c := body[j]
		if inStr {
			if c == '\'' {
				if j+1 < len(body) && body[j+1] == '\'' {
					j++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case '-':
			if depth == 0 && j+1 < len(body) && body[j+1] == '>' && (j+2 >= len(body) || body[j+2] != '>') {
				arrowIdx = j
			}
		}
		if arrowIdx >= 0 {
			break
		}
	}
	if arrowIdx < 0 {
		return "", "", false
	}
	expr := strings.TrimSpace(body[:arrowIdx])
	rest := strings.TrimSpace(body[arrowIdx+2:])
	if len(rest) < 2 || rest[0] != '\'' || rest[len(rest)-1] != '\'' {
		return "", "", false
	}
	return expr, rest[1 : len(rest)-1], true
}

// mysqlJSONPathToPG converts a MySQL JSON path expression ('$.a.b[0]') to a
// sequence of PG `-> key` / `-> index` operators. Leading `$` is dropped.
func mysqlJSONPathToPG(path string) string {
	if !strings.HasPrefix(path, "$") {
		// unknown shape — treat the whole string as a single segment
		return " -> " + sqlString(path)
	}
	var out strings.Builder
	i := 1 // skip $
	for i < len(path) {
		c := path[i]
		switch c {
		case '.':
			// object key
			j := i + 1
			for j < len(path) && path[j] != '.' && path[j] != '[' {
				j++
			}
			out.WriteString(" -> ")
			out.WriteString(sqlString(path[i+1 : j]))
			i = j
		case '[':
			// array index — JSON path segments like $[0]
			j := strings.IndexByte(path[i:], ']')
			if j < 0 {
				return out.String() + " -> " + sqlString(path[i:])
			}
			idx := path[i+1 : i+j]
			out.WriteString(" -> ")
			out.WriteString(idx) // numeric literal — `-> 0`
			i = i + j + 1
		default:
			// bare key without leading dot (`$a`): treat rest as one key
			j := i
			for j < len(path) && path[j] != '.' && path[j] != '[' {
				j++
			}
			out.WriteString(" -> ")
			out.WriteString(sqlString(path[i:j]))
			i = j
		}
	}
	return out.String()
}

// rewriteELT converts MySQL's ELT(n, a, b, c, ...) (1-indexed pick) into a
// PG CASE expression. Stops at unmatched `)` using a paren-depth tracker.
func rewriteELT(s string) string {
	needle := "ELT("
	upper := strings.ToUpper(s)
	for i := 0; i < len(upper); {
		k := strings.Index(upper[i:], needle)
		if k < 0 {
			break
		}
		k += i
		// don't match identifiers that merely end in "ELT" (e.g. "DELT(")
		if k > 0 {
			prev := upper[k-1]
			if (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9') || prev == '_' {
				i = k + len(needle)
				continue
			}
		}
		open := k + len(needle) - 1
		close, ok := matchParen(s, open)
		if !ok {
			break
		}
		args := splitArgs(s[open+1 : close])
		if len(args) < 2 {
			i = close + 1
			continue
		}
		var b strings.Builder
		b.WriteString("CASE ")
		b.WriteString(strings.TrimSpace(args[0]))
		for idx, v := range args[1:] {
			b.WriteString(fmt.Sprintf(" WHEN %d THEN %s", idx+1, strings.TrimSpace(v)))
		}
		b.WriteString(" END")
		s = s[:k] + b.String() + s[close+1:]
		upper = strings.ToUpper(s)
		i = k + b.Len()
	}
	return s
}

// splitArgs splits a function-call argument list at top-level commas,
// respecting parentheses and string literals.
func splitArgs(body string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	inStr := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inStr {
			cur.WriteByte(c)
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					cur.WriteByte(body[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			cur.WriteByte(c)
			inStr = true
		case '(':
			depth++
			cur.WriteByte(c)
		case ')':
			depth--
			cur.WriteByte(c)
		case ',':
			if depth == 0 {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// rewriteHandlers converts the simple
//   DECLARE CONTINUE HANDLER FOR NOT FOUND <stmt>;
// pattern into a PG trick: declare a boolean, check FOUND after each FETCH.
// This covers the most common cursor idiom; complex handlers are left alone
// with a comment.
//
// Byte-walk: locate whole-word DECLARE, then validate the token sequence
// CONTINUE|EXIT, HANDLER, FOR, condition, statement up to ';'. Replaces the
// regex (?is)\bDECLARE\s+(CONTINUE|EXIT)\s+HANDLER\s+FOR\s+
//        (NOT\s+FOUND|SQLEXCEPTION|SQLWARNING)\s+([^;]+);
func rewriteHandlers(s string) string {
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, "DECLARE")
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		// Try to match the handler shape starting at k.
		end, kind, cond, stmt, ok := matchDeclareHandler(s, k)
		if !ok {
			out.WriteString(s[i : k+len("DECLARE")])
			i = k + len("DECLARE")
			continue
		}
		out.WriteString(s[i:k])
		condUpper := strings.ToUpper(cond)
		if condUpper == "NOT FOUND" {
			fmt.Fprintf(&out, "-- converted handler (NOT FOUND → FOUND-after-FETCH pattern): %s %s\n",
				kind, strings.TrimSpace(stmt))
		} else {
			fmt.Fprintf(&out, "-- TODO: wrap containing block in BEGIN ... EXCEPTION WHEN %s THEN %s; END;",
				condUpper, strings.TrimSpace(stmt))
		}
		i = end
	}
	return out.String()
}

// matchDeclareHandler attempts to recognize a DECLARE HANDLER statement
// starting at offset `start` (which must hold the keyword DECLARE). On
// success it returns the byte offset after the terminating ';' along with
// the captured kind ("CONTINUE"|"EXIT"), condition text, and action
// statement text.
func matchDeclareHandler(s string, start int) (end int, kind, cond, stmt string, ok bool) {
	p := skipWS(s, start+len("DECLARE"))
	switch {
	case startsKeywordCI(s, p, "CONTINUE"):
		kind = "CONTINUE"
		p += len("CONTINUE")
	case startsKeywordCI(s, p, "EXIT"):
		kind = "EXIT"
		p += len("EXIT")
	default:
		return 0, "", "", "", false
	}
	p = skipWS(s, p)
	if !startsKeywordCI(s, p, "HANDLER") {
		return 0, "", "", "", false
	}
	p += len("HANDLER")
	p = skipWS(s, p)
	if !startsKeywordCI(s, p, "FOR") {
		return 0, "", "", "", false
	}
	p += len("FOR")
	p = skipWS(s, p)
	condStart := p
	switch {
	case startsKeywordCI(s, p, "NOT"):
		p += len("NOT")
		p = skipWS(s, p)
		if !startsKeywordCI(s, p, "FOUND") {
			return 0, "", "", "", false
		}
		p += len("FOUND")
		cond = "NOT FOUND"
	case startsKeywordCI(s, p, "SQLEXCEPTION"):
		p += len("SQLEXCEPTION")
		cond = "SQLEXCEPTION"
	case startsKeywordCI(s, p, "SQLWARNING"):
		p += len("SQLWARNING")
		cond = "SQLWARNING"
	default:
		return 0, "", "", "", false
	}
	_ = condStart
	stmtStart := skipWS(s, p)
	semi := findStmtTerminator(s, stmtStart)
	if semi >= len(s) {
		return 0, "", "", "", false
	}
	stmt = s[stmtStart:semi]
	end = semi + 1
	ok = true
	return
}

// rewriteRoutineLabels rewrites MySQL-style block labels into PG plpgsql's
// `<<label>>` form:
//
//	  loop1: WHILE cond DO …  →  <<loop1>> WHILE cond LOOP …
//	  block1: BEGIN … END block1; → <<block1>> BEGIN … END block1;
//	  ret: REPEAT … END REPEAT ret; → <<ret>> LOOP … END LOOP ret;
//
// Note: rewriteControlFlow has already turned WHILE/REPEAT/END WHILE/END
// REPEAT into their PG forms by the time we run, so we only have to
// recognise `<ident>:` before BEGIN/LOOP/WHILE and add the angle brackets.
// We deliberately don't touch CASE / handler labels.
//
// Byte-walk replacement for the regex
//   (?i)(^|\n|\r|;)\s*([A-Za-z_][A-Za-z0-9_]*)\s*:\s+(BEGIN|LOOP|WHILE)\b
// — at each statement-boundary byte (start of input, '\n', '\r', ';') we
// scan forward for `<ident> :` and, when followed by one of the three
// loop-opener keywords, splice in `<<ident>> `.
func rewriteRoutineLabels(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		// Determine whether we are at a statement-boundary byte.
		atBoundary := i == 0 || s[i-1] == '\n' || s[i-1] == '\r' || s[i-1] == ';'
		if !atBoundary {
			out.WriteByte(s[i])
			i++
			continue
		}
		// Skip whitespace after the boundary, then try to match an identifier.
		j := skipWS(s, i)
		if j >= len(s) || !isIdentStart(s[j]) {
			out.WriteByte(s[i])
			i++
			continue
		}
		identStart := j
		for j < len(s) && isIdentByte(s[j]) {
			j++
		}
		identEnd := j
		// optional whitespace then ':'
		j = skipWS(s, j)
		if j >= len(s) || s[j] != ':' {
			out.WriteByte(s[i])
			i++
			continue
		}
		// Require at least one whitespace after ':' (the regex used \s+).
		colon := j
		j++
		ws := skipWS(s, j)
		if ws == j {
			out.WriteByte(s[i])
			i++
			continue
		}
		// Match BEGIN | LOOP | WHILE as a whole word.
		var kw string
		switch {
		case startsKeywordCI(s, ws, "BEGIN"):
			kw = "BEGIN"
		case startsKeywordCI(s, ws, "LOOP"):
			kw = "LOOP"
		case startsKeywordCI(s, ws, "WHILE"):
			kw = "WHILE"
		default:
			out.WriteByte(s[i])
			i++
			continue
		}
		// Emit boundary + leading whitespace, then `<<label>> KW`, and skip
		// past the original `label : <ws> KW` span.
		boundaryEnd := identStart // already includes the leading WS run
		out.WriteString(s[i:boundaryEnd])
		out.WriteByte(' ')
		out.WriteString("<<")
		out.WriteString(s[identStart:identEnd])
		out.WriteString(">> ")
		out.WriteString(kw)
		i = ws + len(kw)
		_ = colon
	}
	return out.String()
}

// rewriteResignal maps MySQL/MariaDB's `RESIGNAL;` and `RESIGNAL SQLSTATE
// '...';` (re-raise the current exception, optionally overriding the
// SQLSTATE) onto PG plpgsql's `RAISE;` / `RAISE EXCEPTION SQLSTATE '...';`.
//
// The bare form maps cleanly: PG's `RAISE;` re-raises the current exception
// only inside an EXCEPTION block, mirroring MySQL's restriction on RESIGNAL.
// For the SQLSTATE-overriding form we emit `RAISE EXCEPTION SQLSTATE '<sqlstate>';`
// which is the equivalent PG idiom (the original exception details are
// dropped because PG can't carry them across a custom raise).
//
// Byte-walk replacement for the two regexes
//   (?is)\bRESIGNAL\s+SQLSTATE\s+(VALUE\s+)?'([^']+)'\s*;
//   (?i)\bRESIGNAL\s*;
func rewriteResignal(s string) string {
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, "RESIGNAL")
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		out.WriteString(s[i:k])
		// Skip whitespace after RESIGNAL.
		p := skipWS(s, k+len("RESIGNAL"))
		if p < len(s) && s[p] == ';' {
			out.WriteString("RAISE;")
			i = p + 1
			continue
		}
		if startsKeywordCI(s, p, "SQLSTATE") {
			p += len("SQLSTATE")
			p = skipWS(s, p)
			if startsKeywordCI(s, p, "VALUE") {
				p += len("VALUE")
				p = skipWS(s, p)
			}
			if p < len(s) && s[p] == '\'' {
				// find closing quote (no escape handling — original regex used
				// [^']+ which also forbids embedded quotes)
				closeQ := strings.IndexByte(s[p+1:], '\'')
				if closeQ >= 0 {
					sqlstate := s[p+1 : p+1+closeQ]
					p = p + 1 + closeQ + 1 // past the closing quote
					p = skipWS(s, p)
					if p < len(s) && s[p] == ';' {
						out.WriteString("RAISE EXCEPTION SQLSTATE '")
						out.WriteString(sqlstate)
						out.WriteString("';")
						i = p + 1
						continue
					}
				}
			}
		}
		// Not a recognized RESIGNAL form — emit the keyword verbatim and
		// continue scanning past it.
		out.WriteString(s[k : k+len("RESIGNAL")])
		i = k + len("RESIGNAL")
	}
	return out.String()
}

// detectUntranslated scans the rewritten text for MySQL constructs we did
// NOT know how to translate. Returns a list of short labels suitable for
// surfacing in the checklist. Empty slice = full auto-translation.
//
// Byte-walk: every probe is implemented by findKeywordCI (whole-word, string-
// literal aware) plus a small post-check for the constructs that need a
// follow-on shape (function call, identifier after EXECUTE, etc.). Replaces
// six regular expressions.
func detectUntranslated(rewritten, original string) []string {
	var found []string
	addIf := func(label string, present bool) {
		if present {
			found = append(found, label)
		}
	}
	addIf("DATE_FORMAT (format codes differ from PG's to_char)",
		hasFuncCall(rewritten, "DATE_FORMAT"))
	addIf("STR_TO_DATE (format codes differ from PG's to_date)",
		hasFuncCall(rewritten, "STR_TO_DATE"))
	addIf("GROUP_CONCAT with ORDER BY / DISTINCT (pg string_agg syntax differs)",
		hasFuncCall(rewritten, "GROUP_CONCAT"))
	addIf("Dynamic SQL (PREPARE/EXECUTE)",
		hasDynamicSQL(rewritten))
	addIf("session variables (@var)",
		hasSessionVar(rewritten))
	addIf("SIGNAL SQLSTATE (custom error raising)",
		hasSignalSqlstate(rewritten))
	return found
}

// hasFuncCall reports whether rewritten contains a whole-word case-
// insensitive occurrence of name immediately followed (after optional
// whitespace) by '('. Mirrors `(?i)\bNAME\s*\(`.
func hasFuncCall(s, name string) bool {
	for i := 0; ; {
		k := findKeywordCI(s, i, name)
		if k < 0 {
			return false
		}
		j := skipWS(s, k+len(name))
		if j < len(s) && s[j] == '(' {
			return true
		}
		i = k + len(name)
	}
}

// hasDynamicSQL mirrors `(?i)\bPREPARE\b|\bEXECUTE\b\s+[A-Za-z_]` — true when
// PREPARE is present anywhere as a whole word, or EXECUTE is followed (after
// whitespace) by an identifier byte.
func hasDynamicSQL(s string) bool {
	if findKeywordCI(s, 0, "PREPARE") >= 0 {
		return true
	}
	for i := 0; ; {
		k := findKeywordCI(s, i, "EXECUTE")
		if k < 0 {
			return false
		}
		j := skipWS(s, k+len("EXECUTE"))
		if j < len(s) && isIdentStart(s[j]) {
			return true
		}
		i = k + len("EXECUTE")
	}
}

// hasSessionVar mirrors `@[A-Za-z_][A-Za-z0-9_]*`. Walks bytes, ignoring
// matches inside single-quoted strings.
func hasSessionVar(s string) bool {
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
		if c == '@' && i+1 < len(s) && isIdentStart(s[i+1]) {
			return true
		}
	}
	return false
}

// hasSignalSqlstate mirrors `(?i)\bSIGNAL\s+SQLSTATE\b`.
func hasSignalSqlstate(s string) bool {
	for i := 0; ; {
		k := findKeywordCI(s, i, "SIGNAL")
		if k < 0 {
			return false
		}
		j := skipWS(s, k+len("SIGNAL"))
		if startsKeywordCI(s, j, "SQLSTATE") {
			return true
		}
		i = k + len("SIGNAL")
	}
}
