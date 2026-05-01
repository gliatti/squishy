package db2

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// CREATE — top-level dispatch on the object kind following CREATE [OR REPLACE]
// ---------------------------------------------------------------------------

func (p *Parser) parseCreate() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("CREATE")

	orReplace := false
	if p.isKw("OR") {
		p.advance()
		p.expectKw("REPLACE")
		orReplace = true
	}

	// Modifier qualifiers that may sit between CREATE and the object kind.
	isUnique := false
	isGlobalTemp := false
	isCluster := false
	for {
		switch {
		case p.isKw("UNIQUE"):
			p.advance()
			isUnique = true
			continue
		case p.isKw("GLOBAL"):
			p.advance()
			if p.isKw("TEMPORARY") {
				p.advance()
				isGlobalTemp = true
				continue
			}
		case p.isKw("CLUSTER"):
			p.advance()
			isCluster = true
			continue
		case p.isAnyKw("LARGE", "REGULAR", "SYSTEM"):
			// CREATE LARGE TABLESPACE / SYSTEM TEMPORARY TABLESPACE etc. —
			// captured downstream as Noop (storage-side, no PG counterpart).
			p.advance()
			continue
		}
		break
	}

	switch {
	case p.isKw("TABLE"):
		s := p.parseCreateTable(start)
		if t, ok := s.(*ast.CreateTable); ok {
			t.OrReplace = orReplace
			t.Temporary = isGlobalTemp
		}
		return s
	case p.isKw("INDEX"):
		return p.parseCreateIndex(start, isUnique, isCluster)
	case p.isKw("VIEW"):
		return p.parseCreateView(start, orReplace)
	case p.isKw("SEQUENCE"):
		return p.parseCreateSequence(start, orReplace)
	case p.isKw("TRIGGER"):
		return p.parseCreateTrigger(start, orReplace)
	case p.isKw("PROCEDURE"):
		return p.parseCreateProcedure(start, orReplace)
	case p.isKw("FUNCTION"):
		return p.parseCreateFunction(start, orReplace)
	case p.isKw("ALIAS"), p.isKw("SYNONYM"):
		return p.parseCreateAlias(start, orReplace)
	case p.isKw("TYPE"):
		// CREATE [DISTINCT] TYPE — captured raw (PG distinct types =
		// CREATE DOMAIN; arrays/structured types need translator-side
		// emission). For now, NoopStmt + downstream warning.
		return p.parseNoopToStmtEnd("CREATE TYPE")
	case p.isKw("MODULE"), p.isKw("PACKAGE"):
		return p.parseNoopToStmtEnd("CREATE " + p.cur.Lit)
	case p.isKw("TABLESPACE"), p.isKw("BUFFERPOOL"), p.isKw("STOGROUP"),
		p.isKw("DATABASE"), p.isKw("SCHEMA"), p.isKw("ROLE"), p.isKw("USER"),
		p.isKw("AUXILIARY"):
		return p.parseNoopToStmtEnd("CREATE " + p.cur.Lit)
	}
	p.errorHere("unsupported CREATE variant",
		"TABLE|INDEX|VIEW|SEQUENCE|TRIGGER|PROCEDURE|FUNCTION|ALIAS|TYPE|MODULE")
	return nil
}

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateTable(start Position) ast.Stmt {
	p.expectKw("TABLE")

	t := &ast.CreateTable{P: astPos(start)}

	if p.isKw("IF") {
		p.advance()
		p.expectKw("NOT")
		p.expectKw("EXISTS")
		t.IfNotExists = true
	}

	t.Schema, t.Name = p.parseQualifiedName()

	// (col-def, …) [table-options]
	if !p.expectPunct("(") {
		return t
	}
	for {
		// Look for a TABLE constraint (PRIMARY KEY / UNIQUE / FOREIGN KEY /
		// CHECK / CONSTRAINT name …) before treating the entry as a column.
		if p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") ||
			p.isKw("FOREIGN") || p.isKw("CHECK") {
			if cons := p.parseTableConstraint(); cons != nil {
				t.Constraints = append(t.Constraints, cons)
			}
		} else {
			col := p.parseColumnDef()
			if col != nil {
				t.Columns = append(t.Columns, col)
			}
		}
		if p.isPunct(",") {
			p.advance()
			continue
		}
		break
	}
	p.expectPunct(")")

	// Trailing table-level options (storage / partitioning / …) are
	// captured raw into TableOptions for diagnostic surfacing.
	p.parseTableTrailingOptions(&t.Options)

	return t
}

func (p *Parser) parseColumnDef() *ast.ColumnDef {
	colStart := p.cur.Pos
	name, _ := p.parseIdent()
	col := &ast.ColumnDef{Name: name, P: astPos(colStart)}
	col.Type = p.parseDataType()

	for {
		switch {
		case p.isKw("NOT"):
			p.advance()
			p.expectKw("NULL")
			col.NotNull = true
			col.HasNullable = true
		case p.isKw("NULL"):
			p.advance()
			col.NotNull = false
			col.HasNullable = true
		case p.isKw("DEFAULT"):
			p.advance()
			col.HasDefault = true
			col.Default = p.parseDefaultExpr()
		case p.isKw("WITH"):
			// `WITH DEFAULT [expr]` — DB2 column shorthand, equivalent
			// to DEFAULT.
			p.advance()
			if p.isKw("DEFAULT") {
				p.advance()
				col.HasDefault = true
				if !p.atColumnEnd() {
					col.Default = p.parseDefaultExpr()
				}
			}
		case p.isKw("GENERATED"):
			p.advance()
			// ALWAYS|BY DEFAULT
			if p.isKw("ALWAYS") {
				p.advance()
			} else if p.isKw("BY") {
				p.advance()
				p.expectKw("DEFAULT")
			}
			if p.isKw("AS") {
				p.advance()
				if p.isKw("IDENTITY") {
					p.advance()
					col.AutoInc = true
					// Optional `(START WITH n INCREMENT BY n …)` clause —
					// captured raw and discarded; the translator emits a PG
					// IDENTITY default.
					if p.isPunct("(") {
						_ = p.captureBalancedParens()
					}
				} else {
					// GENERATED ALWAYS AS (expr) — virtual column.
					expr := p.parseDefaultExpr()
					col.Generated = &ast.Generated{Expr: expr}
				}
			}
		case p.isKw("PRIMARY"):
			p.advance()
			p.expectKw("KEY")
			col.PrimaryKey = true
		case p.isKw("UNIQUE"):
			p.advance()
			col.Unique = true
		case p.isKw("REFERENCES"):
			// Inline FK reference — capture target then optional action
			// clauses; expose as a column-level FK via the parent.
			p.advance()
			_, _ = p.parseQualifiedName()
			if p.isPunct("(") {
				_ = p.captureBalancedParens()
			}
			for p.isKw("ON") {
				p.advance()
				p.advance() // DELETE | UPDATE
				if p.isKw("NO") {
					p.advance()
					p.expectKw("ACTION")
				} else if p.isKw("CASCADE") || p.isKw("RESTRICT") || p.isKw("SET") {
					p.advance()
					if p.isKw("NULL") || p.isKw("DEFAULT") {
						p.advance()
					}
				}
			}
		case p.isKw("CHECK"):
			p.advance()
			if p.isPunct("(") {
				_ = p.captureBalancedParens()
			}
		case p.isKw("CONSTRAINT"):
			// inline named constraint — DB2 allows CONSTRAINT name CHECK/UNIQUE/REFERENCES.
			p.advance()
			_, _ = p.parseIdent()
			continue
		case p.isKw("INLINE"):
			// INLINE LENGTH n (LOB columns) — strip silently.
			p.advance()
			if p.isKw("LENGTH") {
				p.advance()
			}
			if p.cur.Kind == TOK_NUMBER {
				p.advance()
			}
		case p.isKw("COMPRESS"), p.isKw("LOGGED"):
			p.advance()
		case p.isKw("NOT_LOGGED"):
			p.advance()
		case p.isKw("IMPLICITLY"):
			// IMPLICITLY HIDDEN — captured silently.
			p.advance()
			if p.isKw("HIDDEN") {
				p.advance()
			}
		default:
			return col
		}
	}
}

// atColumnEnd reports whether the parser sits on a token that ends a column
// definition (comma, close-paren, or terminator).
func (p *Parser) atColumnEnd() bool {
	return p.isPunct(",") || p.isPunct(")") || p.atStatementEnd()
}

// parseDefaultExpr captures a default expression as a literal. DB2 default
// expressions can include function calls, CURRENT_DATE / CURRENT_TIMESTAMP,
// arithmetic. We capture them raw into ast.RawExpr and let the translator
// pass the text through.
func (p *Parser) parseDefaultExpr() ast.Expr {
	startOff := p.cur.Pos.Offset
	depth := 0
	for {
		if depth == 0 && (p.atColumnEnd() ||
			p.isKw("NOT") || p.isKw("NULL") || p.isKw("PRIMARY") ||
			p.isKw("UNIQUE") || p.isKw("REFERENCES") || p.isKw("CHECK") ||
			p.isKw("GENERATED") || p.isKw("CONSTRAINT") || p.isKw("WITH") ||
			p.isKw("INLINE") || p.isKw("COMPRESS") || p.isKw("LOGGED") ||
			p.isKw("NOT_LOGGED") || p.isKw("IMPLICITLY")) {
			break
		}
		if p.cur.Kind == TOK_EOF {
			break
		}
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			if depth == 0 {
				break
			}
			depth--
		}
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	text := strings.TrimSpace(string(p.src[startOff:endOff]))
	return &ast.RawExpr{Text: text}
}

// parseTableConstraint reads a table-level constraint and returns the
// matching AST constraint. Returns nil on failure.
func (p *Parser) parseTableConstraint() ast.TableConstraint {
	var name string
	if p.isKw("CONSTRAINT") {
		p.advance()
		name, _ = p.parseIdent()
	}
	switch {
	case p.isKw("PRIMARY"):
		p.advance()
		p.expectKw("KEY")
		cols := p.parseIndexedColList()
		return &ast.PKConstraint{Name: name, Columns: cols}
	case p.isKw("UNIQUE"):
		p.advance()
		cols := p.parseIndexedColList()
		return &ast.UQConstraint{Name: name, Columns: cols}
	case p.isKw("FOREIGN"):
		p.advance()
		p.expectKw("KEY")
		cols := p.parseColumnList()
		p.expectKw("REFERENCES")
		fk := &ast.FKConstraint{Name: name, Columns: cols}
		fk.RefSchema, fk.RefTable = p.parseQualifiedName()
		if p.isPunct("(") {
			fk.RefColumns = p.parseColumnList()
		}
		for p.isKw("ON") {
			p.advance()
			action := p.cur.Lit
			p.advance() // DELETE|UPDATE
			refAct := ""
			if p.isKw("NO") {
				p.advance()
				p.expectKw("ACTION")
				refAct = "NO ACTION"
			} else if p.isKw("CASCADE") {
				p.advance()
				refAct = "CASCADE"
			} else if p.isKw("RESTRICT") {
				p.advance()
				refAct = "RESTRICT"
			} else if p.isKw("SET") {
				p.advance()
				if p.isKw("NULL") {
					p.advance()
					refAct = "SET NULL"
				} else if p.isKw("DEFAULT") {
					p.advance()
					refAct = "SET DEFAULT"
				}
			}
			if action == "DELETE" {
				fk.OnDelete = refAct
			} else if action == "UPDATE" {
				fk.OnUpdate = refAct
			}
		}
		// Optional NOT ENFORCED — drop with a warning at translate time.
		if p.isKw("NOT") {
			p.advance()
			if p.isKw("ENFORCED") {
				p.advance()
			}
		}
		return fk
	case p.isKw("CHECK"):
		p.advance()
		if !p.isPunct("(") {
			p.errorHere("expected '('", "(")
			return nil
		}
		exprText := p.captureBalancedParens()
		return &ast.CheckConstraint{Name: name, Expr: &ast.RawExpr{Text: exprText}}
	}
	p.errorHere("unsupported table constraint", "PRIMARY|UNIQUE|FOREIGN|CHECK")
	return nil
}

// parseIndexedColList parses `(col [ASC|DESC] [, …])` returning IndexedCol
// entries — used by PK/UQ constraints which carry index-column shape.
func (p *Parser) parseIndexedColList() []ast.IndexedCol {
	if !p.expectPunct("(") {
		return nil
	}
	var cols []ast.IndexedCol
	for {
		name, _ := p.parseIdent()
		ic := ast.IndexedCol{Name: name}
		if p.isKw("ASC") {
			ic.Order = "ASC"
			p.advance()
		} else if p.isKw("DESC") {
			ic.Order = "DESC"
			p.advance()
		}
		cols = append(cols, ic)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return cols
}

// parseColumnList parses `(col [, col]…)` returning the bare names.
func (p *Parser) parseColumnList() []string {
	if !p.expectPunct("(") {
		return nil
	}
	var cols []string
	for {
		name, _ := p.parseIdent()
		cols = append(cols, name)
		// optional ASC/DESC trailing each column (index DDL)
		if p.isKw("ASC") || p.isKw("DESC") {
			p.advance()
		}
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return cols
}

// parseTableTrailingOptions slurps up DB2 table-level options that follow
// the closing `)` of the column list. Most have no PG counterpart and are
// captured into TableOptions.DB2* for diagnostic surfacing.
func (p *Parser) parseTableTrailingOptions(opts *ast.TableOptions) {
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("IN"):
			p.advance()
			tablespace, _ := p.parseIdent()
			opts.DB2Tablespace = tablespace
		case p.isKw("INDEX"):
			p.advance()
			if p.isKw("IN") {
				p.advance()
				tablespace, _ := p.parseIdent()
				opts.DB2IndexInTablespace = tablespace
			}
		case p.isKw("LONG"):
			p.advance()
			if p.isKw("IN") {
				p.advance()
				tablespace, _ := p.parseIdent()
				opts.DB2LongInTablespace = tablespace
			}
		case p.isKw("COMPRESS"):
			p.advance()
			if p.isKw("YES") || (p.cur.Kind == TOK_IDENT && p.cur.Lit == "YES") {
				p.advance()
				opts.DB2CompressionYES = true
				if p.isKw("STATIC") || p.isKw("ADAPTIVE") {
					opts.DB2CompressionMethod = p.cur.Lit
					p.advance()
				}
			} else if p.isKw("NO") || (p.cur.Kind == TOK_IDENT && p.cur.Lit == "NO") {
				p.advance()
			}
		case p.isKw("ORGANIZE"):
			p.advance()
			if p.isKw("BY") {
				p.advance()
				// ROW | COLUMN | KEY SEQUENCE | …
				kind := p.cur.Lit
				p.advance()
				if p.isKw("SEQUENCE") {
					kind += " SEQUENCE"
					p.advance()
					if p.isPunct("(") {
						_ = p.captureBalancedParens()
					}
				}
				opts.DB2OrganizeBy = kind
			}
		case p.isKw("PARTITION"):
			p.advance()
			if p.isKw("BY") {
				p.advance()
				if p.isKw("RANGE") {
					p.advance()
				}
				// Capture column list + partition spec raw.
				start := p.cur.Pos.Offset
				if p.isPunct("(") {
					_ = p.captureBalancedParens()
				}
				if p.isPunct("(") {
					_ = p.captureBalancedParens()
				}
				opts.DB2PartitionByRange = strings.TrimSpace(string(p.src[start:p.cur.Pos.Offset]))
			}
		case p.isKw("DISTRIBUTE"):
			p.advance()
			if p.isKw("BY") {
				p.advance()
				start := p.cur.Pos.Offset
				if p.isKw("HASH") {
					p.advance()
					if p.isPunct("(") {
						_ = p.captureBalancedParens()
					}
				} else if p.isKw("REPLICATION") {
					p.advance()
				}
				opts.DB2DistributeBy = strings.TrimSpace(string(p.src[start:p.cur.Pos.Offset]))
			}
		case p.isKw("DATA"):
			p.advance()
			if p.isKw("CAPTURE") {
				p.advance()
				if p.isKw("CHANGES") || p.isKw("NONE") {
					opts.DB2DataCapture = p.cur.Lit
					p.advance()
				}
			}
		case p.isKw("VOLATILE"):
			p.advance()
			opts.DB2VolatileCardinality = true
			if p.isKw("CARDINALITY") {
				p.advance()
			}
		case p.isKw("NOT_LOGGED"), p.isKw("LOGGED"):
			if p.cur.Lit == "NOT_LOGGED" {
				opts.DB2NotLoggedInitially = true
			}
			p.advance()
			if p.isKw("INITIALLY") {
				p.advance()
			}
		case p.isKw("VALUE"):
			p.advance()
			if p.isKw("COMPRESSION") {
				p.advance()
				opts.DB2InRow = true
			}
		case p.isPunct(","):
			p.advance()
		default:
			// Unknown option — bail out of the loop and let the statement
			// terminator resolve.
			return
		}
	}
}

// ---------------------------------------------------------------------------
// CREATE INDEX
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateIndex(start Position, isUnique bool, isCluster bool) ast.Stmt {
	p.expectKw("INDEX")
	idx := &ast.CreateIndex{Unique: isUnique, P: astPos(start)}
	_ = isCluster // captured via the dialect — PG has no CLUSTER index, dropped silently
	schema, name := p.parseQualifiedName()
	if schema != "" {
		idx.Name = schema + "." + name
	} else {
		idx.Name = name
	}
	p.expectKw("ON")
	idx.Table = p.parseTableRef()
	if p.isPunct("(") {
		p.advance()
		for {
			cname, _ := p.parseIdent()
			ic := ast.IndexedCol{Name: cname}
			if p.isKw("ASC") {
				ic.Order = "ASC"
				p.advance()
			} else if p.isKw("DESC") {
				ic.Order = "DESC"
				p.advance()
			}
			idx.Columns = append(idx.Columns, ic)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
	}
	// INCLUDE (…) — covered columns. Captured raw and dropped for now (PG
	// supports INCLUDE on B-tree, can be added downstream).
	if p.isKw("INCLUDE") {
		p.advance()
		if p.isPunct("(") {
			_ = p.captureBalancedParens()
		}
	}
	// Trailing options: ALLOW REVERSE SCANS, CLUSTER, COMPRESS YES, NOT
	// PARTITIONED, PAGE SPLIT, …
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	return idx
}

// ---------------------------------------------------------------------------
// CREATE VIEW
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateView(start Position, orReplace bool) ast.Stmt {
	p.expectKw("VIEW")
	v := &ast.CreateView{OrReplace: orReplace, P: astPos(start)}
	schema, name := p.parseQualifiedName()
	v.View = ast.TableRef{Schema: schema, Name: name}
	if p.isPunct("(") {
		p.advance()
		for {
			n, _ := p.parseIdent()
			v.Columns = append(v.Columns, n)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
	}
	p.expectKw("AS")
	// Capture the SELECT body raw, stopping at WITH … CHECK OPTION or end.
	startOff := p.cur.Pos.Offset
	endOff := startOff
	depth := 0
	for {
		if p.cur.Kind == TOK_EOF {
			break
		}
		if depth == 0 && p.isKw("WITH") {
			// Look ahead: does this WITH start a CHECK OPTION clause?
			pk := p.peek()
			if pk.Kind == TOK_KEYWORD && (pk.Lit == "CHECK" || pk.Lit == "LOCAL" || pk.Lit == "CASCADED") {
				endOff = p.cur.Pos.Offset
				break
			}
		}
		if depth == 0 && p.atStatementEnd() {
			endOff = p.cur.Pos.Offset
			break
		}
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
		}
		p.advance()
	}
	v.SelectBody = strings.TrimSpace(string(p.src[startOff:endOff]))

	if p.isKw("WITH") {
		p.advance()
		// LOCAL | CASCADED prefix optional
		switch {
		case p.isKw("LOCAL"):
			p.advance()
			p.expectKw("CHECK")
			p.expectKw("OPTION")
			v.CheckOption = "LOCAL CHECK OPTION"
		case p.isKw("CASCADED"):
			p.advance()
			p.expectKw("CHECK")
			p.expectKw("OPTION")
			v.CheckOption = "CASCADED CHECK OPTION"
		case p.isKw("CHECK"):
			p.advance()
			p.expectKw("OPTION")
			v.CheckOption = "CHECK OPTION"
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// CREATE SEQUENCE
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateSequence(start Position, orReplace bool) ast.Stmt {
	p.expectKw("SEQUENCE")
	s := &ast.CreateSequence{OrReplace: orReplace, P: astPos(start)}
	if p.isKw("IF") {
		p.advance()
		p.expectKw("NOT")
		p.expectKw("EXISTS")
		s.IfNotExists = true
	}
	s.Schema, s.Name = p.parseQualifiedName()
	// Optional `AS <data-type>` (DB2 allows a typed sequence base).
	if p.isKw("AS") {
		p.advance()
		_ = p.parseDataType() // discard — PG sequences are bigint-backed
	}
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("START"):
			p.advance()
			if p.isKw("WITH") {
				p.advance()
			}
			if v, ok := p.intLit(); ok {
				s.Start = v
				s.HasStart = true
			}
		case p.isKw("INCREMENT"):
			p.advance()
			if p.isKw("BY") {
				p.advance()
			}
			if v, ok := p.intLit(); ok {
				s.Increment = v
				s.HasIncr = true
			}
		case p.isKw("MAXVALUE"):
			p.advance()
			if v, ok := p.intLit(); ok {
				s.MaxValue = v
				s.HasMax = true
			}
		case p.isKw("NOMAXVALUE"), p.isKwOrIdent("NO_MAXVALUE"):
			p.advance()
			s.NoMax = true
		case p.isKw("NO"):
			p.advance()
			switch {
			case p.isKw("MAXVALUE"):
				p.advance()
				s.NoMax = true
			case p.isKw("MINVALUE"):
				p.advance()
				s.NoMin = true
			case p.isKw("CYCLE"):
				p.advance()
				s.HasCycle = true
				s.Cycle = false
			case p.isKw("CACHE"):
				p.advance()
				s.NoCache = true
			case p.isKw("ORDER"):
				p.advance()
				// PG has no ordering guarantee — silently dropped.
			}
		case p.isKw("MINVALUE"):
			p.advance()
			if v, ok := p.intLit(); ok {
				s.MinValue = v
				s.HasMin = true
			}
		case p.isKw("NOMINVALUE"):
			p.advance()
			s.NoMin = true
		case p.isKw("CYCLE"):
			p.advance()
			s.Cycle = true
			s.HasCycle = true
		case p.isKw("NOCYCLE"):
			p.advance()
			s.HasCycle = true
			s.Cycle = false
		case p.isKw("CACHE"):
			p.advance()
			if v, ok := p.intLit(); ok {
				s.Cache = v
				s.HasCache = true
			}
		case p.isKw("NOCACHE"):
			p.advance()
			s.NoCache = true
		case p.isKw("ORDER"):
			p.advance()
			// dropped
		default:
			p.advance()
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// CREATE PROCEDURE / FUNCTION / TRIGGER — header parse + body raw capture
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateProcedure(start Position, _ bool) ast.Stmt {
	p.expectKw("PROCEDURE")
	s := &ast.CreateProcedure{P: astPos(start)}
	schema, name := p.parseQualifiedName()
	if schema != "" {
		s.Name = schema + "." + name
	} else {
		s.Name = name
	}
	// Param list (in/out/inout col TYPE, …) — captured raw for now.
	if p.isPunct("(") {
		_ = p.captureBalancedParens()
	}
	// Routine attributes prologue.
	p.skipRoutineAttrs(&s.Characteristics)
	// Body. DB2 SQL PL: the body is a single SQL PL statement, typically
	// `BEGIN [ATOMIC] … END` but possibly a single CALL or compound stmt.
	if p.isKw("BEGIN") {
		s.Body = p.captureBlock()
	} else {
		s.Body = p.captureUntilDelimiter()
	}
	return s
}

func (p *Parser) parseCreateFunction(start Position, _ bool) ast.Stmt {
	p.expectKw("FUNCTION")
	s := &ast.CreateFunction{P: astPos(start)}
	schema, name := p.parseQualifiedName()
	if schema != "" {
		s.Name = schema + "." + name
	} else {
		s.Name = name
	}
	if p.isPunct("(") {
		_ = p.captureBalancedParens()
	}
	if p.isKw("RETURNS") {
		p.advance()
		// RETURNS [TABLE (...)] | <data-type>
		if p.isKw("TABLE") {
			p.advance()
			if p.isPunct("(") {
				_ = p.captureBalancedParens()
			}
		} else {
			s.Returns = p.parseDataType()
		}
	}
	p.skipRoutineAttrs(&s.Characteristics)
	if p.isKw("BEGIN") {
		s.Body = p.captureBlock()
	} else if p.isKw("RETURN") {
		s.Body = p.captureUntilDelimiter()
	} else {
		s.Body = p.captureUntilDelimiter()
	}
	return s
}

// skipRoutineAttrs consumes the routine attribute prologue that DB2 places
// between the parameter list / RETURNS clause and the body. It handles
// common attributes (LANGUAGE SQL, DETERMINISTIC, NO SQL / READS SQL DATA /
// MODIFIES SQL DATA / CONTAINS SQL, SPECIFIC name, INHERIT SPECIAL
// REGISTERS, EXTERNAL ACTION, PARAMETER STYLE …) and stops on the first
// keyword that begins the body (BEGIN, RETURN) or a statement terminator.
func (p *Parser) skipRoutineAttrs(rc *ast.RoutineCharacteristics) {
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("LANGUAGE"):
			p.advance()
			lang, _ := p.parseIdent()
			rc.Language = strings.ToUpper(lang)
		case p.isKw("DETERMINISTIC"):
			p.advance()
			rc.Deterministic = true
			rc.HasDeterministic = true
		case p.isKw("NOT"):
			p.advance()
			if p.isKw("DETERMINISTIC") {
				p.advance()
				rc.Deterministic = false
				rc.HasDeterministic = true
			}
		case p.isKw("READS"):
			p.advance()
			if p.isKw("SQL") {
				p.advance()
			}
			if p.isKw("DATA") {
				p.advance()
			}
			rc.SQLDataAccess = "READS SQL DATA"
		case p.isKw("MODIFIES"):
			p.advance()
			if p.isKw("SQL") {
				p.advance()
			}
			if p.isKw("DATA") {
				p.advance()
			}
			rc.SQLDataAccess = "MODIFIES SQL DATA"
		case p.isKw("CONTAINS"):
			p.advance()
			if p.isKw("SQL") {
				p.advance()
			}
			rc.SQLDataAccess = "CONTAINS SQL"
		case p.isKw("NO"):
			p.advance()
			if p.isKw("SQL") {
				p.advance()
				rc.SQLDataAccess = "NO SQL"
			}
		case p.isKw("SPECIFIC"):
			p.advance()
			_, _ = p.parseIdent()
		case p.isKw("INHERIT"):
			// INHERIT SPECIAL REGISTERS — skip the next two keywords.
			p.advance()
			if p.isKw("SPECIAL") {
				p.advance()
			}
			if p.isKw("REGISTERS") {
				p.advance()
			}
		case p.isKw("EXTERNAL"):
			p.advance()
			if p.isKw("ACTION") {
				p.advance()
			}
		case p.isKw("PARAMETER"):
			p.advance()
			if p.isKw("STYLE") {
				p.advance()
				_, _ = p.parseIdent()
			}
		case p.isKw("FENCED"), p.isKw("THREADSAFE"), p.isKw("VARIANT"):
			p.advance()
		case p.isKw("BEGIN"), p.isKw("RETURN"):
			return
		default:
			// Unknown attribute → stop and let the body capture fire.
			return
		}
	}
}

func (p *Parser) parseCreateTrigger(start Position, _ bool) ast.Stmt {
	p.expectKw("TRIGGER")
	tr := &ast.CreateTrigger{P: astPos(start)}
	schema, name := p.parseQualifiedName()
	if schema != "" {
		tr.Name = schema + "." + name
	} else {
		tr.Name = name
	}
	// timing
	switch {
	case p.isKw("BEFORE"):
		p.advance()
		tr.Time = "BEFORE"
	case p.isKw("AFTER"):
		p.advance()
		tr.Time = "AFTER"
	case p.isKw("INSTEAD"):
		p.advance()
		p.expectKw("OF")
		tr.Time = "INSTEAD OF"
	}
	// event(s) — DB2 supports `INSERT OR UPDATE [OF cols] OR DELETE` chains.
	var events []string
	for {
		if p.isKw("INSERT") || p.isKw("UPDATE") || p.isKw("DELETE") {
			ev := p.cur.Lit
			p.advance()
			if ev == "UPDATE" && p.isKw("OF") {
				p.advance()
				_ = p.parseColumnListLoose()
			}
			events = append(events, ev)
		}
		if p.isKw("OR") {
			p.advance()
			continue
		}
		break
	}
	tr.Event = strings.Join(events, " OR ")
	if p.isKw("ON") {
		p.advance()
		tr.Table = p.parseTableRef()
	}
	if p.isKw("REFERENCING") {
		p.advance()
		// NEW [ROW] AS n / OLD [ROW] AS o / NEW_TABLE AS nt / OLD_TABLE AS ot
		for {
			switch {
			case p.isKw("NEW"):
				p.advance()
				if p.isKw("ROW") || p.isKw("AS") {
					if p.isKw("ROW") {
						p.advance()
					}
					if p.isKw("AS") {
						p.advance()
					}
				}
				if p.cur.Kind == TOK_IDENT {
					tr.NewAlias = p.cur.Lit
					p.advance()
				}
			case p.isKw("OLD"):
				p.advance()
				if p.isKw("ROW") {
					p.advance()
				}
				if p.isKw("AS") {
					p.advance()
				}
				if p.cur.Kind == TOK_IDENT {
					tr.OldAlias = p.cur.Lit
					p.advance()
				}
			case p.isKw("NEW_TABLE"), p.isKw("OLD_TABLE"):
				p.advance()
				if p.isKw("AS") {
					p.advance()
				}
				if p.cur.Kind == TOK_IDENT {
					p.advance()
				}
			default:
				goto refDone
			}
		}
	refDone:
	}
	if p.isKw("FOR") {
		p.advance()
		if p.isKw("EACH") {
			p.advance()
			if p.isKw("ROW") {
				p.advance()
				tr.ForEachRow = true
				tr.HasForEach = true
			} else if p.isKw("STATEMENT") {
				p.advance()
				tr.HasForEach = true
			}
		}
	}
	if p.isKw("MODE") {
		p.advance()
		if p.isKwOrIdent("DB2SQL") {
			p.advance()
		}
	}
	if p.isKw("WHEN") {
		p.advance()
		if p.isPunct("(") {
			tr.WhenCond = p.captureBalancedParens()
		}
	}
	// body
	if p.isKw("BEGIN") {
		tr.Body = p.captureBlock()
	} else {
		tr.Body = p.captureUntilDelimiter()
	}
	return tr
}

// parseColumnListLoose parses `(c1[, c2…])` with permissive ident handling
// and discards the result. Returns nothing.
func (p *Parser) parseColumnListLoose() []string {
	if !p.isPunct("(") {
		// DB2 allows `OF c1, c2` without parentheses — capture until next clause.
		var cols []string
		for {
			if p.cur.Kind != TOK_IDENT && p.cur.Kind != TOK_KEYWORD {
				break
			}
			cols = append(cols, p.cur.Lit)
			p.advance()
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		return cols
	}
	return p.parseColumnList()
}

// ---------------------------------------------------------------------------
// CREATE ALIAS / SYNONYM
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateAlias(start Position, _ bool) ast.Stmt {
	kind := p.cur.Lit // ALIAS or SYNONYM
	p.advance()
	// `<alias-name> FOR <target-name>` — captured into a NoopStmt because PG
	// has no native alias; the translator emits a `CREATE VIEW v AS SELECT
	// * FROM target` equivalent at translation time.
	startOff := start.Offset
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	return &ast.NoopStmt{
		Kind: "CREATE " + kind,
		Text: strings.TrimSpace(string(p.src[startOff:endOff])),
		P:    astPos(start),
	}
}

// ---------------------------------------------------------------------------
// ALTER
// ---------------------------------------------------------------------------

func (p *Parser) parseAlter() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("ALTER")
	switch {
	case p.isKw("TABLE"):
		return p.parseAlterTable(start)
	case p.isKw("SEQUENCE"):
		return p.parseNoopToStmtEnd("ALTER SEQUENCE")
	case p.isKw("INDEX"):
		return p.parseNoopToStmtEnd("ALTER INDEX")
	case p.isKw("VIEW"):
		return p.parseNoopToStmtEnd("ALTER VIEW")
	case p.isKw("PROCEDURE"), p.isKw("FUNCTION"), p.isKw("TRIGGER"),
		p.isKw("TYPE"), p.isKw("MODULE"), p.isKw("PACKAGE"),
		p.isKw("TABLESPACE"), p.isKw("BUFFERPOOL"), p.isKw("STOGROUP"),
		p.isKw("DATABASE"):
		return p.parseNoopToStmtEnd("ALTER " + p.cur.Lit)
	}
	return p.parseNoopToStmtEnd("ALTER")
}

func (p *Parser) parseAlterTable(start Position) ast.Stmt {
	p.expectKw("TABLE")
	at := &ast.AlterTable{P: astPos(start)}
	at.Table = p.parseTableRef()
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("ADD"):
			p.advance()
			act := p.parseAlterAdd()
			at.Actions = append(at.Actions, act)
		case p.isKw("DROP"):
			p.advance()
			act := p.parseAlterDrop()
			at.Actions = append(at.Actions, act)
		case p.isKw("ALTER"):
			p.advance()
			if p.isKw("COLUMN") {
				p.advance()
			}
			act := p.parseAlterColumn()
			at.Actions = append(at.Actions, act)
		case p.isKw("RENAME"):
			p.advance()
			if p.isKw("COLUMN") {
				p.advance()
				old, _ := p.parseIdent()
				p.expectKw("TO")
				newN, _ := p.parseIdent()
				at.Actions = append(at.Actions, ast.AlterAction{
					Kind: "RENAME_COLUMN", OldName: old, NewName: newN,
				})
			} else if p.isKw("TO") {
				p.advance()
				newN, _ := p.parseIdent()
				at.Actions = append(at.Actions, ast.AlterAction{
					Kind: "RENAME_TABLE", NewName: newN,
				})
			}
		default:
			// Unknown clause — capture remainder as a noop action so the
			// translator can surface an info explanation.
			text := p.captureUntilDelimiter()
			at.Actions = append(at.Actions, ast.AlterAction{
				Kind: "NOOP", NoopText: text,
			})
			return at
		}
		if p.isPunct(",") {
			p.advance()
			continue
		}
	}
	return at
}

func (p *Parser) parseAlterAdd() ast.AlterAction {
	switch {
	case p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") ||
		p.isKw("FOREIGN") || p.isKw("CHECK"):
		c := p.parseTableConstraint()
		return ast.AlterAction{Kind: "ADD_CONSTRAINT", Constraint: c}
	case p.isKw("COLUMN"):
		p.advance()
		col := p.parseColumnDef()
		return ast.AlterAction{Kind: "ADD_COLUMN", Column: col}
	}
	// DB2 allows `ADD <col-def>` without the COLUMN keyword.
	col := p.parseColumnDef()
	return ast.AlterAction{Kind: "ADD_COLUMN", Column: col}
}

func (p *Parser) parseAlterDrop() ast.AlterAction {
	switch {
	case p.isKw("CONSTRAINT"):
		p.advance()
		name, _ := p.parseIdent()
		return ast.AlterAction{Kind: "DROP_CONSTRAINT", DropName: name}
	case p.isKw("COLUMN"):
		p.advance()
		name, _ := p.parseIdent()
		// Optional CASCADE / RESTRICT
		if p.isKw("CASCADE") || p.isKw("RESTRICT") {
			p.advance()
		}
		return ast.AlterAction{Kind: "DROP_COLUMN", DropName: name}
	case p.isKw("PRIMARY"):
		p.advance()
		p.expectKw("KEY")
		return ast.AlterAction{Kind: "DROP_CONSTRAINT", DropName: ""}
	case p.isKw("FOREIGN"):
		p.advance()
		p.expectKw("KEY")
		name, _ := p.parseIdent()
		return ast.AlterAction{Kind: "DROP_CONSTRAINT", DropName: name}
	}
	// Unknown DROP form — capture as noop.
	text := p.captureUntilDelimiter()
	return ast.AlterAction{Kind: "NOOP", NoopText: "DROP " + text}
}

func (p *Parser) parseAlterColumn() ast.AlterAction {
	name, _ := p.parseIdent()
	switch {
	case p.isKw("SET"):
		p.advance()
		if p.isKw("DEFAULT") {
			p.advance()
			expr := p.parseDefaultExpr()
			text := ""
			if r, ok := expr.(*ast.RawExpr); ok {
				text = r.Text
			}
			return ast.AlterAction{Kind: "SET_DEFAULT", DropName: name, DefaultExpr: text}
		}
		if p.isKw("NOT") {
			p.advance()
			p.expectKw("NULL")
			return ast.AlterAction{
				Kind: "MODIFY_COLUMN",
				Column: &ast.ColumnDef{Name: name, NotNull: true, HasNullable: true},
			}
		}
	case p.isKw("DROP"):
		p.advance()
		if p.isKw("DEFAULT") {
			p.advance()
			return ast.AlterAction{Kind: "DROP_DEFAULT", DropName: name}
		}
		if p.isKw("NOT") {
			p.advance()
			p.expectKw("NULL")
			return ast.AlterAction{
				Kind: "MODIFY_COLUMN",
				Column: &ast.ColumnDef{Name: name, NotNull: false, HasNullable: true},
			}
		}
	case p.isKw("SET_DATA") || p.isKw("SET"):
		// fallthrough to noop
	}
	// Unknown ALTER COLUMN form — capture remainder as noop.
	text := p.captureUntilDelimiter()
	return ast.AlterAction{Kind: "NOOP", NoopText: "ALTER COLUMN " + name + " " + text}
}

// ---------------------------------------------------------------------------
// DROP
// ---------------------------------------------------------------------------

func (p *Parser) parseDrop() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("DROP")
	if p.isKw("TABLE") {
		p.advance()
		s := &ast.DropTable{P: astPos(start)}
		if p.isKw("IF") {
			p.advance()
			p.expectKw("EXISTS")
			s.IfExists = true
		}
		for {
			ref := p.parseTableRef()
			s.Tables = append(s.Tables, ref)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		// Trailing CASCADE / RESTRICT — consumed.
		for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
			p.advance()
		}
		return s
	}
	switch {
	case p.isKw("INDEX"):
		return p.parseDropObject(start, "INDEX")
	case p.isKw("VIEW"):
		return p.parseDropObject(start, "VIEW")
	case p.isKw("SEQUENCE"):
		return p.parseDropObject(start, "SEQUENCE")
	case p.isKw("PROCEDURE"):
		return p.parseDropObject(start, "PROCEDURE")
	case p.isKw("FUNCTION"):
		return p.parseDropObject(start, "FUNCTION")
	case p.isKw("TRIGGER"):
		return p.parseDropObject(start, "TRIGGER")
	case p.isKw("ALIAS"), p.isKw("SYNONYM"):
		return p.parseDropObject(start, p.cur.Lit)
	case p.isKw("TYPE"):
		return p.parseDropObject(start, "TYPE")
	case p.isKw("PACKAGE"), p.isKw("MODULE"),
		p.isKw("TABLESPACE"), p.isKw("BUFFERPOOL"), p.isKw("STOGROUP"),
		p.isKw("DATABASE"), p.isKw("SCHEMA"), p.isKw("ROLE"), p.isKw("USER"):
		return p.parseNoopToStmtEnd("DROP " + p.cur.Lit)
	}
	return p.parseNoopToStmtEnd("DROP")
}

func (p *Parser) parseDropObject(start Position, kind string) ast.Stmt {
	p.advance() // consume the kind keyword
	s := &ast.DropObject{Kind: kind, P: astPos(start)}
	if p.isKw("IF") {
		p.advance()
		if p.isKw("EXISTS") {
			p.advance()
		}
		s.IfExists = true
	}
	s.Schema, s.Name = p.parseQualifiedName()
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("CASCADE"):
			s.Cascade = true
			p.advance()
		case p.isKw("RESTRICT"):
			s.Restrict = true
			p.advance()
		default:
			p.advance()
		}
	}
	return s
}
