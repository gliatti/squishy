package translate

import "strings"

// Helpers for byte-walking SQL source — used by routine_body and body_rewrite
// in lieu of regular expressions. These walks honour single-quoted SQL string
// literals (with the doubled-quote escape) and nested parentheses so they do
// not match constructs hiding inside literals or function calls.
//
// The MySQL/Oracle dialects share these primitives because the lexical rules
// for strings and parens are identical.
//
// SQL comments (`-- …` line, `/* … */` block) are skipped at byte-walk time —
// see skipSQLComment below. Without this, an apostrophe sitting inside a
// `--` comment (e.g. French `d'un`) would be treated as the opening quote
// of a string literal, latch `inStr=true` for the rest of the input, and
// silently disable every keyword/replace pass downstream. PRC_ANN_ABT
// landed in production with two `dbms_output.*` calls and a stray
// `ADD_MONTHS(SYSDATE,-3)` after such an apostrophe-bearing comment;
// `rewriteOraclePLpgSQLWithFlags` ran but every rewrite scanned past the
// match because the walker thought it was inside a string.

// skipSQLComment, when s[j] starts an SQL comment (`--` line or `/*` block),
// returns the offset of the byte just past the comment. Otherwise returns
// j unchanged. Block comments are NOT nested (PG/Oracle treat them flat at
// the lex level).
func skipSQLComment(s string, j int) int {
	if j >= len(s) {
		return j
	}
	// `-- …` line comment runs to end-of-line (or end of input).
	if s[j] == '-' && j+1 < len(s) && s[j+1] == '-' {
		k := j + 2
		for k < len(s) && s[k] != '\n' {
			k++
		}
		return k // sit on the '\n' (or len(s)) — caller's loop will step over it
	}
	// `/* … */` block comment.
	if s[j] == '/' && j+1 < len(s) && s[j+1] == '*' {
		k := j + 2
		for k+1 < len(s) {
			if s[k] == '*' && s[k+1] == '/' {
				return k + 2
			}
			k++
		}
		// Unterminated block comment — swallow the rest. Better than
		// running the rewrite on garbage.
		return len(s)
	}
	return j
}

// findKeywordCI returns the byte offset of the next whole-word, case-
// insensitive occurrence of kw in s starting from i, or -1 when there is no
// further match. Matches inside single-quoted strings are skipped.
//
// "Whole word" means: the byte before the match (if any) and the byte right
// after it (if any) are not identifier bytes (letter/digit/underscore). This
// reproduces the `\b` boundary semantics of regular expressions for ASCII
// identifiers — sufficient for SQL keywords.
func findKeywordCI(s string, i int, kw string) int {
	if kw == "" || i < 0 || i > len(s) {
		return -1
	}
	klen := len(kw)
	inStr := false
	j := i
	for j+klen <= len(s) {
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
		if c == '\'' {
			inStr = true
			j++
			continue
		}
		if !strings.EqualFold(s[j:j+klen], kw) {
			j++
			continue
		}
		if j > 0 && isIdentByte(s[j-1]) {
			j++
			continue
		}
		if j+klen < len(s) && isIdentByte(s[j+klen]) {
			j++
			continue
		}
		return j
	}
	return -1
}

// findStmtTerminator returns the byte offset of the next top-level ';' in s
// starting at off, ignoring semicolons inside parentheses or single-quoted
// strings. Returns len(s) when no terminator is present.
func findStmtTerminator(s string, off int) int {
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
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				return j
			}
		}
		j++
	}
	return len(s)
}

// findTopLevelEquals returns the offset of the next '=' (or ':=') at paren
// depth 0 outside string literals, starting at off. Returns -1 when the next
// such occurrence would lie past a top-level ';' (which signals "no equals
// in this statement"). The second return value is the operator length:
// 1 for '=' or 2 for ':='.
func findTopLevelEquals(s string, off int) (int, int) {
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
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				return -1, 0
			}
		case ':':
			if depth == 0 && j+1 < len(s) && s[j+1] == '=' {
				return j, 2
			}
		case '=':
			if depth == 0 {
				// distinguish '==' / '!=' / '<=' / '>=' — only bare '=' counts.
				if j > 0 {
					switch s[j-1] {
					case '!', '<', '>', '=':
						j++
						continue
					}
				}
				if j+1 < len(s) && s[j+1] == '=' {
					j++
					continue
				}
				return j, 1
			}
		}
		j++
	}
	return -1, 0
}

// skipWS advances off past ASCII whitespace and returns the new offset.
func skipWS(s string, off int) int {
	for off < len(s) {
		c := s[off]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			off++
			continue
		}
		break
	}
	return off
}

// rewindWS walks backward over ASCII whitespace, returning the new offset.
func rewindWS(s string, off int) int {
	for off > 0 {
		c := s[off-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			off--
			continue
		}
		break
	}
	return off
}

// startsKeywordCI reports whether s, starting at off, begins with a whole-word
// case-insensitive match for kw.
func startsKeywordCI(s string, off int, kw string) bool {
	klen := len(kw)
	if off+klen > len(s) {
		return false
	}
	if !strings.EqualFold(s[off:off+klen], kw) {
		return false
	}
	if off+klen < len(s) && isIdentByte(s[off+klen]) {
		return false
	}
	return true
}
