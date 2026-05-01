package oracle

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ParseRoutineBody parses an Oracle/PL-SQL procedural body (the content of
// CREATE PROCEDURE/FUNCTION/TRIGGER/PACKAGE BODY, or an anonymous block) into
// a tree of PLStmt nodes. A body that is a single simple statement (no
// DECLARE/BEGIN block) is represented as a single-element slice wrapping
// that statement.
//
// Spec: internal/dialects/oracle/reference/PlSqlParser.g4, rules
// plsql_block / seq_of_statements / declare_spec and below.
func ParseRoutineBody(src string) ([]ast.PLStmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	if p.isKw("DECLARE") || p.isKw("BEGIN") || p.cur.Kind == TOK_LABEL_START {
		blk := p.parsePLBlock("")
		return []ast.PLStmt{blk}, p.errs
	}
	var out []ast.PLStmt
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF && !p.isPLStopKeyword() {
		if stmt := p.parsePLStmt(); stmt != nil {
			out = append(out, stmt)
		}
		if p.isPunct(";") {
			p.advance()
		}
	}
	return out, p.errs
}

// parseAnonymousBlock is the top-level parser path for a script-level
// anonymous block (DECLARE … BEGIN … END; or BEGIN … END;).
func (p *Parser) parseAnonymousBlock() ast.Stmt {
	start := p.cur.Pos
	// Optional <<label>>
	label := ""
	if p.cur.Kind == TOK_LABEL_START {
		p.advance()
		label, _ = p.parseIdent()
		if p.cur.Kind != TOK_LABEL_END {
			p.errorHere("expected >>", ">>")
		} else {
			p.advance()
		}
	}
	blk := p.parsePLBlock(label)
	// Wrap into a CreateProcedure-less shell: we return the block as a PLStmt
	// wrapped inside a NoopStmt carrying the raw text, so downstream code can
	// treat it as an anonymous DO block. The translator is expected to route
	// anonymous blocks through TranslateRoutineBody directly.
	var sb strings.Builder
	plDumpBlock(&sb, blk)
	return &ast.NoopStmt{Kind: "ANONYMOUS_BLOCK", Text: sb.String(), P: astPos(start)}
}

// plDumpBlock serializes a PL/SQL block back to text for storage inside a
// NoopStmt. The translator re-parses the raw text rather than inspecting the
// NoopStmt, so this is a debug-friendly dump only.
func plDumpBlock(sb *strings.Builder, blk *ast.Block) {
	sb.WriteString("BEGIN; /* anonymous block parsed; ")
	sb.WriteString("decls=")
	sb.WriteString(itoa(len(blk.Decls)))
	sb.WriteString(", stmts=")
	sb.WriteString(itoa(len(blk.Stmts)))
	sb.WriteString(" */")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return sign + string(digits[i:])
}

// isPLStopKeyword returns true when the current token is a terminator within
// a PL/SQL statement sequence (END, ELSE, ELSIF, WHEN, EXCEPTION).
func (p *Parser) isPLStopKeyword() bool {
	if p.cur.Kind != TOK_KEYWORD {
		return false
	}
	switch p.cur.Lit {
	case "END", "ELSE", "ELSIF", "WHEN", "EXCEPTION":
		return true
	}
	return false
}

// parsePLBlock parses a DECLARE? BEGIN <stmts> [EXCEPTION <handlers>] END [name]? block.
func (p *Parser) parsePLBlock(label string) *ast.Block {
	start := p.cur.Pos
	blk := &ast.Block{Label: label, P: astPos(start)}

	// Optional DECLARE section — decls continue until BEGIN.
	if p.isKw("DECLARE") {
		p.advance()
		for !p.isKw("BEGIN") && p.cur.Kind != TOK_EOF {
			if d := p.parsePLDecl(); d != nil {
				blk.Decls = append(blk.Decls, d)
			}
			if p.isPunct(";") {
				p.advance()
			}
		}
	}

	p.expectKw("BEGIN")
	for !p.isKw("END") && !p.isKw("EXCEPTION") && p.cur.Kind != TOK_EOF {
		if stmt := p.parsePLStmt(); stmt != nil {
			blk.Stmts = append(blk.Stmts, stmt)
		}
		if p.isPunct(";") {
			p.advance()
		}
	}

	// EXCEPTION WHEN name THEN stmts ; ...
	if p.isKw("EXCEPTION") {
		exceptStart := p.cur.Pos
		p.advance()
		// Phase 1.10 populates Block.Except with the typed handler list
		// alongside the legacy /*EXCEPTION*/ RawSQL marker. The text path
		// in the translator still parses the marker; the typed Except is
		// strictly additive and consumed by the Phase 3 AST visitor that
		// will eventually replace the legacy path. typed = parsed via a
		// sub-parser run on each handler body so the inner statements are
		// real PLStmt nodes (not opaque text).
		except := &ast.ExceptionBlock{P: astPos(exceptStart)}
		var buf strings.Builder
		buf.WriteString("/*EXCEPTION*/")
		for p.isKw("WHEN") {
			p.advance()
			var names []string
			for {
				if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
					name := p.cur.Lit
					p.advance()
					// Oracle allows package-qualified exception names —
					// e.g. `WHEN UTL_FILE.INVALID_PATH THEN`. Stitch the
					// `.<part>` segments back onto the leading ident so
					// the translator sees the full dotted name and can
					// remap it (PG plpgsql has no concept of package-
					// qualified exception conditions, so the post-pass
					// rewrites them to OTHERS).
					for p.isPunct(".") {
						p.advance()
						if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
							name += "." + p.cur.Lit
							p.advance()
						} else {
							break
						}
					}
					names = append(names, name)
				}
				if p.isKw("OR") {
					p.advance()
					continue
				}
				break
			}
			p.expectKw("THEN")
			// Capture handler body until the next top-level WHEN or
			// the closing END of the EXCEPTION block. The body may
			// contain nested IF/LOOP/CASE/BEGIN blocks whose own
			// END keywords MUST NOT terminate the handler — track a
			// depth counter so a stray `END IF` mid-body doesn't
			// truncate the capture (which previously dropped the
			// remainder of the body, including `END IF;`, and
			// resulted in PG `syntax error at or near ";"` because
			// the IF was left open).
			startOff := p.cur.Pos.Offset
			depth := 0
			for p.cur.Kind != TOK_EOF {
				if depth == 0 && (p.isKw("WHEN") || p.isKw("END")) {
					break
				}
				if p.isKw("BEGIN") || p.isKw("IF") || p.isKw("LOOP") || p.isKw("CASE") {
					depth++
					p.advance()
					continue
				}
				if p.isKw("END") {
					depth--
					p.advance()
					// Consume the optional close-form keyword
					// (`END IF` / `END LOOP` / `END CASE`) so we
					// don't double-count it on the next iteration
					// when the bare keyword would re-increment depth.
					if p.isKw("IF") || p.isKw("LOOP") || p.isKw("CASE") {
						p.advance()
					}
					continue
				}
				p.advance()
			}
			body := strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
			buf.WriteString("\nWHEN ")
			buf.WriteString(strings.Join(names, " OR "))
			buf.WriteString(" THEN ")
			buf.WriteString(body)

			// Re-parse the captured handler body as PLStmts so the typed
			// ExceptionBlock carries structured nodes. ParseRoutineBody
			// without a leading DECLARE/BEGIN walks the body as a stmt
			// sequence, which is exactly what we need here. Failures
			// surface only as Except.Handlers[i].Body == nil — the
			// translator's text path stays authoritative during transition.
			handlerStmts, _ := ParseRoutineBody(body)
			except.Handlers = append(except.Handlers, ast.ExceptionHandler{
				Names: names,
				Body:  handlerStmts,
			})
		}
		blk.Except = except
		blk.Stmts = append(blk.Stmts, &ast.RawSQL{Text: buf.String(), P: astPos(start)})
	}

	p.expectKw("END")
	// optional trailing block-name identifier
	for !p.isPunct(";") && !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	return blk
}

// parsePLDecl — DECLARE section entries: variable, cursor, pragma, type,
// subtype, or exception declaration. Oracle does not use the DECLARE
// keyword per line; entries are separated by ';'.
func (p *Parser) parsePLDecl() ast.PLDecl {
	start := p.cur.Pos
	// TYPE name IS RECORD (...) | IS TABLE OF ... [INDEX BY ...]
	//                  | IS REF CURSOR [RETURN ...]
	//                  | IS VARRAY(n) OF ...
	// SUBTYPE name IS basetype;
	//
	// Oracle TYPE declarations have no direct PG counterpart for the
	// associative-array / nested-table / VARRAY shapes; rewriting them
	// would require translating every collection method invocation
	// (.COUNT, .FIRST, .NEXT(i), .EXTEND, etc.) and every BULK COLLECT
	// site. Until that machinery exists, we capture the full declaration
	// text as a TODO comment node and let the translator surface it as a
	// note + emit a `-- TODO` line in the PG body. Without this branch
	// the unknown-token fallback below truncates one token at a time and
	// the rest of the declaration leaks into the next loop iteration as
	// a chain of bogus `name TEXT;` decls (e.g. `TYPE ovt IS TABLE OF
	// trn.num_trn%TYPE INDEX BY BINARY_INTEGER;` becomes
	// `OVT TEXT; TRN TEXT; NUM_TRN TEXT; BINARY_INTEGER TEXT;`).
	if p.isKw("TYPE") || p.isKw("SUBTYPE") {
		kind := strings.ToUpper(p.cur.Lit)
		p.advance()
		name := ""
		if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
			name = p.cur.Lit
			p.advance()
		}
		body := p.captureUntilSemi()
		return &ast.PragmaStmt{Kind: kind, Text: name + " " + body, P: astPos(start)}
	}
	// PRAGMA …
	if p.isKw("PRAGMA") {
		p.advance()
		kind := ""
		if p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT {
			kind = p.cur.Lit
			p.advance()
		}
		text := p.captureUntilSemi()
		return &ast.PragmaStmt{Kind: kind, Text: text, P: astPos(start)}
	}
	// CURSOR name [(params)] IS <select>;
	if p.isKw("CURSOR") {
		p.advance()
		name, _ := p.parseIdent()
		// optional parameter list — capture the inner text so the
		// translator can re-emit it on the PG cursor declaration.
		// Oracle: `CURSOR c (p1 t1, p2 t2) IS SELECT …`
		// PG:     `c CURSOR (p1 t1, p2 t2) FOR SELECT …`
		// Without this we'd drop the params, the OPEN call would still
		// pass arguments and PG fails with "cursor X has no arguments".
		var params string
		if p.isPunct("(") {
			startOff := p.cur.Pos.Offset + 1 // skip the leading '('
			depth := 1
			p.advance()
			for depth > 0 && p.cur.Kind != TOK_EOF {
				if p.isPunct("(") {
					depth++
				} else if p.isPunct(")") {
					depth--
				}
				if depth > 0 {
					p.advance()
				}
			}
			endOff := p.cur.Pos.Offset // position of the closing ')'
			if endOff > startOff && endOff <= len(p.src) {
				params = strings.TrimSpace(string(p.src[startOff:endOff]))
			}
			p.expectPunct(")")
		}
		// optional RETURN type
		if p.isKw("RETURN") {
			p.advance()
			_ = p.parseDataType()
		}
		if p.isKw("IS") {
			p.advance()
		}
		sel := p.captureUntilSemi()
		return &ast.DeclareCursor{Name: name, Params: params, SelectBody: sel, P: astPos(start)}
	}
	// Variable or constant declaration: name [CONSTANT] type [:= expr];
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		name, _ := p.parseIdent()
		// "exception" keyword follows the name for an exception declaration.
		if p.isKw("EXCEPTION") {
			p.advance()
			return &ast.DeclareVar{Name: name, Type: &ast.UserDefinedType{Name: "EXCEPTION", P: astPos(start)}, P: astPos(start)}
		}
		// Oracle's `CONSTANT` modifier introduces an immutable variable.
		// PG plpgsql has no equivalent of CONSTANT (vars are mutable),
		// so we just consume and drop the keyword. Note that the Oracle
		// lexer tokenises CONSTANT as TOK_IDENT (it's not in the keyword
		// table) so a plain `isKw("CONSTANT")` would miss it and the
		// rest of the parser would treat CONSTANT as the variable's
		// type, then re-parse `<type> := <expr>` as a fresh declaration
		// — yielding two bogus decls per real one (e.g. `t CONSTANT
		// VARCHAR2(1) := CHR(9);` would emit `T TEXT;` and `CHR TEXT;`).
		if p.isAnyKwOrIdent("CONSTANT") {
			p.advance()
		}
		typ := p.parseDataType()
		var def ast.Expr
		if p.cur.Kind == TOK_ASSIGN || p.isKw("DEFAULT") {
			p.advance()
			def = p.parseExprUntil(";")
		}
		// NOT NULL modifier after type (Oracle allows this in declarations).
		if p.isKw("NOT") {
			p.advance()
			p.expectKw("NULL")
		}
		return &ast.DeclareVar{Name: name, Type: typ, Default: def, P: astPos(start)}
	}
	// Unknown — skip a token.
	p.errorHere("expected declaration", "name | CURSOR | PRAGMA")
	p.advance()
	return nil
}

// parsePLStmt — one procedural statement.
func (p *Parser) parsePLStmt() ast.PLStmt {
	start := p.cur.Pos

	// Labeled statement: <<label>> <stmt>
	label := ""
	if p.cur.Kind == TOK_LABEL_START {
		p.advance()
		label, _ = p.parseIdent()
		if p.cur.Kind == TOK_LABEL_END {
			p.advance()
		}
	}

	switch {
	case p.isKw("BEGIN"):
		return p.parsePLBlock(label)
	case p.isKw("DECLARE"):
		// Nested anonymous block: `DECLARE … BEGIN … END;`. PL/SQL allows
		// these inside any IF/LOOP/etc. body. Without this case, the
		// dispatcher fell through to parseAssignOrCall and emitted phantom
		// `CALL "P_NUM_CTA_ABT"()` statements for each declared variable
		// — see BIR#ECR_CLI which had `if … then DECLARE p_num_cta_abt
		// cpt_cli.num_cta_abt%TYPE; … BEGIN SELECT … INTO p_num_cta_abt`
		// and the SELECT body referenced an undeclared variable in PG.
		return p.parsePLBlock(label)
	case p.isKw("IF"):
		return p.parseIfStmt()
	case p.isKw("CASE"):
		return p.parseCaseStmt()
	case p.isKw("LOOP"):
		return p.parseLoopStmt(label)
	case p.isKw("WHILE"):
		return p.parseWhileStmt(label)
	case p.isKw("FOR"):
		return p.parseForStmt(label)
	case p.isKw("FORALL"):
		return p.parseForallStmt()
	case p.isKw("EXIT"):
		return p.parseExitStmt()
	case p.isKw("CONTINUE"):
		return p.parseContinueStmt()
	case p.isKw("GOTO"):
		p.advance()
		name, _ := p.parseIdent()
		return &ast.GotoStmt{Label: name, P: astPos(start)}
	case p.isKw("RETURN"):
		p.advance()
		if p.isPunct(";") || p.atStatementEnd() {
			return &ast.ReturnStmt{P: astPos(start)}
		}
		e := p.parseExprUntil(";")
		return &ast.ReturnStmt{Expr: e, P: astPos(start)}
	case p.isKw("RAISE"):
		p.advance()
		if p.isPunct(";") || p.atStatementEnd() {
			return &ast.RaiseStmt{P: astPos(start)}
		}
		// Either RAISE <name>; or RAISE_APPLICATION_ERROR(args)
		name, _ := p.parseIdent()
		return &ast.RaiseStmt{Name: name, P: astPos(start)}
	case p.isKw("NULL"):
		p.advance()
		return &ast.NullStmt{P: astPos(start)}
	case p.isKw("EXECUTE"):
		return p.parseExecuteImmediate()
	case p.isKw("OPEN"):
		p.advance()
		name, _ := p.parseIdent()
		s := &ast.OpenStmt{Cursor: name, P: astPos(start)}
		// Static cursor open with optional parameter list:
		//   `OPEN cur(a1, a2)` — capture the inner-paren text so the
		// writer can re-emit it. Without this, a cursor declared with
		// parameters is opened bare (`OPEN cur;`) which PG rejects with
		// `cursor "cur" has arguments`.
		if p.isPunct("(") {
			startOff := p.cur.Pos.Offset + 1 // skip leading `(`
			depth := 1
			p.advance()
			for depth > 0 && p.cur.Kind != TOK_EOF {
				if p.isPunct("(") {
					depth++
				} else if p.isPunct(")") {
					depth--
				}
				if depth > 0 {
					p.advance()
				}
			}
			endOff := p.cur.Pos.Offset // position of the closing `)`
			if endOff > startOff && endOff <= len(p.src) {
				s.Args = strings.TrimSpace(string(p.src[startOff:endOff]))
			}
			p.expectPunct(")")
		}
		// Dynamic / for-query open: `OPEN cur FOR <query|expr> [USING args]`.
		if p.isKw("FOR") {
			p.advance()
			startOff := p.cur.Pos.Offset
			depth := 0
			for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
				if p.isPunct("(") {
					depth++
				} else if p.isPunct(")") {
					depth--
				}
				if depth == 0 && p.isKw("USING") {
					break
				}
				p.advance()
			}
			s.ForQuery = strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
			// Dynamic vs static: if the body's first significant token isn't
			// SELECT (or WITH for CTEs), treat it as a runtime-built string
			// expression — PG needs OPEN … FOR EXECUTE … in that case.
			head := strings.ToUpper(strings.TrimLeft(s.ForQuery, " \t\r\n("))
			s.IsDynamic = !(strings.HasPrefix(head, "SELECT") || strings.HasPrefix(head, "WITH"))
			if p.isKw("USING") {
				p.advance()
				for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
					argStart := p.cur.Pos.Offset
					ad := 0
					for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
						if p.isPunct("(") {
							ad++
						} else if p.isPunct(")") {
							ad--
						}
						if ad == 0 && p.isPunct(",") {
							break
						}
						p.advance()
					}
					arg := strings.TrimSpace(string(p.src[argStart:p.cur.Pos.Offset]))
					if arg != "" {
						s.UsingArgs = append(s.UsingArgs, arg)
					}
					if p.isPunct(",") {
						p.advance()
					} else {
						break
					}
				}
			}
		}
		return s
	case p.isKw("CLOSE"):
		p.advance()
		name, _ := p.parseIdent()
		return &ast.CloseStmt{Cursor: name, P: astPos(start)}
	case p.isKw("FETCH"):
		return p.parseFetchStmt()
	case p.isKw("COMMIT"), p.isKw("ROLLBACK"), p.isKw("SAVEPOINT"):
		raw := p.captureUntilSemi()
		return &ast.RawSQL{Text: raw, P: astPos(start)}
	case p.isKw("SELECT"), p.isKw("WITH"):
		return p.parseSelectOrRawDML(start)
	case p.isKw("INSERT"):
		return p.parseInsertStatement()
	case p.isKw("UPDATE"):
		return p.parseUpdateStatement()
	case p.isKw("DELETE"):
		return p.parseDeleteStatement()
	case p.isKw("MERGE"):
		return p.parseMergeStatement()
	}

	// Assignment or procedure call: identifier chain then := expr  OR  (args).
	// TOK_BIND is also accepted as the target prefix because Oracle trigger
	// bodies use `:NEW.col := …` / `:OLD.col := …` where `:NEW`/`:OLD` lex
	// as bind tokens. The post-translation rewriter strips the leading `:`
	// so the emitted PG code is valid.
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_BIND {
		return p.parseAssignOrCall(start)
	}
	// NEW / OLD lex as keywords (trigger metadata clauses) but at statement
	// start they introduce a trigger pseudorecord assignment such as
	// `NEW.col := …;`. Accept them here so the body parser doesn't bail
	// out — the colon-prefixed `:NEW.col` form is already pre-rewritten to
	// `NEW.col` upstream by stripBindRefColons.
	if p.isKw("NEW") || p.isKw("OLD") {
		return p.parseAssignOrCall(start)
	}

	p.errorHere("unexpected token in PL/SQL body", "stmt")
	p.advance()
	return nil
}

func (p *Parser) parseIfStmt() *ast.IfStmt {
	start := p.cur.Pos
	p.expectKw("IF")
	s := &ast.IfStmt{P: astPos(start)}
	// Phase 3.8: typed Cond — Phase 3.3 introduced
	// MakeTriggerAliasComposite and Phase 3.6/3.8 wired it into the
	// translation pipeline (trigger callers thread NewAlias/OldAlias
	// to TranslateRoutineBodyExtV). The AST visitor renames NR/OR1 →
	// NEW/OLD on the parsed Idents BEFORE rawExpr renders, and the
	// writer's isPLpgSQLPseudoCol guard emits the unquoted PG plpgsql
	// form. This unblocks the migration deferred in Phase 1.1.
	cond := p.parseExprUntil("THEN")
	p.expectKw("THEN")
	body := p.parseStmtsUntilAny("END", "ELSIF", "ELSE")
	s.Branches = append(s.Branches, ast.IfBranch{Cond: cond, Body: body})
	for p.isKw("ELSIF") {
		p.advance()
		c := p.parseExprUntil("THEN")
		p.expectKw("THEN")
		b := p.parseStmtsUntilAny("END", "ELSIF", "ELSE")
		s.Branches = append(s.Branches, ast.IfBranch{Cond: c, Body: b})
	}
	if p.isKw("ELSE") {
		p.advance()
		s.Else = p.parseStmtsUntilAny("END")
	}
	// Only consume `END IF` when it actually IS `END IF`. If we land on
	// an `END LOOP` / `END CASE` / etc. — the IF isn't ours to close
	// and a structural error happened upstream (typically a body-stmt
	// dispatcher saw a `LOOP` keyword and parsed a phantom LoopStmt
	// inside the IF body, mismatching the real END LOOP that came
	// later). Soft-fail here: leave END in place so the outer block
	// reaper closes its enclosing structure correctly. Without this,
	// PRC_MAJ_TRC ended up with an extra `END IF;` and the next
	// statement (a LOOP) got mis-parsed as a new top-level loop.
	if p.isKw("END") && p.peek().Kind == TOK_KEYWORD && p.peek().Lit == "IF" {
		p.expectKw("END") // consume END
		p.expectKw("IF")  // consume IF
	}
	return s
}

func (p *Parser) parseCaseStmt() *ast.CaseStmt {
	start := p.cur.Pos
	p.expectKw("CASE")
	s := &ast.CaseStmt{P: astPos(start)}
	// searched CASE starts directly with WHEN
	if !p.isKw("WHEN") {
		s.Expr = p.parseExprUntil("WHEN")
	}
	for p.isKw("WHEN") {
		p.advance()
		match := p.parseExprUntil("THEN")
		p.expectKw("THEN")
		body := p.parseStmtsUntilAny("WHEN", "ELSE", "END")
		s.When = append(s.When, ast.CaseWhen{Match: match, Body: body})
	}
	if p.isKw("ELSE") {
		p.advance()
		s.Else = p.parseStmtsUntilAny("END")
	}
	p.expectKw("END")
	if p.isKw("CASE") {
		p.advance()
	}
	return s
}

func (p *Parser) parseLoopStmt(label string) *ast.LoopStmt {
	start := p.cur.Pos
	p.expectKw("LOOP")
	body := p.parseStmtsUntilAny("END")
	p.expectKw("END")
	p.expectKw("LOOP")
	// optional trailing label ident
	if p.cur.Kind == TOK_IDENT {
		p.advance()
	}
	return &ast.LoopStmt{Label: label, Body: body, P: astPos(start)}
}

func (p *Parser) parseWhileStmt(label string) *ast.WhileStmt {
	start := p.cur.Pos
	p.expectKw("WHILE")
	cond := p.parseExprUntil("LOOP")
	p.expectKw("LOOP")
	body := p.parseStmtsUntilAny("END")
	p.expectKw("END")
	p.expectKw("LOOP")
	if p.cur.Kind == TOK_IDENT {
		p.advance()
	}
	return &ast.WhileStmt{Label: label, Cond: cond, Body: body, P: astPos(start)}
}

func (p *Parser) parseForStmt(label string) ast.PLStmt {
	start := p.cur.Pos
	p.expectKw("FOR")
	v, _ := p.parseIdent()
	p.expectKw("IN")
	reverse := false
	if p.isKw("REVERSE") {
		p.advance()
		reverse = true
	}
	// Variants:
	//   (SELECT ...) LOOP     — cursor-for over inline query
	//   cursor_name[(args)] LOOP — cursor-for over a named cursor
	//   expr .. expr LOOP     — numeric for
	if p.isPunct("(") {
		// inline SELECT
		body := p.captureBalancedParens()
		p.expectKw("LOOP")
		stmts := p.parseStmtsUntilAny("END")
		p.expectKw("END")
		p.expectKw("LOOP")
		if p.cur.Kind == TOK_IDENT {
			p.advance()
		}
		return &ast.CursorForStmt{
			Label: label, Record: v,
			SelectBody: body,
			Body:       stmts,
			P:          astPos(start),
		}
	}
	// could be numeric or cursor-name
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		// Save position to allow rewinding — we need to detect `.. ` ahead.
		// Simpler: parse an expression up to either LOOP or "..", then decide.
		// parseExpr halts naturally on TOK_RANGE (`..`) and on the LOOP
		// keyword, so the typed cascade is a drop-in replacement here.
		expr1 := p.parseExprUntil("LOOP")
		if p.cur.Kind == TOK_RANGE {
			p.advance()
			expr2 := p.parseExprUntil("LOOP")
			p.expectKw("LOOP")
			stmts := p.parseStmtsUntilAny("END")
			p.expectKw("END")
			p.expectKw("LOOP")
			if p.cur.Kind == TOK_IDENT {
				p.advance()
			}
			return &ast.NumericForStmt{
				Label:   label,
				Var:     v,
				Reverse: reverse,
				Low:     expr1,
				High:    expr2,
				Body:    stmts,
				P:       astPos(start),
			}
		}
		// cursor-name for: expr1 should be the cursor name
		p.expectKw("LOOP")
		stmts := p.parseStmtsUntilAny("END")
		p.expectKw("END")
		p.expectKw("LOOP")
		if p.cur.Kind == TOK_IDENT {
			p.advance()
		}
		return &ast.CursorForStmt{
			Label:      label,
			Record:     v,
			CursorName: rawExprText(expr1),
			Body:       stmts,
			P:          astPos(start),
		}
	}
	// Numeric FOR starting with a number: 1..10
	expr1 := p.parseExprUntil("LOOP")
	if p.cur.Kind == TOK_RANGE {
		p.advance()
		expr2 := p.parseExprUntil("LOOP")
		p.expectKw("LOOP")
		stmts := p.parseStmtsUntilAny("END")
		p.expectKw("END")
		p.expectKw("LOOP")
		if p.cur.Kind == TOK_IDENT {
			p.advance()
		}
		return &ast.NumericForStmt{
			Label:   label,
			Var:     v,
			Reverse: reverse,
			Low:     expr1,
			High:    expr2,
			Body:    stmts,
			P:       astPos(start),
		}
	}
	p.errorHere("unsupported FOR form", "numeric range or cursor")
	p.syncToDelimiter()
	return nil
}

func (p *Parser) parseForallStmt() *ast.ForallStmt {
	start := p.cur.Pos
	p.expectKw("FORALL")
	s := &ast.ForallStmt{P: astPos(start)}
	s.Var, _ = p.parseIdent()
	p.expectKw("IN")
	switch {
	case p.isKw("INDICES"):
		p.advance()
		p.expectKw("OF")
		id, _ := p.parseIdent()
		s.IndicesOf = id
	case p.isKw("VALUES"):
		p.advance()
		p.expectKw("OF")
		id, _ := p.parseIdent()
		s.ValuesOf = id
	default:
		// Typed range bounds for FORALL i IN low .. high SAVE EXCEPTIONS …
		// parseExpr halts on TOK_RANGE for Low and on the trailing FORALL
		// statement keyword for High.
		s.Low = p.parseExprUntil("..")
		if p.cur.Kind == TOK_RANGE {
			p.advance()
		}
		s.High = p.parseExprUntil("SAVE", "INSERT", "UPDATE", "DELETE", "MERGE", "EXECUTE")
	}
	if p.isKw("SAVE") {
		p.advance()
		p.expectKw("EXCEPTIONS")
		s.SaveExcept = true
	}
	s.RawBody = p.captureUntilSemi()
	return s
}

func (p *Parser) parseExitStmt() ast.PLStmt {
	start := p.cur.Pos
	p.expectKw("EXIT")
	label := ""
	if p.cur.Kind == TOK_IDENT {
		label = p.cur.Lit
		p.advance()
	}
	if p.isKw("WHEN") {
		// EXIT [label] WHEN cond — Phase 1.5 typed the cursor attribute
		// node so parseExpr now understands `cur%NOTFOUND` and friends,
		// removing the prior fallback to parseExprUntilSemi.
		p.advance()
		cond := p.parseExprUntil(";")
		return &ast.LeaveStmt{Label: label, WhenCond: cond, P: astPos(start)}
	}
	return &ast.LeaveStmt{Label: label, P: astPos(start)}
}

func (p *Parser) parseContinueStmt() ast.PLStmt {
	start := p.cur.Pos
	p.expectKw("CONTINUE")
	label := ""
	if p.cur.Kind == TOK_IDENT {
		label = p.cur.Lit
		p.advance()
	}
	if p.isKw("WHEN") {
		// CONTINUE [label] WHEN cond — see parseExitStmt.
		p.advance()
		cond := p.parseExprUntil(";")
		return &ast.IterateStmt{Label: label, WhenCond: cond, P: astPos(start)}
	}
	return &ast.IterateStmt{Label: label, P: astPos(start)}
}

func (p *Parser) parseExecuteImmediate() *ast.ExecuteImmediateStmt {
	start := p.cur.Pos
	p.expectKw("EXECUTE")
	if p.isKw("IMMEDIATE") {
		p.advance()
	}
	s := &ast.ExecuteImmediateStmt{P: astPos(start)}
	// parseExpr's cascade naturally halts at INTO / USING / `;` since
	// none are in its grammar. The `||` concat operator is handled by
	// parseConcat, so an EXECUTE IMMEDIATE built by string concatenation
	// (`'CREATE TABLE ' || tname || ' (id NUMBER)'`) becomes a typed
	// BinaryExpr tree of Literal + Ident nodes — the foundation the
	// Phase 4 dynamic-DDL rewriter walks instead of having to re-tokenise
	// raw text.
	s.SQL = p.parseExprUntil("INTO", "USING", ";")
	if p.isKw("INTO") || p.isKw("BULK") {
		// BULK COLLECT INTO vars
		if p.isKw("BULK") {
			p.advance()
			p.expectKw("COLLECT")
		}
		p.expectKw("INTO")
		for {
			name, _ := p.parseIdent()
			s.Into = append(s.Into, name)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	if p.isKw("USING") {
		p.advance()
		for {
			// parseExpr halts on `,` and `;` naturally (neither is in its
			// grammar), so each USING bind argument becomes a typed Expr.
			e := p.parseExprUntil(",", ";")
			s.Using = append(s.Using, e)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	return s
}

func (p *Parser) parseFetchStmt() *ast.FetchStmt {
	start := p.cur.Pos
	p.expectKw("FETCH")
	name, _ := p.parseIdent()
	s := &ast.FetchStmt{Cursor: name, P: astPos(start)}
	if p.isKw("INTO") {
		p.advance()
		for {
			v, _ := p.parseIdent()
			s.Into = append(s.Into, v)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	} else if p.isKw("BULK") {
		p.advance()
		p.expectKw("COLLECT")
		p.expectKw("INTO")
		for {
			// Allow dotted idents (record-of-collection fields, e.g.
			// `t_TRN.NUM_TRN`). Without this, the parser stopped at the
			// first `.` and PRC_TLR ended up with vars=[t_TRN] only,
			// then the trailing `.NUM_TRN, t_TRN.ANN_TRN, …` leaked
			// onto the next statement.
			parts := []string{}
			v, _ := p.parseIdent()
			parts = append(parts, v)
			for p.isPunct(".") {
				p.advance()
				n, _ := p.parseIdent()
				parts = append(parts, n)
			}
			s.Into = append(s.Into, strings.Join(parts, "."))
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		// Optional `LIMIT <expr>` clause. PG plpgsql FETCH has no
		// LIMIT counterpart — we just consume and discard the clause
		// so it doesn't leak as phantom CallStmts (`CALL "L_I_TAILLE"()`
		// in PRC_TLR). Semantic equivalence requires reshaping the
		// surrounding LOOP, which we don't do here; downstream is the
		// best place for that. For now we drop LIMIT to keep the FETCH
		// statement parse-clean.
		if p.isKw("LIMIT") {
			p.advance()
			// Capture the LIMIT expr until `;` or stmt-end so it's not
			// re-parsed.
			_ = p.parseExprUntilSemi()
		}
	}
	return s
}

func (p *Parser) parseSelectOrRawDML(start Position) ast.PLStmt {
	// Discriminate procedural `SELECT … INTO vars FROM …` (which the AST
	// tracks via SelectInto + raw query text) from a plain SELECT or
	// `WITH … SELECT` that flows through the structured DML parser.
	// The discriminator is a byte-walk over the source ahead of the cursor
	// so we don't consume tokens prematurely.
	if !p.peekProceduralSelectInto() {
		return p.parseSelectStatement()
	}
	// Capture everything between the SELECT keyword and the (top-level)
	// INTO clause as the projection list so the writer can emit a
	// well-formed PG `SELECT <list> INTO <vars> <rest>` even when the
	// source has the INTO sandwiched between the projection and the FROM.
	selectListStart := p.cur.Pos.Offset
	depth := 0
	bulkPrefix := false // tracks `… BULK COLLECT INTO` so we strip those
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
		}
		if depth == 0 && p.isKw("BULK") {
			// Look-ahead: BULK COLLECT INTO. We treat the BULK COLLECT
			// pair as part of the projection-list capture window — the
			// projection ends at BULK, INTO is the next-next keyword.
			// The captured selectList will include the trailing
			// `BULK COLLECT` text which we strip below before re-emit
			// (PG has no BULK COLLECT — assigning a SELECT result to
			// each variable returns one row, the user's reviewer can
			// reshape to array_agg if collection semantics matter).
			bulkPrefix = true
			break
		}
		if depth == 0 && p.isKw("INTO") {
			break
		}
		p.advance()
	}
	selectListEnd := p.cur.Pos.Offset
	if bulkPrefix {
		// Eat `BULK COLLECT` so the remainder is parsed as a regular
		// SELECT INTO. selectListEnd is already at the BULK keyword.
		p.expectKw("BULK")
		p.expectKw("COLLECT")
	}
	selectList := strings.TrimSpace(string(p.src[selectListStart:selectListEnd]))
	// Strip a leading SELECT keyword, case-insensitive, so the writer can
	// re-emit `SELECT <list> INTO <vars> <rest>` without a duplicate.
	if len(selectList) >= 6 && strings.EqualFold(selectList[:6], "SELECT") {
		selectList = strings.TrimSpace(selectList[6:])
	}
	// Strip `--` line comments from the captured projection list. Oracle
	// `dba_source` text often wraps long lines and inserts a `--` comment
	// in the middle of a SELECT projection (e.g. an old version of the
	// expression preserved as documentation). When this list is later
	// concatenated with the rest of the query on a SINGLE line by the
	// writer, the trailing newline that protected the `--` comment is
	// trimmed, so the comment swallows the rest of the joined statement
	// — including the `INTO target FROM …`. PG raised
	// `syntax error at or near "IF"` on BIR#CAR_ART_CLC$ID_CAR_ART_CLC
	// trigger because the SELECT had no `;` (the `;` was inside the
	// `--` comment) so the next plpgsql token was an unmatched `END IF`.
	// Block comments (`/* … */`) are line-safe and stay; only `--` is
	// the hazard here.
	selectList = stripDashLineComments(selectList)
	// BULK COLLECT INTO semantics: `SELECT col1, col2 BULK COLLECT INTO
	// arr1, arr2 FROM …` collects ALL matching rows into arrays. PG has
	// no BULK COLLECT, but the equivalent is `SELECT array_agg(col1),
	// array_agg(col2) INTO arr1, arr2 FROM …`. We wrap each top-level
	// projection in array_agg(...) so the row-set is folded into one
	// array per output array variable. The writer's INTO target (a
	// PG-typed array variable) then receives the aggregated set in a
	// single statement — preserving Oracle's bulk semantics.
	if bulkPrefix {
		selectList = wrapProjectionsInArrayAgg(selectList)
	}
	p.advance() // INTO
	var vars []string
	for {
		// Var slot may be a qualified name (`NEW.col`, `pkg.var`,
		// `record.field`, `pkg.rec.field`). Walk the dot chain so the
		// remainder of the SELECT (FROM ... WHERE ...) is captured cleanly
		// as RawQuery — otherwise the trailing `.col` leaks into the body
		// and the rendered SELECT comes out as `SELECT .col FROM …`.
		parts := []string{}
		v, _ := p.parseIdent()
		parts = append(parts, v)
		for p.isPunct(".") {
			p.advance()
			n, _ := p.parseIdent()
			parts = append(parts, n)
		}
		vars = append(vars, strings.Join(parts, "."))
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	rawStart := p.cur.Pos.Offset
	for !p.isPunct(";") && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	raw := strings.TrimSpace(string(p.src[rawStart:p.cur.Pos.Offset]))
	// Same `--` hazard as for selectList — when this raw FROM/WHERE/…
	// fragment is joined back to the projection list, line comments
	// embedded in it would also swallow the rest. Strip preventively.
	raw = stripDashLineComments(raw)
	// Re-assemble the full SELECT text in PG-friendly order:
	//   SELECT <list> INTO <vars> <FROM/WHERE/...>
	if selectList != "" {
		raw = "SELECT " + selectList + " " + raw
	}
	// Phase 1.9: also try to type the body via the DML parser so the
	// translator can consume a structured *ast.SelectStmt rather than
	// the reassembled text. Failures fall through to the legacy RawQuery
	// path — every existing site still consults RawQuery first, the new
	// Stmt field is purely additive during the transition window.
	stmt, perrs := ParseSelect(raw)
	if len(perrs) > 0 || stmt == nil {
		stmt = nil
	}
	return &ast.SelectInto{Vars: vars, RawQuery: raw, Stmt: stmt, P: astPos(start)}
}

// wrapProjectionsInArrayAgg splits a comma-separated SELECT projection
// list at top-level commas (honouring parens and string literals) and
// wraps each projection in array_agg(...). Used for BULK COLLECT INTO
// to fold the row-set into one PG array per output variable.
//
// Optional column aliases (`expr AS alias` or `expr alias`) are
// preserved AFTER the array_agg wrap, e.g.
//
//	`a, b AS x, count(*) c` → `array_agg(a), array_agg(b) AS x, array_agg(count(*)) c`
//
// Returns the input unchanged when it cannot be cleanly split.
func wrapProjectionsInArrayAgg(list string) string {
	if strings.TrimSpace(list) == "" {
		return list
	}
	// Split at top-level commas.
	parts := []string{}
	depth := 0
	inStr := false
	last := 0
	for i := 0; i < len(list); i++ {
		c := list[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(list) && list[i+1] == '\'' {
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
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, list[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, list[last:])
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Detect a trailing alias: `expr [AS] alias` where alias is the
		// last whitespace-separated token and is a bare identifier.
		expr := p
		alias := ""
		if idx := lastUnquotedSpace(p); idx > 0 {
			tail := strings.TrimSpace(p[idx:])
			head := strings.TrimRightFunc(p[:idx], func(r rune) bool { return r == ' ' || r == '\t' })
			// Detect `expr AS alias` two-word tail.
			if strings.EqualFold(tail, "AS") {
				// keep — bad form, ignore
			} else if isBareIdent(tail) {
				// One-word alias, possibly with optional AS keyword in head.
				if h := strings.TrimRightFunc(head, func(r rune) bool { return r == ' ' || r == '\t' }); strings.HasSuffix(strings.ToUpper(h), " AS") {
					alias = " AS " + tail
					expr = strings.TrimSpace(h[:len(h)-3])
				} else {
					alias = " " + tail
					expr = strings.TrimSpace(head)
				}
			}
		}
		parts[i] = "array_agg(" + expr + ")" + alias
	}
	return strings.Join(parts, ", ")
}

// lastUnquotedSpace returns the index of the last space/tab in s that
// is not inside a single-quoted string literal, or -1 if none.
func lastUnquotedSpace(s string) int {
	inStr := false
	last := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if c == '\'' {
			inStr = true
			continue
		}
		if c == ' ' || c == '\t' {
			last = i
		}
	}
	return last
}

// isBareIdent reports whether s is a non-empty bare SQL identifier
// (letters/digits/underscore/`#`/`$`, not starting with a digit).
func isBareIdent(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '#' || c == '$') {
			return false
		}
	}
	return true
}

// stripDashLineComments removes `--` line comments from s, replacing
// each comment-to-end-of-line span with a single space. Single-quoted
// string literals are honoured (with the doubled-quote `''` escape) so
// `'-- not a comment'` stays intact. Block comments `/* … */` are
// preserved as-is — they're line-safe and the writer's space-joining
// doesn't expose them the way it does `--`.
func stripDashLineComments(s string) string {
	if !strings.Contains(s, "--") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte(s[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		if c == '\'' {
			b.WriteByte(c)
			inStr = true
			continue
		}
		if c == '-' && i+1 < len(s) && s[i+1] == '-' {
			// Skip to end of line (or end of input).
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte(' ') // collapse the comment to whitespace
			if i < len(s) && s[i] == '\n' {
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// peekProceduralSelectInto reports whether the SELECT statement at the
// cursor contains an INTO clause before FROM (i.e. the PL/SQL `SELECT ...
// INTO vars` form). The walk is byte-level on the source rune slice so it
// does not consume tokens. String literals and depth>0 paren groups are
// skipped. `BULK COLLECT INTO` also counts (the SELECT-into-collections
// form): the `BULK COLLECT` keywords are stripped by the route below, so
// the rest of the pipeline sees a regular SELECT INTO.
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
		case "INTO", "BULK":
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

func (p *Parser) parseAssignOrCall(start Position) ast.PLStmt {
	// Capture the identifier chain (possibly qualified) into a target string.
	parts := []string{}
	// Accept a leading bind token (:NEW / :OLD) as a bare identifier. The
	// colon is kept in `name` so downstream regex rewrites (rewriteOraclePLpgSQL)
	// can strip it with a simple ":NEW." → "NEW." replacement.
	if p.cur.Kind == TOK_BIND {
		parts = append(parts, ":"+p.cur.Lit)
		p.advance()
	} else {
		name, _ := p.parseIdent()
		parts = append(parts, name)
	}
	for p.isPunct(".") {
		p.advance()
		n, _ := p.parseIdent()
		parts = append(parts, n)
	}
	// Optional array index: ident(<expr>)[.member] — ambiguous with call.
	// If we see := right here, it's an assignment.
	if p.cur.Kind == TOK_ASSIGN {
		p.advance()
		expr := p.parseExprUntil(";")
		return &ast.AssignStmt{
			Target: strings.Join(parts, "."),
			Expr:   expr,
			P:      astPos(start),
		}
	}
	// Call: possibly with args in parens.
	var args []ast.Expr
	if p.isPunct("(") {
		p.advance()
		if !p.isPunct(")") {
			for {
				e := p.parseExprUntilAnyKeywordOrRange(",", ")")
				args = append(args, e)
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
		}
		p.expectPunct(")")
	}
	// Could still be an assignment to a collection element:
	//   Oracle  `v(i) := expr`
	//   PG      `v[i] := expr`
	// We already parsed the inner-paren content as `args` above; use
	// those expressions to build the bracket-indexed target. Without
	// this, the placeholder `(...)` we used to emit became the literal
	// substring `[...]` after the post-pass collection element rewrite
	// — and PG raised `syntax error at or near ".."`.
	if p.cur.Kind == TOK_ASSIGN {
		p.advance()
		expr := p.parseExprUntil(";")
		target := strings.Join(parts, ".")
		if len(args) > 0 {
			idx := make([]string, len(args))
			for i, a := range args {
				idx[i] = rawExprText(a)
			}
			target += "[" + strings.Join(idx, ", ") + "]"
		}
		return &ast.AssignStmt{
			Target: target,
			Expr:   expr,
			P:      astPos(start),
		}
	}
	schema := ""
	n := parts[0]
	if len(parts) > 1 {
		schema = parts[0]
		n = strings.Join(parts[1:], ".")
	}
	return &ast.CallStmt{Schema: schema, Name: n, Args: args, P: astPos(start)}
}

// ---------------------------------------------------------------------------
// Statement-list helpers
// ---------------------------------------------------------------------------

func (p *Parser) parseStmtsUntilAny(stops ...string) []ast.PLStmt {
	var out []ast.PLStmt
	for p.cur.Kind != TOK_EOF {
		if p.cur.Kind == TOK_KEYWORD {
			matched := false
			for _, s := range stops {
				if p.cur.Lit == s {
					matched = true
					break
				}
			}
			if matched {
				return out
			}
		}
		if stmt := p.parsePLStmt(); stmt != nil {
			out = append(out, stmt)
		}
		if p.isPunct(";") {
			p.advance()
		}
	}
	return out
}

// parseExprUntilSemi / parseExprUntilKeyword / parseExprUntilRange capture a
// raw expression slice.
func (p *Parser) parseExprUntilSemi() ast.Expr {
	return p.parseExprUntilAnyKeywordOrRange(";")
}

func (p *Parser) parseExprUntilKeyword(stops ...string) ast.Expr {
	return p.parseExprUntilAnyKeywordOrRange(stops...)
}

func (p *Parser) parseExprUntilRange() ast.Expr {
	start := p.cur.Pos
	startOff := p.cur.Pos.Offset
	depth := 0
	for p.cur.Kind != TOK_EOF {
		if p.isPunct("(") {
			depth++
			p.advance()
			continue
		}
		if p.isPunct(")") {
			if depth == 0 {
				break
			}
			depth--
			p.advance()
			continue
		}
		if depth == 0 && p.cur.Kind == TOK_RANGE {
			break
		}
		p.advance()
	}
	return &ast.RawExpr{Text: strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset])), P: astPos(start)}
}

// parseExprUntilAnyKeywordOrRange captures a raw expr until any of the stop
// tokens or EOF. Stops include keywords (matched against current KEYWORD.Lit)
// and literal punctuation characters (";", ",", ")").
func (p *Parser) parseExprUntilAnyKeywordOrRange(stops ...string) ast.Expr {
	start := p.cur.Pos
	startOff := p.cur.Pos.Offset
	depth := 0
	for p.cur.Kind != TOK_EOF {
		if p.isPunct("(") {
			depth++
			p.advance()
			continue
		}
		if p.isPunct(")") {
			if depth == 0 {
				break
			}
			depth--
			p.advance()
			continue
		}
		if depth == 0 {
			// Always break on the Oracle range token `..` so callers
			// downstream can branch on `cur.Kind == TOK_RANGE` to tell
			// numeric FOR (`FOR i IN 1..N LOOP`) apart from cursor FOR
			// (`FOR rec IN cur LOOP`). Without this stop the expression
			// captures `1..N` whole, leaves `cur` at LOOP, and the
			// numeric branch never fires — the entire loop body is then
			// dropped from the AST and an `EXIT;` from inside it
			// surfaces as a top-level "EXIT cannot be used outside a
			// loop" error in PG.
			if p.cur.Kind == TOK_RANGE {
				return &ast.RawExpr{Text: strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset])), P: astPos(start)}
			}
			for _, s := range stops {
				if s == ";" && p.isPunct(";") {
					return &ast.RawExpr{Text: strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset])), P: astPos(start)}
				}
				if s == "," && p.isPunct(",") {
					return &ast.RawExpr{Text: strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset])), P: astPos(start)}
				}
				if s == ")" && p.isPunct(")") {
					return &ast.RawExpr{Text: strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset])), P: astPos(start)}
				}
				if p.cur.Kind == TOK_KEYWORD && p.cur.Lit == s {
					return &ast.RawExpr{Text: strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset])), P: astPos(start)}
				}
			}
		}
		p.advance()
	}
	return &ast.RawExpr{Text: strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset])), P: astPos(start)}
}

// captureUntilSemi reads verbatim up to the next ';' (or statement end).
func (p *Parser) captureUntilSemi() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	for p.cur.Kind != TOK_EOF {
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
		}
		if depth == 0 && (p.isPunct(";") || p.cur.Kind == TOK_SLASH_TERM) {
			break
		}
		p.advance()
	}
	return strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
}

// rawExprText extracts the Text field of a RawExpr (or empty).
// rawExprText extracts a flat textual rendering of e for the few sites
// (CursorForStmt.CursorName, OpenStmt arg list, …) that need a string.
// Handles RawExpr verbatim plus the small set of typed Expr nodes the
// parser now produces at those sites — Ident and FuncCall — so the
// migration of parseExprUntilSemi/Keyword to parseExprUntil does not
// regress callers that still expect a string.
func rawExprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.RawExpr:
		return x.Text
	case *ast.Ident:
		return strings.Join(x.Parts, ".")
	case *ast.FuncCall:
		args := make([]string, 0, len(x.Args))
		for _, a := range x.Args {
			args = append(args, rawExprText(a))
		}
		return x.Name + "(" + strings.Join(args, ", ") + ")"
	case *ast.Literal:
		return x.Text
	case *ast.ParenExpr:
		return "(" + rawExprText(x.Inner) + ")"
	}
	return ""
}
