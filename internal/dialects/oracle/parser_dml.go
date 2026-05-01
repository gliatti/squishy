package oracle

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// Oracle DML parser — SELECT / INSERT / UPDATE / DELETE / MERGE.
//
// Coverage targets PlSqlParser.g4 productions:
//   select_statement, subquery, query_block, group_by_clause,
//   order_by_clause, fetch_clause, offset_clause, for_update_clause,
//   set_clause, table_ref, table_ref_aux, join_clause,
//   subquery_factoring_clause (WITH … AS), single_table_insert,
//   update_statement, delete_statement, merge_statement.
//
// Constructs intentionally not modeled here (fall back to the legacy
// RawSQL path via parseSelectOrRawDML when encountered):
//   - CONNECT BY / START WITH (hierarchical queries)
//   - MODEL clause
//   - PIVOT / UNPIVOT
//   - multi-table INSERT (INSERT ALL / INSERT FIRST WHEN …)
//   - flashback (`AS OF SCN/TIMESTAMP`) qualifiers
//
// Adding any of these requires extending the AST first; see the plan in
// ~/.claude/plans/dans-le-code-une-luminous-cat.md.
// ---------------------------------------------------------------------------

// ParseSelect parses a stand-alone Oracle SELECT statement (with optional
// WITH preamble and UNION/INTERSECT/MINUS chain) and returns the resulting
// *ast.SelectStmt. Mirrors the MySQL ParseSelect entrypoint.
func ParseSelect(src string) (*ast.SelectStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseSelectStatement()
	return stmt, p.errs
}

// ParseInsert parses a stand-alone Oracle INSERT statement.
func ParseInsert(src string) (*ast.InsertStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseInsertStatement()
	return stmt, p.errs
}

// ParseUpdate parses a stand-alone Oracle UPDATE statement.
func ParseUpdate(src string) (*ast.UpdateStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseUpdateStatement()
	return stmt, p.errs
}

// ParseDelete parses a stand-alone Oracle DELETE statement.
func ParseDelete(src string) (*ast.DeleteStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseDeleteStatement()
	return stmt, p.errs
}

// ParseMerge parses a stand-alone Oracle MERGE statement.
func ParseMerge(src string) (*ast.MergeStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	stmt := p.parseMergeStatement()
	return stmt, p.errs
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

func (p *Parser) parseSelectStatement() *ast.SelectStmt {
	var with *ast.WithClause
	if p.isKw("WITH") {
		with = p.parseWithClause()
	}
	stmt := p.parseQueryBlock()
	if stmt == nil {
		return nil
	}
	stmt.With = with

	for p.isKw("UNION") || p.isKw("INTERSECT") || p.isKw("MINUS") || p.isKw("EXCEPT") {
		op := p.cur.Lit
		p.advance()
		if p.isKw("ALL") {
			op += " ALL"
			p.advance()
		} else if p.isKw("DISTINCT") {
			p.advance()
		}
		next := p.parseQueryBlock()
		if next == nil {
			break
		}
		stmt.SetOps = append(stmt.SetOps, ast.SetOp{Op: op, Stmt: next})
	}

	if p.isKw("ORDER") {
		stmt.OrderBy = p.parseOrderByClause()
	}
	// Oracle pagination — `OFFSET n {ROW|ROWS} FETCH {FIRST|NEXT} m {ROW|ROWS}
	// [WITH TIES] ONLY`. We capture OFFSET/FETCH as Limit/Offset expressions.
	if p.isKw("OFFSET") {
		stmt.Offset = p.parseFetchOffsetAtom()
		// optional ROW / ROWS
		if p.isKw("ROW") || p.isKw("ROWS") {
			p.advance()
		}
	}
	if p.isKw("FETCH") {
		p.advance()
		// FIRST | NEXT
		if p.isKw("FIRST") || p.isKw("NEXT") {
			p.advance()
		}
		stmt.Limit = p.parseFetchOffsetAtom()
		if p.isKw("PERCENT") {
			p.advance()
		}
		if p.isKw("ROW") || p.isKw("ROWS") {
			p.advance()
		}
		// WITH TIES | ONLY
		if p.isKw("WITH") {
			p.advance()
			if p.isKw("TIES") {
				p.advance()
			}
		} else if p.isKw("ONLY") {
			p.advance()
		}
	}
	if lock := p.parseForUpdateClause(); lock != "" {
		stmt.ForUpdate = lock
	}
	return stmt
}

// parseQueryBlock handles a single SELECT body, including the parenthesised
// form `(SELECT ...)` (which the grammar allows wherever a subquery is
// permitted).
func (p *Parser) parseQueryBlock() *ast.SelectStmt {
	if p.isPunct("(") {
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
	p.consumeSelectModifiers(stmt)
	stmt.Cols = p.parseSelectList()
	// Oracle SELECT ... INTO (PL/SQL) is handled by parseSelectOrRawDML, not
	// here. The caller routes to the appropriate path.
	if p.isKw("FROM") {
		p.advance()
		stmt.From = p.parseFromList()
	}
	if p.isKw("WHERE") {
		p.advance()
		stmt.Where = p.parseExpr()
	}
	// CONNECT BY / START WITH — opaque tail; capture verbatim into ForUpdate
	// (lazy reuse of a string field). Translator surfaces it as a manual
	// remediation prerequisite.
	if p.isKw("CONNECT") || p.isKw("START") {
		stmt.ForUpdate = p.captureHierarchicalTail()
	}
	if p.isKw("GROUP") {
		p.advance()
		p.expectKw("BY")
		stmt.GroupBy = p.parseGroupByList()
	}
	if p.isKw("HAVING") {
		p.advance()
		stmt.Having = p.parseExpr()
	}
	if p.isKw("ORDER") {
		stmt.OrderBy = p.parseOrderByClause()
	}
	if p.isKw("OFFSET") {
		p.advance()
		stmt.Offset = p.parseFetchOffsetAtom()
		if p.isKw("ROW") || p.isKw("ROWS") {
			p.advance()
		}
	}
	if p.isKw("FETCH") {
		p.advance()
		if p.isKw("FIRST") || p.isKw("NEXT") {
			p.advance()
		}
		stmt.Limit = p.parseFetchOffsetAtom()
		if p.isKw("PERCENT") {
			p.advance()
		}
		if p.isKw("ROW") || p.isKw("ROWS") {
			p.advance()
		}
		if p.isKw("WITH") {
			p.advance()
			if p.isKw("TIES") {
				p.advance()
			}
		} else if p.isKw("ONLY") {
			p.advance()
		}
	}
	if lock := p.parseForUpdateClause(); lock != "" {
		stmt.ForUpdate = lock
	}
	return stmt
}

func (p *Parser) consumeSelectModifiers(stmt *ast.SelectStmt) {
	for {
		switch {
		case p.isKw("DISTINCT"), p.isAnyKw("UNIQUE"):
			stmt.Distinct = true
			p.advance()
		case p.isKw("ALL"):
			stmt.AllOnly = true
			p.advance()
		default:
			return
		}
	}
}

// parseSelectList parses the projection list of a SELECT. `*` and `t.*` are
// supported via SelectItem.Star/Qualifier.
func (p *Parser) parseSelectList() []ast.SelectItem {
	var out []ast.SelectItem
	for {
		out = append(out, p.parseSelectItem())
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

func (p *Parser) parseSelectItem() ast.SelectItem {
	if p.isPunct("*") {
		p.advance()
		return ast.SelectItem{Star: true}
	}
	// Look ahead for `<ident> . *`.
	if (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD) && p.peek().Kind == TOK_PUNCT && p.peek().Lit == "." {
		saved := p.cur
		p.advance()
		p.advance() // '.'
		if p.isPunct("*") {
			p.advance()
			return ast.SelectItem{Star: true, Qualifier: saved.Lit}
		}
		// Reconstruct a partial Ident path and continue the expression.
		parts := []string{saved.Lit}
		if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
			parts = append(parts, p.cur.Lit)
			p.advance()
		}
		var head ast.Expr = &ast.Ident{Parts: parts, P: astPos(saved.Pos)}
		for p.isPunct(".") {
			p.advance()
			if n, _ := p.parseIdent(); n != "" {
				head.(*ast.Ident).Parts = append(head.(*ast.Ident).Parts, n)
			}
		}
		return p.finishSelectItem(head)
	}
	expr := p.parseExpr()
	return p.finishSelectItem(expr)
}

func (p *Parser) finishSelectItem(expr ast.Expr) ast.SelectItem {
	alias := ""
	if p.isKw("AS") {
		p.advance()
		alias, _ = p.parseIdent()
	} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		alias = p.cur.Lit
		p.advance()
	}
	return ast.SelectItem{Expr: expr, Alias: alias}
}

func (p *Parser) parseGroupByList() []ast.Expr {
	var out []ast.Expr
	for {
		// ROLLUP(...), CUBE(...), GROUPING SETS(...) are kept opaque — we
		// capture the call as a FuncCall via the regular expression parser.
		out = append(out, p.parseExpr())
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

func (p *Parser) parseOrderByClause() []ast.OrderItem {
	p.expectKw("ORDER")
	if p.isKw("SIBLINGS") {
		p.advance()
	}
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
		if p.isKw("NULLS") {
			p.advance()
			b := false
			if p.isKw("FIRST") {
				p.advance()
			} else if p.isKw("LAST") {
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

// parseFetchOffsetAtom reads the count/offset expression of OFFSET/FETCH.
func (p *Parser) parseFetchOffsetAtom() ast.Expr {
	if p.cur.Kind == TOK_NUMBER {
		v := p.cur.Lit
		pos := p.cur.Pos
		p.advance()
		return &ast.Literal{Kind: "number", Text: v, P: astPos(pos)}
	}
	return p.parseExpr()
}

// parseForUpdateClause captures `FOR UPDATE [OF cols] [NOWAIT|WAIT n|SKIP
// LOCKED]` and returns it as an opaque text trailer. Anything not starting
// with `FOR` returns "".
func (p *Parser) parseForUpdateClause() string {
	if !p.isKw("FOR") {
		return ""
	}
	startOff := p.cur.Pos.Offset
	p.advance()
	if p.isKw("UPDATE") || p.isKw("SHARE") {
		p.advance()
	}
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		if p.isKw("UNION") || p.isKw("INTERSECT") || p.isKw("MINUS") || p.isKw("EXCEPT") || p.isPunct(")") {
			break
		}
		p.advance()
	}
	return strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
}

// captureHierarchicalTail captures a `START WITH ... CONNECT BY ...` clause
// (in either order) verbatim. The translator emits a manual-review note.
func (p *Parser) captureHierarchicalTail() string {
	startOff := p.cur.Pos.Offset
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		if p.isKw("GROUP") || p.isKw("HAVING") || p.isKw("ORDER") ||
			p.isKw("OFFSET") || p.isKw("FETCH") || p.isKw("FOR") ||
			p.isKw("UNION") || p.isKw("INTERSECT") || p.isKw("MINUS") || p.isKw("EXCEPT") ||
			p.isPunct(")") {
			break
		}
		p.advance()
	}
	return strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
}

// ---------------------------------------------------------------------------
// FROM
// ---------------------------------------------------------------------------

func (p *Parser) parseFromList() []ast.FromItem {
	var out []ast.FromItem
	out = append(out, p.parseTableSourceWithJoins())
	for p.isPunct(",") {
		p.advance()
		out = append(out, p.parseTableSourceWithJoins())
	}
	return out
}

func (p *Parser) parseTableSourceWithJoins() ast.FromItem {
	first := p.parseTableSourceItem()
	for {
		next, ok := p.tryParseJoin(first)
		if !ok {
			return first
		}
		first = next
	}
}

func (p *Parser) parseTableSourceItem() ast.FromItem {
	start := p.cur.Pos
	if p.isKw("LATERAL") {
		p.advance()
	}
	if p.isPunct("(") {
		p.advance()
		if p.isKw("SELECT") || p.isKw("WITH") {
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
		// (table_ref) — a parenthesised join tree.
		ts := p.parseTableSourceWithJoins()
		for p.isPunct(",") {
			p.advance()
			next := p.parseTableSourceWithJoins()
			ts = &ast.FromJoin{Kind: ast.CrossJoin, Left: ts, Right: next, P: ts.Pos()}
		}
		p.expectPunct(")")
		return ts
	}
	ref := p.parseTableRef()
	t := &ast.FromTable{
		Schema: ref.Schema, Name: ref.Name, NameBacktick: ref.NameBacktick,
		P: astPos(start),
	}
	// Optional table-qualifier suffixes — flashback / dblink / partition —
	// captured verbatim into nothing; we just consume them.
	if p.isKw("PARTITION") || p.isKw("SUBPARTITION") {
		p.advance()
		p.captureBalancedParens()
	}
	if p.isKw("AS") {
		// Oracle does not normally write `AS` for a table alias, but tolerate
		// it for cross-dialect dumps.
		p.advance()
		t.Alias, _ = p.parseIdent()
	} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		t.Alias = p.cur.Lit
		p.advance()
	}
	return t
}

func (p *Parser) tryParseJoin(left ast.FromItem) (ast.FromItem, bool) {
	start := p.cur.Pos
	switch {
	case p.isKw("INNER") || p.isKw("CROSS"):
		kind := ast.InnerJoin
		if p.isKw("CROSS") {
			kind = ast.CrossJoin
		}
		p.advance()
		p.expectKw("JOIN")
		right := p.parseTableSourceItem()
		j := &ast.FromJoin{Kind: kind, Left: left, Right: right, P: astPos(start)}
		p.parseJoinSpec(j)
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
		return &ast.FromJoin{Kind: kind, Left: left, Right: right, Natural: true, P: astPos(start)}, true
	case p.isKw("JOIN"):
		p.advance()
		right := p.parseTableSourceItem()
		j := &ast.FromJoin{Kind: ast.InnerJoin, Left: left, Right: right, P: astPos(start)}
		p.parseJoinSpec(j)
		return j, true
	}
	return left, false
}

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

// ---------------------------------------------------------------------------
// WITH
// ---------------------------------------------------------------------------

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

// parseInsertStatement covers single-table `INSERT INTO t [(cols)] {VALUES
// (...) | <subquery>} [RETURNING ...]`. Multi-table INSERT (INSERT
// ALL/FIRST WHEN ... THEN INTO ...) is not modeled.
func (p *Parser) parseInsertStatement() *ast.InsertStmt {
	start := p.cur.Pos
	p.expectKw("INSERT")
	// optional hint /*+ ... */ — already stripped by lexer (TOK_COMMENT).
	if p.isKw("INTO") {
		p.advance()
	}
	ref := p.parseTableRef()
	stmt := &ast.InsertStmt{Table: ref, P: astPos(start)}
	if p.isPunct("(") {
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
	}
	switch {
	case p.isKw("VALUES") || p.isKw("VALUE"):
		p.parseInsertValues(stmt)
	case p.isKw("SELECT") || p.isKw("WITH") || p.isPunct("("):
		stmt.Select = p.parseSelectStatement()
	}
	if p.isKw("RETURNING") {
		p.advance()
		stmt.Returning = p.parseSelectList()
		// Optional `INTO vars` — drop it; PG's RETURNING just emits the
		// resultset, the caller wires it.
		if p.isKw("INTO") {
			p.advance()
			for {
				// Accept bind variables (`:v`) as well as identifiers — the
				// usual target of RETURNING … INTO in Oracle PL/SQL.
				if p.cur.Kind == TOK_BIND {
					p.advance()
				} else {
					_, _ = p.parseIdent()
				}
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
		}
	}
	return stmt
}

func (p *Parser) parseInsertValues(stmt *ast.InsertStmt) {
	p.advance() // VALUES / VALUE
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

func (p *Parser) parseValueExpr() ast.Expr {
	if p.isKw("DEFAULT") {
		start := p.cur.Pos
		p.advance()
		return &ast.Ident{Parts: []string{"DEFAULT"}, P: astPos(start)}
	}
	return p.parseExpr()
}

func parseUpdatedElements(p *Parser) []ast.Assign {
	var out []ast.Assign
	for {
		// Oracle tuple form: `(c1, c2, ..., cn) = (SELECT v1, …, vn FROM …)`
		// or `(c1, …, cn) = (v1, …, vn)`. Both PG and Oracle parse this; we
		// emit a single Assign with TupleCols populated and Expr holding the
		// right-hand source expression verbatim (parens included so the PG
		// writer renders it as-is).
		//
		// At SET-list position a leading `(` unambiguously opens a tuple
		// target — there is no other Oracle UPDATE SET form that starts
		// with `(`. So we don't need to backtrack: consume the paren list
		// of column names, then `=`, then the RHS expression.
		if p.isPunct("(") && (p.peek().Kind == TOK_IDENT || p.peek().Kind == TOK_QUOTED_IDENT) {
			p.advance() // consume '('
			var cols []string
			for {
				c, _ := p.parseIdent()
				cols = append(cols, c)
				if p.isPunct(",") {
					p.advance()
					continue
				}
				break
			}
			p.expectPunct(")")
			if p.cur.Kind == TOK_ASSIGN {
				p.advance()
			} else {
				p.expectPunct("=")
			}
			val := p.parseValueExpr()
			out = append(out, ast.Assign{TupleCols: cols, Expr: val})
			if !p.isPunct(",") {
				break
			}
			p.advance()
			continue
		}
		col, _ := p.parseIdent()
		for p.isPunct(".") {
			p.advance()
			c2, _ := p.parseIdent()
			col = col + "." + c2
		}
		// Oracle uses `=` for set assignments; tolerate `:=` for round-trip.
		if p.cur.Kind == TOK_ASSIGN {
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

func (p *Parser) parseUpdateStatement() *ast.UpdateStmt {
	start := p.cur.Pos
	p.expectKw("UPDATE")
	// Oracle allows `UPDATE (subquery) SET …` (updatable view inline). We
	// handle both shapes.
	stmt := &ast.UpdateStmt{P: astPos(start)}
	if p.isPunct("(") {
		// updatable inline view — treat the subquery as the source. Push
		// onto From; Table stays zeroed.
		ts := p.parseTableSourceItem()
		stmt.From = []ast.FromItem{ts}
	} else {
		ref := p.parseTableRef()
		stmt.Table = ref
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
	if p.isKw("RETURNING") {
		p.advance()
		stmt.Returning = p.parseSelectList()
		if p.isKw("INTO") {
			p.advance()
			for {
				// Accept bind variables (`:v`) as well as identifiers — the
				// usual target of RETURNING … INTO in Oracle PL/SQL.
				if p.cur.Kind == TOK_BIND {
					p.advance()
				} else {
					_, _ = p.parseIdent()
				}
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
		}
	}
	return stmt
}

// ---------------------------------------------------------------------------
// DELETE
// ---------------------------------------------------------------------------

func (p *Parser) parseDeleteStatement() *ast.DeleteStmt {
	start := p.cur.Pos
	p.expectKw("DELETE")
	stmt := &ast.DeleteStmt{P: astPos(start)}
	if p.isKw("FROM") {
		p.advance()
	}
	if p.isPunct("(") {
		// inline view target
		ts := p.parseTableSourceItem()
		stmt.Using = []ast.FromItem{ts}
	} else {
		ref := p.parseTableRef()
		stmt.Table = ref
		if p.isKw("AS") {
			p.advance()
			stmt.Alias, _ = p.parseIdent()
		} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
			stmt.Alias = p.cur.Lit
			p.advance()
		}
	}
	if p.isKw("WHERE") {
		p.advance()
		stmt.Where = p.parseExpr()
	}
	if p.isKw("RETURNING") {
		p.advance()
		stmt.Returning = p.parseSelectList()
		if p.isKw("INTO") {
			p.advance()
			for {
				// Accept bind variables (`:v`) as well as identifiers — the
				// usual target of RETURNING … INTO in Oracle PL/SQL.
				if p.cur.Kind == TOK_BIND {
					p.advance()
				} else {
					_, _ = p.parseIdent()
				}
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
		}
	}
	return stmt
}

// ---------------------------------------------------------------------------
// MERGE
// ---------------------------------------------------------------------------

// parseMergeStatement parses `MERGE INTO target [alias] USING source [alias]
// ON (cond) WHEN [NOT] MATCHED [AND cond] THEN <action> [...]`.
//
// Oracle MERGE accepts `LOG ERRORS` trailers and the `WHEN MATCHED THEN
// UPDATE SET ... DELETE WHERE ...` combo; both are tolerated by skipping
// past unrecognised tail content (the existing translator surfaces them
// as warnings in plsql_xlate.go).
func (p *Parser) parseMergeStatement() *ast.MergeStmt {
	start := p.cur.Pos
	p.expectKw("MERGE")
	p.expectKw("INTO")
	stmt := &ast.MergeStmt{P: astPos(start)}
	stmt.Target = p.parseTableRef()
	if p.isKw("AS") {
		p.advance()
		stmt.TargetAlias, _ = p.parseIdent()
	} else if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		stmt.TargetAlias = p.cur.Lit
		p.advance()
	}
	p.expectKw("USING")
	stmt.Source = p.parseTableSourceItem()
	p.expectKw("ON")
	if p.isPunct("(") {
		p.advance()
		stmt.On = p.parseExpr()
		p.expectPunct(")")
	} else {
		stmt.On = p.parseExpr()
	}
	for p.isKw("WHEN") {
		p.advance()
		not := false
		if p.isKw("NOT") {
			not = true
			p.advance()
		}
		p.expectKw("MATCHED")
		// Optional `AND <cond>`.
		var cond ast.Expr
		if p.isKw("AND") {
			p.advance()
			cond = p.parseExpr()
		}
		p.expectKw("THEN")
		switch {
		case p.isKw("UPDATE"):
			p.advance()
			p.expectKw("SET")
			act := ast.MergeAction{Kind: "UPDATE", Cond: cond, Sets: parseUpdatedElements(p)}
			// Trailing `WHERE cond` on UPDATE: Oracle filters which matched
			// rows to update. PG has no inline filter on the UPDATE branch
			// — we lift it into the WHEN guard via AND so the semantics are
			// preserved.
			if p.isKw("WHERE") {
				p.advance()
				whereCond := p.parseExpr()
				if act.Cond == nil {
					act.Cond = whereCond
				} else {
					act.Cond = &ast.BinaryExpr{Op: "AND", Lhs: act.Cond, Rhs: whereCond, P: act.Cond.Pos()}
				}
			}
			// Oracle's `... UPDATE SET ... DELETE WHERE ...` combo — the
			// inline DELETE phase has no PG equivalent. We drop it here and
			// flag the action so the translator surfaces a remediation note.
			if p.isKw("DELETE") {
				p.advance()
				if p.isKw("WHERE") {
					p.advance()
					_ = p.parseExpr()
				}
				act.HasInlineDelete = true
			}
			if not {
				stmt.WhenNotMatched = append(stmt.WhenNotMatched, act)
			} else {
				stmt.WhenMatched = append(stmt.WhenMatched, act)
			}
		case p.isKw("DELETE"):
			p.advance()
			act := ast.MergeAction{Kind: "DELETE", Cond: cond}
			if p.isKw("WHERE") {
				p.advance()
				whereCond := p.parseExpr()
				if act.Cond == nil {
					act.Cond = whereCond
				} else {
					act.Cond = &ast.BinaryExpr{Op: "AND", Lhs: act.Cond, Rhs: whereCond, P: act.Cond.Pos()}
				}
			}
			if not {
				stmt.WhenNotMatched = append(stmt.WhenNotMatched, act)
			} else {
				stmt.WhenMatched = append(stmt.WhenMatched, act)
			}
		case p.isKw("INSERT"):
			p.advance()
			act := ast.MergeAction{Kind: "INSERT", Cond: cond}
			if p.isPunct("(") {
				p.advance()
				for {
					n, _ := p.parseIdent()
					if n != "" {
						act.InsertCols = append(act.InsertCols, n)
					}
					if !p.isPunct(",") {
						break
					}
					p.advance()
				}
				p.expectPunct(")")
			}
			if p.isKw("VALUES") {
				p.advance()
				p.expectPunct("(")
				if !p.isPunct(")") {
					act.InsertValues = append(act.InsertValues, p.parseValueExpr())
					for p.isPunct(",") {
						p.advance()
						act.InsertValues = append(act.InsertValues, p.parseValueExpr())
					}
				}
				p.expectPunct(")")
			}
			if p.isKw("WHERE") {
				p.advance()
				whereCond := p.parseExpr()
				if act.Cond == nil {
					act.Cond = whereCond
				} else {
					act.Cond = &ast.BinaryExpr{Op: "AND", Lhs: act.Cond, Rhs: whereCond, P: act.Cond.Pos()}
				}
			}
			if not {
				stmt.WhenNotMatched = append(stmt.WhenNotMatched, act)
			} else {
				stmt.WhenMatched = append(stmt.WhenMatched, act)
			}
		case p.isKw("DO"):
			p.advance()
			if p.isKw("NOTHING") {
				p.advance()
			}
			act := ast.MergeAction{Kind: "DO NOTHING", Cond: cond}
			if not {
				stmt.WhenNotMatched = append(stmt.WhenNotMatched, act)
			} else {
				stmt.WhenMatched = append(stmt.WhenMatched, act)
			}
		}
	}
	// Tolerate `LOG ERRORS [INTO …] [REJECT LIMIT n|UNLIMITED]` trailer.
	// PG MERGE has no equivalent — the parser flags it on the AST so the
	// translator emits a remediation warning.
	if p.isKw("LOG") {
		stmt.HasLogErrors = true
		for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
			p.advance()
		}
	}
	return stmt
}
