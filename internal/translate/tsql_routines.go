package translate

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_routines.go — SQL Server routine bodies (procedures, scalar and
// multi-statement table-valued functions, DML triggers) → PL/pgSQL.
//
// This is the RoutineBodyTranslator registered for dialects.KindSQLServer
// (tsql_routines_register.go). The translator core (translator.go) builds
// the PG header (parameters, RETURNS, trigger timing and transition
// tables) and hands over the raw T-SQL body with its RoutineCtx.
// Everything below works on the typed AST of the T-SQL parser
// (tsqldialect.ParseRoutineBody), never on SQL text:
//
//  1. the SQL Server expression / DML visitors (rewriteTSQLBody,
//     tsql_visitors.go) run first, with the routine context: function
//     renames, identifier folding (`@CustomerID` → variable
//     `customerid`), schema mapping, `+` typed as concatenation from the
//     parameter / variable / column types. They leave alone what only
//     the routine context resolves: variable references stay Variable
//     idents, @@ROWCOUNT / @@FETCH_STATUS / @@TRANCOUNT, ERROR_*(),
//     UPDATE(col), XACT_STATE(), table variables and #temp tables.
//  2. collect: DECLAREd variables, table variables, cursors.
//  3. render, in source order: each T-SQL procedural statement becomes
//     PL/pgSQL nodes (pgast.PL*); the expressions and DML go through the
//     post rewriter (variables, pseudo-tables, temp tables, the context
//     built-ins) and the PG writer.
//
// Variables. T-SQL tells variables from columns by the @ prefix; PG does
// not, and a parameter named like a column (`@ProductID` vs
// `ProductID`, the usual T-SQL style) makes every unqualified reference
// ambiguous. So every variable reference is qualified — parameters with
// the routine name (PL/pgSQL labels a function's outer block with its
// name), locals with the label of the body block (tsqlLocalsLabel) — and
// the body starts with `#variable_conflict use_column`: an unqualified
// name is then always the column, as it was in T-SQL. The PG variable
// names are the T-SQL ones without @, lower-cased (tsqlParamName /
// tsqlVarName), as the routine header and the visitors spell them.

// tsqlLocalsLabel labels the PL/pgSQL block holding the routine's local
// variables: references are written "_tsql"."v". Chosen so it cannot be
// a T-SQL table alias in practice (a qualified name resolves to the
// column first under #variable_conflict use_column).
const tsqlLocalsLabel = "_tsql"

// tsqlSelectIntoLabel is the label the visitors' residual pass gives a
// SELECT … INTO new_table. The routine translator turns SELECT … INTO
// #temp into CREATE TEMP TABLE … AS itself, so it drops that label when
// every SELECT … INTO of the body targets a #temp table.
const tsqlSelectIntoLabel = "SELECT … INTO a new table"

// tsqlVar is a parameter or local variable of the routine.
type tsqlVar struct {
	src    string // source spelling (with its @ for T-SQL variables)
	name   string // PG name
	qual   string // qualifier of every reference (routine name / locals label)
	pgType string
	init   string // DECLARE initialiser (locals the translator adds)
	tvp    bool   // READONLY table-valued parameter (a PG composite array)
}

// ref is a reference to the variable as an expression.
func (v *tsqlVar) ref(p ast.Position) *ast.Ident {
	return &ast.Ident{Parts: []string{v.qual, v.name}, P: p}
}

// target renders the variable as an assignment / INTO / GET DIAGNOSTICS
// target.
func (v *tsqlVar) target() string { return pgast.WriteExpr(v.ref(ast.Position{})) }

// tsqlTableVar is a table variable (`DECLARE @t TABLE (…)`, or the result
// table of a multi-statement table-valued function), emulated by a
// temporary table.
type tsqlTableVar struct {
	src  string
	name string   // PG temp table name
	cols []string // rendered column definitions
}

// tsqlCursor is a DECLAREd cursor, emulated by a refcursor variable
// opened with `OPEN c FOR <query>` where T-SQL opens it (T-SQL evaluates
// the variables of the cursor query at OPEN too).
type tsqlCursor struct {
	// key is the folded T-SQL name (normalizeTSQLIdent), the key of
	// tsqlBody.cursors and tsqlBody.scrollUse; name is the PG local,
	// which differs when another variable already took the folded name.
	key    string
	name   string
	decl   *ast.DeclareCursor
	scroll bool
}

// tsqlBody is the state of one routine-body translation.
type tsqlBody struct {
	ctx      *RoutineCtx
	ectx     *ExprCtx
	warnings []string
	notes    []string
	said     map[string]bool

	vars       map[string]*tsqlVar // PG name (folded, no @) → variable
	locals     []*tsqlVar          // locals in declaration order
	taken      map[string]bool     // PG variable names in use
	tables     map[string]*tsqlTableVar
	tableOrder []*tsqlTableVar
	tempNames  map[string]bool // PG names of the table-variable temp tables
	cursors    map[string]*tsqlCursor
	cursorList []*tsqlCursor
	scrollUse  map[string]bool // folded T-SQL names of the cursors fetched in another direction than NEXT
	savepoints map[string]bool

	resultSets int // result-set SELECTs opened on ctx.ResultCursors so far
	// rowcountDone lists the SET ROWCOUNT statements applySetRowcount
	// translated; rowcountWhy says why the others are not.
	rowcountDone map[*ast.SetRowcountStmt]bool
	rowcountWhy  string
	rowcount     *tsqlVar // _rowcount, when @@ROWCOUNT is read after DML
	fetchStatus  *tsqlVar // _fetch_status, when @@FETCH_STATUS is read
	// updated maps a column tested by UPDATE(col) in a statement-level
	// UPDATE trigger to its _updated_<col> local (see updatedMark).
	updated map[string]*tsqlVar

	// permanentInto is set when a SELECT … INTO targets a permanent
	// table (the visitors' label then stands).
	permanentInto bool
	// xmlDMLGuarded is set when an UPDATE … SET col.modify(…) was
	// turned into its raising guard (xmlModifyGuard), which reports it.
	xmlDMLGuarded bool

	dmlSeen bool // a statement setting @@ROWCOUNT ran before (source order)
	inTry   int
	inCatch int
	// errCtx are the locals holding the error context the enclosing
	// CATCH blocks replaced (innermost last), see catchHandler.
	errCtx []*tsqlVar
	// dynVarTypes maps a folded variable / parameter name to its PG type,
	// for the arguments of inlined dynamic SQL (tsql_dynamic.go).
	dynVarTypes map[string]string
}

// translateTSQLRoutineBody is the SQL Server RoutineBodyTranslator: it
// returns the complete PL/pgSQL body of the routine described by ctx.
func translateTSQLRoutineBody(ctx *RoutineCtx) (string, []string, []string) {
	b := newTSQLBody(ctx)
	stmts, errs := tsqldialect.ParseRoutineBody(ctx.Body)
	if len(errs) > 0 {
		b.warn("the T-SQL parser reported errors in the body, the translation is incomplete: " + errs.Error())
	}
	if len(stmts) == 0 {
		b.warn("the body could not be parsed: the routine is created with a body that raises an error until it is written by hand")
		blk := &pgast.PLBlock{Body: []pgast.PLStmt{&pgast.PLRaise{
			Level: "EXCEPTION", Msg: tsqlRaiseText("routine " + ctx.Schema + "." + ctx.Name + " was not translated from T-SQL"),
		}}}
		if ctx.Trigger != nil {
			blk.Body = append(blk.Body, &pgast.PLReturn{Expr: ctx.Trigger.ReturnRow})
		}
		return pgast.WritePLpgSQL(blk), b.warnings, b.notes
	}
	// Constant dynamic SQL is source text like the body: inlined before
	// the rewrites (tsql_dynamic.go).
	stmts = b.inlineConstantDynamicSQL(stmts)
	b.applySetRowcount(stmts)
	stmts, labels := b.dialectPass(stmts)
	b.collect(stmts)
	text := b.render(stmts)
	for _, l := range labels {
		if l == tsqlSelectIntoLabel && !b.permanentInto {
			continue
		}
		if l == tsqlXMLModifyLabel && b.xmlDMLGuarded {
			continue
		}
		b.warn("T-SQL construct not translated: " + l)
	}
	return text, b.warnings, b.notes
}

func newTSQLBody(ctx *RoutineCtx) *tsqlBody {
	b := &tsqlBody{
		ctx: ctx, ectx: ctx.Expr(), said: map[string]bool{},
		vars: map[string]*tsqlVar{}, taken: map[string]bool{},
		tables: map[string]*tsqlTableVar{}, tempNames: map[string]bool{},
		cursors: map[string]*tsqlCursor{}, scrollUse: map[string]bool{}, savepoints: map[string]bool{},
	}
	for _, c := range ctx.ResultCursors {
		b.taken[c] = true
	}
	for _, p := range ctx.Params {
		// Keyed by the folded source spelling (what references fold to):
		// p.Name may differ (tsqlRenameClashingParams).
		b.vars[tsqlVarName(p.SourceName)] = &tsqlVar{src: p.SourceName, name: p.Name, qual: ctx.Name, pgType: p.Type, tvp: p.Param.ReadOnly}
		b.taken[p.Name] = true
	}
	if ctx.Kind == RoutineMultiTVF && ctx.TableVar != "" {
		tv := &tsqlTableVar{src: ctx.TableVar, name: b.tempTableName(ctx.TableVar)}
		for _, c := range ctx.TableCols {
			tv.cols = append(tv.cols, quoteIdent(c.Name)+" "+c.Type)
		}
		b.addTableVar(tv)
	}
	return b
}

// warn records a construct left untranslated (a blocking review entry);
// note a translation whose PG behaviour differs in a documented way.
func (b *tsqlBody) warn(msg string) {
	if !b.said["w"+msg] {
		b.said["w"+msg] = true
		b.warnings = append(b.warnings, msg)
	}
}

func (b *tsqlBody) note(msg string) {
	if !b.said["n"+msg] {
		b.said["n"+msg] = true
		b.notes = append(b.notes, msg)
	}
}

// translator returns the run's translator (alias types, DDL
// expressions), or a bare one outside a Translate run.
func (b *tsqlBody) translator() *translator {
	if b.ctx.Run != nil && b.ctx.Run.t != nil {
		return b.ctx.Run.t
	}
	return &translator{opt: Options{SourceKind: dialects.KindSQLServer}}
}

func (b *tsqlBody) mapType(dt ast.DataType) string {
	if b.ctx.Run != nil {
		return b.ctx.Run.MapType(dt)
	}
	return MapType(dialects.KindSQLServer, dt, "", Caps{}).PG
}

// dialectPass runs the SQL Server expression / DML visitors over the
// body with the routine context (rewriteTSQLBody). It calls them
// directly rather than through the registered BodyRewriter (the generic
// routine-body path), which reports every label itself: some labels
// name constructs this translator then handles (SELECT … INTO #temp).
// The notes are reported the way the other T-SQL rewrites report them.
func (b *tsqlBody) dialectPass(stmts []ast.PLStmt) ([]ast.PLStmt, []string) {
	out, res := rewriteTSQLBody(stmts, b.ectx)
	reportTSQLRewrite(b.ectx, tsqlRewriteResult{Notes: res.Notes}, "")
	return out, res.Labels
}

// ---------------------------------------------------------------------------
// Declarations
// ---------------------------------------------------------------------------

// tsqlEachStmt calls fn on every statement of stmts, nested ones
// included, in source order.
func tsqlEachStmt(stmts []ast.PLStmt, fn func(ast.PLStmt)) {
	for _, s := range stmts {
		if s == nil {
			continue
		}
		fn(s)
		switch x := s.(type) {
		case *ast.Block:
			tsqlEachStmt(x.Stmts, fn)
			if x.Except != nil {
				for _, h := range x.Except.Handlers {
					tsqlEachStmt(h.Body, fn)
				}
			}
		case *ast.IfStmt:
			for _, br := range x.Branches {
				tsqlEachStmt(br.Body, fn)
			}
			tsqlEachStmt(x.Else, fn)
		case *ast.WhileStmt:
			tsqlEachStmt(x.Body, fn)
		}
	}
}

// tsqlOwnExprs returns the expressions a statement evaluates itself
// (not those of the statements it contains), for the ordered scans.
func tsqlOwnExprs(s ast.PLStmt) []ast.Node {
	switch x := s.(type) {
	case *ast.Block:
		return nil
	case *ast.IfStmt:
		out := make([]ast.Node, 0, len(x.Branches))
		for _, br := range x.Branches {
			if br.Cond != nil {
				out = append(out, br.Cond)
			}
		}
		return out
	case *ast.WhileStmt:
		if x.Cond == nil {
			return nil
		}
		return []ast.Node{x.Cond}
	}
	return []ast.Node{s}
}

// tsqlReadsSysVar reports whether n reads the system variable name.
func tsqlReadsSysVar(n ast.Node, name string) bool {
	found := false
	ast.Rewrite(n, func(m ast.Node) ast.Node {
		if id, ok := m.(*ast.Ident); ok && id.Variable && len(id.Parts) == 1 && strings.EqualFold(id.Parts[0], name) {
			found = true
		}
		return m
	})
	return found
}

// collect registers the declarations of the (rewritten) body — T-SQL
// variables are scoped to the whole batch whatever block declares them —
// and what the body reads of @@ROWCOUNT and @@FETCH_STATUS.
func (b *tsqlBody) collect(stmts []ast.PLStmt) {
	dml := false
	tsqlEachStmt(stmts, func(s ast.PLStmt) {
		switch x := s.(type) {
		case *ast.DeclareStmt:
			for _, d := range x.Decls {
				switch dd := d.(type) {
				case *ast.DeclareVar:
					b.declareVar(dd)
				case *ast.DeclareCursor:
					b.declareCursor(dd)
				}
			}
		case *ast.FetchStmt:
			if x.Direction != "" && x.Direction != "NEXT" {
				b.scrollUse[normalizeTSQLIdent(x.Cursor)] = true
			}
		case *ast.TransactionStmt:
			if x.Kind == "SAVE" && x.Name != "" {
				b.savepoints[normalizeTSQLIdent(x.Name)] = true
			}
		}
		for _, e := range tsqlOwnExprs(s) {
			// A read of @@ROWCOUNT needs the _rowcount local (and GET
			// DIAGNOSTICS after every statement processing rows) unless
			// it is the trigger-top form (see rowcountExpr).
			if b.rowcount == nil && tsqlReadsSysVar(e, "@@ROWCOUNT") && (b.ctx.Trigger == nil || dml) {
				b.rowcount = b.addLocal("_rowcount", "bigint", "0")
			}
			if b.fetchStatus == nil && tsqlReadsSysVar(e, "@@FETCH_STATUS") {
				b.fetchStatus = b.addLocal("_fetch_status", "integer", "-1")
			}
		}
		if tsqlSetsRowcount(s) {
			dml = true
		}
	})
	for _, c := range b.cursorList {
		if b.scrollUse[c.key] {
			c.scroll = true
		}
	}
}

// tsqlSetsRowcount reports whether a statement sets @@ROWCOUNT to the
// number of rows it processed.
func tsqlSetsRowcount(s ast.PLStmt) bool {
	switch s.(type) {
	case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt, *ast.MergeStmt, *ast.SelectStmt,
		*ast.SelectInto, *ast.TruncateTable, *ast.FetchStmt, *ast.CallStmt, *ast.ExecuteImmediateStmt:
		return true
	}
	return false
}

// addLocal declares a local the translation needs (not a source
// variable).
func (b *tsqlBody) addLocal(base, typ, init string) *tsqlVar {
	v := &tsqlVar{name: b.localName(base), qual: tsqlLocalsLabel, pgType: typ, init: init}
	b.locals = append(b.locals, v)
	return v
}

// declareVar registers a DECLAREd variable. The visitors already folded
// its name (`@Total` → `total`).
func (b *tsqlBody) declareVar(dv *ast.DeclareVar) {
	name := tsqlVarName(dv.Name)
	src := "@" + name
	if tt, ok := dv.Type.(*ast.TSQLTableType); ok {
		if b.tables[src] != nil {
			return
		}
		b.addTableVar(&tsqlTableVar{src: src, name: b.tempTableName(src), cols: b.tableColumns(tt.Columns, tt.Constraints)})
		return
	}
	if b.vars[name] != nil {
		return // T-SQL refuses a second declaration of a name
	}
	v := &tsqlVar{src: src, name: b.localName(name), qual: tsqlLocalsLabel}
	if ud, ok := dv.Type.(*ast.UserDefinedType); ok {
		if ref, found := lookupTSQLUserType(b.translator().userTypes, ud.Schema, ud.Name); found && ref.Kind == "composite" {
			b.warn("variable " + src + " is declared with the table type " + ud.Name + ": table-type variables are not emulated (declare a TABLE variable with the same columns)")
		}
	}
	v.pgType = b.mapType(dv.Type)
	if v.pgType == "" {
		v.pgType = "text"
		b.warn("the type of variable " + src + " could not be mapped; it is declared text")
	}
	b.vars[name] = v
	b.locals = append(b.locals, v)
}

func (b *tsqlBody) declareCursor(dc *ast.DeclareCursor) {
	key := normalizeTSQLIdent(dc.Name)
	if b.cursors[key] != nil {
		return
	}
	c := &tsqlCursor{key: key, name: b.localName(key), decl: dc}
	for _, o := range dc.Options {
		switch o {
		case "SCROLL":
			c.scroll = true
		case "DYNAMIC", "KEYSET", "SCROLL_LOCKS", "OPTIMISTIC":
			b.note("cursor " + dc.Name + " option " + o + " dropped: PostgreSQL cursors are insensitive (they do not see the changes made after OPEN)")
		}
	}
	b.cursors[key] = c
	b.cursorList = append(b.cursorList, c)
}

func (b *tsqlBody) addTableVar(tv *tsqlTableVar) {
	b.tables[normalizeTSQLIdent(tv.src)] = tv
	b.tableOrder = append(b.tableOrder, tv)
	b.tempNames[tv.name] = true
}

// localName returns a PG variable name derived from base that no other
// variable of the routine uses.
func (b *tsqlBody) localName(base string) string {
	if base == "" {
		base = "v"
	}
	n := tsqlTruncIdent(base)
	for i := 1; b.taken[n]; i++ {
		n = tsqlTruncIdent(base + "_" + strconv.Itoa(i))
	}
	b.taken[n] = true
	return n
}

// tempTableName names the temporary table of a table variable after the
// routine's schema, the routine and the variable, so two routines' `@t`
// never share a table — same-named routines of two schemas included: a
// callee would TRUNCATE (or, for a TVF, DROP) its caller's rows. A name
// past 63 bytes is cut with a hash of the whole (pgIdentWithSuffix), so
// two long names sharing a prefix stay apart. (A routine calling itself
// still shares its own table, as the session-wide temp table is the
// emulation.)
func (b *tsqlBody) tempTableName(src string) string {
	full := b.ctx.Schema + "_" + normalizeTSQLIdent(b.ctx.Name) + "_" + tsqlParamName(src)
	base := pgIdentWithSuffix(full, "")
	n := base
	for i := 1; b.tempNames[n]; i++ {
		n = tsqlTruncIdent(base + "_" + strconv.Itoa(i))
	}
	return n
}

// tsqlTruncIdent cuts an identifier to PostgreSQL's 63 bytes, on a rune
// boundary — the package's single cut (clipUTF8 / pgMaxIdentLen), the
// one normalizeTSQLIdent applies.
func tsqlTruncIdent(s string) string { return clipUTF8(s, pgMaxIdentLen) }

// tsqlTempName is the PG name of a #temp / ##global temp table name.
func tsqlTempName(name string) (string, bool) {
	i := 0
	for i < len(name) && name[i] == '#' {
		i++
	}
	if i == 0 || i == len(name) {
		return "", false
	}
	return normalizeTSQLIdent(name[i:]), true
}

// tableColumns renders the column list of a temp table emulating a table
// variable or a #temp table.
func (b *tsqlBody) tableColumns(cols []*ast.ColumnDef, cons []ast.TableConstraint) []string {
	var out []string
	for _, c := range cols {
		name := normalizeTSQLIdent(c.Name)
		if c.Type == nil {
			b.warn("computed column " + c.Name + " of a table variable / temp table is not emulated (it is created as a text column)")
			out = append(out, quoteIdent(name)+" text")
			continue
		}
		pgType := b.mapType(c.Type)
		def := quoteIdent(name) + " " + pgType
		if c.AutoInc {
			def += " GENERATED BY DEFAULT AS IDENTITY"
			if c.HasIdentityArgs {
				def += " (START WITH " + strconv.FormatInt(c.IdentitySeed, 10) + " INCREMENT BY " + strconv.FormatInt(c.IdentityIncrement, 10) + ")"
			}
		}
		if c.NotNull || c.PrimaryKey {
			def += " NOT NULL"
		}
		if c.Default != nil {
			def += " DEFAULT " + b.ddlExpr(c.Default, pgType)
		}
		if c.PrimaryKey {
			def += " PRIMARY KEY"
		} else if c.Unique {
			def += " UNIQUE"
		}
		if c.Check != nil {
			def += " CHECK (" + b.ddlExpr(c.Check, "") + ")"
		}
		out = append(out, def)
	}
	for _, tc := range cons {
		switch x := tc.(type) {
		case *ast.PKConstraint:
			out = append(out, "PRIMARY KEY ("+tsqlIndexCols(x.Columns)+")")
		case *ast.UQConstraint:
			out = append(out, "UNIQUE ("+tsqlIndexCols(x.Columns)+")")
		case *ast.CheckConstraint:
			out = append(out, "CHECK ("+b.ddlExpr(x.Expr, "")+")")
		}
	}
	return out
}

func tsqlIndexCols(cols []ast.IndexedCol) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, quoteIdent(normalizeTSQLIdent(c.Name)))
	}
	return strings.Join(parts, ", ")
}

// ddlExpr renders a DEFAULT / CHECK expression of a temp table with the
// SQL Server expression visitors, as table DDL does. pgType is the
// column type for a DEFAULT ("" for a CHECK).
func (b *tsqlBody) ddlExpr(e ast.Expr, pgType string) string {
	if b.ctx.Run != nil && b.ctx.Run.t != nil {
		return b.ctx.Run.t.tsqlDefault(e, pgType == "boolean", b.ectx)
	}
	out, _ := rewriteTSQLExpr(e, b.ectx)
	return pgast.WriteExpr(boolifyTSQL(out, pgType))
}

// ---------------------------------------------------------------------------
// Post rewriter (variables, pseudo-tables, temp tables, built-ins)
// ---------------------------------------------------------------------------

// expr renders an expression after the post rewriter.
func (b *tsqlBody) expr(e ast.Expr) string {
	if e == nil {
		return "NULL"
	}
	if r, ok := ast.Rewrite(e, b.post).(ast.Expr); ok && r != nil {
		e = r
	}
	return pgast.WriteExpr(e)
}

// dml renders a DML statement after the post rewriter.
func (b *tsqlBody) dml(s ast.Stmt) string {
	if r, ok := ast.Rewrite(s, b.post).(ast.Stmt); ok && r != nil {
		s = r
	}
	switch x := s.(type) {
	case *ast.InsertStmt:
		x.Table = b.tableRef(x.Table)
	case *ast.UpdateStmt:
		x.Table = b.tableRef(x.Table)
	case *ast.DeleteStmt:
		x.Table = b.tableRef(x.Table)
	case *ast.MergeStmt:
		x.Target = b.tableRef(x.Target)
	}
	return pgast.WriteStmt(s)
}

// tableRef maps a DML target naming a table variable or a #temp table to
// its temp table.
func (b *tsqlBody) tableRef(ref ast.TableRef) ast.TableRef {
	if ref.Name == "" {
		return ref
	}
	if ref.Name[0] == '@' {
		if tv := b.tables[normalizeTSQLIdent(ref.Name)]; tv != nil {
			return ast.TableRef{Schema: "pg_temp", Name: tv.name}
		}
		if v := b.vars[tsqlVarName(ref.Name)]; v != nil && v.tvp {
			b.warn("table-valued parameter " + v.src + " is READONLY: it cannot be the target of a statement")
		} else {
			b.warn("table variable " + ref.Name + " is not declared in the routine")
		}
		return ref
	}
	if n, ok := tsqlTempName(ref.Name); ok {
		return ast.TableRef{Schema: "pg_temp", Name: n}
	}
	return ref
}

// post is the render-time rewriter (post-order, see ast.Rewrite).
func (b *tsqlBody) post(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.Ident:
		if x.Variable && len(x.Parts) == 1 {
			return b.varRef(x)
		}
	case *ast.FuncCall:
		return b.builtin(x)
	case *ast.FromTable:
		return b.fromTable(x)
	}
	return n
}

// varRef resolves a variable reference (already folded by the visitors).
func (b *tsqlBody) varRef(x *ast.Ident) ast.Node {
	name := x.Parts[0]
	if isTSQLSystemVar(name) {
		switch strings.ToUpper(name) {
		case "@@ROWCOUNT":
			return b.rowcountExpr(x.P)
		case "@@FETCH_STATUS":
			if b.fetchStatus != nil {
				return b.fetchStatus.ref(x.P)
			}
		case "@@TRANCOUNT":
			b.note("@@TRANCOUNT reads 1: a PostgreSQL routine always runs inside a transaction, and transactions do not nest")
			return &ast.Literal{Kind: "number", Text: "1", P: x.P}
		case "@@ERROR":
			// Reported by the visitors (no counterpart): 0 keeps the
			// routine valid, an error raises an exception on PG instead.
			return &ast.Literal{Kind: "number", Text: "0", P: x.P}
		}
		return x
	}
	if v := b.vars[tsqlVarName(name)]; v != nil {
		return v.ref(x.P)
	}
	b.warn("variable @" + tsqlVarName(name) + " is not declared in the routine")
	return x
}

// rowcountExpr translates a read of @@ROWCOUNT. At the top of a trigger
// (no statement ran yet) it is the number of rows the triggering
// statement affected: the size of the transition table of a
// statement-level trigger, 1 for a row-level one. Anywhere else it is
// the _rowcount local, refreshed by GET DIAGNOSTICS after every
// statement processing rows.
func (b *tsqlBody) rowcountExpr(p ast.Position) ast.Expr {
	if trig := b.ctx.Trigger; trig != nil && !b.dmlSeen {
		if trig.ForEach == "ROW" {
			return &ast.Literal{Kind: "number", Text: "1", P: p}
		}
		tbl := "deleted"
		if b.hasEvent("INSERT") || b.hasEvent("UPDATE") {
			tbl = "inserted"
		}
		return &ast.SubqueryExpr{Stmt: &ast.SelectStmt{
			Cols: []ast.SelectItem{{Expr: &ast.FuncCall{Name: "count", Args: []ast.Expr{&ast.Ident{Parts: []string{"*"}, P: p}}, P: p}}},
			From: []ast.FromItem{&ast.FromTable{Name: tbl, P: p}},
			P:    p,
		}, P: p}
	}
	if b.rowcount == nil {
		// collect allocates it whenever such a read exists; kept for
		// safety (the read then sees 0).
		b.rowcount = b.addLocal("_rowcount", "bigint", "0")
	}
	return b.rowcount.ref(p)
}

func (b *tsqlBody) hasEvent(ev string) bool {
	if b.ctx.Trigger == nil {
		return false
	}
	for _, e := range b.ctx.Trigger.Events {
		if e == ev {
			return true
		}
	}
	return false
}

// tsqlTypedNull is `CAST(NULL AS typ)`.
func tsqlTypedNull(typ string, p ast.Position) ast.Expr {
	return &ast.CastExpr{Expr: &ast.Literal{Kind: "null", Text: "NULL", P: p}, Type: &ast.PGType{Name: typ, P: p}, P: p}
}

// builtin translates the built-ins that read the routine context (the
// visitors leave them as parsed: upper-case names).
func (b *tsqlBody) builtin(x *ast.FuncCall) ast.Node {
	if len(x.Args) == 0 {
		switch x.Name {
		case "ERROR_MESSAGE", "ERROR_NUMBER", "ERROR_SEVERITY", "ERROR_STATE", "ERROR_LINE", "ERROR_PROCEDURE":
			return b.errorFunc(x.Name, x.P)
		case "XACT_STATE":
			b.note("XACT_STATE() reads 1 (active, committable transaction): inside a PostgreSQL EXCEPTION handler the failed part is already rolled back and the transaction can go on")
			return &ast.Literal{Kind: "number", Text: "1", P: x.P}
		case "COLUMNS_UPDATED":
			b.warn("COLUMNS_UPDATED() has no counterpart in a PostgreSQL trigger: it reads NULL; compare the inserted / deleted columns (or use UPDATE OF col triggers)")
			return tsqlTypedNull("bytea", x.P)
		}
		return x
	}
	if x.Name == "UPDATE" && len(x.Args) == 1 && b.ctx.Trigger != nil {
		switch {
		case b.hasEvent("UPDATE"):
			return b.columnUpdated(x.Args[0], x.P)
		case b.hasEvent("INSERT"):
			// UPDATE(col) is TRUE for every column on an INSERT.
			return &ast.Literal{Kind: "bool", Text: "TRUE", P: x.P}
		default:
			return &ast.Literal{Kind: "bool", Text: "FALSE", P: x.P}
		}
	}
	return x
}

// columnUpdated translates UPDATE(col) in a trigger handling UPDATE. In
// a statement-level trigger it is exact (updatedMark). Otherwise — a
// row-level (INSTEAD OF) trigger, which PG refuses a column list for,
// or UPDATE(<expression>) — it is approximated:
// T-SQL answers whether the UPDATE's SET list names the column; a PG
// trigger cannot know it, so the test becomes "the column's values
// changed": the new values of the transition table are not the old ones
// (compared as text, which every type has), or NEW.col differs from
// OLD.col in a row-level trigger. TRUE (the guarded code always runs)
// would make `IF NOT UPDATE(col)` dead code. A SET col = <same value>
// still differs, hence the warning.
func (b *tsqlBody) columnUpdated(col ast.Expr, p ast.Position) ast.Expr {
	if id, ok := col.(*ast.Ident); ok && len(id.Parts) == 1 && b.ctx.Trigger.ForEach == "STATEMENT" && tsqlUpdateMarkOrdered(b.ctx.Name) {
		return b.updatedMark(id.Parts[0], p)
	}
	b.warn("UPDATE(column) is not available in PostgreSQL triggers (they do not know which columns the UPDATE statement set): it is approximated by \"the column's values changed\", which differs when the UPDATE sets a column to its current value; split the trigger with UPDATE OF col where that matters")
	id, ok := col.(*ast.Ident)
	if !ok || len(id.Parts) != 1 {
		b.warn("UPDATE(<expression>) not translated: it reads TRUE")
		return &ast.Literal{Kind: "bool", Text: "TRUE", P: p}
	}
	name := id.Parts[0]
	asText := func(e ast.Expr) ast.Expr { return &ast.CastExpr{Expr: e, Type: &ast.PGType{Name: "text"}, P: p} }
	if b.ctx.Trigger.ForEach == "ROW" {
		changed := &ast.BinaryExpr{
			Op:  "IS DISTINCT FROM",
			Lhs: asText(&ast.Ident{Parts: []string{"NEW", name}, P: p}),
			Rhs: asText(&ast.Ident{Parts: []string{"OLD", name}, P: p}),
			P:   p,
		}
		if !b.hasEvent("INSERT") && !b.hasEvent("DELETE") {
			return &ast.ParenExpr{Inner: changed, P: p}
		}
		// One row-level trigger for several events: OLD / NEW exist for
		// an UPDATE only, and UPDATE(col) is TRUE on an INSERT.
		tgOp := func(op string) ast.Expr {
			return &ast.BinaryExpr{Op: "=", Lhs: &ast.Ident{Parts: []string{"TG_OP"}, P: p}, Rhs: &ast.Literal{Kind: "string", Text: op, P: p}, P: p}
		}
		return &ast.CaseExpr{Whens: []ast.CaseExprWhen{{Match: tgOp("UPDATE"), Then: changed}}, Else: tgOp("INSERT"), P: p}
	}
	side := func(tbl string) *ast.SelectStmt {
		return &ast.SelectStmt{
			Cols: []ast.SelectItem{{Expr: asText(&ast.Ident{Parts: []string{name}, P: p})}},
			From: []ast.FromItem{&ast.FromTable{Name: tbl, P: p}},
			P:    p,
		}
	}
	q := side("inserted")
	q.SetOps = []ast.SetOp{{Op: "EXCEPT ALL", Stmt: side("deleted")}}
	return &ast.ExistsExpr{Subquery: q, P: p}
}

// updatedMark translates UPDATE(col) in a statement-level UPDATE
// trigger exactly: the column is recorded on the trigger
// (TriggerInfo.UpdatedColumns), for which the translator adds a helper
// `AFTER UPDATE OF col` trigger marking the statement
// (tsql.mark_column_updated, see tsql_shim/60_column_updated.sql), and
// UPDATE(col) reads a boolean local initialised from that mark when the
// body starts — before any early RETURN, so the mark never outlives the
// statement. PG fires an UPDATE OF col trigger exactly when the SET list
// names col, T-SQL's rule for UPDATE(col).
func (b *tsqlBody) updatedMark(col string, p ast.Position) ast.Expr {
	if v := b.updated[col]; v != nil {
		return v.ref(p)
	}
	if b.updated == nil {
		b.updated = map[string]*tsqlVar{}
	}
	init := pgast.WriteExpr(&ast.FuncCall{Name: "tsql.take_column_updated", Args: []ast.Expr{
		&ast.Ident{Parts: []string{"TG_RELID"}},
		&ast.Ident{Parts: []string{"TG_NAME"}},
		&ast.Literal{Kind: "string", Text: col},
	}})
	v := b.addLocal("_updated_"+col, "boolean", init)
	b.updated[col] = v
	b.ctx.Trigger.UpdatedColumns = append(b.ctx.Trigger.UpdatedColumns, col)
	if b.ctx.Run != nil {
		b.ctx.Run.Use("tsql.take_column_updated")
	}
	return v.ref(p)
}

// tsqlUpdateMarkOrdered reports whether the helper triggers of
// updatedMark, named "0_<trigger>…" (tsqlUpdateMarkTrigger), sort before
// the translated trigger itself: PG fires them in name order, and the
// mark must be set before the body reads it.
func tsqlUpdateMarkOrdered(trigger string) bool {
	return trigger != "" && trigger[0] > '0'
}

// tsqlUpdateMarkTrigger names the helper trigger marking the i-th
// (0-based) column a trigger tests with UPDATE(col).
func tsqlUpdateMarkTrigger(trigger string, i int) string {
	return pgIdentWithSuffix("0_"+trigger, "_upd_"+strconv.Itoa(i+1))
}

// errorFunc translates ERROR_MESSAGE() & co. Inside a CATCH block they
// read the error PL/pgSQL exposes in the EXCEPTION handler; outside one
// (a routine called from its caller's CATCH, like AdventureWorks'
// uspLogError) they read the error context the caller's translated CATCH
// block published (tsql_error_context.go).
func (b *tsqlBody) errorFunc(name string, p ast.Position) ast.Expr {
	if b.inCatch == 0 {
		return b.errorContextFunc(name, p)
	}
	switch name {
	case "ERROR_MESSAGE":
		return &ast.Ident{Parts: []string{"sqlerrm"}, P: p}
	case "ERROR_NUMBER":
		b.note(tsqlErrorNumberNote)
		b.use("tsql.error_number_of")
		return &ast.FuncCall{Name: "tsql.error_number_of", Args: []ast.Expr{&ast.Ident{Parts: []string{"sqlstate"}, P: p}}, P: p}
	case "ERROR_SEVERITY":
		b.note("ERROR_SEVERITY() reads 16 (PostgreSQL errors have no severity; every error reaching an EXCEPTION handler is an error)")
		return &ast.Literal{Kind: "number", Text: "16", P: p}
	case "ERROR_STATE":
		b.note("ERROR_STATE() reads 1 (PostgreSQL errors have no state number)")
		return &ast.Literal{Kind: "number", Text: "1", P: p}
	case "ERROR_LINE":
		b.note(tsqlErrorLineNote)
		return &ast.Literal{Kind: "number", Text: "0", P: p}
	}
	b.note("ERROR_PROCEDURE() reads NULL (GET STACKED DIAGNOSTICS … PG_EXCEPTION_CONTEXT holds the routine names instead)")
	return tsqlTypedNull("text", p)
}

// fromTable maps the FROM sources only the routine knows: table
// variables, table-valued parameters, #temp tables and the trigger
// pseudo-tables.
func (b *tsqlBody) fromTable(x *ast.FromTable) ast.Node {
	if x.Name == "" {
		return x
	}
	if x.Name[0] == '@' {
		if tv := b.tables[normalizeTSQLIdent(x.Name)]; tv != nil {
			return &ast.FromTable{Schema: "pg_temp", Name: tv.name, Alias: x.Alias, P: x.P}
		}
		if v := b.vars[tsqlVarName(x.Name)]; v != nil && v.tvp {
			alias := x.Alias
			if alias == "" {
				alias = v.name
			}
			return &ast.FromFunc{
				Func:  &ast.FuncCall{Name: "unnest", Args: []ast.Expr{v.ref(x.P)}, P: x.P},
				Alias: alias, P: x.P,
			}
		}
		b.warn("table variable " + x.Name + " is not declared in the routine")
		return x
	}
	if n, ok := tsqlTempName(x.Name); ok {
		return &ast.FromTable{Schema: "pg_temp", Name: n, Alias: x.Alias, P: x.P}
	}
	if r := b.pseudoTable(x); r != nil {
		return r
	}
	return x
}

// pseudoTable maps a FROM reference to the trigger pseudo-tables
// inserted / deleted: the transition tables of a statement-level
// trigger, a one-row relation over NEW / OLD in a row-level one, and an
// empty relation of the trigger table's shape when the event has none
// (an INSERT has no deleted rows). nil when x is not one.
func (b *tsqlBody) pseudoTable(x *ast.FromTable) ast.FromItem {
	trig := b.ctx.Trigger
	if trig == nil || x.Schema != "" {
		return nil
	}
	n := normalizeTSQLIdent(x.Name)
	if n != "inserted" && n != "deleted" {
		return nil
	}
	alias := x.Alias
	if alias == "" {
		alias = n
	}
	if trig.ForEach == "STATEMENT" {
		if (n == "inserted" && trig.NewTable != "") || (n == "deleted" && trig.OldTable != "") {
			return &ast.FromTable{Name: n, Alias: x.Alias, P: x.P}
		}
		return b.emptyRelation(alias, x.P)
	}
	row := "NEW"
	has := b.hasEvent("INSERT") || b.hasEvent("UPDATE")
	if n == "deleted" {
		row = "OLD"
		has = b.hasEvent("UPDATE") || b.hasEvent("DELETE")
	}
	if !has {
		return b.emptyRelation(alias, x.P)
	}
	b.note("the inserted / deleted pseudo-tables of a row-level trigger are the single NEW / OLD row: (SELECT NEW.*) AS inserted")
	return &ast.FromSubquery{
		Stmt:  &ast.SelectStmt{Cols: []ast.SelectItem{{Expr: &ast.Ident{Parts: []string{row, "*"}, P: x.P}}}, P: x.P},
		Alias: alias, P: x.P,
	}
}

// emptyRelation is `(SELECT * FROM <trigger relation> WHERE false) AS
// alias`: the pseudo-table an event does not have, with the right
// columns.
func (b *tsqlBody) emptyRelation(alias string, p ast.Position) ast.FromItem {
	trig := b.ctx.Trigger
	return &ast.FromSubquery{
		Stmt: &ast.SelectStmt{
			Cols:  []ast.SelectItem{{Star: true}},
			From:  []ast.FromItem{&ast.FromTable{Schema: trig.Schema, Name: trig.Table, P: p}},
			Where: &ast.Literal{Kind: "bool", Text: "false", P: p},
			P:     p,
		},
		Alias: alias, P: p,
	}
}

// ---------------------------------------------------------------------------
// Render
// ---------------------------------------------------------------------------

func (b *tsqlBody) render(stmts []ast.PLStmt) string {
	var body []pgast.PLStmt
	body = append(body, b.insteadEnter()...)
	for _, tv := range b.tableOrder {
		if len(tv.cols) == 0 {
			b.warn("table variable " + tv.src + " has no column the translation could read")
			continue
		}
		// IF NOT EXISTS + TRUNCATE: a second call in the same transaction
		// reuses the (ON COMMIT DROP) table, emptied like a fresh T-SQL
		// table variable.
		body = append(body,
			tsqlRaw("CREATE TEMP TABLE IF NOT EXISTS "+quoteIdent(tv.name)+" ("+strings.Join(tv.cols, ", ")+") ON COMMIT DROP"),
			tsqlRaw("TRUNCATE TABLE "+tsqlTempRef(tv.name)))
	}
	body = append(body, b.stmts(stmts)...)
	switch {
	case b.ctx.Trigger != nil:
		if _, ok := tsqlLastPL(body).(*pgast.PLReturn); !ok {
			body = append(body, b.insteadLeave()...)
			body = append(body, &pgast.PLReturn{Expr: b.ctx.Trigger.ReturnRow})
		}
	case b.ctx.Kind == RoutineMultiTVF:
		if _, ok := tsqlLastStmt(stmts).(*ast.ReturnStmt); !ok {
			body = append(body, b.tvfReturn()...)
		}
	}
	blk := &pgast.PLBlock{Body: tsqlNonEmpty(body)}
	for _, v := range b.locals {
		decl := quoteIdent(v.name) + " " + v.pgType
		if v.init != "" {
			decl += " := " + v.init
		}
		blk.Decls = append(blk.Decls, pgast.PLBlockDecl{Text: decl + ";"})
	}
	for _, c := range b.cursorList {
		blk.Decls = append(blk.Decls, pgast.PLBlockDecl{Text: quoteIdent(c.name) + " refcursor;"})
	}
	if len(blk.Decls) > 0 {
		blk.Label = tsqlLocalsLabel
	}
	text := pgast.WritePLpgSQL(blk)
	if len(b.ctx.Params) > 0 || len(b.locals) > 0 || b.ctx.Kind == RoutineMultiTVF {
		text = "#variable_conflict use_column\n" + text
	}
	return text
}

func tsqlRaw(text string) pgast.PLStmt { return &pgast.PLRawSQL{Text: text} }

func tsqlTempRef(name string) string { return quoteIdent("pg_temp") + "." + quoteIdent(name) }

func tsqlNonEmpty(in []pgast.PLStmt) []pgast.PLStmt {
	if len(in) == 0 {
		return []pgast.PLStmt{tsqlRaw("NULL")}
	}
	return in
}

func tsqlLastStmt(in []ast.PLStmt) ast.PLStmt {
	if len(in) == 0 {
		return nil
	}
	if blk, ok := in[len(in)-1].(*ast.Block); ok && !blk.TryCatch {
		return tsqlLastStmt(blk.Stmts)
	}
	return in[len(in)-1]
}

func tsqlLastPL(in []pgast.PLStmt) pgast.PLStmt {
	if len(in) == 0 {
		return nil
	}
	return in[len(in)-1]
}

func (b *tsqlBody) stmts(in []ast.PLStmt) []pgast.PLStmt {
	var out []pgast.PLStmt
	for i, s := range in {
		if s == nil {
			continue
		}
		var next ast.PLStmt
		if i+1 < len(in) {
			next = in[i+1]
		}
		out = append(out, b.stmt(s, next)...)
		if tsqlSetsRowcount(s) {
			b.dmlSeen = true
		}
	}
	return out
}

// stmt renders one T-SQL statement; next is the statement following it
// in the same list (nil at the end).
func (b *tsqlBody) stmt(n ast.PLStmt, next ast.PLStmt) []pgast.PLStmt {
	switch x := n.(type) {
	case *ast.Block:
		if !x.TryCatch {
			// T-SQL BEGIN … END only groups statements (no scope).
			return b.stmts(x.Stmts)
		}
		if x.Except != nil && b.ctx.Trigger == nil && tsqlTryPartialRollback(x.Stmts) {
			b.warn("BEGIN TRY runs several data modifications outside an explicit transaction: when a later statement fails, SQL Server keeps the earlier ones (each statement commits on its own), while the PL/pgSQL EXCEPTION block rolls back everything the TRY did. Wrap the TRY in BEGIN TRAN … COMMIT on the source semantics you want, or split it")
		}
		b.inTry++
		try := b.stmts(x.Stmts)
		b.inTry--
		blk := &pgast.PLBlock{Body: tsqlNonEmpty(try)}
		if x.Except != nil {
			b.inCatch++
			for _, h := range x.Except.Handlers {
				blk.Handlers = append(blk.Handlers, pgast.PLHandler{Conditions: []string{"OTHERS"}, Body: tsqlNonEmpty(b.catchHandler(h.Body))})
			}
			b.inCatch--
		}
		return []pgast.PLStmt{blk}
	case *ast.DeclareStmt:
		// The declarations are hoisted (collect); an initialiser is an
		// assignment where T-SQL declares.
		var out []pgast.PLStmt
		for _, d := range x.Decls {
			dv, ok := d.(*ast.DeclareVar)
			if !ok || dv.Default == nil {
				continue
			}
			if v := b.vars[tsqlVarName(dv.Name)]; v != nil {
				out = append(out, &pgast.PLAssign{Target: v.target(), Expr: b.expr(dv.Default)})
			}
		}
		return out
	case *ast.AssignStmt:
		v := b.vars[tsqlVarName(x.Target)]
		if v == nil || v.tvp {
			b.warn("assignment to @" + tsqlVarName(x.Target) + ", which is not a scalar variable of the routine")
			return nil
		}
		if _, ok := x.Expr.(*ast.MethodCall); ok {
			b.warn("SET @" + v.name + ".method(…) (xml modify() / CLR mutator) has no PostgreSQL counterpart")
			return nil
		}
		return []pgast.PLStmt{&pgast.PLAssign{Target: v.target(), Expr: b.expr(x.Expr)}}
	case *ast.SelectInto:
		return b.selectInto(x)
	case *ast.SelectStmt:
		return b.selectStmt(x)
	case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt, *ast.MergeStmt:
		return b.dmlStmt(n)
	case *ast.TruncateTable:
		ref := b.tableRef(x.Table)
		return b.withRowcount(tsqlRaw("TRUNCATE TABLE " + pgast.WriteExpr(tsqlRefIdent(ref))))
	case *ast.IfStmt:
		out := &pgast.PLIf{}
		for _, br := range x.Branches {
			out.Branches = append(out.Branches, pgast.PLIfBranch{Cond: b.expr(br.Cond), Body: tsqlNonEmpty(b.stmts(br.Body))})
		}
		out.Else = b.stmts(x.Else)
		return []pgast.PLStmt{out}
	case *ast.WhileStmt:
		return []pgast.PLStmt{&pgast.PLWhile{Label: x.Label, Cond: b.expr(x.Cond), Body: tsqlNonEmpty(b.stmts(x.Body))}}
	case *ast.LeaveStmt:
		return []pgast.PLStmt{&pgast.PLExit{Label: x.Label}}
	case *ast.IterateStmt:
		return []pgast.PLStmt{&pgast.PLContinue{Label: x.Label}}
	case *ast.ReturnStmt:
		return b.returnStmt(x)
	case *ast.GotoStmt, *ast.LabelStmt:
		b.warn("GOTO and labels have no PL/pgSQL counterpart: restructure the flow with IF / loops / EXIT")
		return nil
	case *ast.RaiseStmt:
		return b.raise(x, next)
	case *ast.CallStmt:
		return b.call(x)
	case *ast.ExecuteImmediateStmt:
		return b.execute(x)
	case *ast.TransactionStmt:
		return b.transaction(x)
	case *ast.OpenStmt:
		return b.open(x)
	case *ast.FetchStmt:
		return b.fetch(x)
	case *ast.CloseStmt:
		if x.Deallocate {
			// DEALLOCATE follows CLOSE; PG forgets a closed cursor.
			return nil
		}
		return []pgast.PLStmt{tsqlRaw("CLOSE " + quoteIdent(b.cursorName(x.Cursor)))}
	case *ast.SetRowcountStmt:
		if !b.rowcountDone[x] {
			b.warn("SET ROWCOUNT not translated: " + b.rowcountWhy)
		}
		return nil
	case *ast.NullStmt:
		if x.Warn {
			b.warn("statement dropped (no PostgreSQL counterpart): " + head(x.Source))
		}
		return nil
	case *ast.EmbeddedDDL:
		return b.embeddedDDL(x)
	}
	b.warn("T-SQL statement not translated: " + tsqlNodeLabel(n))
	return nil
}

// tsqlNodeLabel names a statement node for a diagnostic.
func tsqlNodeLabel(n ast.Node) string {
	switch n.(type) {
	case *ast.CaseStmt:
		return "CASE statement"
	case *ast.LoopStmt, *ast.RepeatStmt:
		return "loop"
	case *ast.RawSQL:
		return "unparsed statement"
	}
	return "statement"
}

func tsqlRefIdent(ref ast.TableRef) *ast.Ident {
	if ref.Schema == "" {
		return &ast.Ident{Parts: []string{ref.Name}}
	}
	return &ast.Ident{Parts: []string{ref.Schema, ref.Name}}
}

// withRowcount appends `GET DIAGNOSTICS _rowcount = ROW_COUNT` after a
// statement processing rows, when the body reads @@ROWCOUNT.
func (b *tsqlBody) withRowcount(stmts ...pgast.PLStmt) []pgast.PLStmt {
	if b.rowcount != nil && len(stmts) > 0 {
		stmts = append(stmts, &pgast.PLGetDiagnostics{Target: b.rowcount.target()})
	}
	return stmts
}

func (b *tsqlBody) selectInto(x *ast.SelectInto) []pgast.PLStmt {
	sel := x.Stmt
	if sel == nil {
		b.warn("SELECT @variable = … could not be parsed: " + head(x.RawQuery))
		return nil
	}
	vars := make([]*tsqlVar, len(x.Vars))
	for i, name := range x.Vars {
		vars[i] = b.vars[tsqlVarName(name)]
		if vars[i] == nil {
			b.warn("SELECT assigns @" + tsqlVarName(name) + ", which is not a variable of the routine")
			return nil
		}
	}
	if len(sel.From) == 0 && sel.Where == nil && len(sel.GroupBy) == 0 && sel.Having == nil &&
		len(sel.SetOps) == 0 && sel.With == nil && sel.Top == nil && sel.Limit == nil && len(vars) == len(sel.Cols) {
		// SELECT @a = 1, @b = @a + 1 without FROM: T-SQL assigns left to
		// right, which successive assignments reproduce.
		out := make([]pgast.PLStmt, 0, len(vars))
		for i, v := range vars {
			out = append(out, &pgast.PLAssign{Target: v.target(), Expr: b.expr(sel.Cols[i].Expr)})
		}
		return b.withRowcount(out...)
	}
	if r, ok := ast.Rewrite(sel, b.post).(*ast.SelectStmt); ok && r != nil {
		sel = r
	}
	if len(sel.From) > 0 {
		for i, v := range vars {
			if i < len(sel.Cols) && tsqlRefersTo(sel.Cols[i].Expr, v) {
				b.warn("SELECT " + v.src + " = <expression using " + v.src + "> FROM … accumulates over every row in T-SQL; PostgreSQL SELECT … INTO keeps the first row only: rewrite it with an aggregate (string_agg, sum, …)")
			}
		}
	}
	targets := make([]string, len(vars))
	for i, v := range vars {
		targets[i] = v.target()
	}
	return b.withRowcount(tsqlRaw(pgast.WriteSelectInto(sel, targets)))
}

// tsqlRefersTo reports whether e reads variable v (a rewritten,
// qualified reference).
func tsqlRefersTo(e ast.Expr, v *tsqlVar) bool {
	found := false
	ast.Rewrite(e, func(n ast.Node) ast.Node {
		if id, ok := n.(*ast.Ident); ok && len(id.Parts) == 2 && id.Parts[0] == v.qual && id.Parts[1] == v.name {
			found = true
		}
		return n
	})
	return found
}

func (b *tsqlBody) selectStmt(x *ast.SelectStmt) []pgast.PLStmt {
	if x.IntoTable != nil {
		ref := *x.IntoTable
		cp := *x
		cp.IntoTable = nil
		text := b.dml(&cp)
		if n, ok := tsqlTempName(ref.Name); ok {
			// T-SQL creates a fresh #temp table; PG keeps temp tables for
			// the session, so a previous call's table is dropped first.
			return b.withRowcount(
				tsqlRaw("DROP TABLE IF EXISTS "+tsqlTempRef(n)),
				tsqlRaw("CREATE TEMP TABLE "+quoteIdent(n)+" AS "+text))
		}
		b.permanentInto = true
		// A permanent table (the visitors resolved its name): CREATE TABLE … AS,
		// which fails when it exists — the visitors' label stands.
		return b.withRowcount(tsqlRaw("CREATE TABLE " + pgast.WriteExpr(tsqlRefIdent(ref)) + " AS " + text))
	}
	text := b.dml(x)
	if b.ctx.Kind == RoutineProcedure && b.resultSets < len(b.ctx.ResultCursors) {
		// The result set is returned through the next INOUT refcursor
		// parameter: OPEN on a NULL refcursor names a new portal and
		// stores its name in the parameter, which the caller receives.
		c := b.ctx.ResultCursors[b.resultSets]
		b.resultSets++
		if b.rowcount != nil {
			b.warn("@@ROWCOUNT after a result-set SELECT returned through a refcursor reads 0: the caller fetches the rows later")
		}
		return b.withRowcount(tsqlRaw("OPEN " + quoteIdent(c) + " FOR " + text))
	}
	if b.ctx.Kind == RoutineProcedure {
		b.note("a SELECT returning rows to the caller is run with PERFORM and its rows are discarded (a PostgreSQL procedure returns no result set)")
	} else {
		b.note("a SELECT returning rows to the client is run with PERFORM and its rows are discarded")
	}
	return b.withRowcount(&pgast.PLPerform{Expr: "* FROM (" + text + ") AS " + quoteIdent("_rs")})
}

func (b *tsqlBody) dmlStmt(n ast.PLStmt) []pgast.PLStmt {
	switch x := n.(type) {
	case *ast.InsertStmt:
		if x.Exec != nil {
			b.warn("INSERT … EXEC (the result set of a procedure or of dynamic SQL) has no PostgreSQL counterpart: turn the procedure into a set-returning function and INSERT … SELECT from it")
			return nil
		}
	case *ast.UpdateStmt:
		for _, a := range x.Sets {
			if a.Col != "" && a.Col[0] == '@' {
				b.warn("UPDATE … SET " + a.Col + " = … (assigning a variable inside an UPDATE) has no PostgreSQL counterpart: use UPDATE … RETURNING … INTO")
				return nil
			}
		}
		if g := b.xmlModifyGuard(x); g != nil {
			return g
		}
	}
	stmt, _ := n.(ast.Stmt)
	return b.withRowcount(tsqlRaw(b.dml(stmt)))
}

func (b *tsqlBody) returnStmt(x *ast.ReturnStmt) []pgast.PLStmt {
	switch {
	case b.ctx.Trigger != nil:
		return append(append(b.leaveErrorContext(), b.insteadLeave()...), &pgast.PLReturn{Expr: b.ctx.Trigger.ReturnRow})
	case b.ctx.Kind == RoutineMultiTVF:
		return b.tvfReturn()
	case b.ctx.Kind == RoutineScalarFunction:
		if x.Expr == nil {
			return []pgast.PLStmt{&pgast.PLReturn{Expr: "NULL"}}
		}
		return []pgast.PLStmt{&pgast.PLReturn{Expr: b.expr(x.Expr)}}
	}
	if x.Expr != nil {
		b.note("RETURN <status> of a procedure: the return status is dropped (a PostgreSQL procedure returns no value; pass it through an INOUT parameter)")
	}
	return append(b.leaveErrorContext(), &pgast.PLReturn{})
}

// tvfReturn ends a multi-statement table-valued function: the rows of the
// result table variable are returned and its temp table dropped.
func (b *tsqlBody) tvfReturn() []pgast.PLStmt {
	tv := b.tables[normalizeTSQLIdent(b.ctx.TableVar)]
	if tv == nil {
		return []pgast.PLStmt{&pgast.PLReturn{}}
	}
	ref := tsqlTempRef(tv.name)
	return []pgast.PLStmt{
		tsqlRaw("RETURN QUERY SELECT * FROM " + ref),
		tsqlRaw("DROP TABLE " + ref),
		&pgast.PLReturn{},
	}
}

// raise translates PRINT, THROW and RAISERROR.
func (b *tsqlBody) raise(x *ast.RaiseStmt, next ast.PLStmt) []pgast.PLStmt {
	switch x.Kind {
	case "PRINT":
		return []pgast.PLStmt{tsqlRaw("RAISE NOTICE '%', " + b.expr(x.Message))}
	case "THROW":
		if x.Message == nil && x.ErrNumber == nil {
			if b.inCatch == 0 {
				b.warn("THROW without arguments outside a CATCH block")
			}
			return []pgast.PLStmt{tsqlRaw("RAISE")}
		}
		b.note("THROW error_number, message, state: the error number and state are not carried (RAISE EXCEPTION raises SQLSTATE P0001; a caller's ERROR_NUMBER() reads that SQLSTATE)")
		return []pgast.PLStmt{tsqlRaw("RAISE EXCEPTION USING MESSAGE = " + b.expr(x.Message))}
	case "RAISERROR":
		level := "EXCEPTION"
		if x.Level == "NOTICE" {
			level = "NOTICE"
		} else if b.inTry == 0 {
			if _, ret := next.(*ast.ReturnStmt); !ret && next != nil {
				b.note("RAISERROR with severity 11-19 outside TRY lets T-SQL go on with the next statement; RAISE EXCEPTION ends the routine")
			}
		}
		return []pgast.PLStmt{tsqlRaw("RAISE " + level + " USING MESSAGE = " + b.raiserrorMessage(x))}
	}
	b.warn("RAISE statement not translated")
	return nil
}

// raiserrorMessage renders the message of RAISERROR: a literal message
// with substitution arguments becomes format(), its printf-style
// placeholders converted (tsqlFormatPlaceholders).
func (b *tsqlBody) raiserrorMessage(x *ast.RaiseStmt) string {
	if x.Message == nil {
		b.warn("RAISERROR(msg_id, …) reads its text from sys.messages, which has no PostgreSQL counterpart: the message is the error number")
		return b.expr(&ast.BinaryExpr{Op: "||", Lhs: &ast.Literal{Kind: "string", Text: "error "}, Rhs: x.ErrNumber})
	}
	lit, ok := x.Message.(*ast.Literal)
	if !ok || lit.Kind != "string" {
		if len(x.Args) > 0 {
			b.warn("RAISERROR with a non-literal message and substitution arguments: its printf-style placeholders cannot be converted to format() (the arguments are dropped)")
		}
		return b.expr(x.Message)
	}
	text, lossy := tsqlFormatPlaceholders(lit.Text, len(x.Args) > 0)
	if lossy {
		b.note("RAISERROR placeholder flags / precision / hexadecimal / octal formats have no format() counterpart: the arguments are printed with %s")
	}
	msg := &ast.Literal{Kind: "string", Text: text, NString: lit.NString, P: lit.P}
	if len(x.Args) == 0 {
		return b.expr(msg)
	}
	return b.expr(&ast.FuncCall{Name: "format", Args: append([]ast.Expr{msg}, x.Args...), P: lit.P})
}

// tsqlFormatPlaceholders converts the printf-style placeholders of one
// RAISERROR message literal — %[flags][width][.precision][h|l]type — to
// the %[-][width]s of PostgreSQL's format(), walking that single string
// literal byte by byte (placeholders are ASCII; UTF-8 continuation bytes
// never match). Without substitution arguments only %% is unescaped:
// the message is then used as it is. lossy reports a flag, precision or
// numeric base format() cannot reproduce.
func tsqlFormatPlaceholders(msg string, hasArgs bool) (string, bool) {
	var out strings.Builder
	lossy := false
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c != '%' {
			out.WriteByte(c)
			continue
		}
		if i+1 < len(msg) && msg[i+1] == '%' {
			if hasArgs {
				out.WriteString("%%")
			} else {
				out.WriteByte('%')
			}
			i++
			continue
		}
		if !hasArgs {
			out.WriteByte('%')
			continue
		}
		j := i + 1
		minus := false
	flags:
		for j < len(msg) {
			switch msg[j] {
			case '-':
				minus = true
			case '+', '0', '#', ' ':
				lossy = true
			default:
				break flags
			}
			j++
		}
		width := ""
		if j < len(msg) && msg[j] == '*' {
			width = "*"
			j++
		} else {
			w := j
			for j < len(msg) && msg[j] >= '0' && msg[j] <= '9' {
				j++
			}
			width = msg[w:j]
		}
		if j < len(msg) && msg[j] == '.' {
			lossy = true
			j++
			if j < len(msg) && msg[j] == '*' {
				j++
			}
			for j < len(msg) && msg[j] >= '0' && msg[j] <= '9' {
				j++
			}
		}
		for j < len(msg) && (msg[j] == 'h' || msg[j] == 'l' || msg[j] == 'I') {
			j++
		}
		if j < len(msg) {
			switch msg[j] {
			case 'o', 'x', 'X':
				lossy = true
				fallthrough
			case 'd', 'i', 'u', 's':
				out.WriteByte('%')
				if minus {
					out.WriteByte('-')
				}
				out.WriteString(width)
				out.WriteByte('s')
				i = j
				continue
			}
		}
		// Not a placeholder: a literal percent sign.
		out.WriteString("%%")
	}
	return out.String(), lossy
}

// resolve maps a source object reference to its PG schema and name.
func (b *tsqlBody) resolve(schema, name string) (string, string) {
	if b.ctx.Run != nil && b.ctx.Run.Schema != nil {
		return b.ctx.Run.ResolveRelation(schema, name, b.ctx.SourceSchema)
	}
	if schema == "" {
		schema = b.ctx.SourceSchema
	}
	return normalizeTSQLIdent(schema), normalizeTSQLIdent(name)
}

// call translates EXEC proc args into CALL. Named arguments use PG's
// named notation; OUTPUT arguments are passed as the variable, which
// PL/pgSQL's CALL assigns back from the INOUT parameter.
func (b *tsqlBody) call(x *ast.CallStmt) []pgast.PLStmt {
	if x.Name == "" || x.Name[0] == '@' {
		b.warn("EXEC @procedure_variable (procedure name held in a variable) not translated: use EXECUTE format('CALL %I(…)', …)")
		return nil
	}
	lname := strings.ToLower(x.Name)
	if strings.EqualFold(x.Schema, "sys") || (len(lname) > 3 && (lname[:3] == "sp_" || lname[:3] == "xp_")) {
		b.warn("system procedure " + x.Name + " has no PostgreSQL counterpart: the call is dropped")
		return nil
	}
	schema, name := b.resolve(x.Schema, x.Name)
	// INOUT parameters need writable arguments (tsql_call_args.go).
	args, pre := b.callArgs(x)
	out := append(pre, &pgast.PLCall{Schema: schema, Name: name, Args: args})
	if x.ReturnVar != "" {
		b.warn("EXEC " + x.ReturnVar + " = " + x.Name + ": a PostgreSQL procedure has no return status; " + x.ReturnVar + " is set to 0 (success) after the CALL")
		if v := b.vars[tsqlVarName(x.ReturnVar)]; v != nil {
			out = append(out, &pgast.PLAssign{Target: v.target(), Expr: "0"})
		}
	}
	return out
}

func tsqlIsDefault(e ast.Expr) bool {
	lit, ok := e.(*ast.Literal)
	return ok && lit.Kind == "default"
}

func tsqlAllDefault(args []ast.Expr) bool {
	for _, a := range args {
		if !tsqlIsDefault(a) {
			return false
		}
	}
	return true
}

// execute translates EXEC (@sql) and sp_executesql into EXECUTE … USING.
// The SQL string itself is T-SQL built at run time: it is never
// rewritten, and always reported.
func (b *tsqlBody) execute(x *ast.ExecuteImmediateStmt) []pgast.PLStmt {
	text := "EXECUTE " + b.expr(x.SQL)
	var using []string
	for _, a := range x.Using {
		if na, ok := a.(*ast.NamedArg); ok {
			if na.Output {
				b.warn("sp_executesql OUTPUT parameter " + na.Name + " not translated (PostgreSQL: EXECUTE … INTO)")
			}
			using = append(using, b.expr(na.Value))
			continue
		}
		using = append(using, b.expr(a))
	}
	if len(using) > 0 {
		text += " USING " + strings.Join(using, ", ")
	}
	msg := "dynamic SQL (EXEC (…) / sp_executesql) runs its string on PostgreSQL unchanged: the statement it builds is T-SQL and must be checked by hand"
	if x.SPExecuteSQL && len(using) > 0 {
		msg += "; its @parameters must become $1, $2, … placeholders, in the order of the USING list"
	}
	b.warn(msg)
	return b.withRowcount(tsqlRaw(text))
}

// transaction translates BEGIN TRAN / COMMIT / ROLLBACK / SAVE TRAN.
func (b *tsqlBody) transaction(x *ast.TransactionStmt) []pgast.PLStmt {
	switch b.ctx.Kind {
	case RoutineProcedure:
		switch x.Kind {
		case "BEGIN":
			b.note("BEGIN TRANSACTION dropped: a PostgreSQL procedure runs in its caller's transaction, which COMMIT / ROLLBACK end (a new one starts right after)")
			return nil
		case "SAVE":
			b.warn("SAVE TRANSACTION (savepoint) has no PL/pgSQL statement: use a nested BEGIN … EXCEPTION … END block")
			return nil
		}
		if x.Kind == "ROLLBACK" && x.Name != "" && b.savepoints[normalizeTSQLIdent(x.Name)] {
			b.warn("ROLLBACK TRANSACTION " + x.Name + " (to a savepoint) has no PL/pgSQL statement: use a nested BEGIN … EXCEPTION … END block")
			return nil
		}
		if b.inTry > 0 || b.inCatch > 0 {
			b.note(x.Kind + " inside BEGIN TRY … END CATCH dropped: PostgreSQL refuses transaction control in a block with an EXCEPTION clause; the block's implicit savepoint already undoes the TRY part when an error is caught")
			return nil
		}
		b.note(x.Kind + " kept: it ends the current transaction, and fails (invalid transaction termination) when the procedure is called inside an explicit transaction block or from a function")
		return []pgast.PLStmt{tsqlRaw(x.Kind)}
	case RoutineTrigger:
		if x.Kind == "ROLLBACK" && x.Name == "" {
			b.note("ROLLBACK TRANSACTION in a trigger becomes RAISE EXCEPTION: the triggering statement and its transaction are rolled back as in T-SQL, but the rest of the trigger body does not run")
			if b.inCatch > 0 {
				return []pgast.PLStmt{tsqlRaw("RAISE EXCEPTION 'transaction rolled back by trigger %: %', TG_NAME, SQLERRM")}
			}
			return []pgast.PLStmt{tsqlRaw("RAISE EXCEPTION 'transaction rolled back by trigger %', TG_NAME")}
		}
	}
	b.warn(x.Kind + " TRANSACTION dropped: transaction control is not allowed in PostgreSQL functions and triggers")
	return nil
}

func (b *tsqlBody) cursorName(src string) string {
	if c := b.cursors[normalizeTSQLIdent(src)]; c != nil {
		return c.name
	}
	return normalizeTSQLIdent(src)
}

// open translates OPEN c: the refcursor is opened FOR the query of its
// DECLARE.
func (b *tsqlBody) open(x *ast.OpenStmt) []pgast.PLStmt {
	c := b.cursors[normalizeTSQLIdent(x.Cursor)]
	if c == nil || c.decl.Stmt == nil {
		b.warn("OPEN " + x.Cursor + ": the cursor is not declared in the routine (or its query did not parse)")
		return nil
	}
	text := "OPEN " + quoteIdent(c.name)
	if c.scroll {
		text += " SCROLL"
	}
	text += " FOR " + b.dml(c.decl.Stmt)
	if c.decl.ForUpdate != "" {
		text += " FOR UPDATE"
	}
	return []pgast.PLStmt{tsqlRaw(text)}
}

// fetch translates FETCH [dir] FROM c INTO @a, … and keeps @@FETCH_STATUS
// (the _fetch_status local) in step: T-SQL only changes it on FETCH,
// while PL/pgSQL's FOUND changes after any statement.
func (b *tsqlBody) fetch(x *ast.FetchStmt) []pgast.PLStmt {
	dir := x.Direction
	if dir == "" {
		dir = "NEXT"
	}
	if (dir == "ABSOLUTE" || dir == "RELATIVE") && x.Offset != nil {
		dir += " " + b.expr(x.Offset)
	}
	name := quoteIdent(b.cursorName(x.Cursor))
	var out []pgast.PLStmt
	if len(x.Into) == 0 {
		b.warn("FETCH without INTO returns the row to the client: the cursor only moves (MOVE)")
		out = append(out, tsqlRaw("MOVE "+dir+" FROM "+name))
	} else {
		targets := make([]string, 0, len(x.Into))
		for _, n := range x.Into {
			v := b.vars[tsqlVarName(n)]
			if v == nil {
				b.warn("FETCH INTO @" + tsqlVarName(n) + ", which is not a variable of the routine")
				return nil
			}
			targets = append(targets, v.target())
		}
		out = append(out, tsqlRaw("FETCH "+dir+" FROM "+name+" INTO "+strings.Join(targets, ", ")))
	}
	if b.fetchStatus != nil {
		found := &ast.CaseExpr{
			Whens: []ast.CaseExprWhen{{Match: &ast.Ident{Parts: []string{"found"}}, Then: &ast.Literal{Kind: "number", Text: "0"}}},
			Else:  &ast.Literal{Kind: "number", Text: "-1"},
		}
		out = append(out, &pgast.PLAssign{Target: b.fetchStatus.target(), Expr: pgast.WriteExpr(found)})
	}
	return out
}

// embeddedDDL translates the DDL of a routine body: CREATE / DROP of
// #temp tables.
func (b *tsqlBody) embeddedDDL(x *ast.EmbeddedDDL) []pgast.PLStmt {
	switch d := x.Stmt.(type) {
	case *ast.CreateTable:
		if n, ok := tsqlTempName(d.Name); ok {
			// T-SQL drops a #temp table when the procedure that created it
			// ends; PG keeps it for the session, so a previous call's table
			// is dropped first.
			return []pgast.PLStmt{
				tsqlRaw("DROP TABLE IF EXISTS " + tsqlTempRef(n)),
				tsqlRaw("CREATE TEMP TABLE " + quoteIdent(n) + " (" + strings.Join(b.tableColumns(d.Columns, d.Constraints), ", ") + ")"),
			}
		}
	case *ast.DropTable:
		var out []pgast.PLStmt
		for _, t := range d.Tables {
			n, ok := tsqlTempName(t.Name)
			if !ok {
				out = nil
				break
			}
			out = append(out, tsqlRaw("DROP TABLE IF EXISTS "+tsqlTempRef(n)))
		}
		if out != nil {
			return out
		}
	}
	b.warn("DDL statement inside the routine body not translated (only CREATE / DROP of #temp tables is)")
	return nil
}

// insteadEnter opens the recursion guard of an INSTEAD OF trigger on a
// table, emulated by a BEFORE … FOR EACH ROW trigger returning NULL
// (TriggerInfo.InsteadGuard). SQL Server does not fire an INSTEAD OF
// trigger again for the DML its own body runs against its table — the
// usual `INSTEAD OF INSERT AS INSERT INTO T SELECT … FROM inserted`
// performs the insert — while PG fires the BEFORE trigger for it, which
// would recurse until the stack depth limit. While the body runs, a
// transaction-local setting marks the trigger as active; the nested DML
// finds it and proceeds as a plain row operation (RETURN NEW / OLD).
// The setting is cleared before every RETURN of the body, and an error
// rolls it back with the (sub)transaction that set it.
func (b *tsqlBody) insteadEnter() []pgast.PLStmt {
	key := b.insteadKey()
	if key == "" {
		return nil
	}
	return []pgast.PLStmt{
		&pgast.PLIf{Branches: []pgast.PLIfBranch{{
			Cond: "coalesce(current_setting(" + key + ", true), '') = 'on'",
			Body: []pgast.PLStmt{
				&pgast.PLIf{Branches: []pgast.PLIfBranch{{Cond: "TG_OP = 'DELETE'", Body: []pgast.PLStmt{&pgast.PLReturn{Expr: "OLD"}}}}},
				&pgast.PLReturn{Expr: "NEW"},
			},
		}}},
		&pgast.PLPerform{Expr: "set_config(" + key + ", 'on', true)"},
	}
}

// insteadLeave clears the guard insteadEnter set (nil without one).
func (b *tsqlBody) insteadLeave() []pgast.PLStmt {
	key := b.insteadKey()
	if key == "" {
		return nil
	}
	return []pgast.PLStmt{&pgast.PLPerform{Expr: "set_config(" + key + ", '', true)"}}
}

// insteadKey is the PG expression naming the guard setting of the
// trigger: per relation (TG_RELID) and trigger. A custom setting name is
// made of simple identifiers, so the trigger part keeps only
// [a-z0-9_] of the folded trigger name.
func (b *tsqlBody) insteadKey() string {
	if b.ctx.Trigger == nil || !b.ctx.Trigger.InsteadGuard {
		return ""
	}
	name := []byte(b.ctx.Name)
	for i, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			name[i] = '_'
		}
	}
	return "'tsql.instead_' || TG_RELID::text || '_" + string(name) + "'"
}

// tsqlTryPartialRollback reports whether a TRY block runs two or more
// data modifications (DML, EXEC of a procedure, dynamic SQL) before any
// BEGIN TRAN of its own. Without an explicit transaction SQL Server
// commits each statement (XACT_ABORT only aborts the failing one): an
// error in the second leaves the first in place, while the PL/pgSQL
// block the TRY becomes rolls both back. With the usual BEGIN TRAN at
// the top of the TRY (and ROLLBACK in the CATCH) both sides undo
// everything. Triggers are not checked: they run inside the triggering
// statement's transaction with XACT_ABORT on.
func tsqlTryPartialRollback(stmts []ast.PLStmt) bool {
	mods := 0
	var walk func([]ast.PLStmt) bool
	walk = func(list []ast.PLStmt) bool {
		for _, s := range list {
			switch x := s.(type) {
			case *ast.TransactionStmt:
				if x.Kind == "BEGIN" {
					return true
				}
			case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt, *ast.MergeStmt,
				*ast.TruncateTable, *ast.CallStmt, *ast.ExecuteImmediateStmt:
				mods++
			case *ast.Block:
				if !x.TryCatch && walk(x.Stmts) {
					return true
				}
			case *ast.IfStmt:
				// One branch runs: count the busiest.
				base, best := mods, mods
				bodies := [][]ast.PLStmt{x.Else}
				for _, br := range x.Branches {
					bodies = append(bodies, br.Body)
				}
				for _, body := range bodies {
					mods = base
					if walk(body) {
						return true
					}
					best = max(best, mods)
				}
				mods = best
			case *ast.WhileStmt:
				// A loop body runs any number of times.
				before := mods
				if walk(x.Body) {
					return true
				}
				if mods > before {
					mods++
				}
			}
			if mods >= 2 {
				return true
			}
		}
		return false
	}
	walk(stmts)
	return mods >= 2
}

// tsqlRaiseText makes a fixed message safe as the format string of a
// PL/pgSQL RAISE: object names may hold a % (legal inside T-SQL
// brackets), which RAISE would read as a placeholder — PL/pgSQL then
// rejects the function at CREATE time ("too few parameters specified
// for RAISE"). %% prints one %.
func tsqlRaiseText(msg string) string {
	var b []byte
	for i := 0; i < len(msg); i++ {
		if msg[i] == '%' {
			if b == nil {
				b = append(b, msg[:i]...)
			}
			b = append(b, '%', '%')
			continue
		}
		if b != nil {
			b = append(b, msg[i])
		}
	}
	if b == nil {
		return msg
	}
	return string(b)
}
