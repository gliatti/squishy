package tsql

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// T-SQL DML parser — SELECT / INSERT / UPDATE / DELETE / MERGE.
//
// Coverage follows reference/TSqlParser.g4: select_statement_standalone,
// with_expression / common_table_expression, query_expression,
// query_specification (TOP, INTO, FROM, WHERE, GROUP BY [WITH ROLLUP|CUBE],
// HAVING), select_order_by_clause (ORDER BY, OFFSET … FETCH), for_clause
// (kept raw), option_clause (kept raw), table_sources / table_source_item
// (tables with hints, derived tables, VALUES constructors, table-valued
// functions, XML nodes(), full-text table functions, PIVOT / UNPIVOT),
// join_part (INNER/LEFT/RIGHT/FULL [OUTER] JOIN, CROSS JOIN, CROSS/OUTER
// APPLY, nested join syntax), insert_statement, update_statement,
// delete_statement and merge_statement (output_clause kept raw).
//
// Table hints (`WITH (NOLOCK)`, legacy `(NOLOCK)`) and join hints
// (LOOP / HASH / MERGE / REMOTE) are parsed and dropped: they never change
// a query's result.
// ---------------------------------------------------------------------------

// ParseSelect parses a stand-alone SELECT (with optional WITH preamble).
// Trailing tokens other than ';' are reported as an error.
func ParseSelect(src string) (*ast.SelectStmt, ErrorList) {
	p := newParser(src)
	stmt := p.parseSelectStatement()
	p.expectStandaloneEnd()
	return stmt, p.errs
}

// ParseExpr parses a stand-alone expression. The whole input must be
// consumed.
func ParseExpr(src string) (ast.Expr, ErrorList) {
	p := newParser(src)
	e := p.parseExpr()
	if p.cur.Kind != TOK_EOF {
		p.errorHere("trailing tokens after expression", "EOF")
	}
	return e, p.errs
}

// ParseInsert parses a stand-alone INSERT statement.
func ParseInsert(src string) (*ast.InsertStmt, ErrorList) {
	p := newParser(src)
	stmt := p.parseInsertStatement(nil)
	p.expectStandaloneEnd()
	return stmt, p.errs
}

// ParseUpdate parses a stand-alone UPDATE statement.
func ParseUpdate(src string) (*ast.UpdateStmt, ErrorList) {
	p := newParser(src)
	stmt := p.parseUpdateStatement(nil)
	p.expectStandaloneEnd()
	return stmt, p.errs
}

// ParseDelete parses a stand-alone DELETE statement.
func ParseDelete(src string) (*ast.DeleteStmt, ErrorList) {
	p := newParser(src)
	stmt := p.parseDeleteStatement(nil)
	p.expectStandaloneEnd()
	return stmt, p.errs
}

// expectStandaloneEnd accepts optional ';' / GO tokens and then requires
// the end of input.
func (p *Parser) expectStandaloneEnd() {
	for p.isPunct(";") || p.cur.Kind == TOK_GO {
		p.advance()
	}
	if p.cur.Kind != TOK_EOF {
		p.errorHere("unexpected token after statement", "EOF")
	}
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

// parseSelectStatement parses
//
//	[WITH cte, …] query_expression [ORDER BY …] [OFFSET n ROWS [FETCH …]]
//	[FOR XML|JSON|BROWSE …] [OPTION (…)]
func (p *Parser) parseSelectStatement() *ast.SelectStmt {
	var with *ast.WithClause
	if p.isKw("WITH") {
		with = p.parseWithClause()
	}
	stmt := p.parseQueryExpression()
	if stmt == nil {
		return nil
	}
	if with != nil {
		stmt.With = with
	}
	p.parseSelectTail(stmt)
	return stmt
}

// parseSelectTail parses the clauses that follow a query expression.
func (p *Parser) parseSelectTail(stmt *ast.SelectStmt) {
	if p.isKw("ORDER") && p.peekIsKw(1, "BY") {
		p.advance()
		p.advance()
		stmt.OrderBy = p.parseOrderByClause()
	}
	if p.isKw("OFFSET") {
		p.advance()
		stmt.Offset = p.parseExpr()
		if p.isKw("ROW") || p.isKw("ROWS") {
			p.advance()
		} else {
			p.errorHere("expected ROWS", "ROWS")
		}
		if p.isKw("FETCH") {
			p.advance()
			if p.isKw("FIRST") || p.isKw("NEXT") {
				p.advance()
			} else {
				p.errorHere("expected FIRST or NEXT", "FIRST|NEXT")
			}
			stmt.Limit = p.parseExpr()
			if p.isKw("ROW") || p.isKw("ROWS") {
				p.advance()
			} else {
				p.errorHere("expected ROWS", "ROWS")
			}
			p.expectKw("ONLY")
		}
	}
	if p.isKw("FOR") && (p.peekIsKw(1, "XML") || p.peekIsKw(1, "JSON") || p.peekIsKw(1, "BROWSE")) {
		stmt.ForXMLKind, stmt.ForXML = p.parseForClause()
	}
	if p.isKw("OPTION") && p.peekIsPunct(1, "(") {
		stmt.OptionClause = p.parseOptionClause()
	}
}

// parseForClause parses `FOR XML …` / `FOR JSON …` / `FOR BROWSE` and
// returns the clause family and its raw text.
func (p *Parser) parseForClause() (string, string) {
	start := p.cur
	p.advance() // FOR
	kind := strings.ToUpper(p.cur.Lit)
	p.advance()
	if kind == "BROWSE" {
		return kind, p.textFrom(start)
	}
	// directive [ ( 'name' ) ] { , directive [ ( … ) ] }
	for {
		if p.isIdentTok() || p.cur.Kind == TOK_KEYWORD && !p.isStmtStartKeyword() && !p.isKw("OPTION") && !p.isKw("FOR") {
			p.advance()
			for p.isIdentTok() && !p.cur.LineStart {
				// two-word directives: BINARY BASE64, ELEMENTS XSINIL,
				// INCLUDE_NULL_VALUES …
				p.advance()
			}
			if p.isPunct("(") {
				p.skipBalancedParens()
			}
		}
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return kind, p.textFrom(start)
}

// parseOptionClause parses `OPTION ( hint, … )` and returns its raw text.
func (p *Parser) parseOptionClause() string {
	start := p.cur
	p.advance() // OPTION
	p.skipBalancedParens()
	return p.textFrom(start)
}

// parseQueryExpression parses query terms combined with UNION [ALL] /
// EXCEPT / INTERSECT (left to right, as the mysql parser does).
func (p *Parser) parseQueryExpression() *ast.SelectStmt {
	stmt := p.parseQueryTerm()
	if stmt == nil {
		return nil
	}
	for p.isKw("UNION") || p.isKw("EXCEPT") || p.isKw("INTERSECT") {
		op := p.cur.Lit
		p.advance()
		if p.isKw("ALL") {
			op += " ALL"
			p.advance()
		}
		next := p.parseQueryTerm()
		if next == nil {
			break
		}
		stmt.SetOps = append(stmt.SetOps, ast.SetOp{Op: op, Stmt: next})
	}
	return stmt
}

// parseQueryTerm parses a query specification or a parenthesised query.
func (p *Parser) parseQueryTerm() *ast.SelectStmt {
	if p.isPunct("(") {
		p.advance()
		inner := p.parseSelectStatement()
		p.expectPunct(")")
		return inner
	}
	return p.parseQuerySpecification()
}

// parseQuerySpecification parses one SELECT … [INTO] [FROM] [WHERE]
// [GROUP BY] [HAVING] block.
func (p *Parser) parseQuerySpecification() *ast.SelectStmt {
	start := p.cur.Pos
	if !p.expectKw("SELECT") {
		return nil
	}
	stmt := &ast.SelectStmt{P: astPos(start)}
	if p.isKw("DISTINCT") {
		stmt.Distinct = true
		p.advance()
	} else if p.isKw("ALL") {
		stmt.AllOnly = true
		p.advance()
	}
	if p.isKw("TOP") {
		p.advance()
		stmt.Top = p.parseTopValue()
		if p.isKw("PERCENT") {
			stmt.TopPercent = true
			p.advance()
		}
		if p.isKw("WITH") && p.peekIsKw(1, "TIES") {
			stmt.TopWithTies = true
			p.advance()
			p.advance()
		}
	}
	var vars []string
	stmt.Cols, vars = p.parseSelectElements()
	if vars != nil {
		p.assigns[stmt] = vars
	}
	if p.isKw("INTO") {
		p.advance()
		ref := p.parseTableRef()
		stmt.IntoTable = &ref
	}
	if p.isKw("FROM") {
		p.advance()
		stmt.From = p.parseTableSources()
	}
	if p.isKw("WHERE") {
		p.advance()
		stmt.Where = p.parseExpr()
	}
	if p.isKw("GROUP") && p.peekIsKw(1, "BY") {
		p.advance()
		p.advance()
		if p.isKw("ALL") {
			p.advance()
		}
		stmt.GroupBy = p.parseGroupByItems()
		// `GROUP BY a, b WITH ROLLUP|CUBE` is the legacy spelling of
		// `GROUP BY ROLLUP(a, b)` / `CUBE(a, b)`, which is also PG's.
		if p.isKw("WITH") && (p.peekIsKw(1, "ROLLUP") || p.peekIsKw(1, "CUBE")) {
			p.advance()
			fstart := p.cur.Pos
			name := strings.ToUpper(p.cur.Lit)
			p.advance()
			stmt.GroupBy = []ast.Expr{&ast.FuncCall{Name: name, Args: stmt.GroupBy, P: astPos(fstart)}}
		}
	}
	if p.isKw("HAVING") {
		p.advance()
		stmt.Having = p.parseExpr()
	}
	return stmt
}

// parseTopValue parses the value of TOP: `(expr)` or a bare number /
// variable (legacy form).
func (p *Parser) parseTopValue() ast.Expr {
	if p.isPunct("(") {
		start := p.cur.Pos
		p.advance()
		e := p.parseExpr()
		p.expectPunct(")")
		return &ast.ParenExpr{Inner: e, P: astPos(start)}
	}
	if p.cur.Kind == TOK_NUMBER || p.cur.Kind == TOK_VARIABLE {
		return p.parsePrimary()
	}
	p.errorHere("expected TOP value", "(expr) | number")
	return &ast.RawExpr{P: astPos(p.cur.Pos)}
}

// parseSelectElements parses the select list. vars is non-nil when the
// list uses the variable-assignment form (`SELECT @a = x, @b += y`): one
// variable per item, all items must assign.
func (p *Parser) parseSelectElements() ([]ast.SelectItem, []string) {
	var items []ast.SelectItem
	var vars []string
	assigning := false
	for {
		item, v := p.parseSelectElement()
		items = append(items, item)
		if v != "" {
			assigning = true
		}
		vars = append(vars, v)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	if !assigning {
		return items, nil
	}
	for _, v := range vars {
		if v == "" {
			p.errorHere("a SELECT that assigns variables cannot also return data", "@var = expr")
			break
		}
	}
	return items, vars
}

// compoundOps maps T-SQL compound assignment operators to the binary
// operator they apply.
var compoundOps = map[string]string{
	"+=": "+", "-=": "-", "*=": "*", "/=": "/", "%=": "%", "&=": "&", "^=": "^", "|=": "|",
}

// parseSelectElement parses one select item: `*`, `t.*`, `expr [[AS]
// alias]`, `alias = expr`, or a variable assignment `@v = expr` /
// `@v += expr` (returned with the variable name in v).
func (p *Parser) parseSelectElement() (ast.SelectItem, string) {
	if p.isPunct("*") {
		p.advance()
		return ast.SelectItem{Star: true}, ""
	}
	// qualifier.*: ident (. ident)* . *
	if p.isIdentTok() {
		j := p.i
		var qual []string
		for j+1 < len(p.toks) {
			t := p.toks[j]
			if t.Kind != TOK_IDENT && t.Kind != TOK_QUOTED_IDENT {
				break
			}
			nt := p.toks[j+1]
			if nt.Kind != TOK_PUNCT || nt.Lit != "." {
				break
			}
			qual = append(qual, t.Lit)
			if j+2 < len(p.toks) && p.toks[j+2].Kind == TOK_PUNCT && p.toks[j+2].Lit == "*" {
				for p.i < j+3 {
					p.advance()
				}
				return ast.SelectItem{Star: true, Qualifier: strings.Join(qual, ".")}, ""
			}
			j += 2
		}
	}
	if p.cur.Kind == TOK_VARIABLE && p.peek(1).Kind == TOK_PUNCT {
		nt := p.peek(1)
		if nt.Lit == "=" || compoundOps[nt.Lit] != "" {
			v := p.cur
			p.advance()
			p.advance()
			e := p.parseExpr()
			if op := compoundOps[nt.Lit]; op != "" {
				e = &ast.BinaryExpr{Op: op, Lhs: &ast.Ident{Parts: []string{v.Lit}, Variable: true, P: astPos(v.Pos)}, Rhs: e, P: astPos(v.Pos)}
			}
			return ast.SelectItem{Expr: e}, v.Lit
		}
	}
	// alias = expr
	if (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_STRING) && p.peekIsPunct(1, "=") {
		alias := p.cur.Lit
		p.advance()
		p.advance()
		return ast.SelectItem{Expr: p.parseExpr(), Alias: alias}, ""
	}
	e := p.parseExpr()
	item := ast.SelectItem{Expr: e}
	item.Alias = p.parseOptionalAlias(true)
	return item, ""
}

// parseOptionalAlias parses `[AS] alias`. A string literal is accepted as
// an alias when allowString is true (select list).
func (p *Parser) parseOptionalAlias(allowString bool) string {
	if p.isKw("AS") {
		p.advance()
		if allowString && p.cur.Kind == TOK_STRING {
			v := p.cur.Lit
			p.advance()
			return v
		}
		n, _ := p.parseIdent()
		return n
	}
	if p.canBeAlias() || (allowString && p.cur.Kind == TOK_STRING) {
		v := p.cur.Lit
		p.advance()
		return v
	}
	return ""
}

// canBeAlias reports whether the current token can be an implicit alias:
// an identifier that is neither reserved nor a statement-starting word,
// nor a label (`lbl:`).
func (p *Parser) canBeAlias() bool {
	switch p.cur.Kind {
	case TOK_QUOTED_IDENT:
		return true
	case TOK_IDENT:
		return !p.isContextualStmtWord() && !p.peekIsPunct(1, ":")
	}
	return false
}

// parseGroupByItems parses the GROUP BY expression list.
func (p *Parser) parseGroupByItems() []ast.Expr {
	var out []ast.Expr
	for {
		out = append(out, p.parseExpr())
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseOrderByClause parses the items after ORDER BY.
func (p *Parser) parseOrderByClause() []ast.OrderItem {
	var out []ast.OrderItem
	for {
		it := ast.OrderItem{Expr: p.parseExpr()}
		if p.isKw("ASC") {
			p.advance()
		} else if p.isKw("DESC") {
			it.Desc = true
			p.advance()
		}
		out = append(out, it)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseWithClause parses `WITH [XMLNAMESPACES (…),] cte [, cte]*`. A CTE
// whose body reads its own name is recursive (T-SQL has no RECURSIVE
// keyword; PG requires it), which sets WithClause.Recursive.
func (p *Parser) parseWithClause() *ast.WithClause {
	p.expectKw("WITH")
	w := &ast.WithClause{}
	if p.isKw("XMLNAMESPACES") {
		p.advance()
		p.skipBalancedParens()
		if !p.isPunct(",") {
			return w
		}
		p.advance()
	}
	for {
		name, _ := p.parseIdent()
		cte := ast.CTE{Name: name}
		if p.isPunct("(") {
			cte.Columns = p.parseIdentList()
		}
		p.expectKw("AS")
		p.expectPunct("(")
		cte.Body = p.parseSelectStatement()
		p.expectPunct(")")
		if cte.Body != nil && selectReads(cte.Body, name) {
			w.Recursive = true
		}
		w.CTEs = append(w.CTEs, cte)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return w
}

// fromTableFinder is an ast.Visitor that looks for a FROM-clause table of
// a given (one-part) name.
type fromTableFinder struct {
	name  string
	found bool
}

func (f *fromTableFinder) Visit(n ast.Node) ast.Visitor {
	if f.found {
		return nil
	}
	if ft, ok := n.(*ast.FromTable); ok && ft.Schema == "" && strings.EqualFold(ft.Name, f.name) {
		f.found = true
		return nil
	}
	return f
}

// selectReads reports whether sel reads the table name (case-insensitive,
// SQL Server's default collation) anywhere in its FROM clauses.
func selectReads(sel *ast.SelectStmt, name string) bool {
	f := &fromTableFinder{name: name}
	ast.Walk(f, sel)
	return f.found
}

// ---------------------------------------------------------------------------
// FROM clause
// ---------------------------------------------------------------------------

// parseTableSources parses the comma-separated FROM list.
func (p *Parser) parseTableSources() []ast.FromItem {
	var out []ast.FromItem
	for {
		out = append(out, p.parseTableSource())
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseTableSource parses a table source item followed by its joins.
func (p *Parser) parseTableSource() ast.FromItem {
	item := p.parseTableSourceItem()
	for {
		joined, ok := p.tryParseJoinPart(item)
		if !ok {
			return item
		}
		item = joined
	}
}

// tableFuncKeywords are reserved words that name table-valued functions.
var tableFuncKeywords = map[string]bool{
	"CONTAINSTABLE": true, "FREETEXTTABLE": true, "OPENXML": true, "OPENROWSET": true,
	"OPENQUERY": true, "OPENDATASOURCE": true,
	"SEMANTICKEYPHRASETABLE": true, "SEMANTICSIMILARITYTABLE": true,
	"SEMANTICSIMILARITYDETAILSTABLE": true,
}

// tableHints are the legacy table hints accepted without WITH: `t (NOLOCK)`.
var tableHints = map[string]bool{
	"NOLOCK": true, "READUNCOMMITTED": true, "UPDLOCK": true, "REPEATABLEREAD": true,
	"SERIALIZABLE": true, "READCOMMITTED": true, "TABLOCK": true, "TABLOCKX": true,
	"PAGLOCK": true, "ROWLOCK": true, "NOWAIT": true, "READPAST": true, "XLOCK": true,
	"SNAPSHOT": true, "NOEXPAND": true, "HOLDLOCK": true, "INDEX": true, "FORCESEEK": true,
	"FORCESCAN": true, "READCOMMITTEDLOCK": true,
}

// parseTableSourceItem parses one FROM source with its alias, hints and
// PIVOT / UNPIVOT suffixes.
func (p *Parser) parseTableSourceItem() ast.FromItem {
	start := p.cur
	pos := astPos(start.Pos)
	var item ast.FromItem
	switch {
	case p.isPunct("("):
		switch {
		case p.startsQuery():
			p.advance()
			sub := p.parseSelectStatement()
			p.expectPunct(")")
			fs := &ast.FromSubquery{Stmt: sub, P: pos}
			fs.Alias = p.parseOptionalAlias(false)
			if p.isPunct("(") {
				fs.Cols = p.parseIdentList()
			}
			item = fs
		case p.peekIsKw(1, "VALUES"):
			p.advance()
			p.advance()
			fv := &ast.FromValues{Rows: p.parseValuesRows(), P: pos}
			p.expectPunct(")")
			fv.Alias = p.parseOptionalAlias(false)
			if p.isPunct("(") {
				fv.Cols = p.parseIdentList()
			}
			item = fv
		default:
			// Parenthesised join tree: `(a JOIN b ON …)`.
			p.advance()
			inner := p.parseTableSource()
			p.expectPunct(")")
			item = inner
		}
	case p.cur.Kind == TOK_VARIABLE:
		if p.peekIsPunct(1, ".") && p.peekIsPunct(3, "(") {
			// @xml.nodes('…') AS t(c)
			p.advance()
			p.advance()
			method, _ := p.parseIdentAny()
			call := p.finishMethodCall(start, &ast.Ident{Parts: []string{start.Lit}, Variable: true, P: pos}, method)
			item = p.finishFromFunc(call, pos)
		} else {
			p.advance()
			ft := &ast.FromTable{Name: start.Lit, P: pos}
			p.skipTableHints()
			ft.Alias = p.parseOptionalAlias(false)
			p.skipTableHints()
			item = ft
		}
	case p.cur.Kind == TOK_KEYWORD && tableFuncKeywords[p.cur.Lit] && p.peekIsPunct(1, "("):
		name := p.cur.Lit
		p.advance()
		call := p.finishFuncCall(start, name)
		item = p.finishFromFunc(call, pos)
	case p.isIdentTok():
		parts, quoted := p.parseMultiPartName()
		if p.isPunct("(") && !p.parensAreTableHints() {
			var call ast.Expr
			last := strings.ToLower(parts[len(parts)-1])
			if len(parts) >= 2 && (xmlMethods[last] || clrMethods[last]) {
				call = p.finishMethodCall(start, &ast.Ident{Parts: parts[:len(parts)-1], Backtick: quoted, P: pos}, parts[len(parts)-1])
			} else {
				name := strings.Join(parts, ".")
				if len(parts) == 1 && !quoted {
					name = strings.ToUpper(name)
				}
				call = p.finishFuncCall(start, name)
			}
			if p.isKw("WITH") && p.peekIsPunct(1, "(") {
				// OPENJSON(…) WITH (col type '$.path', …): the explicit
				// schema has no PG spelling; parse and drop it with the
				// call kept for the translator to report.
				p.advance()
				p.skipBalancedParens()
			}
			item = p.finishFromFunc(call, pos)
		} else {
			ref := tableRefOf(parts, quoted)
			ft := &ast.FromTable{Schema: ref.Schema, Name: ref.Name, NameBacktick: ref.NameBacktick, P: pos}
			if p.isKw("FOR") && p.peekIsKw(1, "SYSTEM_TIME") {
				p.errorHere("FOR SYSTEM_TIME temporal queries are not supported", "")
			}
			if p.isKw("TABLESAMPLE") {
				p.advance()
				if p.isKw("SYSTEM") {
					p.advance()
				}
				p.skipBalancedParens()
				if p.isKw("REPEATABLE") {
					p.advance()
					p.skipBalancedParens()
				}
			}
			p.skipTableHints()
			ft.Alias = p.parseOptionalAlias(false)
			p.skipTableHints()
			item = ft
		}
	default:
		p.errorHere("expected table source", "table | (subquery) | function")
		if !p.isPunct(";") && p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO && !p.isStmtStartKeyword() {
			p.advance()
		}
		return &ast.FromTable{P: pos}
	}
	for p.isKw("PIVOT") || p.isKw("UNPIVOT") {
		item = p.parsePivot(item)
	}
	return item
}

// finishFromFunc attaches the alias and column-alias list of a
// table-valued function source.
func (p *Parser) finishFromFunc(call ast.Expr, pos ast.Position) ast.FromItem {
	ff := &ast.FromFunc{Func: call, P: pos}
	ff.Alias = p.parseOptionalAlias(false)
	if p.isPunct("(") {
		ff.Cols = p.parseIdentList()
	}
	return ff
}

// parensAreTableHints reports whether the '(' after a table name opens a
// legacy hint list (`t (NOLOCK)`) rather than a function call.
func (p *Parser) parensAreTableHints() bool {
	t := p.peek(1)
	if t.Kind != TOK_IDENT && t.Kind != TOK_KEYWORD {
		return false
	}
	return tableHints[strings.ToUpper(t.Lit)] && (p.peekIsPunct(2, ")") || p.peekIsPunct(2, ",") || p.peekIsPunct(2, "("))
}

// skipTableHints consumes `WITH ( hint, … )` or a legacy `( hint, … )`.
func (p *Parser) skipTableHints() {
	if p.isKw("WITH") && p.peekIsPunct(1, "(") {
		p.advance()
		p.skipBalancedParens()
		return
	}
	if p.isPunct("(") && p.parensAreTableHints() {
		p.skipBalancedParens()
	}
}

// parsePivot parses a PIVOT / UNPIVOT suffix applied to src.
func (p *Parser) parsePivot(src ast.FromItem) ast.FromItem {
	start := p.cur.Pos
	fp := &ast.FromPivot{Source: src, Unpivot: p.isKw("UNPIVOT"), P: astPos(start)}
	p.advance()
	p.expectPunct("(")
	if fp.Unpivot {
		fp.ValueCol, _ = p.parseIdent()
	} else {
		fp.Agg = p.parseExpr()
	}
	p.expectKw("FOR")
	if fp.Unpivot {
		fp.ForCol, _ = p.parseIdent()
	} else {
		parts, _ := p.parseMultiPartName()
		fp.ForCol = parts[len(parts)-1]
	}
	p.expectKw("IN")
	fp.InCols = p.parseIdentList()
	p.expectPunct(")")
	fp.Alias = p.parseOptionalAlias(false)
	return fp
}

// tryParseJoinPart parses one join_part applied to left.
func (p *Parser) tryParseJoinPart(left ast.FromItem) (ast.FromItem, bool) {
	start := p.cur.Pos
	switch {
	case p.isKw("CROSS") && p.peekIsKw(1, "JOIN"):
		p.advance()
		p.advance()
		right := p.parseTableSourceItem()
		return &ast.FromJoin{Kind: ast.CrossJoin, Left: left, Right: right, P: astPos(start)}, true
	case p.isKw("CROSS") && p.peekIsKw(1, "APPLY"):
		p.advance()
		p.advance()
		right := p.parseTableSourceItem()
		return &ast.FromJoin{Kind: ast.CrossApply, Left: left, Right: right, P: astPos(start)}, true
	case p.isKw("OUTER") && p.peekIsKw(1, "APPLY"):
		p.advance()
		p.advance()
		right := p.parseTableSourceItem()
		return &ast.FromJoin{Kind: ast.OuterApply, Left: left, Right: right, P: astPos(start)}, true
	}
	kind, ok := p.parseQualifiedJoinKeywords()
	if !ok {
		return nil, false
	}
	right := p.parseJoinedRight()
	j := &ast.FromJoin{Kind: kind, Left: left, Right: right, P: astPos(start)}
	if p.expectKw("ON") {
		j.On = p.parseExpr()
	}
	return j, true
}

// parseQualifiedJoinKeywords consumes `[INNER] [hint] JOIN`,
// `{LEFT|RIGHT|FULL} [OUTER] [hint] JOIN` and returns the join kind.
func (p *Parser) parseQualifiedJoinKeywords() (ast.JoinKind, bool) {
	kind := ast.InnerJoin
	switch {
	case p.isKw("JOIN"):
	case p.isKw("INNER"):
		p.advance()
	case p.isKw("LEFT"), p.isKw("RIGHT"), p.isKw("FULL"):
		switch p.cur.Lit {
		case "LEFT":
			kind = ast.LeftJoin
		case "RIGHT":
			kind = ast.RightJoin
		default:
			kind = ast.FullJoin
		}
		if p.peekIsPunct(1, "(") {
			return 0, false // LEFT(…) / RIGHT(…) function, not a join
		}
		p.advance()
		if p.isKw("OUTER") {
			p.advance()
		}
	default:
		return 0, false
	}
	// join hint
	if p.isKw("LOOP") || p.isKw("HASH") || p.isKw("MERGE") || p.isKw("REMOTE") {
		p.advance()
	}
	if !p.expectKw("JOIN") {
		return kind, true
	}
	return kind, true
}

// isQualifiedJoinStart reports whether a qualified join (one that takes
// an ON clause) starts at the current token.
func (p *Parser) isQualifiedJoinStart() bool {
	switch {
	case p.isKw("JOIN"), p.isKw("INNER"):
		return true
	case p.isKw("LEFT"), p.isKw("RIGHT"), p.isKw("FULL"):
		return !p.peekIsPunct(1, "(")
	}
	return false
}

// parseJoinedRight parses the right operand of a qualified join. It
// supports the nested join syntax `a JOIN b JOIN c ON x ON y`, where the
// right operand is itself a join tree whose ON comes first.
func (p *Parser) parseJoinedRight() ast.FromItem {
	item := p.parseTableSourceItem()
	for p.isQualifiedJoinStart() || (p.isKw("CROSS") && (p.peekIsKw(1, "JOIN") || p.peekIsKw(1, "APPLY"))) ||
		(p.isKw("OUTER") && p.peekIsKw(1, "APPLY")) {
		joined, ok := p.tryParseJoinPart(item)
		if !ok {
			break
		}
		item = joined
	}
	return item
}

// parseValuesRows parses `(e, …) [, (e, …)]*` after VALUES.
func (p *Parser) parseValuesRows() [][]ast.Expr {
	var rows [][]ast.Expr
	for {
		if !p.expectPunct("(") {
			return rows
		}
		var row []ast.Expr
		if !p.isPunct(")") {
			row = append(row, p.parseExpr())
			for p.isPunct(",") {
				p.advance()
				row = append(row, p.parseExpr())
			}
		}
		p.expectPunct(")")
		rows = append(rows, row)
		if !p.isPunct(",") {
			return rows
		}
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// INSERT / UPDATE / DELETE / MERGE
// ---------------------------------------------------------------------------

// parseOutputClause parses `OUTPUT items [INTO target [(cols)]]` (possibly
// twice: OUTPUT … INTO … OUTPUT …) and returns its raw text and its
// typed clauses.
func (p *Parser) parseOutputClause() (string, []ast.OutputClause) {
	start := p.cur
	var out []ast.OutputClause
	for p.isKw("OUTPUT") {
		p.advance()
		var oc ast.OutputClause
		oc.Items, _ = p.parseSelectElements()
		if p.isKw("INTO") {
			p.advance()
			ref := p.parseTableRef()
			oc.Into = &ref
			if p.isPunct("(") {
				oc.IntoCols = p.parseIdentList()
			}
		}
		out = append(out, oc)
	}
	return p.textFrom(start), out
}

// parseDMLTop parses `TOP (n) [PERCENT]` after INSERT / UPDATE / DELETE /
// MERGE. PERCENT is folded into the returned expression's caller.
func (p *Parser) parseDMLTop() ast.Expr {
	if !p.isKw("TOP") {
		return nil
	}
	p.advance()
	e := p.parseTopValue()
	if p.isKw("PERCENT") {
		p.errorHere("TOP … PERCENT in a data-modification statement is not supported", "")
		p.advance()
	}
	return e
}

// parseDMLTarget parses the target of INSERT / UPDATE / DELETE / MERGE: a
// (qualified) table, a @table variable or an alias, with optional table
// hints.
func (p *Parser) parseDMLTarget() ast.TableRef {
	ref := p.parseTableRef()
	p.skipTableHints()
	return ref
}

// parseInsertStatement parses
//
//	INSERT [TOP (n)] [INTO] target [WITH (hints)] [(cols)] [OUTPUT …]
//	{ VALUES (…)[, (…)] | select | EXEC … | DEFAULT VALUES }
func (p *Parser) parseInsertStatement(with *ast.WithClause) *ast.InsertStmt {
	start := p.cur.Pos
	p.expectKw("INSERT")
	stmt := &ast.InsertStmt{With: with, P: astPos(start)}
	stmt.Top = p.parseDMLTop()
	if p.isKw("INTO") {
		p.advance()
	}
	stmt.Table = p.parseDMLTarget()
	if p.isPunct("(") && !p.startsQuery() {
		stmt.Cols = p.parseIdentList()
	}
	if p.isKw("OUTPUT") {
		stmt.Output, stmt.Outputs = p.parseOutputClause()
	}
	switch {
	case p.isKw("VALUES"):
		p.advance()
		stmt.Values = p.parseValuesRows()
	case p.isKw("DEFAULT") && p.peekIsKw(1, "VALUES"):
		p.advance()
		p.advance()
		stmt.DefaultValues = true
	case p.isKw("EXEC") || p.isKw("EXECUTE"):
		stmt.Exec = p.parseExec()
	case p.startsQuery() || p.isKw("WITH"):
		stmt.Select = p.parseSelectStatement()
	default:
		p.errorHere("expected VALUES, SELECT, EXEC or DEFAULT VALUES", "VALUES|SELECT|EXEC|DEFAULT VALUES")
	}
	return stmt
}

// parseSetItems parses the SET list of UPDATE (and MERGE … UPDATE SET).
func (p *Parser) parseSetItems() []ast.Assign {
	var out []ast.Assign
	for {
		out = append(out, p.parseSetItem())
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseSetItem parses one assignment of an UPDATE SET list:
//
//	col = expr | col op= expr | [qual.]col = expr | @v = expr |
//	@v = col = expr | col.method(…) (xml modify(), CLR mutators)
func (p *Parser) parseSetItem() ast.Assign {
	if p.cur.Kind == TOK_VARIABLE {
		v := p.cur
		p.advance()
		op := p.cur.Lit
		if !p.isPunct("=") && compoundOps[op] == "" {
			p.errorHere("expected '=' after variable", "'='")
			return ast.Assign{Col: v.Lit}
		}
		p.advance()
		// @v = col = expr assigns both.
		if p.isIdentTok() && p.peekIsPunct(1, "=") {
			col, _ := p.parseIdent()
			p.advance()
			e := p.parseExpr()
			_ = col
			return ast.Assign{Col: v.Lit, Expr: e}
		}
		e := p.parseExpr()
		if bop := compoundOps[op]; bop != "" {
			e = &ast.BinaryExpr{Op: bop, Lhs: &ast.Ident{Parts: []string{v.Lit}, Variable: true, P: astPos(v.Pos)}, Rhs: e, P: astPos(v.Pos)}
		}
		return ast.Assign{Col: v.Lit, Expr: e}
	}
	start := p.cur
	parts, quoted := p.parseMultiPartName()
	if p.isPunct("(") && len(parts) >= 2 {
		// col.modify(N'…') / col.WRITE(…) / col.SetXY(…): a mutator call on
		// the column, kept as the column's new value.
		target := &ast.Ident{Parts: parts[:len(parts)-1], Backtick: quoted, P: astPos(start.Pos)}
		e := p.finishMethodCall(start, target, parts[len(parts)-1])
		return ast.Assign{Col: parts[len(parts)-2], ColQualifier: parts[:len(parts)-2], Expr: e}
	}
	a := ast.Assign{Col: parts[len(parts)-1]}
	if len(parts) > 1 {
		a.ColQualifier = parts[:len(parts)-1]
	}
	op := p.cur.Lit
	if p.isPunct("=") || (p.cur.Kind == TOK_PUNCT && compoundOps[op] != "") {
		p.advance()
	} else {
		p.errorHere("expected '=' in SET", "'='")
		return a
	}
	a.Expr = p.parseExpr()
	if bop := compoundOps[op]; bop != "" {
		a.Expr = &ast.BinaryExpr{Op: bop, Lhs: &ast.Ident{Parts: parts, Backtick: quoted, P: astPos(start.Pos)}, Rhs: a.Expr, P: astPos(start.Pos)}
	}
	return a
}

// parseWhereClause parses `WHERE cond` or `WHERE CURRENT OF [GLOBAL]
// cursor` (kept as a RawExpr of Kind "CURRENT_OF" holding the cursor
// name).
func (p *Parser) parseWhereClause() ast.Expr {
	p.expectKw("WHERE")
	if p.isKw("CURRENT") && p.peekIsKw(1, "OF") {
		start := p.cur.Pos
		p.advance()
		p.advance()
		if p.isKw("GLOBAL") {
			p.advance()
		}
		name := p.cur.Lit
		if p.cur.Kind == TOK_VARIABLE {
			p.advance()
		} else {
			name, _ = p.parseIdent()
		}
		return &ast.RawExpr{Kind: "CURRENT_OF", Text: name, P: astPos(start)}
	}
	return p.parseExpr()
}

// parseUpdateStatement parses
//
//	UPDATE [TOP (n)] target [WITH (hints)] SET … [OUTPUT …]
//	[FROM sources] [WHERE …] [OPTION (…)]
func (p *Parser) parseUpdateStatement(with *ast.WithClause) *ast.UpdateStmt {
	start := p.cur.Pos
	p.expectKw("UPDATE")
	stmt := &ast.UpdateStmt{With: with, P: astPos(start)}
	stmt.Top = p.parseDMLTop()
	stmt.Table = p.parseDMLTarget()
	p.expectKw("SET")
	stmt.Sets = p.parseSetItems()
	if p.isKw("OUTPUT") {
		stmt.Output, stmt.Outputs = p.parseOutputClause()
	}
	if p.isKw("FROM") {
		p.advance()
		stmt.From = p.parseTableSources()
	}
	if p.isKw("WHERE") {
		stmt.Where = p.parseWhereClause()
	}
	if p.isKw("OPTION") && p.peekIsPunct(1, "(") {
		stmt.OptionClause = p.parseOptionClause()
	}
	return stmt
}

// parseDeleteStatement parses
//
//	DELETE [TOP (n)] [FROM] target [WITH (hints)] [OUTPUT …]
//	[FROM sources] [WHERE …] [OPTION (…)]
func (p *Parser) parseDeleteStatement(with *ast.WithClause) *ast.DeleteStmt {
	start := p.cur.Pos
	p.expectKw("DELETE")
	stmt := &ast.DeleteStmt{With: with, P: astPos(start)}
	stmt.Top = p.parseDMLTop()
	if p.isKw("FROM") {
		p.advance()
	}
	stmt.Table = p.parseDMLTarget()
	if p.isKw("OUTPUT") {
		stmt.Output, stmt.Outputs = p.parseOutputClause()
	}
	if p.isKw("FROM") {
		p.advance()
		stmt.Using = p.parseTableSources()
	}
	if p.isKw("WHERE") {
		stmt.Where = p.parseWhereClause()
	}
	if p.isKw("OPTION") && p.peekIsPunct(1, "(") {
		stmt.OptionClause = p.parseOptionClause()
	}
	return stmt
}

// parseMergeStatement parses
//
//	MERGE [TOP (n)] [INTO] target [WITH (hints)] [[AS] alias]
//	USING source ON cond
//	{ WHEN MATCHED [AND c] THEN {UPDATE SET … | DELETE}
//	| WHEN NOT MATCHED [BY TARGET] [AND c] THEN INSERT [(cols)] {VALUES (…) | DEFAULT VALUES}
//	| WHEN NOT MATCHED BY SOURCE [AND c] THEN {UPDATE SET … | DELETE} }…
//	[OUTPUT …] [OPTION (…)]
func (p *Parser) parseMergeStatement(with *ast.WithClause) *ast.MergeStmt {
	start := p.cur.Pos
	p.expectKw("MERGE")
	stmt := &ast.MergeStmt{With: with, P: astPos(start)}
	stmt.Top = p.parseDMLTop()
	if p.isKw("INTO") {
		p.advance()
	}
	stmt.Target = p.parseDMLTarget()
	if !p.isKw("USING") {
		stmt.TargetAlias = p.parseOptionalAlias(false)
	}
	p.expectKw("USING")
	stmt.Source = p.parseTableSource()
	p.expectKw("ON")
	stmt.On = p.parseExpr()
	for p.isKw("WHEN") {
		p.advance()
		bySource := false
		matched := true
		if p.isKw("NOT") {
			p.advance()
			matched = false
		}
		p.expectKw("MATCHED")
		if !matched && p.isKw("BY") {
			p.advance()
			if p.isKw("SOURCE") {
				bySource = true
				p.advance()
			} else {
				p.expectKw("TARGET")
			}
		}
		act := ast.MergeAction{}
		if p.isKw("AND") {
			p.advance()
			act.Cond = p.parseExpr()
		}
		p.expectKw("THEN")
		switch {
		case p.isKw("UPDATE"):
			p.advance()
			p.expectKw("SET")
			act.Kind = "UPDATE"
			act.Sets = p.parseSetItems()
		case p.isKw("DELETE"):
			p.advance()
			act.Kind = "DELETE"
		case p.isKw("INSERT"):
			p.advance()
			act.Kind = "INSERT"
			if p.isPunct("(") {
				act.InsertCols = p.parseIdentList()
			}
			if p.isKw("DEFAULT") && p.peekIsKw(1, "VALUES") {
				p.advance()
				p.advance()
			} else {
				p.expectKw("VALUES")
				rows := p.parseValuesRows()
				if len(rows) > 0 {
					act.InsertValues = rows[0]
				}
			}
		default:
			p.errorHere("expected UPDATE, DELETE or INSERT", "UPDATE|DELETE|INSERT")
		}
		switch {
		case matched:
			stmt.WhenMatched = append(stmt.WhenMatched, act)
		case bySource:
			stmt.WhenNotMatchedBySource = append(stmt.WhenNotMatchedBySource, act)
		default:
			stmt.WhenNotMatched = append(stmt.WhenNotMatched, act)
		}
	}
	if p.isKw("OUTPUT") {
		stmt.Output, stmt.Outputs = p.parseOutputClause()
	}
	if p.isKw("OPTION") && p.peekIsPunct(1, "(") {
		p.parseOptionClause()
	}
	return stmt
}
