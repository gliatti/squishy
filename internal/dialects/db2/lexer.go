package db2

import (
	"strings"
	"unicode"
)

// Lexer scans raw IBM Db2 SQL / SQL PL source into a stream of tokens.
//
// DB2-specific lexical features:
//   - Unquoted identifiers fold to UPPER (DB2 catalog stores upper-case).
//   - "Quoted identifiers" preserve case (and may contain any char except ").
//   - Single-quote strings with '' escape; N'…' national string treated as
//     plain TOK_STRING (the N marker has no semantic effect for the parser).
//   - Typed binary literals: X'ff…' (hex bytes), GX'ff…' (graphic hex,
//     UCS-2), UX'ff…' (UTF-8 hex), BX'01…' (bit string). All emitted as
//     TOK_HEX_STRING with Lit prefixed by the kind ("X:", "GX:", …) so the
//     parser can route them to the matching FOR-BIT-DATA / GRAPHIC type.
//   - Embedded-SQL host variables `:name` → TOK_HOST_VAR.
//   - Dynamic placeholders `?` → TOK_PARAM (distinct from punctuation).
//   - `:=` assignment in SQL PL → TOK_ASSIGN.
//   - `||` concatenation, `**` exponent, `<>`, `!=`, `^=` (z/OS alias) → TOK_PUNCT.
//   - Lone `/` at line start → TOK_SLASH (CLP script terminator, à la SQL*Plus).
type Lexer struct {
	src       []rune
	off       int
	line, col int

	atLineStart bool

	peeked *Token
}

func NewLexer(src string) *Lexer {
	return &Lexer{src: []rune(src), off: 0, line: 1, col: 1, atLineStart: true}
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
		l.atLineStart = true
	} else {
		l.col++
		if r != ' ' && r != '\t' && r != '\r' {
			l.atLineStart = false
		}
	}
	return r
}

func (l *Lexer) skipSpaces() {
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
	l.skipSpaces()
	start := l.pos()

	if l.off >= len(l.src) {
		return Token{Kind: TOK_EOF, Pos: start}
	}

	r := l.peekR()

	// Lone '/' at line start → CLP terminator.
	if r == '/' && l.atLineStart {
		save, sc, sl := l.off, l.col, l.line
		l.advance()
		for l.off < len(l.src) && (l.peekR() == ' ' || l.peekR() == '\t' || l.peekR() == '\r') {
			l.advance()
		}
		if l.off >= len(l.src) || l.peekR() == '\n' {
			return Token{Kind: TOK_SLASH, Lit: "/", Pos: start}
		}
		l.off, l.col, l.line = save, sc, sl
	}

	// Line comments -- ...
	if r == '-' && l.peekR2() == '-' {
		return l.readLineComment(start)
	}

	// Block comment /* ... */
	if r == '/' && l.peekR2() == '*' {
		return l.readBlockComment(start)
	}

	// Typed binary literals: X'…', GX'…', UX'…', BX'…'.
	if (r == 'x' || r == 'X') && l.peekR2() == '\'' {
		return l.readHexString(start, "X")
	}
	if (r == 'g' || r == 'G') && (l.peekR2() == 'x' || l.peekR2() == 'X') &&
		l.off+2 < len(l.src) && l.src[l.off+2] == '\'' {
		l.advance() // G
		return l.readHexString(start, "GX")
	}
	if (r == 'u' || r == 'U') && (l.peekR2() == 'x' || l.peekR2() == 'X') &&
		l.off+2 < len(l.src) && l.src[l.off+2] == '\'' {
		l.advance() // U
		return l.readHexString(start, "UX")
	}
	if (r == 'b' || r == 'B') && (l.peekR2() == 'x' || l.peekR2() == 'X') &&
		l.off+2 < len(l.src) && l.src[l.off+2] == '\'' {
		l.advance() // B
		return l.readHexString(start, "BX")
	}

	// Quoted identifier "..."
	if r == '"' {
		return l.readQuotedIdent(start)
	}

	// String literal (with optional N prefix for national)
	if r == '\'' {
		return l.readString(start)
	}
	if (r == 'n' || r == 'N') && l.peekR2() == '\'' {
		l.advance()
		return l.readString(start)
	}
	if (r == 'g' || r == 'G') && l.peekR2() == '\'' {
		// G'…' graphic string literal — still a string for the parser.
		l.advance()
		return l.readString(start)
	}

	// Number
	if unicode.IsDigit(r) || (r == '.' && unicode.IsDigit(l.peekR2())) {
		return l.readNumber(start)
	}

	// Host variable :name
	if r == ':' && (unicode.IsLetter(l.peekR2()) || l.peekR2() == '"' || l.peekR2() == '_') {
		return l.readHostVar(start)
	}

	// Identifier or keyword
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
	b.WriteRune(l.advance()) // /
	b.WriteRune(l.advance()) // *
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

func (l *Lexer) readQuotedIdent(start Position) Token {
	var b, raw strings.Builder
	raw.WriteRune(l.advance()) // "
	for l.off < len(l.src) {
		r := l.peekR()
		if r == '"' {
			if l.peekR2() == '"' {
				raw.WriteRune(l.advance())
				raw.WriteRune(l.advance())
				b.WriteRune('"')
				continue
			}
			raw.WriteRune(l.advance())
			return Token{Kind: TOK_QUOTED_IDENT, Lit: b.String(), Raw: raw.String(), Pos: start}
		}
		raw.WriteRune(r)
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated \"identifier\"", Pos: start}
}

func (l *Lexer) readString(start Position) Token {
	var b, raw strings.Builder
	raw.WriteRune(l.advance()) // '
	for l.off < len(l.src) {
		r := l.peekR()
		if r == '\'' {
			if l.peekR2() == '\'' {
				raw.WriteRune(l.advance())
				raw.WriteRune(l.advance())
				b.WriteRune('\'')
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

// readHexString reads `<prefix>'<hex bytes>'` and returns a TOK_HEX_STRING.
// On entry, the next rune is the X (or x) opener; for the GX/UX/BX
// variants, the leading G/U/B has already been consumed by the caller.
// Lit format: "<prefix>:<bytes>" so the parser can branch on prefix.
func (l *Lexer) readHexString(start Position, prefix string) Token {
	var raw, body strings.Builder
	raw.WriteRune(l.advance()) // X (or x)
	raw.WriteRune(l.advance()) // '
	for l.off < len(l.src) {
		r := l.peekR()
		if r == '\'' {
			raw.WriteRune(l.advance())
			return Token{Kind: TOK_HEX_STRING, Lit: prefix + ":" + body.String(), Raw: raw.String(), Pos: start}
		}
		raw.WriteRune(r)
		body.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated " + prefix + " literal", Pos: start}
}

func (l *Lexer) readNumber(start Position) Token {
	var b strings.Builder
	seenDot := false
	for l.off < len(l.src) {
		r := l.peekR()
		if unicode.IsDigit(r) {
			b.WriteRune(l.advance())
			continue
		}
		if r == '.' && !seenDot {
			// Don't grab a trailing '.' if followed by another '.' (range op).
			if l.peekR2() == '.' {
				break
			}
			seenDot = true
			b.WriteRune(l.advance())
			continue
		}
		break
	}
	// exponent
	if l.off < len(l.src) && (l.peekR() == 'e' || l.peekR() == 'E') {
		p2 := l.peekR2()
		if unicode.IsDigit(p2) || p2 == '+' || p2 == '-' {
			b.WriteRune(l.advance())
			if l.peekR() == '+' || l.peekR() == '-' {
				b.WriteRune(l.advance())
			}
			for l.off < len(l.src) && unicode.IsDigit(l.peekR()) {
				b.WriteRune(l.advance())
			}
		}
	}
	return Token{Kind: TOK_NUMBER, Lit: b.String(), Raw: b.String(), Pos: start}
}

func (l *Lexer) readHostVar(start Position) Token {
	var b, raw strings.Builder
	raw.WriteRune(l.advance()) // :
	if l.peekR() == '"' {
		id := l.readQuotedIdent(l.pos())
		raw.WriteString(id.Raw)
		b.WriteString(id.Lit)
		return Token{Kind: TOK_HOST_VAR, Lit: b.String(), Raw: raw.String(), Pos: start}
	}
	for l.off < len(l.src) {
		r := l.peekR()
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			raw.WriteRune(r)
			b.WriteRune(l.advance())
			continue
		}
		break
	}
	return Token{Kind: TOK_HOST_VAR, Lit: b.String(), Raw: raw.String(), Pos: start}
}

func (l *Lexer) readWord(start Position) Token {
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '#' || r == '@' || r == '$' {
			b.WriteRune(l.advance())
			continue
		}
		break
	}
	word := b.String()
	upper := strings.ToUpper(word)
	if isKeyword(upper) {
		return Token{Kind: TOK_KEYWORD, Lit: upper, Raw: word, Pos: start}
	}
	// DB2 semantics: unquoted identifiers fold to upper.
	return Token{Kind: TOK_IDENT, Lit: upper, Raw: word, Pos: start}
}

func (l *Lexer) readPunct(start Position) Token {
	r := l.peekR()
	r2 := l.peekR2()
	two := string(r) + string(r2)
	switch two {
	case ":=":
		l.advance()
		l.advance()
		return Token{Kind: TOK_ASSIGN, Lit: ":=", Pos: start}
	case "||", "**", "<=", ">=", "<>", "!=", "^=":
		l.advance()
		l.advance()
		// Normalize z/OS-style ^= to <>.
		lit := two
		if lit == "^=" || lit == "!=" {
			lit = "<>"
		}
		return Token{Kind: TOK_PUNCT, Lit: lit, Pos: start}
	}
	switch r {
	case '?':
		l.advance()
		return Token{Kind: TOK_PARAM, Lit: "?", Pos: start}
	case '(', ')', ',', ';', '.', '@', '=', '<', '>', '+', '-', '*', '/', '%', '!', '~', '^', '&', '|', ':':
		l.advance()
		return Token{Kind: TOK_PUNCT, Lit: string(r), Pos: start}
	}
	bad := string(l.advance())
	return Token{Kind: TOK_ERROR, Lit: "unexpected '" + bad + "'", Pos: start}
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
		out = append(out, t)
	}
}
