package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// rewriteMySQLBody converts a MySQL DML fragment (a view's SELECT body, a
// routine body) into a PostgreSQL-compatible fragment by walking tokens from
// the MySQL lexer and re-emitting them in the shape the vendored PostgreSQL
// grammar (reference/PostgreSQLParser.g4) accepts.
//
// This is NOT a full DML translator — a proper MySQL SELECT parser is out of
// scope for v1. But by tokenizing, we respect string/identifier/comment
// boundaries (no more false positives from regex on content inside literals)
// and map MySQL productions to their PG counterparts by token kind:
//
//   - TOK_QUOTED_IDENT  (`foo`)          → "foo"                          (PG delimited_identifier)
//   - TOK_KEYWORD       (MySQL-specific) → PG equivalent where known
//   - TOK_IDENT         (function name)  → PG function name when mapped
//   - TOK_STRING/NUMBER/...              → verbatim
//   - operator  "<=>"                    → " IS NOT DISTINCT FROM "
//
// Constructs not covered remain untouched — that's intentional, so reviewers
// see where manual work is needed rather than getting silently broken SQL.
func rewriteMySQLBody(body string) string {
	if strings.TrimSpace(body) == "" {
		return body
	}

	l := mysql.NewLexer(body)
	var out strings.Builder
	prevEnd := 0 // RUNE offset just past the last emitted token
	// The MySQL lexer indexes its source as `[]rune`, so `tok.Pos.Offset`
	// is a rune offset, not a byte offset. Slicing a `[]byte(body)` with
	// rune offsets shifts every span past the first non-ASCII character
	// (`Ú` in Latin-1-tinted dumps becomes 2 bytes / 1 rune, etc.) and the
	// emitted text shows up scrambled — letters from the next word leak
	// in, parens duplicate, the surrounding routine fails to compile.
	// Stay in runes throughout to keep the indices coherent.
	src := []rune(body)

	emitRaw := func(s string) { out.WriteString(s) }

	for {
		tok := l.Next()
		if tok.Kind == mysql.TOK_EOF {
			// emit any trailing whitespace/comment that followed the last token
			if prevEnd < len(src) {
				emitRaw(string(src[prevEnd:]))
			}
			break
		}
		// Preserve whitespace/comments between tokens by copying the raw
		// source slice before the token start.
		if tok.Pos.Offset > prevEnd && tok.Pos.Offset <= len(src) {
			emitRaw(string(src[prevEnd:tok.Pos.Offset]))
		}
		tokEnd := tok.Pos.Offset + tokenSourceLen(tok, src)
		// Defensive: a token's reported Raw/Lit length can overshoot the
		// source when the lexer normalizes content (e.g. adds escapes).
		// Clamping to len(src) keeps the slice valid; the worst case is a
		// truncated token in the output, but the alternative is a hard
		// panic that aborts the entire plan.
		if tokEnd > len(src) {
			tokEnd = len(src)
		}
		if tok.Pos.Offset > len(src) {
			tok.Pos.Offset = len(src)
		}

		switch tok.Kind {
		case mysql.TOK_QUOTED_IDENT:
			emitRaw(`"` + strings.ReplaceAll(tok.Lit, `"`, `""`) + `"`)
		case mysql.TOK_COMMENT:
			emitRaw(string(src[tok.Pos.Offset:tokEnd]))
		case mysql.TOK_STRING:
			emitRaw(string(src[tok.Pos.Offset:tokEnd]))
		case mysql.TOK_KEYWORD:
			emitRaw(mapKeywordOrFunc(tok.Lit, tok.Raw))
		case mysql.TOK_IDENT:
			// Function-style identifiers get mapped; plain identifiers pass
			// through unchanged.
			emitRaw(mapFuncName(tok.Lit))
		case mysql.TOK_PUNCT:
			if tok.Lit == "<=>" {
				emitRaw(" IS NOT DISTINCT FROM ")
			} else {
				emitRaw(tok.Lit)
			}
		default:
			emitRaw(string(src[tok.Pos.Offset:tokEnd]))
		}

		prevEnd = tokEnd
	}

	// Post-pass: GROUP_CONCAT(expr SEPARATOR 'x') and jsonb_extract_path(col,
	// '$.a.b') need structural rewrites that can't be done at token level
	// without a parser. Handle them textually on the post-token output — at
	// this stage backticks are already gone and our matches are unambiguous.
	s := rewriteGroupConcat(out.String())
	s = rewriteJSONExtractPath(s)
	s = rewriteBareJoinAsCross(s)
	return s
}

// rewriteBareJoinAsCross rewrites the mysqldump view pattern
//
//	FROM ((((`a` join `b`) join `c`) join `d`) ...)
//
// into a PG-acceptable form. Bare `JOIN` without an ON/USING clause is legal
// in MySQL (it's an implicit CROSS JOIN whose conditions normally live in
// WHERE) but PostgreSQL's INNER/JOIN requires an ON or USING clause and would
// raise "syntax error at or near …". We only rewrite a bare JOIN when no
// ON/USING token follows the immediate next table reference — in that case
// the join is unambiguously a cross-product, and emitting CROSS JOIN
// preserves the semantics under the existing WHERE-based filter.
func rewriteBareJoinAsCross(s string) string {
	if s == "" {
		return s
	}
	// Quick reject if there's no JOIN keyword anywhere — saves a tokenize
	// pass on the common case.
	if !containsKeywordCI(s, "JOIN") {
		return s
	}
	l := mysql.NewLexer(s)
	type tokSpan struct {
		kind        mysql.TokenKind
		litUpper    string
		startOffset int
		endOffset   int
	}
	var toks []tokSpan
	// Lexer offsets are RUNE indices (Lexer.src is []rune); keep the
	// downstream slicing in runes too so we don't shift past UTF-8.
	src := []rune(s)
	for {
		t := l.Next()
		if t.Kind == mysql.TOK_EOF {
			break
		}
		end := t.Pos.Offset + tokenSourceLen(t, src)
		var up string
		if t.Kind == mysql.TOK_KEYWORD || t.Kind == mysql.TOK_IDENT {
			up = strings.ToUpper(t.Lit)
		}
		toks = append(toks, tokSpan{kind: t.Kind, litUpper: up, startOffset: t.Pos.Offset, endOffset: end})
	}
	// Collect (start,end) byte ranges of JOIN tokens to rewrite, then patch
	// from the back so earlier offsets stay valid.
	type patch struct{ start, end int }
	var patches []patch
	prevQualifier := func(idx int) string {
		for j := idx - 1; j >= 0; j-- {
			tk := toks[j]
			if tk.kind == mysql.TOK_COMMENT {
				continue
			}
			return tk.litUpper
		}
		return ""
	}
	hasOnOrUsing := func(start int) bool {
		// Walk forward from start (the JOIN token index+1) past the next
		// table reference and any (alias) parenthesized list, stopping at
		// the next JOIN-related keyword or punctuation that ends the join
		// element. If we find ON or USING before that, the join already has
		// a qualifier.
		depth := 0
		for j := start; j < len(toks); j++ {
			tk := toks[j]
			switch tk.kind {
			case mysql.TOK_PUNCT:
				if string(src[tk.startOffset:tk.endOffset]) == "(" {
					depth++
					continue
				}
				if string(src[tk.startOffset:tk.endOffset]) == ")" {
					depth--
					if depth < 0 {
						return false
					}
					continue
				}
				if depth == 0 && string(src[tk.startOffset:tk.endOffset]) == "," {
					return false
				}
			case mysql.TOK_KEYWORD:
				if depth > 0 {
					continue
				}
				switch tk.litUpper {
				case "ON", "USING":
					return true
				case "JOIN", "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "WINDOW", "UNION", "INTERSECT", "EXCEPT", "MINUS", "FETCH":
					return false
				}
			}
		}
		return false
	}
	for i, tk := range toks {
		if tk.kind != mysql.TOK_KEYWORD || tk.litUpper != "JOIN" {
			continue
		}
		switch prevQualifier(i) {
		case "CROSS", "INNER", "LEFT", "RIGHT", "FULL", "OUTER", "NATURAL", "STRAIGHT_JOIN":
			continue
		}
		if hasOnOrUsing(i + 1) {
			continue
		}
		patches = append(patches, patch{start: tk.startOffset, end: tk.endOffset})
	}
	if len(patches) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(patches)*len("CROSS "))
	last := 0
	for _, p := range patches {
		b.WriteString(string(src[last:p.start]))
		b.WriteString("CROSS JOIN")
		last = p.end
	}
	b.WriteString(string(src[last:]))
	return b.String()
}

// containsKeywordCI returns true when `kw` appears as a whole word in `s`,
// case-insensitive.
func containsKeywordCI(s, kw string) bool {
	upper := strings.ToUpper(s)
	kw = strings.ToUpper(kw)
	for i := 0; ; {
		k := strings.Index(upper[i:], kw)
		if k < 0 {
			return false
		}
		k += i
		before := k == 0 || !isIdentByte(upper[k-1])
		after := k+len(kw) == len(upper) || !isIdentByte(upper[k+len(kw)])
		if before && after {
			return true
		}
		i = k + len(kw)
	}
}

// rewriteJSONExtractPath rewrites the output of the token pass, where every
// MySQL JSON_EXTRACT / JSON_UNQUOTE has already been renamed to its PG
// counterpart (jsonb_extract_path / jsonb_extract_path_text), but the second
// argument is still a MySQL path string of the form '$.a.b.c'. PG expects a
// VARIADIC list of text keys, so we split the path into separate args. Paths
// using array indices ([0]), wildcards (*), or anything beyond plain
// "$.segment(.segment)*" are left alone — they require manual review.
func rewriteJSONExtractPath(s string) string {
	upper := strings.ToUpper(s)
	for _, fname := range []string{"jsonb_extract_path_text", "jsonb_extract_path"} {
		needle := strings.ToUpper(fname) + "("
		for i := 0; i < len(upper); {
			k := strings.Index(upper[i:], needle)
			if k < 0 {
				break
			}
			k += i
			// Require a non-identifier character before the match, so we don't
			// snag substrings like "my_jsonb_extract_path(".
			if k > 0 && isIdentByte(s[k-1]) {
				i = k + len(needle)
				continue
			}
			openIdx := k + len(needle) - 1
			end, ok := matchParen(s, openIdx)
			if !ok {
				i = k + len(needle)
				continue
			}
			inner := s[openIdx+1 : end]
			args := splitTopLevelArgs(inner)
			if len(args) != 2 {
				i = k + len(needle)
				continue
			}
			pathArg := strings.TrimSpace(args[1])
			if len(pathArg) < 4 || !strings.HasPrefix(pathArg, "'$.") || !strings.HasSuffix(pathArg, "'") {
				i = k + len(needle)
				continue
			}
			pathContent := pathArg[3 : len(pathArg)-1]
			if strings.ContainsAny(pathContent, "[]*?'") {
				i = k + len(needle)
				continue
			}
			segments := strings.Split(pathContent, ".")
			quoted := make([]string, 0, len(segments))
			valid := true
			for _, seg := range segments {
				if seg == "" {
					valid = false
					break
				}
				quoted = append(quoted, "'"+seg+"'")
			}
			if !valid {
				i = k + len(needle)
				continue
			}
			repl := fname + "(" + strings.TrimSpace(args[0]) + ", " + strings.Join(quoted, ", ") + ")"
			s = s[:k] + repl + s[end+1:]
			upper = strings.ToUpper(s)
			i = k + len(repl)
		}
	}
	return s
}

// splitTopLevelArgs splits a comma-separated argument list at commas that are
// outside of any nested parens and outside of single-quoted strings.
func splitTopLevelArgs(s string) []string {
	var out []string
	depth := 0
	inStr := false
	start := 0
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
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// tokenSourceLen returns the length (in bytes) of a token's original source
// slice, inferred from its Raw field when available, or falling back to the
// literal length.
func tokenSourceLen(tok mysql.Token, src []rune) int {
	// Caller's offsets are RUNE-indexed (lexer source is []rune), so the
	// returned length must also be a rune count. Using `len(string)` would
	// double-count UTF-8 bytes for non-ASCII tokens.
	if tok.Raw != "" {
		return len([]rune(tok.Raw))
	}
	// Last-resort: 1 rune (caller will not advance past end-of-stream anyway).
	return len([]rune(tok.Lit))
}

// mapKeywordOrFunc handles keywords that either participate as function names
// (CURRENT_TIMESTAMP, NOW, UUID) or stand on their own. For non-function
// keywords we emit the original raw text to preserve case.
func mapKeywordOrFunc(upper, raw string) string {
	switch upper {
	case "CURRENT_TIMESTAMP", "NOW", "LOCALTIMESTAMP":
		return "now"
	case "UUID":
		return "gen_random_uuid"
	}
	return raw
}

// mapFuncName returns the PG equivalent for known MySQL function identifiers.
// Applied at token level, so it only matches when the token is an identifier
// (no false matches inside strings or column names).
func mapFuncName(name string) string {
	switch strings.ToUpper(name) {
	case "IFNULL":
		return "COALESCE"
	case "CHAR_LENGTH", "CHARACTER_LENGTH":
		return "length"
	case "JSON_EXTRACT":
		return "jsonb_extract_path"
	case "JSON_UNQUOTE":
		return "jsonb_extract_path_text"
	case "CONCAT":
		return "concat"
	case "TRUNC":
		return "trunc"
	}
	return name
}

// rewriteMySQLInterval converts MySQL's INTERVAL syntax (unquoted unit:
// `INTERVAL 1 HOUR`) to PG's form (quoted: `INTERVAL '1 hour'`). Idempotent
// when the input already uses PG syntax — the second pass sees the value in
// quotes and walks past it.
//
// Token-driven: we run the MySQL lexer and look for the sequence
//   KW(INTERVAL) NUMBER KW(unit-keyword)
// where unit-keyword ∈ {SECOND,MINUTE,HOUR,DAY,WEEK,MONTH,QUARTER,YEAR},
// optionally followed by a trailing `S` ident (the regex tolerated plural
// forms such as `INTERVAL 1 HOURS`). String/comment boundaries are honoured
// implicitly because the lexer skips them.
func rewriteMySQLInterval(expr string) string {
	if !strings.Contains(strings.ToUpper(expr), "INTERVAL") {
		return expr
	}
	l := mysql.NewLexer(expr)
	// Lexer.src is []rune — keep the slice and the offsets in the same
	// space to avoid byte-vs-rune drift on UTF-8 input.
	src := []rune(expr)
	var out strings.Builder
	prevEnd := 0
	for {
		tok := l.Next()
		if tok.Kind == mysql.TOK_EOF {
			if prevEnd < len(src) {
				out.WriteString(string(src[prevEnd:]))
			}
			break
		}
		if tok.Pos.Offset > prevEnd {
			out.WriteString(string(src[prevEnd:tok.Pos.Offset]))
		}
		tokLen := tokenSourceLen(tok, src)
		tokEnd := tok.Pos.Offset + tokLen

		if tok.Kind == mysql.TOK_KEYWORD && tok.Lit == "INTERVAL" {
			// Try to consume "<NUMBER> <unit>[S]"
			n := l.Peek()
			if n.Kind == mysql.TOK_NUMBER {
				_ = l.Next() // consume the NUMBER
				u := l.Peek()
				if u.Kind == mysql.TOK_KEYWORD && intervalUnit(u.Lit) {
					_ = l.Next() // consume the unit
					unit := strings.ToLower(u.Lit)
					trailingEnd := u.Pos.Offset + tokenSourceLen(u, src)
					// Optional trailing `S` (HOURS, DAYS, …) tokenised as IDENT.
					trailing := l.Peek()
					if trailing.Kind == mysql.TOK_IDENT && strings.EqualFold(trailing.Lit, "S") {
						_ = l.Next()
						trailingEnd = trailing.Pos.Offset + tokenSourceLen(trailing, src)
					}
					out.WriteString("INTERVAL '")
					out.WriteString(n.Lit)
					out.WriteString(" ")
					out.WriteString(unit)
					out.WriteString("'")
					prevEnd = trailingEnd
					continue
				}
			}
		}
		out.WriteString(string(src[tok.Pos.Offset:tokEnd]))
		prevEnd = tokEnd
	}
	return out.String()
}

func intervalUnit(kw string) bool {
	switch kw {
	case "SECOND", "MINUTE", "HOUR", "DAY", "WEEK", "MONTH", "QUARTER", "YEAR":
		return true
	}
	return false
}

// rewriteGroupConcat rewrites the PG-invalid GROUP_CONCAT construct into
// string_agg. It is a structural transform — we need to look past the
// function boundary and its optional SEPARATOR clause — so we keep it as a
// regex over the post-token text (which at this point already has PG-style
// identifiers).
func rewriteGroupConcat(s string) string {
	upper := strings.ToUpper(s)
	const needle = "GROUP_CONCAT("
	for i := 0; i < len(upper); {
		k := strings.Index(upper[i:], needle)
		if k < 0 {
			break
		}
		k += i
		// Find the matching closing parenthesis, respecting nesting and strings.
		end, ok := matchParen(s, k+len(needle)-1)
		if !ok {
			i = k + len(needle)
			continue
		}
		inner := s[k+len(needle) : end]
		expr, sep := splitGroupConcat(inner)
		repl := "string_agg((" + strings.TrimSpace(expr) + ")::text, " + sep + ")"
		s = s[:k] + repl + s[end+1:]
		upper = strings.ToUpper(s)
		i = k + len(repl)
	}
	return s
}

// matchParen returns the index of the ')' matching the '(' at position open.
func matchParen(s string, open int) (int, bool) {
	depth := 0
	inStr := false
	for i := open; i < len(s); i++ {
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
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return -1, false
}

// splitGroupConcat isolates the aggregated expression and the optional
// SEPARATOR literal from the body of a GROUP_CONCAT() call. Case-insensitive
// match on SEPARATOR, respecting string literals.
func splitGroupConcat(body string) (expr, sep string) {
	upper := strings.ToUpper(body)
	const kw = " SEPARATOR "
	// Find the keyword SEPARATOR outside of any string literal.
	inStr := false
	for i := 0; i+len(kw) <= len(upper); i++ {
		if !inStr && upper[i] == '\'' {
			inStr = true
			continue
		}
		if inStr {
			if upper[i] == '\'' {
				if i+1 < len(upper) && upper[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if upper[i:i+len(kw)] == kw {
			return body[:i], strings.TrimSpace(body[i+len(kw):])
		}
	}
	return body, "','"
}
