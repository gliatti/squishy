package oracle

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

// tableQualifiers carries the leading CREATE-clause qualifiers down to
// parseCreateTable so they end up on TableOptions for the translator.
type tableQualifiers struct {
	GlobalTemp  bool
	PrivateTemp bool
	Immutable   bool
	Blockchain  bool
	Sharded     bool
	Duplicated  bool
}

func (p *Parser) parseCreateTable(start Position, global bool) ast.Stmt {
	return p.parseCreateTableQuals(start, tableQualifiers{GlobalTemp: global})
}

func (p *Parser) parseCreateTableQuals(start Position, q tableQualifiers) ast.Stmt {
	p.expectKw("TABLE")
	ifNotExists := false
	if p.isKw("IF") {
		p.advance()
		p.expectKw("NOT")
		p.expectKw("EXISTS")
		ifNotExists = true
	}
	schema, name := p.parseQualifiedName()
	t := &ast.CreateTable{
		Schema:      schema,
		Name:        name,
		IfNotExists: ifNotExists,
		Temporary:   q.GlobalTemp || q.PrivateTemp,
		P:           astPos(start),
	}
	if !p.expectPunct("(") {
		return t
	}
	for {
		// Table-level constraint?
		if p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") ||
			p.isKw("FOREIGN") || p.isKw("CHECK") {
			c := p.parseTableConstraint()
			if c != nil {
				t.Constraints = append(t.Constraints, c)
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
	t.Options = p.parseTableOptions()
	t.Options.OracleImmutable = q.Immutable
	t.Options.OracleBlockchain = q.Blockchain
	t.Options.OracleSharded = q.Sharded
	t.Options.OracleDuplicated = q.Duplicated
	t.Options.OraclePrivateTemp = q.PrivateTemp
	return t
}

// parseColumnDef — name datatype [constraints...]
func (p *Parser) parseColumnDef() *ast.ColumnDef {
	start := p.cur.Pos
	name, bt := p.parseIdent()
	col := &ast.ColumnDef{Name: name, NameBacktick: bt, P: astPos(start)}
	if p.isPunct(")") || p.isPunct(",") {
		p.errorHere("expected data type", "TYPE")
		return col
	}
	col.Type = p.parseDataType()
	// Column-level clauses in any order.
	for !p.isPunct(",") && !p.isPunct(")") && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("DEFAULT"):
			p.advance()
			if p.isKw("ON") { // DEFAULT ON NULL <expr>
				p.advance()
				if p.isKw("NULL") {
					p.advance()
				}
				col.DefaultOnNull = true
			}
			col.Default = p.parseExprUntilBoundary()
			col.HasDefault = true
		case p.isKw("NOT"):
			p.advance()
			p.expectKw("NULL")
			col.NotNull = true
			col.HasNullable = true
		case p.isKw("NULL"):
			p.advance()
			col.NotNull = false
			col.HasNullable = true
		case p.isKw("VISIBLE"):
			p.advance()
			col.Invisible = false
		case p.isKw("INVISIBLE"):
			p.advance()
			col.Invisible = true
		case p.isKw("GENERATED"):
			p.advance()
			col.Generated = p.parseGeneratedClause()
			// identity vs computed handling
			if col.Generated == nil {
				col.AutoInc = true
			}
		case p.isKw("PRIMARY"):
			p.advance()
			p.expectKw("KEY")
			col.PrimaryKey = true
		case p.isKw("UNIQUE"):
			p.advance()
			col.Unique = true
		case p.isKw("CHECK"):
			p.advance()
			p.expectPunct("(")
			col.Check = &ast.RawExpr{Text: p.captureUntilParen(), P: astPos(start)}
			p.expectPunct(")")
		case p.isKw("REFERENCES"):
			// Inline FK reference; convert to table-level constraint-like note.
			p.advance()
			ref := p.parseTableRef()
			fk := &ast.FKConstraint{
				Columns:    []string{col.Name},
				RefSchema:  ref.Schema,
				RefTable:   ref.Name,
				P:          astPos(start),
			}
			if p.isPunct("(") {
				p.advance()
				for {
					rc, _ := p.parseIdent()
					fk.RefColumns = append(fk.RefColumns, rc)
					if !p.isPunct(",") {
						break
					}
					p.advance()
				}
				p.expectPunct(")")
			}
			p.parseFKActions(fk)
			// Attach as table-level constraint via a side-channel: skip here,
			// we don't mutate the parent table from inside a column def. The
			// translator accepts a column-level FK via ColumnDef if needed;
			// fall through as a discarded fragment.
			_ = fk
		case p.isKw("CONSTRAINT"):
			// Named column-level constraint.
			p.advance()
			cname, _ := p.parseIdent()
			_ = cname
			// Loop again to pick up the constraint keyword that follows.
			continue
		case p.isKw("ENABLE"), p.isKw("DISABLE"), p.isKw("VALIDATE"), p.isKw("NOVALIDATE"),
			p.isKw("DEFERRABLE"), p.isKw("INITIALLY"), p.isKw("DEFERRED"), p.isKw("IMMEDIATE"):
			p.advance()
		case p.isKw("COLLATE"):
			p.advance()
			if n, _ := p.parseIdent(); n != "" {
				col.Collation = n
			}
		default:
			// Unknown clause — skip a token to avoid infinite loop; Oracle has
			// many storage/clause tails that we ignore.
			p.advance()
		}
	}
	return col
}

// parseGeneratedClause consumes GENERATED [ALWAYS|BY DEFAULT [ON NULL]] {AS (expr) [VIRTUAL|STORED] | AS IDENTITY [(opts)]}.
// Returns *Generated for computed columns; returns nil for IDENTITY (caller
// sets AutoInc=true).
func (p *Parser) parseGeneratedClause() *ast.Generated {
	// Already consumed GENERATED.
	if p.isKw("ALWAYS") {
		p.advance()
	} else if p.isKw("BY") {
		p.advance()
		p.expectKw("DEFAULT")
		if p.isKw("ON") {
			p.advance()
			p.expectKw("NULL")
		}
	}
	if p.isKw("AS") {
		p.advance()
		if p.isKw("IDENTITY") {
			p.advance()
			// Parenthesized options: ( START WITH n INCREMENT BY n ... )
			if p.isPunct("(") {
				p.advance()
				depth := 1
				for depth > 0 && p.cur.Kind != TOK_EOF {
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
			// DBMS_METADATA emits the sequence options BARE (no parens):
			//   MINVALUE n MAXVALUE n INCREMENT BY n START WITH n CACHE n
			//   NOORDER NOCYCLE NOKEEP NOSCALE
			// Consume them so they don't confuse parseColumnDef's tail loop.
			for {
				switch {
				case p.isKw("MINVALUE"), p.isKw("MAXVALUE"), p.isKw("CACHE"):
					p.advance()
					_, _ = p.intLit()
				case p.isKw("NOMINVALUE"), p.isKw("NOMAXVALUE"),
					p.isKw("NOCACHE"), p.isKw("NOCYCLE"),
					p.isKw("CYCLE"), p.isKw("ORDER"), p.isKw("NOORDER"),
					p.isKw("KEEP"):
					p.advance()
				case p.cur.Kind == TOK_IDENT && (p.cur.Lit == "NOKEEP" || p.cur.Lit == "NOSCALE" || p.cur.Lit == "SCALE"):
					p.advance()
				case p.isKw("INCREMENT"):
					p.advance()
					if p.isKw("BY") {
						p.advance()
					}
					_, _ = p.intLit()
				case p.isKw("START"):
					p.advance()
					if p.isKw("WITH") {
						p.advance()
					}
					_, _ = p.intLit()
				default:
					return nil // IDENTITY marker
				}
			}
		}
		if p.isPunct("(") {
			p.advance()
			expr := p.parseExprUntilParenClose()
			p.expectPunct(")")
			g := &ast.Generated{Expr: expr}
			if p.isKw("VIRTUAL") {
				p.advance()
				g.Virtual = true
				g.HasVirtual = true
			} else if p.isKw("STORED") {
				p.advance()
				g.Virtual = false
				g.HasVirtual = true
			}
			return g
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Table-level constraints
// ---------------------------------------------------------------------------

func (p *Parser) parseTableConstraint() ast.TableConstraint {
	start := p.cur.Pos
	var name string
	if p.isKw("CONSTRAINT") {
		p.advance()
		name, _ = p.parseIdent()
	}
	switch {
	case p.isKw("PRIMARY"):
		p.advance()
		p.expectKw("KEY")
		p.expectPunct("(")
		cols := p.parseIndexedCols()
		p.expectPunct(")")
		st := p.parseConstraintState()
		return &ast.PKConstraint{Name: name, Columns: cols, State: st, P: astPos(start)}
	case p.isKw("UNIQUE"):
		p.advance()
		p.expectPunct("(")
		cols := p.parseIndexedCols()
		p.expectPunct(")")
		st := p.parseConstraintState()
		return &ast.UQConstraint{Name: name, Columns: cols, State: st, P: astPos(start)}
	case p.isKw("FOREIGN"):
		p.advance()
		p.expectKw("KEY")
		p.expectPunct("(")
		var cols []string
		for {
			c, _ := p.parseIdent()
			cols = append(cols, c)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
		p.expectKw("REFERENCES")
		ref := p.parseTableRef()
		fk := &ast.FKConstraint{
			Name:      name,
			Columns:   cols,
			RefSchema: ref.Schema,
			RefTable:  ref.Name,
			P:         astPos(start),
		}
		if p.isPunct("(") {
			p.advance()
			for {
				c, _ := p.parseIdent()
				fk.RefColumns = append(fk.RefColumns, c)
				if !p.isPunct(",") {
					break
				}
				p.advance()
			}
			p.expectPunct(")")
		}
		p.parseFKActions(fk)
		fk.State = p.parseConstraintState()
		return fk
	case p.isKw("CHECK"):
		p.advance()
		p.expectPunct("(")
		expr := p.parseExprUntilParenClose()
		p.expectPunct(")")
		st := p.parseConstraintState()
		return &ast.CheckConstraint{Name: name, Expr: expr, Enforced: true, State: st, P: astPos(start)}
	}
	p.errorHere("expected PRIMARY|UNIQUE|FOREIGN|CHECK", "constraint keyword")
	return nil
}

func (p *Parser) parseFKActions(fk *ast.FKConstraint) {
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
			return
		}
	}
}

func (p *Parser) parseRefAction() string {
	switch {
	case p.isKw("CASCADE"):
		p.advance()
		return "CASCADE"
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
	case p.isKw("NO"):
		p.advance()
		p.expectKw("ACTION")
		return "NO ACTION"
	case p.isKw("RESTRICT"):
		p.advance()
		return "RESTRICT"
	}
	return ""
}

// parseConstraintState captures the trailing Oracle constraint-state
// clauses (ENABLE/DISABLE, VALIDATE/NOVALIDATE, RELY/NORELY, DEFERRABLE/
// NOT DEFERRABLE, INITIALLY DEFERRED/IMMEDIATE) into an ast.ConstraintState.
// It also tolerates `USING INDEX [<spec>]` clauses (consumed without
// extracting the index spec — that lives outside the state semantics).
func (p *Parser) parseConstraintState() ast.ConstraintState {
	var st ast.ConstraintState
	for {
		switch {
		case p.isKw("ENABLE"):
			p.advance()
			st.Disabled = false
		case p.isKw("DISABLE"):
			p.advance()
			st.Disabled = true
		case p.isKw("VALIDATE"):
			p.advance()
			st.NoValidate = false
		case p.isKw("NOVALIDATE"):
			p.advance()
			st.NoValidate = true
		case p.isAnyKwOrIdent("RELY"):
			p.advance()
			st.Rely = true
		case p.isAnyKwOrIdent("NORELY"):
			p.advance()
			st.Rely = false
		case p.isKw("NOT"):
			// NOT DEFERRABLE — consume both, leave Deferrable=false.
			p.advance()
			if p.isKw("DEFERRABLE") {
				p.advance()
				st.Deferrable = false
			}
		case p.isKw("DEFERRABLE"):
			p.advance()
			st.Deferrable = true
		case p.isKw("INITIALLY"):
			p.advance()
			if p.isKw("DEFERRED") {
				p.advance()
				st.InitiallyDeferred = true
			} else if p.isKw("IMMEDIATE") {
				p.advance()
				st.InitiallyDeferred = false
			}
		case p.isKw("USING"):
			// USING INDEX [<index_name> | (create_index_spec)] — consume the
			// keyword pair and, optionally, a balanced-parens index spec OR a
			// (possibly schema-qualified, possibly quoted) index name reference.
			// DBMS_METADATA emits forms like:
			//   USING INDEX                                  (bare)
			//   USING INDEX  ENABLE                          (bare, inline)
			//   USING INDEX "HR"."DEPT_ID_PK"  ENABLE        (qualified ref)
			//   USING INDEX (CREATE UNIQUE INDEX … )         (inline spec)
			// Stop at comma, ')', or the next recognized constraint-state kw.
			p.advance()
			st.HasUsingIndex = true
			if p.isKw("INDEX") {
				p.advance()
			}
			if p.isPunct("(") {
				p.captureBalancedParens()
				continue
			}
			// Optional index name: identifier, optionally schema-qualified.
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
				p.advance()
				if p.isPunct(".") {
					p.advance()
					if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
						p.advance()
					}
				}
			}
		default:
			return st
		}
	}
}

// skipConstraintState is preserved for callers that don't need the state
// (currently none — the wrapper just discards parseConstraintState's result).
func (p *Parser) skipConstraintState() {
	_ = p.parseConstraintState()
}

func (p *Parser) parseIndexedCols() []ast.IndexedCol {
	var out []ast.IndexedCol
	for {
		ic := p.parseOneIndexedCol()
		out = append(out, ic)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseOneIndexedCol parses a single index key entry. Oracle accepts both
// bare column names and arbitrary expressions (function-based and
// expression indexes — PlSqlParser.g4 `index_expr_option`). We classify
// upfront: a leading '(' or an IDENT followed by '(' (function call) is
// treated as an expression and captured verbatim from the source so the
// translator can re-emit it as a PG expression index `((expr))`. Anything
// else flows through the original bare-ident path.
func (p *Parser) parseOneIndexedCol() ast.IndexedCol {
	if p.startsExprIndex() {
		expr := p.captureIndexExpr()
		return ast.IndexedCol{Expr: expr, IsExpr: true, Order: p.consumeIndexedColOrder()}
	}
	// Bare identifier path (with optional schema/qualifier handled below).
	startOff := p.cur.Pos.Offset
	name, _ := p.parseIdent()
	// Function call masquerading as ident — e.g. `sysdate()` or any other
	// expression that startsExprIndex didn't catch (currently it does, but
	// keep the safety net).
	if p.isPunct("(") {
		// Roll the captured ident into the expression text by re-reading
		// from the original offset.
		expr := p.captureIndexExprFrom(startOff)
		_ = name
		return ast.IndexedCol{Expr: expr, IsExpr: true, Order: p.consumeIndexedColOrder()}
	}
	// Bare ident followed by an operator (e.g. `col1 + col2`) — also an
	// expression. Decide by what follows: if it's anything other than a
	// separator (',' / ')') or ASC/DESC, treat as expression.
	if !p.isPunct(",") && !p.isPunct(")") && !p.isAscDesc() {
		expr := p.captureIndexExprFrom(startOff)
		_ = name
		return ast.IndexedCol{Expr: expr, IsExpr: true, Order: p.consumeIndexedColOrder()}
	}
	return ast.IndexedCol{Name: name, Order: p.consumeIndexedColOrder()}
}

// isAscDesc reports whether the current token is ASC or DESC, regardless of
// whether the lexer registered it as a keyword (the Oracle keyword table
// doesn't include ASC/DESC, so the lexer emits them as IDENT).
func (p *Parser) isAscDesc() bool {
	if p.cur.Kind != TOK_KEYWORD && p.cur.Kind != TOK_IDENT {
		return false
	}
	return strings.EqualFold(p.cur.Lit, "ASC") || strings.EqualFold(p.cur.Lit, "DESC")
}

// startsExprIndex returns true when the current token cannot be the start
// of a bare column name (parenthesised expression, literal, …).
func (p *Parser) startsExprIndex() bool {
	if p.isPunct("(") || p.cur.Kind == TOK_STRING || p.cur.Kind == TOK_NUMBER {
		return true
	}
	return false
}

// captureIndexExpr captures source text from the current token up to (but
// not including) the next top-level ',' or ')'. Parens nest; single-quoted
// strings are honoured via the lexer's tokenisation (no `'` will appear as
// TOK_PUNCT, so depth tracking on '(' / ')' is sufficient).
func (p *Parser) captureIndexExpr() string {
	return p.captureIndexExprFrom(p.cur.Pos.Offset)
}

// captureIndexExprFrom is captureIndexExpr starting from a specific offset
// (used when the caller has already consumed the first token of the
// expression and needs to roll it back into the captured text). The capture
// stops at the next top-level ',' / ')' OR at a top-level ASC/DESC token,
// since those mark the end of the expression and are consumed separately
// by consumeIndexedColOrder.
func (p *Parser) captureIndexExprFrom(startOff int) string {
	depth := 0
	for p.cur.Kind != TOK_EOF {
		if depth == 0 {
			if p.isPunct(",") || p.isPunct(")") || p.isAscDesc() {
				endOff := p.cur.Pos.Offset
				return strings.TrimSpace(string(p.src[startOff:endOff]))
			}
		}
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			depth--
		}
		p.advance()
	}
	return strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
}

// consumeIndexedColOrder reads an optional trailing ASC/DESC and returns
// the canonical form. Unknown tokens are left untouched.
func (p *Parser) consumeIndexedColOrder() string {
	if !p.isAscDesc() {
		return ""
	}
	order := strings.ToUpper(p.cur.Lit)
	p.advance()
	return order
}

// ---------------------------------------------------------------------------
// Table-level options (storage, tablespace, partitioning) — captured raw.
// ---------------------------------------------------------------------------

func (p *Parser) parseTableOptions() ast.TableOptions {
	var opts ast.TableOptions
	for {
		switch {
		case p.isKw("TABLESPACE"):
			p.advance()
			name, _ := p.parseIdent()
			opts.OracleTablespace = name
		case p.isKw("STORAGE"):
			opts.OracleStorage = "STORAGE(" + p.readBalancedParensAfter() + ")"
		case p.isKw("PARTITION"):
			// PARTITION BY {RANGE|LIST|HASH|REFERENCE|SYSTEM} (col-or-constraint)
			//   [ INTERVAL (expr) [STORE IN (ts1, …)] ]            (RANGE only)
			//   [ AUTOMATIC      [STORE IN (ts1, …)] ]             (LIST only)
			//   [ SUBPARTITION BY {RANGE|LIST|HASH} (cols)
			//        ( SUBPARTITIONS N [STORE IN (...)] |
			//          SUBPARTITION TEMPLATE (...) )? ]            (composite)
			//   [ PARTITIONS N [STORE IN (...)] ]                  (HASH sugar)
			//   [ (PARTITION x VALUES …, PARTITION y …) ]          (explicit list)
			//
			// The whole clause is captured raw and lifted by the translator.
			// We tolerate any combination by walking tokens with paren-depth
			// tracking and stopping at the next top-level table option
			// keyword or statement end. This keeps INTERVAL/AUTOMATIC/
			// SUBPARTITION BY/STORE IN as part of the partition clause
			// rather than letting parseTableOptions's catch-all swallow them
			// one token at a time.
			start := p.cur.Pos.Offset
			p.advance() // PARTITION
			if p.isKw("BY") {
				p.advance()
			}
			// Method: RANGE / LIST are keywords; HASH / REFERENCE / SYSTEM
			// are IDENT in the current keyword table. Match either kind.
			if p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT {
				switch strings.ToUpper(p.cur.Lit) {
				case "RANGE", "LIST", "HASH", "REFERENCE", "SYSTEM":
					p.advance()
				}
			}
			// Partition-key list (or constraint name for REFERENCE).
			if p.isPunct("(") {
				p.captureBalancedParens()
			}
			// Walk the rest of the clause. At depth 0, stop on a sentinel
			// that introduces the *next* table-level option (parseTableOptions
			// will pick it up). Sentinels match exactly the cases handled by
			// the surrounding loop so we don't over-shoot.
			depth := 0
		walk:
			for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
				if depth == 0 && (p.cur.Kind == TOK_KEYWORD || p.cur.Kind == TOK_IDENT) {
					switch strings.ToUpper(p.cur.Lit) {
					case "TABLESPACE", "STORAGE", "ORGANIZATION", "LOB", "COMMENT":
						break walk
					}
				}
				if p.isPunct("(") {
					depth++
				} else if p.isPunct(")") {
					depth--
				}
				p.advance()
			}
			opts.OraclePartitioning = string(p.src[start:p.cur.Pos.Offset])
		case p.isKw("ORGANIZATION"):
			p.advance()
			if p.isAnyKw("HEAP", "INDEX", "EXTERNAL") {
				opts.OracleOrganization = p.cur.Lit
				p.advance()
				// EXTERNAL tables carry a trailing
				//   (TYPE … DEFAULT DIRECTORY … ACCESS PARAMETERS (...)
				//    LOCATION (...) [REJECT LIMIT n] [PARALLEL n])
				// clause that we capture and discard wholesale — there's no
				// PG counterpart at the schema level (FOREIGN TABLE + FDW
				// has to be re-modelled by hand).
				if strings.EqualFold(opts.OracleOrganization, "EXTERNAL") && p.isPunct("(") {
					p.captureBalancedParens()
				}
				// IOT carries (PCTTHRESHOLD … INCLUDING col …) — same
				// strip-and-discard treatment; the IOT semantics are
				// surfaced via OracleOrganization=INDEX downstream.
				if strings.EqualFold(opts.OracleOrganization, "INDEX") && p.isPunct("(") {
					p.captureBalancedParens()
				}
			}
		case p.isKw("LOB"):
			start := p.cur.Pos.Offset
			p.advance()
			for p.cur.Kind != TOK_EOF && !p.atStatementEnd() &&
				!p.isKw("TABLESPACE") && !p.isKw("PARTITION") && !p.isKw("STORAGE") {
				p.advance()
			}
			opts.OracleLob = string(p.src[start:p.cur.Pos.Offset])
		case p.isKw("COMMENT"):
			p.advance()
			if p.isKw("IS") {
				p.advance()
			}
			if p.cur.Kind == TOK_STRING {
				opts.Comment = p.cur.Lit
				p.advance()
			}
		case p.atStatementEnd() || p.cur.Kind == TOK_EOF:
			return opts
		default:
			// unknown option — advance one token to avoid infinite loop
			p.advance()
		}
	}
}

func (p *Parser) readBalancedParensAfter() string {
	p.advance() // consume the keyword before the parens
	if !p.isPunct("(") {
		return ""
	}
	return p.captureBalancedParens()
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

func (p *Parser) parseDataType() ast.DataType {
	start := p.cur.Pos
	if p.cur.Kind != TOK_KEYWORD && p.cur.Kind != TOK_IDENT && p.cur.Kind != TOK_QUOTED_IDENT {
		p.errorHere("expected data type", "TYPE")
		return &ast.UserDefinedType{Name: "UNKNOWN", P: astPos(start)}
	}

	// Anchored types: ident[.ident]%TYPE or ident%ROWTYPE.
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
		// Could be a user-defined type or an anchored type.
		first := p.cur.Lit
		p.advance()
		name := first
		if p.isPunct(".") {
			p.advance()
			second, _ := p.parseIdent()
			name = first + "." + second
		}
		if p.isPunct("%") {
			p.advance()
			// TYPE is a keyword; ROWTYPE is not in the Oracle keyword
			// table — match either form via case-insensitive literal.
			if p.isAnyKwOrIdent("TYPE") {
				p.advance()
				return &ast.UserDefinedType{Name: name, Anchored: "TYPE", P: astPos(start)}
			}
			if p.isAnyKwOrIdent("ROWTYPE") {
				p.advance()
				return &ast.UserDefinedType{Name: name, Anchored: "ROWTYPE", P: astPos(start)}
			}
		}
		return &ast.UserDefinedType{Name: name, P: astPos(start)}
	}

	kw := p.cur.Lit
	switch kw {
	case "NUMBER", "NUMERIC", "DECIMAL", "DEC":
		p.advance()
		t := &ast.OracleNumberType{P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			// DBMS_METADATA emits `NUMBER(*,0)` for INTEGER/INT/SMALLINT — the
			// '*' means "any precision up to 38". We treat it as unspecified.
			if p.isPunct("*") {
				p.advance()
			} else if n, ok := p.intLit(); ok {
				t.Precision = int(n)
				t.HasPrec = true
			}
			if p.isPunct(",") {
				p.advance()
				if p.isPunct("*") {
					p.advance()
				} else if n, ok := p.intLit(); ok {
					t.Scale = int(n)
					t.HasScale = true
				}
			}
			p.expectPunct(")")
		}
		return t
	case "INTEGER", "INT", "SMALLINT":
		p.advance()
		// Oracle treats these as NUMBER(38).
		return &ast.OracleNumberType{Precision: 38, HasPrec: true, P: astPos(start)}
	case "FLOAT":
		p.advance()
		if p.isPunct("(") {
			p.advance()
			n, _ := p.intLit()
			_ = n
			p.expectPunct(")")
		}
		return &ast.FloatType{Name: "FLOAT", P: astPos(start)}
	case "REAL":
		p.advance()
		return &ast.FloatType{Name: "REAL", P: astPos(start)}
	case "DOUBLE":
		p.advance()
		if p.isKw("PRECISION") {
			p.advance()
		}
		return &ast.FloatType{Name: "DOUBLE", P: astPos(start)}
	case "BINARY_FLOAT":
		p.advance()
		return &ast.BinaryFloatType{P: astPos(start)}
	case "BINARY_DOUBLE":
		p.advance()
		return &ast.BinaryDoubleType{P: astPos(start)}
	case "VARCHAR2", "VARCHAR":
		p.advance()
		t := &ast.Varchar2Type{P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				t.Length = int(n)
				t.HasLength = true
			}
			if p.isKw("CHAR") {
				p.advance()
				t.SemanticsChar = true
			} else if p.isKw("BYTE") {
				p.advance()
			}
			p.expectPunct(")")
		}
		return t
	case "NVARCHAR2":
		p.advance()
		t := &ast.NVarchar2Type{P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				t.Length = int(n)
				t.HasLength = true
			}
			p.expectPunct(")")
		}
		return t
	case "CHAR", "CHARACTER":
		p.advance()
		t := &ast.CharType{Name: "CHAR", P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				t.Length = int(n)
				t.HasLength = true
			}
			if p.isKw("CHAR") || p.isKw("BYTE") {
				p.advance()
			}
			p.expectPunct(")")
		}
		return t
	case "NCHAR":
		p.advance()
		t := &ast.NCharType{P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				t.Length = int(n)
				t.HasLength = true
			}
			p.expectPunct(")")
		}
		return t
	case "CLOB":
		p.advance()
		return &ast.ClobType{P: astPos(start)}
	case "NCLOB":
		p.advance()
		return &ast.ClobType{National: true, P: astPos(start)}
	case "LONG":
		p.advance()
		if p.isKw("RAW") {
			p.advance()
			return &ast.RawType{Long: true, P: astPos(start)}
		}
		return &ast.ClobType{Long: true, P: astPos(start)}
	case "RAW":
		p.advance()
		t := &ast.RawType{P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				t.Length = int(n)
				t.HasLength = true
			}
			p.expectPunct(")")
		}
		return t
	case "BLOB":
		p.advance()
		return &ast.BlobType{Name: "BLOB", P: astPos(start)}
	case "BFILE":
		p.advance()
		return &ast.BFileType{P: astPos(start)}
	case "DATE":
		p.advance()
		return &ast.DateTimeType{P: astPos(start)}
	case "TIMESTAMP":
		p.advance()
		fsp := 6
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				fsp = int(n)
			}
			p.expectPunct(")")
		}
		if p.isKw("WITH") {
			p.advance()
			local := false
			if p.isKw("LOCAL") {
				p.advance()
				local = true
			}
			p.expectKw("TIME")
			p.expectKw("ZONE")
			return &ast.TimestampTZType{Fsp: fsp, Local: local, P: astPos(start)}
		}
		return &ast.TimestampType{Fsp: fsp, P: astPos(start)}
	case "INTERVAL":
		p.advance()
		if p.isKw("YEAR") {
			p.advance()
			t := &ast.IntervalYMType{Precision: 2, P: astPos(start)}
			if p.isPunct("(") {
				p.advance()
				if n, ok := p.intLit(); ok {
					t.Precision = int(n)
					t.HasPrec = true
				}
				p.expectPunct(")")
			}
			p.expectKw("TO")
			p.expectKw("MONTH")
			return t
		}
		if p.isKw("DAY") {
			p.advance()
			t := &ast.IntervalDSType{DayPrec: 2, FracPrec: 6, P: astPos(start)}
			if p.isPunct("(") {
				p.advance()
				if n, ok := p.intLit(); ok {
					t.DayPrec = int(n)
					t.HasDay = true
				}
				p.expectPunct(")")
			}
			p.expectKw("TO")
			p.expectKw("SECOND")
			if p.isPunct("(") {
				p.advance()
				if n, ok := p.intLit(); ok {
					t.FracPrec = int(n)
					t.HasFrac = true
				}
				p.expectPunct(")")
			}
			return t
		}
	case "ROWID":
		p.advance()
		return &ast.RowIdType{P: astPos(start)}
	case "UROWID":
		p.advance()
		t := &ast.RowIdType{Urowid: true, P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				t.Length = int(n)
				t.HasLength = true
			}
			p.expectPunct(")")
		}
		return t
	case "BOOLEAN":
		p.advance()
		return &ast.OracleBooleanType{P: astPos(start)}
	case "XMLTYPE":
		p.advance()
		return &ast.XmlType{P: astPos(start)}
	case "JSON":
		p.advance()
		return &ast.OracleJsonType{P: astPos(start)}
	case "VECTOR":
		p.advance()
		t := &ast.VectorType{P: astPos(start)}
		if p.isPunct("(") {
			p.advance()
			if n, ok := p.intLit(); ok {
				t.Dim = int(n)
				t.HasDim = true
			} else if p.isPunct("*") {
				p.advance()
			}
			if p.isPunct(",") {
				p.advance()
				if p.isAnyKw("FLOAT32", "FLOAT64", "INT8", "BINARY") {
					t.ElemKind = p.cur.Lit
					p.advance()
				}
			}
			p.expectPunct(")")
		}
		return t
	}
	// Fallback: treat as user-defined type reference.
	name := p.cur.Lit
	p.advance()
	return &ast.UserDefinedType{Name: name, P: astPos(start)}
}

// ---------------------------------------------------------------------------
// Minimal expression capture — enough for DEFAULT / CHECK / GENERATED (expr).
// ---------------------------------------------------------------------------

func (p *Parser) parseExprUntilBoundary() ast.Expr {
	start := p.cur.Pos
	startOff := p.cur.Pos.Offset
	depth := 0
	for {
		if p.cur.Kind == TOK_EOF {
			break
		}
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
		if depth == 0 && (p.isPunct(",") || p.atStatementEnd()) {
			break
		}
		if depth == 0 && (p.isKw("NOT") || p.isKw("NULL") || p.isKw("PRIMARY") ||
			p.isKw("UNIQUE") || p.isKw("CHECK") || p.isKw("REFERENCES") ||
			p.isKw("GENERATED") || p.isKw("VISIBLE") || p.isKw("INVISIBLE") ||
			p.isKw("CONSTRAINT") || p.isKw("ENABLE") || p.isKw("DISABLE") ||
			p.isKw("COLLATE")) {
			break
		}
		p.advance()
	}
	text := strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
	return &ast.RawExpr{Text: text, P: astPos(start)}
}

func (p *Parser) parseExprUntilParenClose() ast.Expr {
	start := p.cur.Pos
	startOff := p.cur.Pos.Offset
	depth := 0
	for {
		if p.cur.Kind == TOK_EOF {
			break
		}
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
		p.advance()
	}
	text := strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
	return &ast.RawExpr{Text: text, P: astPos(start)}
}

func (p *Parser) captureUntilParen() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	for {
		if p.cur.Kind == TOK_EOF {
			return string(p.src[startOff:p.cur.Pos.Offset])
		}
		if p.isPunct("(") {
			depth++
			p.advance()
			continue
		}
		if p.isPunct(")") {
			if depth == 0 {
				return strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
			}
			depth--
			p.advance()
			continue
		}
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// CREATE INDEX
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateIndex(start Position, unique, bitmap bool) ast.Stmt {
	p.expectKw("INDEX")
	_, name := p.parseQualifiedName()
	p.expectKw("ON")
	tbl := p.parseTableRef()
	s := &ast.CreateIndex{
		Name:   name,
		Unique: unique,
		Table:  tbl,
		P:      astPos(start),
	}
	if bitmap {
		s.Kind = "BITMAP"
	}
	p.expectPunct("(")
	s.Columns = p.parseIndexedCols()
	p.expectPunct(")")
	// Consume trailing storage/tablespace clauses.
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	return s
}

// ---------------------------------------------------------------------------
// CREATE VIEW / MATERIALIZED VIEW
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateView(start Position, orReplace bool) ast.Stmt {
	p.expectKw("VIEW")
	v := p.parseTableRef()
	s := &ast.CreateView{View: v, OrReplace: orReplace, P: astPos(start)}
	// Optional column list. Either present (col1, col2, …) — but not OF the
	// way Oracle object views use the same parens for an OBJECT IDENTIFIER
	// clause, which we handle below.
	if p.isPunct("(") {
		p.advance()
		for {
			c, _ := p.parseIdent()
			s.Columns = append(s.Columns, c)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		p.expectPunct(")")
	}
	// Oracle object-view clauses, all of which sit between the view name and
	// the AS keyword. PostgreSQL has no concept of object views, so we
	// consume and discard the metadata — the SELECT body is still captured
	// and becomes a plain PG view. The relational columns the SELECT
	// projects are what callers actually use, even if Oracle decorated them
	// with an object type.
	//
	//   CREATE VIEW name OF type [WITH OBJECT IDENTIFIER (col[, col]*)]
	//                            [UNDER parent_view]
	//                            [(attribute clauses…)]
	//                            AS SELECT …
	for !p.isKw("AS") && !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("OF") || p.isAnyKwOrIdent("OF"):
			p.advance()
			// Type name: schema.type or just type. Capture the bare
			// type name so the translator can rebuild the projected
			// column list with explicit aliases (PG has no positional
			// OF-binding).
			n1, _ := p.parseIdent()
			n2 := ""
			if p.isPunct(".") {
				p.advance()
				n2, _ = p.parseIdent()
			}
			if n2 != "" {
				s.OfType = n2
			} else {
				s.OfType = n1
			}
		case p.isKw("WITH"):
			p.advance()
			if p.isAnyKwOrIdent("OBJECT") {
				p.advance()
				if p.isAnyKwOrIdent("IDENTIFIER", "OID") {
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
				} else if p.isAnyKwOrIdent("DEFAULT") {
					p.advance()
				}
			}
		case p.isAnyKwOrIdent("UNDER"):
			p.advance()
			_, _ = p.parseIdent()
			if p.isPunct(".") {
				p.advance()
				_, _ = p.parseIdent()
			}
		case p.isPunct("("):
			// Object view attribute-clause list (constraints / scope refs /
			// etc. attached to specific columns). Skip the whole parenthesised
			// block — PG doesn't model these and the SELECT projects the
			// concrete columns regardless.
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
		default:
			// Unrecognised pre-AS token — bail and let expectKw("AS") raise
			// a parse error rather than silently swallowing arbitrary tokens.
			p.expectKw("AS")
			s.SelectBody = p.captureUntilDelimiter()
			return s
		}
	}
	p.expectKw("AS")
	s.SelectBody = p.captureUntilDelimiter()
	return s
}

func (p *Parser) parseCreateMaterializedView(start Position, orReplace bool) ast.Stmt {
	p.expectKw("VIEW")
	v := p.parseTableRef()
	s := &ast.CreateMaterializedView{View: v, OrReplace: orReplace, P: astPos(start)}
	// Consume optional BUILD {IMMEDIATE|DEFERRED} / REFRESH ... / other clauses
	// until we hit AS.
	for !p.isKw("AS") && !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("BUILD"):
			p.advance()
			if p.isAnyKw("IMMEDIATE", "DEFERRED") {
				s.BuildMode = p.cur.Lit
				p.advance()
			}
		case p.isKw("REFRESH"):
			p.advance()
			for p.isAnyKw("COMPLETE", "FAST", "FORCE", "NEVER") {
				s.RefreshMode = p.cur.Lit
				p.advance()
			}
			if p.isKw("ON") {
				p.advance()
				if p.isAnyKw("DEMAND", "COMMIT") {
					s.RefreshOn = p.cur.Lit
					p.advance()
				}
			}
		default:
			s.Extras = append(s.Extras, p.cur.Lit)
			p.advance()
		}
	}
	p.expectKw("AS")
	s.SelectBody = p.captureUntilDelimiter()
	return s
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
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("START"):
			p.advance()
			p.expectKw("WITH")
			n, _ := p.intLit()
			s.Start = n
			s.HasStart = true
		case p.isKw("INCREMENT"):
			p.advance()
			p.expectKw("BY")
			n, _ := p.intLit()
			s.Increment = n
			s.HasIncr = true
		case p.isKw("MAXVALUE"):
			p.advance()
			n, _ := p.intLit()
			s.MaxValue = n
			s.HasMax = true
		case p.isKw("NOMAXVALUE"):
			p.advance()
			s.NoMax = true
		case p.isKw("MINVALUE"):
			p.advance()
			n, _ := p.intLit()
			s.MinValue = n
			s.HasMin = true
		case p.isKw("NOMINVALUE"):
			p.advance()
			s.NoMin = true
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
			n, _ := p.intLit()
			s.Cache = n
			s.HasCache = true
		case p.isKw("NOCACHE"):
			p.advance()
			s.NoCache = true
		case p.isKw("ORDER") || p.isKw("NOORDER") || p.isKw("KEEP") || p.isKw("SESSION"):
			s.IgnoredOptions = append(s.IgnoredOptions, strings.ToUpper(p.cur.Lit))
			p.advance()
		case p.cur.Kind == TOK_IDENT &&
			(strings.EqualFold(p.cur.Lit, "NOKEEP") ||
				strings.EqualFold(p.cur.Lit, "SCALE") ||
				strings.EqualFold(p.cur.Lit, "NOSCALE") ||
				strings.EqualFold(p.cur.Lit, "EXTEND") ||
				strings.EqualFold(p.cur.Lit, "NOEXTEND") ||
				strings.EqualFold(p.cur.Lit, "SHARD") ||
				strings.EqualFold(p.cur.Lit, "NOSHARD") ||
				strings.EqualFold(p.cur.Lit, "SHARING")):
			lit := strings.ToUpper(p.cur.Lit)
			s.IgnoredOptions = append(s.IgnoredOptions, lit)
			p.advance()
			// SHARING <kind> takes a follow-on token (METADATA/DATA/EXTENDED/NONE).
			if lit == "SHARING" && (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD) {
				p.advance()
			}
		case p.isKw("GLOBAL"):
			s.IgnoredOptions = append(s.IgnoredOptions, "GLOBAL")
			p.advance()
		default:
			p.advance()
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// CREATE SYNONYM
// ---------------------------------------------------------------------------

// Synonyms are out of scope: PG has no SYNONYM concept and the user opted
// out of any compensation (stub schemas, thin views, body rewriting).
// We still consume the statement so dumps containing CREATE SYNONYM parse
// cleanly; the resulting NoopStmt is dropped silently by the translator.
func (p *Parser) parseCreateSynonym(_ Position, _, _ bool) ast.Stmt {
	p.expectKw("SYNONYM")
	return p.parseNoopToStmtEnd("CREATE SYNONYM")
}

// ---------------------------------------------------------------------------
// CREATE PROCEDURE / FUNCTION / TRIGGER / PACKAGE / TYPE — header parsed,
// body captured raw; the translator's body rewriter re-parses as PL/SQL.
// ---------------------------------------------------------------------------

func (p *Parser) parseCreateProcedure(start Position, _ bool) ast.Stmt {
	p.expectKw("PROCEDURE")
	_, name := p.parseQualifiedName()
	params := p.parseRoutineParams()
	// optional AUTHID, DETERMINISTIC, etc.
	p.skipRoutineOptions()
	if p.isKw("IS") || p.isKw("AS") {
		p.advance()
	}
	body := p.captureBlock()
	return &ast.CreateProcedure{
		Name:   name,
		Params: params,
		Body:   body,
		P:      astPos(start),
	}
}

func (p *Parser) parseCreateFunction(start Position, _ bool) ast.Stmt {
	p.expectKw("FUNCTION")
	_, name := p.parseQualifiedName()
	params := p.parseRoutineParams()
	p.expectKw("RETURN")
	ret := p.parseDataType()
	p.skipRoutineOptions()
	if p.isKw("IS") || p.isKw("AS") {
		p.advance()
	}
	body := p.captureBlock()
	return &ast.CreateFunction{
		Name:    name,
		Params:  params,
		Returns: ret,
		Body:    body,
		P:       astPos(start),
	}
}

func (p *Parser) parseCreateTrigger(start Position, _ bool) ast.Stmt {
	p.expectKw("TRIGGER")
	_, name := p.parseQualifiedName()
	t := &ast.CreateTrigger{Name: name, P: astPos(start)}
	// Two canonical forms:
	//   1. Classic:  {BEFORE|AFTER|INSTEAD OF} {INSERT|UPDATE|DELETE} [OR event]* ON table [FOR EACH ROW] BEGIN … END;
	//   2. Compound: FOR {INSERT|UPDATE|DELETE} [OR event]* ON table COMPOUND TRIGGER … END;
	// In the compound form the timing is encoded inside the body via
	// BEFORE EACH ROW / AFTER STATEMENT / … timing-point sections — the outer
	// trigger declaration has no BEFORE/AFTER/INSTEAD OF keyword.
	if p.isKw("FOR") {
		p.advance()
		t.Time = "COMPOUND"
	} else if p.isKw("BEFORE") {
		t.Time = "BEFORE"
		p.advance()
	} else if p.isKw("AFTER") {
		t.Time = "AFTER"
		p.advance()
	} else if p.isKw("INSTEAD") {
		p.advance()
		p.expectKw("OF")
		t.Time = "INSTEAD OF"
	}
	var events []string
	for {
		if p.isKw("INSERT") || p.isKw("UPDATE") || p.isKw("DELETE") {
			events = append(events, p.cur.Lit)
			p.advance()
			if p.isKw("OF") {
				// UPDATE OF col[,col]
				p.advance()
				for {
					_, _ = p.parseIdent()
					if !p.isPunct(",") {
						break
					}
					p.advance()
				}
			}
		} else if p.isAnyKwOrIdent("CREATE", "ALTER", "DROP", "ANALYZE",
			"AUDIT", "COMMENT", "GRANT", "NOAUDIT", "RENAME", "REVOKE",
			"TRUNCATE", "LOGON", "LOGOFF", "STARTUP", "SHUTDOWN",
			"SERVERERROR", "SUSPEND", "DB_ROLE_CHANGE") {
			t.SystemTrigger = true
			t.SystemEvent = strings.ToUpper(p.cur.Lit)
			events = append(events, p.cur.Lit)
			p.advance()
		}
		if p.isKw("OR") {
			p.advance()
			continue
		}
		break
	}
	t.Event = strings.Join(events, " OR ")
	p.expectKw("ON")
	if t.SystemTrigger {
		// `ON DATABASE` / `ON [schema.]SCHEMA` / `ON PLUGGABLE DATABASE`.
		if p.isAnyKwOrIdent("DATABASE") {
			t.SystemScope = "DATABASE"
			p.advance()
			if p.isAnyKwOrIdent("DATABASE") {
				p.advance() // PLUGGABLE DATABASE → consume the second token
			}
		} else if p.isAnyKwOrIdent("PLUGGABLE") {
			p.advance()
			if p.isAnyKwOrIdent("DATABASE") {
				p.advance()
			}
			t.SystemScope = "DATABASE"
		} else {
			// Could be SCHEMA or [schema.]SCHEMA — parse a possibly
			// qualified name and check for trailing SCHEMA.
			schema, name := p.parseQualifiedName()
			if strings.EqualFold(name, "SCHEMA") {
				t.SystemScope = "SCHEMA"
				if schema != "" {
					t.Table = ast.TableRef{Schema: schema, Name: "SCHEMA"}
				}
			} else {
				t.SystemScope = "SCHEMA"
				t.Table = ast.TableRef{Schema: schema, Name: name}
			}
		}
	} else if p.isKw("NESTED") {
		// Oracle: ON NESTED TABLE <attr> OF <view-or-table>. The trigger
		// fires on row-level operations against the nested-table column —
		// PostgreSQL has no equivalent (PG would model the same data as a
		// child table with an FK), so we capture the parent object as the
		// "Table" so the rest of the AST stays consistent. Downstream the
		// translator will emit a manual-review note for INSTEAD OF triggers
		// on object views / nested tables.
		p.advance()
		if p.isKw("TABLE") {
			p.advance()
		}
		// Skip the nested-attribute name.
		_, _ = p.parseIdent()
		if p.isPunct(".") {
			p.advance()
			_, _ = p.parseIdent()
		}
		// `OF <parent>` — the parent view/table the nested column belongs
		// to. parseTableRef handles schema-qualified names and quoted idents.
		if p.isKw("OF") {
			p.advance()
		}
		t.Table = p.parseTableRef()
		t.NestedTableTrigger = true
	} else {
		t.Table = p.parseTableRef()
	}
	// [REFERENCING NEW AS n OLD AS o] [FOR EACH ROW] [FOLLOWS name] [WHEN (cond)]
	//
	// Note for COMPOUND triggers: this option loop must also stop when it
	// hits the literal `COMPOUND` keyword. The source for a compound
	// trigger looks like
	//   FOR INSERT OR UPDATE ON orders
	//   COMPOUND TRIGGER
	//     BEFORE EACH ROW IS BEGIN … END BEFORE EACH ROW;
	//   END trg;
	// — the option loop's default branch would otherwise advance past
	// `COMPOUND`, `TRIGGER`, `BEFORE`, `EACH`, `ROW`, `IS` until it lands
	// on BEGIN, which throws away the section header(s) the translator
	// needs to auto-split the body into N PG triggers.
	for !p.isKw("BEGIN") && !p.isKw("DECLARE") && !p.isKw("COMPOUND") && !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("FOR"):
			p.advance()
			t.HasForEach = true
			if p.isKw("EACH") {
				p.advance()
				if p.isKw("ROW") {
					p.advance()
					t.ForEachRow = true
				} else if p.isKw("STATEMENT") {
					p.advance()
					t.ForEachRow = false
				}
			}
		case p.isKw("FOLLOWS"):
			p.advance()
			_, n := p.parseQualifiedName()
			t.Order = "FOLLOWS"
			t.OrderRef = n
		case p.isKw("PRECEDES"):
			p.advance()
			_, n := p.parseQualifiedName()
			t.Order = "PRECEDES"
			t.OrderRef = n
		case p.isKw("REFERENCING"):
			p.advance()
			// Loop while we see NEW / OLD / PARENT (each followed by AS <ident>)
			for p.isKw("NEW") || p.isKw("OLD") || p.isAnyKwOrIdent("PARENT") {
				which := strings.ToUpper(p.cur.Lit)
				p.advance()
				if p.isKw("AS") {
					p.advance()
				}
				alias := ""
				if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
					alias, _ = p.parseIdent()
				}
				switch which {
				case "NEW":
					t.NewAlias = alias
				case "OLD":
					t.OldAlias = alias
				}
			}
		case p.isKw("WHEN"):
			p.advance()
			if p.isPunct("(") {
				t.WhenCond = strings.TrimSpace(p.captureBalancedParens())
			}
		case p.isKw("ENABLE") || p.isKw("DISABLE"):
			p.advance()
		default:
			p.advance()
		}
	}
	if t.Time == "COMPOUND" {
		// Consume the literal `COMPOUND TRIGGER` header keywords that the
		// option loop above intentionally left in place — the body
		// capture below starts AFTER them.
		if p.isKw("COMPOUND") {
			p.advance()
			if p.isKw("TRIGGER") {
				p.advance()
			}
		}
		// COMPOUND TRIGGER bodies aren't a single BEGIN…END block: they're
		// a sequence of timing-point sections each wrapped in BEGIN…END,
		// the whole thing terminated by an outer `END [trigger_name];`.
		// captureBlock() strips section headers (BEFORE EACH ROW IS, …),
		// which is exactly what the translator needs to do the auto-split,
		// so we capture the full source slice verbatim from the current
		// cursor up to the outer `END [name];`.
		//
		// Termination rule: the outer END is the LAST `END` immediately
		// followed by either (a) the trigger name as a quoted/unquoted
		// ident + `;`, or (b) just `;`. Inner section ends always have
		// the timing-point keywords between END and `;`
		// (`END BEFORE EACH ROW;`, `END AFTER STATEMENT;`), and inner
		// statement ends have a keyword (END IF, END LOOP, END CASE).
		// We scan token-by-token and snapshot the cursor every time we
		// see `END <name>?;` to remember the latest plausible outer end —
		// the very last one in the stream is the real outer end.
		startOff := p.cur.Pos.Offset
		var lastOuterEnd int
		trgLower := strings.ToLower(strings.Trim(name, `"`))
		for p.cur.Kind != TOK_EOF {
			if p.isPunct("/") || p.cur.Kind == TOK_SLASH_TERM {
				break
			}
			if !p.isKw("END") {
				p.advance()
				continue
			}
			// Save offset of the END token start.
			endStart := p.cur.Pos.Offset
			p.advance()
			// What follows END decides whether this is the outer end:
			//   - `;`                      → outer end (anonymous block)
			//   - <trigger_name>;          → outer end (matches our trigger)
			// Anything else (END IF, END LOOP, END CASE, END BEFORE EACH
			// ROW;) is a section/statement end — keep scanning.
			if p.isPunct(";") {
				p.advance()
				lastOuterEnd = p.cur.Pos.Offset
				continue
			}
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
				idLower := strings.ToLower(strings.Trim(p.cur.Lit, `"`))
				if idLower == trgLower {
					// END <trigger_name> [;] → outer end
					p.advance()
					if p.isPunct(";") {
						p.advance()
					}
					lastOuterEnd = p.cur.Pos.Offset
					continue
				}
			}
			// Not an outer end — the END belongs to a section/statement.
			// Don't advance further; continue from current position.
			_ = endStart
		}
		if lastOuterEnd == 0 {
			lastOuterEnd = p.cur.Pos.Offset
		}
		t.Body = string(p.src[startOff:lastOuterEnd])
	} else {
		t.Body = p.captureBlock()
	}
	return t
}

func (p *Parser) parseCreatePackage(start Position, orReplace bool) ast.Stmt {
	p.expectKw("PACKAGE")
	body := false
	if p.isKw("BODY") {
		p.advance()
		body = true
	}
	schema, name := p.parseQualifiedName()
	p.skipRoutineOptions()
	if p.isKw("IS") || p.isKw("AS") {
		p.advance()
	}
	raw := p.captureBlock()
	if body {
		// A package body contains N function/procedure implementations each
		// with its own BEGIN/END, followed by the package's closing
		// END [pkg_name];. captureBlock() only captured the first nested
		// routine; consume the rest + the outer END.
		startOff := p.cur.Pos.Offset
		for p.cur.Kind != TOK_EOF {
			if p.isPunct(";") || p.isPunct("/") || p.cur.Kind == TOK_SLASH_TERM {
				p.advance()
				continue
			}
			if p.isKw("END") {
				p.advance()
				if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
					p.advance()
				}
				break
			}
			// Next nested routine — skip header up to BEGIN. CASE
			// expressions in CURSOR ORDER BYs / column projections embed
			// their own ENDs that must NOT be confused with the package
			// or routine boundary, so consume them recursively.
			p.skipUntilRoutineBegin()
			if p.isKw("BEGIN") {
				_ = p.captureBlock()
			}
		}
		raw += string(p.src[startOff:p.cur.Pos.Offset])
		return &ast.CreatePackageBody{
			OrReplace: orReplace,
			Schema:    schema,
			Name:      name,
			Body:      raw,
			P:         astPos(start),
		}
	}
	return &ast.CreatePackage{
		OrReplace: orReplace,
		Schema:    schema,
		Name:      name,
		Spec:      raw,
		P:         astPos(start),
	}
}

func (p *Parser) parseCreateType(start Position, orReplace bool) ast.Stmt {
	p.expectKw("TYPE")
	isBody := false
	if p.isKw("BODY") {
		p.advance()
		isBody = true
	}
	schema, name := p.parseQualifiedName()
	p.skipRoutineOptions()
	// Oracle subtype declaration: `CREATE TYPE name UNDER parent_type (
	//   …extra_attrs… , method_specs… )`. PostgreSQL composite types have
	// no inheritance; we capture the parent name on the AST so the
	// translator can flatten the subtype into a standalone composite
	// (or surface a manual-review note), and we treat the body the same
	// way as an OBJECT type from this point on.
	parentType := ""
	if p.isKw("UNDER") || p.isAnyKwOrIdent("UNDER") {
		p.advance()
		_, n := p.parseQualifiedName()
		parentType = n
	}
	if p.isKw("IS") || p.isKw("AS") {
		p.advance()
	}
	if isBody {
		raw := p.captureBlock()
		// Same as package body: TYPE BODY contains N MEMBER FUNCTION /
		// MEMBER PROCEDURE implementations each with BEGIN/END, plus the
		// type's own closing END;. captureBlock() only grabbed the first
		// member's body — consume the rest + the outer END.
		startOff := p.cur.Pos.Offset
		for p.cur.Kind != TOK_EOF {
			if p.isPunct(";") || p.isPunct("/") || p.cur.Kind == TOK_SLASH_TERM {
				p.advance()
				continue
			}
			if p.isKw("END") {
				p.advance()
				if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT {
					p.advance()
				}
				break
			}
			for !p.isKw("BEGIN") && !p.isKw("END") && p.cur.Kind != TOK_EOF {
				p.advance()
			}
			if p.isKw("BEGIN") {
				_ = p.captureBlock()
			}
		}
		raw += string(p.src[startOff:p.cur.Pos.Offset])
		return &ast.CreateTypeBody{
			OrReplace: orReplace,
			Schema:    schema,
			Name:      name,
			Body:      raw,
			P:         astPos(start),
		}
	}
	kind := ""
	switch {
	case p.isAnyKwOrIdent("OBJECT"):
		kind = "OBJECT"
		p.advance()
	case p.isAnyKwOrIdent("VARRAY", "VARYING"):
		kind = "VARRAY"
		p.advance()
		// VARYING ARRAY → consume the ARRAY keyword
		if p.isAnyKwOrIdent("ARRAY") {
			p.advance()
		}
	case p.isKw("TABLE"):
		kind = "TABLE"
		p.advance()
	}
	// Subtype declarations (`UNDER parent`) skip the OBJECT/VARRAY/TABLE
	// keyword — Oracle infers OBJECT-kind from the parent. Treat them as
	// OBJECT for downstream attribute parsing.
	if kind == "" && parentType != "" {
		kind = "OBJECT"
	}
	t := &ast.CreateType{
		OrReplace:  orReplace,
		Schema:     schema,
		Name:       name,
		Kind:       kind,
		ParentType: parentType,
		P:          astPos(start),
	}
	// For OBJECT types, parse the attribute list via the column-def
	// machinery so the translator gets a structured Attributes slice
	// (mappable to a PG composite type). MEMBER FUNCTION / MEMBER
	// PROCEDURE bodies set HasMethods and the parser falls back to the
	// raw-body capture for those.
	switch kind {
	case "OBJECT":
		bodyStart := p.cur.Pos.Offset
		if p.isPunct("(") {
			p.advance()
			for !p.isPunct(")") && p.cur.Kind != TOK_EOF {
				if p.isAnyKwOrIdent("MEMBER", "STATIC", "MAP", "ORDER", "CONSTRUCTOR",
					"FINAL", "INSTANTIABLE", "OVERRIDING") {
					t.HasMethods = true
					// Skip to next top-level comma or ')' so we don't
					// mis-parse the method header as an attribute.
					depth := 0
					for p.cur.Kind != TOK_EOF {
						if p.isPunct("(") {
							depth++
						} else if p.isPunct(")") {
							if depth == 0 {
								break
							}
							depth--
						}
						if depth == 0 && p.isPunct(",") {
							break
						}
						p.advance()
					}
				} else {
					col := p.parseColumnDef()
					if col != nil {
						t.Attributes = append(t.Attributes, *col)
					}
				}
				if p.isPunct(",") {
					p.advance()
				}
			}
			if p.isPunct(")") {
				p.advance()
			}
		}
		t.Body = strings.TrimSpace(string(p.src[bodyStart:p.cur.Pos.Offset]))
		// Consume any trailing options up to ; (NOT FINAL, AUTHID, etc.)
		for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
			p.advance()
		}
	case "VARRAY", "TABLE":
		// `VARRAY(N) OF <elem>` or `TABLE OF <elem>` — capture the
		// element-type expression up to `;`.
		bodyStart := p.cur.Pos.Offset
		if kind == "VARRAY" && p.isPunct("(") {
			p.captureBalancedParens()
		}
		if p.isKw("OF") {
			p.advance()
		}
		elStart := p.cur.Pos.Offset
		for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
			p.advance()
		}
		t.ElementType = strings.TrimSpace(string(p.src[elStart:p.cur.Pos.Offset]))
		t.Body = strings.TrimSpace(string(p.src[bodyStart:p.cur.Pos.Offset]))
	default:
		t.Body = p.captureUntilDelimiter()
	}
	return t
}

// parseRoutineParams — (p1 [IN|OUT|IN OUT [NOCOPY]] type [DEFAULT expr], ...).
// A missing paren list is valid (0 params).
func (p *Parser) parseRoutineParams() []ast.Param {
	var out []ast.Param
	if !p.isPunct("(") {
		return out
	}
	p.advance()
	if p.isPunct(")") {
		p.advance()
		return out
	}
	for {
		var param ast.Param
		param.Name, _ = p.parseIdent()
		// direction
		switch {
		case p.isKw("IN"):
			p.advance()
			if p.isKw("OUT") {
				p.advance()
				param.Direction = "INOUT"
			} else {
				param.Direction = "IN"
			}
		case p.isKw("OUT"):
			p.advance()
			param.Direction = "OUT"
		}
		if p.isKw("NOCOPY") {
			p.advance()
		}
		param.Type = p.parseDataType()
		// DEFAULT expr / := expr.
		// Use a routine-parameter expression reader rather than the column
		// boundary reader: in a parameter list, `DEFAULT NULL` is a literal
		// expression — only `,` and matching `)` end it. The column-context
		// reader treats NULL as a constraint terminator (since it precedes
		// `NOT NULL`), which would prematurely cut the default and leave the
		// closing `)` unmatched, cascading into thousands of follow-on parse
		// errors when many procedures share the same idiom.
		if p.isKw("DEFAULT") || p.cur.Kind == TOK_ASSIGN {
			p.advance()
			param.Default = p.parseRoutineParamDefault()
		}
		out = append(out, param)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	p.expectPunct(")")
	return out
}

// skipUntilRoutineBegin advances tokens until the current token is BEGIN or
// the package's outer END (or EOF). CASE expressions encountered in routine
// declarations (CURSOR ... ORDER BY CASE WHEN ... END DESC, default
// expressions involving CASE, …) are stepped through to their matching bare
// END so they aren't mistaken for the surrounding routine's terminator.
func (p *Parser) skipUntilRoutineBegin() {
	for p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("BEGIN"), p.isKw("END"):
			return
		case p.isKw("CASE"):
			p.advance()
			depth := 1
			for depth > 0 && p.cur.Kind != TOK_EOF {
				switch {
				case p.isKw("CASE"):
					depth++
					p.advance()
				case p.isKw("END"):
					next := p.peek()
					if next.Kind == TOK_KEYWORD && next.Lit == "CASE" {
						p.advance()
						p.advance()
						depth--
						continue
					}
					// Bare END closes a CASE expression; consume.
					p.advance()
					depth--
				default:
					p.advance()
				}
			}
		default:
			p.advance()
		}
	}
}

// parseRoutineParamDefault reads a parameter's DEFAULT/:= expression up to
// the next top-level `,` or matching `)`. Unlike parseExprUntilBoundary, it
// does NOT terminate on Oracle column-constraint keywords (NULL, NOT,
// PRIMARY, …) because in this context those tokens are part of an
// expression, not a column constraint marker.
func (p *Parser) parseRoutineParamDefault() ast.Expr {
	start := p.cur.Pos
	startOff := p.cur.Pos.Offset
	depth := 0
	for {
		if p.cur.Kind == TOK_EOF {
			break
		}
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
		if depth == 0 && (p.isPunct(",") || p.atStatementEnd()) {
			break
		}
		p.advance()
	}
	text := strings.TrimSpace(string(p.src[startOff:p.cur.Pos.Offset]))
	return &ast.RawExpr{Text: text, P: astPos(start)}
}

// skipRoutineOptions consumes routine-level modifiers between the header and
// the IS/AS keyword: AUTHID CURRENT_USER|DEFINER, DETERMINISTIC, PARALLEL_ENABLE,
// PIPELINED, RESULT_CACHE, etc.
func (p *Parser) skipRoutineOptions() {
	for {
		switch {
		case p.isKw("AUTHID"):
			p.advance()
			if p.isKw("CURRENT_USER") || p.isKw("DEFINER") {
				p.advance()
			}
		case p.isKw("DETERMINISTIC"), p.isKw("PIPELINED"),
			p.isKw("PARALLEL_ENABLE"), p.isKw("RESULT_CACHE"):
			p.advance()
		default:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// ALTER TABLE
// ---------------------------------------------------------------------------

func (p *Parser) parseAlter() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("ALTER")
	switch {
	case p.isKw("TABLE"):
		p.advance()
		ref := p.parseTableRef()
		s := &ast.AlterTable{Table: ref, P: astPos(start)}
		for {
			acts := p.parseAlterActions()
			s.Actions = append(s.Actions, acts...)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		return s
	case p.isKw("SEQUENCE"):
		return p.parseAlterSequence(start)
	case p.isKw("INDEX"):
		return p.parseAlterIndex(start)
	case p.isKw("TRIGGER"):
		return p.parseAlterTrigger(start)
	case p.isKw("VIEW"):
		return p.parseAlterView(start)
	case p.isKw("TYPE"):
		return p.parseAlterType(start)
	case p.isKw("PROCEDURE"), p.isKw("FUNCTION"), p.isKw("PACKAGE"):
		return p.parseAlterRoutine(start)
	case p.isKw("MATERIALIZED"):
		// ALTER MATERIALIZED VIEW [LOG] … — distinguish via the next token.
		p.advance() // MATERIALIZED
		if p.isKw("VIEW") {
			p.advance() // VIEW
			if p.isAnyKwOrIdent("LOG") {
				p.advance()
				return p.parseNoopToStmtEnd("ALTER MATERIALIZED VIEW LOG")
			}
			return p.parseNoopToStmtEnd("ALTER MATERIALIZED VIEW")
		}
		return p.parseNoopToStmtEnd("ALTER MATERIALIZED")
	}
	return p.parseNoopToStmtEnd("ALTER")
}

// parseAlterRoutine handles `ALTER {PROCEDURE|FUNCTION|PACKAGE} [schema.]
// name COMPILE [BODY|SPECIFICATION] [REUSE SETTINGS|DEBUG|…]`. PG has no
// recompile DDL — functions are reparsed every CREATE FUNCTION. Captured
// for an info-level explanation per dropped statement.
func (p *Parser) parseAlterRoutine(start Position) ast.Stmt {
	kind := strings.ToUpper(p.cur.Lit)
	p.advance() // PROCEDURE | FUNCTION | PACKAGE
	s := &ast.AlterRoutine{Kind: kind, P: astPos(start)}
	s.Schema, s.Name = p.parseQualifiedName()
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	return s
}

// parseAlterType handles `ALTER TYPE [schema.]name <action>`. Oracle
// supports type evolution (ADD/MODIFY/DROP ATTRIBUTE, OVERRIDING METHOD,
// COMPILE) on object types; the PG ALTER TYPE that maps closest is for
// composite types (ADD/RENAME/DROP ATTRIBUTE) but Oracle's object-with-
// methods model breaks the round-trip. We classify the leading action
// and let the translator emit a manual-review prereq.
func (p *Parser) parseAlterType(start Position) ast.Stmt {
	p.expectKw("TYPE")
	s := &ast.AlterType{P: astPos(start)}
	s.Schema, s.Name = p.parseQualifiedName()
	switch {
	case p.isAnyKwOrIdent("COMPILE"):
		s.Action = "COMPILE"
	case p.isAnyKwOrIdent("ADD"):
		// ADD ATTRIBUTE | ADD MEMBER FUNCTION | ADD MAP MEMBER FUNCTION | …
		p.advance()
		if p.isAnyKwOrIdent("ATTRIBUTE") {
			s.Action = "ADD_ATTRIBUTE"
		} else {
			s.Action = "OTHER"
		}
	case p.isAnyKwOrIdent("DROP"):
		p.advance()
		if p.isAnyKwOrIdent("ATTRIBUTE") {
			s.Action = "DROP_ATTRIBUTE"
		} else {
			s.Action = "OTHER"
		}
	case p.isAnyKwOrIdent("MODIFY"):
		p.advance()
		if p.isAnyKwOrIdent("ATTRIBUTE") {
			s.Action = "MODIFY_ATTRIBUTE"
		} else {
			s.Action = "OTHER"
		}
	default:
		s.Action = "OTHER"
	}
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	return s
}

// parseAlterView handles `ALTER VIEW [schema.]name <action>`. Oracle's
// actions (COMPILE / EDITIONABLE / NONEDITIONABLE / ADD/MODIFY/DROP
// CONSTRAINT) have no PG counterpart; the AST captures the leading action
// keyword for the explanation log and consumes the rest of the statement.
func (p *Parser) parseAlterView(start Position) ast.Stmt {
	p.expectKw("VIEW")
	s := &ast.AlterView{P: astPos(start)}
	s.Schema, s.Name = p.parseQualifiedName()
	switch {
	case p.isAnyKwOrIdent("COMPILE"):
		s.Action = "COMPILE"
	case p.isAnyKwOrIdent("EDITIONABLE"):
		s.Action = "EDITIONABLE"
	case p.isAnyKwOrIdent("NONEDITIONABLE"):
		s.Action = "NONEDITIONABLE"
	case p.isKw("ADD") || p.isAnyKwOrIdent("MODIFY") || p.isKw("DROP"):
		// followed by CONSTRAINT in Oracle — but we don't translate it
		// either way.
		s.Action = "CONSTRAINT"
	default:
		s.Action = "OTHER"
	}
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	return s
}

// parseAlterTrigger handles `ALTER TRIGGER [schema.]name {ENABLE | DISABLE
// | COMPILE | RENAME TO new}`. Trailing options (DEBUG, REUSE SETTINGS,
// COMPILE clauses) are consumed but not preserved — the translator only
// cares about the primary action.
func (p *Parser) parseAlterTrigger(start Position) ast.Stmt {
	p.expectKw("TRIGGER")
	s := &ast.AlterTrigger{P: astPos(start)}
	s.Schema, s.Name = p.parseQualifiedName()
	switch {
	case p.isAnyKwOrIdent("ENABLE"):
		p.advance()
		s.Action = "ENABLE"
	case p.isAnyKwOrIdent("DISABLE"):
		p.advance()
		s.Action = "DISABLE"
	case p.isAnyKwOrIdent("COMPILE"):
		p.advance()
		s.Action = "COMPILE"
	case p.isAnyKwOrIdent("RENAME"):
		p.advance()
		if p.isKw("TO") {
			p.advance()
		}
		_, newName := p.parseQualifiedName()
		s.NewName = newName
		s.Action = "RENAME"
	default:
		s.Action = "COMPILE"
	}
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	return s
}

// parseAlterIndex handles `ALTER INDEX [schema.]name <action>`. The .g4
// distinguishes alter_index_ops_set1 (storage-level, all dropped in PG)
// from alter_index_ops_set2 (REBUILD / RENAME / ENABLE / DISABLE / …).
// We classify on the first identifier of the action and capture the rest
// of the statement raw — only RENAME TO carries a PG translation, the
// other actions surface as info-level explanations.
func (p *Parser) parseAlterIndex(start Position) ast.Stmt {
	p.expectKw("INDEX")
	s := &ast.AlterIndex{P: astPos(start)}
	s.Schema, s.Name = p.parseQualifiedName()

	tailStart := p.cur.Pos.Offset
	switch {
	case p.isAnyKwOrIdent("RENAME"):
		p.advance()
		if p.isKw("TO") {
			p.advance()
		}
		_, newName := p.parseQualifiedName()
		s.NewName = newName
		s.Action = "RENAME"
	case p.isAnyKwOrIdent("REBUILD"):
		s.Action = "REBUILD"
	case p.isAnyKwOrIdent("COMPILE"):
		s.Action = "COMPILE"
	case p.isAnyKwOrIdent("ENABLE"):
		s.Action = "ENABLE"
	case p.isAnyKwOrIdent("DISABLE"):
		s.Action = "DISABLE"
	case p.isAnyKwOrIdent("UNUSABLE"):
		s.Action = "UNUSABLE"
	case p.isAnyKwOrIdent("VISIBLE"):
		s.Action = "VISIBLE"
	case p.isAnyKwOrIdent("INVISIBLE"):
		s.Action = "INVISIBLE"
	case p.isAnyKwOrIdent("COMPRESS"):
		s.Action = "COMPRESS"
	case p.isAnyKwOrIdent("NOCOMPRESS"):
		s.Action = "NOCOMPRESS"
	case p.isAnyKwOrIdent("MODIFY"), p.isAnyKwOrIdent("ADD"),
		p.isAnyKwOrIdent("SPLIT"), p.isAnyKwOrIdent("COALESCE"),
		p.isAnyKwOrIdent("DROP"):
		s.Action = "PARTITION"
	default:
		s.Action = "OTHER"
	}
	// Consume the rest of the statement so we don't re-enter the dispatcher
	// mid-clause. Capture as raw tail for the explanation log.
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	if endOff > tailStart {
		s.RawTail = strings.TrimSpace(string(p.src[tailStart:endOff]))
	}
	return s
}

// isAnyKwOrIdent matches against a token that is either a registered
// keyword or a bare IDENT with the given uppercase literal — useful for
// Oracle's many "contextual" reserved words that the keyword table omits.
func (p *Parser) isAnyKwOrIdent(literals ...string) bool {
	if p.cur.Kind != TOK_KEYWORD && p.cur.Kind != TOK_IDENT {
		return false
	}
	for _, lit := range literals {
		if strings.EqualFold(p.cur.Lit, lit) {
			return true
		}
	}
	return false
}

// parseAlterSequence handles `ALTER SEQUENCE [schema.]name <spec>+`. Each
// spec mirrors a CREATE SEQUENCE option except START WITH (only allowed
// here in conjunction with RESTART, Oracle 12.2+). Unknown / Oracle-only
// specs are recorded in IgnoredOptions for the translator to surface.
func (p *Parser) parseAlterSequence(start Position) ast.Stmt {
	p.expectKw("SEQUENCE")
	s := &ast.AlterSequence{P: astPos(start)}
	s.Schema, s.Name = p.parseQualifiedName()
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		switch {
		case p.isKw("INCREMENT"):
			p.advance()
			if p.isKw("BY") {
				p.advance()
			}
			n, _ := p.intLit()
			s.Increment = n
			s.HasIncr = true
		case p.isKw("MAXVALUE"):
			p.advance()
			n, _ := p.intLit()
			s.MaxValue = n
			s.HasMax = true
		case p.isKw("NOMAXVALUE"):
			p.advance()
			s.NoMax = true
		case p.isKw("MINVALUE"):
			p.advance()
			n, _ := p.intLit()
			s.MinValue = n
			s.HasMin = true
		case p.isKw("NOMINVALUE"):
			p.advance()
			s.NoMin = true
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
			n, _ := p.intLit()
			s.Cache = n
			s.HasCache = true
		case p.isKw("NOCACHE"):
			p.advance()
			s.NoCache = true
		case p.isKw("START"):
			// Oracle 12.2+: ALTER SEQUENCE … START WITH N is allowed and
			// behaves like RESTART WITH N (only takes effect at the next
			// nextval). PG has no plain START on ALTER, so we map it to
			// RESTART WITH N.
			p.advance()
			if p.isKw("WITH") {
				p.advance()
			}
			n, _ := p.intLit()
			s.StartWith = n
			s.HasStartWith = true
			s.HasRestart = true
			s.Restart = true
		case p.cur.Kind == TOK_IDENT && strings.EqualFold(p.cur.Lit, "RESTART"):
			// `RESTART` isn't in the keyword set; match on literal.
			p.advance()
			s.HasRestart = true
			s.Restart = true
			if p.isKw("WITH") {
				p.advance()
				n, _ := p.intLit()
				s.StartWith = n
				s.HasStartWith = true
			}
		case p.isKw("ORDER") || p.isKw("NOORDER") || p.isKw("KEEP"):
			s.IgnoredOptions = append(s.IgnoredOptions, strings.ToUpper(p.cur.Lit))
			p.advance()
		case p.cur.Kind == TOK_IDENT &&
			(strings.EqualFold(p.cur.Lit, "NOKEEP") ||
				strings.EqualFold(p.cur.Lit, "SCALE") ||
				strings.EqualFold(p.cur.Lit, "NOSCALE") ||
				strings.EqualFold(p.cur.Lit, "EXTEND") ||
				strings.EqualFold(p.cur.Lit, "NOEXTEND") ||
				strings.EqualFold(p.cur.Lit, "SHARD") ||
				strings.EqualFold(p.cur.Lit, "NOSHARD") ||
				strings.EqualFold(p.cur.Lit, "SHARING")):
			lit := strings.ToUpper(p.cur.Lit)
			s.IgnoredOptions = append(s.IgnoredOptions, lit)
			p.advance()
			// SHARING <kind> takes a follow-on token (METADATA/DATA/EXTENDED/NONE).
			if lit == "SHARING" && (p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD) {
				p.advance()
			}
		case p.isKw("SESSION") || p.isKw("GLOBAL"):
			s.IgnoredOptions = append(s.IgnoredOptions, p.cur.Lit)
			p.advance()
		default:
			// Unknown token — advance to avoid an infinite loop. Oracle dump
			// generators emit a finite vocabulary here, so an unrecognised
			// token is most likely a future syntax extension.
			p.advance()
		}
	}
	return s
}

// parseAlterActions parses one or more comma-grouped Oracle ALTER
// TABLE actions and returns them as a flat slice. Most actions are
// single (one ADD CONSTRAINT, one DROP COLUMN), but Oracle's
// parenthesised multi-column form `ADD (c1 t1, c2 t2)` and
// `MODIFY (c1 t1, c2 t2)` expand into one AlterAction per column so
// the translator emits multi-action PG ALTER TABLE statements
// (`ADD COLUMN c1 …, ADD COLUMN c2 …`).
func (p *Parser) parseAlterActions() []ast.AlterAction {
	a := p.parseAlterAction()
	if a == nil {
		return nil
	}
	out := []ast.AlterAction{*a}
	out = append(out, a.Following...)
	a.Following = nil
	out[0] = *a
	return out
}

func (p *Parser) parseAlterAction() *ast.AlterAction {
	switch {
	case p.isKw("ADD"):
		p.advance()
		if p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") ||
			p.isKw("FOREIGN") || p.isKw("CHECK") {
			c := p.parseTableConstraint()
			if c == nil {
				return nil
			}
			return &ast.AlterAction{Kind: "ADD_CONSTRAINT", Constraint: c}
		}
		// ADD (col1 type [...], col2 type [...]) — produce one
		// ADD_COLUMN per column. The first becomes the returned
		// AlterAction; additional ones go into Following so the
		// caller (parseAlterActions) can flatten them into the
		// statement-level Actions slice.
		if p.isPunct("(") {
			p.advance()
			col := p.parseColumnDef()
			var follow []ast.AlterAction
			for p.isPunct(",") {
				p.advance()
				if c2 := p.parseColumnDef(); c2 != nil {
					follow = append(follow, ast.AlterAction{Kind: "ADD_COLUMN", Column: c2})
				}
			}
			p.expectPunct(")")
			if col != nil {
				return &ast.AlterAction{Kind: "ADD_COLUMN", Column: col, Following: follow}
			}
			return nil
		}
		col := p.parseColumnDef()
		if col == nil {
			return nil
		}
		return &ast.AlterAction{Kind: "ADD_COLUMN", Column: col}
	case p.isKw("DROP"):
		p.advance()
		if p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") || p.isKw("COLUMN") {
			p.advance()
		}
		name, _ := p.parseIdent()
		return &ast.AlterAction{Kind: "DROP_CONSTRAINT", DropName: name}
	case p.isKw("MODIFY"):
		p.advance()
		if p.isPunct("(") {
			p.advance()
			col := p.parseColumnDef()
			for p.isPunct(",") {
				p.advance()
				_ = p.parseColumnDef()
			}
			p.expectPunct(")")
			if col != nil {
				return &ast.AlterAction{Kind: "MODIFY_COLUMN", Column: col}
			}
			return nil
		}
		col := p.parseColumnDef()
		if col == nil {
			return nil
		}
		return &ast.AlterAction{Kind: "MODIFY_COLUMN", Column: col}
	case p.isKw("RENAME"):
		p.advance()
		if p.isKw("COLUMN") {
			p.advance()
		}
		old, _ := p.parseIdent()
		p.expectKw("TO")
		newName, _ := p.parseIdent()
		return &ast.AlterAction{Kind: "RENAME_COLUMN", DropName: old + ">" + newName}
	}
	p.errorHere("unsupported ALTER action", "ADD|DROP|MODIFY|RENAME")
	p.syncToDelimiter()
	return nil
}
