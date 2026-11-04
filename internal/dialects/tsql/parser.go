// Package tsql implements a Microsoft SQL Server (T-SQL) DDL + DML +
// routine-body parser.
//
// The canonical grammar reference is grammars-v4 sql/tsql (MIT), vendored
// under reference/ (TSqlLexer.g4, TSqlParser.g4). This Go code is a
// hand-rolled recursive-descent parser implementing the subset of that
// grammar squishy needs to migrate a SQL Server database: the DDL of
// tables / indexes / views / routines / triggers / types / sequences /
// schemas, extended-property comments, the DML used inside views and
// routine bodies, and the procedural statements of T-SQL bodies. The .g4
// files are the source of truth; when the parser diverges from them, the
// .g4 wins and the Go code is updated to match. NO ANTLR runtime is linked.
//
// It produces the dialect-agnostic nodes of internal/sqlparse/ast, the
// same way the mysql package does, so the translator rewrites T-SQL with
// ast.Rewrite visitors and never inspects SQL text.
//
// Statement boundaries. T-SQL does not require statement terminators: the
// grammar alone says where a statement ends (`SET @a = 1 SELECT @a` is two
// statements). The parser therefore works on a pre-lexed token slice
// (comments dropped) and stops every construct at the first token its rule
// cannot consume; reserved words (token.go) end expressions and aliases.
// GO, the sqlcmd batch separator, ends a batch: CREATE VIEW / PROCEDURE /
// FUNCTION / TRIGGER bodies run up to the next GO (or the end of input),
// as SQL Server itself requires them to be alone in their batch.
//
// Entry point: Parse(src string) (stmts []ast.Stmt, errs ErrorList).
package tsql

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

type Parser struct {
	toks []Token
	i    int
	cur  Token
	src  []rune
	errs ErrorList

	// assigns records, for each query specification whose select list
	// uses the T-SQL variable-assignment form (`SELECT @a = x, @b = y …`),
	// the target variable of every select item (in item order). The
	// procedural dispatcher turns such a SELECT into an ast.SelectInto.
	assigns map[*ast.SelectStmt][]string
}

// newParser lexes src into a token slice (comments dropped). Lexical
// errors are reported once, here, and the offending token is skipped.
func newParser(src string) *Parser {
	p := &Parser{src: []rune(src), assigns: map[*ast.SelectStmt][]string{}}
	l := NewLexer(src)
	for {
		t := l.Next()
		if t.Kind == TOK_COMMENT {
			continue
		}
		if t.Kind == TOK_ERROR {
			p.errs = append(p.errs, &ParseError{Pos: t.Pos, Msg: t.Lit, Got: t.Raw})
			continue
		}
		p.toks = append(p.toks, t)
		if t.Kind == TOK_EOF {
			break
		}
	}
	p.cur = p.toks[0]
	return p
}

// subParser returns a parser over the token range [from, to) of p. Tokens
// keep their positions in the enclosing source and the range end reads as
// EOF, so the typed parse of an already delimited fragment (view body,
// inline function query, cursor query) can never run past the boundary
// the enclosing parser chose. Its diagnostics stay on the sub-parser.
func (p *Parser) subParser(from, to int) *Parser {
	if to > len(p.toks)-1 {
		to = len(p.toks) - 1
	}
	if from > to {
		from = to
	}
	toks := make([]Token, 0, to-from+1)
	toks = append(toks, p.toks[from:to]...)
	eof := Token{Kind: TOK_EOF, Pos: p.toks[to].Pos, End: p.toks[to].Pos.Offset}
	toks = append(toks, eof)
	sp := &Parser{toks: toks, src: p.src, assigns: p.assigns}
	sp.cur = sp.toks[0]
	return sp
}

// Parse parses a T-SQL script (sqlcmd layer included: GO, :setvar,
// $(var)) into a list of statements. Statements the migration does not
// exploit are returned as *ast.NoopStmt with their Kind and source Text,
// like the mysql parser does. Errors are collected; the parser
// resynchronises on ';', GO or the next statement keyword starting a line.
func Parse(src string) ([]ast.Stmt, ErrorList) {
	p := newParser(src)
	var stmts []ast.Stmt
	for p.cur.Kind != TOK_EOF {
		switch {
		case p.isPunct(";"), p.cur.Kind == TOK_GO:
			p.advance()
			continue
		case p.cur.Kind == TOK_SQLCMD:
			stmts = append(stmts, &ast.NoopStmt{Kind: ":" + p.cur.Lit, Text: p.cur.Raw, P: astPos(p.cur.Pos)})
			p.advance()
			continue
		}
		startIdx, nerr := p.i, len(p.errs)
		stmt := p.parseTopStatement()
		if stmt != nil {
			stmts = append(stmts, stmt)
		}
		if len(p.errs) > nerr || p.i == startIdx {
			p.syncStatement(startIdx)
		}
	}
	markTriggersOnViews(stmts)
	return stmts, p.errs
}

// markTriggersOnViews sets CreateTrigger.OnView for triggers whose target
// is a view created in the same script. The trigger syntax is the same for
// tables and views, so this is the only way to tell them apart without a
// catalog. Names compare case-insensitively (SQL Server's default
// collation); an omitted schema is dbo.
func markTriggersOnViews(stmts []ast.Stmt) {
	var views []ast.TableRef
	for _, s := range stmts {
		if v, ok := s.(*ast.CreateView); ok {
			views = append(views, v.View)
		}
	}
	if len(views) == 0 {
		return
	}
	schemaOf := func(s string) string {
		if s == "" {
			return "dbo"
		}
		return s
	}
	for _, s := range stmts {
		tr, ok := s.(*ast.CreateTrigger)
		if !ok {
			continue
		}
		for _, v := range views {
			if strings.EqualFold(v.Name, tr.Table.Name) && strings.EqualFold(schemaOf(v.Schema), schemaOf(tr.Table.Schema)) {
				tr.OnView = true
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// script-level statement dispatch
// ---------------------------------------------------------------------------

// parseTopStatement parses one script-level statement. DDL statements are
// typed; everything else (session settings, script-level control flow,
// data loading, administration) is parsed with the full statement grammar
// so its boundary is exact, then returned as an *ast.NoopStmt carrying the
// statement keyword and its source text.
func (p *Parser) parseTopStatement() ast.Stmt {
	start := p.cur
	switch {
	case p.isKw("CREATE"):
		return p.parseCreate()
	case p.isKw("ALTER"):
		return p.parseAlter()
	case p.isKw("DROP"):
		return p.parseDrop()
	case p.isKw("TRUNCATE"):
		return p.parseTruncate()
	case (p.isKw("EXEC") || p.isKw("EXECUTE")) && p.execTargetIs("sp_addextendedproperty"):
		return p.parseExtendedProperty()
	case p.isKw("USE"):
		p.advance()
		p.parseIdentAny()
		return p.noopFrom(start, "USE")
	case p.isKw("SET") && p.peek(1).Kind != TOK_VARIABLE:
		p.parseSetOption()
		return p.noopFrom(start, "SET")
	case p.isKw("GRANT") || p.isKw("DENY") || p.isKw("REVOKE") || p.isKw("DBCC") ||
		p.isKw("CHECKPOINT") || p.isKw("BACKUP") || p.isKw("RESTORE") || p.isKw("KILL") ||
		p.isKw("RECONFIGURE") || p.isKw("SHUTDOWN") || p.isKw("SETUSER") || p.isKw("REVERT") ||
		p.isKw("ENABLE") || p.isKw("DISABLE"):
		return p.parseNoopToStmtEnd(strings.ToUpper(p.cur.Lit), false)
	case p.isKw("BULK"):
		return p.parseBulkInsert()
	case p.cur.Kind == TOK_IDENT && !p.isContextualStmtWord() && !p.peekIsPunct(1, ":"):
		// The first statement of a batch may call a procedure without
		// EXEC (`sp_helpdb`, `dbo.p 1, 2`).
		p.parseExecBody(start.Pos, false)
		return p.noopFrom(start, "EXEC")
	}
	stmt := p.parsePLStmt()
	if stmt == nil {
		return nil
	}
	return p.noopFrom(start, strings.ToUpper(start.Lit))
}

// noopFrom builds the NoopStmt of a statement that started at start and
// ended with the previous token.
func (p *Parser) noopFrom(start Token, kind string) *ast.NoopStmt {
	return &ast.NoopStmt{Kind: kind, Text: p.textFrom(start), P: astPos(start.Pos)}
}

// textFrom returns the source text from start to the end of the previous
// token, trimmed.
func (p *Parser) textFrom(start Token) string {
	end := p.prevEnd()
	if end < start.Pos.Offset {
		return ""
	}
	return strings.TrimSpace(string(p.src[start.Pos.Offset:end]))
}

// prevEnd is the end offset of the token before the current one.
func (p *Parser) prevEnd() int {
	if p.i == 0 {
		return p.cur.Pos.Offset
	}
	return p.toks[p.i-1].End
}

// parseNoopToStmtEnd consumes the tokens of a statement the grammar does
// not need to model (administration, server objects, …) up to the
// statement end: ';', GO, end of input, or a statement keyword starting a
// line outside parentheses. When keepSet is true a SET starting a line
// does not end the statement (`ALTER DATABASE x` / `SET …` continuation).
func (p *Parser) parseNoopToStmtEnd(kind string, keepSet bool) ast.Stmt {
	start := p.cur
	p.advance()
	depth := 0
	for p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO {
		if depth == 0 {
			if p.isPunct(";") {
				break
			}
			if p.cur.LineStart && p.isStmtStartKeyword() && !(keepSet && p.isKw("SET")) {
				break
			}
		}
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") && depth > 0 {
			depth--
		}
		p.advance()
	}
	return p.noopFrom(start, kind)
}

// syncStatement skips to the next statement boundary after a parse error:
// ';' (consumed), GO, end of input, or a statement keyword that starts a
// line. It always makes progress, and never steps over the next
// statement's first token.
func (p *Parser) syncStatement(startIdx int) {
	if p.i == startIdx {
		p.advance()
	}
	for p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO {
		if p.isPunct(";") {
			p.advance()
			return
		}
		if p.cur.LineStart && p.isStmtStartKeyword() {
			return
		}
		p.advance()
	}
}

// isStmtStartKeyword reports whether the current token is a keyword that
// starts a statement (used only for error recovery and for the extent of
// statements that are kept as no-ops).
func (p *Parser) isStmtStartKeyword() bool {
	if p.cur.Kind != TOK_KEYWORD {
		return p.cur.Kind == TOK_IDENT && p.isContextualStmtWord()
	}
	switch p.cur.Lit {
	case "CREATE", "ALTER", "DROP", "EXEC", "EXECUTE", "PRINT", "USE", "DECLARE",
		"INSERT", "UPDATE", "DELETE", "GRANT", "DENY", "REVOKE", "IF", "BEGIN",
		"END", "RAISERROR", "BULK", "TRUNCATE", "MERGE", "SELECT", "WHILE",
		"RETURN", "SET", "ELSE", "COMMIT", "ROLLBACK", "SAVE", "DBCC",
		"CHECKPOINT", "BACKUP", "RESTORE", "OPEN", "CLOSE", "FETCH",
		"DEALLOCATE", "GOTO", "WAITFOR", "BREAK", "CONTINUE":
		return true
	}
	return false
}

// isContextualStmtWord reports whether the current identifier is one of
// the non-reserved words that start a statement (THROW, ENABLE / DISABLE
// TRIGGER, …). They must not be taken as an alias or a call argument of
// the previous statement.
func (p *Parser) isContextualStmtWord() bool {
	if p.cur.Kind != TOK_IDENT {
		return false
	}
	switch strings.ToUpper(p.cur.Lit) {
	case "THROW", "ENABLE", "DISABLE", "SEND", "RECEIVE", "MOVE", "GET":
		return true
	}
	return false
}

// parseSetOption consumes a session SET statement (`SET NOCOUNT ON`,
// `SET ANSI_NULLS, QUOTED_IDENTIFIER ON`, `SET DATEFORMAT mdy`,
// `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`,
// `SET IDENTITY_INSERT dbo.t ON`, `SET ROWCOUNT 10`, …) and returns its
// text. The current token is SET.
func (p *Parser) parseSetOption() string {
	start := p.cur
	p.advance() // SET
	if p.isKw("TRANSACTION") {
		p.advance()
		p.expectKw("ISOLATION")
		p.expectKw("LEVEL")
		switch {
		case p.isKw("READ"):
			p.advance()
			if p.isKw("COMMITTED") || p.isKw("UNCOMMITTED") {
				p.advance()
			} else {
				p.errorHere("expected COMMITTED or UNCOMMITTED", "COMMITTED|UNCOMMITTED")
			}
		case p.isKw("REPEATABLE"):
			p.advance()
			p.expectKw("READ")
		case p.isKw("SNAPSHOT") || p.isKw("SERIALIZABLE"):
			p.advance()
		default:
			p.errorHere("expected isolation level", "READ|REPEATABLE|SNAPSHOT|SERIALIZABLE")
		}
		return p.textFrom(start)
	}
	if p.cur.Kind != TOK_IDENT && p.cur.Kind != TOK_KEYWORD {
		p.errorHere("expected SET option", "option name")
		return p.textFrom(start)
	}
	// Option name list, then the value. Values are ON / OFF, a number, a
	// string, an identifier (DATEFORMAT mdy, LANGUAGE us_english) or, for
	// IDENTITY_INSERT / STATISTICS …, a (qualified) name followed by
	// ON / OFF.
	for p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("ON") || p.isKw("OFF"):
			p.advance()
			if p.isPunct(",") {
				p.advance()
				continue
			}
			return p.textFrom(start)
		case p.cur.Kind == TOK_NUMBER || p.cur.Kind == TOK_STRING || p.cur.Kind == TOK_VARIABLE || p.cur.Kind == TOK_HEX:
			p.advance()
			return p.textFrom(start)
		case p.isPunct("-"):
			p.advance()
		case p.isPunct("=") || p.isPunct(",") || p.isPunct("."):
			p.advance()
		case p.isPunct("("):
			p.skipBalancedParens()
		case p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_SQLCMD_VAR ||
			p.isKw("IDENTITY_INSERT") || p.isKw("STATISTICS") || p.isKw("ROWCOUNT") ||
			p.isKw("TEXTSIZE") || p.isKw("OFFSETS") || p.isKw("DEFAULT") || p.isKw("FILE") ||
			p.isKw("READ") || p.isKw("ALL"):
			if p.cur.LineStart && p.cur.Pos.Line > start.Pos.Line && !p.prevIsPunct(",") && !p.prevIsPunct("=") {
				return p.textFrom(start)
			}
			if p.isContextualStmtWord() {
				return p.textFrom(start)
			}
			p.advance()
		default:
			return p.textFrom(start)
		}
	}
	return p.textFrom(start)
}

// parseBulkInsert consumes `BULK INSERT table FROM 'file' [WITH (…)]`.
func (p *Parser) parseBulkInsert() ast.Stmt {
	start := p.cur
	p.expectKw("BULK")
	p.expectKw("INSERT")
	p.parseMultiPartName()
	p.expectKw("FROM")
	if p.cur.Kind == TOK_STRING || p.cur.Kind == TOK_VARIABLE || p.cur.Kind == TOK_SQLCMD_VAR {
		p.advance()
	} else {
		p.errorHere("expected data file", "STRING")
	}
	if p.isKw("WITH") && p.peekIsPunct(1, "(") {
		p.advance()
		p.skipBalancedParens()
	}
	return p.noopFrom(start, "BULK INSERT")
}

// parseTruncate parses `TRUNCATE TABLE name [WITH (PARTITIONS (…))]`. The
// node also implements ast.PLStmt (routine bodies reuse this entry point).
func (p *Parser) parseTruncate() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("TRUNCATE")
	p.expectKw("TABLE")
	ref := p.parseTableRef()
	if p.isKw("WITH") && p.peekIsPunct(1, "(") {
		p.advance()
		p.skipBalancedParens()
	}
	return &ast.TruncateTable{Table: ref, P: astPos(start)}
}

// ---------------------------------------------------------------------------
// low-level helpers
// ---------------------------------------------------------------------------

func (p *Parser) advance() {
	if p.i < len(p.toks)-1 {
		p.i++
	}
	p.cur = p.toks[p.i]
}

// peek returns the token n positions ahead of the current one (EOF past
// the end).
func (p *Parser) peek(n int) Token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}

// reset moves back to token index i (backtracking), dropping the errors
// recorded since nerr.
func (p *Parser) reset(i, nerr int) {
	p.i = i
	p.cur = p.toks[i]
	p.errs = p.errs[:nerr]
}

func (p *Parser) isPunct(lit string) bool {
	return p.cur.Kind == TOK_PUNCT && p.cur.Lit == lit
}

func (p *Parser) peekIsPunct(n int, lit string) bool {
	t := p.peek(n)
	return t.Kind == TOK_PUNCT && t.Lit == lit
}

func (p *Parser) prevIsPunct(lit string) bool {
	if p.i == 0 {
		return false
	}
	t := p.toks[p.i-1]
	return t.Kind == TOK_PUNCT && t.Lit == lit
}

// isKw reports whether the current token is the keyword kw: a reserved
// keyword, or an unquoted identifier spelling a contextual keyword
// (case-insensitive). kw must be upper-case.
func (p *Parser) isKw(kw string) bool {
	return tokIsKw(p.cur, kw)
}

func (p *Parser) peekIsKw(n int, kw string) bool {
	return tokIsKw(p.peek(n), kw)
}

func tokIsKw(t Token, kw string) bool {
	switch t.Kind {
	case TOK_KEYWORD:
		return t.Lit == kw
	case TOK_IDENT:
		return strings.EqualFold(t.Lit, kw)
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
	got := p.cur.Raw
	if p.cur.Kind == TOK_EOF {
		got = "EOF"
	}
	p.errs = append(p.errs, &ParseError{Pos: p.cur.Pos, Msg: msg, Got: got, Expected: expected})
}

func (p *Parser) errorAt(pos Position, msg string) {
	p.errs = append(p.errs, &ParseError{Pos: pos, Msg: msg})
}

// isIdentTok reports whether the current token can be an identifier:
// unquoted (non-reserved), [bracketed] / "quoted", or a sqlcmd $(var).
func (p *Parser) isIdentTok() bool {
	switch p.cur.Kind {
	case TOK_IDENT, TOK_QUOTED_IDENT, TOK_SQLCMD_VAR:
		return true
	}
	return false
}

// parseIdent parses one identifier. Reserved keywords are rejected (they
// are only identifiers when delimited). A sqlcmd variable is returned as
// its $(Name) spelling.
func (p *Parser) parseIdent() (string, bool) {
	switch p.cur.Kind {
	case TOK_IDENT:
		v := p.cur.Lit
		p.advance()
		return v, false
	case TOK_QUOTED_IDENT:
		v := p.cur.Lit
		p.advance()
		return v, true
	case TOK_SQLCMD_VAR:
		v := "$(" + p.cur.Lit + ")"
		p.advance()
		return v, false
	}
	p.errorHere("expected identifier", "IDENT")
	return "", false
}

// parseIdentAny is parseIdent that also accepts a reserved keyword (used
// where the grammar takes any word, e.g. after a '.' or for option names).
func (p *Parser) parseIdentAny() (string, bool) {
	if p.cur.Kind == TOK_KEYWORD {
		v := p.cur.Raw
		p.advance()
		return v, false
	}
	return p.parseIdent()
}

// parseMultiPartName parses `[server.][db.][schema.]name`; omitted middle
// parts (`db..t`) are returned as "". quoted reports whether any part was
// delimited.
func (p *Parser) parseMultiPartName() (parts []string, quoted bool) {
	name, q := p.parseIdent()
	parts = append(parts, name)
	quoted = q
	for p.isPunct(".") {
		p.advance()
		if p.isPunct(".") {
			parts = append(parts, "")
			continue
		}
		n, q2 := p.parseIdentAny()
		parts = append(parts, n)
		quoted = quoted || q2
	}
	return parts, quoted
}

// tableRefOf maps a multi-part name to a TableRef (the database / server
// parts are dropped: squishy migrates one database at a time).
func tableRefOf(parts []string, quoted bool) ast.TableRef {
	switch len(parts) {
	case 0:
		return ast.TableRef{}
	case 1:
		return ast.TableRef{Name: parts[0], NameBacktick: quoted}
	}
	return ast.TableRef{Schema: parts[len(parts)-2], Name: parts[len(parts)-1], NameBacktick: quoted}
}

// parseTableRef parses `[db.][schema.]name` (or a @table variable).
func (p *Parser) parseTableRef() ast.TableRef {
	if p.cur.Kind == TOK_VARIABLE {
		v := p.cur.Lit
		p.advance()
		return ast.TableRef{Name: v}
	}
	parts, quoted := p.parseMultiPartName()
	return tableRefOf(parts, quoted)
}

// parseIdentList parses `( id [ASC|DESC], … )` and returns the names.
func (p *Parser) parseIdentList() []string {
	var out []string
	if !p.expectPunct("(") {
		return nil
	}
	for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
		n, _ := p.parseIdent()
		out = append(out, n)
		if p.isKw("ASC") || p.isKw("DESC") {
			p.advance()
		}
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return out
}

// skipBalancedParens consumes a parenthesised group (the current token is
// '(') including nested groups.
func (p *Parser) skipBalancedParens() {
	if !p.isPunct("(") {
		return
	}
	depth := 0
	for p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO {
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
			if depth == 0 {
				p.advance()
				return
			}
		}
		p.advance()
	}
	p.errorHere("unbalanced parentheses", "')'")
}

// captureBalancedParens consumes a parenthesised group and returns its
// source text (parentheses included).
func (p *Parser) captureBalancedParens() string {
	start := p.cur
	p.skipBalancedParens()
	return p.textFrom(start)
}

// batchEnd returns the index of the token that ends the current batch (the
// next GO, or EOF).
func (p *Parser) batchEnd() int {
	for j := p.i; j < len(p.toks); j++ {
		if p.toks[j].Kind == TOK_GO || p.toks[j].Kind == TOK_EOF {
			return j
		}
	}
	return len(p.toks) - 1
}

// spanText returns the trimmed source text of tokens [from, to).
func (p *Parser) spanText(from, to int) string {
	if from >= to || from >= len(p.toks) {
		return ""
	}
	start := p.toks[from].Pos.Offset
	end := p.toks[to-1].End
	if end < start {
		return ""
	}
	return strings.TrimSpace(string(p.src[start:end]))
}

// astPos converts our Position to the ast package's one.
func astPos(p Position) ast.Position {
	return ast.Position{Line: p.Line, Col: p.Col, Offset: p.Offset}
}
