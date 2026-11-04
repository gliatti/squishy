package translate

import (
	"sort"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// dialect_hooks.go — per-source-kind extension points of the translator.
//
// The translator core (translator.go, plsql_xlate.go) owns the object
// shapes: tables, columns, indexes, views, routine headers, triggers,
// schemas. What happens INSIDE an expression, a view query or a routine
// body is dialect knowledge. For MySQL / MariaDB and Oracle that
// knowledge is wired inline (historical code paths, unchanged). A newer
// dialect plugs its own passes in through the registries below instead
// of adding branches to translator.go — the same idea as
// RegisterTypeMapper. Registration happens in init() of the dialect's
// own files (e.g. tsql_visitors.go, tsql_routines.go); the last
// registration for a kind wins.
//
// Every hook works on the typed AST (internal/sqlparse/ast) and never on
// SQL text: CLAUDE.md "No regex anywhere — AST only" applies to hooks as
// to the rest of the package. The rendered PG text is produced by the
// internal/dialects/postgres writers from the rewritten AST.
//
// A kind with nothing registered gets identity behaviour: expressions,
// selects and bodies pass through unchanged (and are rendered by the PG
// writer as they are). SQL Server routine bodies default to the T-SQL
// parser (tsqldialect.ParseRoutineBody) so a body is never handed to the
// MySQL parser by accident.

// SchemaMap maps a source schema name (T-SQL `dbo`, `Sales`, … as
// written in the source, any case) to the PostgreSQL schema the
// translator materialises that schema's objects in. An empty source
// schema means the source default (dbo for SQL Server). It is the same
// function the translator uses for every object it emits, so a hook that
// qualifies a reference with it always names the schema the object was
// created in.
type SchemaMap func(srcSchema string) string

// HookRun is the per-Translate state shared by every hook call of one
// Translate run. Hooks must not keep state in package variables (plans
// are built concurrently by the API): what a hook needs to remember
// across calls of the same run (e.g. "a shim function was emitted")
// goes through Use / Used.
type HookRun struct {
	// Kind is the source dialect of the run.
	Kind dialects.Kind
	// Schema is the run's schema map (see SchemaMap).
	Schema SchemaMap
	// Caps are the target capabilities (extensions, user types emitted
	// so far by the translator).
	Caps Caps

	t    *translator
	used map[string]bool
	// noted de-duplicates diagnostics per run (firstTime), apart from
	// the feature set so Features lists only features.
	noted map[string]bool
}

// Use records that the translated output needs feature (a free-form
// key chosen by the hook, e.g. "tsql.like"). A PreActionsProvider reads
// it back with Used / Features to emit only the runtime helpers the
// translated objects actually call.
func (r *HookRun) Use(feature string) {
	if r.used == nil {
		r.used = map[string]bool{}
	}
	r.used[feature] = true
}

// Used reports whether Use(feature) was called during this run.
func (r *HookRun) Used(feature string) bool { return r.used[feature] }

// Features returns every feature recorded by Use, sorted.
func (r *HookRun) Features() []string {
	out := make([]string, 0, len(r.used))
	for f := range r.used {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// firstTime reports whether key is seen for the first time in this run
// (and records it): hooks use it to emit a diagnostic once per object
// and run.
func (r *HookRun) firstTime(key string) bool {
	if r.noted[key] {
		return false
	}
	if r.noted == nil {
		r.noted = map[string]bool{}
	}
	r.noted[key] = true
	return true
}

// Warn reports a diagnostic on object. kind selects the prerequisite
// template in buildPrerequisites (e.g. "routine.untranslated_construct"
// for a blocking manual review, "view.functions"); sev SeverityInfo
// makes it informational.
func (r *HookRun) Warn(object, kind, msg string, sev Severity) {
	if r.t == nil {
		return
	}
	r.t.warnSev(object, kind, msg, sev)
}

// Explain records an explanation bullet (shown in the wizard).
func (r *HookRun) Explain(e Explanation) {
	if r.t == nil {
		return
	}
	r.t.res.Explanations = append(r.t.res.Explanations, e)
}

// Ident folds ONE source identifier to the PG name the translator gives
// it (SQL Server: lower-case, see normalizeTSQLIdent). Other kinds keep
// the identifier as is.
func (r *HookRun) Ident(s string) string {
	if dialects.IsSQLServer(r.Kind) {
		return normalizeTSQLIdent(s)
	}
	return s
}

// ResolveRelation returns the PG schema and name of the relation (table,
// view, function, procedure, sequence, type) a source reference names.
// srcSchema is the schema written in the reference ("" when unqualified)
// and defaultSchema the source schema of the object holding the
// reference (a module's own schema). An unqualified reference resolves
// as SQL Server resolves it inside a module: the module's schema when
// the object exists there in the migrated script, else dbo when it
// exists there, else the module's schema.
func (r *HookRun) ResolveRelation(srcSchema, name, defaultSchema string) (pgSchema, pgName string) {
	pgName = r.Ident(name)
	if srcSchema != "" {
		return r.Schema(srcSchema), pgName
	}
	if r.t != nil {
		for _, cand := range []string{defaultSchema, "dbo"} {
			if cand == "" {
				continue
			}
			if r.t.sourceObjects[sourceObjectKey(cand, name)] {
				return r.Schema(cand), pgName
			}
		}
	}
	return r.Schema(defaultSchema), pgName
}

// TableColumns returns the PG column name → PG type map of a table
// already translated in this run (pgSchema / pgTable are PG names), or
// nil when the run has no such table; a column of a T-SQL alias type
// reports its domain's base type. Tables are translated before
// views and routines, so view and routine hooks see every table.
func (r *HookRun) TableColumns(pgSchema, pgTable string) map[string]string {
	if r.t == nil {
		return nil
	}
	for i := range r.t.res.Plan.Tables {
		tbl := &r.t.res.Plan.Tables[i]
		if tbl.Schema == pgSchema && tbl.Name == pgTable {
			cols := pgColumnTypes(tbl)
			if st := r.t.tsql; st != nil {
				// A column of an alias type is typed with its domain;
				// hooks classify values by base type (text `+`, bit).
				for c, typ := range cols {
					if base, ok := st.domainBase[typ]; ok {
						cols[c] = base
					}
				}
			}
			return cols
		}
	}
	return nil
}

// MapType renders a source data type as its PG type text, with the
// run's capabilities (alias types resolve to their domains).
func (r *HookRun) MapType(dt ast.DataType) string {
	if dt == nil {
		return ""
	}
	return MapType(r.Kind, dt, "", r.Caps).PG
}

// pgColumnTypes returns the PG column name → PG type map of tbl.
func pgColumnTypes(tbl *PGTable) map[string]string {
	out := make(map[string]string, len(tbl.Columns))
	for _, c := range tbl.Columns {
		out[c.Name] = c.Type
	}
	return out
}

// ExprCtx is the context of one expression / query / body rewrite.
type ExprCtx struct {
	Run *HookRun
	// Object names the owner for diagnostics ("table.dbo.t",
	// "view.sales.v", "procedure.dbo.p", …).
	Object string
	// DefaultSchema is the source schema of the owning object: the
	// schema an unqualified reference resolves against first
	// (Run.ResolveRelation).
	DefaultSchema string
	// Columns is the PG column name → PG type map of the table a DDL
	// expression (DEFAULT, CHECK, computed column, index predicate)
	// belongs to; nil for views and routines.
	Columns map[string]string
	// Routine is set when the rewrite happens inside a routine (body,
	// inline table-valued function query): parameters, return shape,
	// trigger context.
	Routine *RoutineCtx
}

// ExprRewriter rewrites a source expression into its PG-shaped AST
// (function renames, identifier folding, schema mapping, operator
// rewrites). Used for table DEFAULTs, CHECKs, computed columns, index
// predicates and alias-type defaults. It must return a non-nil
// expression for a non-nil input (the input itself when nothing
// applies). Constructs it cannot translate are reported through
// ctx.Run.Warn (they are rendered as they are by the PG writer).
// Rewriters may modify the input tree in place (ast.Rewrite does): the
// translator takes whatever it needs from the source expression before
// the call. The same holds for SelectRewriter and BodyRewriter.
type ExprRewriter func(e ast.Expr, ctx *ExprCtx) ast.Expr

// SelectRewriter rewrites a typed view query (or inline table-valued
// function query) into its PG-shaped AST. The returned labels name the
// source-specific constructs left untranslated; each one becomes part of
// a blocking "view.functions" warning of the view (or a blocking
// routine.untranslated_construct of the function).
type SelectRewriter func(s *ast.SelectStmt, ctx *ExprCtx) (*ast.SelectStmt, []string)

// BodyRewriter rewrites a parsed routine body on the AST before the
// generic PL translator (plTranslator) renders it. When a kind registers
// one, the MySQL-only passes (applyMySQLASTRewrites,
// foldMySQLLocalVarRefs, applyMySQLCallArgCasts, mysqlResidualIdioms) are
// skipped for that kind. ctx is nil when the body is translated outside
// a Translate run (TranslateRoutineBody* entry points).
type BodyRewriter func(stmts []ast.PLStmt, ctx *ExprCtx) []ast.PLStmt

// RoutineBodyParser parses a raw routine body into PL statements; the
// string is the parser diagnostics ("" when it parsed cleanly).
type RoutineBodyParser func(body string) ([]ast.PLStmt, string)

// RoutineBodyTranslator produces the complete PL/pgSQL body (the text
// between the dollar quotes: `[DECLARE …] BEGIN … END;`) of a routine
// whose header the translator built (RoutineCtx). warnings become
// blocking routine.untranslated_construct prerequisites, notes
// info-level remarks. A trigger body must end by returning
// ctx.Trigger.ReturnRow. When no translator is registered for the kind,
// the generic path (RoutineBodyParser + BodyRewriter + plTranslator)
// renders the body.
type RoutineBodyTranslator func(ctx *RoutineCtx) (body string, warnings, notes []string)

// PreActionsProvider returns DDL statements (each a complete statement,
// `;`-terminated) the target needs before any table is created — runtime
// helper functions (the T-SQL shim schema), types… It is called once per
// Translate run, after every object was translated, so it can emit only
// what run.Used says the translated objects call. Its statements are
// placed first in Plan.PreActions.
type PreActionsProvider func(run *HookRun) []string

// RoutineKind is the shape of a routine handed to the hooks.
type RoutineKind string

const (
	RoutineProcedure      RoutineKind = "procedure"
	RoutineScalarFunction RoutineKind = "scalar_function"
	// RoutineInlineTVF: `RETURNS TABLE AS RETURN (SELECT …)`.
	RoutineInlineTVF RoutineKind = "inline_tvf"
	// RoutineMultiTVF: `RETURNS @t TABLE (…) AS BEGIN … END`.
	RoutineMultiTVF RoutineKind = "multi_tvf"
	RoutineTrigger  RoutineKind = "trigger"
)

// RoutineParam is one parameter of a routine header as emitted on PG.
type RoutineParam struct {
	// SourceName is the source spelling (T-SQL keeps the @).
	SourceName string
	// Name is the PG parameter name (T-SQL: @ stripped, lower-cased —
	// see tsqlParamName). Body references to the parameter must use it.
	Name string
	// Type is the PG type text.
	Type string
	// Mode is "IN" or "INOUT" (T-SQL OUTPUT parameters).
	Mode string
	// Default is the rendered PG default ("" when none).
	Default string
	// Param is the parsed source parameter.
	Param ast.Param
}

// RoutineColumn is one column of a routine's result table (table-valued
// functions) as emitted on PG.
type RoutineColumn struct {
	SourceName string
	Name       string // PG name
	Type       string // PG type
}

// TriggerInfo describes the PG trigger a trigger body runs in.
type TriggerInfo struct {
	// Timing is "AFTER" or "INSTEAD OF" (T-SQL FOR is AFTER).
	Timing string
	// Events are the DML events ("INSERT", "UPDATE", "DELETE").
	Events []string
	// ForEach is "STATEMENT" (AFTER: transition tables) or "ROW"
	// (INSTEAD OF: PG only has row-level INSTEAD OF triggers).
	ForEach string
	// OnView is true for a trigger on a view.
	OnView bool
	// Schema / Table are the PG schema and name of the relation.
	Schema string
	Table  string
	// NewTable / OldTable are the transition tables declared by
	// REFERENCING ("inserted" / "deleted"), "" when not applicable. A
	// row-level trigger has none: inserted / deleted are then the NEW /
	// OLD rows.
	NewTable string
	OldTable string
	// ReturnRow is what the trigger function returns: "NULL"
	// (statement-level), "NEW" or "OLD" (INSTEAD OF row triggers).
	ReturnRow string
	// UpdatedColumns is filled by the body translator: the PG columns a
	// statement-level UPDATE trigger tests with UPDATE(col), each of
	// which needs a helper `AFTER UPDATE OF col` trigger marking the
	// statement (see tsqlBody.updatedMark).
	UpdatedColumns []string
}

// RoutineCtx is the header context of one routine handed to
// RoutineBodyTranslator (and, through ExprCtx.Routine, to the rewriters).
type RoutineCtx struct {
	Run  *HookRun
	Kind RoutineKind
	// Object names the routine for diagnostics ("procedure.dbo.p").
	Object string
	// SourceSchema / SourceName as written in the source; Schema / Name
	// the PG ones (SchemaMap + identifier fold).
	SourceSchema string
	SourceName   string
	Schema       string
	Name         string
	// Body is the raw source body (T-SQL: the text after AS up to the
	// batch end).
	Body string
	// Params are the emitted parameters, in order.
	Params []RoutineParam
	// Returns is the PG return type of a scalar function ("" otherwise).
	Returns string
	// TableVar is the result table variable of a multi-statement TVF
	// (source spelling, with its @) and TableCols its PG columns — the
	// function is emitted RETURNS TABLE (TableCols…), so the body must
	// RETURN QUERY / RETURN NEXT rows of that shape.
	TableVar  string
	TableCols []RoutineColumn
	// Trigger is set for RoutineTrigger.
	Trigger *TriggerInfo
}

// Expr returns the ExprCtx of a rewrite inside this routine.
func (c *RoutineCtx) Expr() *ExprCtx {
	return &ExprCtx{Run: c.Run, Object: c.Object, DefaultSchema: c.SourceSchema, Routine: c}
}

// Param returns the parameter whose source name is name (compared
// case-insensitively, as T-SQL compares variable names), or nil.
func (c *RoutineCtx) Param(name string) *RoutineParam {
	for i := range c.Params {
		if strings.EqualFold(c.Params[i].SourceName, name) {
			return &c.Params[i]
		}
	}
	return nil
}

var (
	routineBodyParsers     = map[dialects.Kind]RoutineBodyParser{}
	bodyRewriters          = map[dialects.Kind]BodyRewriter{}
	selectRewriters        = map[dialects.Kind]SelectRewriter{}
	exprRewriters          = map[dialects.Kind]ExprRewriter{}
	routineBodyTranslators = map[dialects.Kind]RoutineBodyTranslator{}
	preActionsProviders    = map[dialects.Kind]PreActionsProvider{}
)

// RegisterRoutineBodyParser sets the routine-body parser of kind
// (parseRoutineBodyByKind). MySQL / MariaDB / DB2 and Oracle keep their
// built-in parsers; registering one for them is not supported.
func RegisterRoutineBodyParser(k dialects.Kind, fn RoutineBodyParser) {
	routineBodyParsers[k] = fn
}

// RegisterBodyRewriter sets the routine-body AST pass of kind.
func RegisterBodyRewriter(k dialects.Kind, fn BodyRewriter) { bodyRewriters[k] = fn }

// RegisterSelectRewriter sets the view-query AST pass of kind.
func RegisterSelectRewriter(k dialects.Kind, fn SelectRewriter) { selectRewriters[k] = fn }

// RegisterExprRewriter sets the DDL-expression AST pass of kind.
func RegisterExprRewriter(k dialects.Kind, fn ExprRewriter) { exprRewriters[k] = fn }

// RegisterRoutineBodyTranslator sets the routine-body translator of kind.
func RegisterRoutineBodyTranslator(k dialects.Kind, fn RoutineBodyTranslator) {
	routineBodyTranslators[k] = fn
}

// RegisterPreActionsProvider sets the pre-action provider of kind.
func RegisterPreActionsProvider(k dialects.Kind, fn PreActionsProvider) {
	preActionsProviders[k] = fn
}

// hookedRoutineBodyParser returns the routine-body parser registered for
// kind, the T-SQL parser for SQL Server when none is, or nil (the
// built-in MySQL / Oracle paths of parseRoutineBodyByKind).
func hookedRoutineBodyParser(k dialects.Kind) RoutineBodyParser {
	if fn := routineBodyParsers[k]; fn != nil {
		return fn
	}
	if dialects.IsSQLServer(k) {
		return parseTSQLRoutineBody
	}
	return nil
}

// parseTSQLRoutineBody is the default SQL Server routine-body parser.
func parseTSQLRoutineBody(body string) ([]ast.PLStmt, string) {
	stmts, errs := tsqldialect.ParseRoutineBody(body)
	if len(errs) == 0 {
		return stmts, ""
	}
	return stmts, errs.Error()
}

// hookedBodyRewriter returns the body rewriter of kind, or nil.
func hookedBodyRewriter(k dialects.Kind) BodyRewriter { return bodyRewriters[k] }

// usesHookedBodies reports whether kind's routine bodies go through the
// hook path of translateRoutineBodyWithSigs (no MySQL-only passes): a
// kind with a registered body rewriter, and SQL Server always.
func usesHookedBodies(k dialects.Kind) bool {
	return bodyRewriters[k] != nil || dialects.IsSQLServer(k)
}

// rewriteHookedExpr runs kind's expression rewriter over e (identity
// when none is registered).
func rewriteHookedExpr(k dialects.Kind, e ast.Expr, ctx *ExprCtx) ast.Expr {
	if e == nil {
		return nil
	}
	if fn := exprRewriters[k]; fn != nil {
		if out := fn(e, ctx); out != nil {
			return out
		}
	}
	return e
}

// rewriteHookedSelect runs kind's select rewriter over s (identity when
// none is registered).
func rewriteHookedSelect(k dialects.Kind, s *ast.SelectStmt, ctx *ExprCtx) (*ast.SelectStmt, []string) {
	if s == nil {
		return nil, nil
	}
	if fn := selectRewriters[k]; fn != nil {
		out, labels := fn(s, ctx)
		if out != nil {
			return out, labels
		}
		return s, labels
	}
	return s, nil
}

// hookedRoutineBodyTranslator returns the routine-body translator of
// kind, or nil.
func hookedRoutineBodyTranslator(k dialects.Kind) RoutineBodyTranslator {
	return routineBodyTranslators[k]
}

// hookedPreActions returns the pre-actions of kind's provider for run
// (nil when none is registered).
func hookedPreActions(k dialects.Kind, run *HookRun) []string {
	if fn := preActionsProviders[k]; fn != nil {
		return fn(run)
	}
	return nil
}

// sourceObjectKey is the key of a source object in
// translator.sourceObjects: folded schema (dbo when empty) + "." +
// folded name.
func sourceObjectKey(schema, name string) string {
	if schema == "" {
		schema = "dbo"
	}
	return normalizeTSQLIdent(schema) + "." + normalizeTSQLIdent(name)
}
