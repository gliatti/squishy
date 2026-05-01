package mysql

import (
	"strings"
	"unicode"
)

// Lexer scans raw MySQL source text into a stream of tokens.
// It is deliberately minimal: it is used for DDL + routine headers, and to
// aggregate raw_block content. Client DELIMITER directives are handled by
// tracking the statement separator (default ';').
type Lexer struct {
	src       []rune
	off       int // rune offset
	line, col int

	delim string // current statement delimiter, default ";"
	// peeked token (for 1-lookahead)
	peeked *Token
}

func NewLexer(src string) *Lexer {
	return &Lexer{src: []rune(src), off: 0, line: 1, col: 1, delim: ";"}
}

// Delimiter returns the current client-side statement delimiter.
func (l *Lexer) Delimiter() string { return l.delim }

// SetDelimiter replaces the statement delimiter. Called by the parser
// upon encountering a DELIMITER_CMD token.
func (l *Lexer) SetDelimiter(d string) {
	if d == "" {
		d = ";"
	}
	l.delim = d
}

func (l *Lexer) pos() Position { return Position{Line: l.line, Col: l.col, Offset: l.off} }

func (l *Lexer) peekR() rune {
	if l.off >= len(l.src) {
		return 0
	}
	return l.src[l.off]
}

func (l *Lexer) peekR2() rune {
	if l.off+1 >= len(l.src) {
		return 0
	}
	return l.src[l.off+1]
}

func (l *Lexer) advance() rune {
	if l.off >= len(l.src) {
		return 0
	}
	r := l.src[l.off]
	l.off++
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *Lexer) skipWhitespace() {
	for l.off < len(l.src) {
		r := l.peekR()
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			l.advance()
			continue
		}
		break
	}
}

// Next returns the next token, or EOF at end of input.
func (l *Lexer) Next() Token {
	if l.peeked != nil {
		t := *l.peeked
		l.peeked = nil
		return t
	}
	return l.next()
}

// Peek returns the next token without consuming it.
func (l *Lexer) Peek() Token {
	if l.peeked != nil {
		return *l.peeked
	}
	t := l.next()
	l.peeked = &t
	return t
}

func (l *Lexer) next() Token {
	l.skipWhitespace()
	start := l.pos()

	if l.off >= len(l.src) {
		return Token{Kind: TOK_EOF, Pos: start}
	}

	r := l.peekR()

	// Line comments -- ... and # ...
	if (r == '-' && l.peekR2() == '-') || r == '#' {
		return l.readLineComment(start)
	}

	// Block comment /* ... */
	if r == '/' && l.peekR2() == '*' {
		return l.readBlockComment(start)
	}

	// Backtick-quoted identifier
	if r == '`' {
		return l.readBacktickIdent(start)
	}

	// String literal ' or "
	if r == '\'' || r == '"' {
		return l.readString(start, r)
	}

	// Hex/bit literal X'...' or B'...'
	if (r == 'x' || r == 'X' || r == 'b' || r == 'B') && l.peekR2() == '\'' {
		return l.readHexOrBit(start, r)
	}
	// 0x or 0b
	if r == '0' && (l.peekR2() == 'x' || l.peekR2() == 'X' || l.peekR2() == 'b' || l.peekR2() == 'B') {
		return l.readHexOrBitPrefixed(start)
	}

	// Number
	if unicode.IsDigit(r) || (r == '.' && unicode.IsDigit(l.peekR2())) {
		return l.readNumber(start)
	}

	// Identifier or keyword (letter, underscore; includes $)
	if unicode.IsLetter(r) || r == '_' {
		return l.readWord(start)
	}

	// Punctuation / operators
	return l.readPunct(start)
}

func (l *Lexer) readLineComment(start Position) Token {
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if r == '\n' {
			break
		}
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_COMMENT, Lit: b.String(), Raw: b.String(), Pos: start}
}

func (l *Lexer) readBlockComment(start Position) Token {
	var b strings.Builder
	// consume /*
	b.WriteRune(l.advance())
	b.WriteRune(l.advance())
	for l.off < len(l.src) {
		if l.peekR() == '*' && l.peekR2() == '/' {
			b.WriteRune(l.advance())
			b.WriteRune(l.advance())
			return Token{Kind: TOK_COMMENT, Lit: b.String(), Raw: b.String(), Pos: start}
		}
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_COMMENT, Lit: b.String(), Raw: b.String(), Pos: start}
}

func (l *Lexer) readBacktickIdent(start Position) Token {
	var b, raw strings.Builder
	raw.WriteRune(l.advance()) // `
	for l.off < len(l.src) {
		r := l.peekR()
		if r == '`' {
			raw.WriteRune(l.advance())
			return Token{Kind: TOK_QUOTED_IDENT, Lit: b.String(), Raw: raw.String(), Pos: start}
		}
		raw.WriteRune(r)
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated `identifier`", Pos: start}
}

func (l *Lexer) readString(start Position, quote rune) Token {
	var b, raw strings.Builder
	raw.WriteRune(l.advance()) // opening quote
	for l.off < len(l.src) {
		r := l.peekR()
		if r == '\\' {
			raw.WriteRune(l.advance())
			if l.off < len(l.src) {
				nxt := l.advance()
				raw.WriteRune(nxt)
				switch nxt {
				case 'n':
					b.WriteRune('\n')
				case 't':
					b.WriteRune('\t')
				case 'r':
					b.WriteRune('\r')
				case '0':
					b.WriteRune(0)
				case '\\':
					b.WriteRune('\\')
				case '\'':
					b.WriteRune('\'')
				case '"':
					b.WriteRune('"')
				default:
					b.WriteRune(nxt)
				}
			}
			continue
		}
		if r == quote {
			// doubled-quote escape ('' or "")
			if l.peekR2() == quote {
				raw.WriteRune(l.advance())
				raw.WriteRune(l.advance())
				b.WriteRune(quote)
				continue
			}
			raw.WriteRune(l.advance())
			return Token{Kind: TOK_STRING, Lit: b.String(), Raw: raw.String(), Pos: start}
		}
		raw.WriteRune(r)
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated string", Pos: start}
}

func (l *Lexer) readHexOrBit(start Position, intro rune) Token {
	var raw strings.Builder
	raw.WriteRune(l.advance()) // x/b/X/B
	// read the quoted body
	tok := l.readString(start, '\'')
	if tok.Kind == TOK_ERROR {
		return tok
	}
	raw.WriteString(tok.Raw)
	if intro == 'x' || intro == 'X' {
		return Token{Kind: TOK_HEX, Lit: tok.Lit, Raw: raw.String(), Pos: start}
	}
	return Token{Kind: TOK_BIT, Lit: tok.Lit, Raw: raw.String(), Pos: start}
}

func (l *Lexer) readHexOrBitPrefixed(start Position) Token {
	l.advance() // '0'
	p := l.advance()
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if p == 'x' || p == 'X' {
			if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
				b.WriteRune(l.advance())
				continue
			}
		} else { // b/B
			if r == '0' || r == '1' {
				b.WriteRune(l.advance())
				continue
			}
		}
		break
	}
	if p == 'x' || p == 'X' {
		return Token{Kind: TOK_HEX, Lit: b.String(), Pos: start}
	}
	return Token{Kind: TOK_BIT, Lit: b.String(), Pos: start}
}

func (l *Lexer) readNumber(start Position) Token {
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if unicode.IsDigit(r) || r == '.' {
			b.WriteRune(l.advance())
			continue
		}
		if (r == 'e' || r == 'E') && (l.peekR2() == '+' || l.peekR2() == '-' || unicode.IsDigit(l.peekR2())) {
			b.WriteRune(l.advance())
			b.WriteRune(l.advance())
			for l.off < len(l.src) && unicode.IsDigit(l.peekR()) {
				b.WriteRune(l.advance())
			}
			break
		}
		break
	}
	return Token{Kind: TOK_NUMBER, Lit: b.String(), Raw: b.String(), Pos: start}
}

func (l *Lexer) readWord(start Position) Token {
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' {
			b.WriteRune(l.advance())
			continue
		}
		break
	}
	word := b.String()
	upper := strings.ToUpper(word)

	// Special-case the DELIMITER client directive: consume everything up to end-of-line
	// as the new delimiter.
	if upper == "DELIMITER" {
		// skip spaces/tabs
		for l.off < len(l.src) && (l.peekR() == ' ' || l.peekR() == '\t') {
			l.advance()
		}
		var d strings.Builder
		for l.off < len(l.src) {
			r := l.peekR()
			if r == '\n' || r == '\r' {
				break
			}
			d.WriteRune(l.advance())
		}
		return Token{Kind: TOK_DELIMITER_CMD, Lit: strings.TrimSpace(d.String()), Raw: word + " " + d.String(), Pos: start}
	}

	if isKeyword(upper) {
		return Token{Kind: TOK_KEYWORD, Lit: upper, Raw: word, Pos: start}
	}
	return Token{Kind: TOK_IDENT, Lit: word, Raw: word, Pos: start}
}

func (l *Lexer) readPunct(start Position) Token {
	r := l.peekR()
	r2 := l.peekR2()
	// 3-char
	if r == '<' && r2 == '=' && l.off+2 < len(l.src) && l.src[l.off+2] == '>' {
		l.advance()
		l.advance()
		l.advance()
		return Token{Kind: TOK_PUNCT, Lit: "<=>", Pos: start}
	}
	// 2-char
	two := string(r) + string(r2)
	switch two {
	case "<=", ">=", "<>", "!=", "||", "&&", ":=":
		l.advance()
		l.advance()
		return Token{Kind: TOK_PUNCT, Lit: two, Pos: start}
	}
	// 1-char
	switch r {
	case '(', ')', ',', ';', '.', '@', '=', '<', '>', '+', '-', '*', '/', '%', '!', '~', '^', '&', '|', ':':
		l.advance()
		return Token{Kind: TOK_PUNCT, Lit: string(r), Pos: start}
	}
	// fallthrough : single unknown rune. Store the rune itself in Raw so
	// callers reading Raw to recover the source slice get a 1-rune span,
	// not the 15-rune diagnostic message. Without Raw set,
	// tokenSourceLen falls back to len(Lit) and the rewriter would
	// emit `src[off : off+15]` for each unrecognised char — overlapping
	// the following 14 chars and producing visible duplication
	// (e.g. Oracle inquiry directive `$$PLSQL_UNIT` re-emerged as
	// `$$PLSQL_UNIT$PLSQL_UNITPLSQL_UNIT` and broke PG dollar-quoting).
	bad := string(l.advance())
	return Token{Kind: TOK_ERROR, Lit: "unexpected '" + bad + "'", Raw: bad, Pos: start}
}

// Tokenize consumes all tokens (skipping comments) and returns them.
// Useful for tests and debugging.
func Tokenize(src string) []Token {
	l := NewLexer(src)
	var out []Token
	for {
		t := l.Next()
		if t.Kind == TOK_EOF {
			out = append(out, t)
			return out
		}
		if t.Kind == TOK_COMMENT {
			continue
		}
		if t.Kind == TOK_DELIMITER_CMD {
			l.SetDelimiter(t.Lit)
			continue
		}
		out = append(out, t)
	}
}
