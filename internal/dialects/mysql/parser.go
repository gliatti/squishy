// Package mysql implements a MySQL/MariaDB DDL + routine parser.
//
// The canonical grammar reference is grammars-v4 (Positive-Technologies, MIT),
// vendored under reference/ (MySqlLexer.g4, MySqlParser.g4). This Go code is a
// hand-rolled recursive-descent parser implementing the subset of that grammar
// required by squishy (DDL + routine headers + raw bodies). The .g4 files are
// the source of truth; when the parser diverges from them, the .g4 wins and
// the Go code is updated to match. NO ANTLR runtime is linked — the .g4 files
// serve as specification, not as generation input.
//
// Entry point: Parse(src string) (stmts []ast.Stmt, errs ErrorList).
package mysql

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

type Parser struct {
	l      *Lexer
	errs   ErrorList
	cur    Token
	peeked bool

	// raw_block tracking
	src []rune
}

// Parse parses a MySQL script into a list of statements. It collects as many
// errors as it can by synchronizing on the current delimiter (default ";").
func Parse(src string) ([]ast.Stmt, ErrorList) {
	p := &Parser{l: NewLexer(src), src: []rune(src)}
	p.advance()
	var stmts []ast.Stmt
	for {
		// Skip stray delimiters and comments at script level.
		if p.isPunct(";") {
			p.advance()
			continue
		}
		if p.cur.Kind == TOK_EOF {
			break
		}
		// DELIMITER directive: switch the lexer's statement delimiter.
		if p.cur.Kind == TOK_DELIMITER_CMD {
			p.l.SetDelimiter(p.cur.Lit)
			p.advance()
			continue
		}
		start := p.cur.Pos
		stmt := p.parseStatement()
		if stmt != nil {
			stmts = append(stmts, stmt)
		} else {
			// parse error inside, synchronize to next delimiter boundary
			p.syncToDelimiter()
			_ = start
		}
		// Consume the end-of-statement delimiter (may be ";", "//", "$$", etc.)
		p.consumeStatementEnd()
	}
	return stmts, p.errs
}

// ---------------------------------------------------------------------------
// statement dispatch
// ---------------------------------------------------------------------------

func (p *Parser) parseStatement() ast.Stmt {
	switch {
	case p.isKw("CREATE"):
		return p.parseCreate()
	case p.isKw("DROP"):
		return p.parseDrop()
	case p.isKw("ALTER"):
		return p.parseAlter()
	case p.isKw("USE"):
		return p.parseUseOrCall("USE")
	case p.isKw("SET"):
		return p.parseSet()
	case p.isKw("CALL"):
		return p.parseUseOrCall("CALL")
	case p.isKw("TRUNCATE"):
		return p.parseTruncate()
	case p.isKw("RENAME"):
		return p.parseRenameTable()
	case p.isKw("LOAD"):
		// LOAD DATA INFILE / LOAD XML — no PG counterpart. Consume to end.
		return p.parseNoopToStmtEnd("LOAD")
	case p.isKw("GRANT") || p.isKw("REVOKE") || p.isKw("FLUSH") ||
		p.isKw("HANDLER") || p.isKw("SIGNAL") || p.isKw("RESIGNAL") ||
		p.isKw("CHANGE") || p.isKw("START") || p.isKw("STOP") ||
		p.isKw("XA") || p.isKw("PREPARE") || p.isKw("EXECUTE") ||
		p.isKw("DEALLOCATE") || p.isKw("LOCK") || p.isKw("UNLOCK") ||
		p.isKw("ANALYZE") || p.isKw("OPTIMIZE") || p.isKw("REPAIR") ||
		p.isKw("CHECKSUM") || p.isKw("RESET") || p.isKw("PURGE"):
		// Replication / privileges / admin commands — drop with a generic
		// noop so the rest of the dump still parses cleanly.
		return p.parseNoopToStmtEnd(p.cur.Lit)
	}
	p.errorHere("unexpected token at statement start", "statement keyword")
	return nil
}

func (p *Parser) parseTruncate() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("TRUNCATE")
	if p.isKw("TABLE") {
		p.advance()
	}
	ref := p.parseTableRef()
	return &ast.TruncateTable{Table: ref, P: astPos(start)}
}

func (p *Parser) parseRenameTable() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("RENAME")
	p.expectKw("TABLE")
	out := &ast.RenameTable{P: astPos(start)}
	for {
		from := p.parseTableRef()
		p.expectKw("TO")
		to := p.parseTableRef()
		out.Pairs = append(out.Pairs, ast.RenameTablePair{From: from, To: to})
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return out
}

// parseCreate decides between TABLE / INDEX / VIEW / TRIGGER / PROCEDURE /
// FUNCTION / EVENT, handling optional [DEFINER=...][OR REPLACE][ALGORITHM=...]
// prefixes where relevant.
func (p *Parser) parseCreate() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("CREATE")

	var definer string
	orReplace := false
	algorithm := ""
	sqlSec := ""
	temporary := false

	// [OR REPLACE]
	if p.isKw("OR") {
		p.advance()
		p.expectKw("REPLACE")
		orReplace = true
	}
	// [ALGORITHM = ident]
	if p.isKw("ALGORITHM") {
		p.advance()
		p.expectPunct("=")
		if tok, ok := p.eatIdentOrKw(); ok {
			algorithm = tok
		}
	}
	// [DEFINER = user_spec]
	if p.isKw("DEFINER") {
		p.advance()
		p.expectPunct("=")
		definer = p.parseUserSpec()
	}
	// [SQL SECURITY {DEFINER|INVOKER}]
	if p.isKw("SQL") {
		p.advance()
		p.expectKw("SECURITY")
		if p.isKw("DEFINER") || p.isKw("INVOKER") {
			sqlSec = p.cur.Lit
			p.advance()
		}
	}
	// [TEMPORARY]
	if p.isKw("TEMPORARY") {
		p.advance()
		temporary = true
	}

	switch {
	case p.isKw("TABLE"):
		return p.parseCreateTable(start, temporary, orReplace)
	case p.isKw("UNIQUE") || p.isKw("FULLTEXT") || p.isKw("SPATIAL") || p.isKw("INDEX"):
		return p.parseCreateIndex(start)
	case p.isKw("VIEW"):
		return p.parseCreateView(start, orReplace, algorithm, definer, sqlSec)
	case p.isKw("TRIGGER"):
		return p.parseCreateTrigger(start, definer)
	case p.isKw("PROCEDURE"):
		return p.parseCreateProcedure(start, definer)
	case p.isKw("FUNCTION"):
		return p.parseCreateFunction(start, definer)
	case p.isKw("EVENT"):
		return p.parseCreateEvent(start, definer)
	case p.isKw("SEQUENCE"):
		// MariaDB 10.3+ CREATE SEQUENCE.
		return p.parseCreateSequence(start, orReplace, temporary)
	case p.isKw("PACKAGE"):
		// MariaDB 10.3+ Oracle-compat: CREATE PACKAGE [BODY] <name> ... END;
		// Body content needs PL/SQL → PL/pgSQL translation that is out of
		// scope for the schema migration; consume to delimiter so the rest of
		// the dump still parses, and the translator surfaces a manual-review
		// prerequisite.
		return p.parseCreatePackageNoop(start)
	case p.isKw("LOGFILE") || p.isKw("TABLESPACE") || p.isKw("SERVER") ||
		p.isKw("ROLE") || p.isKw("USER") || p.isKw("DATABASE") ||
		p.isKw("SCHEMA"):
		// CREATE LOGFILE GROUP / TABLESPACE / SERVER / ROLE / USER /
		// DATABASE / SCHEMA — none of these need to round-trip; consume
		// to statement end so the rest of the dump parses cleanly.
		return p.parseNoopToStmtEnd("CREATE " + p.cur.Lit)
	}
	p.errorHere("unsupported CREATE variant",
		"TABLE|INDEX|VIEW|TRIGGER|PROCEDURE|FUNCTION|EVENT|SEQUENCE|LOGFILE|TABLESPACE|SERVER|ROLE|USER|DATABASE|SCHEMA")
	return nil
}

func (p *Parser) parseDrop() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("DROP")

	// Skip leading TEMPORARY keyword (DROP TEMPORARY TABLE …).
	if p.isKw("TEMPORARY") {
		p.advance()
	}

	if p.isKw("TABLE") {
		p.advance()
		ifExists := false
		if p.isKw("IF") {
			p.advance()
			p.expectKw("EXISTS")
			ifExists = true
		}
		s := &ast.DropTable{IfExists: ifExists, P: astPos(start)}
		for {
			ref := p.parseTableRef()
			s.Tables = append(s.Tables, ref)
			if !p.isPunct(",") {
				break
			}
			p.advance()
		}
		// optional RESTRICT|CASCADE
		if p.isKw("RESTRICT") || p.isKw("CASCADE") {
			p.advance()
		}
		return s
	}

	switch {
	case p.isKw("INDEX"):
		return p.parseDropIndex(start)
	case p.isKw("VIEW"):
		return p.parseDropList(start, "VIEW")
	case p.isKw("PROCEDURE"):
		return p.parseDropSingle(start, "PROCEDURE")
	case p.isKw("FUNCTION"):
		return p.parseDropSingle(start, "FUNCTION")
	case p.isKw("TRIGGER"):
		return p.parseDropSingle(start, "TRIGGER")
	case p.isKw("EVENT"):
		return p.parseDropSingle(start, "EVENT")
	}
	// other DROPs (DATABASE, TABLESPACE, ROLE, USER, …): keep as noop.
	return p.parseNoopToStmtEnd("DROP")
}

func (p *Parser) parseDropIndex(start Position) *ast.DropObject {
	p.advance() // INDEX
	// Optional intimeAction: ONLINE | OFFLINE
	if p.isKw("ONLINE") || p.isKw("OFFLINE") {
		p.advance()
	}
	name, _ := p.parseIdent()
	out := &ast.DropObject{Kind: "INDEX", Name: name, P: astPos(start)}
	if p.isKw("ON") {
		p.advance()
		out.OnTable = p.parseTableRef()
	}
	// Optional ALGORITHM = ... | LOCK = ... trailing options.
	for p.isKw("ALGORITHM") || p.isKw("LOCK") {
		p.advance()
		p.consumePunct("=")
		if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_KEYWORD {
			p.advance()
		}
	}
	return out
}

// parseDropSingle handles single-target forms: PROCEDURE / FUNCTION /
// TRIGGER / EVENT. Each accepts an optional `IF EXISTS` and a possibly
// schema-qualified name.
func (p *Parser) parseDropSingle(start Position, kind string) *ast.DropObject {
	p.advance() // kind keyword
	out := &ast.DropObject{Kind: kind, P: astPos(start)}
	if p.isKw("IF") {
		p.advance()
		p.expectKw("EXISTS")
		out.IfExists = true
	}
	ref := p.parseTableRef()
	out.Schema = ref.Schema
	out.Name = ref.Name
	return out
}

// parseDropList handles VIEW (and similar list-form drops): may target
// multiple objects, optional `IF EXISTS`, optional `RESTRICT|CASCADE`.
func (p *Parser) parseDropList(start Position, kind string) *ast.DropObject {
	p.advance() // VIEW
	out := &ast.DropObject{Kind: kind, P: astPos(start)}
	if p.isKw("IF") {
		p.advance()
		p.expectKw("EXISTS")
		out.IfExists = true
	}
	for {
		ref := p.parseTableRef()
		out.Names = append(out.Names, ref)
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	if p.isKw("RESTRICT") {
		p.advance()
		out.Restrict = true
	} else if p.isKw("CASCADE") {
		p.advance()
		out.Cascade = true
	}
	return out
}

func (p *Parser) parseAlter() ast.Stmt {
	start := p.cur.Pos
	p.expectKw("ALTER")
	// Non-table ALTER variants — consume to statement end. ALTER USER /
	// ALTER DATABASE / ALTER SERVER / ALTER LOGFILE GROUP / ALTER EVENT etc.
	if !p.isKw("TABLE") {
		return p.parseNoopToStmtEnd("ALTER " + p.cur.Lit)
	}
	p.expectKw("TABLE")
	ref := p.parseTableRef()
	s := &ast.AlterTable{Table: ref, P: astPos(start)}
	for {
		act := p.parseAlterAction()
		if act != nil {
			s.Actions = append(s.Actions, *act)
		}
		if !p.isPunct(",") {
			break
		}
		p.advance()
	}
	return s
}

func (p *Parser) parseAlterAction() *ast.AlterAction {
	switch {
	case p.isKw("ADD"):
		return p.parseAlterAdd()
	case p.isKw("DROP"):
		return p.parseAlterDrop()
	case p.isKw("MODIFY"):
		p.advance()
		if p.isKw("COLUMN") {
			p.advance()
		}
		col := p.parseColumnDef()
		if col == nil {
			return nil
		}
		p.consumeAlterColumnPosition()
		return &ast.AlterAction{Kind: "MODIFY_COLUMN", Column: col}
	case p.isKw("CHANGE"):
		p.advance()
		if p.isKw("COLUMN") {
			p.advance()
		}
		oldName, _ := p.parseIdent()
		col := p.parseColumnDef()
		if col == nil {
			return nil
		}
		p.consumeAlterColumnPosition()
		return &ast.AlterAction{Kind: "CHANGE_COLUMN", OldName: oldName, Column: col}
	case p.isKw("RENAME"):
		return p.parseAlterRename()
	case p.isKw("ALTER"):
		return p.parseAlterColumnSpec()
	case p.isKw("CONVERT"):
		// CONVERT TO {CHARSET|CHARACTER SET} name [COLLATE coll] — no PG analog.
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("DEFAULT") || p.isKw("CHARACTER") || p.isKw("CHARSET") || p.isKw("COLLATE"):
		// DEFAULT? CHARACTER SET = name [COLLATE = coll] table-option-as-alter.
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("ALGORITHM") || p.isKw("LOCK"):
		// ALGORITHM = INSTANT/INPLACE/COPY/DEFAULT, LOCK = NONE/SHARED/EXCLUSIVE/DEFAULT
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("FORCE"):
		p.advance()
		return &ast.AlterAction{Kind: "NOOP", NoopText: "FORCE"}
	case p.isKw("WITH") || p.isKw("WITHOUT"):
		// WITH/WITHOUT VALIDATION
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("DISABLE") || p.isKw("ENABLE"):
		// DISABLE KEYS / ENABLE KEYS
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("DISCARD") || p.isKw("IMPORT"):
		// DISCARD/IMPORT TABLESPACE [PARTITION ...]
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("ORDER"):
		// ORDER BY col_list
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("PARTITION") || p.isKw("REORGANIZE") || p.isKw("COALESCE") ||
		p.isKw("EXCHANGE") || p.isKw("ANALYZE") || p.isKw("OPTIMIZE") ||
		p.isKw("REBUILD") || p.isKw("REPAIR") || p.isKw("REMOVE") ||
		p.isKw("UPGRADE") || p.isKw("TRUNCATE"):
		// alter_partition_specification — too many shapes to model right
		// now; consume verbatim and surface as a NOOP for the translator to
		// flag.
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	}
	p.errorHere("unsupported ALTER action",
		"ADD|DROP|MODIFY|CHANGE|RENAME|ALTER|CONVERT|ALGORITHM|LOCK|FORCE|VALIDATION|DISABLE|ENABLE|DISCARD|IMPORT|ORDER|PARTITION")
	p.syncToDelimiter()
	return nil
}

func (p *Parser) parseAlterAdd() *ast.AlterAction {
	p.advance() // ADD
	switch {
	case p.isKw("COLUMN"):
		p.advance()
		if p.isPunct("(") {
			// ADD COLUMN (col1 def, col2 def, …) — return the first; subsequent
			// columns are dropped on the floor for now (rare in practice).
			p.advance()
			col := p.parseColumnDef()
			for p.isPunct(",") {
				p.advance()
				_ = p.parseColumnDef()
			}
			p.expectPunct(")")
			if col == nil {
				return nil
			}
			return &ast.AlterAction{Kind: "ADD_COLUMN", Column: col}
		}
		col := p.parseColumnDef()
		if col == nil {
			return nil
		}
		p.consumeAlterColumnPosition()
		return &ast.AlterAction{Kind: "ADD_COLUMN", Column: col}
	case p.isKw("CONSTRAINT") || p.isKw("PRIMARY") || p.isKw("UNIQUE") ||
		p.isKw("FOREIGN") || p.isKw("CHECK") || p.isKw("FULLTEXT") || p.isKw("SPATIAL"):
		c := p.parseTableConstraint()
		if c == nil {
			return nil
		}
		return &ast.AlterAction{Kind: "ADD_CONSTRAINT", Constraint: c}
	case p.isKw("INDEX") || p.isKw("KEY"):
		// ADD INDEX/KEY — translates to a post-CREATE index. Captured as a
		// NOOP for now so the parser doesn't choke; emitting the index is a
		// follow-up.
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: "ADD " + text}
	case p.isKw("PARTITION"):
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: "ADD " + text}
	}
	// Bare `ADD col_def` (no COLUMN keyword).
	col := p.parseColumnDef()
	if col == nil {
		return nil
	}
	p.consumeAlterColumnPosition()
	return &ast.AlterAction{Kind: "ADD_COLUMN", Column: col}
}

func (p *Parser) parseAlterDrop() *ast.AlterAction {
	p.advance() // DROP
	switch {
	case p.isKw("PRIMARY"):
		p.advance()
		p.expectKw("KEY")
		return &ast.AlterAction{Kind: "DROP_CONSTRAINT", DropName: ""}
	case p.isKw("FOREIGN"):
		p.advance()
		p.expectKw("KEY")
		name, _ := p.parseIdent()
		// optional dottedId (very rare)
		if p.isPunct(".") {
			p.advance()
			_, _ = p.parseIdent()
		}
		return &ast.AlterAction{Kind: "DROP_CONSTRAINT", DropName: name}
	case p.isKw("CONSTRAINT") || p.isKw("CHECK"):
		p.advance()
		name, _ := p.parseIdent()
		return &ast.AlterAction{Kind: "DROP_CONSTRAINT", DropName: name}
	case p.isKw("INDEX") || p.isKw("KEY"):
		p.advance()
		name, _ := p.parseIdent()
		return &ast.AlterAction{Kind: "NOOP", NoopText: "DROP INDEX " + name}
	case p.isKw("COLUMN"):
		p.advance()
		name, _ := p.parseIdent()
		// optional RESTRICT / CASCADE
		if p.isKw("RESTRICT") || p.isKw("CASCADE") {
			p.advance()
		}
		return &ast.AlterAction{Kind: "DROP_COLUMN", DropName: name}
	case p.isKw("PARTITION"):
		text := p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: "DROP " + text}
	}
	// Bare `DROP col_name` form.
	name, _ := p.parseIdent()
	if p.isKw("RESTRICT") || p.isKw("CASCADE") {
		p.advance()
	}
	return &ast.AlterAction{Kind: "DROP_COLUMN", DropName: name}
}

func (p *Parser) parseAlterRename() *ast.AlterAction {
	p.advance() // RENAME
	switch {
	case p.isKw("COLUMN"):
		p.advance()
		oldName, _ := p.parseIdent()
		p.expectKw("TO")
		newName, _ := p.parseIdent()
		return &ast.AlterAction{Kind: "RENAME_COLUMN", OldName: oldName, NewName: newName}
	case p.isKw("INDEX") || p.isKw("KEY"):
		p.advance()
		oldName, _ := p.parseIdent()
		p.expectKw("TO")
		newName, _ := p.parseIdent()
		return &ast.AlterAction{Kind: "NOOP",
			NoopText: "RENAME INDEX " + oldName + " TO " + newName}
	case p.isKw("TO") || p.isKw("AS"):
		p.advance()
	}
	// RENAME [TO|AS]? new_table_name
	newName, _ := p.parseIdent()
	// optional schema-qualified: schema.name
	if p.isPunct(".") {
		p.advance()
		second, _ := p.parseIdent()
		newName = newName + "." + second
	}
	return &ast.AlterAction{Kind: "RENAME_TABLE", NewName: newName}
}

// parseAlterColumnSpec handles the ALTER COLUMN / ALTER INDEX subforms:
//
//	ALTER COLUMN col SET DEFAULT expr | DROP DEFAULT
//	ALTER COLUMN col SET VISIBLE | SET INVISIBLE
//	ALTER INDEX  ix VISIBLE | INVISIBLE
//	ALTER CHECK / ALTER CONSTRAINT — consumed as NOOP.
func (p *Parser) parseAlterColumnSpec() *ast.AlterAction {
	p.advance() // ALTER
	switch {
	case p.isKw("INDEX"):
		text := "ALTER " + p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	case p.isKw("CHECK") || p.isKw("CONSTRAINT"):
		text := "ALTER " + p.captureCurrentSegment()
		return &ast.AlterAction{Kind: "NOOP", NoopText: text}
	}
	if p.isKw("COLUMN") {
		p.advance()
	}
	colName, _ := p.parseIdent()
	switch {
	case p.isKw("SET"):
		p.advance()
		if p.isKw("DEFAULT") {
			p.advance()
			expr := p.captureAtom()
			return &ast.AlterAction{
				Kind: "SET_DEFAULT", DropName: colName, DefaultExpr: expr,
			}
		}
		if p.isKw("VISIBLE") || p.isKw("INVISIBLE") {
			p.advance()
			return &ast.AlterAction{Kind: "NOOP",
				NoopText: "ALTER COLUMN " + colName + " SET visibility"}
		}
	case p.isKw("DROP"):
		p.advance()
		if p.isKw("DEFAULT") {
			p.advance()
			return &ast.AlterAction{Kind: "DROP_DEFAULT", DropName: colName}
		}
	}
	p.errorHere("unsupported ALTER COLUMN form", "SET DEFAULT|DROP DEFAULT|SET VISIBLE|SET INVISIBLE")
	return nil
}

// consumeAlterColumnPosition swallows the optional `FIRST` or `AFTER col`
// clause that follows ADD/MODIFY/CHANGE COLUMN.
func (p *Parser) consumeAlterColumnPosition() {
	if p.isKw("FIRST") {
		p.advance()
		return
	}
	if p.isKw("AFTER") {
		p.advance()
		_, _ = p.parseIdent()
	}
}

// captureCurrentSegment slurps tokens up to the next top-level `,` or
// statement-end and returns the raw source slice (trimmed). Useful for
// preserving an unsupported alter spec verbatim so the translator can
// surface it.
func (p *Parser) captureCurrentSegment() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	for p.cur.Kind != TOK_EOF {
		if depth == 0 && (p.atStatementEnd() || (p.isPunct(",") && depth == 0)) {
			break
		}
		if p.isPunct("(") {
			depth++
		} else if p.isPunct(")") {
			if depth > 0 {
				depth--
			}
		}
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	return strings.TrimSpace(string(p.src[startOff:endOff]))
}

func (p *Parser) parseUseOrCall(kind string) ast.Stmt {
	start := p.cur.Pos
	var b strings.Builder
	b.WriteString(p.cur.Lit)
	p.advance()
	// slurp until delimiter
	for !p.atStatementEnd() {
		b.WriteString(" ")
		b.WriteString(p.cur.Lit)
		p.advance()
	}
	return &ast.NoopStmt{Kind: kind, Text: b.String(), P: astPos(start)}
}

// parseSet consumes any MySQL SET statement shape: SET NAMES utf8mb4,
// SET sql_mode = 'ONLY_FULL_GROUP_BY,NO_AUTO_VALUE_ON_ZERO,...',
// SET @@session.foo = 'x', SET character_set_client = 'utf8mb4', …
//
// We deliberately don't model these semantically (nothing translates to PG);
// we just accept them robustly so they don't produce parse errors when MySQL
// injects them around SHOW CREATE VIEW output.
func (p *Parser) parseSet() ast.Stmt {
	start := p.cur.Pos
	var b strings.Builder
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		// Track string literals so a stray ';' inside them doesn't end the
		// statement (the lexer already handles that, but being explicit
		// makes the intent clear and defends against lexer tweaks).
		if p.cur.Kind == TOK_STRING {
			b.WriteByte('\'')
			b.WriteString(p.cur.Lit)
			b.WriteByte('\'')
		} else {
			b.WriteString(p.cur.Lit)
		}
		b.WriteByte(' ')
		p.advance()
	}
	return &ast.NoopStmt{Kind: "SET", Text: strings.TrimSpace(b.String()), P: astPos(start)}
}

// parseCreatePackageNoop consumes a MariaDB Oracle-compat CREATE PACKAGE
// [BODY] declaration. The body is captured as raw text in NoopStmt so the
// translator can surface a clear manual-review prerequisite mentioning the
// package name.
func (p *Parser) parseCreatePackageNoop(start Position) ast.Stmt {
	p.expectKw("PACKAGE")
	kind := "CREATE PACKAGE"
	if p.isKw("BODY") {
		p.advance()
		kind = "CREATE PACKAGE BODY"
	}
	if p.isKw("IF") {
		p.advance()
		if p.isKw("NOT") {
			p.advance()
		}
		if p.isKw("EXISTS") {
			p.advance()
		}
	}
	pkgName := ""
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
		pkgName = p.cur.Lit
		p.advance()
		if p.isPunct(".") {
			p.advance()
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
				pkgName = pkgName + "." + p.cur.Lit
				p.advance()
			}
		}
	}
	var b strings.Builder
	b.WriteString(kind)
	if pkgName != "" {
		b.WriteString(" ")
		b.WriteString(pkgName)
	}
	// Inside a package, semicolons are statement separators of the embedded
	// PL/SQL declarations — treat them as content, not as the outer delimiter.
	// The outer delimiter is the active client delimiter (e.g. "//" inside a
	// DELIMITER // block). When the active delimiter is the default ";" we
	// fall back to scanning for the closing `END [name]` and stop after the
	// trailing ";".
	customDelim := p.l.Delimiter() != ";"
	for p.cur.Kind != TOK_EOF {
		if customDelim && p.isStatementDelimiter() {
			break
		}
		if !customDelim && p.isKw("END") {
			b.WriteString(" END")
			p.advance()
			// optional name after END
			if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
				b.WriteString(" ")
				b.WriteString(p.cur.Lit)
				p.advance()
			}
			// Optional trailing ";"
			if p.isPunct(";") {
				p.advance()
			}
			break
		}
		b.WriteString(" ")
		b.WriteString(p.cur.Lit)
		p.advance()
	}
	return &ast.NoopStmt{Kind: kind, Text: b.String(), P: astPos(start)}
}

func (p *Parser) parseNoopToStmtEnd(kind string) ast.Stmt {
	start := p.cur.Pos
	var b strings.Builder
	b.WriteString(kind)
	for !p.atStatementEnd() {
		b.WriteString(" ")
		b.WriteString(p.cur.Lit)
		p.advance()
	}
	return &ast.NoopStmt{Kind: kind, Text: b.String(), P: astPos(start)}
}

// ---------------------------------------------------------------------------
// low-level helpers
// ---------------------------------------------------------------------------

func (p *Parser) advance() {
	// Skip comments transparently.
	for {
		if p.peeked {
			t := p.l.Peek()
			p.peeked = false
			_ = t
		}
		t := p.l.Next()
		if t.Kind == TOK_COMMENT {
			continue
		}
		if t.Kind == TOK_DELIMITER_CMD {
			p.l.SetDelimiter(t.Lit)
			continue
		}
		p.cur = t
		return
	}
}

func (p *Parser) isPunct(lit string) bool {
	return p.cur.Kind == TOK_PUNCT && p.cur.Lit == lit
}

func (p *Parser) isKw(kw string) bool {
	return p.cur.Kind == TOK_KEYWORD && p.cur.Lit == kw
}

func (p *Parser) expectPunct(lit string) bool {
	if p.isPunct(lit) {
		p.advance()
		return true
	}
	p.errorHere("expected '"+lit+"'", "'"+lit+"'")
	return false
}

func (p *Parser) expectKw(kw string) bool {
	if p.isKw(kw) {
		p.advance()
		return true
	}
	p.errorHere("expected keyword "+kw, kw)
	return false
}

func (p *Parser) errorHere(msg, expected string) {
	p.errs = append(p.errs, &ParseError{
		Pos: p.cur.Pos, Msg: msg, Got: p.cur.Lit, Expected: expected,
	})
}

func (p *Parser) eatIdentOrKw() (string, bool) {
	if p.cur.Kind == TOK_IDENT || p.cur.Kind == TOK_QUOTED_IDENT || p.cur.Kind == TOK_KEYWORD {
		v := p.cur.Lit
		p.advance()
		return v, true
	}
	return "", false
}

func (p *Parser) parseIdent() (string, bool) {
	switch p.cur.Kind {
	case TOK_IDENT:
		v := p.cur.Lit
		p.advance()
		return v, false
	case TOK_QUOTED_IDENT:
		v := p.cur.Lit
		p.advance()
		return v, true
	case TOK_KEYWORD:
		// Allow keywords as identifiers in a lax fashion.
		v := p.cur.Raw
		p.advance()
		return v, false
	}
	p.errorHere("expected identifier", "IDENT")
	return "", false
}

// parseTableRef parses [schema.]name with optional backticks.
func (p *Parser) parseTableRef() ast.TableRef {
	name, bt := p.parseIdent()
	if p.isPunct(".") {
		p.advance()
		second, bt2 := p.parseIdent()
		return ast.TableRef{Schema: name, Name: second, NameBacktick: bt || bt2}
	}
	return ast.TableRef{Name: name, NameBacktick: bt}
}

// parseUserSpec parses `user`@`host` | CURRENT_USER | ident and returns it as
// a canonical string. We do not interpret the value semantically.
func (p *Parser) parseUserSpec() string {
	if p.isKw("CURRENT_USER") {
		p.advance()
		// optional () after CURRENT_USER
		if p.isPunct("(") {
			p.advance()
			if p.isPunct(")") {
				p.advance()
			}
		}
		return "CURRENT_USER"
	}
	user, bt := p.parseIdent()
	out := user
	if bt {
		out = "`" + user + "`"
	}
	if p.isPunct("@") {
		p.advance()
		host, bt2 := p.parseIdent()
		if bt2 {
			out += "@`" + host + "`"
		} else {
			out += "@" + host
		}
	}
	return out
}

func (p *Parser) atStatementEnd() bool {
	return p.cur.Kind == TOK_EOF || p.isPunct(";") || p.isStatementDelimiter()
}

// isStatementDelimiter returns true if the current token matches the active
// custom delimiter (e.g. "//" or "$$" after DELIMITER //).
func (p *Parser) isStatementDelimiter() bool {
	d := p.l.Delimiter()
	if d == ";" {
		return p.isPunct(";")
	}
	// Custom delimiter: our lexer tokenizes punctuation one char at a time,
	// so "//" arrives as two TOK_PUNCT "/" in sequence. We check the current
	// one and trust consumeStatementEnd to swallow the rest.
	if p.cur.Kind == TOK_PUNCT {
		return strings.HasPrefix(d, p.cur.Lit)
	}
	return false
}

func (p *Parser) consumeStatementEnd() {
	d := p.l.Delimiter()
	if d == ";" {
		for p.isPunct(";") {
			p.advance()
		}
		return
	}
	// advance while the concatenation of consecutive punct tokens is a prefix of d
	for {
		var acc strings.Builder
		saved := p.cur
		matched := false
		for p.cur.Kind == TOK_PUNCT {
			acc.WriteString(p.cur.Lit)
			if acc.String() == d {
				p.advance()
				matched = true
				break
			}
			if !strings.HasPrefix(d, acc.String()) {
				break
			}
			p.advance()
		}
		if !matched {
			// restore if we consumed a partial prefix that didn't match
			if p.cur.Pos.Offset != saved.Pos.Offset && acc.Len() > 0 {
				// We already advanced; no way to un-advance. Leave it.
			}
			return
		}
	}
}

func (p *Parser) syncToDelimiter() {
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// raw_block — capture the verbatim source of a routine body.
// We use character offsets from the original source so comments/whitespace
// are preserved exactly.
// ---------------------------------------------------------------------------

// captureRawBody reads a BEGIN ... END block (with nesting) or a single
// statement up to the current delimiter, and returns the raw text.
func (p *Parser) captureRawBody() string {
	if p.isKw("BEGIN") {
		return p.captureBeginEnd()
	}
	return p.captureUntilDelimiter()
}

func (p *Parser) captureBeginEnd() string {
	startOff := p.cur.Pos.Offset
	depth := 0
	// We'll walk tokens, tracking BEGIN/END depth. The raw text is the source
	// slice from the opening BEGIN up to and including the matching END.
	for {
		switch {
		case p.isKw("BEGIN") || p.isKw("CASE") || p.isKw("LOOP") ||
			(p.isKw("IF") && !p.isKw("THEN")):
			// MySQL nesting tokens. CASE ends with END CASE; LOOP with END LOOP; IF with END IF.
			depth++
			p.advance()
		case p.isKw("END"):
			depth--
			p.advance()
			// consume trailing qualifier (END IF / END LOOP / END CASE / END REPEAT / END WHILE)
			if p.isKw("IF") || p.isKw("LOOP") || p.isKw("CASE") || p.isKw("REPEAT") || p.isKw("WHILE") {
				p.advance()
			}
			if depth <= 0 {
				endOff := p.cur.Pos.Offset
				return string(p.src[startOff:endOff])
			}
		case p.cur.Kind == TOK_EOF:
			p.errorHere("unterminated BEGIN block", "END")
			return string(p.src[startOff:])
		default:
			p.advance()
		}
	}
}

func (p *Parser) captureUntilDelimiter() string {
	startOff := p.cur.Pos.Offset
	for !p.atStatementEnd() && p.cur.Kind != TOK_EOF {
		p.advance()
	}
	endOff := p.cur.Pos.Offset
	return strings.TrimSpace(string(p.src[startOff:endOff]))
}

// astPos converts our Position to the ast package's one.
func astPos(p Position) ast.Position {
	return ast.Position{Line: p.Line, Col: p.Col, Offset: p.Offset}
}
