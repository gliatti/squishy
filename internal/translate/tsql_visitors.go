// SQL Server (T-SQL) translation invariant (AST only): source text goes
// through the tsql dialect parser exactly once, the resulting typed
// nodes are rewritten with ast.Rewrite(node, RewriteTSQLAST) — or the
// context-aware pipeline of rewriteTSQLExpr / rewriteTSQLSelect /
// rewriteTSQLBody — and the PostgreSQL text is produced by the
// dialects/postgres writer. No SQL text is inspected here: name
// comparisons only ever touch one identifier (FuncCall.Name, an Ident
// part, a date-part keyword), and the few string literals read (LIKE
// patterns, OBJECT_ID names) are scanned rune by rune or handed back to
// the T-SQL expression parser. ast_only_guard_test.go enforces this.
package translate

import (
	"strconv"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsql_visitors.go — AST rewriters for the SQL Server → PG path.
//
// Same contract as mysql_visitors.go: every visit* method is an
// ast.Rewriter that returns the node unchanged or a freshly built
// replacement, never mutating the node it receives (copy, then modify),
// and ast.Rewrite runs the chain in post-order, so a parent always sees
// children that are already PG-shaped (CHARINDEX sees the folded column
// it searches, `+` sees the to_char its operand became).
//
// Unlike the MySQL passes, several T-SQL rewrites need context: the
// schema map (T-SQL schemas are preserved, `Sales` → `aw_sales`), the
// column types of the tables in scope (`a + b` is a concatenation only
// when an operand is text; `flag = 1` compares a boolean when flag was
// a bit column), the declared variables and routine parameters. That
// context lives in a tsqlRW built per rewrite call from the hook's
// ExprCtx. RewriteTSQLAST is the context-free instance: schema
// qualifiers are only folded to lower case, column types are unknown,
// and nothing is recorded (it is safe for concurrent use).
//
// The rewriters turn calls into operators (STUFF stays a call, but
// DATEADD-like day arithmetic, `^` → `#`, `~x` → `-x - 1` do not), so
// VisitMySQLOperandPrecedence — which only knows PG precedence levels
// and is therefore dialect-neutral — runs last in the chain. Nodes a
// visitor builds with inner operators are parenthesised by
// construction (tsqlPrec), since the chain only revisits the node it
// returns.

// RewriteTSQLAST is the context-free T-SQL → PG rewrite pipeline,
// applied with ast.Rewrite to any expression, DML statement, view query
// or routine statement parsed by internal/dialects/tsql. Prefer the
// context-aware rewriteTSQLExpr / rewriteTSQLSelect / rewriteTSQLBody
// inside a Translate run: they map schemas, prove text concatenations
// from column types, record the tsql.* shim functions used and report
// what could not be translated.
var RewriteTSQLAST = tsqlStatic.chain()

// tsqlStatic is the context-free rewriter state behind RewriteTSQLAST.
// Its maps are nil, so it never records anything and never writes.
var tsqlStatic = &tsqlRW{}

// applyTSQLASTRewrites runs RewriteTSQLAST over every statement of a
// parsed T-SQL routine body (mirror of applyMySQLASTRewrites).
func applyTSQLASTRewrites(stmts []ast.PLStmt) []ast.PLStmt {
	return rewriteTSQLStmts(stmts, RewriteTSQLAST)
}

func rewriteTSQLStmts(stmts []ast.PLStmt, fn ast.Rewriter) []ast.PLStmt {
	if len(stmts) == 0 {
		return stmts
	}
	out := make([]ast.PLStmt, len(stmts))
	for i, s := range stmts {
		if rs, ok := ast.Rewrite(s, fn).(ast.PLStmt); ok {
			out[i] = rs
		} else {
			out[i] = s
		}
	}
	return out
}

// tsqlRewriteResult is what a context-aware rewrite reports besides the
// rewritten tree: Labels name the T-SQL constructs left untranslated
// (blocking manual review), Notes the semantic differences of what was
// translated (informational).
type tsqlRewriteResult struct {
	Labels []string
	Notes  []string
}

// rewriteTSQLExpr rewrites one T-SQL expression with the context of
// ctx (nil: context-free). The tsql.* shim functions the result calls
// are recorded on ctx.Run.
func rewriteTSQLExpr(e ast.Expr, ctx *ExprCtx) (ast.Expr, tsqlRewriteResult) {
	if e == nil {
		return nil, tsqlRewriteResult{}
	}
	rw := newTSQLRW(ctx)
	rw.prepare(e)
	out, ok := ast.Rewrite(e, rw.chain()).(ast.Expr)
	if !ok || out == nil {
		out = e
	}
	return out, rw.finish(out)
}

// rewriteTSQLSelect rewrites a T-SQL query (view body, inline TVF
// query, cursor query) with the context of ctx.
func rewriteTSQLSelect(s *ast.SelectStmt, ctx *ExprCtx) (*ast.SelectStmt, tsqlRewriteResult) {
	if s == nil {
		return nil, tsqlRewriteResult{}
	}
	rw := newTSQLRW(ctx)
	rw.prepare(s)
	out, ok := ast.Rewrite(s, rw.chain()).(*ast.SelectStmt)
	if !ok || out == nil {
		out = s
	}
	return out, rw.finish(out)
}

// rewriteTSQLBody rewrites the statements of a parsed T-SQL routine body
// with the context of ctx (its Routine carries the parameters and the
// trigger relation). After it, every variable reference uses the PG
// spelling of its declaration (`@CustomerID` → `customerid`: Ident
// parts, AssignStmt.Target, DeclareVar.Name, SelectInto.Vars,
// FetchStmt.Into, ExecuteImmediateStmt.Into, NamedArg.Name) and every
// relation of a DML statement or query is its final PG name (schema
// mapped): callers must not map them again.
func rewriteTSQLBody(stmts []ast.PLStmt, ctx *ExprCtx) ([]ast.PLStmt, tsqlRewriteResult) {
	if len(stmts) == 0 {
		return stmts, tsqlRewriteResult{}
	}
	rw := newTSQLRW(ctx)
	nodes := make([]ast.Node, len(stmts))
	for i, s := range stmts {
		nodes[i] = s
	}
	rw.prepare(nodes...)
	out := rewriteTSQLStmts(stmts, rw.chain())
	nodes = nodes[:0]
	for _, s := range out {
		nodes = append(nodes, s)
	}
	return out, rw.finish(nodes...)
}

// ---------------------------------------------------------------------------
// Rewrite state
// ---------------------------------------------------------------------------

// tsqlRW is the state of one context-aware rewrite (or, with nil maps,
// the context-free pipeline).
type tsqlRW struct {
	ctx *ExprCtx
	run *HookRun
	// ctes are the folded names of every CTE defined in the rewritten
	// tree: an unqualified relation with such a name is never schema
	// qualified.
	ctes map[string]bool
	// scope maps a relation key (folded alias, or folded table name) to
	// the PG column → PG type map of the table it names.
	scope map[string]map[string]string
	// loose maps an unqualified column name to its PG type across every
	// table in scope ("" when two tables disagree).
	loose map[string]string
	// vars maps a folded variable / parameter name to its PG type.
	vars   map[string]string
	labels []string
	notes  []string
	seen   map[string]bool
	// core skips, in the residual report, the clauses the translator
	// reports itself on view / TVF queries (FOR XML, TOP … PERCENT).
	core bool
}

func newTSQLRW(ctx *ExprCtx) *tsqlRW {
	rw := &tsqlRW{
		ctx:   ctx,
		ctes:  map[string]bool{},
		scope: map[string]map[string]string{},
		loose: map[string]string{},
		vars:  map[string]string{},
		seen:  map[string]bool{},
	}
	if ctx != nil {
		rw.run = ctx.Run
		if ctx.Routine != nil {
			for _, p := range ctx.Routine.Params {
				// Keyed by what references fold to (tsqlVarName); p.Name
				// may be a renamed spelling (tsqlRenameClashingParams).
				rw.vars[tsqlVarName(p.SourceName)] = p.Type
			}
			if ctx.Routine.Kind == RoutineInlineTVF {
				// Parameters are referenced as "fn"."param" there
				// (inlineTVFParam): typed like a relation of that name.
				params := map[string]string{}
				for _, p := range ctx.Routine.Params {
					params[p.Name] = p.Type
				}
				rw.scope[ctx.Routine.Name] = params
			}
			if ctx.Routine.Kind == RoutineMultiTVF && ctx.Routine.TableVar != "" {
				// The result table variable is a relation of the body.
				cols := map[string]string{}
				for _, c := range ctx.Routine.TableCols {
					cols[c.Name] = c.Type
				}
				rw.addScopeCols(normalizeTSQLIdent(ctx.Routine.TableVar), cols)
			}
		}
	}
	return rw
}

// recording reports whether rw collects labels and notes (false for the
// context-free pipeline, which must stay write-free).
func (rw *tsqlRW) recording() bool { return rw.seen != nil }

// label records a construct left untranslated (blocking).
func (rw *tsqlRW) label(s string) {
	if !rw.recording() || rw.seen["l:"+s] {
		return
	}
	rw.seen["l:"+s] = true
	rw.labels = append(rw.labels, s)
}

// note records a semantic difference of a translated construct (info).
func (rw *tsqlRW) note(s string) {
	if !rw.recording() || rw.seen["n:"+s] {
		return
	}
	rw.seen["n:"+s] = true
	rw.notes = append(rw.notes, s)
}

// finish collects the residual idioms of the rewritten tree, records the
// tsql.* shim functions it calls on the run and returns what the rewrite
// reported.
func (rw *tsqlRW) finish(nodes ...ast.Node) tsqlRewriteResult {
	for _, n := range nodes {
		for _, l := range rw.residual(n, rw.core) {
			rw.label(l)
		}
		if rw.run != nil {
			recordTSQLShimUse(rw.run, n)
		}
	}
	return tsqlRewriteResult{Labels: rw.labels, Notes: rw.notes}
}

// schema maps a source schema qualifier.
func (rw *tsqlRW) schema(src string) string {
	if rw.run != nil && rw.run.Schema != nil {
		return rw.run.Schema(src)
	}
	return normalizeTSQLIdent(src)
}

// mapType renders a T-SQL type as its PG type text (run capabilities
// when available: alias types resolve to their domains).
func (rw *tsqlRW) mapType(dt ast.DataType) string {
	if dt == nil {
		return ""
	}
	if rw.run != nil {
		return rw.run.MapType(dt)
	}
	return MapType(dialects.KindSQLServer, dt, "", Caps{}).PG
}

// tsqlVarName is the PG name of a T-SQL variable or parameter reference:
// the single leading @ stripped, lower-cased (`@CustomerID` →
// `customerid`, the convention of tsqlParamName). System variables
// (@@ROWCOUNT) and names without @ are only lower-cased.
func tsqlVarName(s string) string {
	if len(s) > 1 && s[0] == '@' && s[1] != '@' {
		s = s[1:]
	}
	return normalizeTSQLIdent(s)
}

// isTSQLSystemVar reports whether an Ident variable part is a system
// variable (@@NAME).
func isTSQLSystemVar(s string) bool { return len(s) > 2 && s[0] == '@' && s[1] == '@' }

// isTSQLLocalRelation reports whether a relation name is a temporary
// table (#t, ##t) or a table variable (@t): never schema qualified.
func isTSQLLocalRelation(name string) bool {
	return name != "" && (name[0] == '#' || name[0] == '@')
}

// resolveRelation returns the PG schema ("" for an unqualified PG
// reference resolved through search_path) and name of a relation
// reference. A qualified reference maps its schema; an unqualified one
// is qualified only when the migrated script defines the object (in the
// owning object's schema first, then dbo — SQL Server's resolution
// inside a module). CTEs, the trigger transition tables inserted /
// deleted, temporary tables and table variables stay unqualified.
func (rw *tsqlRW) resolveRelation(srcSchema, name string) (string, string) {
	pgName := normalizeTSQLIdent(name)
	if name == "" || isTSQLLocalRelation(name) {
		return "", pgName
	}
	if srcSchema != "" {
		return rw.schema(srcSchema), pgName
	}
	if rw.ctes[pgName] || pgName == "inserted" || pgName == "deleted" {
		return "", pgName
	}
	if rw.run != nil && rw.run.t != nil {
		def := ""
		if rw.ctx != nil {
			def = rw.ctx.DefaultSchema
		}
		for _, cand := range []string{def, "dbo"} {
			if cand != "" && rw.run.t.sourceObjects[sourceObjectKey(cand, name)] {
				return rw.schema(cand), pgName
			}
		}
	}
	return "", pgName
}

// prepare walks the source tree once before the rewrite: CTE names,
// declared variables, and the column types of every relation in scope.
func (rw *tsqlRW) prepare(nodes ...ast.Node) {
	if !rw.recording() {
		return
	}
	c := &tsqlScopeCollector{rw: rw}
	for _, n := range nodes {
		ast.Walk(c, n)
	}
	if rw.run == nil {
		return
	}
	if rw.ctx != nil && rw.ctx.Routine != nil && rw.ctx.Routine.Trigger != nil {
		tr := rw.ctx.Routine.Trigger
		if cols := rw.run.TableColumns(tr.Schema, tr.Table); cols != nil {
			rw.addScopeCols("inserted", cols)
			rw.addScopeCols("deleted", cols)
		}
	}
	for _, r := range c.rels {
		schema, name := rw.resolveRelation(r.schema, r.name)
		var cols map[string]string
		if schema != "" {
			cols = rw.run.TableColumns(schema, name)
		} else {
			// inserted / deleted: typed from the trigger table above.
			cols = rw.scope[name]
		}
		if cols == nil {
			continue
		}
		rw.addScopeCols(name, cols)
		if r.alias != "" {
			rw.addScopeCols(normalizeTSQLIdent(r.alias), cols)
		}
	}
}

func (rw *tsqlRW) addScopeCols(key string, cols map[string]string) {
	rw.scope[key] = cols
	for c, typ := range cols {
		if prev, ok := rw.loose[c]; ok && prev != typ {
			rw.loose[c] = ""
			continue
		}
		rw.loose[c] = typ
	}
}

type tsqlRelRef struct{ schema, name, alias string }

// tsqlScopeCollector is the pre-order walk of prepare.
type tsqlScopeCollector struct {
	rw   *tsqlRW
	rels []tsqlRelRef
}

func (c *tsqlScopeCollector) with(w *ast.WithClause) {
	if w == nil {
		return
	}
	for _, cte := range w.CTEs {
		c.rw.ctes[normalizeTSQLIdent(cte.Name)] = true
	}
}

func (c *tsqlScopeCollector) Visit(n ast.Node) ast.Visitor {
	switch x := n.(type) {
	case *ast.SelectStmt:
		c.with(x.With)
	case *ast.FromTable:
		c.rels = append(c.rels, tsqlRelRef{x.Schema, x.Name, x.Alias})
	case *ast.InsertStmt:
		c.with(x.With)
		c.rels = append(c.rels, tsqlRelRef{x.Table.Schema, x.Table.Name, ""})
	case *ast.UpdateStmt:
		c.with(x.With)
		c.rels = append(c.rels, tsqlRelRef{x.Table.Schema, x.Table.Name, x.Alias})
	case *ast.DeleteStmt:
		c.with(x.With)
		c.rels = append(c.rels, tsqlRelRef{x.Table.Schema, x.Table.Name, x.Alias})
	case *ast.MergeStmt:
		c.with(x.With)
		c.rels = append(c.rels, tsqlRelRef{x.Target.Schema, x.Target.Name, x.TargetAlias})
	case *ast.DeclareVar:
		if tt, ok := x.Type.(*ast.TSQLTableType); ok {
			// A table variable is a relation (`FROM @t`): its columns
			// type the references to it like a table's.
			cols := map[string]string{}
			for _, col := range tt.Columns {
				if col != nil && col.Type != nil {
					cols[normalizeTSQLIdent(col.Name)] = c.rw.mapType(col.Type)
				}
			}
			c.rw.addScopeCols(normalizeTSQLIdent(x.Name), cols)
		} else if x.Type != nil {
			c.rw.vars[tsqlVarName(x.Name)] = c.rw.mapType(x.Type)
		}
	}
	return c
}

// columnType returns the PG type of a folded column / variable
// reference, "" when unknown.
func (rw *tsqlRW) columnType(id *ast.Ident) string {
	if id == nil || len(id.Parts) == 0 {
		return ""
	}
	last := id.Parts[len(id.Parts)-1]
	if id.Variable {
		return rw.vars[last]
	}
	if len(id.Parts) >= 2 {
		if cols := rw.scope[id.Parts[len(id.Parts)-2]]; cols != nil {
			return cols[last]
		}
		return ""
	}
	if rw.ctx != nil && rw.ctx.Columns != nil {
		if t, ok := rw.ctx.Columns[last]; ok {
			return t
		}
	}
	return rw.loose[last]
}

// chain is the rewrite pipeline over rw. Order on one node: names first
// (later passes compare folded names and look up column types), then
// literals, calls, casts, operators, query shapes, and the precedence
// pass last.
func (rw *tsqlRW) chain() ast.Rewriter {
	return ast.Compose(
		rw.visitIdentFold,
		rw.visitRelations,
		rw.visitStmtNames,
		rw.visitLiterals,
		rw.visitFuncs,
		rw.visitCasts,
		rw.visitOperators,
		rw.visitSelectShape,
		VisitMySQLOperandPrecedence,
	)
}

// Exported context-free visitors, mirroring the VisitMySQL* family.

// VisitTSQLIdentFold folds identifiers (see tsqlRW.visitIdentFold).
func VisitTSQLIdentFold(n ast.Node) ast.Node { return tsqlStatic.visitIdentFold(n) }

// VisitTSQLFuncs rewrites T-SQL built-in calls (see tsqlRW.visitFuncs).
func VisitTSQLFuncs(n ast.Node) ast.Node { return tsqlStatic.visitFuncs(n) }

// VisitTSQLCastType maps CAST / CONVERT targets (see tsqlRW.visitCasts).
func VisitTSQLCastType(n ast.Node) ast.Node { return tsqlStatic.visitCasts(n) }

// VisitTSQLOperators rewrites T-SQL operators (see tsqlRW.visitOperators).
func VisitTSQLOperators(n ast.Node) ast.Node { return tsqlStatic.visitOperators(n) }

// VisitTSQLSelectShape rewrites TOP / APPLY / query clauses (see
// tsqlRW.visitSelectShape).
func VisitTSQLSelectShape(n ast.Node) ast.Node { return tsqlStatic.visitSelectShape(n) }

// tsqlPrec parenthesises the operands of a subtree a visitor built
// itself (the chain only revisits the node a visitor returns).
func tsqlPrec(e ast.Expr) ast.Expr {
	if out, ok := ast.Rewrite(e, VisitMySQLOperandPrecedence).(ast.Expr); ok && out != nil {
		return out
	}
	return e
}

// ---------------------------------------------------------------------------
// Identifiers and relations
// ---------------------------------------------------------------------------

// visitIdentFold folds identifiers the way the translator names objects:
//
//	[Sales].[Order].[Qty] → "sales_prefix"."order"."qty"   (schema mapped)
//	[Order Details].[UnitPrice] → "order details"."unitprice"
//	@CustomerID → customerid   (Variable kept: B3 declares it so)
//	@@IDENTITY → lastval()     @@SPID → pg_backend_pid()   @@VERSION → version()
//
// SQL Server compares identifiers case-insensitively (default
// collation), so quoted and bracketed parts are folded too. A
// three-part column reference carries a schema, which goes through the
// schema map; a four-part one names another database: its first part is
// dropped and the reference reported. inserted / deleted (the trigger
// transition tables) fold to themselves. Other system variables
// (@@ROWCOUNT, @@FETCH_STATUS, @@TRANCOUNT, @@ERROR) are left as they
// are for the routine-body translator.
func (rw *tsqlRW) visitIdentFold(n ast.Node) ast.Node {
	id, ok := n.(*ast.Ident)
	if !ok || len(id.Parts) == 0 {
		return n
	}
	if id.Variable {
		name := id.Parts[0]
		if isTSQLSystemVar(name) {
			return rw.systemVar(id)
		}
		v := tsqlVarName(name)
		if q := rw.inlineTVFParam(name); q != nil {
			cp := *id
			cp.Parts, cp.Variable = q, false
			return &cp
		}
		if v == name && len(id.Parts) == 1 {
			return n
		}
		cp := *id
		cp.Parts = []string{v}
		return &cp
	}
	src := id.Parts
	if len(src) >= 4 {
		rw.label("four-part name " + strings.Join(src, ".") + " (database / linked-server prefix dropped)")
		src = src[len(src)-3:]
	}
	parts := make([]string, len(src))
	changed := len(src) != len(id.Parts)
	for i, p := range src {
		f := p
		if p != "*" {
			f = normalizeTSQLIdent(p)
		}
		if len(src) == 3 && i == 0 {
			f = rw.schema(p)
		}
		parts[i] = f
		if f != p {
			changed = true
		}
	}
	if !changed {
		return n
	}
	cp := *id
	cp.Parts = parts
	return &cp
}

// systemVar maps the T-SQL system variables that have a PG function.
func (rw *tsqlRW) systemVar(id *ast.Ident) ast.Node {
	switch strings.ToUpper(id.Parts[0]) {
	case "@@IDENTITY":
		rw.note("@@IDENTITY → lastval(): the last value of any sequence used by the session, as @@IDENTITY (identities generated by triggers included)")
		return &ast.FuncCall{Name: "lastval", P: id.P}
	case "@@SPID":
		return &ast.FuncCall{Name: "pg_backend_pid", P: id.P}
	case "@@VERSION":
		return &ast.FuncCall{Name: "version", P: id.P}
	case "@@DATEFIRST":
		// us_english default; tsql.datepart(weekday, …) assumes it too.
		return &ast.Literal{Kind: "number", Text: "7", P: id.P}
	}
	return id
}

// visitRelations folds and schema-maps FROM sources:
//
//	FROM [Sales].[SalesOrderHeader] AS [SOH] → FROM "aw_sales"."salesorderheader" "soh"
//	CROSS APPLY (SELECT …)                   → alias apply_sub added when missing
//	FROM STRING_SPLIT(@s, ',') AS t          → FROM string_to_table(s, ',') t (value)
//
// Table hints were already dropped by the parser. PIVOT / UNPIVOT
// have no PG counterpart (crosstab needs tablefunc and a different
// query shape) and are reported.
func (rw *tsqlRW) visitRelations(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.FromTable:
		schema, name := rw.resolveRelation(x.Schema, x.Name)
		alias := normalizeTSQLIdent(x.Alias)
		if schema == x.Schema && name == x.Name && alias == x.Alias {
			return n
		}
		cp := *x
		cp.Schema, cp.Name, cp.Alias = schema, name, alias
		return &cp
	case *ast.FromSubquery:
		cp := *x
		cp.Alias = normalizeTSQLIdent(x.Alias)
		cp.Cols = foldTSQLNames(x.Cols)
		return &cp
	case *ast.FromFunc:
		cp := *x
		cp.Alias = normalizeTSQLIdent(x.Alias)
		cp.Cols = foldTSQLNames(x.Cols)
		switch f := x.Func.(type) {
		case *ast.FuncCall:
			// OPENJSON / OPENXML / CONTAINSTABLE … are reported by the
			// residual pass (tsqlResidualFuncs).
			if f.Name == "string_to_table" && len(cp.Cols) == 0 {
				cp.Cols = []string{"value"}
			}
		case *ast.RawExpr:
			rw.label("XML nodes() table source")
		}
		return &cp
	case *ast.FromValues:
		cp := *x
		cp.Alias = normalizeTSQLIdent(x.Alias)
		cp.Cols = foldTSQLNames(x.Cols)
		return &cp
	case *ast.FromPivot:
		if x.Unpivot {
			rw.label("UNPIVOT")
		} else {
			rw.label("PIVOT")
		}
		cp := *x
		cp.Alias = normalizeTSQLIdent(x.Alias)
		return &cp
	case *ast.FromJoin:
		cp := *x
		cp.Using = foldTSQLNames(x.Using)
		if x.Kind == ast.CrossApply || x.Kind == ast.OuterApply {
			if sq, ok := x.Right.(*ast.FromSubquery); ok && sq.Alias == "" {
				s := *sq
				s.Alias = "apply_sub"
				cp.Right = &s
			}
		}
		return &cp
	}
	return n
}

// foldTSQLNames lower-cases a list of column / alias names (nil stays
// nil).
func foldTSQLNames(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = normalizeTSQLIdent(s)
	}
	return out
}

// visitStmtNames folds the names held in string fields of queries, DML
// and procedural statements (the Ident pass only sees Ident nodes):
// projection aliases, CTE names, DML targets and column lists, variable
// targets. It also re-targets the T-SQL `UPDATE alias … FROM t alias
// JOIN …` / `DELETE alias FROM …` forms, which PostgreSQL spells
// `UPDATE t alias … FROM <the other sources> WHERE <join condition>`.
func (rw *tsqlRW) visitStmtNames(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.SelectStmt:
		cp := *x
		if x.Cols != nil {
			cp.Cols = make([]ast.SelectItem, len(x.Cols))
			for i, c := range x.Cols {
				c.Alias = normalizeTSQLIdent(c.Alias)
				c.Qualifier = normalizeTSQLIdent(c.Qualifier)
				cp.Cols[i] = c
			}
		}
		cp.With = foldTSQLWith(x.With)
		if x.IntoTable != nil {
			s, name := rw.resolveRelation(x.IntoTable.Schema, x.IntoTable.Name)
			cp.IntoTable = &ast.TableRef{Schema: s, Name: name, NameBacktick: x.IntoTable.NameBacktick}
		}
		return &cp
	case *ast.InsertStmt:
		cp := *x
		cp.Table = rw.tableRef(x.Table)
		cp.Cols = foldTSQLNames(x.Cols)
		cp.With = foldTSQLWith(x.With)
		if cols := rw.relColumns(x.Table.Schema, x.Table.Name); cols != nil && len(cp.Cols) > 0 && len(x.Values) > 0 {
			cp.Values = make([][]ast.Expr, len(x.Values))
			for i, row := range x.Values {
				r := append([]ast.Expr(nil), row...)
				for j := range r {
					if j < len(cp.Cols) {
						r[j] = boolifyTSQL(r[j], cols[cp.Cols[j]])
					}
				}
				cp.Values[i] = r
			}
		}
		if cols := rw.relColumns(x.Table.Schema, x.Table.Name); cols != nil && len(cp.Cols) > 0 && x.Select != nil {
			cp.Select = boolifyTSQLProjection(x.Select, cp.Cols, cols)
		}
		return &cp
	case *ast.UpdateStmt:
		cp := *x
		cp.With = foldTSQLWith(x.With)
		from, found, cond, outer := tsqlDetachTarget(x.From, rw.targetMatcher(x.Table))
		switch {
		case outer:
			rw.label("UPDATE … FROM where the target table is outer-joined or applied (PostgreSQL UPDATE … FROM joins inner)")
			cp.Table = rw.tableRef(x.Table)
			cp.Alias = normalizeTSQLIdent(x.Alias)
		case found != nil:
			cp.Table = ast.TableRef{Schema: found.Schema, Name: found.Name}
			cp.Alias = found.Alias
			cp.From = from
			cp.Where = tsqlAnd(cond, x.Where)
		default:
			cp.Table = rw.tableRef(x.Table)
			cp.Alias = normalizeTSQLIdent(x.Alias)
		}
		var cols map[string]string
		if rw.run != nil && cp.Table.Schema != "" {
			cols = rw.run.TableColumns(cp.Table.Schema, cp.Table.Name)
		}
		if x.Sets != nil {
			cp.Sets = make([]ast.Assign, len(x.Sets))
			for i, a := range x.Sets {
				a.Col = normalizeTSQLIdent(a.Col)
				a.TupleCols = foldTSQLNames(a.TupleCols)
				// PG SET targets are bare column names of the target.
				a.ColQualifier = nil
				if cols != nil {
					a.Expr = boolifyTSQL(a.Expr, cols[a.Col])
				}
				cp.Sets[i] = a
			}
		}
		return &cp
	case *ast.DeleteStmt:
		cp := *x
		cp.With = foldTSQLWith(x.With)
		using, found, cond, outer := tsqlDetachTarget(x.Using, rw.targetMatcher(x.Table))
		switch {
		case outer:
			rw.label("DELETE … FROM where the target table is outer-joined or applied (PostgreSQL DELETE … USING joins inner)")
			cp.Table = rw.tableRef(x.Table)
			cp.Alias = normalizeTSQLIdent(x.Alias)
		case found != nil:
			cp.Table = ast.TableRef{Schema: found.Schema, Name: found.Name}
			cp.Alias = found.Alias
			cp.Using = using
			cp.Where = tsqlAnd(cond, x.Where)
		default:
			cp.Table = rw.tableRef(x.Table)
			cp.Alias = normalizeTSQLIdent(x.Alias)
		}
		return &cp
	case *ast.MergeStmt:
		cp := *x
		cp.Target = rw.tableRef(x.Target)
		cp.TargetAlias = normalizeTSQLIdent(x.TargetAlias)
		cp.With = foldTSQLWith(x.With)
		cp.WhenMatched = foldTSQLMergeActions(x.WhenMatched)
		cp.WhenNotMatched = foldTSQLMergeActions(x.WhenNotMatched)
		cp.WhenNotMatchedBySource = foldTSQLMergeActions(x.WhenNotMatchedBySource)
		return &cp
	case *ast.AssignStmt:
		t := tsqlVarName(x.Target)
		e := boolifyTSQL(x.Expr, rw.vars[t])
		if t == x.Target && e == x.Expr {
			return n
		}
		cp := *x
		cp.Target, cp.Expr = t, e
		return &cp
	case *ast.DeclareVar:
		cp := *x
		cp.Name = tsqlVarName(x.Name)
		cp.Default = boolifyTSQL(x.Default, rw.vars[cp.Name])
		return &cp
	case *ast.DeclareCursor:
		cp := *x
		cp.Name = tsqlVarName(x.Name)
		return &cp
	case *ast.OpenStmt:
		cp := *x
		cp.Cursor = tsqlVarName(x.Cursor)
		return &cp
	case *ast.CloseStmt:
		cp := *x
		cp.Cursor = tsqlVarName(x.Cursor)
		return &cp
	case *ast.FetchStmt:
		cp := *x
		cp.Cursor = tsqlVarName(x.Cursor)
		cp.Into = foldTSQLVars(x.Into)
		return &cp
	case *ast.SelectInto:
		cp := *x
		cp.Vars = foldTSQLVars(x.Vars)
		return &cp
	case *ast.ExecuteImmediateStmt:
		cp := *x
		cp.Into = foldTSQLVars(x.Into)
		return &cp
	case *ast.NamedArg:
		if x.Name == "" || x.Name[0] != '@' {
			return n
		}
		cp := *x
		cp.Name = tsqlVarName(x.Name)
		return &cp
	}
	return n
}

func foldTSQLVars(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = tsqlVarName(s)
	}
	return out
}

// tableRef resolves a DML target.
func (rw *tsqlRW) tableRef(t ast.TableRef) ast.TableRef {
	s, name := rw.resolveRelation(t.Schema, t.Name)
	return ast.TableRef{Schema: s, Name: name, NameBacktick: t.NameBacktick}
}

// relColumns returns the PG column types of a source relation reference
// (nil when unknown).
func (rw *tsqlRW) relColumns(srcSchema, name string) map[string]string {
	if rw.run == nil {
		return nil
	}
	s, n := rw.resolveRelation(srcSchema, name)
	if s == "" {
		return nil
	}
	return rw.run.TableColumns(s, n)
}

// targetMatcher returns the predicate finding, among the (already
// rewritten) FROM sources of an UPDATE / DELETE, the source the T-SQL
// target names: an alias (`UPDATE h … FROM Sales.H h`), or the same
// table without another alias (`UPDATE Sales.H … FROM Sales.H JOIN …`).
func (rw *tsqlRW) targetMatcher(target ast.TableRef) func(*ast.FromTable) bool {
	folded := normalizeTSQLIdent(target.Name)
	schema, name := rw.resolveRelation(target.Schema, target.Name)
	return func(ft *ast.FromTable) bool {
		if target.Schema == "" && ft.Alias != "" && ft.Alias == folded {
			return true
		}
		return ft.Name == name && ft.Schema == schema && (ft.Alias == "" || ft.Alias == name)
	}
}

// tsqlDetachTarget removes the FROM source matching the DML target from
// items: a top-level source is dropped, a source inner- or cross-joined
// is replaced by its join partner and the join condition returned (to be
// ANDed into WHERE). outer reports a target on the nullable side of an
// outer join, or under an APPLY, which PG's UPDATE … FROM cannot keep.
func tsqlDetachTarget(items []ast.FromItem, match func(*ast.FromTable) bool) (out []ast.FromItem, found *ast.FromTable, cond ast.Expr, outer bool) {
	for i, it := range items {
		repl, f, c, o := tsqlDetachItem(it, match)
		if o {
			return items, nil, nil, true
		}
		if f == nil {
			continue
		}
		out = append(append([]ast.FromItem(nil), items[:i]...), items[i+1:]...)
		if repl != nil {
			out = append(append(append([]ast.FromItem(nil), items[:i]...), repl), items[i+1:]...)
		}
		if len(out) == 0 {
			out = nil
		}
		return out, f, c, false
	}
	return items, nil, nil, false
}

func tsqlDetachItem(it ast.FromItem, match func(*ast.FromTable) bool) (repl ast.FromItem, found *ast.FromTable, cond ast.Expr, outer bool) {
	switch x := it.(type) {
	case *ast.FromTable:
		if match(x) {
			return nil, x, nil, false
		}
	case *ast.FromJoin:
		inner := x.Kind == ast.InnerJoin || x.Kind == ast.CrossJoin
		if l, f, c, o := tsqlDetachItem(x.Left, match); f != nil || o {
			if o || !(inner || x.Kind == ast.LeftJoin) {
				return it, nil, nil, true
			}
			if x.Kind == ast.LeftJoin {
				// The target keeps every row of a LEFT JOIN: PG's
				// UPDATE … FROM would only touch the matched ones.
				return it, nil, nil, true
			}
			if l == nil {
				return x.Right, f, tsqlAnd(c, x.On), false
			}
			cp := *x
			cp.Left = l
			return &cp, f, c, false
		}
		if r, f, c, o := tsqlDetachItem(x.Right, match); f != nil || o {
			if o || !inner {
				return it, nil, nil, true
			}
			if r == nil {
				return x.Left, f, tsqlAnd(c, x.On), false
			}
			cp := *x
			cp.Right = r
			return &cp, f, c, false
		}
	}
	return it, nil, nil, false
}

// tsqlAnd conjoins two optional predicates, parenthesising OR operands.
func tsqlAnd(a, b ast.Expr) ast.Expr {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	l, _ := parenIf(a, pgExprPrec(a) < pgPrecAnd)
	r, _ := parenIf(b, pgExprPrec(b) < pgPrecAnd)
	return &ast.BinaryExpr{Op: "AND", Lhs: l, Rhs: r}
}

func foldTSQLWith(w *ast.WithClause) *ast.WithClause {
	if w == nil {
		return nil
	}
	cp := *w
	cp.CTEs = make([]ast.CTE, len(w.CTEs))
	for i, c := range w.CTEs {
		c.Name = normalizeTSQLIdent(c.Name)
		c.Columns = foldTSQLNames(c.Columns)
		cp.CTEs[i] = c
	}
	return &cp
}

func foldTSQLMergeActions(in []ast.MergeAction) []ast.MergeAction {
	if in == nil {
		return nil
	}
	out := make([]ast.MergeAction, len(in))
	for i, a := range in {
		a.InsertCols = foldTSQLNames(a.InsertCols)
		if a.Sets != nil {
			sets := make([]ast.Assign, len(a.Sets))
			for j, s := range a.Sets {
				s.Col = normalizeTSQLIdent(s.Col)
				s.ColQualifier = nil
				sets[j] = s
			}
			a.Sets = sets
		}
		out[i] = a
	}
	return out
}

// ---------------------------------------------------------------------------
// Literals
// ---------------------------------------------------------------------------

// visitLiterals rewrites T-SQL literal forms PG spells differently:
//
//	0xFF00        → CAST('\xFF00' AS bytea)   (binary literal)
//	COLLATE x     → dropped (note)
//	NEXT VALUE FOR s.seq → nextval('"schema"."seq"')
//
// A binary literal used as an integer operand (`flags & 0x0F`) is turned
// into its integer value by visitOperators.
func (rw *tsqlRW) visitLiterals(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.Literal:
		if x.Kind != "hex" {
			return n
		}
		return &ast.CastExpr{
			Expr: &ast.Literal{Kind: "string", Text: `\x` + x.Text, P: x.P},
			Type: &ast.PGType{Name: "bytea"},
			P:    x.P,
		}
	case *ast.CollateExpr:
		rw.note("COLLATE " + x.Collation + " dropped: PostgreSQL collations are named differently (the default comparison is case-sensitive)")
		return x.Expr
	case *ast.SequenceRef:
		if !strings.EqualFold(x.Op, "NEXTVAL") {
			return n
		}
		schema, name := rw.resolveRelation(x.Schema, x.Name)
		ref := quoteIdent(name)
		if schema != "" {
			ref = quoteIdent(schema) + "." + ref
		}
		return &ast.FuncCall{Name: "nextval", Args: []ast.Expr{&ast.Literal{Kind: "string", Text: ref, P: x.P}}, P: x.P}
	}
	return n
}

// tsqlHexInt returns the integer literal of a rewritten binary literal
// (CAST('\x…' AS bytea)) of at most 8 bytes, as T-SQL converts it in an
// integer expression.
func tsqlHexInt(e ast.Expr) (*ast.Literal, bool) {
	c, ok := e.(*ast.CastExpr)
	if !ok {
		return nil, false
	}
	t, ok := c.Type.(*ast.PGType)
	if !ok || t.Name != "bytea" {
		return nil, false
	}
	lit, ok := ast.IsLiteralKind(c.Expr, "string")
	if !ok || len(lit.Text) < 3 || lit.Text[0] != '\\' || lit.Text[1] != 'x' || len(lit.Text) > 18 {
		return nil, false
	}
	v, err := strconv.ParseUint(lit.Text[2:], 16, 64)
	if err != nil || v > 1<<63-1 {
		return nil, false
	}
	return &ast.Literal{Kind: "number", Text: strconv.FormatUint(v, 10), P: lit.P}, true
}

// ---------------------------------------------------------------------------
// Function calls
// ---------------------------------------------------------------------------

// tsqlDateParts maps every T-SQL date-part keyword and abbreviation to
// its canonical name (the spelling the tsql.* shim functions document).
var tsqlDateParts = map[string]string{
	"year": "year", "yy": "year", "yyyy": "year",
	"quarter": "quarter", "qq": "quarter", "q": "quarter",
	"month": "month", "mm": "month", "m": "month",
	"dayofyear": "dayofyear", "dy": "dayofyear", "y": "dayofyear",
	"day": "day", "dd": "day", "d": "day",
	"week": "week", "wk": "week", "ww": "week",
	"weekday": "weekday", "dw": "weekday", "w": "weekday",
	"hour": "hour", "hh": "hour",
	"minute": "minute", "mi": "minute", "n": "minute",
	"second": "second", "ss": "second", "s": "second",
	"millisecond": "millisecond", "ms": "millisecond",
	"microsecond": "microsecond", "mcs": "microsecond",
	"nanosecond": "nanosecond", "ns": "nanosecond",
	"iso_week": "iso_week", "isowk": "iso_week", "isoww": "iso_week",
	"tzoffset": "tzoffset", "tz": "tzoffset",
}

// tsqlDatePart reads the date-part argument of DATEADD / DATEDIFF /
// DATEPART / DATENAME: a keyword (parsed as a one-part identifier, folded
// by visitIdentFold) or a string literal.
func tsqlDatePart(e ast.Expr) (string, bool) {
	var word string
	switch x := e.(type) {
	case *ast.Ident:
		if x.Variable || len(x.Parts) != 1 {
			return "", false
		}
		word = x.Parts[0]
	case *ast.Literal:
		if x.Kind != "string" {
			return "", false
		}
		word = x.Text
	default:
		return "", false
	}
	p, ok := tsqlDateParts[strings.ToLower(word)]
	return p, ok
}

// tsqlPGTextFuncs are the PG functions (as the rewrites spell them) whose
// result is text: an operand built from them makes `+` a concatenation.
var tsqlPGTextFuncs = map[string]bool{
	"concat": true, "concat_ws": true, "upper": true, "lower": true, "ltrim": true, "rtrim": true,
	"btrim": true, "trim": true, "substring": true, "substr": true, "replace": true, "repeat": true,
	"left": true, "right": true, "reverse": true, "to_char": true, "lpad": true, "rpad": true,
	"overlay": true, "string_agg": true, "chr": true, "initcap": true, "translate": true,
	"quote_ident": true, "quote_literal": true, "md5": true, "current_database": true,
	"pg_catalog.current_user": true, "pg_catalog.session_user": true, "version": true,
	"tsql.convert_style": true, "tsql.datename": true,
}

// tsqlPGNumberFuncs return numbers.
var tsqlPGNumberFuncs = map[string]bool{
	"length": true, "char_length": true, "octet_length": true, "strpos": true, "abs": true,
	"round": true, "trunc": true, "ceil": true, "floor": true, "power": true, "sqrt": true,
	"exp": true, "ln": true, "log": true, "sign": true, "mod": true, "random": true,
	"count": true, "sum": true, "avg": true, "date_part": true, "ascii": true, "lastval": true,
	"nextval": true, "currval": true, "pg_backend_pid": true, "row_number": true, "rank": true,
	"dense_rank": true, "ntile": true, "atan2": true, "pi": true, "pg_sequence_last_value": true,
	"tsql.datediff": true, "tsql.datepart": true, "tsql.isnumeric": true, "tsql.isdate": true,
}

// tsqlPGTimeFuncs return dates / timestamps.
var tsqlPGTimeFuncs = map[string]bool{
	"now": true, "timezone": true, "make_date": true, "make_timestamp": true, "make_time": true,
	"date_trunc": true, "localtimestamp": true, "current_timestamp": true, "current_date": true,
	"tsql.dateadd": true,
}

// tsqlResidualFuncs are the T-SQL built-ins left untranslated (lower
// case, as visitFuncs leaves unknown single-part names).
var tsqlResidualFuncs = map[string]string{
	"checksum":           "CHECKSUM() (SQL Server hash, no PG equivalent)",
	"binary_checksum":    "BINARY_CHECKSUM() (SQL Server hash, no PG equivalent)",
	"checksum_agg":       "CHECKSUM_AGG() (SQL Server hash, no PG equivalent)",
	"hashbytes":          "HASHBYTES() (use pgcrypto digest())",
	"patindex":           "PATINDEX() (pattern search)",
	"soundex":            "SOUNDEX() (fuzzystrmatch extension)",
	"difference":         "DIFFERENCE() (fuzzystrmatch extension)",
	"openquery":          "OPENQUERY()",
	"openrowset":         "OPENROWSET()",
	"opendatasource":     "OPENDATASOURCE()",
	"openxml":            "OPENXML()",
	"openjson":           "OPENJSON()",
	"containstable":      "CONTAINSTABLE() (full-text search)",
	"freetexttable":      "FREETEXTTABLE() (full-text search)",
	"contains":           "CONTAINS() (full-text search)",
	"freetext":           "FREETEXT() (full-text search)",
	"json_value":         "JSON_VALUE()",
	"json_query":         "JSON_QUERY()",
	"json_modify":        "JSON_MODIFY()",
	"isjson":             "ISJSON()",
	"session_context":    "SESSION_CONTEXT()",
	"context_info":       "CONTEXT_INFO()",
	"formatmessage":      "FORMATMESSAGE()",
	"switchoffset":       "SWITCHOFFSET()",
	"todatetimeoffset":   "TODATETIMEOFFSET()",
	"object_name":        "OBJECT_NAME()",
	"object_id":          "OBJECT_ID() with a computed name",
	"ident_current":      "IDENT_CURRENT() of a table the script does not define",
	"db_name":            "DB_NAME(id)",
	"user_name":          "USER_NAME(id)",
	"suser_sname":        "SUSER_SNAME(sid)",
	"serverproperty":     "SERVERPROPERTY() (server metadata; PG: version(), current_setting(...))",
	"databasepropertyex": "DATABASEPROPERTYEX()",
	"objectproperty":     "OBJECTPROPERTY()",
	"columnproperty":     "COLUMNPROPERTY()",
}

func (rw *tsqlRW) fc(name string, p ast.Position, args ...ast.Expr) *ast.FuncCall {
	return &ast.FuncCall{Name: name, Args: args, P: p}
}

// textify returns e when it is provably text, CAST(e AS text) otherwise
// (PG has no implicit cast to text for function arguments, T-SQL
// converts implicitly).
func (rw *tsqlRW) textify(e ast.Expr) ast.Expr {
	switch rw.exprVal(e) {
	case tsqlValText, tsqlValNull:
		return e
	}
	return &ast.CastExpr{Expr: e, Type: &ast.PGType{Name: "text"}}
}

// timeArg casts a string operand of a date function to timestamp, as
// T-SQL converts a string to datetime (and PG cannot pick an overload
// for an untyped literal).
func (rw *tsqlRW) timeArg(e ast.Expr) ast.Expr {
	if rw.exprVal(e) == tsqlValText {
		return &ast.CastExpr{Expr: e, Type: &ast.PGType{Name: "timestamp"}}
	}
	return e
}

// visitFuncs rewrites T-SQL built-in calls (names arrive upper-cased
// from the parser for one-part unquoted names):
//
//	ISNULL(a, b)            → coalesce(a, b)
//	GETDATE() / SYSDATETIME() / CURRENT_TIMESTAMP / SYSDATETIMEOFFSET() → now()
//	GETUTCDATE() / SYSUTCDATETIME() → timezone('UTC', now())
//	NEWID() / NEWSEQUENTIALID()     → gen_random_uuid()
//	LEN(s)                  → length(rtrim(s))
//	DATALENGTH(s)           → octet_length(s)
//	CHARINDEX(a, b[, n])    → strpos(b, a) (start: CASE over strpos(substr(b, n), a))
//	STUFF(s, i, n, r)       → overlay(s, coalesce(r, ''), i, n)
//	REPLICATE(s, n) / SPACE(n) → repeat(s, n) / repeat(' ', n)
//	STRING_AGG(x, s) WITHIN GROUP (ORDER BY o) → string_agg(x, s ORDER BY o)
//	FORMAT(v, f)            → to_char(v, f)   (reported: .NET pattern)
//	STR(x[, len[, dec]])    → lpad(CAST(round(CAST(x AS numeric), dec) AS text), len)
//	IIF(c, a, b) / CHOOSE(i, …) → CASE
//	SCOPE_IDENTITY()        → lastval()
//	IDENT_CURRENT('t')      → pg_sequence_last_value(pg_get_serial_sequence('t', 'idcol'))
//	SUSER_SNAME() / SYSTEM_USER / CURRENT_USER / … → pg_catalog.current_user()
//	HOST_NAME()             → CAST(inet_client_addr() AS text)
//	DB_NAME()               → current_database()
//	OBJECT_ID('s.t'[, ty])  → CAST(to_regclass('"s"."t"') AS oid)
//	ISNUMERIC / ISDATE      → tsql.isnumeric / tsql.isdate
//	DATEADD / DATEDIFF[_BIG] / DATEPART / DATENAME(part, …) → tsql.*('part', …)
//	YEAR / MONTH / DAY(d)   → CAST(date_part('year', d) AS integer)
//	EOMONTH(d[, n])         → CAST(date_trunc('month', d) [+ n months] + INTERVAL '1 month - 1 day' AS date)
//	DATEFROMPARTS / DATETIMEFROMPARTS / … → make_date / make_timestamp / make_time
//	SQUARE(x) → power(x, 2); ROUND(x, n, f≠0) → trunc(x, n); RAND() → random()
//	LOG(x) → ln(x); LOG(x, b) → log(b, x); LOG10(x) → log(x); CEILING → ceil
//	CHAR / NCHAR → chr; UNICODE → ascii; ATN2 → atan2; COUNT_BIG → count
//	QUOTENAME(s) → quote_ident(s); STRING_SPLIT → string_to_table
//
// User functions (multi-part or quoted names) are folded and their
// schema mapped. Other one-part names are lower-cased; the ones without
// PG counterpart (CHECKSUM, HASHBYTES, PATINDEX, …) are reported by the
// residual pass.
func (rw *tsqlRW) visitFuncs(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.WindowedAgg:
		return rw.windowedAgg(x)
	case *ast.MethodCall:
		target := x.StaticType
		if target == "" {
			target = "column"
		}
		rw.label("CLR method " + target + "." + x.Method + "() (hierarchyid / spatial / xml type methods have no PG equivalent)")
		return n
	case *ast.RawExpr:
		if x.Kind == "XML_METHOD" {
			rw.label("XML data-type method (value / query / exist / nodes / modify)")
		}
		return n
	case *ast.FuncCall:
		return rw.funcCall(x)
	}
	return n
}

func (rw *tsqlRW) windowedAgg(w *ast.WindowedAgg) ast.Node {
	if w.Func == nil || !strings.EqualFold(w.Func.Name, "string_agg") || len(w.Within) == 0 {
		return w
	}
	f := *w.Func
	f.AggOrderBy = append(append([]ast.OrderItem(nil), f.AggOrderBy...), w.Within...)
	if w.Over == nil {
		return &f
	}
	cp := *w
	cp.Func = &f
	cp.Within = nil
	return &cp
}

// userFuncName folds and schema-maps a user function name. The parser
// joins the parts of a multi-part name with '.', so the parts are
// recovered by a byte walk (an identifier list, not SQL text).
func (rw *tsqlRW) userFuncName(name string) string {
	var parts []string
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '.' {
			continue
		}
		parts = append(parts, name[start:i])
		start = i + 1
	}
	if len(parts) > 2 {
		rw.label("cross-database function " + name + " (database prefix dropped)")
		parts = parts[len(parts)-2:]
	}
	if len(parts) == 2 {
		return quoteIdent(rw.schema(parts[0])) + "." + quoteIdent(normalizeTSQLIdent(parts[1]))
	}
	n := normalizeTSQLIdent(parts[0])
	if !isSimplePGIdent(n) {
		return quoteIdent(n)
	}
	return n
}

// isSimplePGIdent reports whether s can be written unquoted on PG
// (lower-case letters, digits, underscores; not starting with a digit).
func isSimplePGIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// isUpperName reports whether a one-part name is the parser's upper-cased
// spelling of an unquoted built-in.
func isUpperName(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			return false
		}
		if s[i] == '.' {
			return false
		}
	}
	return true
}

// tsqlKeptFuncs are the T-SQL built-ins left exactly as parsed: the
// routine-body translator rewrites them (trigger UPDATE(col), the
// ERROR_* functions of a CATCH block, transaction state), and the
// quantified-subquery markers ALL / ANY / SOME render as they are.
var tsqlKeptFuncs = map[string]bool{
	"UPDATE": true, "COLUMNS_UPDATED": true, "TRIGGER_NESTLEVEL": true, "XACT_STATE": true,
	"ERROR_MESSAGE": true, "ERROR_NUMBER": true, "ERROR_SEVERITY": true, "ERROR_STATE": true,
	"ERROR_LINE": true, "ERROR_PROCEDURE": true, "ALL": true, "ANY": true, "SOME": true,
}

// hasQuote reports whether a function name was already quoted by
// userFuncName (a second pass over the node must leave it alone).
func hasQuote(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			return true
		}
	}
	return false
}

func (rw *tsqlRW) funcCall(x *ast.FuncCall) ast.Node {
	if tsqlKeptFuncs[x.Name] || hasQuote(x.Name) {
		return x
	}
	if !isUpperName(x.Name) {
		// User function (schema-qualified or quoted name), or a PG name
		// an earlier pass of this chain already produced.
		if tsqlPGTextFuncs[x.Name] || tsqlPGNumberFuncs[x.Name] || tsqlPGTimeFuncs[x.Name] || tsqlShimFuncs[x.Name] != nil || isPGBuiltinSpelling(x.Name) {
			return x
		}
		name := rw.userFuncName(x.Name)
		if name == x.Name {
			return x
		}
		return renameMySQLFuncCall(x, name)
	}
	a := x.Args
	p := x.P
	switch x.Name {
	case "ISNULL":
		out := renameMySQLFuncCall(x, "coalesce")
		rw.boolifyArgs(out.Args)
		return out
	case "COALESCE", "NULLIF":
		out := renameMySQLFuncCall(x, strings.ToLower(x.Name))
		rw.boolifyArgs(out.Args)
		return out
	case "GETDATE", "SYSDATETIME", "CURRENT_TIMESTAMP", "SYSDATETIMEOFFSET":
		return rw.fc("now", p)
	case "GETUTCDATE", "SYSUTCDATETIME":
		return rw.fc("timezone", p, ast.BuildStringLit("UTC"), rw.fc("now", p))
	case "NEWID":
		return rw.fc("gen_random_uuid", p)
	case "NEWSEQUENTIALID":
		rw.note("NEWSEQUENTIALID() → gen_random_uuid(): the values are random, not increasing")
		return rw.fc("gen_random_uuid", p)
	case "LEN":
		if len(a) != 1 {
			return x
		}
		return rw.fc("length", p, rw.fc("rtrim", p, rw.textify(a[0])))
	case "DATALENGTH":
		if len(a) != 1 {
			return x
		}
		rw.note("DATALENGTH → octet_length: PG counts UTF-8 bytes, SQL Server 2 bytes per nchar / nvarchar character")
		return rw.fc("octet_length", p, a[0])
	case "CHARINDEX":
		return rw.charIndex(x)
	case "STUFF":
		if len(a) != 4 {
			return x
		}
		ins := a[3]
		if rw.exprVal(ins) != tsqlValText {
			ins = rw.fc("coalesce", p, rw.textify(ins), ast.BuildStringLit(""))
		}
		return rw.fc("overlay", p, rw.textify(a[0]), ins, a[1], a[2])
	case "REPLICATE":
		if len(a) != 2 {
			return x
		}
		return rw.fc("repeat", p, rw.textify(a[0]), a[1])
	case "SPACE":
		if len(a) != 1 {
			return x
		}
		return rw.fc("repeat", p, ast.BuildStringLit(" "), a[0])
	case "LEFT", "RIGHT", "REVERSE", "LTRIM", "RTRIM", "UPPER", "LOWER", "TRIM", "REPLACE", "TRANSLATE":
		out := renameMySQLFuncCall(x, strings.ToLower(x.Name))
		if len(out.Args) > 0 {
			out.Args[0] = rw.textify(out.Args[0])
		}
		return out
	case "SUBSTRING":
		out := renameMySQLFuncCall(x, "substring")
		if len(out.Args) > 0 {
			out.Args[0] = rw.textify(out.Args[0])
		}
		return out
	case "STRING_AGG":
		out := renameMySQLFuncCall(x, "string_agg")
		if len(out.Args) > 0 {
			out.Args[0] = rw.textify(out.Args[0])
		}
		return out
	case "STRING_SPLIT":
		if len(a) != 2 {
			rw.label("STRING_SPLIT(…, enable_ordinal)")
			return x
		}
		return rw.fc("string_to_table", p, rw.textify(a[0]), a[1])
	case "FORMAT":
		if len(a) < 2 {
			return x
		}
		rw.label("FORMAT(): the .NET format string is passed to to_char unchanged — check the pattern")
		return rw.fc("to_char", p, a[0], a[1])
	case "STR":
		return rw.str(x)
	case "IIF":
		if len(a) != 3 {
			return x
		}
		return &ast.CaseExpr{Whens: []ast.CaseExprWhen{{Match: a[0], Then: a[1]}}, Else: a[2], P: p}
	case "CHOOSE":
		if len(a) < 2 {
			return x
		}
		ce := &ast.CaseExpr{Operand: a[0], P: p}
		for i, v := range a[1:] {
			ce.Whens = append(ce.Whens, ast.CaseExprWhen{Match: ast.BuildIntLit(int64(i + 1)), Then: v})
		}
		return ce
	case "SCOPE_IDENTITY":
		rw.note("SCOPE_IDENTITY() → lastval(): the last value of any sequence used by the session (a trigger inserting into an identity table changes it, unlike SCOPE_IDENTITY)")
		return rw.fc("lastval", p)
	case "IDENT_CURRENT":
		return rw.identCurrent(x)
	case "SUSER_SNAME", "SUSER_NAME", "USER_NAME", "SYSTEM_USER", "CURRENT_USER", "ORIGINAL_LOGIN", "USER", "SESSION_USER":
		if len(a) != 0 {
			return renameMySQLFuncCall(x, strings.ToLower(x.Name))
		}
		if x.Name == "SESSION_USER" || x.Name == "ORIGINAL_LOGIN" {
			return rw.fc("pg_catalog.session_user", p)
		}
		return rw.fc("pg_catalog.current_user", p)
	case "HOST_NAME":
		rw.note("HOST_NAME() → inet_client_addr(): the client IP address, not its host name")
		return &ast.CastExpr{Expr: rw.fc("inet_client_addr", p), Type: &ast.PGType{Name: "text"}, P: p}
	case "DB_NAME":
		if len(a) != 0 {
			return renameMySQLFuncCall(x, "db_name")
		}
		return rw.fc("current_database", p)
	case "OBJECT_ID":
		return rw.objectID(x)
	case "ISNUMERIC":
		if len(a) != 1 {
			return x
		}
		return rw.fc("tsql.isnumeric", p, rw.textify(a[0]))
	case "ISDATE":
		if len(a) != 1 {
			return x
		}
		return rw.fc("tsql.isdate", p, rw.textify(a[0]))
	case "DATEADD", "DATEDIFF", "DATEDIFF_BIG", "DATEPART", "DATENAME":
		return rw.dateFunc(x)
	case "YEAR", "MONTH", "DAY":
		if len(a) != 1 {
			return x
		}
		return &ast.CastExpr{
			Expr: rw.fc("date_part", p, ast.BuildStringLit(strings.ToLower(x.Name)), rw.timeArg(a[0])),
			Type: &ast.PGType{Name: "integer"}, P: p,
		}
	case "EOMONTH":
		if len(a) < 1 || len(a) > 2 {
			return x
		}
		var d ast.Expr = rw.fc("date_trunc", p, ast.BuildStringLit("month"), rw.timeArg(a[0]))
		if len(a) == 2 {
			d = ast.BuildBinary("+", d, ast.BuildBinary("*", parenIfCompound(a[1]), &ast.IntervalLit{Value: "1 month"}))
		}
		d = ast.BuildBinary("+", d, &ast.IntervalLit{Value: "1 month - 1 day"})
		return &ast.CastExpr{Expr: tsqlPrec(d), Type: &ast.PGType{Name: "date"}, P: p}
	case "DATEFROMPARTS":
		if len(a) != 3 {
			return x
		}
		return rw.fc("make_date", p, a...)
	case "DATETIMEFROMPARTS":
		if len(a) != 7 {
			return x
		}
		secs := tsqlPrec(ast.BuildBinary("+", a[5], ast.BuildBinary("/", a[6], &ast.Literal{Kind: "number", Text: "1000.0"})))
		return rw.fc("make_timestamp", p, a[0], a[1], a[2], a[3], a[4], secs)
	case "DATETIME2FROMPARTS":
		if len(a) != 8 {
			return x
		}
		secs := tsqlPrec(ast.BuildBinary("+", a[5], ast.BuildBinary("/", a[6], rw.fc("power", p, &ast.Literal{Kind: "number", Text: "10.0"}, a[7]))))
		return rw.fc("make_timestamp", p, a[0], a[1], a[2], a[3], a[4], secs)
	case "SMALLDATETIMEFROMPARTS":
		if len(a) != 5 {
			return x
		}
		return rw.fc("make_timestamp", p, a[0], a[1], a[2], a[3], a[4], ast.BuildIntLit(0))
	case "TIMEFROMPARTS":
		if len(a) != 5 {
			return x
		}
		secs := tsqlPrec(ast.BuildBinary("+", a[2], ast.BuildBinary("/", a[3], rw.fc("power", p, &ast.Literal{Kind: "number", Text: "10.0"}, a[4]))))
		return rw.fc("make_time", p, a[0], a[1], secs)
	case "SQUARE":
		if len(a) != 1 {
			return x
		}
		return rw.fc("power", p, a[0], ast.BuildIntLit(2))
	case "ROUND":
		return rw.round(x)
	case "RAND":
		if len(a) > 0 {
			rw.note("RAND(seed) → random(): the seed is ignored (call setseed() first for a repeatable sequence)")
		}
		return rw.fc("random", p)
	case "LOG":
		switch len(a) {
		case 1:
			return rw.fc("ln", p, a[0])
		case 2:
			return rw.fc("log", p, a[1], a[0])
		}
		return x
	case "LOG10":
		return renameMySQLFuncCall(x, "log")
	case "CEILING":
		return renameMySQLFuncCall(x, "ceil")
	case "ATN2":
		return renameMySQLFuncCall(x, "atan2")
	case "COUNT_BIG":
		return renameMySQLFuncCall(x, "count")
	case "CHAR", "NCHAR":
		return renameMySQLFuncCall(x, "chr")
	case "UNICODE", "ASCII":
		out := renameMySQLFuncCall(x, "ascii")
		if len(out.Args) == 1 {
			out.Args[0] = rw.textify(out.Args[0])
		}
		return out
	case "QUOTENAME":
		if len(a) == 1 {
			rw.note("QUOTENAME(s) → quote_ident(s): PG quotes identifiers with double quotes, not brackets")
			return rw.fc("quote_ident", p, rw.textify(a[0]))
		}
		if len(a) == 2 {
			if q, ok := ast.IsLiteralKind(a[1], "string"); ok && q.Text == "'" {
				return rw.fc("quote_literal", p, rw.textify(a[0]))
			}
		}
		rw.label("QUOTENAME(s, quote_char)")
		return x
	}
	return renameMySQLFuncCall(x, strings.ToLower(x.Name))
}

// isPGBuiltinSpelling reports whether a non-upper-case name is one this
// chain produces itself (so a second pass over the node is a no-op).
func isPGBuiltinSpelling(name string) bool {
	switch name {
	case "coalesce", "nullif", "gen_random_uuid", "timezone", "rtrim", "overlay", "string_to_table",
		"to_regclass", "to_regproc", "pg_get_serial_sequence", "inet_client_addr", "make_date",
		"make_timestamp", "make_time", "quote_ident", "quote_literal":
		return true
	}
	return false
}

// boolifyArgs turns 0 / 1 literals into booleans in coalesce / nullif
// arguments when another argument is boolean (ISNULL(bitcol, 0)).
func (rw *tsqlRW) boolifyArgs(args []ast.Expr) {
	hasBool := false
	for _, e := range args {
		if rw.exprVal(e) == tsqlValBool {
			hasBool = true
		}
	}
	if !hasBool {
		return
	}
	for i, e := range args {
		args[i] = boolifyTSQL(e, "boolean")
	}
}

func (rw *tsqlRW) charIndex(x *ast.FuncCall) ast.Node {
	a := x.Args
	if len(a) != 2 && len(a) != 3 {
		return x
	}
	p := x.P
	needle, hay := rw.textify(a[0]), rw.textify(a[1])
	if len(a) == 2 {
		return rw.fc("strpos", p, hay, needle)
	}
	var start ast.Expr
	if lit, ok := ast.IsLiteralKind(a[2], "number"); ok {
		k, err := strconv.Atoi(lit.Text)
		if err == nil && k <= 1 {
			return rw.fc("strpos", p, hay, needle)
		}
		if err == nil {
			start = ast.BuildIntLit(int64(k))
		}
	}
	if start == nil {
		start = rw.fc("greatest", p, a[2], ast.BuildIntLit(1))
	}
	rw.note("CHARINDEX(…, start) → CASE over strpos(substr(…, start), …): the searched arguments are evaluated twice")
	found := rw.fc("strpos", p, rw.fc("substr", p, hay, start), needle)
	return &ast.CaseExpr{
		Whens: []ast.CaseExprWhen{{
			Match: ast.BuildBinary("=", found, ast.BuildIntLit(0)),
			Then:  ast.BuildIntLit(0),
		}},
		Else: tsqlPrec(ast.BuildBinary("-", ast.BuildBinary("+", found, start), ast.BuildIntLit(1))),
		P:    p,
	}
}

func (rw *tsqlRW) str(x *ast.FuncCall) ast.Node {
	a := x.Args
	if len(a) < 1 || len(a) > 3 {
		return x
	}
	var length, dec ast.Expr = ast.BuildIntLit(10), ast.BuildIntLit(0)
	if len(a) >= 2 {
		length = a[1]
	}
	if len(a) == 3 {
		dec = a[2]
	}
	rw.note("STR() → lpad(round(…)): a value wider than the length is truncated instead of shown as '**'")
	num := &ast.CastExpr{Expr: a[0], Type: &ast.PGType{Name: "numeric"}}
	txt := &ast.CastExpr{Expr: rw.fc("round", x.P, num, dec), Type: &ast.PGType{Name: "text"}}
	return rw.fc("lpad", x.P, txt, length)
}

func (rw *tsqlRW) round(x *ast.FuncCall) ast.Node {
	a := x.Args
	if len(a) != 3 {
		return renameMySQLFuncCall(x, "round")
	}
	if lit, ok := ast.IsLiteralKind(tsqlUnparen(a[2]), "number"); ok {
		if lit.Text == "0" {
			return rw.fc("round", x.P, a[0], a[1])
		}
		rw.note("ROUND(x, n, function ≠ 0) → trunc(x, n)")
		return rw.fc("trunc", x.P, a[0], a[1])
	}
	rw.note("ROUND(x, n, function) → CASE choosing round / trunc")
	return &ast.CaseExpr{
		Whens: []ast.CaseExprWhen{{Match: ast.BuildBinary("=", a[2], ast.BuildIntLit(0)), Then: rw.fc("round", x.P, a[0], a[1])}},
		Else:  rw.fc("trunc", x.P, a[0], a[1]),
		P:     x.P,
	}
}

// tsqlNameLiteral parses the content of a string literal naming an
// object (`'dbo.T'`, `'[Sales].[Order]'`) with the T-SQL expression
// parser: the name is an identifier, read back from its AST.
func tsqlNameLiteral(e ast.Expr) (schema, name string, ok bool) {
	lit, isLit := ast.IsLiteralKind(e, "string")
	if !isLit {
		return "", "", false
	}
	parsed, errs := tsqldialect.ParseExpr(lit.Text)
	if len(errs) > 0 {
		return "", "", false
	}
	id, isID := parsed.(*ast.Ident)
	if !isID || id.Variable || len(id.Parts) == 0 || len(id.Parts) > 3 {
		return "", "", false
	}
	name = id.Parts[len(id.Parts)-1]
	if len(id.Parts) >= 2 {
		schema = id.Parts[len(id.Parts)-2]
	}
	return schema, name, true
}

// qualifiedLiteral renders the PG name of a relation as a regclass
// string literal.
func (rw *tsqlRW) qualifiedLiteral(srcSchema, name string) *ast.Literal {
	schema, pgName := rw.resolveRelation(srcSchema, name)
	ref := quoteIdent(pgName)
	if schema != "" {
		ref = quoteIdent(schema) + "." + ref
	}
	return ast.BuildStringLit(ref)
}

func (rw *tsqlRW) objectID(x *ast.FuncCall) ast.Node {
	a := x.Args
	if len(a) < 1 || len(a) > 2 {
		return renameMySQLFuncCall(x, "object_id")
	}
	schema, name, ok := tsqlNameLiteral(a[0])
	if !ok {
		return renameMySQLFuncCall(x, "object_id")
	}
	fn := "to_regclass"
	if len(a) == 2 {
		if ty, ok := ast.IsLiteralKind(a[1], "string"); ok {
			switch strings.ToUpper(strings.TrimSpace(ty.Text)) {
			case "P", "PC", "FN", "IF", "TF", "FS", "FT", "X":
				fn = "to_regproc"
			}
		}
	}
	rw.note("OBJECT_ID() → " + fn + "(): the oid is not a SQL Server object id; only NULL / NOT NULL tests keep their meaning")
	return &ast.CastExpr{Expr: rw.fc(fn, x.P, rw.qualifiedLiteral(schema, name)), Type: &ast.PGType{Name: "oid"}, P: x.P}
}

func (rw *tsqlRW) identCurrent(x *ast.FuncCall) ast.Node {
	if len(x.Args) != 1 || rw.run == nil || rw.run.t == nil {
		return renameMySQLFuncCall(x, "ident_current")
	}
	srcSchema, name, ok := tsqlNameLiteral(x.Args[0])
	if !ok {
		return renameMySQLFuncCall(x, "ident_current")
	}
	schema, pgName := rw.resolveRelation(srcSchema, name)
	for i := range rw.run.t.res.Plan.Tables {
		tbl := &rw.run.t.res.Plan.Tables[i]
		if tbl.Schema != schema || tbl.Name != pgName {
			continue
		}
		for _, c := range tbl.Columns {
			if !c.Identity {
				continue
			}
			rw.note("IDENT_CURRENT() → pg_sequence_last_value(): NULL until the identity is first used (SQL Server returns the seed)")
			seq := rw.fc("pg_get_serial_sequence", x.P, rw.qualifiedLiteral(srcSchema, name), ast.BuildStringLit(c.Name))
			return rw.fc("pg_sequence_last_value", x.P, &ast.CastExpr{Expr: seq, Type: &ast.PGType{Name: "regclass"}})
		}
	}
	return renameMySQLFuncCall(x, "ident_current")
}

func (rw *tsqlRW) dateFunc(x *ast.FuncCall) ast.Node {
	a := x.Args
	want := 2
	switch x.Name {
	case "DATEADD", "DATEDIFF", "DATEDIFF_BIG":
		want = 3
	}
	if len(a) != want {
		return x
	}
	part, ok := tsqlDatePart(a[0])
	if !ok {
		rw.label(x.Name + "() with an unknown date part")
		return x
	}
	part0 := &ast.Literal{Kind: "string", Text: part, P: a[0].Pos()}
	switch x.Name {
	case "DATEADD":
		return rw.fc("tsql.dateadd", x.P, part0, a[1], rw.timeArg(a[2]))
	case "DATEDIFF", "DATEDIFF_BIG":
		return rw.fc("tsql.datediff", x.P, part0, rw.timeArg(a[1]), rw.timeArg(a[2]))
	case "DATEPART":
		return rw.fc("tsql.datepart", x.P, part0, rw.timeArg(a[1]))
	}
	return rw.fc("tsql.datename", x.P, part0, rw.timeArg(a[1]))
}

// ---------------------------------------------------------------------------
// CAST / CONVERT
// ---------------------------------------------------------------------------

// tsqlConvertStyles maps the CONVERT styles squishy emulates to their
// PG to_char / to_timestamp pattern (the tsql.convert_style shim formats
// with the same patterns).
var tsqlConvertStyles = map[string]string{
	"101": "MM/DD/YYYY",
	"103": "DD/MM/YYYY",
	"104": "DD.MM.YYYY",
	"105": "DD-MM-YYYY",
	"110": "MM-DD-YYYY",
	"111": "YYYY/MM/DD",
	"112": "YYYYMMDD",
	"120": "YYYY-MM-DD HH24:MI:SS",
	"121": "YYYY-MM-DD HH24:MI:SS.MS",
	"126": `YYYY-MM-DD"T"HH24:MI:SS.MS`,
}

// visitCasts maps the target of CAST / CONVERT / TRY_* to PG:
//
//	CAST(x AS nvarchar(20))        → CAST(x AS varchar(20))
//	CAST(x AS varchar)             → CAST(x AS varchar(30))   (T-SQL CAST default length)
//	CONVERT(varchar(10), d, 112)   → CAST(tsql.convert_style(d, 112) AS varchar(10))
//	CONVERT(datetime, s, 103)      → CAST(to_timestamp(s, 'DD/MM/YYYY') AS timestamp(3))
//	CONVERT(date, d, 112)          → CAST(d AS date)          (style only applies to strings)
//	TRY_CAST / TRY_CONVERT         → CAST (raises instead of NULL: noted)
func (rw *tsqlRW) visitCasts(n ast.Node) ast.Node {
	c, ok := n.(*ast.CastExpr)
	if !ok || c.Type == nil {
		return n
	}
	if _, pg := c.Type.(*ast.PGType); pg {
		return n
	}
	typ := rw.castType(c.Type)
	if c.Try {
		rw.note("TRY_CAST / TRY_CONVERT → CAST: PostgreSQL raises an error on a value it cannot convert instead of returning NULL")
	}
	target := &ast.PGType{Name: typ, P: c.Type.Pos()}
	out := &ast.CastExpr{Expr: c.Expr, Type: target, P: c.P}
	tv := pgTypeVal(typ)
	src := rw.exprVal(c.Expr)
	if tv == tsqlValBool {
		out.Expr = boolifyTSQL(c.Expr, "boolean")
	}
	if c.ConvertStyle == nil {
		if tv == tsqlValText && src == tsqlValTime {
			rw.note("date/time → string conversions use the ISO format on PostgreSQL (SQL Server's default style 0 is 'mon dd yyyy hh:miAM')")
		}
		return out
	}
	style, isLit := ast.IsLiteralKind(tsqlUnparen(c.ConvertStyle), "number")
	switch {
	case tv == tsqlValText:
		switch {
		case src == tsqlValText || src == tsqlValNull:
			// string → string: the style is ignored by SQL Server too.
		case isLit && tsqlConvertStyles[style.Text] != "" && src != tsqlValNumber && src != tsqlValBinary:
			rw.note("CONVERT(…, style " + style.Text + ") emulated by tsql.convert_style()")
			out.Expr = rw.fc("tsql.convert_style", c.P, c.Expr, ast.BuildIntLit(mustAtoi(style.Text)))
		case isLit && style.Text == "0":
			if src == tsqlValTime {
				rw.note("date/time → string conversions use the ISO format on PostgreSQL (SQL Server's default style 0 is 'mon dd yyyy hh:miAM')")
			}
		default:
			rw.label("CONVERT() style " + tsqlStyleText(c.ConvertStyle) + " (output format not emulated)")
		}
	case tv == tsqlValTime:
		if src != tsqlValText {
			// The style only describes a string input.
			return out
		}
		if !isLit {
			rw.label("CONVERT() with a computed style")
			return out
		}
		switch style.Text {
		case "101", "103", "104", "105", "110", "111":
			out.Expr = rw.fc("to_timestamp", c.P, c.Expr, ast.BuildStringLit(tsqlConvertStyles[style.Text]))
		case "0", "112", "120", "121", "126", "127", "23":
			// ISO forms (and the default) PG parses as they are.
		default:
			rw.label("CONVERT() style " + style.Text + " (input format not emulated)")
		}
	}
	return out
}

func tsqlStyleText(e ast.Expr) string {
	if lit, ok := e.(*ast.Literal); ok {
		return lit.Text
	}
	return "(computed)"
}

func mustAtoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// castType maps a CAST / CONVERT target. A character type without a
// length has length 30 in a CAST / CONVERT (1 in a declaration, which is
// what the column mapper assumes).
func (rw *tsqlRW) castType(dt ast.DataType) string {
	if ch, ok := dt.(*ast.CharType); ok && !ch.HasLength && !ch.Max {
		cp := *ch
		cp.HasLength, cp.Length = true, 30
		return rw.mapType(&cp)
	}
	return rw.mapType(dt)
}

// tsqlUnparen strips redundant parentheses (catalog expressions are
// written `((0))`).
func tsqlUnparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.Inner
	}
}

// boolifyTSQL turns a 0 / 1 literal (bit) into a boolean literal when the
// value goes to a boolean (a bit column / variable mapped to boolean):
// PG has no implicit integer → boolean conversion.
func boolifyTSQL(e ast.Expr, pgType string) ast.Expr {
	if e == nil || pgTypeVal(pgType) != tsqlValBool {
		return e
	}
	lit, ok := ast.IsLiteralKind(tsqlUnparen(e), "number")
	if !ok {
		if s, isStr := ast.IsLiteralKind(tsqlUnparen(e), "string"); isStr && (s.Text == "0" || s.Text == "1") {
			lit = s
		} else {
			return e
		}
	}
	switch lit.Text {
	case "0":
		return &ast.Literal{Kind: "bool", Text: "false", P: lit.P}
	case "1":
		return &ast.Literal{Kind: "bool", Text: "true", P: lit.P}
	}
	return e
}

// ---------------------------------------------------------------------------
// Value classes
// ---------------------------------------------------------------------------

// tsqlVal is the value class of a PG-shaped expression, as far as the
// rewrite can prove it.
type tsqlVal int

const (
	tsqlValUnknown tsqlVal = iota
	tsqlValText
	tsqlValNumber
	tsqlValBool
	tsqlValTime
	tsqlValInterval
	tsqlValBinary
	tsqlValNull
	tsqlValOther
)

// pgTypeVal classifies a PG type text (as MapType renders it). Domains
// and composite types are unknown.
func pgTypeVal(typ string) tsqlVal {
	base := typ
	for i := 0; i < len(typ); i++ {
		if typ[i] == '(' {
			base = typ[:i]
			break
		}
	}
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "text", "varchar", "character varying", "char", "character", "bpchar", "citext", "name":
		return tsqlValText
	case "integer", "int", "int4", "bigint", "int8", "smallint", "int2", "numeric", "decimal",
		"real", "double precision", "float4", "float8", "money":
		return tsqlValNumber
	case "boolean", "bool":
		return tsqlValBool
	case "date", "timestamp", "timestamptz", "timestamp with time zone", "timestamp without time zone", "time", "timetz":
		return tsqlValTime
	case "interval":
		return tsqlValInterval
	case "bytea":
		return tsqlValBinary
	case "uuid", "xml", "jsonb", "json":
		return tsqlValOther
	}
	return tsqlValUnknown
}

// exprVal classifies a PG-shaped expression.
func (rw *tsqlRW) exprVal(e ast.Expr) tsqlVal {
	switch x := e.(type) {
	case nil:
		return tsqlValUnknown
	case *ast.Literal:
		switch x.Kind {
		case "string":
			return tsqlValText
		case "number", "money":
			return tsqlValNumber
		case "null":
			return tsqlValNull
		case "bool":
			return tsqlValBool
		case "hex":
			return tsqlValBinary
		}
	case *ast.Ident:
		return pgTypeVal(rw.columnType(x))
	case *ast.ParenExpr:
		return rw.exprVal(x.Inner)
	case *ast.CastExpr:
		if t, ok := x.Type.(*ast.PGType); ok {
			return pgTypeVal(t.Name)
		}
	case *ast.IntervalLit:
		return tsqlValInterval
	case *ast.FuncCall:
		name := strings.ToLower(x.Name)
		switch {
		case tsqlPGTextFuncs[name]:
			return tsqlValText
		case tsqlPGNumberFuncs[name]:
			return tsqlValNumber
		case tsqlPGTimeFuncs[name]:
			return tsqlValTime
		case name == "coalesce" || name == "nullif" || name == "greatest" || name == "least" || name == "min" || name == "max":
			return rw.commonVal(x.Args)
		case name == "gen_random_uuid":
			return tsqlValOther
		}
	case *ast.WindowedAgg:
		if x.Func != nil {
			return rw.exprVal(x.Func)
		}
	case *ast.CaseExpr:
		vals := make([]ast.Expr, 0, len(x.Whens)+1)
		for _, w := range x.Whens {
			vals = append(vals, w.Then)
		}
		if x.Else != nil {
			vals = append(vals, x.Else)
		}
		return rw.commonVal(vals)
	case *ast.SubqueryExpr:
		if x.Stmt != nil && len(x.Stmt.Cols) == 1 && x.Stmt.Cols[0].Expr != nil && len(x.Stmt.SetOps) == 0 {
			return rw.exprVal(x.Stmt.Cols[0].Expr)
		}
	case *ast.UnaryExpr:
		if strings.EqualFold(x.Op, "NOT") {
			return tsqlValBool
		}
		return rw.exprVal(x.Rhs)
	case *ast.BinaryExpr:
		switch strings.ToUpper(x.Op) {
		case "||":
			return tsqlValText
		case "+", "-":
			l, r := rw.exprVal(x.Lhs), rw.exprVal(x.Rhs)
			switch {
			case l == tsqlValNumber && r == tsqlValNumber:
				return tsqlValNumber
			case l == tsqlValTime || r == tsqlValTime:
				if l == tsqlValTime && r == tsqlValTime {
					return tsqlValInterval
				}
				return tsqlValTime
			}
		case "*", "/", "%", "&", "|", "#":
			if rw.exprVal(x.Lhs) == tsqlValNumber && rw.exprVal(x.Rhs) == tsqlValNumber {
				return tsqlValNumber
			}
		case "=", "<>", "!=", "<", "<=", ">", ">=", "AND", "OR", "LIKE", "NOT LIKE", "IS", "IS NOT":
			return tsqlValBool
		}
	case *ast.InExpr, *ast.BetweenExpr, *ast.ExistsExpr:
		return tsqlValBool
	}
	return tsqlValUnknown
}

// commonVal is the class shared by every non-NULL value of vals, unknown
// when they disagree.
func (rw *tsqlRW) commonVal(vals []ast.Expr) tsqlVal {
	out := tsqlValUnknown
	for _, v := range vals {
		c := rw.exprVal(v)
		if c == tsqlValNull {
			continue
		}
		if c == tsqlValUnknown || (out != tsqlValUnknown && out != c) {
			return tsqlValUnknown
		}
		out = c
	}
	return out
}

// ---------------------------------------------------------------------------
// Operators
// ---------------------------------------------------------------------------

// tsqlLikePatternNeedsShim reports whether a LIKE pattern literal uses
// what T-SQL and PG LIKE read differently: a [set] / [a-z] / [^set]
// class (T-SQL only) or a backslash (PG's default escape character). The
// pattern is a LIKE pattern (not SQL), scanned rune by rune.
func tsqlLikePatternNeedsShim(pattern string) bool {
	for _, r := range pattern {
		if r == '[' || r == '\\' {
			return true
		}
	}
	return false
}

// visitOperators rewrites T-SQL operators:
//
//	'a' + b, N'x' + upper(c) → 'a' || b     (concatenation proven text)
//	d + 1 (d datetime)       → d + 1 * INTERVAL '1 day'
//	a ^ b                    → a # b         (T-SQL ^ is bitwise XOR)
//	~a                       → -a - 1        (bitwise NOT, two's complement)
//	flags & 0x0F             → flags & 15
//	bitcol = 1               → bitcol = true (bit columns are boolean)
//	s LIKE '[A-Z]%'          → tsql.like(s, '[A-Z]%')
//	s NOT LIKE p ESCAPE '!'  → NOT tsql.like(s, p, '!')
//
// A `+` neither operand of which is provably text, number or date is
// kept and reported (T-SQL decides concatenation vs addition from the
// operand types, which the rewrite could not establish). Comparisons
// involving text are noted: SQL Server's default collation is
// case-insensitive, PG's is not.
func (rw *tsqlRW) visitOperators(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.UnaryExpr:
		if x.Op != "~" {
			return n
		}
		if hx, ok := tsqlHexInt(x.Rhs); ok {
			return tsqlPrec(ast.BuildBinary("-", &ast.UnaryExpr{Op: "-", Rhs: hx}, ast.BuildIntLit(1)))
		}
		return tsqlPrec(ast.BuildBinary("-", &ast.UnaryExpr{Op: "-", Rhs: parenIfCompound(x.Rhs), P: x.P}, ast.BuildIntLit(1)))
	case *ast.InExpr:
		if rw.exprVal(x.Expr) != tsqlValBool || len(x.List) == 0 {
			if rw.exprVal(x.Expr) == tsqlValText {
				rw.collationNote()
			}
			return n
		}
		cp := *x
		cp.List = make([]ast.Expr, len(x.List))
		for i, e := range x.List {
			cp.List[i] = boolifyTSQL(e, "boolean")
		}
		return &cp
	case *ast.BinaryExpr:
		return rw.binary(x)
	}
	return n
}

func (rw *tsqlRW) collationNote() {
	rw.note("SQL Server default collation is case-insensitive; PostgreSQL compares text case-sensitively (=, <, LIKE, IN, GROUP BY, DISTINCT): check the comparisons on text values")
}

func (rw *tsqlRW) binary(x *ast.BinaryExpr) ast.Node {
	op := strings.ToUpper(x.Op)
	switch op {
	case "+":
		l, r := rw.exprVal(x.Lhs), rw.exprVal(x.Rhs)
		switch {
		case (l == tsqlValText && r != tsqlValNumber && r != tsqlValTime) ||
			(r == tsqlValText && l != tsqlValNumber && l != tsqlValTime):
			cp := *x
			cp.Op = "||"
			return &cp
		case l == tsqlValText || r == tsqlValText:
			rw.label("string + number (SQL Server converts the string to a number)")
		case l == tsqlValTime && r == tsqlValNumber:
			return rw.addDays(x, x.Lhs, x.Rhs)
		case r == tsqlValTime && l == tsqlValNumber:
			return rw.addDays(x, x.Rhs, x.Lhs)
		case l == tsqlValUnknown && r == tsqlValUnknown:
			rw.label("string concatenation with + could not be proven text (kept as +)")
		}
		return rw.hexOperands(x)
	case "-":
		if rw.exprVal(x.Lhs) == tsqlValTime && rw.exprVal(x.Rhs) == tsqlValNumber {
			return rw.addDays(x, x.Lhs, x.Rhs)
		}
		return rw.hexOperands(x)
	case "^":
		cp := *x
		cp.Op = "#"
		return rw.hexOperands(&cp)
	case "&", "|", "*", "/", "%":
		return rw.hexOperands(x)
	case "=", "<>", "!=":
		l, r := rw.exprVal(x.Lhs), rw.exprVal(x.Rhs)
		if l == tsqlValBool || r == tsqlValBool {
			lhs, rhs := x.Lhs, x.Rhs
			if l == tsqlValBool {
				rhs = boolifyTSQL(rhs, "boolean")
			}
			if r == tsqlValBool {
				lhs = boolifyTSQL(lhs, "boolean")
			}
			if lhs == x.Lhs && rhs == x.Rhs {
				return x
			}
			cp := *x
			cp.Lhs, cp.Rhs = lhs, rhs
			return &cp
		}
		if l == tsqlValText || r == tsqlValText {
			rw.collationNote()
		}
	case "<", "<=", ">", ">=":
		if rw.exprVal(x.Lhs) == tsqlValText || rw.exprVal(x.Rhs) == tsqlValText {
			rw.collationNote()
		}
	case "LIKE", "NOT LIKE":
		return rw.like(x, op == "NOT LIKE")
	case "AT TIME ZONE":
		if lit, ok := ast.IsLiteralKind(x.Rhs, "string"); !ok || !strings.EqualFold(lit.Text, "UTC") {
			rw.label("AT TIME ZONE with a Windows time-zone name (PG needs IANA names)")
		}
	}
	return x
}

// addDays rewrites T-SQL `datetime ± number` (days) into interval
// arithmetic.
func (rw *tsqlRW) addDays(x *ast.BinaryExpr, d, n ast.Expr) ast.Node {
	rw.note("datetime ± number → interval arithmetic (the number counts days)")
	days := ast.BuildBinary("*", parenIfCompound(n), &ast.IntervalLit{Value: "1 day"})
	return tsqlPrec(&ast.BinaryExpr{Op: x.Op, Lhs: d, Rhs: days, P: x.P})
}

// hexOperands turns binary-literal operands of an integer operator into
// their integer value.
func (rw *tsqlRW) hexOperands(x *ast.BinaryExpr) ast.Node {
	l, lok := tsqlHexInt(x.Lhs)
	r, rok := tsqlHexInt(x.Rhs)
	if !lok && !rok {
		return x
	}
	cp := *x
	if lok {
		cp.Lhs = l
	}
	if rok {
		cp.Rhs = r
	}
	return &cp
}

// like rewrites a LIKE whose T-SQL meaning PG LIKE does not share.
func (rw *tsqlRW) like(x *ast.BinaryExpr, not bool) ast.Node {
	pat, isLit := ast.IsLiteralKind(x.Rhs, "string")
	if isLit && x.Escape == nil && !tsqlLikePatternNeedsShim(pat.Text) {
		rw.collationNote()
		return x
	}
	args := []ast.Expr{rw.textify(x.Lhs), rw.textify(x.Rhs)}
	if x.Escape != nil {
		args = append(args, x.Escape)
	}
	var out ast.Expr = rw.fc("tsql.like", x.P, args...)
	if not {
		out = &ast.UnaryExpr{Op: "NOT", Rhs: out, P: x.P}
	}
	return out
}

// ---------------------------------------------------------------------------
// Query shapes
// ---------------------------------------------------------------------------

// visitSelectShape rewrites the T-SQL query clauses:
//
//	SELECT TOP (n) …                 → … LIMIT n
//	SELECT TOP 100 PERCENT … ORDER BY → … ORDER BY (TOP dropped: no-op)
//	OPTION (…)                       → dropped (query hints)
//
// TOP … WITH TIES becomes Limit with TopWithTies kept (the writer emits
// FETCH FIRST n ROWS WITH TIES, PG 13+); TOP …
// PERCENT stays on the node for the translator's residue report.
// INSERT / UPDATE / DELETE TOP (n), OUTPUT clauses, INSERT … EXEC and
// SELECT … INTO a new table have no PG statement form and are reported.
func (rw *tsqlRW) visitSelectShape(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.SelectStmt:
		cp := *x
		changed := false
		if x.OptionClause != "" {
			cp.OptionClause = ""
			changed = true
		}
		if x.Top != nil {
			switch {
			case x.TopPercent:
				if lit, ok := ast.IsLiteralKind(tsqlUnparen(x.Top), "number"); ok && (lit.Text == "100" || lit.Text == "100.0") {
					cp.Top, cp.TopPercent = nil, false
					changed = true
				}
			case x.Limit == nil:
				cp.Limit = tsqlUnparen(x.Top)
				cp.Top = nil
				changed = true
				if len(x.SetOps) > 0 {
					rw.label("TOP in a UNION / EXCEPT / INTERSECT query (LIMIT applies to the whole result on PG)")
				}
			}
		}
		for _, so := range x.SetOps {
			if so.Stmt != nil && (so.Stmt.Limit != nil || so.Stmt.Top != nil) {
				rw.label("TOP in a UNION / EXCEPT / INTERSECT branch")
			}
		}
		if x.IntoTable != nil {
			rw.label("SELECT … INTO a new table")
		}
		if !changed {
			return n
		}
		return &cp
	case *ast.InsertStmt:
		if x.Top != nil {
			rw.label("INSERT TOP (n)")
		}
		if x.Output != "" {
			rw.label("INSERT … OUTPUT clause")
		}
		if x.Exec != nil {
			rw.label("INSERT … EXEC")
		}
	case *ast.UpdateStmt:
		if x.Top != nil {
			rw.label("UPDATE TOP (n) (PG UPDATE has no row limit)")
		}
		if x.Output != "" {
			rw.label("UPDATE … OUTPUT clause")
		}
		if x.OptionClause != "" {
			cp := *x
			cp.OptionClause = ""
			return &cp
		}
	case *ast.DeleteStmt:
		if x.Top != nil {
			rw.label("DELETE TOP (n) (PG DELETE has no row limit)")
		}
		if x.Output != "" {
			rw.label("DELETE … OUTPUT clause")
		}
		if x.OptionClause != "" {
			cp := *x
			cp.OptionClause = ""
			return &cp
		}
	case *ast.MergeStmt:
		if x.Top != nil {
			rw.label("MERGE TOP (n)")
		}
		if x.Output != "" {
			rw.label("MERGE … OUTPUT clause")
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Residual idioms
// ---------------------------------------------------------------------------

// tsqlResidualIdioms lists, in tree order and without duplicates, the
// T-SQL constructs still present in a rewritten tree that would reach PG
// unchanged (or be dropped by the writer): untranslated built-ins, CLR /
// XML methods, PIVOT, FOR XML / JSON, TOP … PERCENT / WITH TIES, SELECT
// … INTO, OUTPUT, @@ERROR and other system variables, sqlcmd variables,
// and `+` operands of unknown type. Labels found during a context-aware
// rewrite (FORMAT, CONVERT styles, …) are returned by that rewrite
// instead; this walker only sees what is left on the tree. The routine
// translator handles @@ROWCOUNT, @@FETCH_STATUS and @@TRANCOUNT, which
// are not reported here.
func tsqlResidualIdioms(n ast.Node) []string {
	return tsqlStatic.residual(n, false)
}

// residual is tsqlResidualIdioms with rw's column types (a `+` proven by
// the rewrite is not reported). coreReported skips the clauses the
// translator reports itself on view / TVF queries (FOR XML, TOP
// PERCENT).
func (rw *tsqlRW) residual(n ast.Node, coreReported bool) []string {
	if n == nil {
		return nil
	}
	v := &tsqlResidualVisitor{rw: rw, seen: map[string]bool{}, core: coreReported}
	ast.Walk(v, n)
	return v.out
}

type tsqlResidualVisitor struct {
	rw   *tsqlRW
	out  []string
	seen map[string]bool
	core bool
}

func (v *tsqlResidualVisitor) add(label string) {
	if v.seen[label] {
		return
	}
	v.seen[label] = true
	v.out = append(v.out, label)
}

func (v *tsqlResidualVisitor) Visit(n ast.Node) ast.Visitor {
	switch x := n.(type) {
	case *ast.FuncCall:
		if l, ok := tsqlResidualFuncs[strings.ToLower(x.Name)]; ok {
			v.add(l)
		}
	case *ast.MethodCall:
		target := x.StaticType
		if target == "" {
			target = "column"
		}
		v.add("CLR method " + target + "." + x.Method + "() (hierarchyid / spatial / xml type methods have no PG equivalent)")
	case *ast.RawExpr:
		if x.Kind == "XML_METHOD" {
			v.add("XML data-type method (value / query / exist / nodes / modify)")
		} else if x.Kind != "" {
			v.add(x.Kind + " clause")
		}
	case *ast.FromPivot:
		if x.Unpivot {
			v.add("UNPIVOT")
		} else {
			v.add("PIVOT")
		}
	case *ast.SelectStmt:
		if x.ForXML != "" && !v.core {
			v.add("FOR " + x.ForXMLKind + " clause")
		}
		if x.Top != nil && x.TopPercent && !v.core {
			v.add("TOP … PERCENT")
		}
		if x.TopWithTies && (x.Limit == nil || len(x.OrderBy) == 0) {
			// The writer renders FETCH FIRST … WITH TIES only for a
			// converted TOP with its ORDER BY.
			v.add("TOP … WITH TIES")
		}
		if x.IntoTable != nil {
			v.add("SELECT … INTO a new table")
		}
	case *ast.InsertStmt:
		if x.Output != "" {
			v.add("INSERT … OUTPUT clause")
		}
		if x.Top != nil {
			v.add("INSERT TOP (n)")
		}
	case *ast.UpdateStmt:
		if x.Output != "" {
			v.add("UPDATE … OUTPUT clause")
		}
		if x.Top != nil {
			v.add("UPDATE TOP (n) (PG UPDATE has no row limit)")
		}
	case *ast.DeleteStmt:
		if x.Output != "" {
			v.add("DELETE … OUTPUT clause")
		}
		if x.Top != nil {
			v.add("DELETE TOP (n) (PG DELETE has no row limit)")
		}
	case *ast.MergeStmt:
		if x.Output != "" {
			v.add("MERGE … OUTPUT clause")
		}
	case *ast.Ident:
		if len(x.Parts) == 1 && x.Variable && isTSQLSystemVar(x.Parts[0]) {
			switch strings.ToUpper(x.Parts[0]) {
			case "@@ROWCOUNT", "@@FETCH_STATUS", "@@TRANCOUNT":
			default:
				v.add(strings.ToUpper(x.Parts[0]) + " system variable")
			}
		}
		if len(x.Parts) == 1 && len(x.Parts[0]) > 2 && x.Parts[0][0] == '$' && x.Parts[0][1] == '(' {
			v.add("sqlcmd variable " + x.Parts[0])
		}
	case *ast.BinaryExpr:
		if x.Op == "+" && v.rw.exprVal(x.Lhs) == tsqlValUnknown && v.rw.exprVal(x.Rhs) == tsqlValUnknown {
			v.add("string concatenation with + could not be proven text (kept as +)")
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// Shim usage
// ---------------------------------------------------------------------------

// recordTSQLShimUse records on run every tsql.* shim function the
// rewritten tree calls (detected on the AST: FuncCall names), so the
// pre-actions provider emits exactly those (tsql_shim.go). A routine
// body translator that rewrites with RewriteTSQLAST must call it on its
// output; the context-aware rewriters do it themselves.
func recordTSQLShimUse(run *HookRun, n ast.Node) {
	if run == nil || n == nil {
		return
	}
	ast.Walk(tsqlShimUseVisitor{run: run}, n)
}

type tsqlShimUseVisitor struct{ run *HookRun }

func (v tsqlShimUseVisitor) Visit(n ast.Node) ast.Visitor {
	if fc, ok := n.(*ast.FuncCall); ok && tsqlShimFuncs[fc.Name] != nil {
		v.run.Use(fc.Name)
	}
	return v
}

// inlineTVFParam returns the PG reference of a parameter of the inline
// table-valued function whose RETURN query is rewritten — qualified with
// the function name (`"fn"."customer_id"`), or nil outside one. In a
// LANGUAGE sql function an unqualified name that is both a column and a
// parameter means the column, so T-SQL's usual `WHERE o.customer_id =
// @customer_id` would compare the column with itself; the qualified
// spelling always names the parameter (and follows a renamed one).
func (rw *tsqlRW) inlineTVFParam(name string) []string {
	if rw.ctx == nil || rw.ctx.Routine == nil || rw.ctx.Routine.Kind != RoutineInlineTVF {
		return nil
	}
	p := rw.ctx.Routine.Param(name)
	if p == nil {
		return nil
	}
	return []string{rw.ctx.Routine.Name, p.Name}
}

// boolifyTSQLProjection is the INSERT … SELECT counterpart of the VALUES
// boolify: a 0 / 1 literal projected into a boolean (bit) column becomes
// false / true, in every branch of a set operation. sel is copied, never
// modified.
func boolifyTSQLProjection(sel *ast.SelectStmt, targetCols []string, cols map[string]string) *ast.SelectStmt {
	branch := func(s *ast.SelectStmt) *ast.SelectStmt {
		if s == nil {
			return nil
		}
		cp := *s
		cp.Cols = append([]ast.SelectItem(nil), s.Cols...)
		for j := range cp.Cols {
			if j < len(targetCols) && !cp.Cols[j].Star && cp.Cols[j].Expr != nil {
				cp.Cols[j].Expr = boolifyTSQL(cp.Cols[j].Expr, cols[targetCols[j]])
			}
		}
		return &cp
	}
	out := branch(sel)
	if len(sel.SetOps) > 0 {
		out.SetOps = make([]ast.SetOp, len(sel.SetOps))
		for i, op := range sel.SetOps {
			op.Stmt = branch(op.Stmt)
			out.SetOps[i] = op
		}
	}
	return out
}
