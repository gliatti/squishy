package mysql

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ParseRoutineBody parses a MySQL procedural body (the content of CREATE
// PROCEDURE / FUNCTION / TRIGGER ... BEGIN ... END) into a tree of PLStmt
// nodes. A body that is a single simple statement (no BEGIN/END) is
// represented as a single-element slice wrapping that statement.
//
// Entry point for the translator's v2 procedural path. The canonical grammar
// reference is reference/MySqlParser.g4, procedureStatement rule and below.
func ParseRoutineBody(src string) ([]ast.PLStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmts, errs := p.parsePLBlockStmts(true /*atTopLevel*/)
	return stmts, errs
}

// parsePLBlockStmts parses one statement or a BEGIN…END block's statements.
// When atTopLevel is true the function accepts either shape; otherwise it
// expects a sequence until one of the block-ending keywords (END, ELSE,
// ELSEIF, WHEN, UNTIL).
func (p *Parser) parsePLBlockStmts(atTopLevel bool) ([]ast.PLStmt, ErrorList) {
	if atTopLevel && p.isKw("BEGIN") {
		blk := p.parseBlock("")
		return []ast.PLStmt{blk}, p.errs
	}
	var out []ast.PLStmt
	for {
		if p.cur.Kind == TOK_EOF {
			break
		}
		if p.isPlStopKeyword() {
			break
		}
		if stmt := p.parsePLStmt(); stmt != nil {
			out = append(out, stmt)
		}
		// consume trailing ';' if present
		if p.isPunct(";") {
			p.advance()
		}
		if atTopLevel && (p.cur.Kind == TOK_EOF || p.isStatementDelimiter()) {
			break
		}
	}
	return out, p.errs
}

func (p *Parser) isPlStopKeyword() bool {
	if p.cur.Kind != TOK_KEYWORD {
		return false
	}
	switch p.cur.Lit {
	case "END", "ELSE", "ELSEIF", "WHEN", "UNTIL":
		return true
	}
	return false
}

// parseBlock parses a BEGIN [DECLARE…]+ stmts END block.
func (p *Parser) parseBlock(label string) *ast.Block {
	start := p.cur.Pos
	p.expectKw("BEGIN")
	blk := &ast.Block{Label: label, P: astPos(start)}
	// Declarations must come first (MySQL rule); consume them greedily.
	for p.isKw("DECLARE") {
		if d := p.parsePLDecl(); d != nil {
			blk.Decls = append(blk.Decls, d)
		}
		if p.isPunct(";") {
			p.advance()
		}
	}
	for !p.isKw("END") && p.cur.Kind != TOK_EOF {
		if stmt := p.parsePLStmt(); stmt != nil {
			blk.Stmts = append(blk.Stmts, stmt)
		}
		if p.isPunct(";") {
			p.advance()
		}
	}
	if p.isKw("END") {
		p.advance()
	}
	// optional label trailing END
	if p.cur.Kind == TOK_IDENT && p.cur.Lit == label {
		p.advance()
	}
	return blk
}

// parsePLDecl dispatches to variable, cursor or handler declaration based on
// the token following DECLARE.
func (p *Parser) parsePLDecl() ast.PLDecl {
	start := p.cur.Pos
	p.expectKw("DECLARE")

	// DECLARE {CONTINUE|EXIT} HANDLER FOR ...
	if p.isKw("CONTINUE") || p.isKw("EXIT") {
		kind := p.cur.Lit
		p.advance()
		p.expectKw("HANDLER")
		p.expectKw("FOR")
		cond := p.parseHandlerCondition()
		action := p.parsePLStmt()
		return &ast.DeclareHandler{Kind: kind, Condition: cond, Action: action, P: astPos(start)}
	}

	// DECLARE <name> CURSOR FOR <select>
	// DECLARE <name> <type> [DEFAULT <expr>]
	name, _ := p.parseIdent()
	if p.isKw("CURSOR") {
		p.advance()
		p.expectKw("FOR")
		// Delimit the query up to ';' (SelectBody keeps that raw text), then
		// parse the delimited fragment as a selectStatement (grammar:
		// declareCursor — DECLARE uid CURSOR FOR selectStatement). Stmt is
		// nil when the query does not parse; the diagnostics stay in p.errs.
		bodyStart := p.cur.Pos
		body := p.captureUntilStmtEnd()
		sel, into, _ := p.parseDelimitedSelect(bodyStart, p.cur.Pos.Offset)
		if sel != nil && into != nil {
			p.errorAt(bodyStart, "SELECT … INTO is not allowed in a cursor declaration")
			sel = nil
		}
		return &ast.DeclareCursor{Name: name, Stmt: sel, SelectBody: body, P: astPos(start)}
	}
	// DECLARE list: `DECLARE a, b, c INT [DEFAULT 0]` — all share the same
	// type and default. Collect names.
	names := []string{name}
	for p.isPunct(",") {
		p.advance()
		n, _ := p.parseIdent()
		names = append(names, n)
	}
	typ := p.parseDataType()
	var def ast.Expr
	if p.isKw("DEFAULT") {
		p.advance()
		def = p.parseExpr()
	}
	// Build one declaration per name (same type + default).
	if len(names) == 1 {
		return &ast.DeclareVar{Name: names[0], Type: typ, Default: def, P: astPos(start)}
	}
	// Return the first one; queue the rest as additional declarations by
	// mutating the caller. Since parsePLDecl returns a single PLDecl, we
	// wrap the multi-declaration list into a DeclareVar with joined name
	// and let the emitter expand — simpler: emit just the first and warn.
	// (Multi-name DECLARE is rare; downgrading to single-name keeps the
	// AST clean.)
	return &ast.DeclareVar{Name: strings.Join(names, ","), Type: typ, Default: def, P: astPos(start)}
}

func (p *Parser) parseHandlerCondition() string {
	var b strings.Builder
	for p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("NOT") && p.peekLookaheadKw("FOUND"):
			p.advance()
			p.advance()
			b.WriteString("NOT FOUND")
			return b.String()
		case p.isKw("SQLEXCEPTION") || p.isKw("SQLWARNING"):
			s := p.cur.Lit
			p.advance()
			return s
		case p.isKw("SQLSTATE"):
			p.advance()
			// optional VALUE keyword
			if p.isKw("VALUE") {
				p.advance()
			}
			if p.cur.Kind == TOK_STRING {
				s := p.cur.Lit
				p.advance()
				return "SQLSTATE '" + s + "'"
			}
			return "SQLSTATE"
		default:
			p.advance()
		}
	}
	return ""
}

// parsePLStmt dispatches on the statement-start keyword.
func (p *Parser) parsePLStmt() ast.PLStmt {
	// [label:] prefix
	var label string
	if p.cur.Kind == TOK_IDENT && p.peekIsPunct(":") {
		label = p.cur.Lit
		p.advance() // ident
		p.advance() // ':'
	}
	switch {
	case p.isKw("BEGIN"):
		return p.parseBlock(label)
	case p.isKw("IF"):
		return p.parseIf()
	case p.isKw("CASE"):
		return p.parseCase()
	case p.isKw("WHILE"):
		return p.parseWhile(label)
	case p.isKw("LOOP"):
		return p.parseLoop(label)
	case p.isKw("REPEAT"):
		return p.parseRepeat(label)
	case p.isKw("LEAVE"):
		p.advance()
		n, _ := p.parseIdent()
		return &ast.LeaveStmt{Label: n, P: astPos(p.cur.Pos)}
	case p.isKw("ITERATE"):
		p.advance()
		n, _ := p.parseIdent()
		return &ast.IterateStmt{Label: n, P: astPos(p.cur.Pos)}
	case p.isKw("RETURN"):
		return p.parseReturn()
	case p.isKw("CALL"):
		return p.parseCall()
	case p.isKw("OPEN"):
		p.advance()
		n, _ := p.parseIdent()
		return &ast.OpenStmt{Cursor: n}
	case p.isKw("CLOSE"):
		p.advance()
		n, _ := p.parseIdent()
		return &ast.CloseStmt{Cursor: n}
	case p.isKw("FETCH"):
		return p.parseFetch()
	case p.isKw("SIGNAL") || p.isKw("RESIGNAL"):
		return p.parseSignal()
	case p.isKw("TRUNCATE"):
		return p.parseTruncate().(*ast.TruncateTable)
	case p.isKw("SET"):
		return p.parseAssign()
	case p.isKw("SELECT"):
		return p.parsePLSelect()
	case p.isKw("INSERT"):
		return p.parseInsertStatement()
	case p.isKw("UPDATE"):
		return p.parseUpdateStatement()
	case p.isKw("DELETE"):
		return p.parseDeleteStatement()
	case p.cur.Kind == TOK_IDENT:
		// bare identifier at statement start → likely an assign via `ident := expr`
		// or a function call (unusual in MySQL). Treat as assign/raw.
		return p.parseAssignOrRaw()
	}
	// fallback: capture raw up to ;
	// Nothing keyword-led is PL/pgSQL-compatible as-is (START TRANSACTION in
	// particular fails at runtime with "unsupported transaction command in
	// PL/pgSQL"), so the statement is "not understood" (Verbatim false).
	pos := p.cur.Pos
	return &ast.RawSQL{Text: p.captureUntilStmtEnd(), P: astPos(pos)}
}

// errorAt records a parse error anchored at pos.
func (p *Parser) errorAt(pos Position, msg string) {
	p.errs = append(p.errs, &ParseError{Pos: pos, Msg: msg})
}

// parseDelimitedSelect parses the source fragment [start.Offset, endOff) as
// one selectStatement with a bounded sub-parser. It returns the typed query
// (nil when the fragment does not parse or leaves unconsumed tokens), the
// procedural INTO variables the query carried (nil when none) and the rune
// offsets of that INTO clause. The sub-parser's diagnostics are appended to
// p.errs.
func (p *Parser) parseDelimitedSelect(start Position, endOff int) (*ast.SelectStmt, []string, [2]int) {
	sp := p.subParser(start, endOff)
	if sp.cur.Kind == TOK_EOF {
		p.errorAt(start, "expected SELECT")
		return nil, nil, [2]int{}
	}
	sel := sp.parseSelectStatement()
	if len(sp.errs) == 0 && sp.cur.Kind != TOK_EOF {
		if sp.isKw("INTO") {
			sp.errorHere("SELECT … INTO OUTFILE / DUMPFILE is not supported", "")
		} else {
			sp.errorHere("unexpected token after SELECT", "';'")
		}
	}
	p.errs = append(p.errs, sp.errs...)
	if len(sp.errs) > 0 {
		sel = nil
	}
	return sel, sp.pendingInto, sp.pendingIntoSpan
}

// parsePLSelect parses a SELECT statement inside a routine body. The
// statement is first delimited up to ';' (so a query the typed parser
// rejects never shifts the statement boundary), then parsed as a
// selectStatement. Outcomes:
//
//   - `SELECT … INTO v1, v2 …` (grammar: selectIntoExpression, variable
//     form — before FROM, after LIMIT or after the lock clause) →
//     *ast.SelectInto with Vars and the typed Stmt (INTO clause removed);
//     Stmt is nil when the query does not parse. RawQuery holds the source
//     text without the INTO clause — the legacy `SELECT <list> <rest>`
//     shape the text translator appends ` INTO <vars>` to.
//   - plain SELECT that parses → the typed *ast.SelectStmt.
//   - anything else (parse error, INTO OUTFILE / DUMPFILE, unsupported
//     trailing clause) → *ast.RawSQL with the statement text and
//     Verbatim false; the diagnostics stay in p.errs.
func (p *Parser) parsePLSelect() ast.PLStmt {
	start := p.cur.Pos
	raw := p.captureUntilStmtEnd()
	endOff := p.cur.Pos.Offset
	sel, into, span := p.parseDelimitedSelect(start, endOff)
	if into != nil {
		head := strings.TrimSpace(string(p.src[start.Offset:span[0]]))
		tail := strings.TrimSpace(string(p.src[span[1]:endOff]))
		query := head
		if tail != "" {
			query = head + " " + tail
		}
		return &ast.SelectInto{Vars: into, Stmt: sel, RawQuery: query, P: astPos(start)}
	}
	if sel == nil {
		return &ast.RawSQL{Text: raw, P: astPos(start)}
	}
	return sel
}

func (p *Parser) peekIsPunct(lit string) bool {
	t := p.l.Peek()
	return t.Kind == TOK_PUNCT && t.Lit == lit
}

// parseIf — IF cond THEN stmts [ELSEIF cond THEN stmts]* [ELSE stmts] END IF
func (p *Parser) parseIf() *ast.IfStmt {
	start := p.cur.Pos
	p.expectKw("IF")
	out := &ast.IfStmt{P: astPos(start)}
	branch := ast.IfBranch{Cond: p.parseExpr()}
	p.expectKw("THEN")
	branch.Body, _ = p.parsePLBlockStmts(false)
	out.Branches = append(out.Branches, branch)
	for p.isKw("ELSEIF") {
		p.advance()
		b := ast.IfBranch{Cond: p.parseExpr()}
		p.expectKw("THEN")
		b.Body, _ = p.parsePLBlockStmts(false)
		out.Branches = append(out.Branches, b)
	}
	if p.isKw("ELSE") {
		p.advance()
		out.Else, _ = p.parsePLBlockStmts(false)
	}
	p.expectKw("END")
	p.expectKw("IF")
	return out
}

// parseCase — CASE [expr] WHEN … THEN … [ELSE …] END CASE
func (p *Parser) parseCase() *ast.CaseStmt {
	start := p.cur.Pos
	p.expectKw("CASE")
	out := &ast.CaseStmt{P: astPos(start)}
	if !p.isKw("WHEN") {
		out.Expr = p.parseExpr()
	}
	for p.isKw("WHEN") {
		p.advance()
		w := ast.CaseWhen{Match: p.parseExpr()}
		p.expectKw("THEN")
		w.Body, _ = p.parsePLBlockStmts(false)
		out.When = append(out.When, w)
	}
	if p.isKw("ELSE") {
		p.advance()
		out.Else, _ = p.parsePLBlockStmts(false)
	}
	p.expectKw("END")
	p.expectKw("CASE")
	return out
}

func (p *Parser) parseWhile(label string) *ast.WhileStmt {
	start := p.cur.Pos
	p.expectKw("WHILE")
	w := &ast.WhileStmt{Label: label, P: astPos(start)}
	w.Cond = p.parseExpr()
	p.expectKw("DO")
	w.Body, _ = p.parsePLBlockStmts(false)
	p.expectKw("END")
	p.expectKw("WHILE")
	// trailing label
	if p.cur.Kind == TOK_IDENT && p.cur.Lit == label {
		p.advance()
	}
	return w
}

func (p *Parser) parseLoop(label string) *ast.LoopStmt {
	start := p.cur.Pos
	p.expectKw("LOOP")
	l := &ast.LoopStmt{Label: label, P: astPos(start)}
	l.Body, _ = p.parsePLBlockStmts(false)
	p.expectKw("END")
	p.expectKw("LOOP")
	if p.cur.Kind == TOK_IDENT && p.cur.Lit == label {
		p.advance()
	}
	return l
}

func (p *Parser) parseRepeat(label string) *ast.RepeatStmt {
	start := p.cur.Pos
	p.expectKw("REPEAT")
	r := &ast.RepeatStmt{Label: label, P: astPos(start)}
	r.Body, _ = p.parsePLBlockStmts(false)
	p.expectKw("UNTIL")
	r.Cond = p.parseExpr()
	p.expectKw("END")
	p.expectKw("REPEAT")
	if p.cur.Kind == TOK_IDENT && p.cur.Lit == label {
		p.advance()
	}
	return r
}

func (p *Parser) parseReturn() *ast.ReturnStmt {
	start := p.cur.Pos
	p.expectKw("RETURN")
	r := &ast.ReturnStmt{P: astPos(start)}
	if !p.isPunct(";") && !p.atStatementEnd() {
		r.Expr = p.parseExpr()
	}
	return r
}

func (p *Parser) parseCall() *ast.CallStmt {
	start := p.cur.Pos
	p.expectKw("CALL")
	name, _ := p.parseIdent()
	c := &ast.CallStmt{Name: name, P: astPos(start)}
	if p.isPunct(".") {
		p.advance()
		second, _ := p.parseIdent()
		c.Schema = name
		c.Name = second
	}
	if p.isPunct("(") {
		p.advance()
		if !p.isPunct(")") {
			c.Args = append(c.Args, p.parseExpr())
			for p.isPunct(",") {
				p.advance()
				c.Args = append(c.Args, p.parseExpr())
			}
		}
		p.expectPunct(")")
	}
	return c
}

func (p *Parser) parseFetch() *ast.FetchStmt {
	start := p.cur.Pos
	p.expectKw("FETCH")
	// optional NEXT / FROM
	if p.isKw("NEXT") {
		p.advance()
	}
	if p.isKw("FROM") {
		p.advance()
	}
	name, _ := p.parseIdent()
	f := &ast.FetchStmt{Cursor: name, P: astPos(start)}
	p.expectKw("INTO")
	n, _ := p.parseIdent()
	f.Into = append(f.Into, n)
	for p.isPunct(",") {
		p.advance()
		n, _ = p.parseIdent()
		f.Into = append(f.Into, n)
	}
	return f
}

// parseSignal parses SIGNAL and RESIGNAL (grammar: signalStatement,
// resignalStatement): `[RE]SIGNAL [SQLSTATE [VALUE] 'xxxxx'] [SET item =
// value, …]`. RESIGNAL sets Resignal; both SQLSTATE and SET MESSAGE_TEXT
// are optional there.
func (p *Parser) parseSignal() *ast.SignalStmt {
	start := p.cur.Pos
	s := &ast.SignalStmt{P: astPos(start)}
	if p.isKw("RESIGNAL") {
		s.Resignal = true
		p.advance()
	} else {
		p.expectKw("SIGNAL")
	}
	if p.isKw("SQLSTATE") {
		p.advance()
		if p.isKw("VALUE") {
			p.advance()
		}
		if p.cur.Kind == TOK_STRING {
			s.SQLState = p.cur.Lit
			p.advance()
		}
	}
	// SIGNAL ... SET <item> = <value> [, <item> = <value>]*
	// The diagnostic-area item names (MESSAGE_TEXT, MYSQL_ERRNO, …) are
	// registered as keywords; accept either keyword or ident here. We
	// lift MESSAGE_TEXT into the SignalStmt's Message field; other items
	// are consumed but not currently propagated (PG RAISE has no direct
	// equivalent for them — they would need USING <option> = <expr>).
	if p.isKw("SET") {
		p.advance()
		for {
			itemKw := ""
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
				itemKw = p.cur.Lit
				p.advance()
			}
			if p.isPunct("=") {
				p.advance()
			}
			if p.cur.Kind == TOK_STRING {
				if strings.EqualFold(itemKw, "MESSAGE_TEXT") {
					s.Message = p.cur.Lit
				}
				p.advance()
			} else {
				// numeric or ident-valued item (e.g. MYSQL_ERRNO = 1234) —
				// just skip the value token.
				if p.cur.Kind == TOK_NUMBER || p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
					p.advance()
				}
			}
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	return s
}

func (p *Parser) parseAssign() *ast.AssignStmt {
	start := p.cur.Pos
	p.expectKw("SET")
	target := p.parseAssignTarget()
	if p.isPunct("=") || (p.cur.Kind == TOK_PUNCT && p.cur.Lit == ":=") {
		p.advance()
	}
	e := p.parseExpr()
	return &ast.AssignStmt{Target: target, Expr: e, P: astPos(start)}
}

// parseAssignTarget reads a possibly qualified assignment target (NEW.col,
// OLD.col, @var, simple var).
func (p *Parser) parseAssignTarget() string {
	var b strings.Builder
	if p.isPunct("@") {
		b.WriteByte('@')
		p.advance()
	}
	if p.cur.Kind == TOK_KEYWORD && (p.cur.Lit == "NEW" || p.cur.Lit == "OLD") {
		b.WriteString(p.cur.Lit)
		p.advance()
		if p.isPunct(".") {
			b.WriteByte('.')
			p.advance()
			n, _ := p.parseIdent()
			b.WriteString(n)
		}
		return b.String()
	}
	n, _ := p.parseIdent()
	b.WriteString(n)
	return b.String()
}

func (p *Parser) parseAssignOrRaw() ast.PLStmt {
	start := p.cur.Pos
	// peek for `:=` or `=` after ident → assign
	saved := p.cur
	name, _ := p.parseIdent()
	if p.isPunct(":=") || p.isPunct("=") {
		p.advance()
		e := p.parseExpr()
		return &ast.AssignStmt{Target: name, Expr: e, P: astPos(start)}
	}
	// Not an assignment: keep the statement as raw SQL. Only the statements
	// PL/pgSQL accepts unchanged are Verbatim (non-reserved words, lexed as
	// IDENT); anything else is "not understood":
	//   - bare COMMIT / ROLLBACK (grammar rules commitWork / rollbackWork
	//     without WORK, AND CHAIN, RELEASE or TO SAVEPOINT). PL/pgSQL has no
	//     SAVEPOINT / RELEASE SAVEPOINT / ROLLBACK TO SAVEPOINT;
	//   - GET [CURRENT] DIAGNOSTICS whose items are all `var = ROW_COUNT`
	//     (see getDiagnosticsVerbatim).
	if strings.EqualFold(saved.Lit, "GET") {
		return p.parseGetDiagnosticsRaw(saved, start)
	}
	verbatim := (strings.EqualFold(saved.Lit, "COMMIT") || strings.EqualFold(saved.Lit, "ROLLBACK")) &&
		p.atStatementEnd()
	rest := p.captureUntilStmtEnd()
	return &ast.RawSQL{Text: joinRawHead(saved.Lit, rest), Verbatim: verbatim, P: astPos(start)}
}

// joinRawHead rebuilds a raw statement from its already-consumed leading word
// and the captured remainder, without a trailing blank when the remainder is
// empty (bare `COMMIT`).
func joinRawHead(head, rest string) string {
	if rest == "" {
		return head
	}
	return head + " " + rest
}

// parseGetDiagnosticsRaw captures a GET … DIAGNOSTICS statement (grammar rule
// getDiagnostics) as RawSQL, the leading GET already consumed. It is Verbatim
// only in the one shape PL/pgSQL accepts unchanged:
//
//	GET [CURRENT] DIAGNOSTICS v = ROW_COUNT [, w = ROW_COUNT …]
//
// PG's GET DIAGNOSTICS has no CONDITION n form, no NUMBER item and no @user /
// @@system variable targets, and PG's STACKED form only exposes the error
// items of an exception handler, so MySQL's `GET DIAGNOSTICS CONDITION 1
// @p = MESSAGE_TEXT` handler idiom (and every other shape) stays Verbatim
// false for the translator to handle or warn about.
func (p *Parser) parseGetDiagnosticsRaw(saved Token, start Position) ast.PLStmt {
	startOff := p.cur.Pos.Offset
	verbatim := p.getDiagnosticsVerbatim()
	// Resync on the statement end from wherever the classifier stopped.
	p.captureUntilStmtEnd()
	rest := strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
	return &ast.RawSQL{Text: joinRawHead(saved.Lit, rest), Verbatim: verbatim, P: astPos(start)}
}

// getDiagnosticsVerbatim consumes the tokens after GET while they match the
// PL/pgSQL-compatible GET [CURRENT] DIAGNOSTICS v = ROW_COUNT [, …] shape and
// reports whether the whole statement matched. It stops (leaving the rest for
// the caller to capture) at the first token that does not fit.
func (p *Parser) getDiagnosticsVerbatim() bool {
	if p.isWord("CURRENT") {
		p.advance()
	}
	if !p.isWord("DIAGNOSTICS") {
		return false
	}
	p.advance()
	for {
		// Target: a plain local variable. `@x` / `@@x` start with PUNCT.
		// Quoted (backtick) names are left to the translator: the raw text
		// would reach PG with MySQL quoting.
		if p.cur.Kind != TOK_IDENT {
			return false
		}
		p.advance()
		if !p.isPunct("=") {
			return false
		}
		p.advance()
		if !p.isWord("ROW_COUNT") {
			return false
		}
		p.advance()
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return p.atStatementEnd()
}

// isWord reports whether the current token is the word w, lexed either as a
// keyword or as a non-reserved identifier, compared case-insensitively on
// that single token.
func (p *Parser) isWord(w string) bool {
	return (p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT) && strings.EqualFold(p.cur.Lit, w)
}

// captureUntilStmtEnd returns the source text from current position up to,
// but not including, the terminating ';' or the current statement delimiter.
func (p *Parser) captureUntilStmtEnd() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		// Track paren depth so a ';' inside a function call stops us only
		// at the right level.
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
		}
		if depth < 0 {
			break
		}
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	return strings.TrimSpace(string(p.src[startOff:endOff]))
}
