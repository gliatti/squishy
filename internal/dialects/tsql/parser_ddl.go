package tsql

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// DDL: CREATE / ALTER / DROP (grammar rules ddl_clause and below).
// ---------------------------------------------------------------------------

// parseCreate dispatches on the object kind after CREATE [OR ALTER].
func (p *Parser) parseCreate() ast.Stmt {
	start := p.cur
	p.expectKw("CREATE")
	orAlter := false
	if p.isKw("OR") && p.peekIsKw(1, "ALTER") {
		p.advance()
		p.advance()
		orAlter = true
	}
	switch {
	case p.isKw("TABLE"):
		return p.parseCreateTable(start)
	case p.isKw("UNIQUE") || p.isKw("CLUSTERED") || p.isKw("NONCLUSTERED") || p.isKw("INDEX"):
		return p.parseCreateIndex(start)
	case p.isKw("VIEW"):
		return p.parseCreateView(start, orAlter, false)
	case p.isKw("PROC") || p.isKw("PROCEDURE"):
		return p.parseCreateProcedure(start, orAlter)
	case p.isKw("FUNCTION"):
		return p.parseCreateFunction(start, orAlter)
	case p.isKw("TRIGGER"):
		return p.parseCreateTrigger(start, orAlter)
	case p.isKw("TYPE"):
		return p.parseCreateType(start)
	case p.isKw("SEQUENCE"):
		return p.parseCreateSequence(start)
	case p.isKw("SCHEMA"):
		return p.parseCreateSchema(start)
	}
	// Objects with no PostgreSQL counterpart (or created by other means):
	// XML SCHEMA COLLECTION, FULLTEXT CATALOG / INDEX, PRIMARY XML / XML /
	// SPATIAL / COLUMNSTORE INDEX, PARTITION FUNCTION / SCHEME, SYNONYM,
	// DATABASE, LOGIN, USER, ROLE, ASSEMBLY, STATISTICS, DEFAULT, RULE, …
	kind := "CREATE " + strings.ToUpper(p.cur.Raw)
	switch {
	case p.isKw("XML") && p.peekIsKw(1, "SCHEMA"):
		kind = "CREATE XML SCHEMA COLLECTION"
	case p.isKw("PRIMARY") && p.peekIsKw(1, "XML"):
		kind = "CREATE PRIMARY XML INDEX"
	case p.isKw("PRIMARY") || p.isKw("XML") || p.isKw("SPATIAL") || p.isKw("COLUMNSTORE"):
		kind = "CREATE " + strings.ToUpper(p.cur.Raw) + " INDEX"
	case p.isKw("FULLTEXT") || p.isKw("PARTITION"):
		kind = "CREATE " + strings.ToUpper(p.cur.Raw) + " " + strings.ToUpper(p.peek(1).Raw)
	}
	if p.cur.Kind == TOK_EOF || p.cur.Kind == TOK_GO {
		p.errorHere("expected object kind after CREATE", "TABLE|INDEX|VIEW|PROCEDURE|FUNCTION|TRIGGER|TYPE|SEQUENCE|SCHEMA")
		return nil
	}
	// Re-anchor the no-op on CREATE so its Text is the whole statement.
	p.reset(p.indexOf(start), len(p.errs))
	return p.parseNoopToStmtEnd(kind, false)
}

// indexOf returns the token index of t (identified by its offset).
func (p *Parser) indexOf(t Token) int {
	for j := p.i; j >= 0; j-- {
		if p.toks[j].Pos.Offset == t.Pos.Offset {
			return j
		}
	}
	return p.i
}

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

// parseCreateTable parses
//
//	CREATE TABLE name ( element [, element]* [,] ) [ON fg] [TEXTIMAGE_ON fg]
//	  [FILESTREAM_ON fg] [WITH ( option [, …] )]
//
// where element is a column definition, a computed column, a table
// constraint, an inline INDEX or a PERIOD FOR SYSTEM_TIME declaration.
// Column-level PRIMARY KEY / UNIQUE / FOREIGN KEY / CHECK constraints are
// lifted to table constraints (same semantics, names kept).
func (p *Parser) parseCreateTable(start Token) ast.Stmt {
	p.expectKw("TABLE")
	ref := p.parseTableRef()
	s := &ast.CreateTable{Schema: ref.Schema, Name: ref.Name, NameBacktick: ref.NameBacktick, P: astPos(start.Pos)}
	if len(ref.Name) > 0 && ref.Name[0] == '#' {
		s.Temporary = true
	}
	if !p.expectPunct("(") {
		return s
	}
	for !p.isPunct(")") && p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO {
		p.parseTableElement(&s.Columns, &s.Constraints, &s.Indexes, &s.Periods)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	p.parseTableOptions(s)
	return s
}

// parseTableElement parses one element of a table (or table type / table
// variable) definition and appends it to the right list.
func (p *Parser) parseTableElement(cols *[]*ast.ColumnDef, cons *[]ast.TableConstraint, idx *[]*ast.IndexDef, periods *[]ast.PeriodDef) {
	switch {
	case p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") || p.isKw("FOREIGN") || p.isKw("CHECK"):
		if c := p.parseTableConstraint(); c != nil {
			*cons = append(*cons, c)
		}
	case p.isKw("INDEX"):
		if ix := p.parseInlineIndex(); ix != nil && idx != nil {
			*idx = append(*idx, ix)
		}
	case p.isKw("PERIOD") && p.peekIsKw(1, "FOR"):
		p.advance()
		p.advance()
		p.expectKw("SYSTEM_TIME")
		names := p.parseIdentList()
		if len(names) == 2 && periods != nil {
			*periods = append(*periods, ast.PeriodDef{Name: "SYSTEM_TIME", StartCol: names[0], EndCol: names[1]})
		}
	default:
		col, colCons := p.parseColumnDef()
		if col != nil {
			*cols = append(*cols, col)
		}
		*cons = append(*cons, colCons...)
	}
}

// parseColumnDef parses a column definition (or a computed column
// `name AS expr [PERSISTED [NOT NULL]]`) and returns the column plus the
// column-level constraints lifted to table constraints.
func (p *Parser) parseColumnDef() (*ast.ColumnDef, []ast.TableConstraint) {
	start := p.cur.Pos
	name, quoted := p.parseIdent()
	if name == "" && !quoted {
		return nil, nil
	}
	col := &ast.ColumnDef{Name: name, NameBacktick: quoted, P: astPos(start)}
	if p.isKw("AS") {
		p.advance()
		col.Generated = &ast.Generated{Expr: p.parseExpr(), Virtual: true, HasVirtual: true}
		if p.isKw("PERSISTED") {
			p.advance()
			col.Generated.Virtual = false
		}
	} else {
		col.Type = p.parseDataType()
	}
	cons := p.parseColumnOptions(col)
	return col, cons
}

// parseColumnOptions parses the column attributes and constraints that
// follow the data type.
func (p *Parser) parseColumnOptions(col *ast.ColumnDef) []ast.TableConstraint {
	var cons []ast.TableConstraint
	for {
		consName := ""
		consPos := p.cur.Pos
		if p.isKw("CONSTRAINT") {
			p.advance()
			consName, _ = p.parseIdent()
		}
		switch {
		case p.isKw("NOT") && p.peekIsKw(1, "NULL"):
			p.advance()
			p.advance()
			col.NotNull = true
			col.HasNullable = true
		case p.isKw("NOT") && p.peekIsKw(1, "FOR") && p.peekIsKw(2, "REPLICATION"):
			p.advance()
			p.advance()
			p.advance()
			col.NotForReplication = true
		case p.isKw("NULL"):
			p.advance()
			col.NotNull = false
			col.HasNullable = true
		case p.isKw("IDENTITY"):
			p.advance()
			col.AutoInc = true
			col.IdentitySeed, col.IdentityIncrement = 1, 1
			if p.isPunct("(") {
				p.advance()
				seed, _ := p.parseSignedInt()
				p.expectPunct(",")
				inc, _ := p.parseSignedInt()
				p.expectPunct(")")
				col.IdentitySeed, col.IdentityIncrement = seed, inc
				col.HasIdentityArgs = true
			}
		case p.isKw("DEFAULT"):
			p.advance()
			col.Default = p.parseExpr()
			col.HasDefault = true
			col.DefaultName = consName
			if p.isKw("WITH") && p.peekIsKw(1, "VALUES") {
				p.advance()
				p.advance()
			}
		case p.isKw("PRIMARY") || p.isKw("UNIQUE"):
			if c := p.parseKeyConstraint(consName, consPos, []string{col.Name}); c != nil {
				cons = append(cons, c)
			}
		case p.isKw("FOREIGN") || p.isKw("REFERENCES"):
			if p.isKw("FOREIGN") {
				p.advance()
				p.expectKw("KEY")
			}
			fk := &ast.FKConstraint{Name: consName, Columns: []string{col.Name}, P: astPos(consPos)}
			p.parseReferences(fk)
			cons = append(cons, fk)
		case p.isKw("CHECK"):
			cons = append(cons, p.parseCheckConstraint(consName, consPos))
		case p.isKw("COLLATE"):
			p.advance()
			col.Collation, _ = p.parseIdent()
		case p.isKw("ROWGUIDCOL"):
			p.advance()
			col.RowGUIDCol = true
		case p.isKw("SPARSE"):
			p.advance()
			col.Sparse = true
		case p.isKw("FILESTREAM"):
			p.advance()
			col.FileStream = true
		case p.isKw("PERSISTED") && col.Generated != nil:
			p.advance()
			col.Generated.Virtual = false
		case p.isKw("HIDDEN"):
			p.advance()
			col.Invisible = true
		case p.isKw("GENERATED") && p.peekIsKw(1, "ALWAYS"):
			p.advance()
			p.advance()
			p.expectKw("AS")
			// ROW START | ROW END | TRANSACTION_ID START|END |
			// SEQUENCE_NUMBER START|END
			if p.isKw("ROW") {
				p.advance()
			} else if p.isIdentTok() {
				p.advance()
			}
			if p.isKw("START") || p.isKw("END") {
				col.PeriodRow = strings.ToUpper(p.cur.Raw)
				p.advance()
			} else {
				p.errorHere("expected START or END", "START|END")
			}
			col.SystemVersioning = true
		case p.isKw("MASKED") || p.isKw("ENCRYPTED"):
			p.advance()
			p.expectKw("WITH")
			p.skipBalancedParens()
		case p.isKw("INDEX"):
			// inline column index: INDEX ix [CLUSTERED|NONCLUSTERED]
			p.advance()
			p.parseIdent()
			if p.isKw("CLUSTERED") || p.isKw("NONCLUSTERED") {
				p.advance()
			}
		default:
			if consName != "" {
				p.errorHere("expected constraint after CONSTRAINT name", "PRIMARY KEY|UNIQUE|FOREIGN KEY|CHECK|DEFAULT")
			}
			return cons
		}
	}
}

// parseKeyConstraint parses `PRIMARY KEY | UNIQUE [CLUSTERED|NONCLUSTERED]
// [(cols)] [WITH (…)] [ON fg]`. defaultCols is used for the column-level
// form (no column list).
func (p *Parser) parseKeyConstraint(name string, pos Position, defaultCols []string) ast.TableConstraint {
	primary := p.isKw("PRIMARY")
	p.advance()
	if primary {
		p.expectKw("KEY")
	}
	clustered := ""
	if p.isKw("CLUSTERED") || p.isKw("NONCLUSTERED") {
		clustered = p.cur.Lit
		p.advance()
	}
	var cols []ast.IndexedCol
	if p.isPunct("(") {
		cols = p.parseIndexedCols()
	} else {
		for _, c := range defaultCols {
			cols = append(cols, ast.IndexedCol{Name: c})
		}
		if len(defaultCols) == 0 {
			p.errorHere("expected column list", "'('")
		}
	}
	p.skipIndexOptions(nil)
	if primary {
		return &ast.PKConstraint{Name: name, Columns: cols, Clustered: clustered, P: astPos(pos)}
	}
	return &ast.UQConstraint{Name: name, Columns: cols, Clustered: clustered, P: astPos(pos)}
}

// parseReferences parses `REFERENCES t [(cols)] [ON DELETE a] [ON UPDATE a]
// [NOT FOR REPLICATION]` into fk.
func (p *Parser) parseReferences(fk *ast.FKConstraint) {
	p.expectKw("REFERENCES")
	ref := p.parseTableRef()
	fk.RefSchema, fk.RefTable = ref.Schema, ref.Name
	if p.isPunct("(") {
		fk.RefColumns = p.parseIdentList()
	}
	for {
		switch {
		case p.isKw("ON") && p.peekIsKw(1, "DELETE"):
			p.advance()
			p.advance()
			fk.OnDelete = p.parseRefAction()
		case p.isKw("ON") && p.peekIsKw(1, "UPDATE"):
			p.advance()
			p.advance()
			fk.OnUpdate = p.parseRefAction()
		case p.isKw("NOT") && p.peekIsKw(1, "FOR"):
			p.advance()
			p.advance()
			p.expectKw("REPLICATION")
			fk.NotForReplication = true
		default:
			return
		}
	}
}

// parseRefAction parses NO ACTION | CASCADE | SET NULL | SET DEFAULT.
func (p *Parser) parseRefAction() string {
	switch {
	case p.isKw("NO"):
		p.advance()
		p.expectKw("ACTION")
		return "NO ACTION"
	case p.isKw("CASCADE"):
		p.advance()
		return "CASCADE"
	case p.isKw("SET") && p.peekIsKw(1, "NULL"):
		p.advance()
		p.advance()
		return "SET NULL"
	case p.isKw("SET") && p.peekIsKw(1, "DEFAULT"):
		p.advance()
		p.advance()
		return "SET DEFAULT"
	}
	p.errorHere("expected referential action", "NO ACTION|CASCADE|SET NULL|SET DEFAULT")
	return ""
}

// parseCheckConstraint parses `CHECK [NOT FOR REPLICATION] (expr)`.
func (p *Parser) parseCheckConstraint(name string, pos Position) *ast.CheckConstraint {
	p.expectKw("CHECK")
	c := &ast.CheckConstraint{Name: name, Enforced: true, P: astPos(pos)}
	if p.isKw("NOT") && p.peekIsKw(1, "FOR") {
		p.advance()
		p.advance()
		p.expectKw("REPLICATION")
		c.NotForReplication = true
	}
	p.expectPunct("(")
	c.Expr = p.parseExpr()
	p.expectPunct(")")
	return c
}

// parseTableConstraint parses a table-level constraint.
func (p *Parser) parseTableConstraint() ast.TableConstraint {
	pos := p.cur.Pos
	name := ""
	if p.isKw("CONSTRAINT") {
		p.advance()
		name, _ = p.parseIdent()
	}
	switch {
	case p.isKw("PRIMARY") || p.isKw("UNIQUE"):
		return p.parseKeyConstraint(name, pos, nil)
	case p.isKw("FOREIGN"):
		p.advance()
		p.expectKw("KEY")
		fk := &ast.FKConstraint{Name: name, P: astPos(pos)}
		fk.Columns = p.parseIdentList()
		p.parseReferences(fk)
		return fk
	case p.isKw("CHECK"):
		return p.parseCheckConstraint(name, pos)
	}
	p.errorHere("expected table constraint", "PRIMARY KEY|UNIQUE|FOREIGN KEY|CHECK")
	return nil
}

// parseIndexedCols parses `( col [ASC|DESC], … )`.
func (p *Parser) parseIndexedCols() []ast.IndexedCol {
	var out []ast.IndexedCol
	if !p.expectPunct("(") {
		return nil
	}
	for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
		n, _ := p.parseIdent()
		ic := ast.IndexedCol{Name: n}
		if p.isKw("ASC") || p.isKw("DESC") {
			ic.Order = p.cur.Lit
			p.advance()
		}
		out = append(out, ic)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return out
}

// skipIndexOptions consumes the storage options of an index / key:
// `WITH (opt = v, …)`, legacy `WITH FILLFACTOR = n`, `ON fg | ps(col) |
// "default"`, `FILESTREAM_ON fg`. The raw texts are appended to opts when
// it is non-nil.
func (p *Parser) skipIndexOptions(opts *[]string) {
	for {
		start := p.cur
		switch {
		case p.isKw("WITH") && p.peekIsPunct(1, "("):
			p.advance()
			p.skipBalancedParens()
		case p.isKw("WITH") && p.peekIsKw(1, "FILLFACTOR"):
			p.advance()
			p.advance()
			p.expectPunct("=")
			p.advance()
		case p.isKw("ON") && (p.peek(1).Kind == TOK_IDENT || p.peek(1).Kind == TOK_QUOTED_IDENT || p.peek(1).Kind == TOK_SQLCMD_VAR):
			p.advance()
			p.parseIdent()
			if p.isPunct("(") {
				p.skipBalancedParens()
			}
		case p.isKw("ON") && p.peekIsKw(1, "DEFAULT"):
			p.advance()
			p.advance()
		case p.isKw("FILESTREAM_ON"):
			p.advance()
			p.parseIdentAny()
		default:
			return
		}
		if opts != nil {
			*opts = append(*opts, p.textFrom(start))
		}
	}
}

// parseInlineIndex parses an inline `INDEX ix [UNIQUE] [CLUSTERED |
// NONCLUSTERED] (cols) [INCLUDE (…)] [WHERE …] [WITH (…)] [ON fg]`.
func (p *Parser) parseInlineIndex() *ast.IndexDef {
	pos := p.cur.Pos
	p.expectKw("INDEX")
	ix := &ast.IndexDef{P: astPos(pos)}
	ix.Name, _ = p.parseIdent()
	if p.isKw("UNIQUE") {
		ix.Unique = true
		p.advance()
	}
	if p.isKw("CLUSTERED") || p.isKw("NONCLUSTERED") {
		p.advance()
	}
	if p.isKw("COLUMNSTORE") {
		p.advance()
		ix.Kind = "COLUMNSTORE"
	}
	if p.isPunct("(") {
		ix.Columns = p.parseIndexedCols()
	}
	if p.isKw("INCLUDE") {
		p.advance()
		p.parseIdentList()
	}
	if p.isKw("WHERE") {
		p.advance()
		p.parseExpr()
	}
	p.skipIndexOptions(nil)
	return ix
}

// parseTableOptions parses the clauses after a table's column list.
func (p *Parser) parseTableOptions(s *ast.CreateTable) {
	for {
		switch {
		case p.isKw("ON"):
			p.advance()
			if p.isKw("DEFAULT") {
				p.advance()
				s.Options.Extras = append(s.Options.Extras, "ON=DEFAULT")
				continue
			}
			fg, _ := p.parseIdent()
			if p.isPunct("(") {
				p.skipBalancedParens()
			}
			s.Options.Extras = append(s.Options.Extras, "ON="+fg)
		case p.isKw("TEXTIMAGE_ON") || p.isKw("FILESTREAM_ON"):
			opt := strings.ToUpper(p.cur.Raw)
			p.advance()
			fg, _ := p.parseIdentAny()
			s.Options.Extras = append(s.Options.Extras, opt+"="+fg)
		case p.isKw("WITH") && p.peekIsPunct(1, "("):
			p.advance()
			p.parseTableWithOptions(s)
		case p.isKw("AS") && (p.peekIsKw(1, "NODE") || p.peekIsKw(1, "EDGE") || p.peekIsKw(1, "FILETABLE")):
			p.advance()
			s.Options.Extras = append(s.Options.Extras, "AS="+strings.ToUpper(p.cur.Raw))
			p.advance()
		default:
			return
		}
	}
}

// parseTableWithOptions parses `( option [, option]* )` after a table's
// WITH. SYSTEM_VERSIONING = ON [(HISTORY_TABLE = s.t, …)] sets
// SystemVersioned / HistoryTable; every option is also kept raw in
// Options.Extras.
func (p *Parser) parseTableWithOptions(s *ast.CreateTable) {
	p.expectPunct("(")
	for !p.isPunct(")") && p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO {
		start := p.cur
		name, _ := p.parseIdentAny()
		upper := strings.ToUpper(name)
		if p.isPunct("=") {
			p.advance()
		}
		if upper == "SYSTEM_VERSIONING" {
			if p.isKw("ON") {
				s.SystemVersioned = true
				s.Options.SystemVersioning = true
			}
			p.advance()
			if p.isPunct("(") {
				p.advance()
				for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
					sub, _ := p.parseIdentAny()
					if p.isPunct("=") {
						p.advance()
					}
					if strings.EqualFold(sub, "HISTORY_TABLE") {
						ref := p.parseTableRef()
						s.HistoryTable = &ref
					} else {
						p.skipOptionValue()
					}
					if !p.isPunct(",") {
						break
					}
					p.advance()
				}
				p.expectPunct(")")
			}
		} else {
			p.skipOptionValue()
		}
		s.Options.Extras = append(s.Options.Extras, p.textFrom(start))
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
}

// skipOptionValue consumes an option value up to the next top-level ','
// or ')'.
func (p *Parser) skipOptionValue() {
	for p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO && !p.isPunct(",") && !p.isPunct(")") {
		if p.isPunct("(") {
			p.skipBalancedParens()
			continue
		}
		p.advance()
	}
}

// parseSignedInt parses an optionally signed integer literal.
func (p *Parser) parseSignedInt() (int64, bool) {
	neg := false
	if p.isPunct("-") || p.isPunct("+") {
		neg = p.isPunct("-")
		p.advance()
	}
	if p.cur.Kind != TOK_NUMBER {
		p.errorHere("expected integer", "NUMBER")
		return 0, false
	}
	n, err := strconv.ParseInt(p.cur.Lit, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(p.cur.Lit, 64)
		if ferr != nil {
			p.errorHere("invalid integer", "integer")
		}
		n = int64(f)
	}
	p.advance()
	if neg {
		n = -n
	}
	return n, true
}

// ---------------------------------------------------------------------------
// CREATE INDEX
// ---------------------------------------------------------------------------

// parseCreateIndex parses
//
//	CREATE [UNIQUE] [CLUSTERED|NONCLUSTERED] INDEX ix ON t (cols)
//	  [INCLUDE (cols)] [WHERE pred] [WITH (…)] [ON fg] [FILESTREAM_ON …]
//
// A COLUMNSTORE index has no PG counterpart and is returned as a no-op.
func (p *Parser) parseCreateIndex(start Token) ast.Stmt {
	s := &ast.CreateIndex{P: astPos(start.Pos)}
	if p.isKw("UNIQUE") {
		s.Unique = true
		p.advance()
	}
	if p.isKw("CLUSTERED") || p.isKw("NONCLUSTERED") {
		s.Clustered = p.cur.Lit
		p.advance()
	}
	if p.isKw("COLUMNSTORE") {
		p.reset(p.indexOf(start), len(p.errs))
		return p.parseNoopToStmtEnd("CREATE COLUMNSTORE INDEX", false)
	}
	p.expectKw("INDEX")
	s.Name, _ = p.parseIdent()
	p.expectKw("ON")
	s.Table = p.parseTableRef()
	s.Columns = p.parseIndexedCols()
	if p.isKw("INCLUDE") {
		p.advance()
		s.Include = p.parseIdentList()
	}
	if p.isKw("WHERE") {
		p.advance()
		s.Where = p.parseExpr()
	}
	p.skipIndexOptions(&s.Options)
	return s
}

// ---------------------------------------------------------------------------
// CREATE VIEW
// ---------------------------------------------------------------------------

// parseCreateView parses `CREATE [OR ALTER] VIEW name [(cols)] [WITH
// SCHEMABINDING|ENCRYPTION|VIEW_METADATA, …] AS select [WITH CHECK
// OPTION]`. The body runs to the end of the batch; it is parsed by a
// bounded sub-parser, so a failure is recorded on the node
// (SelectParseError) and never moves the statement boundary. alter is
// true for ALTER VIEW (same node, OrAlter set).
func (p *Parser) parseCreateView(start Token, orAlter, alter bool) ast.Stmt {
	p.expectKw("VIEW")
	s := &ast.CreateView{OrAlter: orAlter || alter, OrReplace: orAlter || alter, P: astPos(start.Pos)}
	s.View = p.parseTableRef()
	if p.isPunct("(") {
		s.Columns = p.parseIdentList()
	}
	if p.isKw("WITH") {
		p.advance()
		for {
			opt, _ := p.parseIdentAny()
			if strings.EqualFold(opt, "SCHEMABINDING") {
				s.SchemaBinding = true
			} else {
				s.ViewOptions = append(s.ViewOptions, strings.ToUpper(opt))
			}
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	}
	if !p.expectKw("AS") {
		p.skipToBatchEnd()
		return s
	}
	bodyFrom := p.i
	end := p.batchEnd()
	sp := p.subParser(bodyFrom, end)
	if sp.cur.Kind == TOK_EOF {
		s.SelectParseError = "empty SELECT"
	} else {
		sel := sp.parseSelectStatement()
		selEnd := sp.i
		if sp.isKw("WITH") && sp.peekIsKw(1, "CHECK") {
			sp.advance()
			sp.advance()
			sp.expectKw("OPTION")
			s.CheckOption = "CHECK OPTION"
		}
		for sp.isPunct(";") {
			sp.advance()
		}
		if len(sp.errs) == 0 && sp.cur.Kind != TOK_EOF {
			sp.errorHere("unexpected token after view SELECT", "end of batch")
		}
		s.SelectBody = p.spanText(bodyFrom, bodyFrom+selEnd)
		if len(sp.errs) > 0 || sel == nil {
			s.SelectParseError = sp.errs.Error()
		} else {
			s.Select = sel
		}
	}
	p.jumpTo(end)
	return s
}

// jumpTo moves the parser to token index i.
func (p *Parser) jumpTo(i int) {
	if i >= len(p.toks) {
		i = len(p.toks) - 1
	}
	p.i = i
	p.cur = p.toks[i]
}

// skipToBatchEnd moves the parser to the end of the current batch.
func (p *Parser) skipToBatchEnd() {
	p.jumpTo(p.batchEnd())
}

// ---------------------------------------------------------------------------
// CREATE PROCEDURE / FUNCTION / TRIGGER
// ---------------------------------------------------------------------------

// parseRoutineOptions parses the `WITH option [, option]*` list of a
// procedure / function / trigger (ENCRYPTION, RECOMPILE, SCHEMABINDING,
// NATIVE_COMPILATION, RETURNS NULL ON NULL INPUT, CALLED ON NULL INPUT,
// INLINE = ON|OFF, EXECUTE AS CALLER|SELF|OWNER|'user') and returns their
// raw texts.
func (p *Parser) parseRoutineOptions() []string {
	var out []string
	if !p.isKw("WITH") {
		return nil
	}
	p.advance()
	for {
		start := p.cur
		switch {
		case p.isKw("EXECUTE") || p.isKw("EXEC"):
			p.advance()
			p.expectKw("AS")
			if p.cur.Kind == TOK_STRING {
				p.advance()
			} else {
				p.parseIdentAny()
			}
		case p.isKw("RETURNS") && p.peekIsKw(1, "NULL"):
			// RETURNS NULL ON NULL INPUT
			for k := 0; k < 5; k++ {
				p.advance()
			}
		case p.isKw("CALLED"):
			// CALLED ON NULL INPUT
			for k := 0; k < 4; k++ {
				p.advance()
			}
		case p.isKw("INLINE"):
			p.advance()
			p.expectPunct("=")
			p.advance()
		default:
			p.parseIdentAny()
		}
		out = append(out, p.textFrom(start))
		if !p.isPunct(",") {
			return out
		}
		p.advance()
	}
}

// parseParam parses one routine parameter:
//
//	@name [AS] [schema.]type [VARYING] [NULL] [= default] [OUT|OUTPUT] [READONLY]
func (p *Parser) parseParam(proc bool) ast.Param {
	prm := ast.Param{}
	if p.cur.Kind != TOK_VARIABLE {
		p.errorHere("expected @parameter", "@name")
		return prm
	}
	prm.Name = p.cur.Lit
	p.advance()
	if p.isKw("AS") {
		p.advance()
	}
	prm.Type = p.parseDataType()
	if proc {
		prm.Direction = "IN"
	}
	for {
		switch {
		case p.isKw("VARYING") || p.isKw("NULL"):
			p.advance()
		case p.isKw("NOT") && p.peekIsKw(1, "NULL"):
			p.advance()
			p.advance()
		case p.isPunct("="):
			p.advance()
			prm.Default = p.parseExpr()
		case p.isKw("OUT") || p.isKw("OUTPUT"):
			p.advance()
			prm.Output = true
			prm.Direction = "INOUT"
		case p.isKw("READONLY"):
			p.advance()
			prm.ReadOnly = true
		default:
			return prm
		}
	}
}

// routineBody returns the raw text of a routine body: from the current
// token to the end of the batch. The parser moves to the batch end.
func (p *Parser) routineBody() string {
	from := p.i
	end := p.batchEnd()
	body := p.spanText(from, end)
	p.jumpTo(end)
	return body
}

// parseCreateProcedure parses
//
//	CREATE [OR ALTER] PROC[EDURE] name [;n] [(] params [)] [WITH opts]
//	  [FOR REPLICATION] AS body
func (p *Parser) parseCreateProcedure(start Token, orAlter bool) ast.Stmt {
	p.advance() // PROC | PROCEDURE
	ref := p.parseTableRef()
	s := &ast.CreateProcedure{Schema: ref.Schema, Name: ref.Name, OrAlter: orAlter, P: astPos(start.Pos)}
	if p.isPunct(";") && p.peek(1).Kind == TOK_NUMBER {
		// numbered procedure group (;1): obsolete, the number is dropped.
		p.advance()
		p.advance()
	}
	paren := false
	if p.isPunct("(") {
		paren = true
		p.advance()
	}
	for p.cur.Kind == TOK_VARIABLE {
		s.Params = append(s.Params, p.parseParam(true))
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	if paren {
		p.expectPunct(")")
	}
	s.Options = p.parseRoutineOptions()
	if p.isKw("FOR") && p.peekIsKw(1, "REPLICATION") {
		p.advance()
		p.advance()
		s.ForReplication = true
	}
	if !p.expectKw("AS") {
		p.skipToBatchEnd()
		return s
	}
	s.Body = p.routineBody()
	return s
}

// parseCreateFunction parses the three T-SQL function shapes:
//
//	scalar:          RETURNS type [WITH opts] [AS] BEGIN … END
//	inline TVF:      RETURNS TABLE [WITH opts] [AS] RETURN [(] select [)]
//	multi-statement: RETURNS @t TABLE (cols) [WITH opts] [AS] BEGIN … END
func (p *Parser) parseCreateFunction(start Token, orAlter bool) ast.Stmt {
	p.expectKw("FUNCTION")
	ref := p.parseTableRef()
	s := &ast.CreateFunction{Schema: ref.Schema, Name: ref.Name, OrAlter: orAlter, P: astPos(start.Pos)}
	p.expectPunct("(")
	for p.cur.Kind == TOK_VARIABLE {
		s.Params = append(s.Params, p.parseParam(false))
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	p.expectKw("RETURNS")
	switch {
	case p.cur.Kind == TOK_VARIABLE:
		s.ReturnsTableVar = p.cur.Lit
		p.advance()
		p.expectKw("TABLE")
		tt := p.parseTableTypeBody(p.cur.Pos)
		s.ReturnsTableCols = tt.Columns
		s.ReturnsTableConstraints = tt.Constraints
	case p.isKw("TABLE"):
		p.advance()
		s.ReturnsInlineTable = true
	default:
		s.Returns = p.parseDataType()
	}
	s.Options = p.parseRoutineOptions()
	if p.isKw("AS") {
		p.advance()
	}
	if !s.ReturnsInlineTable {
		s.Body = p.routineBody()
		return s
	}
	// Inline TVF: RETURN [(] select [)] up to the end of the batch.
	from := p.i
	end := p.batchEnd()
	sp := p.subParser(from, end)
	if sp.expectKw("RETURN") {
		// `RETURN (SELECT …)` and `RETURN SELECT …` alike: a parenthesised
		// query is a query term of the select-statement grammar.
		sel := sp.parseSelectStatement()
		for sp.isPunct(";") {
			sp.advance()
		}
		if len(sp.errs) == 0 && sp.cur.Kind != TOK_EOF {
			sp.errorHere("unexpected token after inline function query", "end of batch")
		}
		if len(sp.errs) > 0 || sel == nil {
			s.SelectParseError = sp.errs.Error()
		} else {
			s.Select = sel
		}
	} else {
		s.SelectParseError = sp.errs.Error()
	}
	s.Body = p.spanText(from, end)
	p.jumpTo(end)
	return s
}

// parseTableTypeBody parses `( element [, element]* )` of a table type,
// table variable or multi-statement function return table.
func (p *Parser) parseTableTypeBody(pos Position) *ast.TSQLTableType {
	tt := &ast.TSQLTableType{P: astPos(pos)}
	if !p.expectPunct("(") {
		return tt
	}
	var idx []*ast.IndexDef
	for !p.isPunct(")") && p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO {
		p.parseTableElement(&tt.Columns, &tt.Constraints, &idx, nil)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return tt
}

// parseCreateTrigger parses
//
//	CREATE [OR ALTER] TRIGGER [s.]name ON {table | view} [WITH opts]
//	  {FOR | AFTER | INSTEAD OF} {INSERT, UPDATE, DELETE} [WITH APPEND]
//	  [NOT FOR REPLICATION] AS body
//
// DDL triggers (ON DATABASE / ON ALL SERVER) and LOGON triggers have no
// PG table-trigger counterpart: they are returned as a no-op covering the
// whole batch (Kind "CREATE DDL TRIGGER").
func (p *Parser) parseCreateTrigger(start Token, orAlter bool) ast.Stmt {
	p.expectKw("TRIGGER")
	ref := p.parseTableRef()
	s := &ast.CreateTrigger{Schema: ref.Schema, Name: ref.Name, OrAlter: orAlter, ForEachRow: false, HasForEach: true, P: astPos(start.Pos)}
	p.expectKw("ON")
	if p.isKw("DATABASE") || p.isKw("ALL") {
		p.skipToBatchEnd()
		return &ast.NoopStmt{Kind: "CREATE DDL TRIGGER", Text: p.textFrom(start), P: astPos(start.Pos)}
	}
	s.Table = p.parseTableRef()
	s.Options = p.parseRoutineOptions()
	switch {
	case p.isKw("FOR") || p.isKw("AFTER"):
		p.advance()
		s.Time = "AFTER"
	case p.isKw("INSTEAD"):
		p.advance()
		p.expectKw("OF")
		s.Time = "INSTEAD OF"
	default:
		p.errorHere("expected FOR, AFTER or INSTEAD OF", "FOR|AFTER|INSTEAD OF")
	}
	for p.isKw("INSERT") || p.isKw("UPDATE") || p.isKw("DELETE") {
		s.Events = append(s.Events, p.cur.Lit)
		p.advance()
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	if len(s.Events) == 0 {
		p.errorHere("expected trigger event", "INSERT|UPDATE|DELETE")
	}
	s.Event = strings.Join(s.Events, " OR ")
	if p.isKw("WITH") && p.peekIsKw(1, "APPEND") {
		p.advance()
		p.advance()
	}
	if p.isKw("NOT") && p.peekIsKw(1, "FOR") {
		p.advance()
		p.advance()
		p.expectKw("REPLICATION")
		s.NotForReplication = true
	}
	if !p.expectKw("AS") {
		p.skipToBatchEnd()
		return s
	}
	s.Body = p.routineBody()
	return s
}

// ---------------------------------------------------------------------------
// CREATE TYPE / SEQUENCE / SCHEMA
// ---------------------------------------------------------------------------

// parseCreateType parses `CREATE TYPE name FROM base [NULL|NOT NULL]`
// (Kind "alias") and `CREATE TYPE name AS TABLE (…)` (Kind "table"). CLR
// types (EXTERNAL NAME) are returned as a no-op.
func (p *Parser) parseCreateType(start Token) ast.Stmt {
	p.expectKw("TYPE")
	ref := p.parseTableRef()
	s := &ast.CreateType{Schema: ref.Schema, Name: ref.Name, P: astPos(start.Pos)}
	switch {
	case p.isKw("FROM"):
		p.advance()
		s.Kind = "alias"
		s.BaseType = p.parseDataType()
		if p.isKw("NOT") && p.peekIsKw(1, "NULL") {
			p.advance()
			p.advance()
			s.NotNull, s.HasNullable = true, true
		} else if p.isKw("NULL") {
			p.advance()
			s.HasNullable = true
		}
	case p.isKw("AS") && p.peekIsKw(1, "TABLE"):
		p.advance()
		p.advance()
		s.Kind = "table"
		tt := p.parseTableTypeBody(p.cur.Pos)
		s.TableCols, s.TableConstraints = tt.Columns, tt.Constraints
		if p.isKw("WITH") && p.peekIsPunct(1, "(") {
			p.advance()
			p.skipBalancedParens()
		}
	default:
		p.reset(p.indexOf(start), len(p.errs))
		return p.parseNoopToStmtEnd("CREATE TYPE", false)
	}
	return s
}

// parseCreateSequence parses
//
//	CREATE SEQUENCE name [AS type] [START WITH n] [INCREMENT BY n]
//	  [MINVALUE n | NO MINVALUE] [MAXVALUE n | NO MAXVALUE]
//	  [CYCLE | NO CYCLE] [CACHE [n] | NO CACHE]
func (p *Parser) parseCreateSequence(start Token) ast.Stmt {
	p.expectKw("SEQUENCE")
	ref := p.parseTableRef()
	s := &ast.CreateSequence{Schema: ref.Schema, Name: ref.Name, P: astPos(start.Pos)}
	for {
		switch {
		case p.isKw("AS"):
			p.advance()
			s.AsType = p.parseDataType()
		case p.isKw("START"):
			p.advance()
			p.expectKw("WITH")
			s.Start, s.HasStart = p.parseSignedInt()
		case p.isKw("INCREMENT"):
			p.advance()
			p.expectKw("BY")
			s.Increment, s.HasIncr = p.parseSignedInt()
		case p.isKw("MINVALUE"):
			p.advance()
			s.MinValue, s.HasMin = p.parseSignedInt()
		case p.isKw("MAXVALUE"):
			p.advance()
			s.MaxValue, s.HasMax = p.parseSignedInt()
		case p.isKw("CYCLE"):
			p.advance()
			s.Cycle, s.HasCycle = true, true
		case p.isKw("CACHE"):
			p.advance()
			s.HasCache = true
			if p.cur.Kind == TOK_NUMBER {
				s.Cache, _ = p.parseSignedInt()
			}
		case p.isKw("NO"):
			p.advance()
			switch {
			case p.isKw("MINVALUE"):
				s.NoMin = true
			case p.isKw("MAXVALUE"):
				s.NoMax = true
			case p.isKw("CYCLE"):
				s.Cycle, s.HasCycle = false, true
			case p.isKw("CACHE"):
				s.NoCache = true
			default:
				p.errorHere("expected MINVALUE, MAXVALUE, CYCLE or CACHE after NO", "MINVALUE|MAXVALUE|CYCLE|CACHE")
				return s
			}
			p.advance()
		default:
			return s
		}
	}
}

// parseAlterSequence parses `ALTER SEQUENCE name` followed by RESTART
// [WITH n], INCREMENT BY, [NO] MINVALUE / MAXVALUE / CYCLE / CACHE.
func (p *Parser) parseAlterSequence(start Token) ast.Stmt {
	p.expectKw("SEQUENCE")
	ref := p.parseTableRef()
	s := &ast.AlterSequence{Schema: ref.Schema, Name: ref.Name, P: astPos(start.Pos)}
	for {
		switch {
		case p.isKw("RESTART"):
			p.advance()
			s.Restart, s.HasRestart = true, true
			if p.isKw("WITH") {
				p.advance()
				s.StartWith, s.HasStartWith = p.parseSignedInt()
			}
		case p.isKw("INCREMENT"):
			p.advance()
			p.expectKw("BY")
			s.Increment, s.HasIncr = p.parseSignedInt()
		case p.isKw("MINVALUE"):
			p.advance()
			s.MinValue, s.HasMin = p.parseSignedInt()
		case p.isKw("MAXVALUE"):
			p.advance()
			s.MaxValue, s.HasMax = p.parseSignedInt()
		case p.isKw("CYCLE"):
			p.advance()
			s.Cycle, s.HasCycle = true, true
		case p.isKw("CACHE"):
			p.advance()
			s.HasCache = true
			if p.cur.Kind == TOK_NUMBER {
				s.Cache, _ = p.parseSignedInt()
			}
		case p.isKw("NO"):
			p.advance()
			switch {
			case p.isKw("MINVALUE"):
				s.NoMin = true
			case p.isKw("MAXVALUE"):
				s.NoMax = true
			case p.isKw("CYCLE"):
				s.Cycle, s.HasCycle = false, true
			case p.isKw("CACHE"):
				s.NoCache = true
			default:
				p.errorHere("expected MINVALUE, MAXVALUE, CYCLE or CACHE after NO", "MINVALUE|MAXVALUE|CYCLE|CACHE")
				return s
			}
			p.advance()
		default:
			return s
		}
	}
}

// parseCreateSchema parses `CREATE SCHEMA name [AUTHORIZATION owner]`.
// Schema elements written in the same statement (CREATE TABLE …, GRANT …)
// are parsed as the statements that follow.
func (p *Parser) parseCreateSchema(start Token) ast.Stmt {
	p.expectKw("SCHEMA")
	s := &ast.CreateSchema{P: astPos(start.Pos)}
	if !p.isKw("AUTHORIZATION") {
		s.Name, _ = p.parseIdent()
	}
	if p.isKw("AUTHORIZATION") {
		p.advance()
		s.Authorization, _ = p.parseIdent()
		if s.Name == "" {
			s.Name = s.Authorization
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// ALTER
// ---------------------------------------------------------------------------

// parseAlter dispatches ALTER TABLE / VIEW / PROCEDURE / FUNCTION /
// TRIGGER / SEQUENCE / INDEX; the other ALTERs (DATABASE, SCHEMA …
// TRANSFER, XML SCHEMA COLLECTION, AUTHORIZATION, LOGIN, …) are no-ops.
func (p *Parser) parseAlter() ast.Stmt {
	start := p.cur
	p.expectKw("ALTER")
	switch {
	case p.isKw("TABLE"):
		return p.parseAlterTable(start)
	case p.isKw("VIEW"):
		return p.parseCreateView(start, false, true)
	case p.isKw("PROC") || p.isKw("PROCEDURE"):
		return p.parseCreateProcedure(start, true)
	case p.isKw("FUNCTION"):
		return p.parseCreateFunction(start, true)
	case p.isKw("TRIGGER"):
		return p.parseCreateTrigger(start, true)
	case p.isKw("SEQUENCE"):
		return p.parseAlterSequence(start)
	case p.isKw("INDEX"):
		return p.parseAlterIndex(start)
	}
	kind := "ALTER " + strings.ToUpper(p.cur.Raw)
	if p.isKw("XML") {
		kind = "ALTER XML SCHEMA COLLECTION"
	}
	keepSet := p.isKw("DATABASE")
	p.reset(p.indexOf(start), len(p.errs))
	return p.parseNoopToStmtEnd(kind, keepSet)
}

// parseAlterIndex parses `ALTER INDEX {name | ALL} ON t action …`.
func (p *Parser) parseAlterIndex(start Token) ast.Stmt {
	p.expectKw("INDEX")
	s := &ast.AlterIndex{P: astPos(start.Pos)}
	if p.isKw("ALL") {
		p.advance()
		s.Name = "ALL"
	} else {
		s.Name, _ = p.parseIdent()
	}
	p.expectKw("ON")
	ref := p.parseTableRef()
	s.Schema = ref.Schema
	actStart := p.cur
	switch {
	case p.isKw("REBUILD"):
		s.Action = "REBUILD"
	case p.isKw("DISABLE"):
		s.Action = "DISABLE"
	default:
		s.Action = "OTHER"
	}
	for p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO && !p.isPunct(";") && !(p.cur.LineStart && p.isStmtStartKeyword()) {
		if p.isPunct("(") {
			p.skipBalancedParens()
			continue
		}
		p.advance()
	}
	s.RawTail = p.textFrom(actStart)
	return s
}

// parseAlterTable parses `ALTER TABLE t action`, where action is
//
//	[WITH CHECK|NOCHECK] ADD element [, element]*
//	ALTER COLUMN c type [COLLATE x] [NULL|NOT NULL] | ALTER COLUMN c {ADD|DROP} attr
//	DROP [CONSTRAINT|COLUMN] [IF EXISTS] name [, …] | DROP PERIOD FOR SYSTEM_TIME
//	[WITH CHECK|NOCHECK] {CHECK|NOCHECK} CONSTRAINT {ALL | name, …}
//	{ENABLE|DISABLE} TRIGGER {ALL | name, …}
//	SET (…), SWITCH …, REBUILD …, … (kept as NOOP actions)
func (p *Parser) parseAlterTable(start Token) ast.Stmt {
	p.expectKw("TABLE")
	s := &ast.AlterTable{Table: p.parseTableRef(), P: astPos(start.Pos)}
	noCheck := false
	if p.isKw("WITH") && (p.peekIsKw(1, "CHECK") || p.peekIsKw(1, "NOCHECK")) {
		p.advance()
		noCheck = p.isKw("NOCHECK")
		p.advance()
	}
	switch {
	case p.isKw("ADD"):
		p.advance()
		for {
			s.Actions = append(s.Actions, p.parseAlterAddElement(noCheck)...)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
	case p.isKw("ALTER") && p.peekIsKw(1, "COLUMN"):
		p.advance()
		p.advance()
		s.Actions = append(s.Actions, p.parseAlterColumn())
	case p.isKw("DROP"):
		p.advance()
		s.Actions = append(s.Actions, p.parseAlterDrop()...)
	case p.isKw("CHECK") || p.isKw("NOCHECK"):
		actStart := p.cur
		p.advance()
		p.expectKw("CONSTRAINT")
		p.parseNameListOrAll()
		s.Actions = append(s.Actions, ast.AlterAction{Kind: "NOOP", NoopText: p.textFrom(actStart)})
	case p.isKw("ENABLE") || p.isKw("DISABLE"):
		actStart := p.cur
		p.advance()
		if p.isKw("TRIGGER") {
			p.advance()
			p.parseNameListOrAll()
		} else {
			p.skipNoopAlterTail()
		}
		s.Actions = append(s.Actions, ast.AlterAction{Kind: "NOOP", NoopText: p.textFrom(actStart)})
	default:
		actStart := p.cur
		p.skipNoopAlterTail()
		s.Actions = append(s.Actions, ast.AlterAction{Kind: "NOOP", NoopText: p.textFrom(actStart)})
	}
	return s
}

// parseNameListOrAll parses `ALL` or `name [, name]*`.
func (p *Parser) parseNameListOrAll() {
	if p.isKw("ALL") {
		p.advance()
		return
	}
	for {
		p.parseIdent()
		if !p.isPunct(",") {
			return
		}
		p.advance()
	}
}

// skipNoopAlterTail consumes an ALTER TABLE action the migration does not
// model, up to the statement end.
func (p *Parser) skipNoopAlterTail() {
	for p.cur.Kind != TOK_EOF && p.cur.Kind != TOK_GO && !p.isPunct(";") {
		if p.cur.LineStart && p.isStmtStartKeyword() && !p.isKw("SET") {
			return
		}
		if p.isPunct("(") {
			p.skipBalancedParens()
			continue
		}
		p.advance()
	}
}

// parseAlterAddElement parses one element of ALTER TABLE … ADD: a column,
// a constraint (WITH NOCHECK marks it NoValidate), a named / unnamed
// `DEFAULT expr FOR col` (SET_DEFAULT action) or a PERIOD FOR
// SYSTEM_TIME (NOOP action).
func (p *Parser) parseAlterAddElement(noCheck bool) []ast.AlterAction {
	if p.isKw("CONSTRAINT") && p.peekIsKw(2, "DEFAULT") || p.isKw("DEFAULT") {
		name := ""
		if p.isKw("CONSTRAINT") {
			p.advance()
			name, _ = p.parseIdent()
		}
		p.expectKw("DEFAULT")
		exprStart := p.cur
		e := p.parseExpr()
		raw := p.textFrom(exprStart)
		p.expectKw("FOR")
		col, _ := p.parseIdent()
		if p.isKw("WITH") && p.peekIsKw(1, "VALUES") {
			p.advance()
			p.advance()
		}
		return []ast.AlterAction{{Kind: "SET_DEFAULT", DropName: col, DefaultExpr: raw, Default: e, ConstraintName: name}}
	}
	switch {
	case p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") || p.isKw("FOREIGN") || p.isKw("CHECK"):
		c := p.parseTableConstraint()
		if c == nil {
			return nil
		}
		if noCheck {
			switch x := c.(type) {
			case *ast.FKConstraint:
				x.State.NoValidate = true
			case *ast.CheckConstraint:
				x.State.NoValidate = true
			}
		}
		return []ast.AlterAction{{Kind: "ADD_CONSTRAINT", Constraint: c}}
	case p.isKw("PERIOD") && p.peekIsKw(1, "FOR"):
		start := p.cur
		p.advance()
		p.advance()
		p.expectKw("SYSTEM_TIME")
		p.parseIdentList()
		return []ast.AlterAction{{Kind: "NOOP", NoopText: "ADD " + p.textFrom(start)}}
	}
	col, cons := p.parseColumnDef()
	if col == nil {
		return nil
	}
	out := []ast.AlterAction{{Kind: "ADD_COLUMN", Column: col}}
	for _, c := range cons {
		out = append(out, ast.AlterAction{Kind: "ADD_CONSTRAINT", Constraint: c})
	}
	return out
}

// parseAlterColumn parses the part after ALTER TABLE t ALTER COLUMN.
func (p *Parser) parseAlterColumn() ast.AlterAction {
	start := p.cur
	name, quoted := p.parseIdent()
	if p.isKw("ADD") || p.isKw("DROP") {
		// ALTER COLUMN c ADD|DROP ROWGUIDCOL | PERSISTED | NOT FOR
		// REPLICATION | SPARSE | HIDDEN | MASKED
		p.skipNoopAlterTail()
		return ast.AlterAction{Kind: "NOOP", NoopText: "ALTER COLUMN " + p.textFrom(start)}
	}
	col := &ast.ColumnDef{Name: name, NameBacktick: quoted, P: astPos(start.Pos)}
	col.Type = p.parseDataType()
	for {
		switch {
		case p.isKw("COLLATE"):
			p.advance()
			col.Collation, _ = p.parseIdent()
		case p.isKw("NOT") && p.peekIsKw(1, "NULL"):
			p.advance()
			p.advance()
			col.NotNull, col.HasNullable = true, true
		case p.isKw("NULL"):
			p.advance()
			col.HasNullable = true
		case p.isKw("SPARSE"):
			p.advance()
			col.Sparse = true
		case p.isKw("WITH") && p.peekIsPunct(1, "("):
			p.advance()
			p.skipBalancedParens()
		default:
			return ast.AlterAction{Kind: "MODIFY_COLUMN", Column: col}
		}
	}
}

// parseAlterDrop parses the list after ALTER TABLE t DROP.
func (p *Parser) parseAlterDrop() []ast.AlterAction {
	var out []ast.AlterAction
	if p.isKw("PERIOD") {
		start := p.cur
		p.advance()
		p.expectKw("FOR")
		p.expectKw("SYSTEM_TIME")
		return []ast.AlterAction{{Kind: "NOOP", NoopText: "DROP " + p.textFrom(start)}}
	}
	kind := "DROP_CONSTRAINT"
	for {
		switch {
		case p.isKw("CONSTRAINT"):
			p.advance()
			kind = "DROP_CONSTRAINT"
		case p.isKw("COLUMN"):
			p.advance()
			kind = "DROP_COLUMN"
		case p.isKw("INDEX"):
			p.advance()
			kind = "DROP_INDEX"
		}
		if p.isKw("IF") && p.peekIsKw(1, "EXISTS") {
			p.advance()
			p.advance()
		}
		name, _ := p.parseIdent()
		if kind == "DROP_INDEX" {
			out = append(out, ast.AlterAction{Kind: "NOOP", NoopText: "DROP INDEX " + name})
		} else {
			out = append(out, ast.AlterAction{Kind: kind, DropName: name})
		}
		if p.isKw("WITH") && p.peekIsPunct(1, "(") {
			p.advance()
			p.skipBalancedParens()
		}
		if !p.isPunct(",") {
			return out
		}
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// DROP
// ---------------------------------------------------------------------------

// parseDrop parses DROP TABLE / VIEW / PROCEDURE / FUNCTION / TRIGGER /
// TYPE / SEQUENCE / SCHEMA / SYNONYM / INDEX; other DROPs are no-ops.
func (p *Parser) parseDrop() ast.Stmt {
	start := p.cur
	p.expectKw("DROP")
	kind := ""
	switch {
	case p.isKw("TABLE"):
		p.advance()
		s := &ast.DropTable{P: astPos(start.Pos)}
		if p.isKw("IF") && p.peekIsKw(1, "EXISTS") {
			p.advance()
			p.advance()
			s.IfExists = true
		}
		for {
			s.Tables = append(s.Tables, p.parseTableRef())
			if !p.isPunct(",") {
				return s
			}
			p.advance()
		}
	case p.isKw("VIEW"):
		kind = "VIEW"
	case p.isKw("PROC") || p.isKw("PROCEDURE"):
		kind = "PROCEDURE"
	case p.isKw("FUNCTION"):
		kind = "FUNCTION"
	case p.isKw("TRIGGER"):
		kind = "TRIGGER"
	case p.isKw("TYPE"):
		kind = "TYPE"
	case p.isKw("SEQUENCE"):
		kind = "SEQUENCE"
	case p.isKw("SCHEMA"):
		kind = "SCHEMA"
	case p.isKw("SYNONYM"):
		kind = "SYNONYM"
	case p.isKw("INDEX"):
		return p.parseDropIndex(start)
	}
	if kind == "" {
		p.reset(p.indexOf(start), len(p.errs))
		return p.parseNoopToStmtEnd("DROP "+strings.ToUpper(p.peek(1).Raw), false)
	}
	p.advance()
	s := &ast.DropObject{Kind: kind, P: astPos(start.Pos)}
	if p.isKw("IF") && p.peekIsKw(1, "EXISTS") {
		p.advance()
		p.advance()
		s.IfExists = true
	}
	for {
		s.Names = append(s.Names, p.parseTableRef())
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	if kind == "TRIGGER" && p.isKw("ON") {
		// DROP TRIGGER t ON DATABASE | ALL SERVER (DDL trigger)
		p.advance()
		if p.isKw("ALL") {
			p.advance()
		}
		p.advance()
	}
	if len(s.Names) == 1 {
		s.Schema, s.Name = s.Names[0].Schema, s.Names[0].Name
		s.Names = nil
	}
	return s
}

// parseDropIndex parses `DROP INDEX [IF EXISTS] ix ON t [, …]` and the
// legacy `DROP INDEX t.ix [, …]`.
func (p *Parser) parseDropIndex(start Token) ast.Stmt {
	p.expectKw("INDEX")
	s := &ast.DropObject{Kind: "INDEX", P: astPos(start.Pos)}
	if p.isKw("IF") && p.peekIsKw(1, "EXISTS") {
		p.advance()
		p.advance()
		s.IfExists = true
	}
	parts, quoted := p.parseMultiPartName()
	if p.isKw("ON") {
		p.advance()
		s.Name = parts[len(parts)-1]
		s.OnTable = p.parseTableRef()
	} else if len(parts) >= 2 {
		s.Name = parts[len(parts)-1]
		s.OnTable = tableRefOf(parts[:len(parts)-1], quoted)
	} else {
		s.Name = parts[0]
	}
	if p.isKw("WITH") && p.peekIsPunct(1, "(") {
		p.advance()
		p.skipBalancedParens()
	}
	for p.isPunct(",") {
		// further indexes of the same statement are dropped from the node
		// but still consumed.
		p.advance()
		p.parseMultiPartName()
		if p.isKw("ON") {
			p.advance()
			p.parseTableRef()
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// EXEC sp_addextendedproperty → CommentStmt
// ---------------------------------------------------------------------------

// execTargetIs reports whether the current EXEC / EXECUTE statement calls
// the system procedure name (any qualification, any case).
func (p *Parser) execTargetIs(name string) bool {
	j := p.i + 1
	if j < len(p.toks) && p.toks[j].Kind == TOK_VARIABLE && j+1 < len(p.toks) && p.toks[j+1].Kind == TOK_PUNCT && p.toks[j+1].Lit == "=" {
		j += 2 // EXEC @rc = proc
	}
	last := ""
	for j < len(p.toks) {
		t := p.toks[j]
		if t.Kind != TOK_IDENT && t.Kind != TOK_QUOTED_IDENT {
			break
		}
		last = t.Lit
		if j+1 < len(p.toks) && p.toks[j+1].Kind == TOK_PUNCT && p.toks[j+1].Lit == "." {
			j += 2
			continue
		}
		break
	}
	return strings.EqualFold(last, name)
}

// extendedPropertyArgs are the parameter names of sp_addextendedproperty,
// in positional order.
var extendedPropertyArgs = []string{
	"@name", "@value", "@level0type", "@level0name",
	"@level1type", "@level1name", "@level2type", "@level2name",
}

// parseExtendedProperty parses `EXEC [sys.]sp_addextendedproperty …`
// (positional or named arguments). An MS_Description property becomes an
// *ast.CommentStmt; any other property is a no-op.
func (p *Parser) parseExtendedProperty() ast.Stmt {
	start := p.cur
	call, ok := p.parseExecBody(start.Pos, true).(*ast.CallStmt)
	if !ok {
		return p.noopFrom(start, "EXEC")
	}
	vals := map[string]ast.Expr{}
	for i, a := range call.Args {
		if na, isNamed := a.(*ast.NamedArg); isNamed && na.Name != "" {
			vals[strings.ToLower(na.Name)] = na.Value
			continue
		}
		if i < len(extendedPropertyArgs) {
			vals[extendedPropertyArgs[i]] = a
		}
	}
	str := func(key string) (string, bool) {
		switch v := vals[key].(type) {
		case *ast.Literal:
			if v.Kind == "null" {
				return "", false
			}
			return v.Text, true
		case *ast.Ident:
			if len(v.Parts) == 1 && !v.Variable {
				return v.Parts[0], true
			}
		}
		return "", false
	}
	propName, _ := str("@name")
	if !strings.EqualFold(propName, "MS_Description") {
		return p.noopFrom(start, "EXEC SP_ADDEXTENDEDPROPERTY")
	}
	cs := &ast.CommentStmt{ObjectKind: "DATABASE", P: astPos(start.Pos)}
	if lit, isLit := vals["@value"].(*ast.Literal); isLit && lit.Kind == "null" || vals["@value"] == nil {
		cs.IsNull = true
	} else if text, okText := str("@value"); okText {
		cs.Text = text
	} else {
		// A computed value (variable, expression) cannot become a static
		// COMMENT: keep the statement as a no-op.
		return p.noopFrom(start, "EXEC SP_ADDEXTENDEDPROPERTY")
	}
	if t0, ok0 := str("@level0type"); ok0 {
		cs.ObjectKind = strings.ToUpper(t0)
		name0, _ := str("@level0name")
		if cs.ObjectKind != "SCHEMA" {
			// Database-scoped level-0 objects (FILEGROUP, ASSEMBLY, USER,
			// a DDL TRIGGER, …) have no schema.
			cs.Object = name0
			return cs
		}
		cs.Schema = name0
		if t1, ok1 := str("@level1type"); ok1 {
			cs.ObjectKind = strings.ToUpper(t1)
			cs.Object, _ = str("@level1name")
			if t2, ok2 := str("@level2type"); ok2 {
				cs.ParentKind = cs.ObjectKind
				cs.ObjectKind = strings.ToUpper(t2)
				cs.Column, _ = str("@level2name")
			}
		}
	}
	return cs
}
