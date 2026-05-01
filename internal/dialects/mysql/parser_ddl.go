package mysql

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateTable(start Position, temporary, orReplace bool) ast.Stmt {
	p.expectKw("TABLE")
	s := &ast.CreateTable{Temporary: temporary, OrReplace: orReplace, P: astPos(start)}

	if p.isKw("IF") {
		p.advance()
		p.expectKw("NOT")
		p.expectKw("EXISTS")
		s.IfNotExists = true
	}

	ref := p.parseTableRef()
	s.Schema = ref.Schema
	s.Name = ref.Name
	s.NameBacktick = ref.NameBacktick

	// `CREATE TABLE dst LIKE src` — copy-table form. The paren-LIKE form
	// (`CREATE TABLE dst (LIKE src)`) is handled below after we open the paren.
	if p.isKw("LIKE") {
		p.advance()
		src := p.parseTableRef()
		return &ast.CreateTableLike{
			Schema: s.Schema, Name: s.Name, NameBacktick: s.NameBacktick,
			IfNotExists: s.IfNotExists, Temporary: temporary,
			LikeSchema: src.Schema, LikeName: src.Name,
			P: astPos(start),
		}
	}

	// `CREATE TABLE dst AS <select>` (no column list, AS form).
	if p.isKw("AS") || p.isKw("SELECT") || p.isKw("IGNORE") || p.isKw("REPLACE") {
		return p.finishCreateTableAs(s, start, "")
	}

	if !p.expectPunct("(") {
		p.syncToDelimiter()
		return s
	}

	// Paren-LIKE form: `CREATE TABLE dst (LIKE src)`.
	if p.isKw("LIKE") {
		p.advance()
		src := p.parseTableRef()
		p.expectPunct(")")
		return &ast.CreateTableLike{
			Schema: s.Schema, Name: s.Name, NameBacktick: s.NameBacktick,
			IfNotExists: s.IfNotExists, Temporary: temporary,
			LikeSchema: src.Schema, LikeName: src.Name,
			P: astPos(start),
		}
	}

	for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") || p.isKw("FOREIGN") || p.isKw("CHECK"):
			if c := p.parseTableConstraint(); c != nil {
				s.Constraints = append(s.Constraints, c)
			}
		case p.isKw("INDEX") || p.isKw("KEY") || p.isKw("FULLTEXT") || p.isKw("SPATIAL") || p.isKw("VECTOR"):
			if idx := p.parseIndexDef(); idx != nil {
				s.Indexes = append(s.Indexes, idx)
			}
		case p.isKw("PERIOD"):
			// MariaDB application- or system-time period:
			//   PERIOD FOR <name> (start_col, end_col)
			//   PERIOD FOR SYSTEM_TIME (start_col, end_col)
			// Postgres has no native PERIOD FOR clause; we capture the metadata
			// so the translator can surface a blocking prerequisite (history
			// table + triggers, or temporal_tables extension).
			p.advance()
			p.expectKw("FOR")
			pd := ast.PeriodDef{}
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
				pd.Name = p.cur.Lit
				p.advance()
			}
			if p.isPunct("(") {
				p.advance()
				if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
					pd.StartCol = p.cur.Lit
					p.advance()
				}
				if p.isPunct(",") {
					p.advance()
				}
				if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
					pd.EndCol = p.cur.Lit
					p.advance()
				}
				// Tolerate trailing tokens before the closing paren.
				for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
					p.advance()
				}
				if p.isPunct(")") {
					p.advance()
				}
			}
			s.Periods = append(s.Periods, pd)
			if strings.EqualFold(pd.Name, "SYSTEM_TIME") {
				s.SystemVersioned = true
			}
		default:
			if col := p.parseColumnDef(); col != nil {
				s.Columns = append(s.Columns, col)
			}
		}
		if p.isPunct(",") {
			p.advance()
			continue
		}
		break
	}
	p.expectPunct(")")

	s.Options = p.parseTableOptions()
	if s.Options.SystemVersioning {
		s.SystemVersioned = true
	}
	if p.isKw("PARTITION") {
		s.Partitioning = p.parsePartitionBy()
	}
	// `queryCreateTable` form: optional [IGNORE|REPLACE] [AS] <select> tail
	// after column defs / table options / partition spec.
	if p.isKw("IGNORE") || p.isKw("REPLACE") || p.isKw("AS") || p.isKw("SELECT") {
		return p.finishCreateTableAs(s, start, "")
	}
	return s
}

// finishCreateTableAs builds a CreateTableAs node from the partial CreateTable
// state already parsed (table name, optional column defs, table options) plus
// the trailing `[IGNORE|REPLACE] [AS]? <select>` tail. The select body is
// captured verbatim from the source so all MySQL-specific syntax round-trips
// through the explanation; the translator decides whether to emit it.
func (p *Parser) finishCreateTableAs(base *ast.CreateTable, start Position, _ string) *ast.CreateTableAs {
	out := &ast.CreateTableAs{
		Schema: base.Schema, Name: base.Name, NameBacktick: base.NameBacktick,
		IfNotExists: base.IfNotExists, Temporary: base.Temporary,
		Columns: base.Columns, Options: base.Options,
		P: astPos(start),
	}
	if p.isKw("IGNORE") || p.isKw("REPLACE") {
		out.KeyConflict = p.cur.Lit
		p.advance()
	}
	if p.isKw("AS") {
		p.advance()
	}
	out.SelectBody = p.captureUntilDelimiter()
	return out
}

// parsePartitionBy implements the MySqlParser.g4 `partitionDefinitions` rule:
//
//	PARTITION BY partitionFunctionDefinition (PARTITIONS N)?
//	  (SUBPARTITION BY subpartitionFunctionDefinition (SUBPARTITIONS N)?)?
//	  ('(' partitionDefinition (',' partitionDefinition)* ')')?
//
// The function tolerates the variants documented in the grammar; unknown
// pieces are consumed best-effort so an unsupported partitioning shape never
// blocks the rest of the statement from parsing.
func (p *Parser) parsePartitionBy() *ast.Partitioning {
	p.expectKw("PARTITION")
	p.expectKw("BY")
	pn := &ast.Partitioning{}
	p.parsePartitionFunc(pn, false)

	if p.isKw("PARTITIONS") {
		p.advance()
		if p.cur.Kind == TOK_NUMBER {
			pn.Count, _ = strconv.Atoi(p.cur.Lit)
			p.advance()
		}
	}

	if p.isKw("SUBPARTITION") {
		p.advance()
		p.expectKw("BY")
		sub := &ast.Subpartitioning{}
		p.parseSubpartitionFunc(sub)
		if p.isKw("SUBPARTITIONS") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				sub.Count, _ = strconv.Atoi(p.cur.Lit)
				p.advance()
			}
		}
		pn.Subpartition = sub
	}

	// Optional definition list.
	if p.isPunct("(") {
		p.advance()
		for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
			d := p.parsePartitionDefinition()
			if d != nil {
				pn.Definitions = append(pn.Definitions, *d)
			}
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
	}
	return pn
}

// parsePartitionFunc fills the function-shape fields of either a Partitioning
// or a Subpartitioning. `subOnly` restricts the accepted methods to HASH/KEY.
func (p *Parser) parsePartitionFunc(pn *ast.Partitioning, subOnly bool) {
	if p.isKw("LINEAR") {
		p.advance()
		pn.Linear = true
	}
	switch {
	case p.isKw("HASH"):
		p.advance()
		pn.Method = "HASH"
		p.expectPunct("(")
		pn.ExprText = p.captureBalancedExpr()
	case p.isKw("KEY"):
		p.advance()
		pn.Method = "KEY"
		if p.isKw("ALGORITHM") {
			p.advance()
			p.expectPunct("=")
			if p.cur.Kind == TOK_NUMBER {
				pn.KeyAlgorithm, _ = strconv.Atoi(p.cur.Lit)
				p.advance()
			}
		}
		p.expectPunct("(")
		// Empty list is allowed (KEY () means "use the PK").
		if !p.isPunct(")") {
			pn.Columns = p.parsePartitionIdentList()
		}
		p.expectPunct(")")
	case !subOnly && p.isKw("RANGE"):
		p.advance()
		if p.isKw("COLUMNS") {
			p.advance()
			pn.Method = "RANGE COLUMNS"
			p.expectPunct("(")
			pn.Columns = p.parsePartitionIdentList()
			p.expectPunct(")")
		} else {
			pn.Method = "RANGE"
			p.expectPunct("(")
			pn.ExprText = p.captureBalancedExpr()
		}
	case !subOnly && p.isKw("LIST"):
		p.advance()
		if p.isKw("COLUMNS") {
			p.advance()
			pn.Method = "LIST COLUMNS"
			p.expectPunct("(")
			pn.Columns = p.parsePartitionIdentList()
			p.expectPunct(")")
		} else {
			pn.Method = "LIST"
			p.expectPunct("(")
			pn.ExprText = p.captureBalancedExpr()
		}
	default:
		p.errorHere("unsupported partition function", "HASH|KEY|RANGE|LIST")
	}
}

func (p *Parser) parseSubpartitionFunc(sub *ast.Subpartitioning) {
	tmp := &ast.Partitioning{}
	p.parsePartitionFunc(tmp, true)
	sub.Method = tmp.Method
	sub.Linear = tmp.Linear
	sub.KeyAlgorithm = tmp.KeyAlgorithm
	sub.ExprText = tmp.ExprText
	sub.Columns = tmp.Columns
}

// parsePartitionIdentList parses `ident (, ident)*` already inside the parens.
func (p *Parser) parsePartitionIdentList() []string {
	var cols []string
	for {
		n, _ := p.parseIdent()
		if n != "" {
			cols = append(cols, n)
		}
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return cols
}

// captureBalancedExpr reads source between '(' (already consumed by the
// caller) and the matching ')', and returns the inner text trimmed. The
// closing ')' is consumed.
func (p *Parser) captureBalancedExpr() string {
	startOff := p.cur.Pos.Offset
	depth := 1
	for p.cur.Kind != TOK_EOF {
		if p.isPunct("(") {
			depth++
			p.advance()
			continue
		}
		if p.isPunct(")") {
			depth--
			if depth == 0 {
				endOff := p.cur.Pos.Offset
				out := strings.TrimSpace(string(p.src[startOff:endOff]))
				p.advance()
				return out
			}
			p.advance()
			continue
		}
		p.advance()
	}
	return ""
}

// captureAtom reads source up to the next top-level ',' or ')' and returns it
// trimmed. Parens nest; single-quoted strings are honored.
func (p *Parser) captureAtom() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	for p.cur.Kind != TOK_EOF {
		if depth == 0 && (p.isPunct(",") || p.isPunct(")")) {
			endOff := p.cur.Pos.Offset
			return strings.TrimSpace(string(p.src[startOff:endOff]))
		}
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
		}
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	return strings.TrimSpace(string(p.src[startOff:endOff]))
}

func (p *Parser) parsePartitionDefinition() *ast.PartitionDefinition {
	if !p.isKw("PARTITION") {
		p.errorHere("expected PARTITION", "PARTITION")
		return nil
	}
	p.advance()
	name, _ := p.parseIdent()
	d := &ast.PartitionDefinition{Name: name}

	if p.isKw("VALUES") {
		p.advance()
		switch {
		case p.isKw("LESS"):
			p.advance()
			p.expectKw("THAN")
			d.HasLessThan = true
			if p.isKw("MAXVALUE") {
				// `VALUES LESS THAN MAXVALUE` (no parens shortcut).
				p.advance()
				d.LessThan = []string{"MAXVALUE"}
			} else if p.isPunct("(") {
				p.advance()
				for {
					if p.isKw("MAXVALUE") {
						p.advance()
						d.LessThan = append(d.LessThan, "MAXVALUE")
					} else {
						d.LessThan = append(d.LessThan, p.captureAtom())
					}
					if p.isPunct(",") {
						p.advance()
						continue
					}
					break
				}
				p.expectPunct(")")
			} else {
				// Single bound expression without parens.
				d.LessThan = append(d.LessThan, p.captureAtom())
			}
		case p.isKw("IN"):
			p.advance()
			d.HasIn = true
			p.expectPunct("(")
			for {
				if p.isPunct("(") {
					// Vector form: (a1, a2, …)
					p.advance()
					var vec []string
					for {
						vec = append(vec, p.captureAtom())
						if p.isPunct(",") {
							p.advance()
							continue
						}
						break
					}
					p.expectPunct(")")
					d.InVectors = append(d.InVectors, vec)
				} else {
					d.InAtoms = append(d.InAtoms, p.captureAtom())
				}
				if p.isPunct(",") {
					p.advance()
					continue
				}
				break
			}
			p.expectPunct(")")
		}
	}

	// partitionOption*: ENGINE=, COMMENT=, DATA DIRECTORY=, INDEX DIRECTORY=,
	// MAX_ROWS=, MIN_ROWS=, TABLESPACE=, NODEGROUP=. Silently consumed.
	for p.consumePartitionOption() {
	}

	// Optional inline subpartition list.
	if p.isPunct("(") {
		p.advance()
		for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
			if p.isKw("SUBPARTITION") {
				p.advance()
				sn, _ := p.parseIdent()
				d.Subpartitions = append(d.Subpartitions, ast.SubpartitionDef{Name: sn})
				for p.consumePartitionOption() {
				}
			} else {
				p.advance() // tolerate unknown junk
			}
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
	}
	return d
}

// consumePartitionOption consumes one partitionOption form (ENGINE / COMMENT /
// DATA DIRECTORY / INDEX DIRECTORY / MAX_ROWS / MIN_ROWS / TABLESPACE /
// NODEGROUP) and returns true if it consumed one. PG has no equivalent for
// any of these, so the values are discarded — only their shape needs to be
// recognised so the parser doesn't choke on them.
func (p *Parser) consumePartitionOption() bool {
	if p.isKw("STORAGE") {
		p.advance()
	}
	switch {
	case p.isKw("ENGINE"):
		p.advance()
		p.consumePunct("=")
		p.eatIdentOrKw()
		return true
	case p.isKw("COMMENT"):
		p.advance()
		p.consumePunct("=")
		if p.cur.Kind == TOK_STRING {
			p.advance()
		}
		return true
	case p.isKw("DATA") || p.isKw("INDEX"):
		p.advance()
		if p.isKw("DIRECTORY") {
			p.advance()
		}
		p.consumePunct("=")
		if p.cur.Kind == TOK_STRING {
			p.advance()
		}
		return true
	case p.isKw("MAX_ROWS") || p.isKw("MIN_ROWS"):
		p.advance()
		p.consumePunct("=")
		if p.cur.Kind == TOK_NUMBER {
			p.advance()
		}
		return true
	case p.isKw("TABLESPACE") || p.isKw("NODEGROUP"):
		p.advance()
		p.consumePunct("=")
		p.eatIdentOrKw()
		return true
	}
	return false
}

func (p *Parser) parseTableOptions() ast.TableOptions {
	var opts ast.TableOptions
	for {
		if p.atStatementEnd() || p.cur.Kind == TOK_EOF {
			return opts
		}
		// PARTITION BY / SUBPARTITION BY ends the table-option list — handled
		// by the caller via parsePartitionBy.
		if p.isKw("PARTITION") || p.isKw("SUBPARTITION") {
			return opts
		}
		// `[IGNORE|REPLACE] [AS] <select>` tail of `queryCreateTable` —
		// handled by the caller via finishCreateTableAs.
		if p.isKw("IGNORE") || p.isKw("REPLACE") || p.isKw("AS") || p.isKw("SELECT") {
			return opts
		}
		// "DEFAULT" prefix before CHARSET / CHARACTER SET / COLLATE is a noise word.
		if p.isKw("DEFAULT") {
			p.advance()
		}
		switch {
		case p.isKw("ENGINE"):
			p.advance()
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				opts.Engine = v
			}
		case p.isKw("CHARSET"):
			p.advance()
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				opts.Charset = v
			}
		case p.isKw("CHARACTER"):
			p.advance()
			p.expectKw("SET")
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				opts.Charset = v
			}
		case p.isKw("COLLATE"):
			p.advance()
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				opts.Collate = v
			}
		case p.isKw("AUTO_INCREMENT"):
			p.advance()
			p.consumePunct("=")
			if p.cur.Kind == TOK_NUMBER {
				if n, err := strconv.ParseInt(p.cur.Lit, 10, 64); err == nil {
					opts.AutoIncrement = n
					opts.HasAutoInc = true
				}
				p.advance()
			}
		case p.isKw("COMMENT"):
			p.advance()
			p.consumePunct("=")
			if p.cur.Kind == TOK_STRING {
				opts.Comment = p.cur.Lit
				p.advance()
			}
		case p.isKw("ROW_FORMAT"):
			p.advance()
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				opts.RowFormat = v
			}
		case p.isKw("KEY_BLOCK_SIZE"):
			p.advance()
			p.consumePunct("=")
			if p.cur.Kind == TOK_NUMBER {
				if n, err := strconv.Atoi(p.cur.Lit); err == nil {
					opts.KeyBlockSize = n
				}
				p.advance()
			}
		case p.isKw("WITH") || p.isKw("WITHOUT"):
			// MariaDB: WITH SYSTEM VERSIONING  /  WITHOUT SYSTEM VERSIONING.
			// PG has no equivalent — flag it so the translator can warn.
			leading := p.cur.Lit
			p.advance()
			if p.isKw("SYSTEM") {
				p.advance()
				if p.isKw("VERSIONING") {
					p.advance()
					opts.SystemVersioning = leading == "WITH"
				}
			}
		case p.isKw("DATA") || p.isKw("INDEX"):
			// `DATA DIRECTORY = '...'` / `INDEX DIRECTORY = '...'` — two-keyword
			// option names that the unknown-K=V default branch would split on
			// the first space. PG has no equivalent; we silently consume.
			leading := p.cur.Lit
			p.advance()
			if !p.isKw("DIRECTORY") {
				// Not a valid two-keyword option — bail to default branch.
				opts.Extras = append(opts.Extras, leading)
				continue
			}
			p.advance()
			p.consumePunct("=")
			if p.cur.Kind == TOK_STRING {
				p.advance()
			}
		case p.isKw("TABLESPACE"):
			p.advance()
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				_ = v
			}
			// Optional trailing `STORAGE {DISK|MEMORY|DEFAULT}`.
			if p.isKw("STORAGE") {
				p.advance()
				if p.isKw("DISK") || p.isKw("MEMORY") || p.isKw("DEFAULT") {
					p.advance()
				}
			}
		case p.isKw("UNION"):
			// `UNION = (t1, t2, …)` for the MERGE engine. Just consume the
			// parens; we never replicate the merge semantics.
			p.advance()
			p.consumePunct("=")
			if p.isPunct("(") {
				depth := 0
				for p.cur.Kind != TOK_EOF {
					if p.isPunct("(") {
						depth++
					} else if p.isPunct(")") {
						depth--
						if depth == 0 {
							p.advance()
							break
						}
					}
					p.advance()
				}
			}
		default:
			// unknown option: try to eat "K = V" pair for forward compat
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
				name := p.cur.Lit
				p.advance()
				p.consumePunct("=")
				val := ""
				if p.cur.Kind == TOK_STRING || p.cur.Kind == TOK_NUMBER || p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
					val = p.cur.Lit
					p.advance()
				}
				opts.Extras = append(opts.Extras, name+"="+val)
				continue
			}
			return opts
		}
	}
}

func (p *Parser) consumePunct(lit string) bool {
	if p.isPunct(lit) {
		p.advance()
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Column definition
// ---------------------------------------------------------------------------

func (p *Parser) parseColumnDef() *ast.ColumnDef {
	start := p.cur.Pos
	name, bt := p.parseIdent()
	if name == "" {
		p.syncToDelimiter()
		return nil
	}
	col := &ast.ColumnDef{Name: name, NameBacktick: bt, P: astPos(start)}
	col.Type = p.parseDataType()
	if col.Type == nil {
		return nil
	}

optionsLoop:
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
			col.Default = p.parseDefaultExpr()
			col.HasDefault = true
		case p.isKw("ON"):
			p.advance()
			p.expectKw("UPDATE")
			col.OnUpdate = p.parseDefaultExpr()
		case p.isKw("AUTO_INCREMENT"):
			p.advance()
			col.AutoInc = true
		case p.isKw("UNIQUE"):
			p.advance()
			if p.isKw("KEY") {
				p.advance()
			}
			col.Unique = true
		case p.isKw("PRIMARY"):
			p.advance()
			p.expectKw("KEY")
			col.PrimaryKey = true
		case p.isKw("KEY"):
			p.advance()
			col.PrimaryKey = true
		case p.isKw("COMMENT"):
			p.advance()
			if p.cur.Kind == TOK_STRING {
				col.Comment = p.cur.Lit
				p.advance()
			}
		case p.isKw("COLLATE"):
			p.advance()
			if v, ok := p.eatIdentOrKw(); ok {
				col.Collation = v
			}
		case p.isKw("CHARACTER"):
			p.advance()
			p.expectKw("SET")
			if v, ok := p.eatIdentOrKw(); ok {
				col.Charset = v
			}
		case p.isKw("GENERATED"):
			p.advance()
			p.expectKw("ALWAYS")
			p.expectKw("AS")
			if p.isKw("ROW") {
				// MariaDB system-versioning period columns:
				//   GENERATED ALWAYS AS ROW START
				//   GENERATED ALWAYS AS ROW END
				// PG has no equivalent. Mark the column so the translator can
				// drop it entirely (and rewrite any PK that references it).
				p.advance()
				if p.isKw("START") || p.isKw("END") {
					p.advance()
				}
				col.SystemVersioning = true
			} else {
				p.expectPunct("(")
				e := p.parseExpr()
				p.expectPunct(")")
				col.Generated = &ast.Generated{Expr: e}
				if p.isKw("VIRTUAL") {
					p.advance()
					col.Generated.Virtual = true
					col.Generated.HasVirtual = true
				} else if p.isKw("STORED") {
					p.advance()
					col.Generated.Virtual = false
					col.Generated.HasVirtual = true
				}
			}
		case p.isKw("AS"):
			// MySQL 5.7 short form
			p.advance()
			p.expectPunct("(")
			e := p.parseExpr()
			p.expectPunct(")")
			col.Generated = &ast.Generated{Expr: e}
			if p.isKw("VIRTUAL") {
				p.advance()
				col.Generated.Virtual = true
				col.Generated.HasVirtual = true
			} else if p.isKw("STORED") {
				p.advance()
				col.Generated.Virtual = false
				col.Generated.HasVirtual = true
			}
		case p.isKw("CHECK"):
			p.advance()
			p.expectPunct("(")
			col.Check = p.parseExpr()
			p.expectPunct(")")
			// optional NOT ENFORCED
			if p.isKw("NOT") {
				p.advance()
				p.expectKw("ENFORCED")
			} else if p.isKw("ENFORCED") {
				p.advance()
			}
		case p.isKw("INVISIBLE"):
			p.advance()
			col.Invisible = true
		case p.isKw("VISIBLE"):
			p.advance()
		case p.isKw("COMPRESSED"):
			// MariaDB per-column transparent compression:
			//   col TEXT COMPRESSED
			//   col TEXT COMPRESSED=zlib
			// PG's TOAST handles equivalent compression automatically — flag
			// for the translator to surface as info.
			p.advance()
			col.Compressed = true
			if p.isPunct("=") {
				p.advance()
				if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
					p.advance()
				}
			}
		case p.isKw("STORAGE"):
			p.advance()
			if p.isKw("DISK") || p.isKw("MEMORY") {
				p.advance()
			}
		default:
			break optionsLoop
		}
	}

	return col
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

func (p *Parser) parseDataType() ast.DataType {
	start := p.cur.Pos
	if p.cur.Kind != TOK_KEYWORD {
		p.errorHere("expected data type", "DATA_TYPE")
		return nil
	}
	name := p.cur.Lit

	switch name {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT":
		p.advance()
		t := &ast.IntType{Name: name, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Width, _ = strconv.Atoi(p.cur.Lit)
				t.HasWidth = true
				p.advance()
			}
			p.expectPunct(")")
		}
		if p.isKw("UNSIGNED") {
			p.advance()
			t.Unsigned = true
		}
		if p.isKw("ZEROFILL") {
			p.advance()
			t.Zerofill = true
		}
		return t
	case "FLOAT", "DOUBLE", "REAL":
		p.advance()
		t := &ast.FloatType{Name: name, P: astPos(start)}
		if name == "DOUBLE" && p.isKw("PRECISION") {
			p.advance()
		}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Precision, _ = strconv.Atoi(p.cur.Lit)
				p.advance()
			}
			if p.isPunct(",") {
				p.advance()
				if p.cur.Kind == TOK_NUMBER {
					t.Scale, _ = strconv.Atoi(p.cur.Lit)
					p.advance()
				}
				t.HasPS = true
			}
			p.expectPunct(")")
		}
		for {
			if p.isKw("UNSIGNED") {
				p.advance()
				t.Unsigned = true
			} else if p.isKw("SIGNED") {
				p.advance()
			} else if p.isKw("ZEROFILL") {
				p.advance()
				t.Zerofill = true
			} else {
				break
			}
		}
		return t
	case "DECIMAL", "NUMERIC", "DEC":
		p.advance()
		t := &ast.DecimalType{Name: name, Precision: 10, Scale: 0, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Precision, _ = strconv.Atoi(p.cur.Lit)
				p.advance()
			}
			if p.isPunct(",") {
				p.advance()
				if p.cur.Kind == TOK_NUMBER {
					t.Scale, _ = strconv.Atoi(p.cur.Lit)
					p.advance()
				}
			}
			p.expectPunct(")")
			t.HasPrec = true
		}
		for {
			if p.isKw("UNSIGNED") {
				p.advance()
				t.Unsigned = true
			} else if p.isKw("SIGNED") {
				p.advance()
			} else if p.isKw("ZEROFILL") {
				p.advance()
				t.Zerofill = true
			} else {
				break
			}
		}
		return t
	case "BIT":
		p.advance()
		t := &ast.BitType{Width: 1, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Width, _ = strconv.Atoi(p.cur.Lit)
				p.advance()
			}
			p.expectPunct(")")
		}
		return t
	case "CHAR", "VARCHAR":
		p.advance()
		t := &ast.CharType{Name: name, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Length, _ = strconv.Atoi(p.cur.Lit)
				t.HasLength = true
				p.advance()
			}
			p.expectPunct(")")
		}
		p.consumeCharsetClause(&t.Charset, &t.Collation)
		return t
	case "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT":
		p.advance()
		t := &ast.TextType{Name: name, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Length, _ = strconv.Atoi(p.cur.Lit)
				t.HasLength = true
				p.advance()
			}
			p.expectPunct(")")
		}
		p.consumeCharsetClause(&t.Charset, &t.Collation)
		return t
	case "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB":
		p.advance()
		return &ast.BlobType{Name: name, P: astPos(start)}
	case "BINARY", "VARBINARY":
		p.advance()
		t := &ast.BinaryType{Name: name, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Length, _ = strconv.Atoi(p.cur.Lit)
				p.advance()
			}
			p.expectPunct(")")
		}
		return t
	case "ENUM":
		p.advance()
		t := &ast.EnumType{P: astPos(start)}
		p.expectPunct("(")
		for {
			if p.cur.Kind == TOK_STRING {
				t.Values = append(t.Values, p.cur.Lit)
				p.advance()
			} else {
				p.errorHere("expected enum string literal", "STRING")
				break
			}
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
		return t
	case "SET":
		p.advance()
		t := &ast.SetType{P: astPos(start)}
		p.expectPunct("(")
		for {
			if p.cur.Kind == TOK_STRING {
				t.Values = append(t.Values, p.cur.Lit)
				p.advance()
			} else {
				p.errorHere("expected set string literal", "STRING")
				break
			}
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
		return t
	case "JSON":
		p.advance()
		return &ast.JSONType{P: astPos(start)}
	case "DATE":
		p.advance()
		return &ast.DateType{P: astPos(start)}
	case "TIME":
		p.advance()
		t := &ast.TimeType{P: astPos(start)}
		t.Fsp = p.parseOptFsp()
		return t
	case "DATETIME":
		p.advance()
		t := &ast.DateTimeType{P: astPos(start)}
		t.Fsp = p.parseOptFsp()
		return t
	case "TIMESTAMP":
		p.advance()
		t := &ast.TimestampType{P: astPos(start)}
		t.Fsp = p.parseOptFsp()
		return t
	case "YEAR":
		p.advance()
		// MySQL accepts YEAR(4) but deprecates the width
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				p.advance()
			}
			p.expectPunct(")")
		}
		return &ast.YearType{P: astPos(start)}
	case "GEOMETRY", "POINT", "LINESTRING", "POLYGON",
		"MULTIPOINT", "MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION":
		p.advance()
		return &ast.SpatialType{Name: name, P: astPos(start)}
	case "VECTOR":
		// MariaDB 11.7+ VECTOR(N) — fixed-dimension float vector. Shares the
		// shared `ast.VectorType` node already used by Oracle 23ai.
		p.advance()
		t := &ast.VectorType{P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				t.Dim, _ = strconv.Atoi(p.cur.Lit)
				t.HasDim = true
				p.advance()
			}
			p.expectPunct(")")
		}
		return t
	}
	p.errorHere("unknown data type "+name, "DATA_TYPE")
	return nil
}

func (p *Parser) parseOptFsp() int {
	if !p.isPunct("(") {
		return 0
	}
	p.advance()
	fsp := 0
	if p.cur.Kind == TOK_NUMBER {
		fsp, _ = strconv.Atoi(p.cur.Lit)
		p.advance()
	}
	p.expectPunct(")")
	return fsp
}

func (p *Parser) consumeCharsetClause(charset, collation *string) {
	for {
		switch {
		case p.isKw("CHARACTER"):
			p.advance()
			p.expectKw("SET")
			if v, ok := p.eatIdentOrKw(); ok {
				*charset = v
			}
		case p.isKw("CHARSET"):
			p.advance()
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				*charset = v
			}
		case p.isKw("COLLATE"):
			p.advance()
			p.consumePunct("=")
			if v, ok := p.eatIdentOrKw(); ok {
				*collation = v
			}
		default:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Table constraints + indexes
// ---------------------------------------------------------------------------

func (p *Parser) parseTableConstraint() ast.TableConstraint {
	start := p.cur.Pos
	var name string
	if p.isKw("CONSTRAINT") {
		p.advance()
		if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
			name, _ = p.parseIdent()
		}
	}

	switch {
	case p.isKw("PRIMARY"):
		p.advance()
		p.expectKw("KEY")
		cols := p.parseIndexedCols()
		return &ast.PKConstraint{Name: name, Columns: cols, P: astPos(start)}
	case p.isKw("UNIQUE"):
		p.advance()
		if p.isKw("KEY") || p.isKw("INDEX") {
			p.advance()
		}
		if name == "" && (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT) {
			name, _ = p.parseIdent()
		}
		cols := p.parseIndexedCols()
		return &ast.UQConstraint{Name: name, Columns: cols, P: astPos(start)}
	case p.isKw("FOREIGN"):
		p.advance()
		p.expectKw("KEY")
		if name == "" && (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT) {
			name, _ = p.parseIdent()
		}
		cols := p.parseColList()
		p.expectKw("REFERENCES")
		ref := p.parseTableRef()
		refCols := p.parseColList()
		fk := &ast.FKConstraint{
			Name: name, Columns: cols, RefSchema: ref.Schema,
			RefTable: ref.Name, RefColumns: refCols, P: astPos(start),
		}
		for p.isKw("ON") {
			p.advance()
			switch {
			case p.isKw("DELETE"):
				p.advance()
				fk.OnDelete = p.parseRefAction()
			case p.isKw("UPDATE"):
				p.advance()
				fk.OnUpdate = p.parseRefAction()
			default:
				return fk
			}
		}
		return fk
	case p.isKw("CHECK"):
		p.advance()
		p.expectPunct("(")
		e := p.parseExpr()
		p.expectPunct(")")
		c := &ast.CheckConstraint{Name: name, Expr: e, P: astPos(start)}
		if p.isKw("NOT") {
			p.advance()
			p.expectKw("ENFORCED")
		} else if p.isKw("ENFORCED") {
			p.advance()
			c.Enforced = true
		} else {
			c.Enforced = true
		}
		return c
	}
	p.errorHere("unknown table constraint", "PRIMARY|UNIQUE|FOREIGN|CHECK")
	return nil
}

func (p *Parser) parseRefAction() string {
	switch {
	case p.isKw("RESTRICT"):
		p.advance()
		return "RESTRICT"
	case p.isKw("CASCADE"):
		p.advance()
		return "CASCADE"
	case p.isKw("NO"):
		p.advance()
		p.expectKw("ACTION")
		return "NO ACTION"
	case p.isKw("SET"):
		p.advance()
		if p.isKw("NULL") {
			p.advance()
			return "SET NULL"
		}
		if p.isKw("DEFAULT") {
			p.advance()
			return "SET DEFAULT"
		}
	}
	return ""
}

func (p *Parser) parseIndexDef() *ast.IndexDef {
	start := p.cur.Pos
	idx := &ast.IndexDef{P: astPos(start)}
	if p.isKw("FULLTEXT") {
		p.advance()
		idx.Kind = "FULLTEXT"
	} else if p.isKw("SPATIAL") {
		p.advance()
		idx.Kind = "SPATIAL"
	} else if p.isKw("VECTOR") {
		// MariaDB 11.7+ VECTOR INDEX (HNSW-style ANN index over a VECTOR(N) col).
		p.advance()
		idx.Kind = "VECTOR"
	}
	if p.isKw("INDEX") || p.isKw("KEY") {
		p.advance()
	}
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		idx.Name, _ = p.parseIdent()
	}
	idx.Columns = p.parseIndexedCols()
	// index options: USING {BTREE|HASH}, COMMENT '...', VISIBLE/INVISIBLE,
	// KEY_BLOCK_SIZE, WITH PARSER, ENGINE_ATTRIBUTE, SECONDARY_ENGINE_ATTRIBUTE.
	for {
		switch {
		case p.isKw("USING"):
			p.advance()
			if v, ok := p.eatIdentOrKw(); ok {
				idx.Using = v
			}
		case p.isKw("COMMENT"):
			p.advance()
			if p.cur.Kind == TOK_STRING {
				idx.Comment = p.cur.Lit
				p.advance()
			}
		case p.isKw("INVISIBLE"):
			p.advance()
			idx.Invisible = true
		case p.isKw("VISIBLE"):
			p.advance()
		case p.isKw("KEY_BLOCK_SIZE"):
			p.advance()
			p.consumePunct("=")
			if p.cur.Kind == TOK_NUMBER {
				p.advance()
			}
		case p.isKw("WITH"):
			// WITH PARSER parser_name (FULLTEXT only).
			p.advance()
			if p.isKw("PARSER") {
				p.advance()
				_, _ = p.parseIdent()
			}
		default:
			// MariaDB 11.7+ VECTOR INDEX accepts bare K=V tails like
			// `M=8 DISTANCE=cosine`. Tolerate them by consuming an
			// IDENT/KEYWORD '=' VALUE triple — but only for VECTOR indexes
			// to avoid masking real parse errors elsewhere.
			if idx.Kind == "VECTOR" && (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD) {
				p.advance()
				if p.consumePunct("=") {
					if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD ||
						p.cur.Kind == TOK_NUMBER || p.cur.Kind == TOK_STRING {
						p.advance()
					}
					continue
				}
			}
			return idx
		}
	}
}

func (p *Parser) parseColList() []string {
	p.expectPunct("(")
	var cols []string
	for {
		n, _ := p.parseIdent()
		cols = append(cols, n)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return cols
}

func (p *Parser) parseIndexedCols() []ast.IndexedCol {
	p.expectPunct("(")
	var cols []ast.IndexedCol
	for {
		c := ast.IndexedCol{}
		// Functional/expression key: `(expr)` directly inside the col list.
		// MySQL 8 added `(LOWER(col))`-style keys; MariaDB 10.5+ has the
		// same shape. We capture the raw source text so the translator can
		// emit it verbatim as a PG expression index.
		if p.isPunct("(") {
			p.advance()
			c.Expr = p.captureBalancedExpr()
			c.IsExpr = true
		} else {
			c.Name, _ = p.parseIdent()
			if p.isPunct("(") {
				p.advance()
				if p.cur.Kind == TOK_NUMBER {
					c.PrefixLen, _ = strconv.Atoi(p.cur.Lit)
					p.advance()
				}
				p.expectPunct(")")
			}
		}
		if p.isKw("ASC") {
			p.advance()
			c.Order = "ASC"
		} else if p.isKw("DESC") {
			p.advance()
			c.Order = "DESC"
		}
		cols = append(cols, c)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return cols
}

// ---------------------------------------------------------------------------
// CREATE INDEX
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateIndex(start Position) ast.Stmt {
	s := &ast.CreateIndex{P: astPos(start)}
	if p.isKw("UNIQUE") {
		p.advance()
		s.Unique = true
	} else if p.isKw("FULLTEXT") {
		p.advance()
		s.Kind = "FULLTEXT"
	} else if p.isKw("SPATIAL") {
		p.advance()
		s.Kind = "SPATIAL"
	}
	p.expectKw("INDEX")
	s.Name, _ = p.parseIdent()
	if p.isKw("USING") {
		p.advance()
		if v, ok := p.eatIdentOrKw(); ok {
			s.Using = v
		}
	}
	p.expectKw("ON")
	s.Table = p.parseTableRef()
	s.Columns = p.parseIndexedCols()
	// swallow any tail index options
	for p.isKw("USING") || p.isKw("COMMENT") {
		switch {
		case p.isKw("USING"):
			p.advance()
			p.eatIdentOrKw()
		case p.isKw("COMMENT"):
			p.advance()
			if p.cur.Kind == TOK_STRING {
				p.advance()
			}
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Expressions — minimal subset for DEFAULT/CHECK/generated.
// Other expression contexts in routine bodies are captured raw.
// ---------------------------------------------------------------------------

func (p *Parser) parseDefaultExpr() ast.Expr {
	// MySQL allows: literal | function | (expr) | NULL | CURRENT_TIMESTAMP[(n)]
	return p.parseExpr()
}

func (p *Parser) parseExpr() ast.Expr        { return p.parseOr() }
func (p *Parser) parseOr() ast.Expr {
	lhs := p.parseAnd()
	for p.isKw("OR") || p.isPunct("||") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseAnd()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}
func (p *Parser) parseAnd() ast.Expr {
	lhs := p.parseNot()
	for p.isKw("AND") || p.isPunct("&&") {
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
	lhs := p.parseAdd()
	for {
		// `[NOT] BETWEEN lo AND hi` and `[NOT] IN (...)` bind here so a
		// chained comparison still produces the correct grouping.
		if p.isKw("NOT") {
			// Tentatively consume NOT to see if it leads into BETWEEN/IN/LIKE.
			savedPos := p.cur.Pos
			p.advance()
			switch {
			case p.isKw("BETWEEN"):
				p.advance()
				low := p.parseAdd()
				p.expectKw("AND")
				high := p.parseAdd()
				lhs = &ast.BetweenExpr{Expr: lhs, Low: low, High: high, Not: true, P: astPos(savedPos)}
				continue
			case p.isKw("IN"):
				p.advance()
				lhs = p.parseInRest(lhs, true, savedPos)
				continue
			case p.isKw("LIKE"):
				p.advance()
				rhs := p.parseAdd()
				lhs = &ast.BinaryExpr{Op: "NOT LIKE", Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
				continue
			default:
				// Not one of our operators — surface a generic NOT prefix
				// (the caller's parseNot is at a higher level so this path
				// only kicks in mid-expression). Wrap rhs as UnaryExpr.
				rhs := p.parseAdd()
				lhs = &ast.UnaryExpr{Op: "NOT", Rhs: rhs, P: astPos(savedPos)}
				continue
			}
		}
		if p.isKw("BETWEEN") {
			start := p.cur.Pos
			p.advance()
			low := p.parseAdd()
			p.expectKw("AND")
			high := p.parseAdd()
			lhs = &ast.BetweenExpr{Expr: lhs, Low: low, High: high, P: astPos(start)}
			continue
		}
		if p.isKw("IN") {
			start := p.cur.Pos
			p.advance()
			lhs = p.parseInRest(lhs, false, start)
			continue
		}
		op := ""
		switch {
		case p.isPunct("="), p.isPunct("<>"), p.isPunct("!="), p.isPunct("<"),
			p.isPunct("<="), p.isPunct(">"), p.isPunct(">="), p.isPunct("<=>"):
			op = p.cur.Lit
			p.advance()
		case p.isKw("IS"):
			p.advance()
			op = "IS"
			if p.isKw("NOT") {
				p.advance()
				op = "IS NOT"
			}
		case p.isKw("LIKE"):
			op = "LIKE"
			p.advance()
		default:
			return lhs
		}
		rhs := p.parseAdd()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
}

// parseInRest parses the right-hand side of `[NOT] IN <rest>` — either a
// parenthesised expression list `(v1, v2, …)` or a subquery `(SELECT …)`.
func (p *Parser) parseInRest(lhs ast.Expr, not bool, start Position) ast.Expr {
	if !p.isPunct("(") {
		// Defer to a generic binary IN over a single rhs expression.
		op := "IN"
		if not {
			op = "NOT IN"
		}
		rhs := p.parseAdd()
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
	for p.isPunct("*") || p.isPunct("/") || p.isPunct("%") || p.isKw("DIV") || p.isKw("MOD") {
		op := p.cur.Lit
		p.advance()
		rhs := p.parseUnary()
		lhs = &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: rhs, P: lhs.Pos()}
	}
	return lhs
}
func (p *Parser) parseUnary() ast.Expr {
	if p.isPunct("-") || p.isPunct("+") || p.isPunct("!") || p.isPunct("~") {
		start := p.cur.Pos
		op := p.cur.Lit
		p.advance()
		rhs := p.parsePrimary()
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
		// `(SELECT ...)` — scalar subquery used as an expression.
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
	case p.cur.Kind == TOK_BIT:
		v := p.cur.Lit
		p.advance()
		return &ast.Literal{Kind: "bit", Text: v, P: astPos(start)}
	case p.isKw("NULL"):
		p.advance()
		return &ast.Literal{Kind: "null", Text: "NULL", P: astPos(start)}
	case p.isKw("TRUE"):
		p.advance()
		return &ast.Literal{Kind: "bool", Text: "TRUE", P: astPos(start)}
	case p.isKw("FALSE"):
		p.advance()
		return &ast.Literal{Kind: "bool", Text: "FALSE", P: astPos(start)}
	case p.isKw("CURRENT_TIMESTAMP") || p.isKw("NOW") || p.isKw("LOCALTIMESTAMP"):
		name := p.cur.Lit
		p.advance()
		fc := &ast.FuncCall{Name: name, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if p.cur.Kind == TOK_NUMBER {
				fc.Args = append(fc.Args, &ast.Literal{Kind: "number", Text: p.cur.Lit, P: astPos(p.cur.Pos)})
				p.advance()
			}
			p.expectPunct(")")
		}
		return fc
	case p.isKw("CASE"):
		return p.parseCaseExpr()
	case p.isKw("CAST"):
		return p.parseCastExpr()
	case p.isKw("INTERVAL"):
		return p.parseIntervalLit()
	case p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD:
		// function call or identifier path
		name, bt := p.parseIdent()
		// function?
		if p.isPunct("(") {
			p.advance()
			fc := &ast.FuncCall{Name: strings.ToUpper(name), P: astPos(start)}
			// Aggregate-style argument prefix: DISTINCT / ALL preceding the
			// expression. We discard the qualifier — the AST does not yet
			// model per-call set-quantifiers, but accepting it keeps real
			// MySQL queries parsing.
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
			return fc
		}
		// identifier path (schema.table.col, NEW.col, OLD.col)
		parts := []string{name}
		for p.isPunct(".") {
			p.advance()
			if n, _ := p.parseIdent(); n != "" {
				parts = append(parts, n)
			}
		}
		return &ast.Ident{Parts: parts, Backtick: bt, P: astPos(start)}
	}
	p.errorHere("unexpected token in expression", "expression")
	p.advance()
	return &ast.RawExpr{Text: "", P: astPos(start)}
}

// parseFuncArg parses one argument of a function call. Handles the special
// `*` argument used by aggregates such as COUNT(*) — emitted as the
// identifier `*` so writers can recognise it without ambiguity.
func (p *Parser) parseFuncArg() ast.Expr {
	if p.isPunct("*") {
		start := p.cur.Pos
		p.advance()
		return &ast.Ident{Parts: []string{"*"}, P: astPos(start)}
	}
	return p.parseExpr()
}

// parseCaseExpr parses both the simple form `CASE x WHEN v1 THEN r1 ... END`
// and the searched form `CASE WHEN cond THEN r ... ELSE e END`.
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
	// MySQL allows an optional `END CASE` at statement level but not in
	// expressions; tolerate it defensively.
	if p.isKw("CASE") {
		p.advance()
	}
	return out
}

// parseCastExpr parses `CAST(<expr> AS <type>)`.
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

// parseIntervalLit parses `INTERVAL <expr> <unit>`. We capture the value
// expression as text (for round-trip) and the unit keyword verbatim. Common
// shapes: `INTERVAL 1 DAY`, `INTERVAL '1' DAY`, `INTERVAL 1 YEAR_MONTH`.
func (p *Parser) parseIntervalLit() ast.Expr {
	start := p.cur.Pos
	p.expectKw("INTERVAL")
	// Capture the value as the next primary expression — usually a number
	// literal or a quoted string. Using parsePrimary avoids consuming the
	// trailing unit keyword that follows it.
	val := p.parsePrimary()
	unit := ""
	switch {
	case p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT:
		unit = p.cur.Lit
		p.advance()
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

// ---------------------------------------------------------------------------
// Routines: VIEW, TRIGGER, PROCEDURE, FUNCTION, EVENT
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateView(start Position, orReplace bool, algorithm, definer, sqlSec string) *ast.CreateView {
	p.expectKw("VIEW")
	s := &ast.CreateView{
		OrReplace: orReplace, Algorithm: algorithm, Definer: definer,
		SQLSecurity: sqlSec, P: astPos(start),
	}
	s.View = p.parseTableRef()
	// optional column list
	if p.isPunct("(") {
		s.Columns = p.parseColList()
	}
	p.expectKw("AS")
	// Capture the SELECT body verbatim up to a WITH CHECK OPTION or delimiter.
	startOff := p.cur.Pos.Offset
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		if p.isKw("WITH") && p.peekLookaheadKw("CHECK") || p.isKw("WITH") && (p.peekLookaheadKw("CASCADED") || p.peekLookaheadKw("LOCAL")) {
			break
		}
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	s.SelectBody = strings.TrimSpace(string(p.src[startOff:endOff]))
	if p.isKw("WITH") {
		p.advance()
		opt := ""
		if p.isKw("CASCADED") {
			p.advance()
			opt = "CASCADED "
		} else if p.isKw("LOCAL") {
			p.advance()
			opt = "LOCAL "
		}
		p.expectKw("CHECK")
		p.expectKw("OPTION")
		s.CheckOption = strings.TrimSpace(opt + "CHECK OPTION")
	}
	return s
}

// peekLookaheadKw is a small 1-token lookahead helper. The lexer has a
// peeked-cache; we use our own 1-slot shadow.
func (p *Parser) peekLookaheadKw(kw string) bool {
	t := p.l.Peek()
	return t.Kind == TOK_KEYWORD && t.Lit == kw
}

func (p *Parser) parseCreateTrigger(start Position, definer string) *ast.CreateTrigger {
	p.expectKw("TRIGGER")
	s := &ast.CreateTrigger{Definer: definer, P: astPos(start)}
	s.Name, _ = p.parseIdent()
	// BEFORE | AFTER
	if p.isKw("BEFORE") || p.isKw("AFTER") {
		s.Time = p.cur.Lit
		p.advance()
	}
	// INSERT | UPDATE | DELETE
	if p.isKw("INSERT") || p.isKw("UPDATE") || p.isKw("DELETE") {
		s.Event = p.cur.Lit
		p.advance()
	}
	p.expectKw("ON")
	s.Table = p.parseTableRef()
	p.expectKw("FOR")
	p.expectKw("EACH")
	p.expectKw("ROW")
	if p.isKw("FOLLOWS") || p.isKw("PRECEDES") {
		s.Order = p.cur.Lit
		p.advance()
		s.OrderRef, _ = p.parseIdent()
	}
	s.Body = p.captureRawBody()
	return s
}

func (p *Parser) parseCreateProcedure(start Position, definer string) *ast.CreateProcedure {
	p.expectKw("PROCEDURE")
	s := &ast.CreateProcedure{Definer: definer, P: astPos(start)}
	s.Name, _ = p.parseIdent()
	p.expectPunct("(")
	if !p.isPunct(")") {
		s.Params = p.parseProcParams()
	}
	p.expectPunct(")")
	s.Characteristics = p.parseRoutineCharacteristics()
	s.Body = p.captureRawBody()
	return s
}

func (p *Parser) parseCreateFunction(start Position, definer string) *ast.CreateFunction {
	p.expectKw("FUNCTION")
	s := &ast.CreateFunction{Definer: definer, P: astPos(start)}
	s.Name, _ = p.parseIdent()
	p.expectPunct("(")
	if !p.isPunct(")") {
		s.Params = p.parseFuncParams()
	}
	p.expectPunct(")")
	p.expectKw("RETURNS")
	s.Returns = p.parseDataType()
	s.Characteristics = p.parseRoutineCharacteristics()
	s.Body = p.captureRawBody()
	return s
}

func (p *Parser) parseProcParams() []ast.Param {
	var out []ast.Param
	for {
		par := ast.Param{}
		if p.isKw("IN") || p.isKw("OUT") || p.isKw("INOUT") {
			par.Direction = p.cur.Lit
			p.advance()
		}
		par.Name, _ = p.parseIdent()
		par.Type = p.parseDataType()
		out = append(out, par)
		if !p.isPunct(",") {
			return out
		}
		p.advance()
	}
}

func (p *Parser) parseFuncParams() []ast.Param {
	var out []ast.Param
	for {
		par := ast.Param{}
		par.Name, _ = p.parseIdent()
		par.Type = p.parseDataType()
		out = append(out, par)
		if !p.isPunct(",") {
			return out
		}
		p.advance()
	}
}

func (p *Parser) parseRoutineCharacteristics() ast.RoutineCharacteristics {
	var c ast.RoutineCharacteristics
	for {
		switch {
		case p.isKw("LANGUAGE"):
			p.advance()
			if p.isKw("SQL") {
				p.advance()
				c.Language = "SQL"
			}
		case p.isKw("NOT"):
			// NOT DETERMINISTIC
			p.advance()
			if p.isKw("DETERMINISTIC") {
				p.advance()
				c.Deterministic = false
				c.HasDeterministic = true
			}
		case p.isKw("DETERMINISTIC"):
			p.advance()
			c.Deterministic = true
			c.HasDeterministic = true
		case p.isKw("CONTAINS"):
			p.advance()
			p.expectKw("SQL")
			c.SQLDataAccess = "CONTAINS SQL"
		case p.isKw("NO"):
			p.advance()
			p.expectKw("SQL")
			c.SQLDataAccess = "NO SQL"
		case p.isKw("READS"):
			p.advance()
			p.expectKw("SQL")
			p.expectKw("DATA")
			c.SQLDataAccess = "READS SQL DATA"
		case p.isKw("MODIFIES"):
			p.advance()
			p.expectKw("SQL")
			p.expectKw("DATA")
			c.SQLDataAccess = "MODIFIES SQL DATA"
		case p.isKw("SQL"):
			p.advance()
			p.expectKw("SECURITY")
			if p.isKw("DEFINER") || p.isKw("INVOKER") {
				c.SQLSecurity = p.cur.Lit
				p.advance()
			}
		case p.isKw("COMMENT"):
			p.advance()
			if p.cur.Kind == TOK_STRING {
				c.Comment = p.cur.Lit
				p.advance()
			}
		default:
			return c
		}
	}
}

func (p *Parser) parseCreateEvent(start Position, definer string) *ast.CreateEvent {
	p.expectKw("EVENT")
	s := &ast.CreateEvent{Definer: definer, P: astPos(start)}
	if p.isKw("IF") {
		p.advance()
		p.expectKw("NOT")
		p.expectKw("EXISTS")
		s.IfNotExists = true
	}
	s.Name, _ = p.parseIdent()
	p.expectKw("ON")
	p.expectKw("SCHEDULE")
	if p.isKw("AT") {
		p.advance()
		s.ScheduleKind = "AT"
		s.At = p.captureExprUpto("ON", "DO", "COMMENT", "ENABLE", "DISABLE")
	} else if p.isKw("EVERY") {
		p.advance()
		s.ScheduleKind = "EVERY"
		if p.cur.Kind == TOK_NUMBER {
			s.EveryN, _ = strconv.ParseInt(p.cur.Lit, 10, 64)
			p.advance()
		}
		if p.cur.Kind == TOK_KEYWORD {
			s.EveryUnit = p.cur.Lit
			p.advance()
		}
		if p.isKw("STARTS") {
			p.advance()
			s.Starts = p.captureExprUpto("ENDS", "ON", "DO", "COMMENT", "ENABLE", "DISABLE")
		}
		if p.isKw("ENDS") {
			p.advance()
			s.Ends = p.captureExprUpto("ON", "DO", "COMMENT", "ENABLE", "DISABLE")
		}
	}
	if p.isKw("ON") && p.peekLookaheadKw("COMPLETION") {
		p.advance()
		p.advance()
		if p.isKw("NOT") {
			p.advance()
			p.expectKw("PRESERVE")
			s.OnCompletion = "NOT PRESERVE"
		} else {
			p.expectKw("PRESERVE")
			s.OnCompletion = "PRESERVE"
		}
	}
	if p.isKw("ENABLE") {
		p.advance()
		s.Enable = "ENABLE"
	} else if p.isKw("DISABLE") {
		p.advance()
		s.Enable = "DISABLE"
		if p.isKw("ON") {
			p.advance()
			p.expectKw("SLAVE")
			s.Enable = "DISABLE ON SLAVE"
		}
	}
	if p.isKw("COMMENT") {
		p.advance()
		if p.cur.Kind == TOK_STRING {
			s.Comment = p.cur.Lit
			p.advance()
		}
	}
	p.expectKw("DO")
	s.Body = p.captureRawBody()
	return s
}

// parseCreateSequence parses MariaDB 10.3+ `CREATE [OR REPLACE] [TEMPORARY]
// SEQUENCE [IF NOT EXISTS] name [option ...]`. Recognised options:
//
//	INCREMENT [BY|=] n
//	MINVALUE [=] n | NO MINVALUE | NOMINVALUE
//	MAXVALUE [=] n | NO MAXVALUE | NOMAXVALUE
//	START [WITH|=] n
//	CACHE [=] n | NOCACHE
//	CYCLE | NOCYCLE
//	(other engine-specific tail tokens are skipped permissively)
func (p *Parser) parseCreateSequence(start Position, orReplace, temporary bool) ast.Stmt {
	p.expectKw("SEQUENCE")
	s := &ast.CreateSequence{
		OrReplace: orReplace,
		Temporary: temporary,
		P:         astPos(start),
	}
	if p.isKw("IF") {
		p.advance()
		p.expectKw("NOT")
		p.expectKw("EXISTS")
		s.IfNotExists = true
	}
	ref := p.parseTableRef()
	s.Schema = ref.Schema
	s.Name = ref.Name
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("INCREMENT"):
			p.advance()
			if p.isKw("BY") {
				p.advance()
			}
			p.consumePunct("=")
			n, ok := p.parseSignedInt()
			if ok {
				s.Increment = n
				s.HasIncr = true
			}
		case p.isKw("START"):
			p.advance()
			if p.isKw("WITH") {
				p.advance()
			}
			p.consumePunct("=")
			n, ok := p.parseSignedInt()
			if ok {
				s.Start = n
				s.HasStart = true
			}
		case p.isKw("MINVALUE"):
			p.advance()
			p.consumePunct("=")
			n, ok := p.parseSignedInt()
			if ok {
				s.MinValue = n
				s.HasMin = true
			}
		case p.isKw("NOMINVALUE"):
			p.advance()
			s.NoMin = true
		case p.isKw("MAXVALUE"):
			p.advance()
			p.consumePunct("=")
			n, ok := p.parseSignedInt()
			if ok {
				s.MaxValue = n
				s.HasMax = true
			}
		case p.isKw("NOMAXVALUE"):
			p.advance()
			s.NoMax = true
		case p.isKw("NO"):
			// `NO MINVALUE` / `NO MAXVALUE` / `NO CYCLE` / `NO CACHE` (PG-style spelling)
			p.advance()
			switch {
			case p.isKw("MINVALUE"):
				p.advance()
				s.NoMin = true
			case p.isKw("MAXVALUE"):
				p.advance()
				s.NoMax = true
			case p.isKw("CYCLE"):
				p.advance()
				s.Cycle = false
				s.HasCycle = true
			case p.isKw("CACHE"):
				p.advance()
				s.NoCache = true
			default:
				p.advance()
			}
		case p.isKw("CYCLE"):
			p.advance()
			s.Cycle = true
			s.HasCycle = true
		case p.isKw("NOCYCLE"):
			p.advance()
			s.Cycle = false
			s.HasCycle = true
		case p.isKw("CACHE"):
			p.advance()
			p.consumePunct("=")
			n, ok := p.parseSignedInt()
			if ok {
				s.Cache = n
				s.HasCache = true
			}
		case p.isKw("NOCACHE"):
			p.advance()
			s.NoCache = true
		case p.isKw("ENGINE"):
			// ENGINE = name (and any other tail engine option) — skip "= word"
			p.advance()
			p.consumePunct("=")
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
				p.advance()
			}
		default:
			// Unknown trailing token: consume to keep parsing forward.
			p.advance()
		}
	}
	return s
}

// parseSignedInt parses an optionally-signed integer literal at the current
// position, returning the value and whether one was consumed.
func (p *Parser) parseSignedInt() (int64, bool) {
	sign := int64(1)
	if p.isPunct("-") {
		p.advance()
		sign = -1
	} else if p.isPunct("+") {
		p.advance()
	}
	if p.cur.Kind != TOK_NUMBER {
		return 0, false
	}
	n, err := strconv.ParseInt(p.cur.Lit, 10, 64)
	if err != nil {
		return 0, false
	}
	p.advance()
	return sign * n, true
}

// captureExprUpto captures raw text from the current position until one of the
// stop keywords (uppercase) or the statement delimiter. Useful for event
// schedule expressions that include arithmetic / INTERVAL that we don't fully
// parse yet.
func (p *Parser) captureExprUpto(stops ...string) string {
	stopSet := map[string]struct{}{}
	for _, s := range stops {
		stopSet[s] = struct{}{}
	}
	startOff := p.cur.Pos.Offset
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		if p.cur.Kind == TOK_KEYWORD {
			if _, ok := stopSet[p.cur.Lit]; ok {
				break
			}
		}
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	return strings.TrimSpace(string(p.src[startOff:endOff]))
}
