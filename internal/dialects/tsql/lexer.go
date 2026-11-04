package tsql

import (
	"strings"
	"unicode"
)

// Lexer scans raw T-SQL source text into a stream of tokens, character by
// character (the only place in the dialect that reads text; everything
// downstream works on tokens and AST nodes). It follows the token rules of
// reference/TSqlLexer.g4:
//
//   - identifiers: unquoted ([A-Z_#] then [A-Z_#$@0-9], Unicode letters
//     included), [bracketed] (]] escapes ]) and "double-quoted" (the
//     QUOTED_IDENTIFIER ON reading, "" escapes ");
//   - @local / @@global variables (LOCAL_ID), #temp names (TEMP_ID),
//     $action-style pseudo columns;
//   - strings '…' and N'…' with doubled-quote escapes (no backslash
//     escapes in T-SQL), 0x… binary, numbers, $12.50 money literals;
//   - -- line comments and nested /* … */ block comments;
//   - the sqlcmd layer: GO [count] alone on its line is the batch
//     separator (the role the client DELIMITER plays for MySQL), :setvar /
//     :on error / :r … lines are directives and $(Name) is a scripting
//     variable reference.
type Lexer struct {
	src       []rune
	off       int // rune offset
	line, col int
	// lastLine is the line on which the previous token ended; a token
	// starting on a later line is the first of its line.
	lastLine int
}

// NewLexer returns a lexer over src. A leading UTF-8 byte-order mark is
// skipped (SSMS saves scripts with one).
func NewLexer(src string) *Lexer {
	return &Lexer{src: []rune(src), line: 1, col: 1, lastLine: 0}
}

func (l *Lexer) pos() Position { return Position{Line: l.line, Col: l.col, Offset: l.off} }

func (l *Lexer) peekAt(n int) rune {
	if l.off+n >= len(l.src) {
		return 0
	}
	return l.src[l.off+n]
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

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\r', '\n', '\f', '\v', 0xFEFF, 0x00A0:
		return true
	}
	return false
}

// isIdentStart reports whether r can start an unquoted identifier
// (grammar ID: [A-Z_#] | FullWidthLetter).
func isIdentStart(r rune) bool {
	return r == '_' || r == '#' || unicode.IsLetter(r)
}

// isIdentPart reports whether r can continue an unquoted identifier
// (grammar ID: [A-Z_#$@0-9] | FullWidthLetter).
func isIdentPart(r rune) bool {
	return r == '_' || r == '#' || r == '$' || r == '@' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isHexDigit(r rune) bool {
	return isDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// Next returns the next token (comments included), or EOF at end of input.
func (l *Lexer) Next() Token {
	for l.off < len(l.src) && isSpace(l.src[l.off]) {
		l.advance()
	}
	start := l.pos()
	if l.off >= len(l.src) {
		return Token{Kind: TOK_EOF, Pos: start, End: l.off}
	}
	lineStart := start.Line > l.lastLine
	tok := l.scan(start, lineStart)
	tok.Pos = start
	tok.End = l.off
	tok.LineStart = lineStart
	if tok.Raw == "" {
		tok.Raw = string(l.src[start.Offset:l.off])
	}
	l.lastLine = l.line
	return tok
}

func (l *Lexer) scan(start Position, lineStart bool) Token {
	r := l.peekAt(0)
	r2 := l.peekAt(1)
	switch {
	case r == '-' && r2 == '-':
		return l.readLineComment()
	case r == '/' && r2 == '*':
		return l.readBlockComment()
	case r == '[':
		return l.readBracketIdent()
	case r == '"':
		return l.readDoubleQuotedIdent()
	case r == '\'':
		return l.readString(false)
	case (r == 'N' || r == 'n') && r2 == '\'':
		l.advance() // N
		return l.readString(true)
	case r == '0' && (r2 == 'x' || r2 == 'X'):
		return l.readHex()
	case isDigit(r) || (r == '.' && isDigit(r2)):
		return l.readNumber()
	case r == '$':
		return l.readDollar()
	case r == '@':
		return l.readVariable()
	case r == ':' && lineStart && unicode.IsLetter(r2):
		return l.readSQLCmd()
	case isIdentStart(r):
		return l.readWord(lineStart)
	}
	return l.readPunct()
}

func (l *Lexer) readLineComment() Token {
	startOff := l.off
	for l.off < len(l.src) && l.src[l.off] != '\n' {
		l.advance()
	}
	text := string(l.src[startOff:l.off])
	return Token{Kind: TOK_COMMENT, Lit: text, Raw: text}
}

// readBlockComment reads a /* … */ comment; T-SQL block comments nest
// (grammar COMMENT: '/*' (COMMENT | .)*? '*/').
func (l *Lexer) readBlockComment() Token {
	startOff := l.off
	depth := 0
	for l.off < len(l.src) {
		if l.peekAt(0) == '/' && l.peekAt(1) == '*' {
			l.advance()
			l.advance()
			depth++
			continue
		}
		if l.peekAt(0) == '*' && l.peekAt(1) == '/' {
			l.advance()
			l.advance()
			depth--
			if depth == 0 {
				text := string(l.src[startOff:l.off])
				return Token{Kind: TOK_COMMENT, Lit: text, Raw: text}
			}
			continue
		}
		l.advance()
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated /* comment"}
}

// readBracketIdent reads a [delimited identifier]; `]]` stands for `]`.
func (l *Lexer) readBracketIdent() Token {
	l.advance() // [
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekAt(0)
		if r == ']' {
			if l.peekAt(1) == ']' {
				l.advance()
				l.advance()
				b.WriteRune(']')
				continue
			}
			l.advance()
			return Token{Kind: TOK_QUOTED_IDENT, Lit: b.String()}
		}
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated [identifier]"}
}

// readDoubleQuotedIdent reads a "delimited identifier" (QUOTED_IDENTIFIER
// ON, the default for every modern client); `""` stands for `"`.
func (l *Lexer) readDoubleQuotedIdent() Token {
	l.advance() // "
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekAt(0)
		if r == '"' {
			if l.peekAt(1) == '"' {
				l.advance()
				l.advance()
				b.WriteRune('"')
				continue
			}
			l.advance()
			return Token{Kind: TOK_QUOTED_IDENT, Lit: b.String()}
		}
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: `unterminated "identifier"`}
}

// readString reads a '…' literal (the optional N prefix is already
// consumed when national is true). T-SQL has no backslash escapes: the only
// escape is the doubled quote.
func (l *Lexer) readString(national bool) Token {
	l.advance() // opening '
	var b strings.Builder
	for l.off < len(l.src) {
		r := l.peekAt(0)
		if r == '\'' {
			if l.peekAt(1) == '\'' {
				l.advance()
				l.advance()
				b.WriteRune('\'')
				continue
			}
			l.advance()
			return Token{Kind: TOK_STRING, Lit: b.String(), NString: national}
		}
		b.WriteRune(l.advance())
	}
	return Token{Kind: TOK_ERROR, Lit: "unterminated string"}
}

// readHex reads a 0x… binary literal (grammar BINARY: '0' 'X' HEX_DIGIT*);
// `0x` alone is the empty binary value.
func (l *Lexer) readHex() Token {
	l.advance() // 0
	l.advance() // x
	startOff := l.off
	for l.off < len(l.src) && isHexDigit(l.src[l.off]) {
		l.advance()
	}
	return Token{Kind: TOK_HEX, Lit: string(l.src[startOff:l.off])}
}

// readNumber reads DECIMAL / FLOAT / REAL: digits [. digits] [E [+-] digits],
// `.5` and `1.` included.
func (l *Lexer) readNumber() Token {
	startOff := l.off
	for l.off < len(l.src) && isDigit(l.src[l.off]) {
		l.advance()
	}
	if l.peekAt(0) == '.' {
		l.advance()
		for l.off < len(l.src) && isDigit(l.src[l.off]) {
			l.advance()
		}
	}
	if r := l.peekAt(0); r == 'e' || r == 'E' {
		r2 := l.peekAt(1)
		if isDigit(r2) || ((r2 == '+' || r2 == '-') && isDigit(l.peekAt(2))) {
			l.advance()
			if r2 == '+' || r2 == '-' {
				l.advance()
			}
			for l.off < len(l.src) && isDigit(l.src[l.off]) {
				l.advance()
			}
		}
	}
	text := string(l.src[startOff:l.off])
	return Token{Kind: TOK_NUMBER, Lit: text}
}

// readDollar handles the three `$` shapes: a money literal ($12.50, $-1 is
// not a literal), a sqlcmd variable $(Name), and a $-prefixed pseudo column
// ($action, $IDENTITY, $ROWGUID, $PARTITION).
func (l *Lexer) readDollar() Token {
	r2 := l.peekAt(1)
	switch {
	case isDigit(r2) || (r2 == '.' && isDigit(l.peekAt(2))):
		l.advance() // $
		num := l.readNumber()
		return Token{Kind: TOK_MONEY, Lit: num.Lit}
	case r2 == '(':
		l.advance() // $
		l.advance() // (
		startOff := l.off
		for l.off < len(l.src) && l.src[l.off] != ')' && l.src[l.off] != '\n' {
			l.advance()
		}
		name := string(l.src[startOff:l.off])
		if l.peekAt(0) != ')' {
			return Token{Kind: TOK_ERROR, Lit: "unterminated $( sqlcmd variable"}
		}
		l.advance() // )
		return Token{Kind: TOK_SQLCMD_VAR, Lit: name}
	case unicode.IsLetter(r2) || r2 == '_':
		startOff := l.off
		l.advance() // $
		for l.off < len(l.src) && isIdentPart(l.src[l.off]) {
			l.advance()
		}
		word := string(l.src[startOff:l.off])
		return Token{Kind: TOK_IDENT, Lit: word}
	}
	l.advance()
	return Token{Kind: TOK_ERROR, Lit: "unexpected '$'"}
}

// readVariable reads @local / @@global (grammar LOCAL_ID: '@' [A-Z_$@#0-9]*).
// A lone '@' is punctuation.
func (l *Lexer) readVariable() Token {
	startOff := l.off
	l.advance() // @
	for l.off < len(l.src) && isIdentPart(l.src[l.off]) {
		l.advance()
	}
	text := string(l.src[startOff:l.off])
	if text == "@" {
		return Token{Kind: TOK_PUNCT, Lit: "@"}
	}
	return Token{Kind: TOK_VARIABLE, Lit: text}
}

// readSQLCmd reads a sqlcmd directive line (`:setvar Name "value"`,
// `:on error exit`, `:r file.sql`, …) up to the end of the line. Lit is the
// upper-cased command word.
func (l *Lexer) readSQLCmd() Token {
	startOff := l.off
	l.advance() // :
	cmdStart := l.off
	for l.off < len(l.src) && (unicode.IsLetter(l.src[l.off]) || l.src[l.off] == '_') {
		l.advance()
	}
	cmd := strings.ToUpper(string(l.src[cmdStart:l.off]))
	for l.off < len(l.src) && l.src[l.off] != '\n' {
		l.advance()
	}
	raw := strings.TrimRight(string(l.src[startOff:l.off]), "\r")
	return Token{Kind: TOK_SQLCMD, Lit: cmd, Raw: raw}
}

// readWord reads an identifier or a keyword. GO alone on its line (with an
// optional repeat count and an optional trailing -- comment) is the batch
// separator.
func (l *Lexer) readWord(lineStart bool) Token {
	startOff := l.off
	for l.off < len(l.src) && isIdentPart(l.src[l.off]) {
		l.advance()
	}
	word := string(l.src[startOff:l.off])
	upper := strings.ToUpper(word)
	if upper == "GO" && lineStart {
		if count, ok := l.goTail(); ok {
			return Token{Kind: TOK_GO, Lit: count}
		}
	}
	if isReserved(upper) {
		return Token{Kind: TOK_KEYWORD, Lit: upper}
	}
	return Token{Kind: TOK_IDENT, Lit: word}
}

// goTail checks that the rest of the line after GO is blank, an optional
// count and/or a -- comment; on success it consumes the count and returns
// it.
func (l *Lexer) goTail() (string, bool) {
	i := l.off
	skipBlanks := func() {
		for i < len(l.src) && (l.src[i] == ' ' || l.src[i] == '\t') {
			i++
		}
	}
	skipBlanks()
	digStart := i
	for i < len(l.src) && isDigit(l.src[i]) {
		i++
	}
	digEnd := i
	skipBlanks()
	atEOL := i >= len(l.src) || l.src[i] == '\n' || l.src[i] == '\r' ||
		(l.src[i] == '-' && i+1 < len(l.src) && l.src[i+1] == '-')
	if !atEOL {
		return "", false
	}
	for l.off < digEnd {
		l.advance()
	}
	return string(l.src[digStart:digEnd]), true
}

func (l *Lexer) readPunct() Token {
	r := l.peekAt(0)
	r2 := l.peekAt(1)
	two := string([]rune{r, r2})
	switch two {
	case "<>", "!=", "!<", "!>", "<=", ">=", "+=", "-=", "*=", "/=", "%=", "&=", "^=", "|=", "::", "||":
		l.advance()
		l.advance()
		return Token{Kind: TOK_PUNCT, Lit: two}
	}
	switch r {
	case '(', ')', ',', ';', '.', '=', '<', '>', '+', '-', '*', '/', '%', '!', '~', '^', '&', '|', ':', '{', '}', '?':
		l.advance()
		return Token{Kind: TOK_PUNCT, Lit: string(r)}
	}
	bad := string(l.advance())
	return Token{Kind: TOK_ERROR, Lit: "unexpected '" + bad + "'", Raw: bad}
}

// Tokenize consumes all tokens, skipping comments, and returns them with a
// trailing EOF token. Useful for tests and debugging.
func Tokenize(src string) []Token {
	l := NewLexer(src)
	var out []Token
	for {
		t := l.Next()
		if t.Kind == TOK_COMMENT {
			continue
		}
		out = append(out, t)
		if t.Kind == TOK_EOF {
			return out
		}
	}
}
