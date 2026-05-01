package oracle

import (
	"strings"
	"unicode"
)

// Lexer scans raw Oracle/PL-SQL source into a stream of tokens.
//
// Oracle-specific lexical features:
//   - Unquoted identifiers fold to UPPER (Oracle semantics).
//   - "Quoted identifiers" preserve case (and may contain any char except ").
//   - Single-quote strings with '' escape. Q-quote alternative: q'[...]', q'{...}',
//     q'(...)', q'<...>', q'!...!' (any char may be the delimiter — matching pair
//     for (), [], {}, <>; otherwise same char closes it).
//   - N'...' / n'...' national string (lexed as TOK_STRING, marker lost).
//   - :name and :1 host variables → TOK_BIND.
//   - Two-char operators := => .. || ** and << >> label delimiters.
//   - "/" alone on a line terminates a PL/SQL block (SQL*Plus convention) → TOK_SLASH_TERM.
type Lexer struct {
	src       []rune
	off       int
	line, col int

	// Track whether we're at the start of a line (after a newline or BOF) —
	// needed so a lone '/' is recognized as the SQL*Plus terminator.
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
		if r == ' ' || r == '\t' || r == '\r' {
			l.advance()
			continue
		}
		if r == '\n' {
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

	// Lone '/' at line start → SQL*Plus block terminator.
	if r == '/' && l.atLineStart {
		// look ahead past spaces/tabs to a newline or EOF
		save := l.off
		saveCol := l.col
		saveLine := l.line
		l.advance()
		for l.off < len(l.src) && (l.peekR() == ' ' || l.peekR() == '\t' || l.peekR() == '\r') {
			l.advance()
		}
		if l.off >= len(l.src) || l.peekR() == '\n' {
			return Token{Kind: TOK_SLASH_TERM, Lit: "/", Pos: start}
		}
		// Not a terminator — rewind.
		l.off = save
		l.col = saveCol
		l.line = saveLine
	}

	// Line comments -- ...
	if r == '-' && l.peekR2() == '-' {
		return l.readLineComment(start)
	}

	// Block comment /* ... */
	if r == '/' && l.peekR2() == '*' {
		return l.readBlockComment(start)
	}

	// Q-quote alternative: q'X...X' / Q'X...X' (and likewise with N/n prefix: Nq'...')
	if (r == 'q' || r == 'Q') && l.peekR2() == '\'' {
		return l.readQQuote(start, false)
	}
	if (r == 'n' || r == 'N') && (l.peekR2() == 'q' || l.peekR2() == 'Q') &&
		l.off+2 < len(l.src) && l.src[l.off+2] == '\'' {
		// Skip the N prefix and delegate.
		l.advance()
		return l.readQQuote(start, true)
	}

	// Quoted identifier "..."
	if r == '"' {
		return l.readQuotedIdent(start)
	}

	// String literal
	if r == '\'' {
		return l.readString(start)
	}
	// N'...' national string
	if (r == 'n' || r == 'N') && l.peekR2() == '\'' {
		l.advance()
		return l.readString(start)
	}

	// 0x hex (non-standard in Oracle but some dumps have it)
	if r == '0' && (l.peekR2() == 'x' || l.peekR2() == 'X') {
		return l.readHex(start)
	}

	// Number
	if unicode.IsDigit(r) || (r == '.' && unicode.IsDigit(l.peekR2())) {
		return l.readNumber(start)
	}

	// Bind variable :name / :1
	if r == ':' && (unicode.IsLetter(l.peekR2()) || unicode.IsDigit(l.peekR2()) || l.peekR2() == '"') {
		return l.readBind(start)
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
			// doubled-quote escape
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

func (l *Lexer) readQQuote(start Position, _ bool) Token {
	var raw strings.Builder
	raw.WriteRune(l.advance()) // q or Q
	raw.WriteRune(l.advance()) // '
	if l.off >= len(l.src) {
		return Token{Kind: TOK_ERROR, Lit: "unterminated q-quote", Pos: start}
	}
	opener := l.advance()
	raw.WriteRune(opener)
	closer := opener
	switch opener {
	case '(':
		closer = ')'
	case '[':
		closer = ']'
	case '{':
		closer = '}'
	case '<':
		closer = '>'
	}
	var body strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if r == closer && l.peekR2() == '\'' {
			raw.WriteRune(l.advance())
			raw.WriteRune(l.advance())
			return Token{Kind: TOK_STRING, Lit: body.String(), Raw: raw.String(), Pos: start}
		}
		raw.WriteRune(r)
		body.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated q-quote", Pos: start}
}

func (l *Lexer) readHex(start Position) Token {
	l.advance() // 0
	l.advance() // x
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			b.WriteRune(l.advance())
			continue
		}
		break
	}
	return Token{Kind: TOK_HEX, Lit: b.String(), Pos: start}
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
		if r == '.' && !seenDot && l.peekR2() != '.' {
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
	// Oracle's f/d literal suffixes (BINARY_FLOAT/BINARY_DOUBLE)
	if l.off < len(l.src) {
		r := l.peekR()
		if r == 'f' || r == 'F' || r == 'd' || r == 'D' {
			b.WriteRune(l.advance())
		}
	}
	return Token{Kind: TOK_NUMBER, Lit: b.String(), Raw: b.String(), Pos: start}
}

func (l *Lexer) readBind(start Position) Token {
	var b, raw strings.Builder
	raw.WriteRune(l.advance()) // :
	if l.peekR() == '"' {
		id := l.readQuotedIdent(l.pos())
		raw.WriteString(id.Raw)
		b.WriteString(id.Lit)
		return Token{Kind: TOK_BIND, Lit: b.String(), Raw: raw.String(), Pos: start}
	}
	for l.off < len(l.src) {
		r := l.peekR()
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' || r == '#' {
			raw.WriteRune(r)
			b.WriteRune(l.advance())
			continue
		}
		break
	}
	return Token{Kind: TOK_BIND, Lit: b.String(), Raw: raw.String(), Pos: start}
}

func (l *Lexer) readWord(start Position) Token {
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekR()
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' || r == '#' {
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
	// Oracle semantics: unquoted identifiers are case-insensitive, canonical
	// form is uppercase.
	return Token{Kind: TOK_IDENT, Lit: upper, Raw: word, Pos: start}
}

func (l *Lexer) readPunct(start Position) Token {
	r := l.peekR()
	r2 := l.peekR2()
	// 2-char operators
	two := string(r) + string(r2)
	switch two {
	case ":=":
		l.advance()
		l.advance()
		return Token{Kind: TOK_ASSIGN, Lit: ":=", Pos: start}
	case "=>":
		l.advance()
		l.advance()
		return Token{Kind: TOK_ARROW, Lit: "=>", Pos: start}
	case "..":
		l.advance()
		l.advance()
		return Token{Kind: TOK_RANGE, Lit: "..", Pos: start}
	case "<<":
		l.advance()
		l.advance()
		return Token{Kind: TOK_LABEL_START, Lit: "<<", Pos: start}
	case ">>":
		l.advance()
		l.advance()
		return Token{Kind: TOK_LABEL_END, Lit: ">>", Pos: start}
	case "||", "**", "<=", ">=", "<>", "!=":
		l.advance()
		l.advance()
		return Token{Kind: TOK_PUNCT, Lit: two, Pos: start}
	}
	// 1-char
	switch r {
	case '(', ')', ',', ';', '.', '@', '=', '<', '>', '+', '-', '*', '/', '%', '!', '~', '^', '&', '|', ':', '?':
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
