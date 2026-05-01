// Package oracle — parser entry point.
//
// Spec source of truth: internal/dialects/oracle/reference/PlSql*.g4.
// This file only wires the top-level dispatch and shared helpers. Actual
// statement parsing lives in parser_ddl.go / parser_plsql.go.
package oracle

import (
	"math"
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

// Parse parses an Oracle script into a list of statements. It accepts ";"
// and "/" (line-start) as statement terminators and synchronizes on either
// when a parse error occurs.
func Parse(src string) ([]ast.Stmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	var stmts []ast.Stmt
	for {
		// Skip stray delimiters and comments at script level. TOK_SLASH_TERM
		// is the canonical SQL*Plus block terminator, but when DBMS_METADATA
		// output is concatenated the lone '/' sometimes lands mid-buffer
		// without a leading newline (e.g. after our script-level ';\n\n'
		// separator), in which case the lexer tokenizes it as a plain PUNCT.
		// Accept both forms.
		if p.isPunct(";") || p.isPunct("/") || p.cur.Kind == TOK_SLASH_TERM {
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
	case p.isKw("COMMENT"):
		return p.parseNoopToStmtEnd("COMMENT")
	case p.isKw("GRANT") || p.isKw("REVOKE") || p.isKw("ANALYZE"):
		return p.parseNoopToStmtEnd(p.cur.Lit)
	case p.isKw("BEGIN") || p.isKw("DECLARE") || p.cur.Kind == TOK_LABEL_START:
		return p.parseAnonymousBlock()
	case p.isKw("SET"):
		return p.parseSet()
	case p.isAnyKwOrIdent("AUDIT", "NOAUDIT"):
		return p.parseNoopToStmtEnd("AUDIT")
	case p.isAnyKwOrIdent("FLASHBACK"):
		return p.parseNoopToStmtEnd("FLASHBACK")
	case p.isAnyKwOrIdent("PURGE"):
		return p.parseNoopToStmtEnd("PURGE")
	case p.isKw("LOCK") || p.isAnyKwOrIdent("ASSOCIATE", "DISASSOCIATE"):
		return p.parseNoopToStmtEnd(p.cur.Lit)
	}
	p.errorHere("unexpected token at statement start", "CREATE|ALTER|DROP|BEGIN|DECLARE")
	return nil
}

// parseCreate dispatches on the object kind following CREATE [OR REPLACE].
func (p *Parser) parseCreate() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("CREATE")
	orReplace := false
	if p.isKw("OR") {
		p.advance()
		p.expectKw("REPLACE")
		orReplace = true
	}

	// CREATE PUBLIC SYNONYM ...
	if p.isKw("PUBLIC") {
		p.advance()
		return p.parseCreateSynonym(start, orReplace, true)
	}

	// Global temp tables, bitmap indexes, etc. have a qualifier before the kind.
	isGlobalTemp := false
	isBitmap := false
	isUnique := false
	isMaterialized := false
	// Oracle 21c+/23c table qualifiers — captured into the TableOptions
	// downstream so the translator can surface a per-qualifier
	// explanation. None translate directly (PG has no IMMUTABLE /
	// BLOCKCHAIN / SHARDED / DUPLICATED / PRIVATE TEMPORARY constructs).
	isImmutable := false
	isBlockchain := false
	isSharded := false
	isDuplicated := false
	isPrivateTemp := false

	for {
		switch {
		case p.isKw("GLOBAL"):
			p.advance()
			if p.isKw("TEMPORARY") {
				p.advance()
				isGlobalTemp = true
				continue
			}
		case p.isAnyKwOrIdent("PRIVATE"):
			p.advance()
			if p.isKw("TEMPORARY") {
				p.advance()
				isPrivateTemp = true
				continue
			}
		case p.isKw("UNIQUE"):
			p.advance()
			isUnique = true
			continue
		case p.isKw("BITMAP"):
			p.advance()
			isBitmap = true
			continue
		case p.isKw("MATERIALIZED"):
			p.advance()
			isMaterialized = true
			continue
		case p.isAnyKwOrIdent("IMMUTABLE"):
			p.advance()
			isImmutable = true
			continue
		case p.isAnyKwOrIdent("BLOCKCHAIN"):
			p.advance()
			isBlockchain = true
			continue
		case p.isAnyKwOrIdent("SHARDED"):
			p.advance()
			isSharded = true
			continue
		case p.isAnyKwOrIdent("DUPLICATED"):
			p.advance()
			isDuplicated = true
			continue
		// Optional modifiers emitted by DBMS_METADATA.GET_DDL on Oracle 12c+ :
		// FORCE/NOFORCE (views) and EDITIONABLE/NONEDITIONABLE (views, triggers,
		// procedures, packages, types). They don't affect the target PG schema,
		// so we simply skip them.
		case p.isKw("FORCE"), p.isKw("NOFORCE"),
			p.isKw("EDITIONABLE"), p.isKw("NONEDITIONABLE"):
			p.advance()
			continue
		}
		break
	}
	// Bundle the table-only qualifier flags so parseCreateTable can stamp
	// them onto TableOptions without growing its signature for every new
	// qualifier we recognise.
	tblQuals := tableQualifiers{
		GlobalTemp:  isGlobalTemp,
		PrivateTemp: isPrivateTemp,
		Immutable:   isImmutable,
		Blockchain:  isBlockchain,
		Sharded:     isSharded,
		Duplicated:  isDuplicated,
	}

	switch {
	case p.isKw("TABLE"):
		return p.parseCreateTableQuals(start, tblQuals)
	case p.isKw("INDEX"):
		return p.parseCreateIndex(start, isUnique, isBitmap)
	case p.isKw("VIEW"):
		if isMaterialized {
			// Distinguish CREATE MATERIALIZED VIEW LOG ON tbl … (no PG
			// counterpart — PG MVs refresh via REFRESH MATERIALIZED VIEW
			// [CONCURRENTLY], no per-base-table change-log sidecar) from a
			// regular CREATE MATERIALIZED VIEW.
			pk := p.peek()
			if (pk.Kind == TOK_KEYWORD || pk.Kind == TOK_IDENT) &&
				strings.EqualFold(pk.Lit, "LOG") {
				p.advance() // VIEW
				p.advance() // LOG
				return p.parseNoopToStmtEnd("CREATE MATERIALIZED VIEW LOG")
			}
			return p.parseCreateMaterializedView(start, orReplace)
		}
		return p.parseCreateView(start, orReplace)
	case isMaterialized && p.isKw("VIEW"):
		return p.parseCreateMaterializedView(start, orReplace)
	case p.isKw("SEQUENCE"):
		return p.parseCreateSequence(start, orReplace)
	case p.isKw("SYNONYM"):
		return p.parseCreateSynonym(start, orReplace, false)
	case p.isKw("TRIGGER"):
		return p.parseCreateTrigger(start, orReplace)
	case p.isKw("PROCEDURE"):
		return p.parseCreateProcedure(start, orReplace)
	case p.isKw("FUNCTION"):
		return p.parseCreateFunction(start, orReplace)
	case p.isKw("PACKAGE"):
		return p.parseCreatePackage(start, orReplace)
	case p.isKw("TYPE"):
		return p.parseCreateType(start, orReplace)
	}
	// Oracle-only object kinds without a PG counterpart — captured as a
	// NoopStmt with a tagged Kind so translateNoop can surface a tailored
	// info-level explanation per category. None of these emit DDL on the
	// PG side; the user gets remediation guidance via the explanation.
	if p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT {
		switch strings.ToUpper(p.cur.Lit) {
		case "CLUSTER":
			return p.parseNoopToStmtEnd("CREATE CLUSTER")
		case "CONTEXT":
			return p.parseNoopToStmtEnd("CREATE CONTEXT")
		case "DIRECTORY":
			return p.parseNoopToStmtEnd("CREATE DIRECTORY")
		case "LIBRARY":
			return p.parseNoopToStmtEnd("CREATE LIBRARY")
		case "JAVA":
			return p.parseNoopToStmtEnd("CREATE JAVA")
		case "DATABASE":
			// CREATE DATABASE LINK or plain CREATE DATABASE.
			pk := p.peek()
			if (pk.Kind == TOK_KEYWORD || pk.Kind == TOK_IDENT) &&
				strings.EqualFold(pk.Lit, "LINK") {
				return p.parseNoopToStmtEnd("CREATE DATABASE LINK")
			}
			return p.parseNoopToStmtEnd("CREATE DATABASE")
		case "PROFILE":
			return p.parseNoopToStmtEnd("CREATE PROFILE")
		case "LOCKDOWN":
			return p.parseNoopToStmtEnd("CREATE LOCKDOWN PROFILE")
		case "EDITION":
			return p.parseNoopToStmtEnd("CREATE EDITION")
		case "ATTRIBUTE":
			return p.parseNoopToStmtEnd("CREATE ATTRIBUTE DIMENSION")
		case "HIERARCHY":
			return p.parseNoopToStmtEnd("CREATE HIERARCHY")
		case "ANALYTIC":
			return p.parseNoopToStmtEnd("CREATE ANALYTIC VIEW")
		case "FLASHBACK":
			return p.parseNoopToStmtEnd("CREATE FLASHBACK ARCHIVE")
		case "AUDIT":
			return p.parseNoopToStmtEnd("CREATE AUDIT POLICY")
		case "TABLESPACE":
			return p.parseNoopToStmtEnd("CREATE TABLESPACE")
		case "ROLE":
			return p.parseNoopToStmtEnd("CREATE ROLE")
		case "USER":
			return p.parseNoopToStmtEnd("CREATE USER")
		case "INDEXTYPE":
			return p.parseNoopToStmtEnd("CREATE INDEXTYPE")
		case "OPERATOR":
			return p.parseNoopToStmtEnd("CREATE OPERATOR")
		case "PLUGGABLE":
			return p.parseNoopToStmtEnd("CREATE PLUGGABLE DATABASE")
		case "SCHEMA":
			return p.parseNoopToStmtEnd("CREATE SCHEMA")
		case "RESTORE":
			return p.parseNoopToStmtEnd("CREATE RESTORE POINT")
		}
	}
	p.errorHere("unsupported CREATE variant",
		"TABLE|INDEX|VIEW|SEQUENCE|SYNONYM|TRIGGER|PROCEDURE|FUNCTION|PACKAGE|TYPE")
	return nil
}

func (p *Parser) parseDrop() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("DROP")
	if p.isKw("TABLE") {
		p.advance()
		s := &ast.DropTable{P: astPos(start)}
		if p.isKw("IF") {
			p.advance()
			p.expectKw("EXISTS")
			s.IfExists = true
		}
		for {
			ref := p.parseTableRef()
			s.Tables = append(s.Tables, ref)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		// Consume optional CASCADE CONSTRAINTS / PURGE.
		for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
			p.advance()
		}
		return s
	}
	// Non-TABLE drops: classify the kind and emit a DropObject. The
	// translator maps these to PG equivalents (with explanations for kinds
	// PG doesn't have, like SYNONYM and PACKAGE).
	switch {
	case p.isKw("INDEX"):
		return p.parseDropObject(start, "INDEX", false)
	case p.isKw("VIEW"):
		return p.parseDropObject(start, "VIEW", true) // multi-target list
	case p.isKw("MATERIALIZED"):
		p.advance()
		if !p.isKw("VIEW") {
			return p.parseNoopToStmtEnd("DROP MATERIALIZED")
		}
		return p.parseDropObject(start, "MATERIALIZED VIEW", false)
	case p.isKw("SEQUENCE"):
		return p.parseDropObject(start, "SEQUENCE", false)
	case p.isKw("SYNONYM"):
		return p.parseDropObject(start, "SYNONYM", false)
	case p.isKw("PROCEDURE"):
		return p.parseDropObject(start, "PROCEDURE", false)
	case p.isKw("FUNCTION"):
		return p.parseDropObject(start, "FUNCTION", false)
	case p.isKw("PACKAGE"):
		// `DROP PACKAGE [BODY] name`
		p.advance() // PACKAGE
		if p.isKw("BODY") {
			p.advance()
			return p.parseDropObjectAfterKw(start, "PACKAGE BODY", false)
		}
		return p.parseDropObjectAfterKw(start, "PACKAGE", false)
	case p.isKw("TRIGGER"):
		return p.parseDropObject(start, "TRIGGER", false)
	case p.isKw("TYPE"):
		// `DROP TYPE [BODY] name [FORCE]`
		p.advance() // TYPE
		if p.isKw("BODY") {
			p.advance()
			return p.parseDropObjectAfterKw(start, "TYPE BODY", false)
		}
		return p.parseDropObjectAfterKw(start, "TYPE", false)
	}
	return p.parseNoopToStmtEnd("DROP")
}

// parseDropObject is the generic Oracle DROP-non-TABLE parser. It handles
// the standard `DROP <kind> [IF EXISTS] [schema.]name [, …]` shape; the
// caller passes the kind keyword (which has not yet been consumed) and a
// flag indicating whether multiple targets are accepted in a single
// statement.
func (p *Parser) parseDropObject(start Position, kind string, multi bool) ast.Stmt {
	p.advance() // consume the kind keyword
	return p.parseDropObjectAfterKw(start, kind, multi)
}

// parseDropObjectAfterKw is the same but expects the kind keyword to
// already be consumed (used for two-token kinds like PACKAGE BODY).
func (p *Parser) parseDropObjectAfterKw(start Position, kind string, multi bool) ast.Stmt {
	s := &ast.DropObject{Kind: kind, P: astPos(start)}
	if p.isKw("IF") {
		p.advance()
		if p.isKw("EXISTS") {
			p.advance()
		}
		s.IfExists = true
	}
	if multi {
		for {
			schema, name := p.parseQualifiedName()
			s.Names = append(s.Names, ast.TableRef{Schema: schema, Name: name})
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		// Single-target convenience: also fill Name when only one was given.
		if len(s.Names) == 1 {
			s.Name = s.Names[0].Name
			s.Schema = s.Names[0].Schema
		}
	} else {
		s.Schema, s.Name = p.parseQualifiedName()
	}
	// Trailing options: CASCADE [CONSTRAINTS] / RESTRICT / FORCE / PURGE.
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("CASCADE"):
			s.Cascade = true
			p.advance()
		case p.isKw("RESTRICT"):
			s.Restrict = true
			p.advance()
		default:
			p.advance()
		}
	}
	return s
}

func (p *Parser) parseSet() ast.Stmt {
	start := p.cur.Pos
	var b strings.Builder
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		if p.cur.Kind == TOK_STRING {
			b.WriteByte('\'')
			b.WriteString(p.cur.Lit)
			b.WriteByte('\'')
		} else {
			b.WriteString(p.cur.Lit)
		}
		b.WriteByte(' ')
		p.advance()
	}
	return &ast.NoopStmt{Kind: "SET", Text: strings.TrimSpace(b.String()), P: astPos(start)}
}

func (p *Parser) parseNoopToStmtEnd(kind string) ast.Stmt {
	start := p.cur.Pos
	var b strings.Builder
	b.WriteString(kind)
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		b.WriteString(" ")
		b.WriteString(p.cur.Lit)
		p.advance()
	}
	return &ast.NoopStmt{Kind: kind, Text: b.String(), P: astPos(start)}
}

// ---------------------------------------------------------------------------
// low-level helpers
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
	p.errs = append(p.errs, &ParseError{
		Pos: p.cur.Pos, Msg: msg, Got: p.cur.Lit, Expected: expected,
	})
}

// parseIdent accepts IDENT / QUOTED_IDENT / permissive-keyword.
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
		// Oracle allows most keywords as identifiers in lax contexts.
		v := p.cur.Lit
		p.advance()
		return v, false
	}
	p.errorHere("expected identifier", "IDENT")
	return "", false
}

// parseTableRef parses [schema.]name.
func (p *Parser) parseTableRef() ast.TableRef {
	name, bt := p.parseIdent()
	if p.isPunct(".") {
		p.advance()
		second, bt2 := p.parseIdent()
		return ast.TableRef{Schema: name, Name: second, NameBacktick: bt || bt2}
	}
	return ast.TableRef{Name: name, NameBacktick: bt}
}

// parseQualifiedName parses [schema.]name and returns its two parts as a tuple.
func (p *Parser) parseQualifiedName() (schema, name string) {
	first, _ := p.parseIdent()
	if p.isPunct(".") {
		p.advance()
		second, _ := p.parseIdent()
		return first, second
	}
	return "", first
}

func (p *Parser) atStatementEnd() bool {
	return p.cur.Kind == TOK_EOF || p.isPunct(";") || p.isPunct("/") || p.cur.Kind == TOK_SLASH_TERM
}

func (p *Parser) consumeStatementEnd() {
	for p.isPunct(";") || p.isPunct("/") || p.cur.Kind == TOK_SLASH_TERM {
		p.advance()
	}
}

func (p *Parser) syncToDelimiter() {
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// Raw body capture — used by CREATE TRIGGER / PROCEDURE / FUNCTION / PACKAGE
// whose bodies are captured verbatim and re-parsed later by parser_plsql.go
// via ParseBlock().
// ---------------------------------------------------------------------------

// captureBlock reads a PL/SQL block starting at BEGIN or DECLARE with matched
// nesting for IF/CASE/LOOP, and returns the raw text from the current token
// up to (and including) the matching END [name];.
func (p *Parser) captureBlock() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	// First keyword defines opening.
	switch {
	case p.isKw("DECLARE"), p.isKw("BEGIN"):
		// proceed
	default:
		// Accept IS/AS followed by BEGIN (function/procedure body).
	}
	caseDepth := 0
	for {
		switch {
		case p.isKw("BEGIN"), p.isKw("LOOP"), p.isKw("IF"):
			depth++
			p.advance()
		case p.isKw("CASE"):
			// Track CASE in its own counter so we can correctly close
			// expression-form `CASE … END;` (which terminates with a bare
			// END followed by an expression continuation, often `;`)
			// without confusing it with the block-form `BEGIN … END;`.
			caseDepth++
			p.advance()
		case p.isKw("END"):
			next := p.peek()
			// END CASE; — statement-form CASE close. Pairs with the
			// `case p.isKw("CASE")` arm; doesn't affect block depth.
			if next.Kind == TOK_KEYWORD && next.Lit == "CASE" {
				if caseDepth > 0 {
					caseDepth--
				}
				p.advance() // END
				p.advance() // CASE
				for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
					p.advance()
				}
				continue
			}
			// END IF; / END LOOP; — control-flow close. Pairs with the
			// IF/LOOP arm above; decrements the block depth.
			if next.Kind == TOK_KEYWORD && (next.Lit == "IF" || next.Lit == "LOOP") {
				p.advance() // END
				p.advance() // IF/LOOP
				for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
					p.advance()
				}
				depth--
				if depth <= 0 {
					return string(p.src[startOff:p.cur.Pos.Offset])
				}
				continue
			}
			// Bare END inside an open CASE expression: closes that
			// expression. The trailing `;`, `)`, `,`, `||`, etc. is
			// consumed by the next loop iteration as a normal token.
			if caseDepth > 0 {
				caseDepth--
				p.advance()
				continue
			}
			// Otherwise it's the block-form END (BEGIN…END[ label];).
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
	lit := strings.TrimRight(p.cur.Lit, "fFdD")
	p.advance()
	// Truncate at the first non-integer rune so things like "1.5" or "1e3"
	// behave like the previous loop did (return the integer prefix).
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
		// Overflow: Oracle's default sequence MAXVALUE is 10^28-1 (28 nines),
		// far past int64. Clamp to the PG bigint range so the translator
		// emits a sane MAXVALUE / MINVALUE instead of a wrapped-negative one.
		if sign < 0 {
			return math.MinInt64, true
		}
		return math.MaxInt64, true
	}
	return sign * n, true
}
