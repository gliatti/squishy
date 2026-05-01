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
		// capture the SELECT body until ';'
		sel := p.captureUntilStmtEnd()
		return &ast.DeclareCursor{Name: name, SelectBody: sel, P: astPos(start)}
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
	case p.isKw("SIGNAL"):
		return p.parseSignal()
	case p.isKw("SET"):
		return p.parseAssign()
	case p.isKw("SELECT"):
		// Procedural SELECT INTO (Oracle / MariaDB MySQL-compat) keeps the
		// dedicated SelectInto path because the AST tracks the captured
		// variables on a different node. Plain SELECT goes through the
		// structured DML parser and surfaces as a typed *ast.SelectStmt;
		// the translator's plTranslator.stmt() case renders it via
		// emitSelectStmt.
		if p.peekProceduralSelectInto() {
			return p.parseSelectInto()
		}
		return p.parseSelectStatement()
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
	return &ast.RawSQL{Text: p.captureUntilStmtEnd(), P: astPos(p.cur.Pos)}
}

func (p *Parser) peekIsPunct(lit string) bool {
	t := p.l.Peek()
	return t.Kind == TOK_PUNCT && t.Lit == lit
}

// peekProceduralSelectInto reports whether the SELECT statement starting at
// the current cursor position contains a procedural `INTO <var>[, <var>...]`
// clause before its FROM clause (or before the statement terminator if there
// is no FROM). The check walks the source text from the current rune offset
// — it does not consume tokens — and stops at depth-zero `;`, `)` or end of
// input. INTO inside a string literal or a nested parenthesis group is
// ignored. This is the discriminator between `SELECT a INTO @x FROM t` and a
// plain `SELECT a FROM t`: the former routes through parseSelectInto, the
// latter through the structured parseSelectStatement.
func (p *Parser) peekProceduralSelectInto() bool {
	if p.cur.Pos.Offset < 0 || p.cur.Pos.Offset >= len(p.src) {
		return false
	}
	src := p.src[p.cur.Pos.Offset:]
	depth := 0
	inStr := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
			continue
		case '(':
			depth++
			continue
		case ')':
			if depth == 0 {
				return false
			}
			depth--
			continue
		case ';':
			if depth == 0 {
				return false
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if !isPlSelectIntoIdentStart(c) {
			continue
		}
		j := i + 1
		for j < len(src) && isPlSelectIntoIdentByte(src[j]) {
			j++
		}
		word := strings.ToUpper(string(src[i:j]))
		switch word {
		case "INTO":
			return true
		case "FROM":
			return false
		}
		i = j - 1
	}
	return false
}

func isPlSelectIntoIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isPlSelectIntoIdentByte(r rune) bool {
	return isPlSelectIntoIdentStart(r) || (r >= '0' && r <= '9') || r == '$' || r == '#'
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

func (p *Parser) parseSignal() *ast.SignalStmt {
	start := p.cur.Pos
	p.expectKw("SIGNAL")
	s := &ast.SignalStmt{P: astPos(start)}
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
	// restore-ish: we can't rewind, so treat as raw SQL starting from the
	// consumed identifier.
	rest := p.captureUntilStmtEnd()
	return &ast.RawSQL{Text: saved.Lit + " " + rest, P: astPos(start)}
}

// parseSelectInto — SELECT <list> INTO <vars> FROM ... (rest as raw).
func (p *Parser) parseSelectInto() ast.PLStmt {
	// Easiest: capture the whole statement text, then extract INTO targets
	// with a small text pass. Works for the common shape without committing
	// to a full SELECT grammar yet.
	start := p.cur.Pos
	raw := p.captureUntilStmtEnd()
	// try to find " INTO " followed by comma-list of idents then whitespace
	// and another SQL keyword (FROM/WHERE). The regex-free split:
	up := strings.ToUpper(raw)
	idx := strings.Index(up, " INTO ")
	if idx < 0 {
		return &ast.RawSQL{Text: raw, P: astPos(start)}
	}
	head := raw[:idx]
	tail := raw[idx+len(" INTO "):]
	// vars go up to the next keyword boundary (FROM/WHERE/GROUP/ORDER/LIMIT)
	endIdx := len(tail)
	for _, kw := range []string{" FROM ", " WHERE ", " GROUP ", " ORDER ", " LIMIT "} {
		if j := strings.Index(strings.ToUpper(tail), kw); j >= 0 && j < endIdx {
			endIdx = j
		}
	}
	varsPart := tail[:endIdx]
	rest := strings.TrimSpace(tail[endIdx:])
	var vars []string
	for _, v := range strings.Split(varsPart, ",") {
		vars = append(vars, strings.TrimSpace(v))
	}
	return &ast.SelectInto{Vars: vars, RawQuery: strings.TrimSpace(head) + " " + rest, P: astPos(start)}
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
