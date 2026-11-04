package tsql

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// Procedural T-SQL (grammar rules sql_clauses, cfl_statement,
// declare_statement, execute_statement, cursor_statement,
// transaction_statement, …) mapped onto the shared ast PL nodes:
//
//	BEGIN … END                      → *ast.Block
//	BEGIN TRY … END TRY
//	BEGIN CATCH … END CATCH          → *ast.Block{TryCatch, Except: OTHERS}
//	DECLARE @a t [= e], @t TABLE (…) → *ast.DeclareStmt{*ast.DeclareVar…}
//	DECLARE c CURSOR … FOR select    → *ast.DeclareStmt{*ast.DeclareCursor}
//	SET @v = e / SET @v += e         → *ast.AssignStmt (compound expanded)
//	SET NOCOUNT ON, SET … options    → *ast.NullStmt{Source, Warn}
//	SELECT @a = x, @b = y …          → *ast.SelectInto (other SELECTs stay typed)
//	IF c s [ELSE s]                  → *ast.IfStmt (ELSE IF chains flattened)
//	WHILE c s                        → *ast.WhileStmt
//	BREAK / CONTINUE                 → *ast.LeaveStmt / *ast.IterateStmt
//	RETURN [e]                       → *ast.ReturnStmt
//	GOTO l / l:                      → *ast.GotoStmt / *ast.LabelStmt
//	THROW / RAISERROR / PRINT        → *ast.RaiseStmt
//	EXEC proc …                      → *ast.CallStmt
//	EXEC (@sql) / sp_executesql      → *ast.ExecuteImmediateStmt
//	OPEN / FETCH / CLOSE / DEALLOCATE → *ast.OpenStmt / FetchStmt / CloseStmt
//	BEGIN TRAN / COMMIT / ROLLBACK / SAVE TRAN → *ast.TransactionStmt
//	WAITFOR …                        → *ast.NullStmt{Warn}
//	INSERT / UPDATE / DELETE / MERGE / TRUNCATE → the typed DML nodes
//	CREATE / ALTER / DROP …          → *ast.EmbeddedDDL
// ---------------------------------------------------------------------------

// ParseRoutineBody parses a T-SQL routine body (the text after AS of
// CREATE PROCEDURE / FUNCTION / TRIGGER: `BEGIN … END` or a bare
// statement sequence) into procedural nodes. A body that is one
// BEGIN … END block is returned as a single-element slice.
func ParseRoutineBody(src string) ([]ast.PLStmt, ErrorList) {
	p := newParser(src)
	stmts := p.parseStmtList(func() bool { return p.cur.Kind == TOK_EOF || p.cur.Kind == TOK_GO })
	if p.cur.Kind == TOK_GO {
		p.errorHere("GO inside a routine body", "end of body")
	}
	return stmts, p.errs
}

// parseStmtList parses statements until stop() (or a block-closing END
// the caller handles). Errors resynchronise on the next statement.
func (p *Parser) parseStmtList(stop func() bool) []ast.PLStmt {
	var out []ast.PLStmt
	for {
		for p.isPunct(";") {
			p.advance()
		}
		if stop() || p.cur.Kind == TOK_EOF || p.cur.Kind == TOK_GO {
			return out
		}
		if p.isKw("END") {
			// END without an opening BEGIN at this level.
			p.errorHere("unexpected END", "statement")
			p.advance()
			continue
		}
		startIdx, nerr := p.i, len(p.errs)
		if st := p.parsePLStmt(); st != nil {
			out = append(out, st)
		}
		if len(p.errs) > nerr || p.i == startIdx {
			p.syncStatement(startIdx)
		}
	}
}

// parseSingleStmt parses the one statement governed by IF / ELSE / WHILE.
func (p *Parser) parseSingleStmt() ast.PLStmt {
	if p.cur.Kind == TOK_EOF || p.cur.Kind == TOK_GO || p.isKw("END") || p.isKw("ELSE") {
		p.errorHere("expected statement", "statement")
		return nil
	}
	return p.parsePLStmt()
}

// parsePLStmt dispatches one procedural statement.
func (p *Parser) parsePLStmt() ast.PLStmt {
	start := p.cur
	pos := astPos(start.Pos)
	switch {
	case p.isKw("BEGIN"):
		return p.parseBegin()
	case p.isKw("DECLARE"):
		return p.parseDeclare()
	case p.isKw("SET"):
		return p.parseSetStmt()
	case p.isKw("IF"):
		return p.parseIf()
	case p.isKw("WHILE"):
		p.advance()
		w := &ast.WhileStmt{Cond: p.parseExpr(), P: pos}
		if body := p.parseSingleStmt(); body != nil {
			w.Body = []ast.PLStmt{body}
		}
		return w
	case p.isKw("BREAK"):
		p.advance()
		return &ast.LeaveStmt{P: pos}
	case p.isKw("CONTINUE"):
		p.advance()
		return &ast.IterateStmt{P: pos}
	case p.isKw("RETURN"):
		p.advance()
		r := &ast.ReturnStmt{P: pos}
		if p.canStartExpr() {
			r.Expr = p.parseExpr()
		}
		return r
	case p.isKw("GOTO"):
		p.advance()
		label, _ := p.parseIdent()
		return &ast.GotoStmt{Label: label, P: pos}
	case p.isKw("EXEC") || p.isKw("EXECUTE"):
		return p.parseExec()
	case p.isKw("PRINT"):
		p.advance()
		return &ast.RaiseStmt{Kind: "PRINT", Level: "NOTICE", Message: p.parseExpr(), P: pos}
	case p.isKw("RAISERROR"):
		return p.parseRaiserror()
	case p.isKw("THROW") && p.cur.Kind == TOK_IDENT:
		return p.parseThrow()
	case p.isKw("SELECT") || (p.isPunct("(") && p.startsQuery()):
		return p.parsePLSelect(nil)
	case p.isKw("WITH"):
		return p.parseWithStatement()
	case p.isKw("INSERT"):
		return p.parseInsertStatement(nil)
	case p.isKw("UPDATE") && p.peekIsKw(1, "STATISTICS"):
		return p.nullFromNoop(start, "UPDATE STATISTICS")
	case p.isKw("UPDATE"):
		return p.parseUpdateStatement(nil)
	case p.isKw("DELETE"):
		return p.parseDeleteStatement(nil)
	case p.isKw("MERGE"):
		return p.parseMergeStatement(nil)
	case p.isKw("TRUNCATE"):
		return p.parseTruncate().(*ast.TruncateTable)
	case p.isKw("CREATE"):
		return &ast.EmbeddedDDL{Stmt: p.parseCreate(), P: pos}
	case p.isKw("ALTER"):
		return &ast.EmbeddedDDL{Stmt: p.parseAlter(), P: pos}
	case p.isKw("DROP"):
		return &ast.EmbeddedDDL{Stmt: p.parseDrop(), P: pos}
	case p.isKw("OPEN"):
		p.advance()
		return &ast.OpenStmt{Cursor: p.parseCursorName(), P: pos}
	case p.isKw("FETCH"):
		return p.parseFetch()
	case p.isKw("CLOSE"):
		p.advance()
		return &ast.CloseStmt{Cursor: p.parseCursorName(), P: pos}
	case p.isKw("DEALLOCATE"):
		p.advance()
		return &ast.CloseStmt{Cursor: p.parseCursorName(), Deallocate: true, P: pos}
	case p.isKw("COMMIT") || p.isKw("ROLLBACK") || p.isKw("SAVE"):
		return p.parseTransaction()
	case p.isKw("WAITFOR"):
		p.advance()
		if p.isKw("DELAY") || p.isKw("TIME") {
			p.advance()
			p.parseExpr()
		} else if p.isPunct("(") {
			p.skipBalancedParens()
		}
		if p.isPunct(",") && p.peekIsKw(1, "TIMEOUT") {
			p.advance()
			p.advance()
			p.parseExpr()
		}
		return &ast.NullStmt{Source: p.textFrom(start), Warn: true, P: pos}
	case p.isKw("USE") || p.isKw("GRANT") || p.isKw("DENY") || p.isKw("REVOKE") ||
		p.isKw("DBCC") || p.isKw("CHECKPOINT") || p.isKw("RECONFIGURE") || p.isKw("KILL") ||
		p.isKw("SETUSER") || p.isKw("REVERT") || p.isKw("BACKUP") || p.isKw("RESTORE") ||
		p.isKw("ENABLE") || p.isKw("DISABLE"):
		return p.nullFromNoop(start, strings.ToUpper(start.Lit))
	case p.isKw("BULK"):
		p.parseBulkInsert()
		return &ast.NullStmt{Source: p.textFrom(start), Warn: true, P: pos}
	case p.cur.Kind == TOK_IDENT && p.peekIsPunct(1, ":") && !p.peekIsPunct(2, ":"):
		label := p.cur.Lit
		p.advance()
		p.advance()
		return &ast.LabelStmt{Label: label, P: pos}
	}
	p.errorHere("unexpected token at statement start", "statement")
	return nil
}

// nullFromNoop consumes a statement with no procedural meaning on PG
// (administration, statistics, …) and returns it as a NullStmt to warn
// about.
func (p *Parser) nullFromNoop(start Token, kind string) ast.PLStmt {
	p.parseNoopToStmtEnd(kind, false)
	return &ast.NullStmt{Source: p.textFrom(start), Warn: true, P: astPos(start.Pos)}
}

// canStartExpr reports whether the current token can begin an expression
// that belongs to the current statement (RETURN [expr], THROW [args]).
func (p *Parser) canStartExpr() bool {
	switch p.cur.Kind {
	case TOK_NUMBER, TOK_STRING, TOK_HEX, TOK_MONEY, TOK_VARIABLE, TOK_QUOTED_IDENT, TOK_SQLCMD_VAR:
		return true
	case TOK_IDENT:
		return !p.isContextualStmtWord() && !p.peekIsPunct(1, ":")
	case TOK_PUNCT:
		return p.isPunct("(") || p.isPunct("-") || p.isPunct("+") || p.isPunct("~")
	case TOK_KEYWORD:
		switch p.cur.Lit {
		case "NULL", "CASE", "CONVERT", "TRY_CONVERT", "EXISTS", "NOT":
			return true
		}
		return niladicKeywords[p.cur.Lit] || (keywordFuncs[p.cur.Lit] && p.peekIsPunct(1, "("))
	}
	return false
}

// ---------------------------------------------------------------------------
// BEGIN … END / TRY … CATCH / BEGIN TRAN
// ---------------------------------------------------------------------------

func (p *Parser) parseBegin() ast.PLStmt {
	start := p.cur
	pos := astPos(start.Pos)
	p.expectKw("BEGIN")
	switch {
	case p.isKw("TRY"):
		return p.parseTryCatch(pos)
	case p.isKw("TRAN") || p.isKw("TRANSACTION") || p.isKw("DISTRIBUTED"):
		ts := &ast.TransactionStmt{Kind: "BEGIN", P: pos}
		if p.isKw("DISTRIBUTED") {
			ts.Distributed = true
			p.advance()
		}
		p.advance() // TRAN | TRANSACTION
		ts.Name = p.parseOptionalTranName()
		if p.isKw("WITH") && p.peekIsKw(1, "MARK") {
			p.advance()
			p.advance()
			if p.cur.Kind == TOK_STRING {
				p.advance()
			}
		}
		return ts
	case p.isKw("ATOMIC"):
		// natively compiled module: BEGIN ATOMIC WITH (…)
		p.advance()
		if p.isKw("WITH") {
			p.advance()
			p.skipBalancedParens()
		}
	}
	blk := &ast.Block{P: pos}
	blk.Stmts = p.parseStmtList(func() bool { return p.isKw("END") })
	p.expectKw("END")
	return blk
}

// parseTryCatch parses `TRY stmts END TRY [;] BEGIN CATCH stmts END CATCH`
// (BEGIN already consumed).
func (p *Parser) parseTryCatch(pos ast.Position) ast.PLStmt {
	p.expectKw("TRY")
	blk := &ast.Block{TryCatch: true, P: pos}
	blk.Stmts = p.parseStmtList(func() bool { return p.isKw("END") })
	p.expectKw("END")
	p.expectKw("TRY")
	for p.isPunct(";") {
		p.advance()
	}
	exStart := p.cur.Pos
	p.expectKw("BEGIN")
	p.expectKw("CATCH")
	catch := p.parseStmtList(func() bool { return p.isKw("END") })
	p.expectKw("END")
	p.expectKw("CATCH")
	blk.Except = &ast.ExceptionBlock{
		Handlers: []ast.ExceptionHandler{{Names: []string{"OTHERS"}, Body: catch}},
		P:        astPos(exStart),
	}
	return blk
}

// parseOptionalTranName parses the optional transaction / savepoint name
// after TRAN[SACTION].
func (p *Parser) parseOptionalTranName() string {
	switch {
	case p.cur.Kind == TOK_VARIABLE:
		v := p.cur.Lit
		p.advance()
		return v
	case p.cur.Kind == TOK_QUOTED_IDENT && !p.cur.LineStart,
		p.cur.Kind == TOK_IDENT && !p.cur.LineStart && !p.isContextualStmtWord() && !p.peekIsPunct(1, ":"):
		v, _ := p.parseIdent()
		return v
	}
	return ""
}

// parseTransaction parses COMMIT / ROLLBACK / SAVE.
func (p *Parser) parseTransaction() ast.PLStmt {
	pos := astPos(p.cur.Pos)
	kind := p.cur.Lit
	p.advance()
	ts := &ast.TransactionStmt{Kind: kind, P: pos}
	switch {
	case p.isKw("TRAN") || p.isKw("TRANSACTION"):
		p.advance()
		ts.Name = p.parseOptionalTranName()
	case p.isKw("WORK"):
		p.advance()
	default:
		if kind == "SAVE" {
			p.errorHere("expected TRANSACTION", "TRAN|TRANSACTION")
		}
	}
	if p.isKw("WITH") && p.peekIsPunct(1, "(") {
		// COMMIT … WITH (DELAYED_DURABILITY = ON)
		p.advance()
		p.skipBalancedParens()
	}
	return ts
}

// ---------------------------------------------------------------------------
// DECLARE / SET
// ---------------------------------------------------------------------------

// cursorOptions are the DECLARE CURSOR options (ISO and T-SQL syntax).
var cursorOptions = map[string]bool{
	"LOCAL": true, "GLOBAL": true, "FORWARD_ONLY": true, "SCROLL": true, "STATIC": true,
	"KEYSET": true, "DYNAMIC": true, "FAST_FORWARD": true, "READ_ONLY": true,
	"SCROLL_LOCKS": true, "OPTIMISTIC": true, "TYPE_WARNING": true, "INSENSITIVE": true,
}

// parseDeclare parses a DECLARE statement into a DeclareStmt holding one
// declaration per declared name.
func (p *Parser) parseDeclare() ast.PLStmt {
	pos := astPos(p.cur.Pos)
	p.expectKw("DECLARE")
	ds := &ast.DeclareStmt{P: pos}
	if p.cur.Kind != TOK_VARIABLE {
		if dc := p.parseDeclareCursor(); dc != nil {
			ds.Decls = append(ds.Decls, dc)
		}
		return ds
	}
	for {
		vstart := p.cur.Pos
		if p.cur.Kind != TOK_VARIABLE {
			p.errorHere("expected @variable", "@name")
			return ds
		}
		dv := &ast.DeclareVar{Name: p.cur.Lit, P: astPos(vstart)}
		p.advance()
		if p.isKw("AS") {
			p.advance()
		}
		if p.isKw("TABLE") {
			p.advance()
			dv.Type = p.parseTableTypeBody(p.cur.Pos)
		} else {
			dv.Type = p.parseDataType()
			if p.isPunct("=") {
				p.advance()
				dv.Default = p.parseExpr()
			}
		}
		ds.Decls = append(ds.Decls, dv)
		if !p.isPunct(",") {
			return ds
		}
		p.advance()
	}
}

// parseDeclareCursor parses `name [INSENSITIVE] [SCROLL] CURSOR [options]
// FOR select [FOR {READ ONLY | UPDATE [OF cols]}]` (DECLARE consumed).
func (p *Parser) parseDeclareCursor() *ast.DeclareCursor {
	pos := astPos(p.cur.Pos)
	name, _ := p.parseIdent()
	dc := &ast.DeclareCursor{Name: name, P: pos}
	for p.cur.Kind == TOK_IDENT && cursorOptions[strings.ToUpper(p.cur.Lit)] {
		dc.Options = append(dc.Options, strings.ToUpper(p.cur.Lit))
		p.advance()
	}
	p.expectKw("CURSOR")
	for p.cur.Kind == TOK_IDENT && cursorOptions[strings.ToUpper(p.cur.Lit)] {
		dc.Options = append(dc.Options, strings.ToUpper(p.cur.Lit))
		p.advance()
	}
	p.expectKw("FOR")
	from := p.i
	dc.Stmt = p.parseSelectStatement()
	dc.SelectBody = p.spanText(from, p.i)
	if p.isKw("FOR") && (p.peekIsKw(1, "READ") || p.peekIsKw(1, "UPDATE")) {
		fstart := p.cur
		p.advance()
		if p.isKw("READ") {
			p.advance()
			p.expectKw("ONLY")
			dc.Options = append(dc.Options, "READ_ONLY")
		} else {
			p.advance() // UPDATE
			if p.isKw("OF") {
				p.advance()
				for {
					p.parseIdent()
					if !p.isPunct(",") {
						break
					}
					p.advance()
				}
			}
			dc.ForUpdate = p.textFrom(fstart)
		}
	}
	return dc
}

// parseSetStmt parses SET: a variable assignment (`SET @v = e`,
// `SET @v += e`), or a session option kept as a NullStmt.
func (p *Parser) parseSetStmt() ast.PLStmt {
	start := p.cur
	pos := astPos(start.Pos)
	if p.peek(1).Kind == TOK_VARIABLE {
		p.advance() // SET
		v := p.cur
		p.advance()
		if p.isPunct(".") {
			// SET @x.modify(…) / SET @geo.STSrid = 4326: a mutator on a
			// CLR / xml variable.
			p.advance()
			method, _ := p.parseIdentAny()
			var e ast.Expr
			if p.isPunct("(") {
				e = p.finishMethodCall(v, &ast.Ident{Parts: []string{v.Lit}, Variable: true, P: astPos(v.Pos)}, method)
			} else {
				p.expectPunct("=")
				e = &ast.MethodCall{Target: &ast.Ident{Parts: []string{v.Lit}, Variable: true, P: astPos(v.Pos)}, Method: method, Args: []ast.Expr{p.parseExpr()}, P: astPos(v.Pos)}
			}
			return &ast.AssignStmt{Target: v.Lit, Expr: e, P: pos}
		}
		op := p.cur.Lit
		if !p.isPunct("=") && compoundOps[op] == "" {
			p.errorHere("expected '=' after SET @variable", "'='")
			return nil
		}
		p.advance()
		if p.isKw("CURSOR") {
			p.errorHere("cursor variables assigned with SET @c = CURSOR are not supported", "expression")
			return nil
		}
		e := p.parseExpr()
		if bop := compoundOps[op]; bop != "" {
			e = &ast.BinaryExpr{Op: bop, Lhs: &ast.Ident{Parts: []string{v.Lit}, Variable: true, P: astPos(v.Pos)}, Rhs: e, P: astPos(v.Pos)}
		}
		return &ast.AssignStmt{Target: v.Lit, Expr: e, P: pos}
	}
	optIdx := p.i + 1
	text := p.parseSetOption()
	warn := true
	if optIdx < p.i {
		opt := strings.ToUpper(p.toks[optIdx].Lit)
		last := p.toks[p.i-1]
		switch {
		case opt == "NOCOUNT":
			warn = false
		case opt == "XACT_ABORT" && tokIsKw(last, "ON"):
			warn = false
		}
	}
	return &ast.NullStmt{Source: text, Warn: warn, P: pos}
}

// ---------------------------------------------------------------------------
// IF / SELECT / WITH
// ---------------------------------------------------------------------------

// parseIf parses `IF cond stmt [;] [ELSE stmt]`. An ELSE whose statement
// is itself an IF (ELSE IF chain) is flattened into Branches.
func (p *Parser) parseIf() ast.PLStmt {
	pos := astPos(p.cur.Pos)
	p.expectKw("IF")
	out := &ast.IfStmt{P: pos}
	br := ast.IfBranch{Cond: p.parseExpr()}
	if body := p.parseSingleStmt(); body != nil {
		br.Body = []ast.PLStmt{body}
	}
	out.Branches = append(out.Branches, br)
	if p.isPunct(";") && p.peekIsKw(1, "ELSE") {
		p.advance()
	}
	if p.isKw("ELSE") {
		p.advance()
		els := p.parseSingleStmt()
		if nested, ok := els.(*ast.IfStmt); ok {
			out.Branches = append(out.Branches, nested.Branches...)
			out.Else = nested.Else
		} else if els != nil {
			out.Else = []ast.PLStmt{els}
		}
	}
	return out
}

// parsePLSelect parses a SELECT inside a body. A select list in the
// variable-assignment form becomes an *ast.SelectInto whose Stmt keeps the
// assigned expressions as its columns; any other SELECT stays typed.
func (p *Parser) parsePLSelect(with *ast.WithClause) ast.PLStmt {
	from := p.i
	var sel *ast.SelectStmt
	if with == nil {
		sel = p.parseSelectStatement()
	} else {
		sel = p.parseQueryExpression()
		if sel != nil {
			sel.With = with
			p.parseSelectTail(sel)
		}
	}
	if sel == nil {
		return nil
	}
	if vars := p.assigns[sel]; vars != nil {
		return &ast.SelectInto{Vars: vars, Stmt: sel, RawQuery: p.spanText(from, p.i), P: sel.P}
	}
	return sel
}

// parseWithStatement parses a statement led by a CTE prelude: WITH …
// {SELECT | INSERT | UPDATE | DELETE | MERGE}.
func (p *Parser) parseWithStatement() ast.PLStmt {
	with := p.parseWithClause()
	switch {
	case p.isKw("SELECT") || p.isPunct("("):
		return p.parsePLSelect(with)
	case p.isKw("INSERT"):
		return p.parseInsertStatement(with)
	case p.isKw("UPDATE"):
		return p.parseUpdateStatement(with)
	case p.isKw("DELETE"):
		return p.parseDeleteStatement(with)
	case p.isKw("MERGE"):
		return p.parseMergeStatement(with)
	}
	p.errorHere("expected SELECT, INSERT, UPDATE, DELETE or MERGE after WITH", "SELECT|INSERT|UPDATE|DELETE|MERGE")
	return nil
}

// ---------------------------------------------------------------------------
// EXEC
// ---------------------------------------------------------------------------

// parseExec parses an EXEC / EXECUTE statement.
func (p *Parser) parseExec() ast.PLStmt {
	return p.parseExecBody(p.cur.Pos, true)
}

// parseExecBody parses
//
//	EXEC[UTE] [@rc =] {proc | @proc_var} [arg [, arg]*] [WITH RECOMPILE | RESULT SETS …]
//	EXEC[UTE] ( string_expr [+ …] ) [AS {LOGIN|USER} = '…'] [AT linked_server]
//	EXEC[UTE] sp_executesql @stmt [, @params [, args…]]
//
// hasKeyword is false for a batch-leading procedure call without EXEC.
func (p *Parser) parseExecBody(pos Position, hasKeyword bool) ast.PLStmt {
	start := p.cur
	if hasKeyword {
		p.advance() // EXEC | EXECUTE
	}
	if p.isPunct("(") {
		p.advance()
		e := p.parseExpr()
		for p.isPunct(",") {
			// EXEC ('… ?', @p) AT linked_server: pass-through parameters.
			p.advance()
			p.parseExpr()
		}
		p.expectPunct(")")
		if p.isKw("AS") && (p.peekIsKw(1, "LOGIN") || p.peekIsKw(1, "USER")) {
			p.advance()
			p.advance()
			p.expectPunct("=")
			p.parseExpr()
		}
		if p.isKw("AT") {
			p.advance()
			p.parseIdent()
		}
		return &ast.ExecuteImmediateStmt{SQL: e, P: astPos(pos)}
	}
	if p.isKw("AS") && (p.peekIsKw(1, "LOGIN") || p.peekIsKw(1, "USER") || p.peekIsKw(1, "CALLER")) {
		// EXECUTE AS … : execution-context switch, no PG counterpart.
		p.advance()
		p.advance()
		if p.isPunct("=") {
			p.advance()
			p.parseExpr()
		}
		for p.isKw("WITH") || p.isPunct(",") {
			p.advance()
			p.parseIdentAny()
		}
		return &ast.NullStmt{Source: p.textFrom(start), Warn: true, P: astPos(pos)}
	}
	call := &ast.CallStmt{P: astPos(pos)}
	if p.cur.Kind == TOK_VARIABLE && p.peekIsPunct(1, "=") {
		call.ReturnVar = p.cur.Lit
		p.advance()
		p.advance()
	}
	if p.cur.Kind == TOK_VARIABLE {
		call.Name = p.cur.Lit
		p.advance()
	} else {
		parts, _ := p.parseMultiPartName()
		ref := tableRefOf(parts, false)
		call.Schema, call.Name = ref.Schema, ref.Name
		if len(parts) >= 3 && parts[len(parts)-2] == "" {
			call.Schema = "" // db..proc
		}
	}
	call.Args = p.parseExecArgs()
	if p.isKw("WITH") && (p.peekIsKw(1, "RECOMPILE") || p.peekIsKw(1, "RESULT")) {
		p.advance()
		if p.isKw("RECOMPILE") {
			p.advance()
		} else {
			p.advance() // RESULT
			p.expectKw("SETS")
			if p.isPunct("(") {
				p.skipBalancedParens()
			} else {
				p.advance() // NONE | UNDEFINED
			}
		}
	}
	if strings.EqualFold(call.Name, "sp_executesql") {
		ei := &ast.ExecuteImmediateStmt{SPExecuteSQL: true, P: astPos(pos)}
		for i, a := range call.Args {
			if na, ok := a.(*ast.NamedArg); ok && !na.Output {
				switch strings.ToLower(na.Name) {
				case "@stmt", "@statement":
					ei.SQL = na.Value
					continue
				case "@params":
					ei.ParamDecl = na.Value
					continue
				}
			}
			switch {
			case i == 0 && ei.SQL == nil:
				ei.SQL = a
			case i == 1 && ei.ParamDecl == nil:
				ei.ParamDecl = a
			default:
				ei.Using = append(ei.Using, a)
			}
		}
		return ei
	}
	return call
}

// canStartExecArg reports whether the current token can begin a procedure
// argument (the argument list is optional and EXEC needs no terminator).
func (p *Parser) canStartExecArg() bool {
	switch p.cur.Kind {
	case TOK_STRING, TOK_NUMBER, TOK_HEX, TOK_MONEY, TOK_VARIABLE, TOK_QUOTED_IDENT, TOK_SQLCMD_VAR:
		return true
	case TOK_IDENT:
		return !p.cur.LineStart && !p.isContextualStmtWord() && !p.peekIsPunct(1, ":")
	case TOK_PUNCT:
		return p.isPunct("-") || p.isPunct("+")
	case TOK_KEYWORD:
		return p.isKw("NULL") || p.isKw("DEFAULT")
	}
	return false
}

// parseExecArgs parses `arg [, arg]*` where arg is
// `[@param =] {value | @var [OUTPUT] | DEFAULT}`. Named and OUTPUT
// arguments are *ast.NamedArg.
func (p *Parser) parseExecArgs() []ast.Expr {
	if !p.canStartExecArg() {
		return nil
	}
	var args []ast.Expr
	for {
		astart := p.cur.Pos
		name := ""
		if p.cur.Kind == TOK_VARIABLE && p.peekIsPunct(1, "=") {
			name = p.cur.Lit
			p.advance()
			p.advance()
		}
		val := p.parseExpr()
		output := false
		if p.isKw("OUTPUT") || p.isKw("OUT") {
			p.advance()
			output = true
		}
		if name != "" || output {
			args = append(args, &ast.NamedArg{Name: name, Value: val, Output: output, P: astPos(astart)})
		} else {
			args = append(args, val)
		}
		if !p.isPunct(",") {
			return args
		}
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// THROW / RAISERROR
// ---------------------------------------------------------------------------

// parseThrow parses `THROW` (re-raise in CATCH) and `THROW n, msg, state`.
func (p *Parser) parseThrow() ast.PLStmt {
	pos := astPos(p.cur.Pos)
	p.advance() // THROW
	rs := &ast.RaiseStmt{Kind: "THROW", Level: "ERROR", P: pos}
	if !p.canStartExpr() {
		return rs
	}
	rs.ErrNumber = p.parseExpr()
	p.expectPunct(",")
	rs.Message = p.parseExpr()
	p.expectPunct(",")
	rs.State = p.parseExpr()
	return rs
}

// parseRaiserror parses `RAISERROR ( {msg | msg_id | @var}, severity,
// state [, arg…] ) [WITH LOG | NOWAIT | SETERROR [, …]]`. Level is derived
// from a literal severity (>= 11: ERROR, else NOTICE); a computed severity
// is treated as an error.
func (p *Parser) parseRaiserror() ast.PLStmt {
	pos := astPos(p.cur.Pos)
	p.expectKw("RAISERROR")
	rs := &ast.RaiseStmt{Kind: "RAISERROR", Level: "ERROR", P: pos}
	if !p.expectPunct("(") {
		return rs
	}
	first := p.parseExpr()
	if lit, ok := first.(*ast.Literal); ok && lit.Kind == "number" {
		rs.ErrNumber = first
	} else {
		rs.Message = first
	}
	p.expectPunct(",")
	rs.Severity = p.parseExpr()
	p.expectPunct(",")
	rs.State = p.parseExpr()
	for p.isPunct(",") {
		p.advance()
		rs.Args = append(rs.Args, p.parseExpr())
	}
	p.expectPunct(")")
	if lit, ok := rs.Severity.(*ast.Literal); ok && lit.Kind == "number" {
		if n, err := strconv.Atoi(lit.Text); err == nil && n <= 10 {
			rs.Level = "NOTICE"
		}
	}
	if p.isKw("WITH") {
		p.advance()
		for {
			opt, _ := p.parseIdentAny()
			rs.Options = append(rs.Options, strings.ToUpper(opt))
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	return rs
}

// ---------------------------------------------------------------------------
// Cursors
// ---------------------------------------------------------------------------

// parseCursorName parses `[GLOBAL] cursor_name | @cursor_variable`.
func (p *Parser) parseCursorName() string {
	if p.isKw("GLOBAL") && (p.peek(1).Kind == TOK_IDENT || p.peek(1).Kind == TOK_QUOTED_IDENT) {
		p.advance()
	}
	if p.cur.Kind == TOK_VARIABLE {
		v := p.cur.Lit
		p.advance()
		return v
	}
	n, _ := p.parseIdent()
	return n
}

// parseFetch parses `FETCH [[NEXT|PRIOR|FIRST|LAST|ABSOLUTE n|RELATIVE n]
// FROM] cursor [INTO @a, …]`.
func (p *Parser) parseFetch() ast.PLStmt {
	pos := astPos(p.cur.Pos)
	p.expectKw("FETCH")
	fs := &ast.FetchStmt{P: pos}
	switch {
	case p.isKw("NEXT") || p.isKw("PRIOR") || p.isKw("FIRST") || p.isKw("LAST"):
		fs.Direction = strings.ToUpper(p.cur.Lit)
		p.advance()
	case p.isKw("ABSOLUTE") || p.isKw("RELATIVE"):
		fs.Direction = strings.ToUpper(p.cur.Lit)
		p.advance()
		fs.Offset = p.parseExpr()
	}
	if p.isKw("FROM") {
		p.advance()
	}
	fs.Cursor = p.parseCursorName()
	if p.isKw("INTO") {
		p.advance()
		for {
			if p.cur.Kind != TOK_VARIABLE {
				p.errorHere("expected @variable", "@name")
				break
			}
			fs.Into = append(fs.Into, p.cur.Lit)
			p.advance()
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	return fs
}
