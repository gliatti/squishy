package mysql

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// MySQL/MariaDB DML parser — SELECT / INSERT / UPDATE / DELETE.
//
// Coverage targets the grammar rules in reference/MySqlParser.g4:
//   selectStatement, querySpecification, queryExpression, unionStatement,
//   tableSources, tableSource, tableSourceItem, joinPart, joinSpec,
//   selectElements, selectElement, fromClause, groupByClause, havingClause,
//   orderByClause, limitClause, withClause, commonTableExpressions,
//   lockClause; plus the full insertStatement, updateStatement (single +
//   multi), and deleteStatement (single + multi USING form).
//
// Productions intentionally NOT modeled at this stage — they are uncommon in
// migrated dumps and downgrade gracefully into a NoopStmt or RawSQL fallback:
//   selectIntoExpression (INTO DUMPFILE / INTO OUTFILE / INTO @vars within
//   the SELECT body), windowClause (named window definitions; OVER (...) on
//   aggregates is captured raw in WindowSpec.RawSpec), JSON_TABLE, LATERAL,
//   selectSpec hints other than ALL/DISTINCT (HIGH_PRIORITY,
//   STRAIGHT_JOIN, SQL_*_RESULT, …), indexHint.
// ---------------------------------------------------------------------------

// ParseSelect parses a stand-alone SELECT (with optional WITH preamble) and
// returns the resulting *ast.SelectStmt. The caller is responsible for
// consuming any trailing statement delimiter.
func ParseSelect(src string) (*ast.SelectStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseSelectStatement()
	return stmt, p.errs
}

// ParseInsert parses a stand-alone INSERT statement.
func ParseInsert(src string) (*ast.InsertStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseInsertStatement()
	return stmt, p.errs
}

// ParseUpdate parses a stand-alone UPDATE statement.
func ParseUpdate(src string) (*ast.UpdateStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseUpdateStatement()
	return stmt, p.errs
}

// ParseDelete parses a stand-alone DELETE statement.
func ParseDelete(src string) (*ast.DeleteStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseDeleteStatement()
	return stmt, p.errs
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

// parseSelectStatement parses a possibly-prefixed query expression:
//   [WITH [RECURSIVE] cte, …] querySpec (UNION [ALL|DISTINCT] querySpec)*
//   [ORDER BY …] [LIMIT …] [FOR UPDATE | LOCK IN SHARE MODE]
func (p *Parser) parseSelectStatement() *ast.SelectStmt {
	var with *ast.WithClause
	if p.isKw("WITH") {
		with = p.parseWithClause()
	}
	stmt := p.parseQuerySpecification()
	if stmt == nil {
		return nil
	}
	stmt.With = with

	for p.isKw("UNION") || p.isKw("INTERSECT") || p.isKw("EXCEPT") || p.isKw("MINUS") {
		op := p.cur.Lit
		p.advance()
		if p.isKw("ALL") {
			op += " ALL"
			p.advance()
		} else if p.isKw("DISTINCT") {
			p.advance()
		}
		next := p.parseQuerySpecification()
		if next == nil {
			break
		}
		stmt.SetOps = append(stmt.SetOps, ast.SetOp{Op: op, Stmt: next})
	}

	if p.isKw("ORDER") {
		stmt.OrderBy = p.parseOrderByClause()
	}
	if p.isKw("LIMIT") {
		stmt.Limit, stmt.Offset = p.parseLimitClause()
	}
	if lock := p.parseLockClause(); lock != "" {
		stmt.ForUpdate = lock
	}
	return stmt
}

// parseQuerySpecification handles a single query body, including the
// parenthesised form `(SELECT ...)` (queryExpression).
func (p *Parser) parseQuerySpecification() *ast.SelectStmt {
	if p.isPunct("(") {
		// queryExpression — parens around an inner SELECT. Allow nested
		// parens; trim them all.
		p.advance()
		inner := p.parseSelectStatement()
		p.expectPunct(")")
		return inner
	}
	if !p.isKw("SELECT") {
		p.errorHere("expected SELECT", "SELECT")
		return nil
	}
	start := p.cur.Pos
	p.expectKw("SELECT")
	stmt := &ast.SelectStmt{P: astPos(start)}
	p.consumeSelectSpec(stmt)
	stmt.Cols = p.parseSelectElements()
	if p.isKw("FROM") {
		p.advance()
		stmt.From = p.parseTableSources()
	}
	if p.isKw("WHERE") {
		p.advance()
		stmt.Where = p.parseExpr()
	}
	if p.isKw("GROUP") {
		p.advance()
		p.expectKw("BY")
		stmt.GroupBy = p.parseGroupByItems()
		if p.isKw("WITH") {
			// `WITH ROLLUP` — informational; the AST does not model it yet.
			p.advance()
			if p.isKw("ROLLUP") {
				p.advance()
			}
		}
	}
	if p.isKw("HAVING") {
		p.advance()
		stmt.Having = p.parseExpr()
	}
	if p.isKw("WINDOW") {
		// Skip the named-window definitions — the AST does not yet carry a
		// per-query window registry. Walk to the next clause boundary.
		p.skipWindowClause()
	}
	if p.isKw("ORDER") {
		stmt.OrderBy = p.parseOrderByClause()
	}
	if p.isKw("LIMIT") {
		stmt.Limit, stmt.Offset = p.parseLimitClause()
	}
	stmt.ForUpdate = p.parseLockClause()
	return stmt
}

// consumeSelectSpec absorbs the optional selectSpec* prefix that may appear
// between `SELECT` and the projection list, recording ALL/DISTINCT on the
// AST and dropping the rest (HIGH_PRIORITY, STRAIGHT_JOIN, SQL_* hints).
func (p *Parser) consumeSelectSpec(stmt *ast.SelectStmt) {
	for {
		switch {
		case p.isKw("DISTINCT"), p.isKw("DISTINCTROW"):
			stmt.Distinct = true
			p.advance()
		case p.isKw("ALL"):
			stmt.AllOnly = true
			p.advance()
		case p.isKw("HIGH_PRIORITY"), p.isKw("STRAIGHT_JOIN"):
			p.advance()
		case p.cur.Kind == TOK_IDENT && isSelectSpecHint(p.cur.Lit):
			p.advance()
		default:
			return
		}
	}
}

// isSelectSpecHint covers the SQL_*_RESULT family and SQL_BUFFER_RESULT /
// SQL_CACHE / SQL_NO_CACHE / SQL_CALC_FOUND_ROWS, which the lexer emits as
// IDENT.
func isSelectSpecHint(lit string) bool {
	switch strings.ToUpper(lit) {
	case "SQL_SMALL_RESULT", "SQL_BIG_RESULT", "SQL_BUFFER_RESULT",
		"SQL_CACHE", "SQL_NO_CACHE", "SQL_CALC_FOUND_ROWS":
		return true
	}
	return false
}

// parseSelectElements parses the projection list:
//   '*' | selectElement (',' selectElement)*
func (p *Parser) parseSelectElements() []ast.SelectItem {
	var out []ast.SelectItem
	out = append(out, p.parseSelectElement())
	for p.isPunct(",") {
		p.advance()
		out = append(out, p.parseSelectElement())
	}
	return out
}

func (p *Parser) parseSelectElement() ast.SelectItem {
	// Pure '*' projection.
	if p.isPunct("*") {
		p.advance()
		return ast.SelectItem{Star: true}
	}
	// `<ident> . *` qualified star — peek two tokens.
	if (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD) && p.peekIsPunct(".") {
		// Save current token; if the dot is followed by '*', it's a qualified star.
		saved := p.cur
		p.advance()  // consume ident
		p.advance()  // consume '.'
		if p.isPunct("*") {
			p.advance()
			return ast.SelectItem{Star: true, Qualifier: saved.Lit}
		}
		// Otherwise: a regular ident-path expression starting with `<ident> .`.
		// Reconstruct the path manually since we already consumed the dot.
		parts := []string{saved.Lit}
		// The current token is the second component of the path.
		if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
			parts = append(parts, p.cur.Lit)
			p.advance()
		}
		var head ast.Expr = &ast.Ident{Parts: parts, P: astPos(saved.Pos)}
		// Allow the rest of the path (additional `.id` components).
		for p.isPunct(".") {
			p.advance()
			if n, _ := p.parseIdent(); n != "" {
				id := head.(*ast.Ident)
				id.Parts = append(id.Parts, n)
			}
		}
		// Continue parsing the expression starting with this prefix — but the
		// remaining infix/postfix operators may apply. The simplest correct
		// thing is to treat this as a complete expression head and then look
		// for operators (binary ops) by calling a continuation parser. To
		// keep the parser simple we accept it as-is and look for an alias.
		return p.finishSelectElementWithExpr(head)
	}
	expr := p.parseExpr()
	return p.finishSelectElementWithExpr(expr)
}

// finishSelectElementWithExpr applies the optional `[AS] alias` tail to a
// fully parsed projection expression.
func (p *Parser) finishSelectElementWithExpr(expr ast.Expr) ast.SelectItem {
	alias := ""
	if p.isKw("AS") {
		p.advance()
		alias, _ = p.parseIdent()
	} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		// Bare alias — the next non-keyword identifier becomes the alias only
		// when it is followed by `,` or end-of-projection (FROM/INTO/UNION/...).
		// Peek ahead by saving the position; since the lexer is forward-only
		// we accept the heuristic that an IDENT at this position is an alias.
		alias = p.cur.Lit
		p.advance()
	}
	return ast.SelectItem{Expr: expr, Alias: alias}
}

// parseGroupByItems collects the expression list of a GROUP BY clause.
// MySQL allows trailing ASC/DESC per item; we drop the order qualifier
// because the AST doesn't model it (PG ignores ASC/DESC in GROUP BY).
func (p *Parser) parseGroupByItems() []ast.Expr {
	var out []ast.Expr
	for {
		out = append(out, p.parseExpr())
		if p.isKw("ASC") || p.isKw("DESC") {
			p.advance()
		}
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseOrderByClause consumes a `ORDER BY` clause and returns the items.
func (p *Parser) parseOrderByClause() []ast.OrderItem {
	p.expectKw("ORDER")
	p.expectKw("BY")
	var out []ast.OrderItem
	for {
		item := ast.OrderItem{Expr: p.parseExpr()}
		if p.isKw("ASC") {
			p.advance()
		} else if p.isKw("DESC") {
			p.advance()
			item.Desc = true
		}
		// MySQL has no `NULLS FIRST/LAST` in ORDER BY (added in MariaDB only)
		// — we accept it defensively for round-trip with mariadb extensions.
		if p.cur.Kind == TOK_IDENT && strings.EqualFold(p.cur.Lit, "NULLS") {
			p.advance()
			b := false
			if p.cur.Kind == TOK_IDENT && strings.EqualFold(p.cur.Lit, "FIRST") {
				p.advance()
			} else if p.cur.Kind == TOK_IDENT && strings.EqualFold(p.cur.Lit, "LAST") {
				p.advance()
				b = true
			}
			item.NullsLast = &b
		}
		out = append(out, item)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseLimitClause supports both MySQL forms:
//   LIMIT count
//   LIMIT offset, count
//   LIMIT count OFFSET offset
func (p *Parser) parseLimitClause() (limit, offset ast.Expr) {
	p.expectKw("LIMIT")
	first := p.parseLimitAtom()
	if p.isPunct(",") {
		p.advance()
		second := p.parseLimitAtom()
		// `LIMIT offset, count` — first is offset, second is limit.
		return second, first
	}
	if p.isKw("OFFSET") {
		p.advance()
		offset = p.parseLimitAtom()
	}
	return first, offset
}

// parseLimitAtom is a numeric literal, a session variable, or an unqualified
// identifier (binding through plpgsql parameters).
func (p *Parser) parseLimitAtom() ast.Expr {
	switch {
	case p.cur.Kind == TOK_NUMBER:
		v := p.cur.Lit
		pos := p.cur.Pos
		p.advance()
		return &ast.Literal{Kind: "number", Text: v, P: astPos(pos)}
	case p.isPunct("@"):
		start := p.cur.Pos
		p.advance()
		// `@var` or `@@session.var`.
		var b strings.Builder
		b.WriteByte('@')
		if p.isPunct("@") {
			b.WriteByte('@')
			p.advance()
		}
		if n, _ := p.parseIdent(); n != "" {
			b.WriteString(n)
		}
		for p.isPunct(".") {
			b.WriteByte('.')
			p.advance()
			if n, _ := p.parseIdent(); n != "" {
				b.WriteString(n)
			}
		}
		return &ast.Ident{Parts: []string{b.String()}, P: astPos(start)}
	default:
		// fallback to a plain expression — covers `LIMIT v` for parameterised
		// queries which MariaDB and PG both accept.
		return p.parseExpr()
	}
}

// parseLockClause returns the trailing FOR UPDATE / LOCK IN SHARE MODE
// clause as raw text, or "" when absent. Anything more exotic (FOR SHARE
// OF, NOWAIT, SKIP LOCKED) is captured verbatim by walking forward to the
// statement terminator boundary.
func (p *Parser) parseLockClause() string {
	if p.isKw("FOR") {
		var b strings.Builder
		b.WriteString("FOR")
		p.advance()
		// FOR UPDATE / FOR SHARE [OF tableName] [NOWAIT|SKIP LOCKED]
		if p.isKw("UPDATE") || p.isKw("SHARE") {
			b.WriteByte(' ')
			b.WriteString(p.cur.Lit)
			p.advance()
		}
		for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
			// stop at the start of the next clause (UNION, etc.) or a closing
			// paren of an enclosing subquery.
			if p.isKw("UNION") || p.isKw("INTERSECT") || p.isKw("EXCEPT") || p.isKw("MINUS") || p.isPunct(")") {
				break
			}
			b.WriteByte(' ')
			b.WriteString(p.cur.Lit)
			p.advance()
		}
		return strings.TrimSpace(b.String())
	}
	if p.isKw("LOCK") {
		var b strings.Builder
		b.WriteString("LOCK")
		p.advance()
		for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
			if p.isKw("UNION") || p.isKw("INTERSECT") || p.isKw("EXCEPT") || p.isKw("MINUS") || p.isPunct(")") {
				break
			}
			b.WriteByte(' ')
			b.WriteString(p.cur.Lit)
			p.advance()
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// skipWindowClause consumes a `WINDOW name AS (spec) [, name AS (spec)]*`
// definition list. The body of each spec is consumed as a balanced
// parenthesis group.
func (p *Parser) skipWindowClause() {
	p.expectKw("WINDOW")
	for {
		if _, _ = p.parseIdent(); false {
		}
		// `AS (`
		if p.isKw("AS") {
			p.advance()
		}
		if p.isPunct("(") {
			depth := 1
			p.advance()
			for depth > 0 && p.cur.Kind != TOK_EOF {
				if p.isPunct("(") {
					depth++
				} else if p.isPunct(")") {
					depth--
				}
				p.advance()
			}
		}
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// FROM / JOIN
// ---------------------------------------------------------------------------

// parseTableSources parses the comma-separated tableSource list of a FROM
// clause.
func (p *Parser) parseTableSources() []ast.FromItem {
	var out []ast.FromItem
	out = append(out, p.parseTableSource())
	for p.isPunct(",") {
		p.advance()
		out = append(out, p.parseTableSource())
	}
	return out
}

// parseTableSource parses one tableSource (a tableSourceItem followed by a
// possibly-empty chain of joinPart entries). The result is a left-leaning
// FromJoin tree when joins are present, or a bare FromItem otherwise. The
// parenthesised forms `(tableSourceItem joinPart*)` and `(tableSources)`
// are both handled inside parseTableSourceItem.
func (p *Parser) parseTableSource() ast.FromItem {
	first := p.parseTableSourceItem()
	for {
		jp, ok := p.tryParseJoinPart(first)
		if !ok {
			return first
		}
		first = jp
	}
}

// parseTableSourceItem parses one of:
//   tableName [PARTITION (uidList)] [AS? alias] [indexHint…]
//   (selectStatement) [AS? alias]
//   (tableSources)
func (p *Parser) parseTableSourceItem() ast.FromItem {
	start := p.cur.Pos
	if p.isPunct("(") {
		// Distinguish `(SELECT ...)` from `(tableSources)`.
		// We commit by advancing and inspecting the next token.
		p.advance()
		if p.isKw("SELECT") || p.isKw("WITH") || p.isPunct("(") {
			inner := p.parseSelectStatement()
			p.expectPunct(")")
			alias := ""
			if p.isKw("AS") {
				p.advance()
				alias, _ = p.parseIdent()
			} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
				alias = p.cur.Lit
				p.advance()
			}
			return &ast.FromSubquery{Stmt: inner, Alias: alias, P: astPos(start)}
		}
		// `(tableSources)` — handled by recursion.
		ts := p.parseTableSource()
		for p.isPunct(",") {
			p.advance()
			next := p.parseTableSource()
			ts = &ast.FromJoin{Kind: ast.CrossJoin, Left: ts, Right: next, P: ts.Pos()}
		}
		p.expectPunct(")")
		return ts
	}
	// tableName form.
	ref := p.parseTableRef()
	t := &ast.FromTable{
		Schema: ref.Schema, Name: ref.Name, NameBacktick: ref.NameBacktick,
		P: astPos(start),
	}
	// Optional PARTITION (uidList) — drop the names; the AST does not yet
	// carry per-source partition pinning.
	if p.isKw("PARTITION") {
		p.advance()
		p.skipBalancedParens()
	}
	// [AS] alias
	if p.isKw("AS") {
		p.advance()
		t.Alias, _ = p.parseIdent()
	} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		// Heuristic: an identifier here is a bare alias unless it is a join
		// keyword (handled elsewhere). The lexer maps reserved keywords to
		// TOK_KEYWORD, so TOK_IDENT here is safe.
		t.Alias = p.cur.Lit
		p.advance()
	}
	// Drop indexHint clauses (USE/IGNORE/FORCE INDEX … (cols)).
	for p.isKw("USE") || p.isKw("IGNORE") || p.isKw("FORCE") {
		p.advance() // USE/IGNORE/FORCE
		if p.isKw("INDEX") || p.isKw("KEY") {
			p.advance()
		}
		if p.isKw("FOR") {
			p.advance()
			if p.isKw("JOIN") || p.isKw("ORDER") || p.isKw("GROUP") {
				p.advance()
				if p.isKw("BY") {
					p.advance()
				}
			}
		}
		p.skipBalancedParens()
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return t
}

// tryParseJoinPart consumes a joinPart if the current token starts one and
// returns the resulting FromJoin (with `left` as its left operand). Returns
// (left, false) when the current token is not a join opener.
func (p *Parser) tryParseJoinPart(left ast.FromItem) (ast.FromItem, bool) {
	start := p.cur.Pos
	switch {
	case p.isKw("INNER") || p.isKw("CROSS"):
		kind := ast.InnerJoin
		if p.isKw("CROSS") {
			kind = ast.CrossJoin
		}
		p.advance()
		p.expectKw("JOIN")
		p.optionalLateral()
		right := p.parseTableSourceItem()
		j := &ast.FromJoin{Kind: kind, Left: left, Right: right, P: astPos(start)}
		p.parseJoinSpec(j)
		return j, true
	case p.isKw("STRAIGHT_JOIN"):
		p.advance()
		right := p.parseTableSourceItem()
		j := &ast.FromJoin{Kind: ast.InnerJoin, Left: left, Right: right, P: astPos(start)}
		// STRAIGHT_JOIN takes an optional ON clause.
		if p.isKw("ON") {
			p.advance()
			j.On = p.parseExpr()
		}
		return j, true
	case p.isKw("LEFT") || p.isKw("RIGHT") || p.isKw("FULL"):
		kind := ast.LeftJoin
		switch p.cur.Lit {
		case "RIGHT":
			kind = ast.RightJoin
		case "FULL":
			kind = ast.FullJoin
		}
		p.advance()
		if p.isKw("OUTER") {
			p.advance()
		}
		p.expectKw("JOIN")
		p.optionalLateral()
		right := p.parseTableSourceItem()
		j := &ast.FromJoin{Kind: kind, Left: left, Right: right, P: astPos(start)}
		p.parseJoinSpec(j)
		return j, true
	case p.isKw("NATURAL"):
		p.advance()
		kind := ast.InnerJoin
		switch {
		case p.isKw("LEFT"):
			p.advance()
			if p.isKw("OUTER") {
				p.advance()
			}
			kind = ast.LeftJoin
		case p.isKw("RIGHT"):
			p.advance()
			if p.isKw("OUTER") {
				p.advance()
			}
			kind = ast.RightJoin
		}
		p.expectKw("JOIN")
		right := p.parseTableSourceItem()
		return &ast.FromJoin{
			Kind: kind, Left: left, Right: right, Natural: true, P: astPos(start),
		}, true
	case p.isKw("JOIN"):
		// Bare JOIN — equivalent to INNER JOIN.
		p.advance()
		p.optionalLateral()
		right := p.parseTableSourceItem()
		j := &ast.FromJoin{Kind: ast.InnerJoin, Left: left, Right: right, P: astPos(start)}
		p.parseJoinSpec(j)
		return j, true
	}
	return left, false
}

func (p *Parser) optionalLateral() {
	if p.isKw("LATERAL") {
		p.advance()
	}
}

// parseJoinSpec parses the ON / USING tail of a join. Multiple `ON`
// expressions are AND-folded onto j.On to keep the AST shape uniform.
func (p *Parser) parseJoinSpec(j *ast.FromJoin) {
	for {
		switch {
		case p.isKw("ON"):
			p.advance()
			cond := p.parseExpr()
			if j.On == nil {
				j.On = cond
			} else {
				j.On = &ast.BinaryExpr{Op: "AND", Lhs: j.On, Rhs: cond, P: j.On.Pos()}
			}
		case p.isKw("USING"):
			p.advance()
			p.expectPunct("(")
			for {
				n, _ := p.parseIdent()
				if n != "" {
					j.Using = append(j.Using, n)
				}
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
			p.expectPunct(")")
		default:
			return
		}
	}
}

// skipBalancedParens consumes an entire `(…)` group at depth 1, advancing
// past the matching ')'. No-op when the cursor is not on '('.
func (p *Parser) skipBalancedParens() {
	if !p.isPunct("(") {
		return
	}
	depth := 1
	p.advance()
	for depth > 0 && p.cur.Kind != TOK_EOF {
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
		}
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// WITH (CTE)
// ---------------------------------------------------------------------------

// parseWithClause parses `WITH [RECURSIVE] cte (',' cte)*`.
func (p *Parser) parseWithClause() *ast.WithClause {
	p.expectKw("WITH")
	w := &ast.WithClause{}
	if p.isKw("RECURSIVE") {
		w.Recursive = true
		p.advance()
	}
	for {
		c := ast.CTE{}
		c.Name, _ = p.parseIdent()
		if p.isPunct("(") {
			p.advance()
			for {
				n, _ := p.parseIdent()
				if n != "" {
					c.Columns = append(c.Columns, n)
				}
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
			p.expectPunct(")")
		}
		p.expectKw("AS")
		p.expectPunct("(")
		c.Body = p.parseSelectStatement()
		p.expectPunct(")")
		w.CTEs = append(w.CTEs, c)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return w
}

// ---------------------------------------------------------------------------
// INSERT
// ---------------------------------------------------------------------------

// parseInsertStatement parses an `INSERT [pri] [IGNORE] [INTO] tbl [(cols)]
// {VALUES … | SELECT …} [ON DUPLICATE KEY UPDATE …]` statement, plus the
// `INSERT … SET col=expr` MariaDB shorthand.
func (p *Parser) parseInsertStatement() *ast.InsertStmt {
	start := p.cur.Pos
	p.expectKw("INSERT")
	consumeInsertModifiers(p)
	if p.isKw("INTO") {
		p.advance()
	}
	ref := p.parseTableRef()
	stmt := &ast.InsertStmt{Table: ref, P: astPos(start)}
	if p.isKw("PARTITION") {
		p.advance()
		p.skipBalancedParens()
	}
	switch {
	case p.isPunct("("):
		p.advance()
		for {
			n, _ := p.parseIdent()
			if n != "" {
				stmt.Cols = append(stmt.Cols, n)
			}
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
		// After the column list, the source can be VALUES … or a SELECT.
		switch {
		case p.isKw("VALUES") || p.isKw("VALUE"):
			p.parseInsertValues(stmt)
		case p.isKw("SELECT") || p.isKw("WITH"):
			stmt.Select = p.parseSelectStatement()
		}
	case p.isKw("VALUES") || p.isKw("VALUE"):
		p.parseInsertValues(stmt)
	case p.isKw("SELECT") || p.isKw("WITH"):
		stmt.Select = p.parseSelectStatement()
	case p.isKw("SET"):
		p.advance()
		stmt.Cols, stmt.Values = parseInsertSetForm(p)
	}
	// optional `AS uid` row alias (MySQL 8.0.19+ INSERT … VALUES … AS new ...).
	if p.isKw("AS") {
		p.advance()
		_, _ = p.parseIdent()
		// optional column-alias list
		if p.isPunct("(") {
			p.skipBalancedParens()
		}
	}
	if p.isKw("ON") {
		p.advance()
		p.expectKw("DUPLICATE")
		p.expectKw("KEY")
		p.expectKw("UPDATE")
		oc := &ast.OnConflict{}
		oc.Sets = parseUpdatedElements(p)
		stmt.OnConflict = oc
	}
	if p.isKw("RETURNING") {
		p.advance()
		stmt.Returning = p.parseSelectElements()
	}
	return stmt
}

// consumeInsertModifiers absorbs LOW_PRIORITY / DELAYED / HIGH_PRIORITY and
// IGNORE prefixes on INSERT/REPLACE statements.
func consumeInsertModifiers(p *Parser) {
	for {
		switch {
		case p.isKw("LOW_PRIORITY"), p.isKw("DELAYED"), p.isKw("HIGH_PRIORITY"),
			p.isKw("IGNORE"), p.isKw("QUICK"):
			p.advance()
		default:
			return
		}
	}
}

// parseInsertValues parses one or more `VALUES (…)` row tuples.
func (p *Parser) parseInsertValues(stmt *ast.InsertStmt) {
	if p.isKw("VALUES") || p.isKw("VALUE") {
		p.advance()
	}
	for {
		p.expectPunct("(")
		var row []ast.Expr
		if !p.isPunct(")") {
			row = append(row, p.parseValueExpr())
			for p.isPunct(",") {
				p.advance()
				row = append(row, p.parseValueExpr())
			}
		}
		p.expectPunct(")")
		stmt.Values = append(stmt.Values, row)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
}

// parseValueExpr accepts the special token DEFAULT in addition to a normal
// expression — `INSERT … VALUES (1, DEFAULT, 'x')` is a frequent shape.
func (p *Parser) parseValueExpr() ast.Expr {
	if p.isKw("DEFAULT") {
		start := p.cur.Pos
		p.advance()
		return &ast.Ident{Parts: []string{"DEFAULT"}, P: astPos(start)}
	}
	return p.parseExpr()
}

// parseInsertSetForm consumes the `INSERT … SET col = expr (, col = expr)*`
// shorthand. We surface it as a single one-row VALUES tuple paired with the
// column list so downstream code can treat both forms uniformly.
func parseInsertSetForm(p *Parser) ([]string, [][]ast.Expr) {
	cols := []string{}
	row := []ast.Expr{}
	for {
		col, _ := p.parseIdent()
		// optional `tbl.col` qualified form
		if p.isPunct(".") {
			p.advance()
			c2, _ := p.parseIdent()
			col = col + "." + c2
		}
		// `=` or `:=`
		if p.isPunct(":=") {
			p.advance()
		} else {
			p.expectPunct("=")
		}
		row = append(row, p.parseValueExpr())
		cols = append(cols, col)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return cols, [][]ast.Expr{row}
}

// parseUpdatedElements parses `col = expr (, col = expr)*` — the common
// payload of ON DUPLICATE KEY UPDATE and UPDATE SET.
func parseUpdatedElements(p *Parser) []ast.Assign {
	var out []ast.Assign
	for {
		// fullColumnName: [schema.][table.]col, all lowercase or backticked.
		col, _ := p.parseIdent()
		for p.isPunct(".") {
			p.advance()
			c2, _ := p.parseIdent()
			col = col + "." + c2
		}
		if p.isPunct(":=") {
			p.advance()
		} else {
			p.expectPunct("=")
		}
		val := p.parseValueExpr()
		out = append(out, ast.Assign{Col: col, Expr: val})
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// ---------------------------------------------------------------------------
// UPDATE
// ---------------------------------------------------------------------------

// parseUpdateStatement parses both the single- and multi-table forms of
// MySQL's UPDATE statement. The grammar makes them distinct rules; the
// difference is purely whether `tableSources` resolves to one or many
// items (and whether a single-target alias / WHERE-only / ORDER+LIMIT
// trailer applies). We collapse them into the same AST shape.
func (p *Parser) parseUpdateStatement() *ast.UpdateStmt {
	start := p.cur.Pos
	p.expectKw("UPDATE")
	consumeInsertModifiers(p)
	sources := p.parseTableSources()
	stmt := &ast.UpdateStmt{P: astPos(start)}
	// In the single-table case, hoist the bare table reference + alias out
	// of the FROM list so the AST matches the conventional shape; in the
	// multi-table case, all sources go into From and Table is left zeroed.
	if len(sources) == 1 {
		if t, ok := sources[0].(*ast.FromTable); ok {
			stmt.Table = ast.TableRef{Schema: t.Schema, Name: t.Name, NameBacktick: t.NameBacktick}
			stmt.Alias = t.Alias
		} else {
			stmt.From = sources
		}
	} else {
		stmt.From = sources
	}
	// Optional `[AS] alias` after the single-target form. The grammar puts
	// this on `singleUpdateStatement` only, so guard on the table being set.
	if stmt.Table.Name != "" && stmt.Alias == "" {
		if p.isKw("AS") {
			p.advance()
			stmt.Alias, _ = p.parseIdent()
		} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
			stmt.Alias = p.cur.Lit
			p.advance()
		}
	}
	p.expectKw("SET")
	stmt.Sets = parseUpdatedElements(p)
	if p.isKw("WHERE") {
		p.advance()
		stmt.Where = p.parseExpr()
	}
	if p.isKw("ORDER") {
		// `ORDER BY` on UPDATE applies to the single-target form only and
		// disambiguates which rows are touched first when LIMIT N is given.
		// The AST does not yet carry it; consume to advance the cursor.
		_ = p.parseOrderByClause()
	}
	if p.isKw("LIMIT") {
		_, _ = p.parseLimitClause()
	}
	if p.isKw("RETURNING") {
		p.advance()
		stmt.Returning = p.parseSelectElements()
	}
	return stmt
}

// ---------------------------------------------------------------------------
// DELETE
// ---------------------------------------------------------------------------

// parseDeleteStatement parses both single- and multi-table DELETE forms,
// including the `DELETE FROM tbl USING tableSources` PG-style multi-target.
func (p *Parser) parseDeleteStatement() *ast.DeleteStmt {
	start := p.cur.Pos
	p.expectKw("DELETE")
	consumeInsertModifiers(p)
	stmt := &ast.DeleteStmt{P: astPos(start)}
	if p.isKw("FROM") {
		p.advance()
		// Could be:
		//   FROM tbl [.*[, tbl[.*]]…] USING tableSources [WHERE]
		//   FROM tbl [WHERE] [ORDER BY] [LIMIT]
		// We parse the first reference and look ahead for `,` or USING.
		first := p.parseTableRef()
		consumeStarSuffix(p)
		if p.isPunct(",") || p.isKw("USING") {
			// multi-target shape — stash the first table refs in Using as
			// FromTable items; Table stays zeroed.
			extras := []ast.FromItem{&ast.FromTable{
				Schema: first.Schema, Name: first.Name, NameBacktick: first.NameBacktick,
			}}
			for p.isPunct(",") {
				p.advance()
				ref := p.parseTableRef()
				consumeStarSuffix(p)
				extras = append(extras, &ast.FromTable{
					Schema: ref.Schema, Name: ref.Name, NameBacktick: ref.NameBacktick,
				})
			}
			p.expectKw("USING")
			stmt.Using = p.parseTableSources()
			// Targets ahead of USING are joined in Using too — we keep them
			// because PG's DELETE USING accepts the same list.
			stmt.Using = append(extras, stmt.Using...)
		} else {
			stmt.Table = first
			// optional `[AS] alias` for the single-target form
			if p.isKw("AS") {
				p.advance()
				stmt.Alias, _ = p.parseIdent()
			} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
				stmt.Alias = p.cur.Lit
				p.advance()
			}
			if p.isKw("PARTITION") {
				p.advance()
				p.skipBalancedParens()
			}
		}
	} else {
		// `DELETE tbl[.*][, …] FROM tableSources` — multi-target form.
		first := p.parseTableRef()
		consumeStarSuffix(p)
		extras := []ast.FromItem{&ast.FromTable{
			Schema: first.Schema, Name: first.Name, NameBacktick: first.NameBacktick,
		}}
		for p.isPunct(",") {
			p.advance()
			ref := p.parseTableRef()
			consumeStarSuffix(p)
			extras = append(extras, &ast.FromTable{
				Schema: ref.Schema, Name: ref.Name, NameBacktick: ref.NameBacktick,
			})
		}
		p.expectKw("FROM")
		stmt.Using = append(extras, p.parseTableSources()...)
	}
	if p.isKw("WHERE") {
		p.advance()
		stmt.Where = p.parseExpr()
	}
	if p.isKw("ORDER") {
		_ = p.parseOrderByClause()
	}
	if p.isKw("LIMIT") {
		_, _ = p.parseLimitClause()
	}
	if p.isKw("RETURNING") {
		p.advance()
		stmt.Returning = p.parseSelectElements()
	}
	return stmt
}

// consumeStarSuffix swallows the optional `.*` qualifier appended to a
// table reference in MySQL's multi-target DELETE.
func consumeStarSuffix(p *Parser) {
	if p.isPunct(".") {
		p.advance()
		if p.isPunct("*") {
			p.advance()
		}
	}
}
