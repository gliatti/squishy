// Package db2 — parser entry point.
//
// Spec source of truth: internal/dialects/db2/reference/Db2*.g4.
// This file only wires the top-level dispatch and shared helpers. Statement
// parsing lives in parser_ddl.go (DDL) and parser_dml.go (raw passthrough).
// SQL PL routine bodies are captured verbatim and emitted as
// CreateProcedure.Body / CreateFunction.Body / CreateTrigger.Body for
// downstream lexical rewriting by internal/translate/db2_body_xlate.go.
package db2

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

type Parser struct {
	l    *Lexer
	errs ErrorList
	cur  Token

	src []rune
}

// Parse parses a Db2 script into a list of statements. It accepts ";" and
// the bare "/" line-start CLP terminator, and synchronizes on either when
// a parse error occurs.
func Parse(src string) ([]ast.Stmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	var stmts []ast.Stmt
	for {
		// Skip stray delimiters at script level.
		if p.isPunct(";") || p.cur.Kind == TOK_SLASH {
			p.advance()
			continue
		}
		if p.cur.Kind == TOK_EOF {
			break
		}
		stmt := p.parseStatement()
		if stmt != nil {
			stmts = append(stmts, stmt)
		} else {
			p.syncToDelimiter()
		}
		p.consumeStatementEnd()
	}
	return stmts, p.errs
}

// ---------------------------------------------------------------------------
// statement dispatch
// ---------------------------------------------------------------------------

func (p *Parser) parseStatement() ast.Stmt {
	switch {
	case p.isKw("CREATE"):
		return p.parseCreate()
	case p.isKw("ALTER"):
		return p.parseAlter()
	case p.isKw("DROP"):
		return p.parseDrop()
	case p.isKw("TRUNCATE"):
		return p.parseNoopToStmtEnd("TRUNCATE")
	case p.isKw("RENAME"):
		return p.parseNoopToStmtEnd("RENAME")
	case p.isKw("COMMENT"):
		return p.parseNoopToStmtEnd("COMMENT")
	case p.isKw("LABEL"):
		return p.parseNoopToStmtEnd("LABEL")
	case p.isKw("GRANT"), p.isKw("REVOKE"):
		return p.parseNoopToStmtEnd(p.cur.Lit)
	case p.isKw("SET"):
		return p.parseNoopToStmtEnd("SET")
	case p.isKw("CALL"):
		return p.parseNoopToStmtEnd("CALL")
	case p.isKw("INSERT"), p.isKw("UPDATE"), p.isKw("DELETE"),
		p.isKw("MERGE"), p.isKw("SELECT"), p.isKw("WITH"):
		// Top-level DML — captured raw as a NoopStmt of Kind "DML" so the
		// translator's body rewriter can pass it through to PG.
		return p.parseNoopToStmtEnd("DML")
	case p.isKw("BEGIN"), p.isKw("DECLARE"):
		// Anonymous SQL PL block at script top-level.
		return p.parseAnonymousBlock()
	}
	p.errorHere("unexpected token at statement start",
		"CREATE|ALTER|DROP|COMMENT|GRANT|BEGIN|DECLARE|SELECT|INSERT|UPDATE|DELETE|MERGE")
	return nil
}

// parseAnonymousBlock captures a top-level BEGIN…END or DECLARE…END script
// as a NoopStmt of Kind "ANONYMOUS_BLOCK". The translator wraps it in a PG
// `DO $$ … $$` after lexical rewriting.
func (p *Parser) parseAnonymousBlock() ast.Stmt {
	start := p.cur.Pos
	body := p.captureBlock()
	return &ast.NoopStmt{Kind: "ANONYMOUS_BLOCK", Text: body, P: astPos(start)}
}

// parseNoopToStmtEnd consumes tokens until the next ";" / "/" / EOF and
// returns a NoopStmt carrying the raw text.
func (p *Parser) parseNoopToStmtEnd(kind string) ast.Stmt {
	start := p.cur.Pos
	startOff := start.Offset
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	text := strings.TrimSpace(string(p.src[startOff:endOff]))
	return &ast.NoopStmt{Kind: kind, Text: text, P: astPos(start)}
}

// ---------------------------------------------------------------------------
// low-level helpers (mirrors internal/dialects/oracle/parser.go)
// ---------------------------------------------------------------------------

func (p *Parser) advance() {
	for {
		t := p.l.Next()
		if t.Kind == TOK_COMMENT {
			continue
		}
		p.cur = t
		return
	}
}

func (p *Parser) peek() Token {
	return p.l.Peek()
}

func (p *Parser) isPunct(lit string) bool {
	return p.cur.Kind == TOK_PUNCT && p.cur.Lit == lit
}

func (p *Parser) isKw(kw string) bool {
	return p.cur.Kind == TOK_KEYWORD && p.cur.Lit == kw
}

func (p *Parser) isAnyKw(kws ...string) bool {
	if p.cur.Kind != TOK_KEYWORD {
		return false
	}
	for _, k := range kws {
		if p.cur.Lit == k {
			return true
		}
	}
	return false
}

// isKwOrIdent matches a keyword or an unquoted identifier (case-folded
// upper). Useful where DB2 may parse a contextual keyword as an ident
// because we didn't list it in token.go.
func (p *Parser) isKwOrIdent(s string) bool {
	if p.cur.Kind != TOK_KEYWORD && p.cur.Kind != TOK_IDENT {
		return false
	}
	return p.cur.Lit == s
}

func (p *Parser) expectPunct(lit string) bool {
	if p.isPunct(lit) {
		p.advance()
		return true
	}
	p.errorHere("expected '"+lit+"'", "'"+lit+"'")
	return false
}

func (p *Parser) expectKw(kw string) bool {
	if p.isKw(kw) {
		p.advance()
		return true
	}
	p.errorHere("expected keyword "+kw, kw)
	return false
}

func (p *Parser) errorHere(msg, expected string) {
	got := p.cur.Lit
	if got == "" {
		got = p.cur.Kind.String()
	}
	p.errs = append(p.errs, &ParseError{
		Pos: p.cur.Pos, Msg: msg, Got: got, Expected: expected,
	})
}

// parseIdent accepts IDENT / QUOTED_IDENT / permissive-keyword-as-ident.
func (p *Parser) parseIdent() (name string, quoted bool) {
	switch p.cur.Kind {
	case TOK_IDENT:
		v := p.cur.Lit
		p.advance()
		return v, false
	case TOK_QUOTED_IDENT:
		v := p.cur.Lit
		p.advance()
		return v, true
	case TOK_KEYWORD:
		// DB2 — like Oracle — accepts most keywords as identifiers in lax
		// contexts (column names, alias names). The catalog stores the upper
		// form which is what we already have in Lit.
		v := p.cur.Lit
		p.advance()
		return v, false
	}
	p.errorHere("expected identifier", "IDENT")
	return "", false
}

// parseQualifiedName parses [schema.]name and returns its two parts.
func (p *Parser) parseQualifiedName() (schema, name string) {
	first, _ := p.parseIdent()
	if p.isPunct(".") {
		p.advance()
		second, _ := p.parseIdent()
		return first, second
	}
	return "", first
}

// parseTableRef parses [schema.]name into a TableRef.
func (p *Parser) parseTableRef() ast.TableRef {
	name, bt := p.parseIdent()
	if p.isPunct(".") {
		p.advance()
		second, bt2 := p.parseIdent()
		return ast.TableRef{Schema: name, Name: second, NameBacktick: bt || bt2}
	}
	return ast.TableRef{Name: name, NameBacktick: bt}
}

func (p *Parser) atStatementEnd() bool {
	return p.cur.Kind == TOK_EOF || p.isPunct(";") || p.cur.Kind == TOK_SLASH
}

func (p *Parser) consumeStatementEnd() {
	for p.isPunct(";") || p.cur.Kind == TOK_SLASH {
		p.advance()
	}
}

func (p *Parser) syncToDelimiter() {
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// Raw body capture — used by CREATE TRIGGER / PROCEDURE / FUNCTION whose
// bodies are captured verbatim and re-parsed later by db2_body_xlate.go.
// ---------------------------------------------------------------------------

// captureBlock reads a SQL PL block starting at BEGIN [ATOMIC] (or DECLARE
// in the anonymous-block case) with matched nesting for IF/CASE/LOOP/
// WHILE/REPEAT/FOR. Returns the raw text from the current token up to
// (and including) the matching END [label];.
func (p *Parser) captureBlock() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	caseDepth := 0
	for {
		switch {
		case p.isKw("BEGIN"), p.isKw("LOOP"), p.isKw("IF"),
			p.isKw("WHILE"), p.isKw("REPEAT"):
			// FOR is intentionally NOT tracked here — DB2 overloads it in
			// many contexts (`DECLARE … HANDLER FOR SQLEXCEPTION`,
			// `CURSOR FOR SELECT …`, `FETCH FIRST n ROWS ONLY` etc.) where
			// the keyword does not open a block. Routine bodies that
			// genuinely contain a `FOR var AS cursor DO …  END FOR` pass
			// through raw — captureBlock keeps depth balanced because the
			// END FOR arm below also skips depth bookkeeping.
			depth++
			p.advance()
		case p.isKw("CASE"):
			caseDepth++
			p.advance()
		case p.isKw("END"):
			next := p.peek()
			if next.Kind == TOK_KEYWORD {
				switch next.Lit {
				case "CASE":
					if caseDepth > 0 {
						caseDepth--
					}
					p.advance() // END
					p.advance() // CASE
					continue
				case "IF", "LOOP", "WHILE", "REPEAT":
					p.advance() // END
					p.advance() // IF/LOOP/...
					depth--
					if depth <= 0 {
						return string(p.src[startOff:p.cur.Pos.Offset])
					}
					continue
				case "FOR":
					// `END FOR` closes a FOR-cursor loop. Since we don't
					// open it, we just consume the pair and move on.
					p.advance()
					p.advance()
					continue
				}
			}
			// Bare END inside an open CASE expression: closes the expression
			// without affecting block depth.
			if caseDepth > 0 {
				caseDepth--
				p.advance()
				continue
			}
			// Otherwise it's the block-form END — possibly followed by a
			// label identifier (DB2 supports `END label;`).
			p.advance()
			for !p.atStatementEnd() && p.cur.Kind != TOK_EOF &&
				!p.isKw("BEGIN") && !p.isKw("DECLARE") && !p.isKw("END") {
				p.advance()
			}
			depth--
			if depth <= 0 {
				return string(p.src[startOff:p.cur.Pos.Offset])
			}
		case p.cur.Kind == TOK_EOF:
			p.errorHere("unterminated block", "END")
			return string(p.src[startOff:])
		default:
			p.advance()
		}
	}
}

// captureUntilDelimiter reads verbatim until the next statement terminator.
func (p *Parser) captureUntilDelimiter() string {
	startOff := p.cur.Pos.Offset
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	return strings.TrimSpace(string(p.src[startOff:endOff]))
}

// captureBalancedParens reads from the current '(' up to and including the
// matching ')' and returns the raw slice (sans outer parens).
func (p *Parser) captureBalancedParens() string {
	if !p.isPunct("(") {
		p.errorHere("expected '('", "(")
		return ""
	}
	startOff := p.cur.Pos.Offset + 1 // skip '('
	depth := 0
	for {
		if p.cur.Kind == TOK_EOF {
			p.errorHere("unterminated '('", ")")
			return ""
		}
		if p.isPunct("(") {
			depth++
			p.advance()
			continue
		}
		if p.isPunct(")") {
			depth--
			if depth == 0 {
				endOff := p.cur.Pos.Offset
				p.advance() // consume ')'
				return string(p.src[startOff:endOff])
			}
			p.advance()
			continue
		}
		p.advance()
	}
}

func astPos(p Position) ast.Position {
	return ast.Position{Line: p.Line, Col: p.Col, Offset: p.Offset}
}

// intLit parses the current token as an int literal (with optional sign).
// Returns 0 and false when the current token is not numeric.
func (p *Parser) intLit() (int64, bool) {
	sign := int64(1)
	if p.isPunct("-") {
		sign = -1
		p.advance()
	} else if p.isPunct("+") {
		p.advance()
	}
	if p.cur.Kind != TOK_NUMBER {
		return 0, false
	}
	lit := p.cur.Lit
	p.advance()
	end := len(lit)
	for i, r := range lit {
		if r == '.' || r == 'e' || r == 'E' || r < '0' || r > '9' {
			end = i
			break
		}
	}
	digits := lit[:end]
	if digits == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, true
	}
	return sign * n, true
}
