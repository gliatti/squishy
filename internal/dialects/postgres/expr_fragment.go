package postgres

import (
	"fmt"
	"unicode/utf8"
)

// CheckExprFragment is the structural guard of a user-provided PostgreSQL
// expression that squishy embeds verbatim inside a parenthesised clause of
// the DDL it runs (today: a generation expression override, emitted as
// `GENERATED ALWAYS AS (<text>) STORED` and executed by create_ddl through
// the simple query protocol, which runs several statements).
//
// It is not a parser — PostgreSQL remains the judge of syntax, types and
// immutability — but a hand-rolled lexical scanner (the lexer exception
// of the repo's "No regex anywhere" rule: one pass, byte by byte, no
// regex, no substring search) that proves the text cannot leave the
// clause it is embedded in. It tracks single-quoted strings (plain and
// E / B / X / N / U& prefixed) and double-quoted identifiers, and rejects:
//
//   - an empty or blank expression;
//   - a ')' that closes below depth 0 (`'x') STORED); DROP …`);
//   - an unclosed '(' , string or quoted identifier;
//   - a ';' outside strings / quoted identifiers (a second statement);
//   - any comment, '--' or '/*' (a trailing `--` would swallow the
//     `) STORED` that follows the expression, and PostgreSQL nests block
//     comments where a naive scanner would not);
//   - any '$' outside strings / quoted identifiers: dollar-quoted strings
//     are refused rather than tracked, because whether `$tag$` opens one
//     depends on the preceding token (`a$$` is an identifier, `+$$` opens
//     a string, `1$$` depends on the version) and a scanner that misjudged it
//     would see a string where PostgreSQL sees code (write the literal as a
//     single-quoted string);
//     `$n` parameters are meaningless in DDL anyway;
//   - a backslash immediately followed by a quote inside a single-quoted
//     string: `\'` ends a plain string with standard_conforming_strings=on
//     but escapes the quote in an E-prefixed string or with the setting off,
//     so the scanner cannot know where the string ends (double the quote).
//     Any other backslash is consumed with the byte after it, which gives
//     the same string end in every quoting mode, so the E prefix does not
//     need to be recognised;
//   - a NUL byte or invalid UTF-8.
//
// A text that passes cannot close the enclosing parenthesis, start
// another statement, or hide the rest of the clause: the worst it can do
// is be an invalid or non-immutable expression, which PostgreSQL rejects
// when the table is created.
func CheckExprFragment(text string) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("the expression is not valid UTF-8")
	}
	depth := 0
	blank := true
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch c {
		case 0:
			return fmt.Errorf("the expression contains a NUL byte (offset %d)", i)
		case ' ', '\t', '\n', '\r', '\f', '\v':
			continue
		}
		blank = false
		switch c {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return fmt.Errorf("unbalanced ')' at offset %d: the expression would close the enclosing clause", i)
			}
			depth--
		case ';':
			return fmt.Errorf("';' at offset %d outside a string: the expression must be a single expression, not several statements", i)
		case '$':
			return fmt.Errorf("'$' at offset %d outside a string or quoted identifier: dollar-quoted strings and $n parameters are not allowed (write the literal with single quotes, quote an identifier containing $)", i)
		case '-':
			if i+1 < len(text) && text[i+1] == '-' {
				return fmt.Errorf("'--' comment at offset %d: comments are not allowed in the expression", i)
			}
		case '/':
			if i+1 < len(text) && text[i+1] == '*' {
				return fmt.Errorf("'/*' comment at offset %d: comments are not allowed in the expression", i)
			}
		case '\'':
			end, err := scanQuoted(text, i)
			if err != nil {
				return err
			}
			i = end
		case '"':
			end, err := scanQuotedIdent(text, i)
			if err != nil {
				return err
			}
			i = end
		}
	}
	if blank {
		return fmt.Errorf("the expression is empty")
	}
	if depth > 0 {
		return fmt.Errorf("%d unclosed '(' at the end of the expression", depth)
	}
	return nil
}

// scanQuoted returns the offset of the quote that closes the single-quoted
// string opened at text[start]. A doubled quote is embedded; a backslash
// consumes the byte after it, except a quote, which is refused (see
// CheckExprFragment).
func scanQuoted(text string, start int) (int, error) {
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case 0:
			return 0, fmt.Errorf("the expression contains a NUL byte (offset %d)", i)
		case '\\':
			if i+1 < len(text) && text[i+1] == '\'' {
				return 0, fmt.Errorf("\\' at offset %d inside a string: its meaning depends on the string prefix and standard_conforming_strings, write '' to embed a quote", i)
			}
			i++
		case '\'':
			if i+1 < len(text) && text[i+1] == '\'' {
				i++
				continue
			}
			return i, nil
		}
	}
	return 0, fmt.Errorf("unterminated string starting at offset %d", start)
}

// scanQuotedIdent returns the offset of the double quote that closes the
// quoted identifier opened at text[start] ("" is an embedded quote).
func scanQuotedIdent(text string, start int) (int, error) {
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case 0:
			return 0, fmt.Errorf("the expression contains a NUL byte (offset %d)", i)
		case '"':
			if i+1 < len(text) && text[i+1] == '"' {
				i++
				continue
			}
			return i, nil
		}
	}
	return 0, fmt.Errorf("unterminated quoted identifier starting at offset %d", start)
}
