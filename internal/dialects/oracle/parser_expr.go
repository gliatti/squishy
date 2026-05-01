package oracle

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// Structured expression parser for Oracle SQL / PL/SQL.
//
// Mirrors the MySQL counterpart but adapted to Oracle's quirks:
//   - String concatenation operator `||`
//   - The `(+)` outer-join marker attached to column references
//   - Hierarchical PRIOR unary
//   - Single-quote strings (no doubled-quote escape inside the lexer; the
//     lexer already strips `q'[...]'` alternative quoting too)
//   - Bind variables (`:name`, `:1`) emitted as TOK_BIND
//   - `CASE`, `CAST`, `INTERVAL`, `EXISTS`, `BETWEEN`, `IN`, `LIKE`, `IS`
//
// The cascade follows standard SQL precedence:
//   parseOr   → parseAnd ('OR' parseAnd)*
//   parseAnd  → parseNot ('AND' parseNot)*
//   parseNot  → 'NOT' parseCmp | parseCmp
//   parseCmp  → parseConcat (cmp_op parseConcat | [NOT] BETWEEN ... AND ...
//                            | [NOT] IN (list|subquery)
//                            | [NOT] LIKE expr [ESCAPE c]
//                            | IS [NOT] (NULL|DISTINCT FROM expr)
//                            )*
//   parseConcat → parseAdd ('||' parseAdd)*
//   parseAdd  → parseMul ([+-] parseMul)*
//   parseMul  → parseUnary ([*/] parseUnary)*
//   parseUnary → ('+'|'-'|'PRIOR'|'CONNECT_BY_ROOT') parsePrimary | parsePrimary
//   parsePrimary → number | string | NULL | TRUE | FALSE | bind | ident-path
//                  | '(' (SELECT|expr) ')'
//                  | EXISTS '(' SELECT ')'
//                  | CASE ... END
//                  | CAST '(' expr AS type ')'
//                  | INTERVAL '<expr>' <unit>
//                  | functionCall
// ---------------------------------------------------------------------------

func (p *Parser) parseExpr() ast.Expr        { return p.parseOr() }

// parseExprUntil parses a structured expression and returns a typed
// ast.Expr. The variadic `stops` argument is documentation only:
// parseExpr's cascade naturally halts on any token it can't recognise
// (THEN, LOOP, INTO, USING, …), so the caller knows that on return
// p.cur is positioned on one of `stops` (or EOF). Use this in place of
// the legacy parseExprUntilKeyword/parseExprUntilSemi which emit
// *ast.RawExpr — every site migrated to parseExprUntil now feeds the
// AST-only translation pipeline with typed nodes.
//
// If parseExpr ever stops on a token NOT in `stops` (parser bug, or an
// unrecognised primary), the caller will surface that as a downstream
// expectKw failure — kept loose here to match parseExprUntilKeyword's
// silent recovery behaviour during the staged migration.
func (p *Parser) parseExprUntil(stops ...string) ast.Expr {
	_ = stops
	return p.parseExpr()
}

func (p *Parser) parseOr() ast.Expr {
	lhs := p.parseAnd()
	for p.isKw("OR") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseAnd()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseAnd() ast.Expr {
	lhs := p.parseNot()
	for p.isKw("AND") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseNot()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseNot() ast.Expr {
	if p.isKw("NOT") {
		start := p.cur.Pos
		p.advance()
		rhs := p.parseCmp()
		return &ast.UnaryExpr{Op: "NOT", Rhs: rhs, P: astPos(start)}
	}
	return p.parseCmp()
}

func (p *Parser) parseCmp() ast.Expr {
	lhs := p.parseConcat()
	for {
		// `[NOT] BETWEEN lo AND hi`, `[NOT] IN (...)` and `[NOT] LIKE rhs`.
		if p.isKw("NOT") {
			savedPos := p.cur.Pos
			p.advance()
			switch {
			case p.isKw("BETWEEN"):
				p.advance()
				low := p.parseConcat()
				p.expectKw("AND")
				high := p.parseConcat()
				lhs = &ast.BetweenExpr{Expr: lhs, Low: low, High: high, Not: true, P: astPos(savedPos)}
				continue
			case p.isKw("IN"):
				p.advance()
				lhs = p.parseInRest(lhs, true, savedPos)
				continue
			case p.isKw("LIKE"):
				p.advance()
				rhs := p.parseConcat()
				p.consumeEscape()
				lhs = &ast.BinaryExpr{Op: "NOT LIKE", Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
				continue
			default:
				rhs := p.parseConcat()
				lhs = &ast.UnaryExpr{Op: "NOT", Rhs: rhs, P: astPos(savedPos)}
				continue
			}
		}
		if p.isKw("BETWEEN") {
			start := p.cur.Pos
			p.advance()
			low := p.parseConcat()
			p.expectKw("AND")
			high := p.parseConcat()
			lhs = &ast.BetweenExpr{Expr: lhs, Low: low, High: high, P: astPos(start)}
			continue
		}
		if p.isKw("IN") {
			start := p.cur.Pos
			p.advance()
			lhs = p.parseInRest(lhs, false, start)
			continue
		}
		if p.isKw("LIKE") {
			p.advance()
			rhs := p.parseConcat()
			p.consumeEscape()
			lhs = &ast.BinaryExpr{Op: "LIKE", Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
			continue
		}
		if p.isKw("IS") {
			start := p.cur.Pos
			p.advance()
			op := "IS"
			if p.isKw("NOT") {
				p.advance()
				op = "IS NOT"
			}
			// IS [NOT] NULL — null literal is a keyword in Oracle.
			if p.isKw("NULL") {
				p.advance()
				rhs := &ast.Literal{Kind: "null", Text: "NULL", P: astPos(start)}
				lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
				continue
			}
			// IS [NOT] DISTINCT FROM expr (Oracle 23ai+).
			if p.isKw("DISTINCT") {
				p.advance()
				p.expectKw("FROM")
				rhs := p.parseConcat()
				op2 := "IS DISTINCT FROM"
				if op == "IS NOT" {
					op2 = "IS NOT DISTINCT FROM"
				}
				lhs = &ast.BinaryExpr{Op: op2, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
				continue
			}
			// Fallback: parse a generic rhs.
			rhs := p.parseConcat()
			lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
			continue
		}
		op := ""
		switch {
		case p.isPunct("="), p.isPunct("<>"), p.isPunct("!="), p.isPunct("^="),
			p.isPunct("<"), p.isPunct("<="), p.isPunct(">"), p.isPunct(">="):
			op = p.cur.Lit
			p.advance()
		default:
			return lhs
		}
		rhs := p.parseConcat()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
}

// consumeEscape swallows the optional `ESCAPE 'c'` suffix attached to LIKE.
// The escape character is dropped for now — PG's LIKE supports the same
// clause but the AST does not yet model it.
func (p *Parser) consumeEscape() {
	if p.isKw("ESCAPE") {
		p.advance()
		// expression for the escape literal (usually a single-character string)
		_ = p.parseConcat()
	}
}

func (p *Parser) parseInRest(lhs ast.Expr, not bool, start Position) ast.Expr {
	if !p.isPunct("(") {
		op := "IN"
		if not {
			op = "NOT IN"
		}
		rhs := p.parseConcat()
		return &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	p.advance()
	in := &ast.InExpr{Expr: lhs, Not: not, P: astPos(start)}
	if p.isKw("SELECT") || p.isKw("WITH") {
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

func (p *Parser) parseConcat() ast.Expr {
	lhs := p.parseAdd()
	for p.isPunct("||") {
		p.advance()
		rhs := p.parseAdd()
		lhs = &ast.BinaryExpr{Op: "||", Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseAdd() ast.Expr {
	lhs := p.parseMul()
	for p.isPunct("+") || p.isPunct("-") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseMul()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseMul() ast.Expr {
	lhs := p.parseUnary()
	for p.isPunct("*") || p.isPunct("/") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseUnary()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}

func (p *Parser) parseUnary() ast.Expr {
	if p.isPunct("-") || p.isPunct("+") {
		start := p.cur.Pos
		op := p.cur.Lit
		p.advance()
		rhs := p.parseUnary()
		return &ast.UnaryExpr{Op: op, Rhs: rhs, P: astPos(start)}
	}
	if p.isKw("PRIOR") || p.isKw("CONNECT_BY_ROOT") {
		start := p.cur.Pos
		op := p.cur.Lit
		p.advance()
		rhs := p.parseUnary()
		return &ast.UnaryExpr{Op: op, Rhs: rhs, P: astPos(start)}
	}
	return p.parsePrimary()
}

func (p *Parser) parsePrimary() ast.Expr {
	start := p.cur.Pos
	switch {
	case p.isKw("EXISTS"):
		p.advance()
		p.expectPunct("(")
		sub := p.parseSelectStatement()
		p.expectPunct(")")
		return &ast.ExistsExpr{Subquery: sub, P: astPos(start)}
	case p.isPunct("("):
		p.advance()
		if p.isKw("SELECT") || p.isKw("WITH") {
			sub := p.parseSelectStatement()
			p.expectPunct(")")
			return &ast.SubqueryExpr{Stmt: sub, P: astPos(start)}
		}
		inner := p.parseExpr()
		p.expectPunct(")")
		return &ast.ParenExpr{Inner: inner, P: astPos(start)}
	case p.cur.Kind == TOK_NUMBER:
		v := p.cur.Lit
		p.advance()
		return &ast.Literal{Kind: "number", Text: v, P: astPos(start)}
	case p.cur.Kind == TOK_STRING:
		v := p.cur.Lit
		p.advance()
		return &ast.Literal{Kind: "string", Text: v, P: astPos(start)}
	case p.cur.Kind == TOK_HEX:
		v := p.cur.Lit
		p.advance()
		return &ast.Literal{Kind: "hex", Text: v, P: astPos(start)}
	case p.cur.Kind == TOK_BIND:
		v := ":" + p.cur.Lit
		p.advance()
		return &ast.Ident{Parts: []string{v}, P: astPos(start)}
	case p.isKw("NULL"):
		p.advance()
		return &ast.Literal{Kind: "null", Text: "NULL", P: astPos(start)}
	case p.isKw("TRUE"):
		p.advance()
		return &ast.Literal{Kind: "bool", Text: "TRUE", P: astPos(start)}
	case p.isKw("FALSE"):
		p.advance()
		return &ast.Literal{Kind: "bool", Text: "FALSE", P: astPos(start)}
	case p.isKw("CASE"):
		return p.parseCaseExpr()
	case p.isKw("CAST"):
		return p.parseCastExpr()
	case p.isKw("INTERVAL"):
		return p.parseIntervalLit()
	case p.isKw("SYSDATE"), p.isKw("SYSTIMESTAMP"), p.isKw("CURRENT_TIMESTAMP"),
		p.isKw("CURRENT_DATE"), p.isKw("LOCALTIMESTAMP"), p.isKw("UID"),
		p.isKw("ROWNUM"), p.isKw("LEVEL"):
		name := p.cur.Lit
		p.advance()
		fc := &ast.FuncCall{Name: name, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if !p.isPunct(")") {
				fc.Args = append(fc.Args, p.parseFuncArg())
				for p.isPunct(",") {
					p.advance()
					fc.Args = append(fc.Args, p.parseFuncArg())
				}
			}
			p.expectPunct(")")
		}
		return fc
	}
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
		name, bt := p.parseIdent()
		// function call?
		if p.isPunct("(") {
			p.advance()
			fc := &ast.FuncCall{Name: strings.ToUpper(name), P: astPos(start)}
			if p.isKw("DISTINCT") || p.isKw("ALL") {
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
			// Optional `WITHIN GROUP (ORDER BY ...) [OVER (...)]` — wrap in
			// WindowedAgg.
			if p.isKw("WITHIN") || p.isKw("OVER") {
				return p.maybeWindowedAgg(fc, start)
			}
			// `(+)` outer-join marker after a function-call result is not
			// valid Oracle, so we don't check it here.
			return fc
		}
		// identifier path
		parts := []string{name}
		for p.isPunct(".") {
			p.advance()
			if p.isPunct("*") {
				p.advance()
				parts = append(parts, "*")
				break
			}
			n, _ := p.parseIdent()
			if n != "" {
				parts = append(parts, n)
			}
		}
		// Schema-qualified function call — `pkg.fn(args)`, `schema.pkg.fn(args)`.
		// The plain function-call branch above only handles the bare-name shape.
		// Without this trailing-paren detection, calls like `UTL_FILE.FOPEN(…)`
		// at statement position parse as Ident{[UTL_FILE,FOPEN]} with the `(`
		// left for the next iteration, which then tries to start a fresh PL/SQL
		// statement on `(` and surfaces as "unexpected token in PL/SQL body
		// (got "(", expected stmt)". Wire up the FuncCall here so the dotted
		// path is consumed cleanly when followed by an arg list.
		//
		// Skip the conversion when the path ends with `*` (schema.tbl.*) — that
		// shape is a SELECT-projection star, not a callable. Skip too when the
		// next non-paren token forms a `(+)` outer-join marker (handled below).
		if p.isPunct("(") && len(parts) > 1 && parts[len(parts)-1] != "*" {
			next := p.l.Peek()
			isOuterJoinHint := next.Kind == TOK_PUNCT && next.Lit == "+"
			if !isOuterJoinHint {
				fullName := strings.Join(parts, ".")
				p.advance() // consume '('
				fc := &ast.FuncCall{Name: strings.ToUpper(fullName), P: astPos(start)}
				if p.isKw("DISTINCT") || p.isKw("ALL") {
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
				if p.isKw("WITHIN") || p.isKw("OVER") {
					return p.maybeWindowedAgg(fc, start)
				}
				return fc
			}
		}
		// Oracle PL/SQL cursor attribute: cur%FOUND / cur%NOTFOUND /
		// cur%ISOPEN / cur%ROWCOUNT, plus the implicit-cursor form
		// SQL%<attr>. We're in an expression context (parsePrimary),
		// so the only `%`-suffix that makes sense here is a cursor
		// attribute — `%TYPE` / `%ROWTYPE` are type-anchor markers
		// consumed by parseDataType in declaration contexts and never
		// reach here. The attribute reference forces a single-part
		// cursor name (no schema-qualified cursors in PL/SQL).
		if p.isPunct("%") && len(parts) == 1 {
			attrTok := p.l.Peek()
			attrLit := strings.ToUpper(attrTok.Lit)
			if attrTok.Kind == TOK_IDENT || attrTok.Kind == TOK_KEYWORD {
				switch attrLit {
				case "FOUND", "NOTFOUND", "ISOPEN", "ROWCOUNT":
					p.advance() // consume '%'
					p.advance() // consume attr keyword
					return &ast.CursorAttr{Cursor: parts[0], Attr: attrLit, P: astPos(start)}
				}
			}
		}
		// Oracle sequence pseudo-column: seq.NEXTVAL / seq.CURRVAL,
		// plus the schema-qualified shape schema.seq.NEXTVAL. Detect
		// when the LAST part of the ident path is the upper-case form
		// of NEXTVAL or CURRVAL — Oracle reserves both and there's no
		// ambiguity with regular column refs (Oracle disallows columns
		// with those names). The remaining parts (1 or 2) split into
		// schema/name as expected.
		if n := len(parts); n >= 2 {
			op := strings.ToUpper(parts[n-1])
			if op == "NEXTVAL" || op == "CURRVAL" {
				ref := &ast.SequenceRef{Op: op, P: astPos(start)}
				if n == 2 {
					ref.Name = parts[0]
				} else {
					// 3-part path: schema.seq.NEXTVAL. Anything beyond
					// schema.seq.<op> isn't valid Oracle, so we honour
					// only the canonical shape.
					ref.Schema = parts[0]
					ref.Name = parts[1]
				}
				return ref
			}
		}
		var head ast.Expr = &ast.Ident{Parts: parts, Backtick: bt, P: astPos(start)}
		// Oracle outer-join marker `(+)` on a column reference.
		if p.isPunct("(") {
			// Peek inside the parens to see if it's `(+)`.
			savedPos := p.cur.Pos
			_ = savedPos
			// We must not commit to consuming '(' until we know it's `(+)`.
			// The lexer's Peek gives us one token of look-ahead. That's not
			// enough to differentiate `(+)` from `(...)` reliably. We handle
			// this defensively: only attempt `(+)` consumption when the next
			// token after `(` is the `+` punct followed by `)`.
			t := p.l.Peek()
			if t.Kind == TOK_PUNCT && t.Lit == "+" {
				p.advance() // consume '('
				p.advance() // consume '+'
				if p.isPunct(")") {
					p.advance()
					head = &ast.OuterJoinHint{Inner: head, P: astPos(start)}
				}
				// If we mis-detected, the parser is in a broken state; the
				// downstream code surfaces the next error.
			}
		}
		return head
	}
	p.errorHere("unexpected token in expression", "expression")
	p.advance()
	return &ast.RawExpr{Text: "", P: astPos(start)}
}

func (p *Parser) parseFuncArg() ast.Expr {
	if p.isPunct("*") {
		start := p.cur.Pos
		p.advance()
		return &ast.Ident{Parts: []string{"*"}, P: astPos(start)}
	}
	// Some Oracle aggregate calls accept a `keyword => value` named-argument
	// notation; we accept it loosely by passing through the value.
	if (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD) && p.l.Peek().Kind == TOK_ARROW {
		// consume name and =>
		p.advance()
		p.advance()
	}
	return p.parseExpr()
}

// maybeWindowedAgg wraps a freshly-parsed FuncCall in a WindowedAgg when the
// next token starts a `WITHIN GROUP (ORDER BY ...)` or `OVER (...)` clause.
func (p *Parser) maybeWindowedAgg(fc *ast.FuncCall, start Position) ast.Expr {
	w := &ast.WindowedAgg{Func: fc, P: astPos(start)}
	if p.isKw("WITHIN") {
		p.advance()
		p.expectKw("GROUP")
		p.expectPunct("(")
		if p.isKw("ORDER") {
			p.advance()
			p.expectKw("BY")
			for {
				oi := ast.OrderItem{Expr: p.parseExpr()}
				if p.isKw("ASC") {
					p.advance()
				} else if p.isKw("DESC") {
					p.advance()
					oi.Desc = true
				}
				w.Within = append(w.Within, oi)
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
		}
		p.expectPunct(")")
	}
	if p.isKw("OVER") {
		p.advance()
		w.Over = p.parseWindowSpec()
	}
	return w
}

// parseWindowSpec consumes a `(...)` window body, capturing PartitionBy /
// OrderBy when recognised and falling back to RawSpec for the rest. The
// frame clause (ROWS/RANGE/GROUPS BETWEEN ... AND ...) is preserved verbatim.
func (p *Parser) parseWindowSpec() *ast.WindowSpec {
	p.expectPunct("(")
	w := &ast.WindowSpec{}
	startOff := p.cur.Pos.Offset
	if p.isKw("PARTITION") {
		p.advance()
		p.expectKw("BY")
		for {
			w.PartitionBy = append(w.PartitionBy, p.parseExpr())
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	if p.isKw("ORDER") {
		p.advance()
		p.expectKw("BY")
		for {
			oi := ast.OrderItem{Expr: p.parseExpr()}
			if p.isKw("ASC") {
				p.advance()
			} else if p.isKw("DESC") {
				p.advance()
				oi.Desc = true
			}
			w.OrderBy = append(w.OrderBy, oi)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	// Frame clause — keep as raw text up to the closing paren.
	if p.isKw("ROWS") || p.isKw("RANGE") || p.isAnyKw("GROUPS") {
		frameStart := p.cur.Pos.Offset
		depth := 0
		for !p.isPunct(")") || depth > 0 {
			if p.isPunct("(") {
				depth++
			} else if p.isPunct(")") {
				depth--
			}
			if p.cur.Kind == TOK_EOF {
				break
			}
			p.advance()
		}
		w.Frame = strings.TrimSpace(string(p.src[frameStart:p.cur.Pos.Offset]))
	}
	if w.Frame == "" && len(w.PartitionBy) == 0 && len(w.OrderBy) == 0 {
		// Spec we couldn't structurally decompose — capture the body raw.
		// Walk to the matching ')' (we are already past '(').
		depth := 0
		for !p.isPunct(")") || depth > 0 {
			if p.isPunct("(") {
				depth++
			} else if p.isPunct(")") {
				depth--
			}
			if p.cur.Kind == TOK_EOF {
				break
			}
			p.advance()
		}
		w.RawSpec = strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
	}
	p.expectPunct(")")
	return w
}

// parseCaseExpr handles both simple (`CASE x WHEN v THEN r ... END`) and
// searched (`CASE WHEN cond THEN r ... ELSE e END`) forms.
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
	if p.isKw("ELSE") {
		p.advance()
		out.Else = p.parseExpr()
	}
	p.expectKw("END")
	return out
}

// parseCastExpr handles `CAST(<expr> AS <type>)`.
func (p *Parser) parseCastExpr() ast.Expr {
	start := p.cur.Pos
	p.expectKw("CAST")
	p.expectPunct("(")
	inner := p.parseExpr()
	p.expectKw("AS")
	dt := p.parseDataType()
	p.expectPunct(")")
	return &ast.CastExpr{Expr: inner, Type: dt, P: astPos(start)}
}

// parseIntervalLit handles `INTERVAL <expr> <unit> [TO <unit>]`.
func (p *Parser) parseIntervalLit() ast.Expr {
	start := p.cur.Pos
	p.expectKw("INTERVAL")
	val := p.parsePrimary()
	unit := ""
	if p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT {
		unit = p.cur.Lit
		p.advance()
		// optional precision (n)
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
		// optional TO <unit>
		if p.isKw("TO") {
			p.advance()
			if p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT {
				unit = unit + " TO " + p.cur.Lit
				p.advance()
			}
		}
	}
	value := ""
	switch v := val.(type) {
	case *ast.Literal:
		value = v.Text
	case *ast.Ident:
		value = strings.Join(v.Parts, ".")
	}
	return &ast.IntervalLit{Value: value, Unit: unit, P: astPos(start)}
}
