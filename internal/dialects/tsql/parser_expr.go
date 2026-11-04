package tsql

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// Expressions (grammar rules expression, search_condition, predicate,
// primitive_expression, function_call, case_expression, over_clause …).
//
// Precedence, loosest first: OR; AND; NOT; predicates (comparison,
// [NOT] BETWEEN / IN / LIKE [ESCAPE], IS [NOT] NULL); additive and bitwise
// (+ - & ^ |); multiplicative (* / %); unary (- + ~); postfix (COLLATE,
// AT TIME ZONE, CLR / XML method calls). T-SQL does not distinguish
// boolean and value expressions syntactically, so one entry point serves
// both.
// ---------------------------------------------------------------------------

func (p *Parser) parseExpr() ast.Expr { return p.parseOr() }

func (p *Parser) parseOr() ast.Expr {
	lhs := p.parseAnd()
	for p.isKw("OR") {
		p.advance()
		rhs := p.parseAnd()
		lhs = &ast.BinaryExpr{Op: "OR", Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseAnd() ast.Expr {
	lhs := p.parseNot()
	for p.isKw("AND") {
		p.advance()
		rhs := p.parseNot()
		lhs = &ast.BinaryExpr{Op: "AND", Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseNot() ast.Expr {
	if p.isKw("NOT") {
		start := p.cur.Pos
		p.advance()
		rhs := p.parseNot()
		return &ast.UnaryExpr{Op: "NOT", Rhs: rhs, P: astPos(start)}
	}
	return p.parsePredicate()
}

// comparisonOps maps the T-SQL comparison tokens to the operator kept on
// the node. `!<` / `!>` are spelled with their exact equivalents so
// translators only meet standard operators.
var comparisonOps = map[string]string{
	"=": "=", "<>": "<>", "!=": "!=", "<": "<", "<=": "<=", ">": ">", ">=": ">=",
	"!<": ">=", "!>": "<=",
}

func (p *Parser) parsePredicate() ast.Expr {
	lhs := p.parseAdd()
	for {
		not := false
		save := p.i
		start := p.cur.Pos
		if p.isKw("NOT") && (p.peekIsKw(1, "BETWEEN") || p.peekIsKw(1, "IN") || p.peekIsKw(1, "LIKE")) {
			not = true
			p.advance()
		}
		switch {
		case p.isKw("BETWEEN"):
			p.advance()
			low := p.parseAdd()
			p.expectKw("AND")
			high := p.parseAdd()
			lhs = &ast.BetweenExpr{Expr: lhs, Low: low, High: high, Not: not, P: astPos(start)}
			continue
		case p.isKw("IN"):
			p.advance()
			lhs = p.parseInRest(lhs, not, start)
			continue
		case p.isKw("LIKE"):
			p.advance()
			op := "LIKE"
			if not {
				op = "NOT LIKE"
			}
			rhs := p.parseAdd()
			be := &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
			if p.isKw("ESCAPE") {
				p.advance()
				be.Escape = p.parseAdd()
			}
			lhs = be
			continue
		}
		if not {
			p.i = save
			p.cur = p.toks[save]
		}
		if p.isKw("IS") {
			p.advance()
			op := "IS"
			if p.isKw("NOT") {
				p.advance()
				op = "IS NOT"
			}
			rstart := p.cur.Pos
			if !p.expectKw("NULL") {
				return lhs
			}
			lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: &ast.Literal{Kind: "null", Text: "NULL", P: astPos(rstart)}, P: lhs.Pos()}
			continue
		}
		if p.cur.Kind == TOK_PUNCT {
			if op, ok := comparisonOps[p.cur.Lit]; ok {
				p.advance()
				var rhs ast.Expr
				if (p.isKw("ALL") || p.isKw("ANY") || p.isKw("SOME")) && p.peekIsPunct(1, "(") {
					qstart := p.cur.Pos
					name := strings.ToUpper(p.cur.Lit)
					p.advance()
					p.advance() // (
					sub := p.parseSelectStatement()
					p.expectPunct(")")
					rhs = &ast.FuncCall{Name: name, Args: []ast.Expr{&ast.SubqueryExpr{Stmt: sub, P: astPos(qstart)}}, P: astPos(qstart)}
				} else {
					rhs = p.parseAdd()
				}
				lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
				continue
			}
		}
		return lhs
	}
}

// parseInRest parses the right-hand side of `[NOT] IN`: a parenthesised
// expression list or a subquery.
func (p *Parser) parseInRest(lhs ast.Expr, not bool, start Position) ast.Expr {
	in := &ast.InExpr{Expr: lhs, Not: not, P: astPos(start)}
	if !p.expectPunct("(") {
		return in
	}
	if p.startsQuery() {
		in.Subquery = p.parseSelectStatement()
	} else if !p.isPunct(")") {
		in.List = append(in.List, p.parseExpr())
		for p.isPunct(",") {
			p.advance()
			in.List = append(in.List, p.parseExpr())
		}
	}
	p.expectPunct(")")
	return in
}

// startsQuery reports whether the current token starts a query expression
// (SELECT, WITH …, or parentheses around one).
func (p *Parser) startsQuery() bool {
	j := p.i
	for j < len(p.toks) && p.toks[j].Kind == TOK_PUNCT && p.toks[j].Lit == "(" {
		j++
	}
	if j >= len(p.toks) {
		return false
	}
	t := p.toks[j]
	return tokIsKw(t, "SELECT") || (tokIsKw(t, "WITH") && j == p.i)
}

func (p *Parser) parseAdd() ast.Expr {
	lhs := p.parseMul()
	for p.isPunct("+") || p.isPunct("-") || p.isPunct("&") || p.isPunct("^") || p.isPunct("|") || p.isPunct("||") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseMul()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseMul() ast.Expr {
	lhs := p.parseUnary()
	for p.isPunct("*") || p.isPunct("/") || p.isPunct("%") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseUnary()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseUnary() ast.Expr {
	if p.isPunct("-") || p.isPunct("+") || p.isPunct("~") {
		start := p.cur.Pos
		op := p.cur.Lit
		p.advance()
		rhs := p.parseUnary()
		return &ast.UnaryExpr{Op: op, Rhs: rhs, P: astPos(start)}
	}
	return p.parsePostfix()
}

// parsePostfix parses a primary followed by its postfix operators:
// `COLLATE name`, `AT TIME ZONE expr` and method calls on a computed value
// (`CAST(x AS xml).value(…)`, `f(x).ToString()`).
func (p *Parser) parsePostfix() ast.Expr {
	start := p.cur
	e := p.parsePrimary()
	for {
		switch {
		case p.isKw("COLLATE"):
			p.advance()
			name, _ := p.parseIdent()
			e = &ast.CollateExpr{Expr: e, Collation: name, P: e.Pos()}
		case p.isKw("AT") && p.peekIsKw(1, "TIME") && p.peekIsKw(2, "ZONE"):
			p.advance()
			p.advance()
			p.advance()
			zone := p.parsePrimary()
			e = &ast.BinaryExpr{Op: "AT TIME ZONE", Lhs: e, Rhs: zone, P: e.Pos()}
		case p.isPunct(".") && p.peek(1).Kind != TOK_PUNCT && p.peekIsPunct(2, "(") && isMethodTarget(e):
			p.advance() // .
			method, _ := p.parseIdentAny()
			e = p.finishMethodCall(start, e, method)
		default:
			return e
		}
	}
}

// isMethodTarget reports whether e is a computed value a method call can
// be chained on directly (identifier chains carry their method in the
// chain itself, see parseIdentChain).
func isMethodTarget(e ast.Expr) bool {
	switch e.(type) {
	case *ast.FuncCall, *ast.CastExpr, *ast.ParenExpr, *ast.MethodCall, *ast.SubqueryExpr, *ast.Ident:
		return true
	}
	return false
}

// xmlMethods are the xml data-type methods. Their XQuery argument is
// parsed into typed nodes (ParseXQuery) by finishMethodCall.
var xmlMethods = map[string]bool{"value": true, "query": true, "exist": true, "nodes": true, "modify": true}

// clrMethods are the hierarchyid / geography / geometry CLR methods
// recognised on a column or variable (`OrganizationNode.GetAncestor(1)`).
// A two-part call whose last part is not listed is a schema-qualified
// function call (`dbo.ufnGetStock(@id)`).
var clrMethods = map[string]bool{
	"getancestor": true, "getdescendant": true, "getlevel": true, "getroot": true,
	"isdescendantof": true, "getreparentedvalue": true, "tostring": true,
	"read": true, "write": true, "parse": true,
	"stastext": true, "stasbinary": true, "stastext_": true, "stdistance": true,
	"stintersects": true, "stcontains": true, "stbuffer": true, "starea": true,
	"stlength": true, "stsrid": true, "stgeometrytype": true, "stnumpoints": true,
	"stpointn": true, "stequals": true, "stwithin": true, "stunion": true,
	"stintersection": true, "stx": true, "sty": true, "stisvalid": true,
	"makevalid": true, "reduce": true, "lat": true, "long": true,
	"stgeomfromtext": true, "stpointfromtext": true, "point": true,
}

// finishMethodCall parses the parenthesised arguments of `target.method(…)`
// (the current token is '(') and returns the MethodCall. For an xml
// method whose XQuery argument is a string literal, the XQuery is parsed
// (MethodCall.XQuery, or XQueryErr when outside the supported subset;
// modify()'s XML DML is never parsed), and the SQL type literal of
// value() is parsed as a T-SQL data type (MethodCall.ValueType).
func (p *Parser) finishMethodCall(start Token, target ast.Expr, method string) ast.Expr {
	mc := &ast.MethodCall{Target: target, Method: method, P: astPos(start.Pos)}
	mc.Args = p.parseCallArgs()
	lower := strings.ToLower(method)
	if !xmlMethods[lower] {
		return mc
	}
	if lower == "modify" {
		mc.XQueryErr = "XML DML (modify()) has no PostgreSQL counterpart"
		return mc
	}
	if len(mc.Args) == 0 {
		mc.XQueryErr = "missing XQuery argument"
		return mc
	}
	lit, ok := mc.Args[0].(*ast.Literal)
	if !ok || lit.Kind != "string" {
		mc.XQueryErr = "the XQuery argument is not a string literal"
		return mc
	}
	q, err := ParseXQuery(lit.Text)
	if err != nil {
		mc.XQueryErr = err.Error()
	} else {
		mc.XQuery = q
	}
	if lower == "value" && len(mc.Args) == 2 {
		if tl, ok := mc.Args[1].(*ast.Literal); ok && tl.Kind == "string" {
			mc.ValueType = parseTypeLiteral(tl.Text)
		}
	}
	return mc
}

// parseTypeLiteral parses the SQL type named by the second argument of
// xml.value() (`'nvarchar(30)'`, `'decimal(9, 4)'`): the literal's
// content is T-SQL type syntax, lexed and parsed like a declaration's
// type. nil when it is not exactly one data type.
func parseTypeLiteral(src string) ast.DataType {
	sp := newParser(src)
	if len(sp.errs) > 0 {
		return nil
	}
	dt := sp.parseDataType()
	if dt == nil || len(sp.errs) > 0 || sp.cur.Kind != TOK_EOF {
		return nil
	}
	return dt
}

// parseCallArgs parses `( [arg, …] )` with plain expression arguments.
func (p *Parser) parseCallArgs() []ast.Expr {
	var args []ast.Expr
	if !p.expectPunct("(") {
		return nil
	}
	if !p.isPunct(")") {
		args = append(args, p.parseFuncArg())
		for p.isPunct(",") {
			p.advance()
			args = append(args, p.parseFuncArg())
		}
	}
	p.expectPunct(")")
	return args
}

// keywordFuncs are reserved words that are also function names when
// followed by '('.
var keywordFuncs = map[string]bool{
	"COALESCE": true, "NULLIF": true, "LEFT": true, "RIGHT": true, "UPDATE": true,
	"IDENTITY": true, "CONTAINS": true, "FREETEXT": true, "OPENQUERY": true,
	"OPENROWSET": true, "OPENDATASOURCE": true, "OPENXML": true,
	"CONTAINSTABLE": true, "FREETEXTTABLE": true, "USER": true, "SESSION_USER": true,
	"SYSTEM_USER": true, "CURRENT_USER": true,
}

// niladicKeywords are reserved words that are value expressions on their
// own (no parentheses in T-SQL).
var niladicKeywords = map[string]bool{
	"CURRENT_TIMESTAMP": true, "CURRENT_USER": true, "SESSION_USER": true,
	"SYSTEM_USER": true, "USER": true, "CURRENT_DATE": true, "CURRENT_TIME": true,
}

func (p *Parser) parsePrimary() ast.Expr {
	start := p.cur
	pos := astPos(start.Pos)
	switch p.cur.Kind {
	case TOK_NUMBER:
		p.advance()
		return &ast.Literal{Kind: "number", Text: start.Lit, P: pos}
	case TOK_STRING:
		p.advance()
		return &ast.Literal{Kind: "string", Text: start.Lit, NString: start.NString, P: pos}
	case TOK_HEX:
		p.advance()
		return &ast.Literal{Kind: "hex", Text: start.Lit, P: pos}
	case TOK_MONEY:
		p.advance()
		return &ast.Literal{Kind: "money", Text: start.Lit, P: pos}
	case TOK_VARIABLE:
		p.advance()
		v := &ast.Ident{Parts: []string{start.Lit}, Variable: true, P: pos}
		if p.isPunct(".") && p.peek(1).Kind != TOK_PUNCT {
			if p.peekIsPunct(2, "(") {
				p.advance()
				method, _ := p.parseIdentAny()
				return p.finishMethodCall(start, v, method)
			}
			// Property of a CLR value (@geo.Lat).
			p.advance()
			prop, _ := p.parseIdentAny()
			return &ast.MethodCall{Target: v, Method: prop, P: pos}
		}
		return v
	case TOK_SQLCMD_VAR:
		p.advance()
		return &ast.Ident{Parts: []string{"$(" + start.Lit + ")"}, P: pos}
	case TOK_PUNCT:
		if p.isPunct("(") {
			if p.startsQuery() {
				p.advance()
				sub := p.parseSelectStatement()
				p.expectPunct(")")
				return &ast.SubqueryExpr{Stmt: sub, P: pos}
			}
			p.advance()
			inner := p.parseExpr()
			p.expectPunct(")")
			return &ast.ParenExpr{Inner: inner, P: pos}
		}
	case TOK_KEYWORD:
		switch p.cur.Lit {
		case "NULL":
			p.advance()
			return &ast.Literal{Kind: "null", Text: "NULL", P: pos}
		case "DEFAULT":
			p.advance()
			return &ast.Literal{Kind: "default", Text: "DEFAULT", P: pos}
		case "CASE":
			return p.parseCaseExpr()
		case "CONVERT", "TRY_CONVERT":
			return p.parseConvert()
		case "EXISTS":
			p.advance()
			p.expectPunct("(")
			sub := p.parseSelectStatement()
			p.expectPunct(")")
			return &ast.ExistsExpr{Subquery: sub, P: pos}
		}
		if keywordFuncs[p.cur.Lit] && p.peekIsPunct(1, "(") {
			name := p.cur.Lit
			p.advance()
			return p.finishFuncCall(start, name)
		}
		if niladicKeywords[p.cur.Lit] {
			p.advance()
			return &ast.FuncCall{Name: start.Lit, P: pos}
		}
	case TOK_IDENT:
		switch {
		case (p.isKw("CAST") || p.isKw("TRY_CAST")) && p.peekIsPunct(1, "("):
			return p.parseCast()
		case p.isKw("NEXT") && p.peekIsKw(1, "VALUE") && p.peekIsKw(2, "FOR"):
			return p.parseNextValueFor()
		}
		return p.parseIdentChain()
	case TOK_QUOTED_IDENT:
		return p.parseIdentChain()
	}
	p.errorHere("unexpected token in expression", "expression")
	// Never step over a statement boundary: the caller's resynchronisation
	// must still find it, otherwise a malformed expression swallows the
	// following statement.
	if !p.isPunct(";") && p.cur.Kind != TOK_GO && p.cur.Kind != TOK_EOF && !p.isStmtStartKeyword() {
		p.advance()
	}
	return &ast.RawExpr{Text: "", P: pos}
}

// parseIdentChain parses a (multi-part) identifier and what may follow it:
// a function call (with OVER / WITHIN GROUP), a static CLR call
// (`hierarchyid::GetRoot()`), a CLR / XML method on a column
// (`e.[OrganizationNode].GetAncestor(1)`, `[Resume].value(…)`), or a plain
// column reference.
func (p *Parser) parseIdentChain() ast.Expr {
	start := p.cur
	pos := astPos(start.Pos)
	parts, quoted := p.parseMultiPartName()
	if len(parts) == 1 && p.isPunct("::") {
		p.advance()
		method, _ := p.parseIdentAny()
		mc := &ast.MethodCall{StaticType: parts[0], Method: method, P: pos}
		if p.isPunct("(") {
			mc.Args = p.parseCallArgs()
		}
		return mc
	}
	if p.isPunct("(") {
		if len(parts) >= 2 {
			last := strings.ToLower(parts[len(parts)-1])
			if xmlMethods[last] || clrMethods[last] {
				target := &ast.Ident{Parts: parts[:len(parts)-1], Backtick: quoted, P: pos}
				return p.finishMethodCall(start, target, parts[len(parts)-1])
			}
		}
		name := strings.Join(parts, ".")
		if len(parts) == 1 && !quoted {
			name = strings.ToUpper(name)
		}
		return p.finishFuncCall(start, name)
	}
	return &ast.Ident{Parts: parts, Backtick: quoted, P: pos}
}

// finishFuncCall parses the argument list of a function call (the current
// token is '(') plus its optional WITHIN GROUP and OVER clauses.
func (p *Parser) finishFuncCall(start Token, name string) ast.Expr {
	pos := astPos(start.Pos)
	p.expectPunct("(")
	fc := &ast.FuncCall{Name: name, P: pos}
	if p.isKw("DISTINCT") {
		fc.Distinct = true
		p.advance()
	} else if p.isKw("ALL") {
		p.advance()
	}
	if !p.isPunct(")") {
		fc.Args = append(fc.Args, p.parseFuncArg())
		for p.isPunct(",") {
			p.advance()
			fc.Args = append(fc.Args, p.parseFuncArg())
		}
	}
	p.expectPunct(")")
	var out ast.Expr = fc
	var within []ast.OrderItem
	if p.isKw("WITHIN") && p.peekIsKw(1, "GROUP") {
		p.advance()
		p.advance()
		p.expectPunct("(")
		p.expectKw("ORDER")
		p.expectKw("BY")
		within = p.parseOrderByClause()
		p.expectPunct(")")
	}
	if p.isKw("OVER") {
		p.advance()
		spec := p.parseOverClause()
		return &ast.WindowedAgg{Func: fc, Within: within, Over: spec, P: pos}
	}
	if within != nil {
		out = &ast.WindowedAgg{Func: fc, Within: within, P: pos}
	}
	return out
}

// parseFuncArg parses one function argument: `*` (COUNT(*), full-text
// table functions, kept as the identifier "*"), `LANGUAGE expr` (full-text
// functions, a *NamedArg) or an expression.
func (p *Parser) parseFuncArg() ast.Expr {
	if p.isPunct("*") {
		start := p.cur.Pos
		p.advance()
		return &ast.Ident{Parts: []string{"*"}, P: astPos(start)}
	}
	if p.isKw("LANGUAGE") && p.peek(1).Kind != TOK_PUNCT {
		start := p.cur.Pos
		p.advance()
		return &ast.NamedArg{Name: "LANGUAGE", Value: p.parseExpr(), P: astPos(start)}
	}
	return p.parseExpr()
}

// parseOverClause parses the window specification after OVER:
// `( [PARTITION BY e, …] [ORDER BY …] [ROWS | RANGE frame] )`. The frame
// is kept as its token text in WindowSpec.Frame.
func (p *Parser) parseOverClause() *ast.WindowSpec {
	spec := &ast.WindowSpec{}
	if !p.expectPunct("(") {
		return spec
	}
	if p.isKw("PARTITION") && p.peekIsKw(1, "BY") {
		p.advance()
		p.advance()
		spec.PartitionBy = append(spec.PartitionBy, p.parseExpr())
		for p.isPunct(",") {
			p.advance()
			spec.PartitionBy = append(spec.PartitionBy, p.parseExpr())
		}
	}
	if p.isKw("ORDER") {
		p.advance()
		p.expectKw("BY")
		spec.OrderBy = p.parseOrderByClause()
	}
	if p.isKw("ROWS") || p.isKw("RANGE") {
		var parts []string
		for p.cur.Kind != TOK_EOF && !p.isPunct(")") {
			parts = append(parts, strings.ToUpper(p.cur.Raw))
			p.advance()
		}
		spec.Frame = strings.Join(parts, " ")
	}
	p.expectPunct(")")
	return spec
}

// parseCaseExpr parses the simple and the searched CASE forms.
func (p *Parser) parseCaseExpr() ast.Expr {
	start := p.cur.Pos
	p.expectKw("CASE")
	out := &ast.CaseExpr{P: astPos(start)}
	if !p.isKw("WHEN") {
		out.Operand = p.parseExpr()
	}
	for p.isKw("WHEN") {
		p.advance()
		w := ast.CaseExprWhen{Match: p.parseExpr()}
		p.expectKw("THEN")
		w.Then = p.parseExpr()
		out.Whens = append(out.Whens, w)
	}
	if len(out.Whens) == 0 {
		p.errorHere("expected WHEN", "WHEN")
	}
	if p.isKw("ELSE") {
		p.advance()
		out.Else = p.parseExpr()
	}
	p.expectKw("END")
	return out
}

// parseCast parses `CAST(expr AS type)` / `TRY_CAST(expr AS type)`.
func (p *Parser) parseCast() ast.Expr {
	start := p.cur.Pos
	try := p.isKw("TRY_CAST")
	p.advance()
	p.expectPunct("(")
	inner := p.parseExpr()
	p.expectKw("AS")
	dt := p.parseDataType()
	p.expectPunct(")")
	return &ast.CastExpr{Expr: inner, Type: dt, Try: try, P: astPos(start)}
}

// parseConvert parses `CONVERT(type, expr [, style])` /
// `TRY_CONVERT(…)` into a CastExpr with IsConvert set.
func (p *Parser) parseConvert() ast.Expr {
	start := p.cur.Pos
	try := p.isKw("TRY_CONVERT")
	p.advance()
	p.expectPunct("(")
	dt := p.parseDataType()
	p.expectPunct(",")
	inner := p.parseExpr()
	ce := &ast.CastExpr{Expr: inner, Type: dt, IsConvert: true, Try: try, P: astPos(start)}
	if p.isPunct(",") {
		p.advance()
		ce.ConvertStyle = p.parseExpr()
	}
	p.expectPunct(")")
	return ce
}

// parseNextValueFor parses `NEXT VALUE FOR [schema.]seq [OVER (ORDER BY …)]`.
func (p *Parser) parseNextValueFor() ast.Expr {
	start := p.cur.Pos
	p.advance() // NEXT
	p.advance() // VALUE
	p.advance() // FOR
	ref := p.parseTableRef()
	if p.isKw("OVER") {
		p.advance()
		p.parseOverClause()
	}
	return &ast.SequenceRef{Schema: ref.Schema, Name: ref.Name, Op: "NEXTVAL", P: astPos(start)}
}

// ---------------------------------------------------------------------------
// Data types (grammar rule data_type)
// ---------------------------------------------------------------------------

// parseDataType parses a T-SQL data type: a built-in (`int`, `[nvarchar](50)`,
// `"datetime"`, `varchar(max)`, `decimal(8, 2)`, `xml(CONTENT s.c)`,
// `double precision`, `national character varying(n)`, …) or a user-defined
// alias type (`[Name]`, `dbo.Flag`).
func (p *Parser) parseDataType() ast.DataType {
	start := p.cur.Pos
	pos := astPos(start)
	if p.isKw("DOUBLE") && p.peekIsKw(1, "PRECISION") {
		p.advance()
		p.advance()
		return &ast.FloatType{Name: "FLOAT", Precision: 53, HasPS: true, P: pos}
	}
	if p.isKw("CURSOR") {
		p.advance()
		return &ast.TSQLType{Name: "CURSOR", P: pos}
	}
	national := false
	if p.isKw("NATIONAL") {
		national = true
		p.advance()
	}
	if !p.isIdentTok() {
		p.errorHere("expected data type", "data type")
		if p.cur.Kind == TOK_KEYWORD && !p.isStmtStartKeyword() {
			p.advance()
		}
		return &ast.UserDefinedType{P: pos}
	}
	parts, _ := p.parseMultiPartName()
	name := parts[len(parts)-1]
	if len(parts) > 1 && !strings.EqualFold(parts[len(parts)-2], "sys") {
		return &ast.UserDefinedType{Schema: parts[len(parts)-2], Name: name, P: pos}
	}
	lower := strings.ToLower(name)
	// Two-word spellings: CHAR VARYING, CHARACTER VARYING, BINARY VARYING.
	if (lower == "char" || lower == "character" || lower == "binary") && p.isKw("VARYING") {
		p.advance()
		lower = "var" + lower
		if lower == "varcharacter" {
			lower = "varchar"
		}
	}
	if lower == "character" {
		lower = "char"
	}
	if national {
		switch lower {
		case "char":
			lower = "nchar"
		case "varchar":
			lower = "nvarchar"
		case "text":
			lower = "ntext"
		}
	}
	switch lower {
	case "bigint", "int", "integer", "smallint", "tinyint":
		n := strings.ToUpper(lower)
		if n == "INTEGER" {
			n = "INT"
		}
		return &ast.IntType{Name: n, P: pos}
	case "decimal", "numeric", "dec":
		n := "DECIMAL"
		if lower == "numeric" {
			n = "NUMERIC"
		}
		t := &ast.DecimalType{Name: n, Precision: 18, Scale: 0, P: pos}
		if args, ok := p.parseTypeArgs(); ok && len(args) > 0 {
			t.HasPrec = true
			t.Precision = args[0]
			if len(args) > 1 {
				t.Scale = args[1]
			}
		}
		return t
	case "float":
		t := &ast.FloatType{Name: "FLOAT", Precision: 53, P: pos}
		if args, ok := p.parseTypeArgs(); ok && len(args) > 0 {
			t.Precision = args[0]
			t.HasPS = true
		}
		return t
	case "real":
		return &ast.FloatType{Name: "REAL", Precision: 24, P: pos}
	case "date":
		return &ast.DateType{P: pos}
	case "time":
		// T-SQL's default fractional-seconds precision is 7.
		t := &ast.TimeType{Fsp: 7, P: pos}
		if args, ok := p.parseTypeArgs(); ok && len(args) > 0 {
			t.Fsp = args[0]
		}
		return t
	case "datetime2", "datetimeoffset":
		t := &ast.TSQLType{Name: strings.ToUpper(lower), Precision: 7, P: pos}
		if args, ok := p.parseTypeArgs(); ok && len(args) > 0 {
			t.Precision = args[0]
			t.HasPrecision = true
		}
		return t
	case "char", "varchar", "nchar", "nvarchar":
		t := &ast.CharType{Name: strings.ToUpper(lower), P: pos}
		if p.isPunct("(") {
			if p.peekIsKw(1, "MAX") {
				p.advance()
				p.advance()
				p.expectPunct(")")
				t.Max = true
			} else if args, ok := p.parseTypeArgs(); ok && len(args) > 0 {
				t.Length = args[0]
				t.HasLength = true
			}
		}
		return t
	case "binary", "varbinary":
		t := &ast.BinaryType{Name: strings.ToUpper(lower), P: pos}
		if p.isPunct("(") {
			if p.peekIsKw(1, "MAX") {
				p.advance()
				p.advance()
				p.expectPunct(")")
				t.Max = true
			} else if args, ok := p.parseTypeArgs(); ok && len(args) > 0 {
				t.Length = args[0]
			}
		} else {
			t.Length = 1
		}
		return t
	case "text":
		return &ast.TextType{Name: "TEXT", P: pos}
	case "xml":
		t := &ast.TSQLType{Name: "XML", P: pos}
		if p.isPunct("(") {
			p.advance()
			if p.isKw("CONTENT") {
				p.advance()
			} else if p.isKw("DOCUMENT") {
				p.advance()
				t.XMLDocument = true
			}
			ref := p.parseTableRef()
			if ref.Schema != "" {
				t.XMLSchema = ref.Schema + "." + ref.Name
			} else {
				t.XMLSchema = ref.Name
			}
			p.expectPunct(")")
		}
		return t
	case "bit", "money", "smallmoney", "datetime", "smalldatetime", "uniqueidentifier",
		"rowversion", "timestamp", "hierarchyid", "sql_variant", "geography", "geometry",
		"image", "ntext", "sysname":
		return &ast.TSQLType{Name: strings.ToUpper(lower), P: pos}
	}
	return &ast.UserDefinedType{Name: name, P: pos}
}

// parseTypeArgs parses the `(n [, m])` arguments of a data type. ok is
// false when no '(' follows.
func (p *Parser) parseTypeArgs() ([]int, bool) {
	if !p.isPunct("(") {
		return nil, false
	}
	p.advance()
	var out []int
	for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
		if p.cur.Kind != TOK_NUMBER {
			p.errorHere("expected type length / precision", "NUMBER")
			break
		}
		n, err := strconv.Atoi(p.cur.Lit)
		if err != nil {
			p.errorHere("invalid type length / precision", "integer")
		}
		out = append(out, n)
		p.advance()
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return out, true
}
