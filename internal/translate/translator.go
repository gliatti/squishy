package translate

import (
	"fmt"
	"sort"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Options tunes the translator.
type Options struct {
	TargetSchema string // PG schema to materialize everything into (default "public")

	// SourceKind identifies the source dialect so the type mapper can pick the
	// right rule set. Empty defaults to MySQL (historical behavior).
	SourceKind dialects.Kind

	// TargetExtensions lists extensions installed on the target PG at plan
	// time. The translator uses this to decide whether it can emit native
	// types/calls (geometry, cron.schedule) or must fall back to placeholders
	// plus a blocking prerequisite telling the user to install the extension.
	//
	// Typical members: "postgis", "pg_cron", "pgcrypto", "citext", "vector".
	TargetExtensions []string
}

// hasExt reports whether opt.TargetExtensions contains ext (case-insensitive).
func (o Options) hasExt(ext string) bool {
	for _, e := range o.TargetExtensions {
		if strings.EqualFold(e, ext) {
			return true
		}
	}
	return false
}

// Result is what Translate returns: a SchemaPlan ready for persistence and
// emission to the wizard, plus DDL strings split into pre-copy and post-copy
// scripts (post-copy holds indexes + FKs, created after the bulk data load).
type Result struct {
	Plan          SchemaPlan     `json:"plan"`
	DDLScript     string         `json:"ddl_script"`
	DDLPostCopy   string         `json:"ddl_post_script"`
	Explanations  []Explanation  `json:"explanations"`
	Warnings      []Warning      `json:"warnings"`
	TypeMappings  []TypeMapping  `json:"type_mappings"`
	Prerequisites []Prerequisite `json:"prerequisites"`
}

// Translate walks a list of MySQL AST statements and produces the PG migration
// artifacts. It never fails: errors are collected as Warnings so the wizard can
// surface them while still allowing the user to proceed with a best-effort
// migration plan.
func Translate(stmts []ast.Stmt, opt Options) *Result {
	if opt.TargetSchema == "" {
		opt.TargetSchema = "public"
	}
	t := &translator{opt: opt, res: &Result{}}
	t.res.Plan.PostActions = []string{}
	t.res.Plan.TargetExtensions = append([]string(nil), opt.TargetExtensions...)
	// Pre-pass: register every CREATE TYPE the dump contains so that table
	// columns referencing those types (CUSTOMERS.cust_address →
	// CUST_ADDRESS_TYP) resolve to the migrated PG composite/array even if
	// the table appears earlier in the input than the type. The httpapi
	// concatenates Tables before Events, so without this pre-pass column
	// resolution always saw an empty userTypes registry.
	t.preRegisterUserTypes(stmts)
	t.preCollectPackageVars(stmts)
	t.preCollectTriggerTables(stmts)
	// emittedTypes tracks which CREATE TYPE we've already pushed into
	// Plan.PreActions during the dependency walk, so the main-loop
	// translateCreateType call doesn't double-emit. We pre-emit composite
	// types in topo order (a type that references another composite as an
	// attribute must be created after its dependency) — without this, a
	// dump that lists e.g. ACTIONS_T (which has a field of type ACTION_V →
	// ACTION_T[]) before ACTION_T would fail at apply time with
	// `type "mig.action_t[]" does not exist`.
	t.emittedTypes = map[string]bool{}
	t.emitTypesInTopoOrder(stmts)
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.CreateTable:
			t.translateTable(x)
		case *ast.CreateTableLike:
			t.translateTableLike(x)
		case *ast.CreateTableAs:
			t.translateTableAs(x)
		case *ast.DropObject:
			t.translateDropObject(x)
		case *ast.TruncateTable:
			t.translateTruncate(x)
		case *ast.RenameTable:
			t.translateRenameTable(x)
		case *ast.CreateView:
			t.translateView(x)
		case *ast.CreateTrigger:
			t.translateTrigger(x)
		case *ast.CreateProcedure:
			t.translateProcedure(x)
		case *ast.CreateFunction:
			t.translateFunction(x)
		case *ast.CreateEvent:
			t.translateEvent(x)
		case *ast.CreateSequence:
			t.translateSequence(x)
		case *ast.AlterSequence:
			t.translateAlterSequence(x)
		case *ast.AlterIndex:
			t.translateAlterIndex(x)
		case *ast.AlterTrigger:
			// Buffer the AlterTrigger; the actual emission runs after the
			// main loop so Plan.Routines is fully populated by every
			// CreateTrigger regardless of dump order. Without this, an
			// ALTER TRIGGER appearing before its matching CREATE in the
			// dump fell back to PreActions, which run BEFORE the CREATE
			// TABLE statements — and PG raised "relation does not exist".
			t.pendingAlterTriggers = append(t.pendingAlterTriggers, x)
		case *ast.AlterView:
			t.translateAlterView(x)
		case *ast.AlterType:
			t.translateAlterType(x)
		case *ast.AlterRoutine:
			t.translateAlterRoutine(x)
		case *ast.CreateType:
			t.translateCreateType(x)
		case *ast.CreateTypeBody:
			t.translateCreateTypeBody(x)
		case *ast.NoopStmt:
			t.translateNoop(x)
		case *ast.CreateIndex:
			t.translateCreateIndex(x)
		case *ast.AlterTable:
			// DBMS_METADATA emits the PK/UQ for non-IOT tables as a separate
			// `ALTER TABLE … ADD CONSTRAINT … PRIMARY KEY (...) USING INDEX …`
			// statement following the CREATE TABLE. Merge those into the
			// already-collected PGTable so that the PG CREATE TABLE carries
			// the PK and downstream FKs find a unique constraint to match.
			t.translateAlterTable(x)
		}
	}
	// Drain the pending AlterTrigger buffer now that every CreateTrigger
	// has populated Plan.Routines and triggerTable.
	for _, at := range t.pendingAlterTriggers {
		t.translateAlterTrigger(at)
	}
	t.harmonizeFKTypes()
	t.buildDDL()
	t.buildPrerequisites()
	return t.res
}

// harmonizeFKTypes aligns the PG type of FK source columns with their
// referenced PK column type. Oracle lets a NUMBER (arbitrary precision) FK
// reference a NUMBER(p,0) PK because Oracle promotes both to the same
// internal type, but in PostgreSQL NUMERIC and INTEGER are distinct types
// and `cannot be implemented` for an FK. We rewrite the FK column type to
// match the referenced column type so the post-copy ADD FK succeeds.
//
// We only adjust the FK source column. The referenced column drives the type
// because it is the PK / UQ side that owns the constraint's semantics.
func (t *translator) harmonizeFKTypes() {
	tableByName := make(map[string]*PGTable, len(t.res.Plan.Tables))
	for i := range t.res.Plan.Tables {
		tbl := &t.res.Plan.Tables[i]
		tableByName[strings.ToLower(tbl.Name)] = tbl
	}
	for _, fk := range t.res.Plan.ForeignKeys {
		srcTbl := tableByName[strings.ToLower(fk.Table)]
		refTbl := tableByName[strings.ToLower(fk.RefTable)]
		if srcTbl == nil || refTbl == nil {
			continue
		}
		if len(fk.Columns) != len(fk.RefColumns) {
			continue
		}
		for i, srcCol := range fk.Columns {
			refColName := fk.RefColumns[i]
			srcIdx := columnIndex(srcTbl.Columns, srcCol)
			refIdx := columnIndex(refTbl.Columns, refColName)
			if srcIdx < 0 || refIdx < 0 {
				continue
			}
			if srcTbl.Columns[srcIdx].Type == refTbl.Columns[refIdx].Type {
				continue
			}
			old := srcTbl.Columns[srcIdx].Type
			srcTbl.Columns[srcIdx].Type = refTbl.Columns[refIdx].Type
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: srcTbl.Name + "." + srcCol,
				Source: old,
				Target: refTbl.Columns[refIdx].Type,
				Reason: fmt.Sprintf("FK column %s.%s narrowed/widened from %s to %s to match %s.%s (PG requires identical types across an FK)",
					srcTbl.Name, srcCol, old, refTbl.Columns[refIdx].Type, refTbl.Name, refColName),
				Level: "info",
			})
		}
	}
}

func columnIndex(cols []PGColumn, name string) int {
	for i, c := range cols {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

type translator struct {
	opt Options
	res *Result
	// triggerTable maps a normalised (lowercase, unschema'd) trigger name
	// to the table it was created on. Populated by translateTrigger so
	// translateAlterTrigger can resolve `ALTER TRIGGER name ENABLE/DISABLE/
	// RENAME` into PG's table-aware form (`ALTER TABLE tbl ENABLE TRIGGER
	// name` / `ALTER TRIGGER name ON tbl RENAME TO …`).
	triggerTable map[string]string
	// userTypes records every CREATE TYPE this translation emits, keyed by
	// the source-side bare type name in lowercase. Populated by
	// translateCreateType; consulted via caps() so the type mapper can swap
	// column references like `OE.CUST_ADDRESS_TYP` for the migrated PG
	// composite or array form.
	userTypes map[string]UserTypeRef
	// objectAttrs caches the parser's CreateType.Attributes per Oracle type
	// name (lowercased, schema-stripped). Lets translateCreateType resolve
	// `UNDER parent` subtypes by inheriting the parent's attribute list —
	// PG has no composite-type inheritance, so we inline the chain at
	// emit time.
	objectAttrs map[string]objectTypeInfo
	// emittedTypes is the set of CREATE TYPE source names already pushed
	// into Plan.PreActions, keyed via userTypeKey. Populated by
	// emitTypesInTopoOrder; consulted by translateCreateType to avoid
	// double-emitting once the main loop reaches the same statement.
	emittedTypes map[string]bool
	// packageVars maps a lowercased Oracle package name to its declared
	// session-scoped variables. Populated by preCollectPackageVars before
	// any routine body is translated; consulted from TranslateRoutineBody
	// (via t.translateRoutineBody) so `<pkg>.<var>` references in
	// procedures, functions, triggers, views, and type bodies are
	// rewritten to `current_setting('squishy.<pkg>.<var>', true)::<type>`
	// (read) or `PERFORM set_config(...)` (write).
	packageVars map[string][]packageVar
	// usedAdminpack is set when any routine body's rewrite emitted a
	// `pg_file_write(...)` call (translated from Oracle UTL_FILE.PUT_LINE
	// / PUT / PUTF / NEW_LINE). Drives a blocking prerequisite that
	// instructs the operator to install the `adminpack` extension on the
	// target database before applying the routines, since pg_file_write
	// lives there.
	usedAdminpack bool
	// pendingAlterTriggers buffers every AlterTrigger encountered during
	// the main loop. They're drained AFTER the loop so the routine
	// lookup sees a fully populated Plan.Routines, regardless of the
	// dump's CREATE/ALTER ordering.
	pendingAlterTriggers []*ast.AlterTrigger
}

type objectTypeInfo struct {
	ParentType string
	Attributes []ast.ColumnDef
}

// skippedTriggerMarker is stored as the "table" entry in triggerTable
// when translateTrigger decides to drop a CREATE TRIGGER (XDB-internal,
// nested-table INSTEAD OF, owning relation absent from the plan).
// translateAlterTrigger reads this back to convert the matching `ALTER
// TRIGGER name ENABLE` (emitted by DBMS_METADATA as a separate
// statement) into an info-level explanation rather than a blocking
// "resolve the table" prereq.
const skippedTriggerMarker = "__skipped__"

// rememberTrigger records the (trigger → table) association so a later
// ALTER TRIGGER in the same dump can be re-emitted with the table name PG
// requires. Idempotent — last write wins, which matches Oracle's "the most
// recent CREATE OR REPLACE TRIGGER" semantics.
func (t *translator) rememberTrigger(trigger, table string) {
	if t.triggerTable == nil {
		t.triggerTable = make(map[string]string)
	}
	t.triggerTable[strings.ToLower(trigger)] = table
}

// preCollectTriggerTables walks the entire statement list and seeds the
// trigger→table map from every CreateTrigger before the main translation
// loop. DBMS_METADATA dumps occasionally emit `ALTER TRIGGER name ENABLE`
// alongside (or before) the matching CREATE TRIGGER; without this pass
// the ALTER would be processed first, find an empty registry, and surface
// a blocking "Résoudre la table" prerequisite for what is in fact a
// resolvable construct.
//
// We populate the map with the trigger's table name in PG-friendly form
// (normalised through the Oracle ident folding when applicable). The
// main-loop translateTrigger still calls rememberTrigger to overwrite
// the entry with the final emitted name, so this pre-pass is a
// lower-priority seed.
func (t *translator) preCollectTriggerTables(stmts []ast.Stmt) {
	for _, s := range stmts {
		ct, ok := s.(*ast.CreateTrigger)
		if !ok {
			continue
		}
		tbl := ct.Table.Name
		if dialects.IsOracle(t.opt.SourceKind) {
			tbl = normalizeOracleIdent(tbl)
		}
		if tbl == "" {
			continue
		}
		name := ct.Name
		if dialects.IsOracle(t.opt.SourceKind) {
			name = normalizeOracleIdent(name)
		}
		t.rememberTrigger(name, tbl)
	}
}

// caps snapshots the translator's capability flags derived from
// opt.TargetExtensions — used by MapType and event translation to decide
// between native PG types/calls and placeholders + blocking prereqs.
func (t *translator) caps() Caps {
	return Caps{
		HasPostGIS:  t.opt.hasExt("postgis"),
		HasPgCron:   t.opt.hasExt("pg_cron"),
		HasPgVector: t.opt.hasExt("vector") || t.opt.hasExt("pgvector"),
		UserTypes:   t.userTypes,
	}
}

// preRegisterUserTypes walks the entire statement list and seeds the
// userTypes registry from every CREATE TYPE found, before the main
// per-statement translation loop runs. The httpapi concatenates source
// snapshots Tables→Views→Triggers→Procedures→Functions→Events, so types
// (which live under Events) would otherwise be processed AFTER the tables
// that reference them — leaving the column-type resolver with an empty
// registry and forcing every user-typed column to fall back to TEXT.
//
// Only the registry is populated here; the actual DDL emission still
// happens during the main pass when we hit the CreateType statement.
func (t *translator) preRegisterUserTypes(stmts []ast.Stmt) {
	// First sweep: stash every OBJECT type's attribute list so the second
	// sweep (and translateCreateType) can resolve `UNDER parent` chains by
	// inheriting parent attrs.
	for _, s := range stmts {
		ct, ok := s.(*ast.CreateType)
		if !ok {
			continue
		}
		if !strings.EqualFold(ct.Kind, "OBJECT") {
			continue
		}
		key := userTypeKey(ct.Name)
		if t.objectAttrs == nil {
			t.objectAttrs = make(map[string]objectTypeInfo)
		}
		t.objectAttrs[key] = objectTypeInfo{
			ParentType: ct.ParentType,
			Attributes: ct.Attributes,
		}
	}
	// Second sweep: register the migrated PG name (composite or array form)
	// so column-type resolution at table-translation time can swap the
	// reference. We call flattenObjectAttrs here so subtypes that *only*
	// inherit attrs (no own ones) still register as composites.
	for _, s := range stmts {
		ct, ok := s.(*ast.CreateType)
		if !ok {
			continue
		}
		schema := t.opt.TargetSchema
		if dialects.IsOracle(t.opt.SourceKind) {
			schema = normalizeOracleIdent(schema)
		}
		name := ct.Name
		if dialects.IsOracle(t.opt.SourceKind) {
			name = normalizeOracleIdent(name)
		}
		switch strings.ToUpper(ct.Kind) {
		case "OBJECT":
			if len(t.flattenObjectAttrs(ct)) > 0 {
				t.registerUserType(ct.Name, UserTypeRef{
					Kind:   "composite",
					Schema: schema,
					Name:   name,
				})
			}
		case "VARRAY", "TABLE":
			if elemPG, kind := t.renderCollectionElement(ct.ElementType); elemPG != "" {
				t.registerUserType(ct.Name, UserTypeRef{
					Kind:   kind,
					ElemPG: elemPG,
				})
			}
		}
	}
}

// emitTypesInTopoOrder walks every CREATE TYPE in the input and pushes the
// corresponding DDL into Plan.PreActions in dependency order: a composite is
// emitted only after every other composite it references through one of
// its attributes (directly, or via an array_composite VARRAY/TABLE OF). PG
// has no shell-composite-type forward declaration, so the apply-time
// `CREATE TYPE foo AS (bar foo_elem[])` would otherwise fail with `type
// foo_elem does not exist` whenever the dump's natural order wasn't
// already correct.
func (t *translator) emitTypesInTopoOrder(stmts []ast.Stmt) {
	type typeStmt struct {
		st *ast.CreateType
		// deps is the set of userTypeKey()-ed names this composite depends
		// on (other composites referenced as field types).
		deps map[string]bool
	}
	byKey := map[string]*typeStmt{}
	for _, s := range stmts {
		ct, ok := s.(*ast.CreateType)
		if !ok {
			continue
		}
		// Only OBJECT types emit DDL — VARRAY/TABLE OF are anonymous
		// arrays at use sites. Skip them in the topo walk.
		if !strings.EqualFold(ct.Kind, "OBJECT") {
			continue
		}
		k := userTypeKey(ct.Name)
		ts := &typeStmt{st: ct, deps: map[string]bool{}}
		// Parent of an UNDER subtype: depends on parent so we can inline
		// its attributes via flattenObjectAttrs (parent must be in
		// objectAttrs by now — preRegisterUserTypes runs first).
		if ct.ParentType != "" {
			ts.deps[userTypeKey(ct.ParentType)] = true
		}
		// Attribute types: any attr whose type is itself a registered
		// composite must be created first. We resolve via the type-mapper
		// using the in-progress userTypes registry; if the attr maps to
		// `"mig"."<name>"` (composite) or `"mig"."<name>"[]`
		// (array_composite), record the name as a dep.
		for _, attr := range t.flattenObjectAttrs(ct) {
			r := MapType(t.opt.SourceKind, attr.Type, attr.Name, t.caps())
			pg := r.PG
			pg = strings.TrimSuffix(pg, "[]")
			if dep, ok := compositeKeyFromPG(pg); ok && dep != k {
				ts.deps[dep] = true
			}
		}
		byKey[k] = ts
	}
	// Stable topo sort: visit each node, recursing into deps first.
	visited := map[string]bool{}
	var visit func(k string)
	visit = func(k string) {
		if visited[k] {
			return
		}
		ts, ok := byKey[k]
		if !ok {
			return
		}
		visited[k] = true
		// Stable order: walk deps in sorted name order so the emitted
		// DDL is deterministic across runs.
		var depKeys []string
		for d := range ts.deps {
			depKeys = append(depKeys, d)
		}
		sort.Strings(depKeys)
		for _, d := range depKeys {
			visit(d)
		}
		t.emitCompositeFor(ts.st)
	}
	// Walk in input order so independent types stay close to where they
	// appeared in the dump.
	for _, s := range stmts {
		ct, ok := s.(*ast.CreateType)
		if !ok || !strings.EqualFold(ct.Kind, "OBJECT") {
			continue
		}
		visit(userTypeKey(ct.Name))
	}
}

// emitCompositeFor pushes the CREATE TYPE … AS (...) DDL for a single
// OBJECT type into Plan.PreActions and marks the source type as emitted so
// the main-loop translateCreateType skips re-emission. Side-effects on
// res.Explanations / res.Prerequisites are deferred to translateCreateType
// so the per-type explanations land in the same order as the rest of the
// translation pipeline.
func (t *translator) emitCompositeFor(s *ast.CreateType) {
	key := userTypeKey(s.Name)
	if t.emittedTypes[key] {
		return
	}
	attrs := t.flattenObjectAttrs(s)
	if len(attrs) == 0 {
		return
	}
	schema := t.opt.TargetSchema
	if dialects.IsOracle(t.opt.SourceKind) {
		schema = normalizeOracleIdent(schema)
	}
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	qname := fmt.Sprintf("%s.%s", quoteIdent(schema), quoteIdent(name))
	parts := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		attrName := attr.Name
		if dialects.IsOracle(t.opt.SourceKind) {
			attrName = normalizeOracleIdent(attrName)
		}
		typeRes := MapType(t.opt.SourceKind, attr.Type, attr.Name, t.caps())
		parts = append(parts, fmt.Sprintf("%s %s", quoteIdent(attrName), typeRes.PG))
	}
	ddl := fmt.Sprintf("CREATE TYPE %s AS (%s);", qname, strings.Join(parts, ", "))
	t.res.Plan.PreActions = append(t.res.Plan.PreActions, ddl)
	t.emittedTypes[key] = true
}

// compositeKeyFromPG extracts the bare composite-type name from a PG type
// expression of the form `"mig"."foo"` (or with a trailing `[]`, already
// stripped by the caller). Returns ok=false for scalar / non-composite
// expressions like `varchar(20)` or `numeric`.
func compositeKeyFromPG(pg string) (string, bool) {
	pg = strings.TrimSpace(pg)
	if pg == "" {
		return "", false
	}
	// Look for the last `"<schema>"."<name>"` shape — the composite ones we
	// emit always go through pgQualified() and quote both segments.
	if !strings.HasPrefix(pg, `"`) {
		return "", false
	}
	parts := strings.Split(strings.Trim(pg, `"`), `"."`)
	if len(parts) != 2 {
		return "", false
	}
	return strings.ToLower(strings.Trim(parts[1], `"`)), true
}

// userTypeKey normalises a (possibly schema-qualified, possibly quoted)
// Oracle type identifier to the lowercase bare name we use as registry key.
func userTypeKey(raw string) string {
	k := strings.Trim(raw, `"`)
	if i := strings.LastIndex(k, "."); i >= 0 {
		k = strings.Trim(k[i+1:], `"`)
	}
	return strings.ToLower(k)
}

// flattenObjectAttrs returns the attribute list for an OBJECT type with the
// `UNDER parent` chain inlined: parent attrs first, then own attrs. PG
// composite types have no inheritance — we materialise the full attribute
// list at emit time so a subtype is queryable without relying on a parent
// composite. Cycle-safe via a visited set (Oracle rejects cycles, but a
// malformed dump shouldn't loop us forever).
func (t *translator) flattenObjectAttrs(s *ast.CreateType) []ast.ColumnDef {
	if t.objectAttrs == nil {
		return append([]ast.ColumnDef(nil), s.Attributes...)
	}
	visited := map[string]bool{}
	var collect func(name string, own []ast.ColumnDef, parent string) []ast.ColumnDef
	collect = func(name string, own []ast.ColumnDef, parent string) []ast.ColumnDef {
		if name != "" {
			if visited[name] {
				return own
			}
			visited[name] = true
		}
		var attrs []ast.ColumnDef
		if parent != "" {
			pkey := userTypeKey(parent)
			if pinfo, ok := t.objectAttrs[pkey]; ok {
				attrs = collect(pkey, pinfo.Attributes, pinfo.ParentType)
			}
		}
		attrs = append(attrs, own...)
		return attrs
	}
	return collect(userTypeKey(s.Name), s.Attributes, s.ParentType)
}

// registerUserType records that a CREATE TYPE was emitted earlier in the
// migration so that subsequent column-type references (CUSTOMERS.cust_address
// → CUST_ADDRESS_TYP) resolve to the migrated PG name instead of falling
// back to TEXT + warning. Names are stored lowercase to match the
// case-insensitive Oracle lookup convention.
func (t *translator) registerUserType(srcName string, ref UserTypeRef) {
	if t.userTypes == nil {
		t.userTypes = make(map[string]UserTypeRef)
	}
	key := strings.ToLower(strings.Trim(srcName, `"`))
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = strings.Trim(key[i+1:], `"`)
	}
	t.userTypes[key] = ref
}

// mysqlEveryToCron converts `EVERY N UNIT` to an approximate cron expression.
// pg_cron uses standard 5-field cron syntax. For units that don't map cleanly
// (EVERY 2 HOUR, EVERY 15 MINUTE) we emit a best-effort expression.
func mysqlEveryToCron(n int64, unit string) string {
	unit = strings.ToUpper(unit)
	if n <= 0 {
		n = 1
	}
	switch unit {
	case "MINUTE":
		if n == 1 {
			return "* * * * *"
		}
		return fmt.Sprintf("*/%d * * * *", n)
	case "HOUR":
		if n == 1 {
			return "0 * * * *"
		}
		return fmt.Sprintf("0 */%d * * *", n)
	case "DAY":
		if n == 1 {
			return "0 3 * * *"
		}
		return fmt.Sprintf("0 3 */%d * *", n)
	case "WEEK":
		return "0 3 * * 1"
	case "MONTH":
		if n == 1 {
			return "0 3 1 * *"
		}
		return fmt.Sprintf("0 3 1 */%d *", n)
	case "SECOND":
		// pg_cron supports seconds via a different syntax; fall back to every minute.
		return "* * * * *"
	}
	return "0 3 * * *"
}

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

func (t *translator) translateTable(s *ast.CreateTable) {
	if dialects.IsOracle(t.opt.SourceKind) {
		normalizeOracleTable(s)
	}
	tbl := PGTable{Schema: t.opt.TargetSchema, Name: s.Name, Comment: s.Options.Comment}

	// MariaDB `CREATE OR REPLACE TABLE` has no PG counterpart. Emit a
	// `DROP TABLE IF EXISTS … CASCADE` pre-action so dependent objects don't
	// fail the recreation.
	if s.OrReplace {
		t.res.Plan.PreActions = append(t.res.Plan.PreActions,
			fmt.Sprintf("DROP TABLE IF EXISTS %s.%s CASCADE;",
				quoteIdent(t.opt.TargetSchema), quoteIdent(s.Name)))
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: s.Name,
			Source: "CREATE OR REPLACE TABLE",
			Target: "DROP TABLE IF EXISTS … CASCADE; CREATE TABLE …",
			Reason: "PostgreSQL has no OR REPLACE for tables; DROP IF EXISTS + CREATE preserves the source intent. CASCADE is needed because dependent FKs/views would otherwise block the DROP.",
			Level:  "info",
		})
	}

	// MariaDB SYSTEM VERSIONING period columns (`GENERATED ALWAYS AS ROW
	// START|END`) have no PG equivalent and are marked STORED GENERATED by the
	// source, which means the data copier (`internal/dataxfer`) filters them
	// out of the SELECT list. Keeping them in the PG DDL therefore yields
	// NOT-NULL violations on COPY. Drop them here, and exclude them from any
	// primary key that references them.
	dropped := map[string]bool{}
	for _, c := range s.Columns {
		if c.SystemVersioning {
			dropped[c.Name] = true
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: s.Name + "." + c.Name,
				Source: "GENERATED ALWAYS AS ROW START|END (SYSTEM VERSIONING)",
				Target: "(dropped)",
				Reason: "MariaDB system-versioning period columns have no PG equivalent. Dropped from target; remove from any PK/index that references them.",
				Level:  "warn",
			})
		}
	}

	// PERIOD FOR (start_col, end_col) declarations that don't carry a
	// matching ROW START/END column (e.g. application-time periods, or a
	// SYSTEM_TIME period with explicit timestamp columns) — surface them too
	// so the user knows the temporal semantics aren't replicated.
	for _, pd := range s.Periods {
		kind := "PERIOD FOR " + pd.Name
		if strings.EqualFold(pd.Name, "SYSTEM_TIME") {
			// Folded into the system-versioning prereq below; just record
			// the column pair for traceability.
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: s.Name,
				Source: kind + " (" + pd.StartCol + ", " + pd.EndCol + ")",
				Target: "(dropped)",
				Reason: "PostgreSQL has no PERIOD FOR clause. The (start, end) timestamp columns are kept as plain columns; rebuild row-versioning logic via triggers or the temporal_tables extension.",
				Level:  "warn",
			})
		} else {
			// Application-time period (MD-03 territory): no temporal semantics
			// in PG. Document and move on.
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: s.Name,
				Source: kind + " (" + pd.StartCol + ", " + pd.EndCol + ")",
				Target: "(dropped)",
				Reason: "PostgreSQL has no application-time PERIOD FOR clause. The two columns remain as plain columns; range-overlap exclusion (and FOR PORTION OF) must be rebuilt with EXCLUDE constraints + tstzrange.",
				Level:  "warn",
			})
			t.warn(s.Name, "table.application_period",
				"PERIOD FOR "+pd.Name+" ("+pd.StartCol+", "+pd.EndCol+") has no PG equivalent")
		}
	}

	// `WITH SYSTEM VERSIONING` (MariaDB 10.3+): no native equivalent in PG.
	// Emit a blocking prerequisite so the user explicitly chooses a remediation
	// path (history table + triggers, or the temporal_tables extension) before
	// the migration silently loses temporal history.
	if s.SystemVersioned {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: s.Name,
			Source: "WITH SYSTEM VERSIONING",
			Target: "(plain table — temporal history NOT replicated)",
			Reason: "PostgreSQL has no native system-versioning. The current row is migrated as a plain table; historical rows (the system-versioned shadow table in MariaDB) and the AS OF / FOR SYSTEM_TIME query semantics are NOT carried over.",
			Level:  "warn",
		})
		t.warn(s.Name, "table.system_versioning",
			"WITH SYSTEM VERSIONING has no PG equivalent — temporal history not replicated")
	}

	// Columns
	for _, c := range s.Columns {
		if dropped[c.Name] {
			continue
		}
		pgCol, expl, mappings := t.translateColumn(s.Name, c)
		tbl.Columns = append(tbl.Columns, pgCol)
		t.res.Explanations = append(t.res.Explanations, expl...)
		t.res.TypeMappings = append(t.res.TypeMappings, mappings...)
		// Inline PK in column def
		if c.PrimaryKey {
			tbl.PK = append(tbl.PK, c.Name)
		}
	}

	// Collect PK/UQ/FK from table constraints
	for _, cons := range s.Constraints {
		switch c := cons.(type) {
		case *ast.PKConstraint:
			if t.dropDisabledConstraint(s.Name, "PRIMARY KEY", c.Name, c.State) {
				continue
			}
			t.warnUnsupportedConstraintState(s.Name, "PRIMARY KEY", c.Name, c.State)
			for _, col := range c.Columns {
				if dropped[col.Name] {
					continue
				}
				tbl.PK = append(tbl.PK, col.Name)
			}
		case *ast.UQConstraint:
			if t.dropDisabledConstraint(s.Name, "UNIQUE", c.Name, c.State) {
				continue
			}
			t.warnUnsupportedConstraintState(s.Name, "UNIQUE", c.Name, c.State)
			// Unique via index (created post-copy)
			name := c.Name
			if name == "" {
				name = fmt.Sprintf("uq_%s_%s", s.Name, joinCols(c.Columns))
			}
			t.res.Plan.Indexes = append(t.res.Plan.Indexes, PGIndex{
				Schema: t.opt.TargetSchema, Table: s.Name, Name: name,
				Unique: true, Columns: columnsNames(c.Columns),
			})
		case *ast.FKConstraint:
			if t.dropDisabledConstraint(s.Name, "FOREIGN KEY", c.Name, c.State) {
				continue
			}
			name := c.Name
			if name == "" {
				name = fmt.Sprintf("fk_%s_%s", s.Name, strings.Join(c.Columns, "_"))
			}
			// DBMS_METADATA qualifies every FK reference with the source owner
			// ("SQUISHY"."CUSTOMERS") — we always migrate into a single
			// target schema, so any captured source-schema prefix must be
			// replaced, not preserved.
			refSchema := t.opt.TargetSchema
			if c.RefSchema != "" && !dialects.IsOracle(t.opt.SourceKind) {
				refSchema = c.RefSchema
			}
			t.res.Plan.ForeignKeys = append(t.res.Plan.ForeignKeys, PGForeignKey{
				Schema: t.opt.TargetSchema, Table: s.Name, Name: name,
				Columns: c.Columns, RefSchema: refSchema, RefTable: c.RefTable,
				RefColumns: c.RefColumns, OnDelete: c.OnDelete, OnUpdate: c.OnUpdate,
				NotValid:          c.State.NoValidate,
				Deferrable:        c.State.Deferrable,
				InitiallyDeferred: c.State.InitiallyDeferred,
			})
			if c.State.Rely {
				t.res.Explanations = append(t.res.Explanations, Explanation{
					Object: s.Name + "." + name,
					Source: "FOREIGN KEY … RELY",
					Target: "(no PG equivalent)",
					Reason: "Oracle RELY tells the cost-based optimiser to trust an unenforced or unvalidated constraint when rewriting queries (e.g. join elimination). PostgreSQL's planner has no equivalent hint.",
					Level:  "info",
				})
			}
		case *ast.CheckConstraint:
			if t.dropDisabledConstraint(s.Name, "CHECK", c.Name, c.State) {
				continue
			}
			t.warnUnsupportedConstraintState(s.Name, "CHECK", c.Name, c.State)
			tbl.Checks = append(tbl.Checks, rawExpr(c.Expr))
			if !c.Enforced {
				// MariaDB CHECK ... NOT ENFORCED is parsed but not actually
				// validated. PG has no NOT ENFORCED equivalent — every CHECK
				// is enforced. Surface this so existing rows that violate the
				// CHECK (legal in MariaDB) don't blindside the user at COPY time.
				t.res.Explanations = append(t.res.Explanations, Explanation{
					Object: s.Name,
					Source: "CHECK ... NOT ENFORCED",
					Target: "CHECK (always enforced in PG)",
					Reason: "MariaDB allows NOT ENFORCED to keep the CHECK as documentation only; PostgreSQL enforces every CHECK. Existing rows that violated the constraint in MariaDB will fail to COPY into PG.",
					Level:  "warn",
				})
			}
		}
	}

	// Secondary indexes
	for _, idx := range s.Indexes {
		name := idx.Name
		if name == "" {
			name = fmt.Sprintf("idx_%s_%s", s.Name, joinCols(idx.Columns))
		}
		cols, dirs, isExpr, hadDir, hadExpr := indexColumnPayload(idx.Columns)
		pgIdx := PGIndex{
			Schema: t.opt.TargetSchema, Table: s.Name, Name: name,
			Columns: cols, Using: strings.ToLower(idx.Using),
		}
		if hadDir {
			pgIdx.ColumnDirs = dirs
		}
		if hadExpr {
			pgIdx.ColumnIsExpr = isExpr
		}
		// MySQL prefix length `col(N)` has no PG equivalent — drop with a
		// note so the user can decide whether to add a functional
		// `LEFT(col, N)` index manually.
		for _, c := range idx.Columns {
			if c.PrefixLen > 0 {
				t.warn(s.Name, "index.prefix",
					fmt.Sprintf("INDEX %s: prefix length on %s(%d) dropped; PG indexes the full value. Consider a functional index `(LEFT(%s, %d))` if storage matters.",
						name, c.Name, c.PrefixLen, c.Name, c.PrefixLen))
			}
		}
		if idx.Invisible {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: s.Name + "." + name,
				Source: "INVISIBLE",
				Target: "(no PG equivalent)",
				Reason: "MySQL INVISIBLE keeps the index up to date but hides it from the planner; PG has no equivalent. Drop the index manually if you want the same effect.",
				Level:  "info",
			})
		}
		switch idx.Kind {
		case "FULLTEXT":
			stmt := buildFulltextIndexDDL(t.opt.TargetSchema, s.Name, name, pgIdx.Columns)
			t.res.Plan.PostActions = append(t.res.Plan.PostActions, stmt)
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: s.Name + "." + name,
				Source: "FULLTEXT(" + strings.Join(pgIdx.Columns, ",") + ")",
				Target: "GIN expression index on to_tsvector('simple', …)",
				Reason: "MySQL FULLTEXT mapped to a PG GIN index on a to_tsvector expression. The 'simple' configuration is language-agnostic — swap to 'english'/'french'/… for stemming if desired.",
				Level:  "info",
			})
			continue
		case "SPATIAL":
			if t.caps().HasPostGIS {
				pgIdx.Using = "gist"
				t.res.Plan.Indexes = append(t.res.Plan.Indexes, pgIdx)
				continue
			}
			t.warn(s.Name, "index.spatial",
				"SPATIAL index requires PostGIS; index deferred")
			continue
		case "VECTOR":
			// MariaDB 11.7+ VECTOR INDEX → pgvector HNSW. Default to cosine
			// distance which matches MariaDB's default; users can swap to
			// `vector_l2_ops` / `vector_ip_ops` post-migration if needed.
			pgIdx.Using = "hnsw"
			if len(pgIdx.Columns) == 1 {
				// Encode the operator class on the single column expression.
				pgIdx.Columns = []string{pgIdx.Columns[0] + " vector_cosine_ops"}
				pgIdx.ColumnIsExpr = []bool{true}
				pgIdx.ColumnDirs = nil
			}
			if t.caps().HasPgVector {
				t.res.Plan.Indexes = append(t.res.Plan.Indexes, pgIdx)
				t.res.Explanations = append(t.res.Explanations, Explanation{
					Object: s.Name + "." + name,
					Source: "VECTOR INDEX",
					Target: "USING hnsw (... vector_cosine_ops)",
					Reason: "MariaDB VECTOR INDEX (HNSW-style ANN over a VECTOR column) mapped to pgvector's HNSW with cosine distance — MariaDB's default. Switch the operator class to vector_l2_ops or vector_ip_ops if your similarity semantics differ.",
					Level:  "info",
				})
			} else {
				t.warn(s.Name+"."+name, "type",
					"VECTOR INDEX requires the pgvector extension — install it on the target before running the migration")
			}
			continue
		}
		t.res.Plan.Indexes = append(t.res.Plan.Indexes, pgIdx)
	}

	// ON UPDATE CURRENT_TIMESTAMP → trigger function
	for _, c := range s.Columns {
		if c.OnUpdate != nil {
			t.emitOnUpdateTrigger(s.Name, c.Name)
		}
	}

	// Oracle DEFAULT ON NULL → trigger that coalesces explicit NULLs to the
	// default expression. Plain PG DEFAULT only fires when the column is
	// omitted, so without this trigger an `INSERT … VALUES (…, NULL, …)`
	// would store NULL instead of the documented fallback.
	for _, c := range s.Columns {
		if c.DefaultOnNull && c.HasDefault && c.Default != nil {
			t.emitDefaultOnNullTrigger(s.Name, c.Name, rawExpr(c.Default))
		}
	}

	// AUTO_INCREMENT table option → ALTER SEQUENCE post-copy
	if s.Options.HasAutoInc {
		t.res.Plan.PostActions = append(t.res.Plan.PostActions,
			fmt.Sprintf("-- AUTO_INCREMENT=%d source; restart identity to match after data load", s.Options.AutoIncrement))
	}
	if s.Options.Engine != "" && !strings.EqualFold(s.Options.Engine, "InnoDB") {
		eng := strings.ToUpper(s.Options.Engine)
		// MariaDB-specific engines have semantics that go well beyond storage
		// layout (Spider/S3 are sharding/remote, ColumnStore is OLAP, Aria is
		// crash-safe MyISAM-like). The translator emits a normal PG table for
		// the schema in every case; surface a tailored warning per engine so
		// the user knows what is silently dropped.
		var detail string
		switch eng {
		case "ARIA":
			detail = "MariaDB Aria (crash-safe MyISAM successor) — translates to a regular PG table; tune autovacuum if needed for the workload that previously relied on Aria's per-table page caching."
		case "COLUMNSTORE":
			detail = "MariaDB ColumnStore (column-oriented OLAP engine) — schema migrates as a row-store PG table. For analytical workloads, evaluate citus, hydra, or pg_duckdb on the PG side; ColumnStore-specific aggregations and partitioning are NOT replicated."
		case "SPIDER":
			detail = "MariaDB Spider (transparent sharding/federated proxy) — only the local table shape is migrated; the federated routing across remote backends is dropped. Re-create the federation via postgres_fdw + foreign tables, or by sharding via Citus, depending on the topology."
		case "S3":
			detail = "MariaDB S3 (read-only tables backed by S3 objects) — schema migrates as a regular PG table but the S3-backed data is NOT copied. Use aws_s3 / aws_lambda extensions or COPY from a downloaded snapshot to re-load."
		case "MYISAM", "MEMORY", "BLACKHOLE", "ARCHIVE", "FEDERATED", "MRG_MYISAM", "CSV":
			detail = "non-InnoDB MySQL engine '" + s.Options.Engine + "' — PG has a single storage engine. The table migrates normally; engine-specific semantics (no transactions, in-memory only, …) are dropped."
		default:
			detail = "non-InnoDB engine '" + s.Options.Engine + "' ignored; PG has a single storage engine"
		}
		t.warn(s.Name, "engine", detail)
	}

	// MySQL/MariaDB PARTITION BY clause (structured AST) → PG declarative
	// partitioning. Same failure semantics as Oracle below: degrade to an
	// unpartitioned table and surface the cause as a warning.
	if s.Partitioning != nil && !dialects.IsOracle(t.opt.SourceKind) {
		part, notes, err := translateMysqlPartitioning(s.Partitioning)
		for _, n := range notes {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "PARTITION BY " + s.Partitioning.Method,
				Target: "PG declarative partitioning (degraded)",
				Reason: n,
				Level:  "warn",
			})
		}
		if err != nil {
			t.warn(s.Name, "partitioning",
				"could not translate MySQL PARTITION BY clause; table emitted unpartitioned: "+err.Error())
		} else if part != nil {
			tbl.Partitioning = part
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + tbl.Name,
				Source: "PARTITION BY " + s.Partitioning.Method,
				Target: fmt.Sprintf("PG declarative %s partitioning (%d partitions)", part.Method, len(part.Partitions)),
				Reason: "MySQL partitioning lifted to PostgreSQL declarative partitioning; data routes automatically through the parent.",
				Level:  "info",
			})
		}
	}

	// Oracle PARTITION BY clause → PG declarative partitioning. Failures
	// downgrade to an unpartitioned table + warning so the run still succeeds.
	if raw := strings.TrimSpace(s.Options.OraclePartitioning); raw != "" && dialects.IsOracle(t.opt.SourceKind) {
		part, notes, err := parseOraclePartitioning(raw)
		t.applyOraclePartitioningNotes(tbl.Name, notes)
		switch {
		case err != nil:
			t.warn(s.Name, "partitioning",
				"could not translate Oracle PARTITION BY clause; table emitted unpartitioned: "+err.Error())
		case part != nil:
			tbl.Partitioning = part
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + tbl.Name,
				Source: "PARTITION BY " + part.Method,
				Target: fmt.Sprintf("PG declarative %s partitioning (%d partitions)", part.Method, len(part.Partitions)),
				Reason: "Oracle " + part.Method + " partitioning lifted to PostgreSQL declarative partitioning; data routes automatically through the parent.",
				Level:  "info",
			})
		}
	}

	// Oracle 21c+/23c table-level qualifiers (IMMUTABLE / BLOCKCHAIN /
	// SHARDED / DUPLICATED / PRIVATE TEMPORARY) have no PG counterpart —
	// the table is emitted as a regular heap (or TEMPORARY for the temp
	// variants). Surface a per-qualifier explanation so the user knows
	// the locked-row / sharding / per-session semantics were dropped.
	if dialects.IsOracle(t.opt.SourceKind) {
		if s.Options.OracleImmutable {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "CREATE IMMUTABLE TABLE",
				Target: "PG heap (no IMMUTABLE constraint)",
				Reason: "Oracle IMMUTABLE TABLE rejects all UPDATE/DELETE after the row is committed. PostgreSQL has no DDL equivalent — enforce via row-level security + policy that DENIES UPDATE/DELETE, or via a BEFORE UPDATE/DELETE trigger that RAISE EXCEPTIONs.",
				Level:  "warn",
			})
		}
		if s.Options.OracleBlockchain {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "CREATE BLOCKCHAIN TABLE",
				Target: "PG heap (no tamper-evident chaining)",
				Reason: "Oracle BLOCKCHAIN TABLE chains rows with cryptographic hashes for tamper evidence. PostgreSQL has no built-in equivalent — install pg_audit or roll a hash-chain trigger pattern manually if integrity attestation matters.",
				Level:  "warn",
			})
		}
		if s.Options.OracleSharded {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "CREATE SHARDED TABLE",
				Target: "PG heap (no sharding)",
				Reason: "Oracle Sharding distributes rows across multiple databases via SHARDING_KEY. PostgreSQL has no built-in sharding — use Citus, partitioning + foreign tables, or app-level routing.",
				Level:  "warn",
			})
		}
		if s.Options.OracleDuplicated {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "CREATE DUPLICATED TABLE",
				Target: "PG heap (no auto-replication to every shard)",
				Reason: "Oracle DUPLICATED TABLE replicates the same content to every shard for star-schema dimension lookups. PostgreSQL has no equivalent — replicate manually via logical replication or app-level fan-out.",
				Level:  "warn",
			})
		}
		if s.Options.OraclePrivateTemp {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "CREATE PRIVATE TEMPORARY TABLE",
				Target: "PG TEMPORARY (per-session, dropped at end-of-session)",
				Reason: "Oracle Private Temporary Tables (`ORA$PTT_…`) live for a single transaction or session. PostgreSQL TEMPORARY tables are per-session by default — close enough that we keep the TEMPORARY emission; for the per-transaction variant (`ON COMMIT DELETE ROWS` / `ON COMMIT DROP`), add the corresponding clause manually.",
				Level:  "info",
			})
		}
	}

	// Oracle ORGANIZATION EXTERNAL / INDEX (IOT) — surface as
	// info/blocking prerequisites since the table is emitted as a regular
	// PG heap (no FOREIGN TABLE / IOT semantics).
	if dialects.IsOracle(t.opt.SourceKind) {
		switch strings.ToUpper(s.Options.OracleOrganization) {
		case "EXTERNAL":
			t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualReview,
				Object:      "table." + s.Name,
				Title:       "Reimplement Oracle external table " + s.Name + " as a PG FOREIGN TABLE",
				Description: "The Oracle dump contains an ORGANIZATION EXTERNAL table with a TYPE / DEFAULT DIRECTORY / ACCESS PARAMETERS / LOCATION clause. squishy emitted the table as a regular PG heap and copied no rows (the source data lives on the filesystem outside the database). To preserve the original 'data lives in a flat file' semantics, recreate the table as a PG FOREIGN TABLE backed by an FDW.",
				Remediation: `Pick the FDW that matches the Oracle ACCESS DRIVER:

  - ORACLE_LOADER (csv/text)        → file_fdw
      CREATE EXTENSION file_fdw;
      CREATE SERVER files FOREIGN DATA WRAPPER file_fdw;
      CREATE FOREIGN TABLE mig.` + s.Name + ` (...) SERVER files
        OPTIONS (filename '/path/to/file', format 'csv', header 'true');

  - ORACLE_DATAPUMP (binary export) → no direct FDW; reload via impdp +
      regular CREATE TABLE on the PG side.

  - ORACLE_HDFS / ORACLE_HIVE       → hdfs_fdw / hive_fdw or move the data
      into a real PG table.

The regular PG table squishy emitted is empty — DROP it once the
FOREIGN TABLE is in place if you don't want both.`,
			})
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "ORGANIZATION EXTERNAL",
				Target: "PG heap (no FDW configured)",
				Reason: "Oracle external table semantics (data on the filesystem, accessed via ACCESS DRIVERs) lifted to a regular PG heap. See the matching prerequisite for FDW remediation.",
				Level:  "warn",
			})
		case "INDEX":
			// Index-organized table. PG has no IOT; the closest analogue
			// is a normal heap with the PK index covering all columns
			// (CLUSTER + INCLUDE), which approximates the locality but
			// not the storage savings.
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + s.Name,
				Source: "ORGANIZATION INDEX (IOT)",
				Target: "PG heap (no IOT — approximate via CLUSTER + INCLUDE)",
				Reason: "Oracle Index-Organized Tables store row data inline in the PK B-tree leaf, eliminating the separate heap and giving range-scan locality. PostgreSQL has no IOT — the table was emitted as a regular heap. To approximate the locality, run `CLUSTER mig." + s.Name + " USING <pk_index>` after the data load (PG re-clustering is one-shot, not auto-maintained); covering indexes (CREATE INDEX … INCLUDE (cols)) eliminate the heap fetch for index-only scans.",
				Level:  "info",
			})
		}
	}

	t.res.Plan.Tables = append(t.res.Plan.Tables, tbl)
}

// applyOraclePartitioningNotes turns the advanced-form discoveries the
// Oracle partitioning lifter records into structured warnings + blocking
// prerequisites so the user sees exactly which Oracle-only semantic was
// dropped during the PG downgrade.
//
// REFERENCE and SYSTEM partitioning have no PG equivalent at all, so the
// caller will have set part=nil and the table is emitted unpartitioned;
// we surface a blocking prereq pointing at the manual remediation. The
// remaining notes (INTERVAL, AUTOMATIC LIST, composite SUBPARTITION BY,
// SUBPARTITION TEMPLATE) describe semantics that were stripped from the
// clause but where the top-level partitioning could still be lifted —
// those flow through as info/warn explanations + manual-review prereqs.
func (t *translator) applyOraclePartitioningNotes(tableName string, n oraclePartNotes) {
	obj := "table." + tableName

	if n.HasReference {
		t.warn(tableName, "partitioning.reference",
			"PARTITION BY REFERENCE ("+n.ReferenceConstraint+") has no PostgreSQL equivalent; table emitted unpartitioned. Child rows will not auto-route by parent partition.")
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityBlocking,
			Category:    CatManualReview,
			Object:      obj,
			Title:       "Reimplement Oracle REFERENCE partitioning manually",
			Description: "PostgreSQL has no built-in equivalent of Oracle's PARTITION BY REFERENCE. Child rows in '" + tableName + "' were partitioned by the parent's partition key via foreign key constraint '" + n.ReferenceConstraint + "'. squishy emitted the table without partitioning so the run can complete; the equivalent locality must be re-established by hand.",
			Remediation: `Two viable approaches in PG:

1. Add the parent's partition key as a column on the child table (denormalised),
   then declare the child PARTITION BY <method> on that column with the same
   bounds as the parent. Maintain the column via a BEFORE INSERT trigger that
   reads the parent row.

2. Skip child-side partitioning entirely and rely on PG's partition-pruning
   for queries that join parent + child on the partition key. This is simpler
   but loses the per-partition maintenance benefits.

Pick (1) only if you regularly run partition-level operations (DROP PARTITION,
EXCHANGE PARTITION, parallel maintenance) on the child table.`,
		})
	}

	if n.HasSystem {
		t.warn(tableName, "partitioning.system",
			"PARTITION BY SYSTEM has no PostgreSQL equivalent; table emitted unpartitioned. Application-managed partition routing must be re-implemented.")
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityBlocking,
			Category:    CatManualReview,
			Object:      obj,
			Title:       "Reimplement Oracle SYSTEM partitioning manually",
			Description: "Oracle PARTITION BY SYSTEM is application-controlled — INSERT statements must explicitly name the target partition. PostgreSQL has no equivalent; the table was emitted unpartitioned.",
			Remediation: `Either:
  • Pick a real partition key (column-based RANGE/LIST/HASH) and migrate the
    application's explicit partition-targeting code to standard INSERT.
  • Keep the table unpartitioned and rely on regular indexes; SYSTEM partitioning
    in Oracle is rarely a performance win on its own.`,
		})
	}

	if n.HasInterval {
		t.warn(tableName, "partitioning.interval",
			"INTERVAL ("+n.IntervalExpr+") auto-creation dropped: PostgreSQL has no native interval partitioning. The pre-existing partitions become a fixed snapshot — new ranges must be created manually or via pg_partman.")
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityBlocking,
			Category:    CatManualReview,
			Object:      obj,
			Title:       "Replace Oracle INTERVAL partitioning with pg_partman or a cron job",
			Description: "Oracle's INTERVAL clause auto-creates the next RANGE partition when an INSERT lands beyond the highest bound. PostgreSQL does not auto-create partitions. The fixed snapshot squishy emitted will reject inserts past the last bound with `no partition of relation … found for row`.",
			Remediation: `Pick one:

1. (recommended) Install pg_partman and convert this table:
     CREATE EXTENSION IF NOT EXISTS pg_partman;
     SELECT partman.create_parent(
       p_parent_table => 'mig.` + tableName + `',
       p_control      => '<partition_key_column>',
       p_type         => 'native',
       p_interval     => '1 month',                -- match Oracle INTERVAL
       p_premake      => 4
     );
   Then schedule run_maintenance_proc() in cron or pg_cron to keep new ranges
   ahead of the writer.

2. Add a cron job that issues
     CREATE TABLE mig.` + tableName + `_yYYYY_mMM PARTITION OF mig.` + tableName + `
       FOR VALUES FROM ('YYYY-MM-01') TO ('YYYY-MM-01'::date + interval '1 month');
   on the schedule that matches the Oracle INTERVAL.`,
		})
	}

	if n.HasAutomaticList {
		t.warn(tableName, "partitioning.automatic_list",
			"AUTOMATIC LIST partitioning dropped: PostgreSQL has no auto-create for new LIST values. INSERTs of unseen values will fail unless a DEFAULT partition exists.")
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityBlocking,
			Category:    CatManualReview,
			Object:      obj,
			Title:       "Replace Oracle AUTOMATIC LIST with a DEFAULT partition or pg_partman",
			Description: "Oracle's AUTOMATIC LIST option auto-creates a new partition when an INSERT brings a previously-unseen LIST value. PostgreSQL has no equivalent; unseen values will raise `no partition of relation … found for row`.",
			Remediation: `Either:
  1. Add a catch-all DEFAULT partition so unseen values land somewhere:
       CREATE TABLE mig.` + tableName + `_default
         PARTITION OF mig.` + tableName + ` DEFAULT;
     Periodically split rows out of _default into proper LIST partitions.

  2. Trigger-based auto-creation via pg_partman with p_type='native', or a
     BEFORE INSERT trigger on the parent that runs CREATE TABLE … PARTITION
     OF on the fly when the value is new.`,
		})
	}

	if n.HasSubpartitioning {
		method := n.SubpartitionMethod
		if method == "" {
			method = "?"
		}
		t.warn(tableName, "partitioning.subpartition",
			"SUBPARTITION BY "+method+" flattened: only the top-level partitioning was lifted. Per-partition subpartition data will be co-located in the top-level partition rather than split further.")
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityBlocking,
			Category:    CatManualReview,
			Object:      obj,
			Title:       "Reimplement Oracle composite (SUB)partitioning manually",
			Description: "Oracle composite partitioning (e.g. RANGE-HASH, LIST-HASH) maps to PG's nested PARTITION BY but each top-level partition must declare its own subpartition spec — Oracle's SUBPARTITION TEMPLATE doesn't translate. squishy emitted only the top level so the migration can run.",
			Remediation: `For each top-level partition emitted, decide whether to add a second level:

  ALTER TABLE mig.` + tableName + ` DETACH PARTITION mig.<child>;
  -- recreate as PARTITION BY ` + method + ` (...) and re-attach with the same bound:
  CREATE TABLE mig.<child>_repart (...)
    PARTITION BY ` + method + ` (<sub_key>);
  -- create each subpartition explicitly (no TEMPLATE in PG):
  CREATE TABLE mig.<child>_sp1 PARTITION OF mig.<child>_repart ...;
  ALTER TABLE mig.` + tableName + ` ATTACH PARTITION mig.<child>_repart
    FOR VALUES FROM (…) TO (…);

If you don't need the second level (HASH subpartitioning is often cosmetic
in Oracle once the data fits in memory), the as-emitted single-level table
is the simpler choice.`,
		})
	}

	if n.HasSubpartTemplate && !n.HasSubpartitioning {
		// Stray template (no SUBPARTITION BY found) — should be rare;
		// surface as info so the user is at least aware.
		t.warn(tableName, "partitioning.subpartition_template",
			"SUBPARTITION TEMPLATE clause stripped (PG has no template syntax — each subpartition must be declared explicitly).")
	}
}

// translateTableLike handles `CREATE TABLE dst LIKE src`. Postgres has the
// same shape natively (`CREATE TABLE dst (LIKE src INCLUDING ALL)`) so we
// can emit it as a pre-action without a manual review. We do flag it as an
// info-level explanation so the user is aware the column types and
// constraints are inherited from the source-side translation.
func (t *translator) translateTableLike(s *ast.CreateTableLike) {
	srcRef := pgQuote(s.LikeName)
	if s.LikeSchema != "" {
		srcRef = pgQuote(s.LikeSchema) + "." + pgQuote(s.LikeName)
	} else {
		srcRef = pgQuote(t.opt.TargetSchema) + "." + pgQuote(s.LikeName)
	}
	ifne := ""
	if s.IfNotExists {
		ifne = "IF NOT EXISTS "
	}
	stmt := fmt.Sprintf("CREATE TABLE %s%s.%s (LIKE %s INCLUDING ALL);",
		ifne, pgQuote(t.opt.TargetSchema), pgQuote(s.Name), srcRef)
	t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "table." + s.Name,
		Source: "CREATE TABLE " + s.Name + " LIKE " + s.LikeName,
		Target: "CREATE TABLE … (LIKE " + s.LikeName + " INCLUDING ALL)",
		Reason: "MySQL CREATE TABLE … LIKE mapped to PG's native (LIKE source INCLUDING ALL); column types and constraints are inherited from the previously translated source table.",
		Level:  "info",
	})
}

// translateTableAs handles `CREATE TABLE dst [(cols)] [opts] AS <select>`.
// The SELECT body almost always references MySQL-specific functions or
// column-name casing, so we emit it as a manual-review prerequisite and
// drop the table from the auto-generated plan.
func (t *translator) translateTableAs(s *ast.CreateTableAs) {
	t.warn(s.Name, "create_table_as",
		"CREATE TABLE … AS SELECT skipped: the SELECT body must be reviewed and rewritten for PG before running. Source body preserved in remediation.")
	t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
		ID:          "create_table_as_" + s.Name,
		Severity:    SeverityBlocking,
		Category:    CatManualReview,
		Object:      "table." + s.Name,
		Title:       "Manual review: CREATE TABLE " + s.Name + " AS SELECT …",
		Description: "MySQL `CREATE TABLE … AS SELECT` translates 1:1 to PG, but the embedded SELECT typically references MySQL-specific syntax (backtick identifiers, builtin functions) that PG rejects. Review and adapt the SELECT body before running.",
		Remediation: fmt.Sprintf("Adapt and run manually:\n\nCREATE TABLE %s.%s AS\n%s;\n",
			t.opt.TargetSchema, s.Name, s.SelectBody),
	})
}

// translateDropObject lifts MySQL/MariaDB DROP {INDEX|VIEW|PROCEDURE|
// FUNCTION|TRIGGER|EVENT} into the equivalent PG pre-action. The result is
// queued as a Plan.PreActions entry so it executes before the new schema is
// created (typical use: idempotent dump replays).
//
// Forms with no PG analogue (DROP EVENT) emit a manual-review prereq
// instead, so the user is aware that a dropped pg_cron schedule needs
// hand-removal.
func (t *translator) translateDropObject(s *ast.DropObject) {
	ifexists := ""
	if s.IfExists {
		ifexists = "IF EXISTS "
	}
	schema := t.opt.TargetSchema
	// Oracle quoting / case-folding: ident from the parser is uppercase
	// (DBMS_METADATA convention), so we normalise to PG's lowercase before
	// quoting. MySQL/MariaDB names round-trip as-is.
	caseFold := func(name string) string {
		if dialects.IsOracle(t.opt.SourceKind) {
			return normalizeOracleIdent(name)
		}
		return name
	}
	q := func(name string) string { return pgQuote(schema) + "." + pgQuote(caseFold(name)) }
	tail := ""
	if s.Cascade {
		tail = " CASCADE"
	} else if s.Restrict {
		tail = " RESTRICT"
	}

	switch s.Kind {
	case "INDEX":
		// PG's `DROP INDEX` doesn't take an `ON table` clause — the index
		// name alone is enough. The MySQL `ON table` qualifier is used to
		// disambiguate (per-table index namespaces) but PG indexes share a
		// per-schema namespace, so we drop the qualifier.
		stmt := fmt.Sprintf("DROP INDEX %s%s.%s%s;",
			ifexists, pgQuote(schema), pgQuote(caseFold(s.Name)), tail)
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "VIEW":
		var qualified []string
		if len(s.Names) > 0 {
			for _, ref := range s.Names {
				qualified = append(qualified, q(ref.Name))
			}
		} else {
			qualified = []string{q(s.Name)}
		}
		stmt := fmt.Sprintf("DROP VIEW %s%s%s;",
			ifexists, strings.Join(qualified, ", "), tail)
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "MATERIALIZED VIEW":
		stmt := fmt.Sprintf("DROP MATERIALIZED VIEW %s%s%s;",
			ifexists, q(s.Name), tail)
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "SEQUENCE":
		stmt := fmt.Sprintf("DROP SEQUENCE %s%s%s;",
			ifexists, q(s.Name), tail)
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "PROCEDURE":
		// PG procedures require argument types for unambiguous resolution,
		// but we don't have them at parse time. Emit the unqualified form;
		// PG will use it as long as the name is unique in the schema.
		stmt := fmt.Sprintf("DROP PROCEDURE %s%s%s;", ifexists, q(s.Name), tail)
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "FUNCTION":
		stmt := fmt.Sprintf("DROP FUNCTION %s%s%s;", ifexists, q(s.Name), tail)
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "TRIGGER":
		name := caseFold(s.Name)
		// Oracle: resolve the owning table from the migration plan so PG's
		// table-aware DROP TRIGGER syntax can be emitted directly. MySQL
		// drops still rely on the TODO placeholder since MySQL has no
		// table-name in the source statement.
		if dialects.IsOracle(t.opt.SourceKind) && t.triggerTable != nil {
			if tbl, ok := t.triggerTable[strings.ToLower(name)]; ok {
				stmt := fmt.Sprintf("DROP TRIGGER %s%s ON %s.%s%s;",
					ifexists, pgQuote(name), pgQuote(schema), pgQuote(tbl), tail)
				t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
				return
			}
		}
		t.warn(s.Name, "drop_trigger",
			"DROP TRIGGER "+s.Name+" emitted as a TODO comment because PG requires the table name; review and adjust before running.")
		stmt := fmt.Sprintf("-- TODO: DROP TRIGGER %s%s ON <table>;",
			ifexists, pgQuote(name))
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "TYPE":
		stmt := fmt.Sprintf("DROP TYPE %s%s%s;", ifexists, q(s.Name), tail)
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, stmt)
	case "TYPE BODY":
		// Oracle's TYPE BODY holds the implementation of object-type
		// methods. PG composite types have no methods → no body to drop.
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "type." + caseFold(s.Name),
			Source: "DROP TYPE BODY " + s.Name,
			Target: "(no PG equivalent — dropped)",
			Reason: "Oracle TYPE BODY holds method implementations for object types; PG composite types have no methods, so there is no body to drop. The matching DROP TYPE (when present) handles the type itself.",
			Level:  "info",
		})
	case "PACKAGE", "PACKAGE BODY":
		// PG has no packages — package routines were promoted to standalone
		// functions during CREATE translation (MD-07/Oracle path). Surface
		// an info explanation pointing the user at the manual cleanup.
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "package." + caseFold(s.Name),
			Source: "DROP " + s.Kind + " " + s.Name,
			Target: "(no PG equivalent — dropped)",
			Reason: "PostgreSQL has no PACKAGE concept. The package's routines were translated to standalone PG functions; if you no longer need them, drop those functions individually (`DROP FUNCTION pkg_routine`).",
			Level:  "info",
		})
	case "EVENT":
		// MySQL events are scheduled jobs; the PG counterpart is pg_cron.
		// We don't auto-remove pg_cron jobs (we don't know the cron
		// schedule's id), so surface as a manual review.
		t.warn(s.Name, "drop_event",
			"DROP EVENT "+s.Name+" has no automatic PG equivalent. If pg_cron was used to schedule it, unschedule manually.")
	}
}

// translateTruncate maps `TRUNCATE TABLE name` to the equivalent PG
// statement, emitted as a pre-action.
func (t *translator) translateTruncate(s *ast.TruncateTable) {
	t.res.Plan.PreActions = append(t.res.Plan.PreActions,
		fmt.Sprintf("TRUNCATE TABLE %s.%s;",
			pgQuote(t.opt.TargetSchema), pgQuote(s.Table.Name)))
}

// translateCreateIndex handles standalone `CREATE [UNIQUE|BITMAP] INDEX
// name ON table (cols-or-exprs)` statements (Oracle's DBMS_METADATA emits
// secondary indexes this way; MySQL dumps may also). Inline KEY/INDEX
// declarations inside CREATE TABLE go through translateTable's own
// per-index loop.
//
// Function-based / expression keys (Oracle's `LOWER(col)` or `(col1+col2)`,
// MySQL's `((LOWER(col)))`) are preserved verbatim via IndexedCol.IsExpr →
// PGIndex.ColumnIsExpr; the PG writer wraps each one in `(expr)`.
//
// FULLTEXT / SPATIAL / VECTOR variants are not currently emitted by the
// Oracle parser through this path (Oracle uses BITMAP / domain indexes for
// those), but the dispatch mirrors the inline-KEY logic so MySQL standalone
// CREATE FULLTEXT INDEX would also work.
func (t *translator) translateCreateIndex(s *ast.CreateIndex) {
	tblName := s.Table.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		tblName = normalizeOracleIdent(tblName)
	}
	name := s.Name
	if name == "" {
		name = fmt.Sprintf("idx_%s_%s", tblName, joinCols(s.Columns))
	}
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	cols, dirs, isExpr, hadDir, hadExpr := indexColumnPayload(s.Columns)
	// Bare-column entries are case-folded to the PG convention (lowercase
	// for Oracle); expression entries carry mixed-case source text and are
	// passed through unchanged.
	if dialects.IsOracle(t.opt.SourceKind) {
		for i := range cols {
			if !isExpr[i] {
				cols[i] = normalizeOracleIdent(cols[i])
			}
		}
	}
	pgIdx := PGIndex{
		Schema:  t.opt.TargetSchema,
		Table:   tblName,
		Name:    name,
		Unique:  s.Unique,
		Columns: cols,
		Using:   strings.ToLower(s.Using),
	}
	if hadDir {
		pgIdx.ColumnDirs = dirs
	}
	if hadExpr {
		pgIdx.ColumnIsExpr = isExpr
	}
	switch s.Kind {
	case "BITMAP":
		// Oracle BITMAP indexes have no PG equivalent — emit a plain
		// B-tree with an info-level note. PG's planner handles equality
		// joins on low-cardinality columns adequately on B-tree.
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: tblName + "." + name,
			Source: "BITMAP INDEX",
			Target: "B-tree (no PG bitmap index DDL)",
			Reason: "Oracle BITMAP indexes have no PG equivalent — they're synthesised on the fly by the planner for AND/OR over multiple B-trees. The B-tree emitted here is functionally equivalent for most workloads.",
			Level:  "info",
		})
	}
	t.res.Plan.Indexes = append(t.res.Plan.Indexes, pgIdx)
}

// translateRenameTable expands MySQL's multi-pair `RENAME TABLE a TO b,
// c TO d` into N PG `ALTER TABLE … RENAME TO …` statements (PG only renames
// one table per ALTER).
func (t *translator) translateRenameTable(s *ast.RenameTable) {
	for _, pair := range s.Pairs {
		t.res.Plan.PreActions = append(t.res.Plan.PreActions,
			fmt.Sprintf("ALTER TABLE %s.%s RENAME TO %s;",
				pgQuote(t.opt.TargetSchema), pgQuote(pair.From.Name),
				pgQuote(pair.To.Name)))
	}
}

// translateAlterTable handles ALTER TABLE statements emitted by the source
// inspector — currently only ADD CONSTRAINT (PK / UQ / FK / CHECK), which
// DBMS_METADATA emits separately for non-IOT Oracle tables. The constraints
// are merged into the previously translated PGTable so the PG CREATE TABLE
// carries the PK; without this the post-copy FK constraints fail with
// "no unique constraint matching given keys for referenced table".
func (t *translator) translateAlterTable(s *ast.AlterTable) {
	if dialects.IsOracle(t.opt.SourceKind) {
		normalizeOracleAlterTable(s)
	}
	tblIdx := -1
	for i := range t.res.Plan.Tables {
		if strings.EqualFold(t.res.Plan.Tables[i].Name, s.Table.Name) {
			tblIdx = i
			break
		}
	}
	for _, act := range s.Actions {
		if act.Kind != "ADD_CONSTRAINT" || act.Constraint == nil {
			continue
		}
		switch c := act.Constraint.(type) {
		case *ast.PKConstraint:
			if tblIdx < 0 {
				continue
			}
			tbl := &t.res.Plan.Tables[tblIdx]
			// Only set if not already populated (avoid duplicating an inline PK).
			if len(tbl.PK) == 0 {
				for _, col := range c.Columns {
					tbl.PK = append(tbl.PK, col.Name)
				}
			}
		case *ast.UQConstraint:
			name := c.Name
			if name == "" {
				name = fmt.Sprintf("uq_%s_%s", s.Table.Name, joinCols(c.Columns))
			}
			t.res.Plan.Indexes = append(t.res.Plan.Indexes, PGIndex{
				Schema: t.opt.TargetSchema, Table: s.Table.Name, Name: name,
				Unique: true, Columns: columnsNames(c.Columns),
			})
		case *ast.FKConstraint:
			name := c.Name
			if name == "" {
				name = fmt.Sprintf("fk_%s_%s", s.Table.Name, strings.Join(c.Columns, "_"))
			}
			refSchema := t.opt.TargetSchema
			if c.RefSchema != "" && !dialects.IsOracle(t.opt.SourceKind) {
				refSchema = c.RefSchema
			}
			t.res.Plan.ForeignKeys = append(t.res.Plan.ForeignKeys, PGForeignKey{
				Schema: t.opt.TargetSchema, Table: s.Table.Name, Name: name,
				Columns: c.Columns, RefSchema: refSchema, RefTable: c.RefTable,
				RefColumns: c.RefColumns, OnDelete: c.OnDelete, OnUpdate: c.OnUpdate,
			})
		case *ast.CheckConstraint:
			if tblIdx < 0 {
				continue
			}
			t.res.Plan.Tables[tblIdx].Checks = append(
				t.res.Plan.Tables[tblIdx].Checks, rawExpr(c.Expr))
			if !c.Enforced {
				// See translateTable: NOT ENFORCED has no PG equivalent.
				t.res.Explanations = append(t.res.Explanations, Explanation{
					Object: s.Table.Name,
					Source: "ALTER TABLE … ADD CHECK … NOT ENFORCED",
					Target: "CHECK (always enforced in PG)",
					Reason: "MariaDB allows NOT ENFORCED to keep the CHECK as documentation only; PostgreSQL enforces every CHECK. Existing rows that violated the constraint in MariaDB will fail at validation time.",
					Level:  "warn",
				})
			}
		}
	}
}

func (t *translator) translateColumn(tableName string, c *ast.ColumnDef) (PGColumn, []Explanation, []TypeMapping) {
	expls := []Explanation{}
	mappings := []TypeMapping{}

	tr := MapType(t.opt.SourceKind, c.Type, c.Name, t.caps())
	pg := PGColumn{Name: c.Name, Type: tr.PG, NotNull: c.NotNull, Comment: c.Comment}

	// AUTO_INCREMENT handling:
	//   - integer types (SMALLINT/INTEGER/BIGINT): use native IDENTITY.
	//   - NUMERIC(20,0) (only used for BIGINT UNSIGNED to preserve full range):
	//     PG IDENTITY requires an integer type, so we emit an explicit
	//     SEQUENCE + DEFAULT nextval — the bigint-sized sequence covers
	//     allocation up to 2^63-1; values beyond that still round-trip via
	//     the COPY (they're preserved literally, just not auto-allocated).
	if c.AutoInc {
		pg.NotNull = true
		if strings.HasPrefix(pg.Type, "NUMERIC") {
			seqName := tableName + "_" + c.Name + "_seq"
			// Double-quote identifiers inside the regclass string literal so
			// PG preserves the original case (e.g. "t_Case_sensitive_id_seq").
			seqRef := fmt.Sprintf(`"%s"."%s"`,
				strings.ReplaceAll(t.opt.TargetSchema, `"`, `""`),
				strings.ReplaceAll(seqName, `"`, `""`))
			t.res.Plan.PreActions = append(t.res.Plan.PreActions,
				fmt.Sprintf("CREATE SEQUENCE IF NOT EXISTS %s.%s AS bigint;",
					quoteIdent(t.opt.TargetSchema), quoteIdent(seqName)))
			pg.Default = fmt.Sprintf("nextval('%s')",
				strings.ReplaceAll(seqRef, `'`, `''`))
			// After data load, bump the sequence past existing values so
			// subsequent inserts don't collide with copied data.
			t.res.Plan.PostActions = append(t.res.Plan.PostActions,
				fmt.Sprintf("SELECT setval('%s', COALESCE((SELECT max(%s)::bigint FROM %s.%s), 1));",
					strings.ReplaceAll(seqRef, `'`, `''`),
					quoteIdent(c.Name),
					quoteIdent(t.opt.TargetSchema), quoteIdent(tableName)))
			expls = append(expls, Explanation{
				Object: tableName + "." + c.Name,
				Source: "AUTO_INCREMENT BIGINT UNSIGNED",
				Target: "NUMERIC(20,0) DEFAULT nextval(seq)",
				Reason: "PG IDENTITY requires an integer type; using explicit sequence to preserve the 0..2^64-1 range of BIGINT UNSIGNED.",
				Level:  "info",
			})
		} else {
			if !strings.HasPrefix(pg.Type, "INTEGER") && !strings.HasPrefix(pg.Type, "BIGINT") && !strings.HasPrefix(pg.Type, "SMALLINT") {
				pg.Type = "BIGINT"
			}
			pg.Identity = true
			expls = append(expls, Explanation{
				Object: tableName + "." + c.Name,
				Source: "AUTO_INCREMENT",
				Target: "GENERATED BY DEFAULT AS IDENTITY",
				Reason: "MySQL AUTO_INCREMENT → PG identity column. Sequence restart adjusted post-copy.",
				Level:  "info",
			})
		}
	}

	// Oracle ROWID/UROWID → surrogate BIGINT with auto-sequence. See
	// mapOracleType's RowIdType case for rationale. We mirror the
	// AUTO_INCREMENT/NUMERIC sequence scaffolding below: create a sequence,
	// wire DEFAULT nextval, and emit a post-copy setval so subsequent
	// INSERTs don't collide with rows that arrived with an explicit value.
	if _, isRowid := c.Type.(*ast.RowIdType); isRowid {
		seqName := tableName + "_" + c.Name + "_rowid_seq"
		seqRef := fmt.Sprintf(`"%s"."%s"`,
			strings.ReplaceAll(t.opt.TargetSchema, `"`, `""`),
			strings.ReplaceAll(seqName, `"`, `""`))
		t.res.Plan.PreActions = append(t.res.Plan.PreActions,
			fmt.Sprintf("CREATE SEQUENCE IF NOT EXISTS %s.%s AS bigint;",
				quoteIdent(t.opt.TargetSchema), quoteIdent(seqName)))
		pg.Default = fmt.Sprintf("nextval('%s')",
			strings.ReplaceAll(seqRef, `'`, `''`))
		t.res.Plan.PostActions = append(t.res.Plan.PostActions,
			fmt.Sprintf("SELECT setval('%s', COALESCE((SELECT max(%s)::bigint FROM %s.%s), 1));",
				strings.ReplaceAll(seqRef, `'`, `''`),
				quoteIdent(c.Name),
				quoteIdent(t.opt.TargetSchema), quoteIdent(tableName)))
		expls = append(expls, Explanation{
			Object: tableName + "." + c.Name,
			Source: "ROWID",
			Target: "BIGINT DEFAULT nextval(seq)",
			Reason: "Oracle ROWID is an opaque row pointer with no PG equivalent. Materialized as a surrogate BIGINT + sequence so each row still has a unique monotonic handle.",
			Level:  "info",
		})
	}

	// DEFAULT translation
	if c.HasDefault && c.Default != nil {
		pg.Default = translateDefault(c.Default)
		// Oracle-specific expression rewrites (SYSDATE/SYSTIMESTAMP/NVL/…).
		// PG parses `DEFAULT SYSDATE` as a column-reference because SYSDATE is
		// not a PG function; we substitute these before emission.
		if dialects.IsOracle(t.opt.SourceKind) {
			pg.Default = rewriteOracleExpr(pg.Default)
		}
	}

	// GENERATED
	if c.Generated != nil {
		stored := !c.Generated.Virtual // PG only supports STORED
		pg.Generated = &PGGenerated{Expr: rawExpr(c.Generated.Expr), Stored: true}
		if !stored {
			expls = append(expls, Explanation{
				Object: tableName + "." + c.Name,
				Source: "GENERATED ... VIRTUAL",
				Target: "GENERATED ALWAYS AS (...) STORED",
				Reason: "PostgreSQL only supports STORED generated columns; VIRTUAL is promoted to STORED.",
				Level:  "warn",
			})
		}
	}

	// Column-level CHECK
	check := tr.Check
	if c.Check != nil {
		colCheck := rawExpr(c.Check)
		// MariaDB/MySQL's idiom for JSON columns is LONGTEXT + CHECK(JSON_VALID(col)).
		// PG has no JSON_VALID function; the clean equivalent is a native JSONB
		// column with no CHECK needed. Promote and drop the check.
		if isJSONValidCheck(colCheck, c.Name) {
			pg.Type = "JSONB"
			expls = append(expls, Explanation{
				Object: tableName + "." + c.Name,
				Source: mysqlTypeRepr(c.Type) + " CHECK(JSON_VALID)",
				Target: "JSONB",
				Reason: "MariaDB LONGTEXT + CHECK(JSON_VALID(col)) is the historical JSON idiom. Promoted to native PG JSONB; the CHECK is dropped as jsonb validates on insert.",
				Level:  "info",
			})
			colCheck = ""
		}
		if colCheck != "" {
			if check != "" {
				check = check + " AND (" + colCheck + ")"
			} else {
				check = colCheck
			}
		}
	}
	if check != "" {
		pg.Check = check
	}

	// Per-column COLLATE / CHARSET. parseDataType consumes the inline
	// `[CHARACTER SET x] [COLLATE y]` clause for CHAR/VARCHAR/TEXT and
	// stores it on the type itself; the column-def option loop owns the
	// outer-form clauses (`col TYPE COLLATE …`). Try the column field first,
	// then fall back to the type's.
	colColl, colCharset := c.Collation, c.Charset
	if colColl == "" {
		if t, ok := c.Type.(*ast.CharType); ok {
			colColl, colCharset = t.Collation, t.Charset
		} else if t, ok := c.Type.(*ast.TextType); ok {
			colColl, colCharset = t.Collation, t.Charset
		}
	}
	if colColl != "" || colCharset != "" {
		pgColl, note := mapMySQLColumnCollation(colColl, colCharset)
		if pgColl != "" {
			pg.Collation = pgColl
		}
		if note != "" {
			expls = append(expls, Explanation{
				Object: tableName + "." + c.Name,
				Source: collationSource(colColl, colCharset),
				Target: collationTarget(pgColl),
				Reason: note,
				Level:  "info",
			})
		}
	}

	// Explanations
	if tr.Note != "" {
		expls = append(expls, Explanation{
			Object: tableName + "." + c.Name,
			Source: mysqlTypeRepr(c.Type),
			Target: tr.PG,
			Reason: tr.Note,
			Level:  "info",
		})
	}
	if tr.Warning != "" {
		t.warnSev(tableName+"."+c.Name, "type", tr.Warning, tr.WarningSeverity)
	}

	mappings = append(mappings, TypeMapping{
		Object: tableName + "." + c.Name,
		MySQL:  mysqlTypeRepr(c.Type),
		PG:     pg.Type,
		Note:   tr.Note,
	})

	// INVISIBLE column (MySQL 8 / MariaDB 10.3+): kept hidden from `SELECT *`
	// but still selectable by name. PG has no native equivalent — the column is
	// migrated as a normal column. Surface the divergence so apps relying on
	// `SELECT *` not returning it can adjust.
	if c.Invisible {
		expls = append(expls, Explanation{
			Object: tableName + "." + c.Name,
			Source: "INVISIBLE column",
			Target: "(plain column — visible to SELECT *)",
			Reason: "MySQL/MariaDB INVISIBLE columns are excluded from `SELECT *` but still readable by explicit reference. PostgreSQL has no equivalent; the column is migrated as a regular column and will appear in `SELECT *`. Adjust queries or use a view if you relied on the hiding.",
			Level:  "info",
		})
	}

	// COMPRESSED column (MariaDB transparent per-column compression). PG's
	// TOAST compresses out-of-line storage automatically for variable-length
	// types — no explicit setting required. Surface as info so the user knows
	// the storage saving is preserved through a different mechanism.
	if c.Compressed {
		expls = append(expls, Explanation{
			Object: tableName + "." + c.Name,
			Source: "COMPRESSED column",
			Target: "(PG TOAST handles compression transparently)",
			Reason: "MariaDB per-column COMPRESSED requires an explicit attribute; PostgreSQL TOAST compresses oversized variable-length values automatically (default `pglz`, optionally `lz4` via `ALTER TABLE … ALTER COLUMN … SET COMPRESSION lz4`). No DDL change needed for equivalent storage.",
			Level:  "info",
		})
	}

	return pg, expls, mappings
}

// emitDefaultOnNullTrigger creates a PG BEFORE INSERT/UPDATE trigger that
// emulates Oracle's `DEFAULT ON NULL <expr>` clause: when an INSERT (or
// UPDATE) explicitly sets the column to NULL, replace it with the default
// expression. Plain PG DEFAULT only fires when the column is omitted, so
// without this trigger the source's documented fallback is silently
// dropped on every NULL write.
func (t *translator) emitDefaultOnNullTrigger(tbl, col, defaultExpr string) {
	schema := t.opt.TargetSchema
	fnName := fmt.Sprintf("default_on_null_%s_%s", tbl, col)
	fn := fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.%s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.%s IS NULL THEN
    NEW.%s := %s;
  END IF;
  RETURN NEW;
END;$$;`,
		quoteIdent(schema), quoteIdent(fnName),
		quoteIdent(col), quoteIdent(col), defaultExpr)
	trgName := fmt.Sprintf("trg_default_on_null_%s_%s", tbl, col)
	trg := fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT OR UPDATE OF %s ON %s.%s
  FOR EACH ROW EXECUTE FUNCTION %s.%s();`,
		quoteIdent(trgName), quoteIdent(col),
		quoteIdent(schema), quoteIdent(tbl),
		quoteIdent(schema), quoteIdent(fnName))
	t.res.Plan.PostActions = append(t.res.Plan.PostActions, fn, trg)
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "table." + tbl + "." + col,
		Source: "DEFAULT ON NULL " + defaultExpr,
		Target: "BEFORE INSERT OR UPDATE trigger that COALESCEs NULL → default",
		Reason: "Oracle's DEFAULT ON NULL fires both when the column is omitted AND when it's explicitly set to NULL. PostgreSQL's plain DEFAULT only fires for omitted columns, so squishy wraps the default in a trigger that catches the explicit-NULL case too.",
		Level:  "info",
	})
}

// emitOnUpdateTrigger creates a PG trigger that emulates MySQL's
// `ON UPDATE CURRENT_TIMESTAMP` clause using the set_updated_at() helper.
func (t *translator) emitOnUpdateTrigger(tbl, col string) {
	fn := fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.set_%s_%s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.%s := now();
  RETURN NEW;
END;$$;`, quoteIdent(t.opt.TargetSchema), tbl, col, quoteIdent(col))
	trg := fmt.Sprintf(`CREATE TRIGGER trg_%s_%s BEFORE UPDATE ON %s.%s
  FOR EACH ROW EXECUTE FUNCTION %s.set_%s_%s();`,
		tbl, col, quoteIdent(t.opt.TargetSchema), quoteIdent(tbl),
		quoteIdent(t.opt.TargetSchema), tbl, col)
	t.res.Plan.PostActions = append(t.res.Plan.PostActions, fn, trg)
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func (t *translator) translateView(s *ast.CreateView) {
	name := s.View.Name
	// Oracle stores unquoted identifiers in uppercase. The rest of the
	// pipeline (tables, columns, params) lowercases them so that the PG-side
	// schema matches the convention used by `mig.sales`, `mig.costs`, etc.
	// Without this the view ends up as mig."PROFITS" while its referenced
	// tables are mig.sales — the SELECT body still resolves but every caller
	// must quote the view name, which breaks `validate` and surprises users.
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	// Oracle's `CREATE VIEW name (col1, col2, …) AS …` exports column names
	// double-quoted (DBMS_METADATA always quotes idents), so what was
	// canonical case-insensitive on the source side becomes a CASE-SENSITIVE
	// quoted identifier on PG once the AST writer re-emits the column list.
	// Other Oracle pipeline objects (tables, columns, params) are normalised
	// to lowercase via normalizeOracleIdent so a downstream `p.product_id`
	// reference works without quoting; do the same for the view's column
	// list. Otherwise dependent views (e.g. bombay_inventory referencing
	// products.product_id) fail with `column p.product_id does not exist`.
	cols := s.Columns
	if dialects.IsOracle(t.opt.SourceKind) {
		cols = make([]string, len(s.Columns))
		for i, c := range s.Columns {
			cols[i] = normalizeOracleIdent(c)
		}
	}
	// Object-view positional binding: when the source view is `OF
	// <type_name>`, Oracle binds each SELECT column positionally to the
	// type's attributes. PG has no such binding — the column names come
	// straight from the SELECT, which (for `ROW(...)::type` casts and
	// multi-arg expressions) gives unhelpful auto-names like `row` or the
	// function name. Pull the attribute list from the OBJECT type
	// registry and use it as the view's explicit column list so callers
	// see the same names they'd see on Oracle.
	if dialects.IsOracle(t.opt.SourceKind) && len(cols) == 0 && s.OfType != "" {
		if attrs := t.flattenOfTypeAttrs(s.OfType); len(attrs) > 0 {
			cols = make([]string, len(attrs))
			for i, a := range attrs {
				cols[i] = normalizeOracleIdent(a.Name)
			}
		}
	}
	rewritten := rewriteMySQLBody(s.SelectBody)
	// For Oracle sources, run the Oracle-specific expression rewriter over
	// the view body too (LISTAGG → string_agg, NVL → COALESCE, SYSDATE → …).
	// rewriteMySQLBody only knows MySQL idioms and leaves Oracle built-ins
	// like LISTAGG verbatim, which then fail at CREATE VIEW time.
	if dialects.IsOracle(t.opt.SourceKind) {
		rewritten = rewriteOracleExpr(rewritten)
		// Rewrite Oracle composite-type constructor calls (`warehouse_typ(
		// w.warehouse_id, w.warehouse_name, w.location_id)`) into the PG
		// `ROW(...)::"mig"."<type>"` form. Without this the CREATE VIEW
		// fails with `function warehouse_typ(...) does not exist` because
		// PG composite types don't double as constructor functions.
		rewritten = rewriteOracleCompositeConstructors(rewritten, t.userTypes, t.opt.TargetSchema)
		// Object-relational rewrites: `MAKE_REF(view, key)` collapses to
		// just the key value (PG has no REF type, so we keep the
		// observable identity), `DEREF(expr)` collapses to expr, and
		// `CAST(MULTISET(SELECT …) AS list_typ)` becomes
		// `ARRAY(SELECT ROW(t.*)::"mig"."<elem>" FROM (…) t)` for object
		// collections or `ARRAY(SELECT col FROM …)` for scalar collections.
		rewritten = rewriteOracleMakeRef(rewritten)
		rewritten = rewriteOracleDeref(rewritten)
		rewritten = rewriteOracleMultisetCast(rewritten, t.userTypes)
		// Oracle's `c.cust_address.country_id` (composite-attribute access
		// inside a SELECT) needs the PG-mandatory parens:
		// `(c.cust_address).country_id`. Apply for every column we know
		// the migration emitted with a composite type.
		rewritten = rewriteOracleCompositeAttrAccess(rewritten, t.compositeColumnNames())
		// DBMS_METADATA appends `WITH READ ONLY` to view bodies; PG has no
		// equivalent (read-only is enforced via privileges or rules). Strip
		// it. `WITH CHECK OPTION` is also Oracle-specific in this position;
		// PG models it via `CREATE VIEW … WITH CHECK OPTION` on the view
		// statement, not inside the SELECT body, so strip it too.
		rewritten = stripOracleViewTrailers(rewritten)
		// Oracle's `(+)` outer-join syntax must be rewritten into ANSI
		// LEFT/RIGHT JOIN before PG can parse it. The rewriter restructures
		// FROM/WHERE in-place; on success it returns the ANSI-form body, on
		// failure it returns the original (which then surfaces as a parse
		// error downstream rather than being silently miscompiled).
		if hasOuterJoinPlus(rewritten) {
			rewritten = rewriteOraclePlusOuterJoins(rewritten)
		}
	}
	node := &pgast.CreateView{
		Schema:      t.opt.TargetSchema,
		Name:        name,
		Columns:     cols,
		Body:        rewritten,
		CheckOption: s.CheckOption,
		Security:    s.SQLSecurity,
	}
	// The rewriter covers JSON_EXTRACT / JSON_UNQUOTE / GROUP_CONCAT structurally
	// on simple paths. Only flag the view for manual review if any of those
	// MySQL-specific calls survived the rewrite — that means the rewriter
	// deemed the case too ambiguous (e.g. path uses [] or *).
	upperRewritten := strings.ToUpper(rewritten)
	if strings.Contains(upperRewritten, "JSON_EXTRACT(") ||
		strings.Contains(upperRewritten, "JSON_UNQUOTE(") ||
		strings.Contains(upperRewritten, "GROUP_CONCAT(") {
		t.warn("view."+name, "view.functions",
			"view uses MySQL-specific functions (JSON_EXTRACT/GROUP_CONCAT); manual review required")
	}
	t.res.Plan.Views = append(t.res.Plan.Views, PGView{
		Schema: t.opt.TargetSchema, Name: name,
		SelectBody: s.SelectBody, CheckOption: s.CheckOption,
		Security: s.SQLSecurity,
		DDL:      pgast.Write([]pgast.Stmt{node}),
	})
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "view." + name,
		Source: "CREATE VIEW",
		Target: "CREATE OR REPLACE VIEW",
		Reason: "View body is copied verbatim; review function usage.",
		Level:  "info",
	})
}

// ---------------------------------------------------------------------------
// Triggers
// ---------------------------------------------------------------------------

func (t *translator) translateTrigger(s *ast.CreateTrigger) {
	// Oracle system triggers (BEFORE/AFTER CREATE/ALTER/DROP/LOGON/…) attach
	// to ON DATABASE / ON SCHEMA, not a table. PG has event triggers but
	// the firing semantics differ enough that auto-translation would
	// silently miscompile (e.g. Oracle SERVERERROR → PG ddl_command_end is
	// NOT equivalent). Surface a blocking prereq with PG event-trigger
	// guidance and skip the rest of the translation pipeline.
	if s.SystemTrigger {
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityBlocking,
			Category:    CatManualReview,
			Object:      "trigger." + s.Name,
			Title:       "Reimplement Oracle system trigger " + s.Name + " as a PG event trigger",
			Description: "The Oracle dump contains a system trigger on " + s.SystemEvent + " (scope: " + s.SystemScope + "). PostgreSQL has CREATE EVENT TRIGGER which fires on a similar set of DDL events but the firing semantics, available context information, and security model differ — auto-translation would silently miscompile the body.",
			Remediation: `For DDL events (CREATE/ALTER/DROP/TRUNCATE), the closest PG fit is:
  CREATE OR REPLACE FUNCTION ` + s.Name + `_fn() RETURNS event_trigger AS $$
  BEGIN
    -- Reimplement the Oracle body here. PG exposes:
    --   tg_event   ('ddl_command_start','ddl_command_end','sql_drop','table_rewrite')
    --   tg_tag     (the verb, e.g. 'CREATE TABLE')
    --   pg_event_trigger_ddl_commands() / pg_event_trigger_dropped_objects()
  END;
  $$ LANGUAGE plpgsql;
  CREATE EVENT TRIGGER ` + s.Name + ` ON ddl_command_end EXECUTE FUNCTION ` + s.Name + `_fn();

For LOGON/LOGOFF: PG has no equivalent — use connection logging
(log_connections=on) or a connection-pool plugin instead.

For STARTUP/SHUTDOWN/SERVERERROR/SUSPEND/DB_ROLE_CHANGE: no PG counterpart;
move the logic to your operational tooling (systemd unit, healthcheck,
Postgres extension hook).`,
		})
		t.res.Plan.Routines = append(t.res.Plan.Routines, PGRoutine{
			Kind: "trigger", Schema: t.opt.TargetSchema, Name: s.Name,
			RawBody: s.Body,
			DDL:     "-- System trigger " + s.Name + " on " + s.SystemEvent + " skipped — see prerequisite for PG event-trigger guidance.\n",
		})
		return
	}

	// Skip triggers whose owning relation isn't part of the migration
	// plan. Oracle inspect intentionally drops object/nested-storage
	// tables (which can't be DBMS_METADATA-dumped or relationally
	// translated), but those tables can still own auto-generated
	// infrastructure triggers (XML DB's `*$xd`, advanced-queue triggers,
	// etc.) — creating those against a non-existent PG table would fail
	// with `relation does not exist`. We also accept views as the owning
	// relation: PG supports CREATE TRIGGER … INSTEAD OF … ON view, which
	// is the natural translation of Oracle's INSTEAD OF triggers on
	// object views (oc_orders, oc_customers, …).
	tblNorm := s.Table.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		tblNorm = normalizeOracleIdent(tblNorm)
	}
	tableInPlan := false
	for _, tab := range t.res.Plan.Tables {
		if tab.Name == tblNorm {
			tableInPlan = true
			break
		}
	}
	viewInPlan := false
	if !tableInPlan {
		for _, v := range t.res.Plan.Views {
			if v.Name == tblNorm {
				viewInPlan = true
				break
			}
		}
	}
	// Oracle XDB-internal triggers (`*$xd`, body uses xdb.xdb_pitrig_pkg)
	// are auto-generated by Oracle when an XML object table is created.
	// They have no PG counterpart — drop them silently. The presence of
	// `xdb.xdb_pitrig_pkg` in the body is the unambiguous tell.
	xdbInternal := strings.Contains(strings.ToLower(s.Body), "xdb.xdb_pitrig_pkg")
	// Triggers on Oracle nested-table columns (`INSTEAD OF INSERT ON
	// NESTED TABLE …`) have no PG counterpart — PG's nested-table model
	// is just an array column, with no per-row trigger semantics.
	nested := s.NestedTableTrigger
	if !tableInPlan && !viewInPlan || xdbInternal || nested {
		reason := "trigger references " + s.Table.Name + " which is not in the migration plan — skipped"
		switch {
		case xdbInternal:
			reason = "Oracle XDB-internal trigger (`xdb.xdb_pitrig_pkg.*`) — auto-generated by Oracle for XML object tables, no PG equivalent. Skipped."
		case nested:
			reason = "Oracle `INSTEAD OF INSERT ON NESTED TABLE` has no PG equivalent (nested-table model is a plain array column with no per-row trigger semantics). Skipped."
		case !tableInPlan && !viewInPlan:
			reason = "trigger references `" + s.Table.Name + "` which is not in the migration plan (object/nested table not exportable) — skipped"
		}
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "trigger." + s.Name,
			Source: "CREATE TRIGGER",
			Target: "-- skipped",
			Reason: reason,
			Level:  "info",
		})
		// Mark the trigger as skipped so the matching ALTER TRIGGER
		// statement (which Oracle emits as a separate `ALTER TRIGGER
		// name ENABLE`) drops to an info-level explanation instead of
		// the unresolved-table blocking prereq.
		t.rememberTrigger(s.Name, skippedTriggerMarker)
		return
	}

	// COMPOUND TRIGGER has no PostgreSQL equivalent — Oracle wraps up to
	// four timing-point sections (BEFORE/AFTER EACH ROW/STATEMENT) under
	// a single declaration. We auto-split into N standalone CREATE
	// TRIGGER statements (one per timing-point section) when the body
	// matches the canonical shape. If the split fails (shared state in
	// the DECLARE preamble, unrecognised section, etc.) we fall back to
	// the manual-review prereq.
	if strings.EqualFold(s.Time, "COMPOUND") {
		if t.tryAutoSplitCompoundTrigger(s) {
			return
		}
		t.res.Plan.Routines = append(t.res.Plan.Routines, PGRoutine{
			Kind: "trigger", Schema: t.opt.TargetSchema, Name: s.Name,
			RawBody: s.Body,
			DDL: "-- Compound trigger " + s.Name + " was skipped: PostgreSQL has no compound-trigger\n" +
				"-- equivalent. Split the timing-point sections into N standard triggers\n" +
				"-- (one per BEFORE/AFTER EACH ROW/STATEMENT) and adapt the shared state\n" +
				"-- via a session-scoped table if needed.\n",
		})
		t.warnSev("trigger."+s.Name, "trigger.compound",
			"Oracle COMPOUND TRIGGER has no PG equivalent — skipped; split manually into N triggers",
			SeverityInfo)
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityBlocking,
			Category:    CatManualReview,
			Object:      "trigger." + s.Name,
			Title:       "Split Oracle COMPOUND TRIGGER " + s.Name + " into N PG triggers",
			Description: "Oracle COMPOUND TRIGGER bundles up to four timing-point sections (BEFORE STATEMENT, BEFORE EACH ROW, AFTER EACH ROW, AFTER STATEMENT) plus a shared declaration block under a single declaration. PostgreSQL has no compound trigger — each timing point requires its own CREATE TRIGGER, and shared state has to live somewhere both halves can reach (session-local temp table, GUC, or a per-session counter table).",
			Remediation: `For each timing-point section in the compound body, emit one PG trigger:

  CREATE FUNCTION ` + s.Name + `_before_each_row_fn() RETURNS trigger LANGUAGE plpgsql
  AS $$ … BEFORE EACH ROW body … $$;
  CREATE TRIGGER ` + s.Name + `_before_row BEFORE INSERT OR UPDATE OR DELETE
    ON <table> FOR EACH ROW EXECUTE FUNCTION ` + s.Name + `_before_each_row_fn();

  CREATE FUNCTION ` + s.Name + `_after_each_row_fn() …
  CREATE TRIGGER ` + s.Name + `_after_row AFTER … FOR EACH ROW …;

  CREATE FUNCTION ` + s.Name + `_before_stmt_fn() …
  CREATE TRIGGER ` + s.Name + `_before_stmt BEFORE … FOR EACH STATEMENT …;

For the shared DECLARE block, two patterns work:
  - In-memory: a session-local temp table (CREATE TEMP TABLE … ON COMMIT DROP)
    plus DELETE/INSERT for state writes. Cheap when the trigger fires often.
  - Per-statement: a transition table on the statement-level trigger
    (REFERENCING NEW TABLE AS new_rows) — populates a real relation that the
    AFTER STATEMENT body can iterate.

The compound source body is preserved in the routines plan as a comment so
you can copy/paste each section into the right place.`,
		})
		// Same trick as for the table-not-in-plan branch: record the
		// trigger as skipped so the matching `ALTER TRIGGER name ENABLE`
		// (which DBMS_METADATA emits as a separate statement) doesn't
		// fire its own redundant blocking "resolve the table" prereq.
		t.rememberTrigger(s.Name, skippedTriggerMarker)
		return
	}

	fnName := s.Name + "_fn"
	retStmt := "RETURN NEW;"
	if strings.EqualFold(s.Time, "AFTER") && strings.EqualFold(s.Event, "DELETE") {
		retStmt = "RETURN OLD;"
	}

	// Phase 3.8: thread REFERENCING aliases through to the AST visitor
	// orchestrator so the row-alias rename happens on the parsed Idents
	// before pgast translation. The legacy rewriteRowAlias text pass
	// stays as a safety net during the transition window — it's a no-op
	// when the AST visitor already substituted the names.
	pgBody, untranslated, notes, usedAdmin := TranslateRoutineBodyExtV(
		s.Body, t.opt.SourceKind, s.NewAlias, s.OldAlias,
	)
	pgBody = rewritePackageVarRefs(pgBody, t.packageVars)
	if usedAdmin {
		t.usedAdminpack = true
	}
	// Belt-and-suspenders: text-level row-alias rewrite catches any
	// reference the AST visitor missed (RawExpr-bound text, SQL strings,
	// etc.). Once Phase 5 retires the legacy passes the AST visitor
	// becomes the sole renamer.
	if s.NewAlias != "" && !strings.EqualFold(s.NewAlias, "NEW") {
		pgBody = rewriteRowAlias(pgBody, s.NewAlias, "NEW")
	}
	if s.OldAlias != "" && !strings.EqualFold(s.OldAlias, "OLD") {
		pgBody = rewriteRowAlias(pgBody, s.OldAlias, "OLD")
	}
	// If the incoming body already started with BEGIN/END, preserve it and
	// just append the RETURN statement. Otherwise wrap in BEGIN ... END.
	body := wrapTriggerBody(pgBody, retStmt)

	fn := &pgast.CreateFunction{
		Schema:  t.opt.TargetSchema,
		Name:    fnName,
		Returns: "trigger",
		Body:    body,
	}
	// Oracle reports table names in UPPERCASE (DBMS_METADATA convention) but
	// our CREATE TABLE DDL folds them to lowercase via normalizeOracleTable.
	// The trigger's ON clause must reference the PG-side name, not the Oracle
	// original, or PG will quote-search for "ORDERS" and fail.
	tblName := s.Table.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		tblName = normalizeOracleIdent(tblName)
	}
	// FOR EACH STATEMENT vs FOR EACH ROW: Oracle defaults to STATEMENT-level
	// when the clause is absent. PG defaults to STATEMENT too if FOR EACH is
	// omitted. We pass through the explicit choice when known.
	forEach := ""
	if s.HasForEach {
		if s.ForEachRow {
			forEach = "ROW"
		} else {
			forEach = "STATEMENT"
		}
	}
	trg := &pgast.CreateTrigger{
		Name:     s.Name,
		Timing:   s.Time,
		Event:    s.Event,
		Schema:   t.opt.TargetSchema,
		Table:    tblName,
		FnName:   fnName,
		ForEach:  forEach,
		WhenCond: s.WhenCond,
	}
	t.res.Plan.Routines = append(t.res.Plan.Routines, PGRoutine{
		Kind: "trigger", Schema: t.opt.TargetSchema, Name: s.Name,
		RawBody: s.Body,
		DDL:     pgast.Write([]pgast.Stmt{fn, trg}),
	})
	t.rememberTrigger(s.Name, tblName)
	for _, u := range untranslated {
		t.warn("trigger."+s.Name, "routine.untranslated_construct", u)
	}
	for _, n := range notes {
		t.warnSev("trigger."+s.Name, "routine.translated_with_remarks", n, SeverityInfo)
	}
	if s.Order == "FOLLOWS" || s.Order == "PRECEDES" {
		// Intrinsic PG limitation: PG orders same-event triggers
		// alphabetically by name. We translate Oracle's FOLLOWS/PRECEDES
		// by auto-renaming the dependent trigger so its alphabetical
		// order matches the requested ordering — same firing semantics
		// without forcing the user to do the rename by hand.
		//
		// FOLLOWS R : this trigger must fire AFTER R. Need name > R.
		// PRECEDES R: this trigger must fire BEFORE R. Need name < R.
		newName := autoRenameForOrder(s.Name, s.OrderRef, s.Order)
		if newName != "" && newName != s.Name {
			// Patch the last-emitted PGRoutine (the one we just appended
			// above) so the CREATE TRIGGER carries the new name.
			if n := len(t.res.Plan.Routines); n > 0 {
				r := &t.res.Plan.Routines[n-1]
				r.Name = newName
				r.DDL = strings.ReplaceAll(r.DDL, quoteIdent(s.Name), quoteIdent(newName))
			}
			// The matching `ALTER TRIGGER <orig> ENABLE` that DBMS_METADATA
			// emits separately can no longer find a trigger under the old
			// name (we renamed it) AND the ENABLE is a no-op anyway since
			// PG creates triggers enabled by default. Mark the original
			// name as skipped so translateAlterTrigger absorbs it as an
			// info-level explanation instead of trying to ALTER TABLE
			// orders ENABLE TRIGGER <orig> — which would (a) fail because
			// the trigger is now named differently, and (b) land in
			// PreActions before the CREATE TABLE for orders.
			t.rememberTrigger(s.Name, skippedTriggerMarker)
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "trigger." + s.Name,
				Source: "Oracle " + s.Order + " " + s.OrderRef,
				Target: "PG trigger renamed to `" + newName + "` to preserve alphabetical ordering",
				Reason: "PostgreSQL fires same-event triggers in alphabetical order; squishy renames the dependent trigger so its alphabetical position matches the Oracle FOLLOWS/PRECEDES intent. The matching ALTER TRIGGER ENABLE is dropped (PG creates triggers enabled by default).",
				Level:  "info",
			})
		} else {
			// Already in the right alphabetical order — just record an
			// info note so the user knows.
			t.warnSev("trigger."+s.Name, "trigger.follows",
				"Oracle "+s.Order+" "+s.OrderRef+" preserved by alphabetical ordering — no rename needed",
				SeverityInfo)
		}
	}
}

// autoRenameForOrder computes a renamed identifier for a trigger that
// has an Oracle FOLLOWS/PRECEDES dependency, so PG's alphabetical
// firing order matches the requested execution order. Returns the
// original name when no rename is needed.
//
//	FOLLOWS  R : name must be > R alphabetically. Prepend "zz_" if not.
//	PRECEDES R : name must be < R alphabetically. Prepend "aa_" if not.
//
// Names are compared and emitted lowercased — Oracle identifiers fold
// to lowercase on the PG side and that's also the order PG uses to
// schedule trigger firing.
func autoRenameForOrder(name, ref, order string) string {
	if ref == "" {
		return name
	}
	curLower := strings.ToLower(name)
	refLower := strings.ToLower(ref)
	switch strings.ToUpper(order) {
	case "FOLLOWS":
		if curLower > refLower {
			return name
		}
		return "zz_" + name
	case "PRECEDES":
		if curLower < refLower {
			return name
		}
		return "aa_" + name
	}
	return name
}

// tryAutoSplitCompoundTrigger attempts to split an Oracle COMPOUND
// TRIGGER body into one PG CREATE TRIGGER per timing-point section.
// Oracle's compound shape is:
//
//	[shared DECLARE block]
//	BEFORE STATEMENT IS BEGIN … END BEFORE STATEMENT;
//	BEFORE EACH ROW  IS BEGIN … END BEFORE EACH ROW;
//	AFTER  EACH ROW  IS BEGIN … END AFTER  EACH ROW;
//	AFTER  STATEMENT IS BEGIN … END AFTER  STATEMENT;
//	END <trigger_name>;
//
// PG models each timing-point as its own CREATE TRIGGER + plpgsql
// function. We extract each section's body, generate one PG trigger per
// section, then call rememberTrigger so the matching `ALTER TRIGGER
// name ENABLE` lands on the new schema instead of firing the
// unresolved-table prereq.
//
// Returns true on a successful split (caller short-circuits and skips
// the legacy blocking prereq path); returns false when the body has a
// non-empty shared DECLARE block (state we can't trivially route
// through PG), or when no recognised section is found.
func (t *translator) tryAutoSplitCompoundTrigger(s *ast.CreateTrigger) bool {
	sections := splitCompoundTriggerSections(s.Body)
	if len(sections) == 0 {
		return false
	}
	// Reject bodies whose preamble has anything other than whitespace and
	// `COMPOUND TRIGGER` — a real DECLARE block needs shared state we
	// can't auto-route. The user gets the original blocking prereq with
	// the manual-rewrite guidance.
	preamble := strings.ToUpper(strings.TrimSpace(sections[0].preamble))
	preamble = strings.TrimPrefix(preamble, "COMPOUND TRIGGER")
	preamble = strings.TrimSpace(preamble)
	if preamble != "" {
		return false
	}
	tblName := s.Table.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		tblName = normalizeOracleIdent(tblName)
	}
	for _, sec := range sections {
		fnName := strings.ToLower(s.Name) + "_" + sec.suffix + "_fn"
		retStmt := "RETURN NEW;"
		if strings.EqualFold(sec.timing, "AFTER") && strings.EqualFold(s.Event, "DELETE") {
			retStmt = "RETURN OLD;"
		}
		pgBody, untranslated, notes, usedAdmin := TranslateRoutineBodyExt(sec.body, t.opt.SourceKind)
		pgBody = rewritePackageVarRefs(pgBody, t.packageVars)
	if usedAdmin {
		t.usedAdminpack = true
	}
		body := wrapTriggerBody(pgBody, retStmt)

		fn := &pgast.CreateFunction{
			Schema:  t.opt.TargetSchema,
			Name:    fnName,
			Returns: "trigger",
			Body:    body,
		}
		forEach := "STATEMENT"
		if sec.eachRow {
			forEach = "ROW"
		}
		trgName := strings.ToLower(s.Name) + "_" + sec.suffix
		trg := &pgast.CreateTrigger{
			Name:    trgName,
			Timing:  sec.timing, // "BEFORE" | "AFTER"
			Event:   s.Event,
			Schema:  t.opt.TargetSchema,
			Table:   tblName,
			FnName:  fnName,
			ForEach: forEach,
		}
		t.res.Plan.Routines = append(t.res.Plan.Routines, PGRoutine{
			Kind: "trigger", Schema: t.opt.TargetSchema, Name: trgName,
			RawBody: sec.body,
			DDL:     pgast.Write([]pgast.Stmt{fn, trg}),
		})
		for _, u := range untranslated {
			t.warn("trigger."+s.Name+"."+sec.suffix, "routine.untranslated_construct", u)
		}
		for _, n := range notes {
			t.warnSev("trigger."+s.Name+"."+sec.suffix, "routine.translated_with_remarks", n, SeverityInfo)
		}
	}
	// Mark the original trigger name as skipped so that the matching
	// `ALTER TRIGGER trg_compound ENABLE` (emitted as a separate
	// statement by DBMS_METADATA) doesn't try to enable a trigger that
	// no longer exists under that name — the auto-split renamed each
	// section to `<orig>_<section>`. PG triggers are enabled by default
	// after CREATE, so the ALTER is a semantic no-op anyway; absorbing
	// it as an info-level explanation is the right call.
	t.rememberTrigger(s.Name, skippedTriggerMarker)
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "trigger." + s.Name,
		Source: "Oracle COMPOUND TRIGGER",
		Target: fmt.Sprintf("%d standalone PG triggers (one per timing-point section)", len(sections)),
		Reason: "Oracle COMPOUND TRIGGER auto-split into N CREATE TRIGGER statements — each timing-point section (BEFORE/AFTER STATEMENT/EACH ROW) becomes its own PG trigger function on the same table. No shared DECLARE state in this trigger, so the split is semantics-preserving.",
		Level:  "info",
	})
	return true
}

// compoundSection is one timing-point block extracted from an Oracle
// COMPOUND TRIGGER body.
type compoundSection struct {
	timing   string // "BEFORE" | "AFTER"
	eachRow  bool   // true for EACH ROW, false for STATEMENT
	suffix   string // "before_stmt" | "before_row" | "after_row" | "after_stmt"
	body     string // the BEGIN … END section content (raw PL/SQL)
	preamble string // text from the start of the trigger body up to the
	// first section header (only populated on the first
	// element so the caller can validate emptiness — the
	// shared DECLARE block lives here).
}

// splitCompoundTriggerSections walks the trigger body looking for the
// canonical timing-point sections and returns one entry per match. The
// returned slice is empty when the body doesn't follow Oracle's
// compound-trigger shape.
func splitCompoundTriggerSections(body string) []compoundSection {
	if body == "" {
		return nil
	}
	upper := strings.ToUpper(body)
	type hit struct {
		start, after int
		timing       string
		eachRow      bool
		suffix       string
	}
	var hits []hit
	headers := []struct {
		keyword string
		timing  string
		eachRow bool
		suffix  string
	}{
		{"BEFORE STATEMENT IS", "BEFORE", false, "before_stmt"},
		{"BEFORE EACH ROW IS", "BEFORE", true, "before_row"},
		{"AFTER EACH ROW IS", "AFTER", true, "after_row"},
		{"AFTER STATEMENT IS", "AFTER", false, "after_stmt"},
	}
	for _, h := range headers {
		// Allow extra whitespace between keywords by collapsing the
		// header to a regex-like scan: find each token in order.
		idx := indexHeaderSequence(upper, h.keyword)
		if idx < 0 {
			continue
		}
		hits = append(hits, hit{
			start:   idx,
			after:   idx + headerKeywordLen(upper, idx, h.keyword),
			timing:  h.timing,
			eachRow: h.eachRow,
			suffix:  h.suffix,
		})
	}
	if len(hits) == 0 {
		return nil
	}
	// Sort by start offset so the first hit also bounds the preamble.
	sort.Slice(hits, func(i, j int) bool { return hits[i].start < hits[j].start })
	preamble := body[:hits[0].start]
	out := make([]compoundSection, 0, len(hits))
	for i, h := range hits {
		// The body of section i runs from after the header up to the
		// matching `END BEFORE/AFTER STATEMENT/EACH ROW;`. We could
		// rely on the next header start, but trailing semicolons and
		// the closing `END <name>;` need explicit handling. Find the
		// END-of-section marker by scanning for `END` followed by the
		// same timing/scope keywords as the header.
		endMarker := strings.ToUpper(strings.Replace(headers[indexOfHeader(headers, h.suffix)].keyword, " IS", "", 1))
		// scan from after the header for `END <endMarker>;`
		body0 := h.after
		bodyEnd := -1
		j := h.after
		for j < len(body) {
			// skip strings
			if body[j] == '\'' {
				j++
				for j < len(body) && body[j] != '\'' {
					j++
				}
				if j < len(body) {
					j++
				}
				continue
			}
			if matchKeywordAt(upper, "END", j) {
				k := j + len("END")
				for k < len(body) && (body[k] == ' ' || body[k] == '\t' || body[k] == '\r' || body[k] == '\n') {
					k++
				}
				if k+len(endMarker) <= len(upper) && upper[k:k+len(endMarker)] == endMarker {
					bodyEnd = j
					break
				}
			}
			j++
		}
		if bodyEnd < 0 {
			// Malformed section — bail out.
			return nil
		}
		secBody := strings.TrimSpace(body[body0:bodyEnd])
		secBody = strings.TrimPrefix(secBody, "BEGIN")
		secBody = strings.TrimSpace(secBody)
		sec := compoundSection{
			timing:  h.timing,
			eachRow: h.eachRow,
			suffix:  h.suffix,
			body:    "BEGIN\n" + secBody + "\nEND;",
		}
		if i == 0 {
			sec.preamble = preamble
		}
		out = append(out, sec)
	}
	return out
}

// indexOfHeader returns the index of the header descriptor whose suffix
// matches `suffix` — small helper to keep the splitter readable.
func indexOfHeader(headers []struct {
	keyword string
	timing  string
	eachRow bool
	suffix  string
}, suffix string) int {
	for i, h := range headers {
		if h.suffix == suffix {
			return i
		}
	}
	return -1
}

// indexHeaderSequence finds the first whitespace-tolerant occurrence of
// `kw` (a multi-word phrase like "BEFORE EACH ROW IS") in `upper`. The
// match collapses any run of whitespace between the words, so `BEFORE
// \n\t  EACH  ROW   IS` matches `BEFORE EACH ROW IS`. Returns -1 when
// not found.
func indexHeaderSequence(upper, kw string) int {
	words := strings.Fields(kw)
	if len(words) == 0 {
		return -1
	}
	for i := 0; i < len(upper); {
		// find first word as whole word
		k := indexKeyword(upper, words[0], i)
		if k < 0 {
			return -1
		}
		j := k + len(words[0])
		ok := true
		for w := 1; w < len(words); w++ {
			for j < len(upper) && (upper[j] == ' ' || upper[j] == '\t' || upper[j] == '\r' || upper[j] == '\n') {
				j++
			}
			if !matchKeywordAt(upper, words[w], j) {
				ok = false
				break
			}
			j += len(words[w])
		}
		if ok {
			return k
		}
		i = k + len(words[0])
	}
	return -1
}

// headerKeywordLen returns the number of bytes consumed by a multi-word
// header at offset `at` — used to position the cursor at the start of
// the section body after the IS keyword.
func headerKeywordLen(upper string, at int, kw string) int {
	words := strings.Fields(kw)
	if len(words) == 0 {
		return 0
	}
	j := at + len(words[0])
	for w := 1; w < len(words); w++ {
		for j < len(upper) && (upper[j] == ' ' || upper[j] == '\t' || upper[j] == '\r' || upper[j] == '\n') {
			j++
		}
		j += len(words[w])
	}
	return j - at
}

// rewriteRowAlias replaces every word-boundary occurrence of `from` (case-
// insensitive) with `to` in a trigger body. Used to map Oracle's
// `REFERENCING NEW AS x OLD AS y` aliases back to PG's fixed NEW/OLD
// pseudorecord names. The substitution preserves quoted-string contents
// and existing identifiers that merely contain `from` as a substring.
func rewriteRowAlias(body, from, to string) string {
	if from == "" || strings.EqualFold(from, to) {
		return body
	}
	upper := strings.ToUpper(body)
	upperFrom := strings.ToUpper(from)
	var b strings.Builder
	b.Grow(len(body))
	inSingle := false
	for i := 0; i < len(body); {
		c := body[i]
		if inSingle {
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					b.WriteByte('\'')
					i += 2
					continue
				}
				inSingle = false
			}
			i++
			continue
		}
		if c == '\'' {
			b.WriteByte(c)
			inSingle = true
			i++
			continue
		}
		if i+len(upperFrom) <= len(body) && upper[i:i+len(upperFrom)] == upperFrom {
			leftOK := i == 0 || !isIdentChar(body[i-1])
			rightOK := i+len(upperFrom) == len(body) || !isIdentChar(body[i+len(upperFrom)])
			if leftOK && rightOK {
				b.WriteString(to)
				i += len(upperFrom)
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// wrapTriggerBody ensures the translated PL/pgSQL is a proper BEGIN ... END
// block and has a trailing RETURN statement (PG trigger functions must
// return NEW or OLD).
func wrapTriggerBody(pgBody, retStmt string) string {
	// Oracle accepts bare `RETURN;` inside a trigger body as an early
	// exit. PG's plpgsql refuses RETURN-without-expression in a function
	// returning `trigger` ("missing expression at or near ';'"). Promote
	// every bare RETURN to the trigger's natural return value (NEW for
	// INSERT/UPDATE/AFTER triggers, OLD for BEFORE DELETE) so the
	// auto-translated routine compiles. retStmt is the suffix the wrapper
	// appends — reuse the same expression for any inline early-return.
	if retExpr := strings.TrimSuffix(strings.TrimSpace(retStmt), ";"); retExpr != "" {
		pgBody = rewriteBareReturn(pgBody, retExpr)
	}
	trimmed := strings.TrimSpace(pgBody)
	// If the body is a naked single statement (no BEGIN), wrap it.
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "BEGIN") {
		return fmt.Sprintf("BEGIN\n  %s\n  %s\nEND;", trimmed, retStmt)
	}
	// Strip trailing END/END;/END <kw> so we can insert the RETURN before
	// the closing END. We scan backward over trailing whitespace, optional
	// trailing ';' and inter-token whitespace, then check for the keyword
	// END preceded by a non-identifier byte (so we do not match e.g. `LEGEND`).
	if cut := findTrailingEnd(trimmed); cut >= 0 {
		inner := strings.TrimSpace(trimmed[:cut])
		return fmt.Sprintf("%s\n  %s\nEND;", inner, retStmt)
	}
	return fmt.Sprintf("%s\n  %s", trimmed, retStmt)
}

// rewriteBareReturn replaces `RETURN;` (and `RETURN ;`) tokens at statement
// boundaries with `<retExpr>;` so an Oracle early-exit `return;` inside a
// trigger body becomes a valid PG `RETURN NEW;` (or RETURN OLD; depending
// on the trigger event). String literals are skipped so identifiers
// containing `RETURN` inside a string are left alone.
func rewriteBareReturn(s, retExpr string) string {
	upper := strings.ToUpper(s)
	var out strings.Builder
	out.Grow(len(s) + 8)
	i := 0
	inStr := false
	for i < len(s) {
		c := s[i]
		if inStr {
			out.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					out.WriteByte(s[i+1])
				i += 2
				continue
				}
				inStr = false
			}
			i++
			continue
		}
		if c == '\'' {
			out.WriteByte(c)
			inStr = true
			i++
			continue
		}
		// Match RETURN at a word boundary, followed by optional whitespace
		// and a `;` — the bare-return form to rewrite. Anything else
		// (RETURN expr; / RETURN INTO / RETURN QUERY ...) is left as-is.
		if i+6 <= len(upper) && upper[i:i+6] == "RETURN" {
			leftOK := i == 0 || !isIdentByte(s[i-1])
			j := i + 6
			rightOK := j == len(s) || !isIdentByte(s[j])
			if leftOK && rightOK {
				k := j
				for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
					k++
				}
				if k < len(s) && s[k] == ';' {
					out.WriteString("RETURN ")
					out.WriteString(retExpr)
					out.WriteByte(';')
					i = k + 1
					continue
				}
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// findTrailingEnd returns the byte offset of the trailing `END[;]` keyword in
// s, or -1 when there is none. Replacement for the previous regex
// `(?i)\bEND\s*;?\s*$`. Whitespace and an optional trailing semicolon after
// END are tolerated and excluded from the returned offset's tail content.
func findTrailingEnd(s string) int {
	i := len(s)
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\r' || s[i-1] == '\n') {
		i--
	}
	if i > 0 && s[i-1] == ';' {
		i--
		for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\r' || s[i-1] == '\n') {
			i--
		}
	}
	if i < 3 {
		return -1
	}
	tail := s[i-3 : i]
	if !strings.EqualFold(tail, "END") {
		return -1
	}
	if i-3 > 0 && isIdentByte(s[i-4]) {
		return -1
	}
	return i - 3
}

// ---------------------------------------------------------------------------
// Procedures / Functions
// ---------------------------------------------------------------------------

func (t *translator) translateProcedure(s *ast.CreateProcedure) {
	sig := pgProcSignature(s.Params, false, t.caps(), t.opt.SourceKind)
	pgBody, untranslated, notes, usedAdmin := TranslateRoutineBodyExtVS(s.Body, t.opt.SourceKind, "", "", t.opt.TargetSchema)
	pgBody = rewritePackageVarRefs(pgBody, t.packageVars)
	if usedAdmin {
		t.usedAdminpack = true
	}
	body := wrapProcedureBody(pgBody)

	node := &pgast.CreateProcedure{
		Schema:   t.opt.TargetSchema,
		Name:     s.Name,
		Params:   sig,
		Security: s.Characteristics.SQLSecurity,
		Body:     body,
	}
	t.res.Plan.Routines = append(t.res.Plan.Routines, PGRoutine{
		Kind: "procedure", Schema: t.opt.TargetSchema, Name: s.Name,
		Signature: sig, Security: s.Characteristics.SQLSecurity,
		RawBody: s.Body,
		DDL:     pgast.Write([]pgast.Stmt{node}),
	})
	for _, u := range untranslated {
		t.warn("procedure."+s.Name, "routine.untranslated_construct", u)
	}
	for _, n := range notes {
		t.warnSev("procedure."+s.Name, "routine.translated_with_remarks", n, SeverityInfo)
	}
}

// wrapProcedureBody ensures the translated text is a proper BEGIN ... END
// block; if the body was already a block, returns it unchanged.
func wrapProcedureBody(pgBody string) string {
	trimmed := strings.TrimSpace(pgBody)
	if strings.HasPrefix(strings.ToUpper(trimmed), "BEGIN") {
		return trimmed
	}
	if trimmed == "" {
		return "BEGIN\n  NULL;\nEND;"
	}
	return fmt.Sprintf("BEGIN\n  %s\nEND;", trimmed)
}

func (t *translator) translateFunction(s *ast.CreateFunction) {
	sig := pgProcSignature(s.Params, true, t.caps(), t.opt.SourceKind)
	ret := MapType(t.opt.SourceKind, s.Returns, "ret", t.caps()).PG
	vol := "VOLATILE"
	switch {
	case s.Characteristics.SQLDataAccess == "NO SQL" && s.Characteristics.Deterministic:
		vol = "IMMUTABLE"
	case s.Characteristics.SQLDataAccess == "READS SQL DATA":
		vol = "STABLE"
	case s.Characteristics.Deterministic && s.Characteristics.SQLDataAccess == "":
		vol = "IMMUTABLE"
	}
	pgBody, untranslated, notes, usedAdmin := TranslateRoutineBodyExtVS(s.Body, t.opt.SourceKind, "", "", t.opt.TargetSchema)
	pgBody = rewritePackageVarRefs(pgBody, t.packageVars)
	if usedAdmin {
		t.usedAdminpack = true
	}
	body := wrapProcedureBody(pgBody) // same wrapper logic

	node := &pgast.CreateFunction{
		Schema:   t.opt.TargetSchema,
		Name:     s.Name,
		Params:   sig,
		Returns:  ret,
		Volatile: vol,
		Security: s.Characteristics.SQLSecurity,
		Body:     body,
	}
	t.res.Plan.Routines = append(t.res.Plan.Routines, PGRoutine{
		Kind: "function", Schema: t.opt.TargetSchema, Name: s.Name,
		Signature: sig, Returns: ret, Volatile: vol,
		Security: s.Characteristics.SQLSecurity, RawBody: s.Body,
		DDL: pgast.Write([]pgast.Stmt{node}),
	})
	for _, u := range untranslated {
		t.warn("function."+s.Name, "routine.untranslated_construct", u)
	}
	for _, n := range notes {
		t.warnSev("function."+s.Name, "routine.translated_with_remarks", n, SeverityInfo)
	}
}

// ---------------------------------------------------------------------------
// Events → pg_cron suggestion
// ---------------------------------------------------------------------------

func (t *translator) translateEvent(s *ast.CreateEvent) {
	cronExpr := mysqlEveryToCron(s.EveryN, s.EveryUnit)

	var snippet string
	switch s.ScheduleKind {
	case "EVERY":
		snippet = fmt.Sprintf("SELECT cron.schedule('%s', '%s', $$ %s $$);",
			s.Name, cronExpr, strings.TrimSpace(s.Body))
	case "AT":
		// one-shot: schedule now() + small delay as-is — pg_cron doesn't
		// natively support one-shot; user should adjust.
		snippet = fmt.Sprintf("-- one-shot MySQL EVENT %s AT %s\nSELECT cron.schedule('%s', '<cron expr>', $$ %s $$);",
			s.Name, s.At, s.Name, strings.TrimSpace(s.Body))
	}

	if t.caps().HasPgCron && s.ScheduleKind == "EVERY" {
		// Schedule it for real — the create_routine step will execute this.
		t.res.Plan.Events = append(t.res.Plan.Events, PGEvent{
			Name: s.Name, PgCron: snippet, Comment: s.Comment,
		})
		t.res.Plan.PostActions = append(t.res.Plan.PostActions, snippet)
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "event." + s.Name,
			Source: "CREATE EVENT",
			Target: "cron.schedule('" + cronExpr + "')",
			Reason: "pg_cron is installed on target; the MySQL EVENT is scheduled automatically.",
			Level:  "info",
		})
		return
	}

	// pg_cron IS installed and schedule is AT (one-shot): emit a self-firing
	// + self-unscheduling DO block. PG evaluates the AT expression at run
	// time, derives the cron expression from it, schedules the job, and the
	// job's body unschedules itself after the first fire.
	if t.caps().HasPgCron && s.ScheduleKind == "AT" {
		atExpr := rewriteMySQLInterval(s.At)
		body := strings.TrimSpace(s.Body)
		if body == "" {
			body = "-- empty body"
		}
		selfScheduling := fmt.Sprintf(`DO $do$
DECLARE
  fire_at   TIMESTAMPTZ := %s;
  cron_expr TEXT := to_char(fire_at AT TIME ZONE 'UTC',
                            'MI FMHH24 FMDD FMMM') || ' *';
BEGIN
  PERFORM cron.schedule(%s, cron_expr,
    $job$ %s; PERFORM cron.unschedule(%s); $job$);
END
$do$;`, atExpr, sqlString(s.Name), body, sqlString(s.Name))

		t.res.Plan.Events = append(t.res.Plan.Events, PGEvent{
			Name: s.Name, PgCron: selfScheduling, Comment: s.Comment,
		})
		t.res.Plan.PostActions = append(t.res.Plan.PostActions, selfScheduling)
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "event." + s.Name,
			Source: "CREATE EVENT ... ON SCHEDULE AT " + s.At,
			Target: "DO block: schedule at computed cron expression, job body unschedules itself after firing",
			Reason: "pg_cron has no native one-shot mode, but a self-unscheduling wrapper simulates it faithfully: the body runs once at the target time, then removes its own cron entry.",
			Level:  "info",
		})
		return
	}

	// pg_cron missing or one-shot event: keep as prerequisite for the user.
	t.res.Plan.Events = append(t.res.Plan.Events, PGEvent{
		Name: s.Name, PgCron: snippet, Comment: s.Comment,
	})
	if !t.caps().HasPgCron {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "event." + s.Name,
			Source: "CREATE EVENT",
			Target: "pg_cron snippet (extension missing)",
			Reason: "MySQL EVENT scheduler has no PG counterpart. Install pg_cron and apply the provided snippet.",
			Level:  "warn",
		})
		t.warn("event."+s.Name, "event.pg_cron_required",
			"pg_cron extension not installed — event deferred; snippet provided for manual scheduling")
		return
	}
	// pg_cron IS installed but this is an AT (one-shot) event, which pg_cron
	// doesn't model natively. Distinguish from the "not installed" case so
	// the checklist remediation stays accurate.
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "event." + s.Name,
		Source: "CREATE EVENT ... ON SCHEDULE AT",
		Target: "manual cron.schedule with one-shot semantics",
		Reason: "pg_cron is installed but does not natively support one-shot AT schedules. Review the suggested snippet and adjust the cron expression for your desired fire time.",
		Level:  "warn",
	})
	t.warn("event."+s.Name, "event.one_shot",
		"one-shot MySQL EVENT AT ... requires manual cron expression in pg_cron")
}

// ---------------------------------------------------------------------------
// CREATE SEQUENCE (MariaDB 10.3+ / Oracle)
// ---------------------------------------------------------------------------

// translateSequence emits a Postgres CREATE SEQUENCE pre-action mirroring the
// source options. PG and MariaDB share the same option vocabulary (INCREMENT,
// MINVALUE / NO MINVALUE, MAXVALUE / NO MAXVALUE, START, CACHE, CYCLE), so the
// mapping is largely 1:1. PG has no `OR REPLACE` for sequences — when the
// source uses it we emit a `DROP SEQUENCE IF EXISTS` first to preserve the
// "replace" semantics.
func (t *translator) translateSequence(s *ast.CreateSequence) {
	// Always materialise the sequence in the target schema (`mig` by
	// default). Preserving the source-side schema (e.g. Oracle's `HR`)
	// would emit `CREATE SEQUENCE "HR".… ` against a Postgres database
	// that has no `HR` schema, which is what made the Oracle HR smoke
	// fail with `schema "HR" does not exist`. Sequence names are unique
	// per schema, so dropping the source qualifier is always correct.
	qname := fmt.Sprintf("%s.%s",
		quoteIdent(t.opt.TargetSchema), quoteIdent(s.Name))

	if s.OrReplace {
		t.res.Plan.PreActions = append(t.res.Plan.PreActions,
			fmt.Sprintf("DROP SEQUENCE IF EXISTS %s;", qname))
	}

	var b strings.Builder
	b.WriteString("CREATE ")
	if s.Temporary {
		b.WriteString("TEMPORARY ")
	}
	b.WriteString("SEQUENCE ")
	if s.IfNotExists || s.OrReplace {
		// IF NOT EXISTS is harmless under OR REPLACE — we just dropped it.
		b.WriteString("IF NOT EXISTS ")
	}
	b.WriteString(qname)
	if s.HasIncr {
		fmt.Fprintf(&b, " INCREMENT BY %d", s.Increment)
	}
	switch {
	case s.NoMin:
		b.WriteString(" NO MINVALUE")
	case s.HasMin:
		fmt.Fprintf(&b, " MINVALUE %d", s.MinValue)
	}
	switch {
	case s.NoMax:
		b.WriteString(" NO MAXVALUE")
	case s.HasMax:
		fmt.Fprintf(&b, " MAXVALUE %d", s.MaxValue)
	}
	if s.HasStart {
		fmt.Fprintf(&b, " START WITH %d", s.Start)
	}
	switch {
	case s.NoCache:
		// PG has no NO CACHE — CACHE 1 is the closest equivalent (the per-
		// session preallocation window is one value, i.e. effectively no cache).
		b.WriteString(" CACHE 1")
	case s.HasCache:
		fmt.Fprintf(&b, " CACHE %d", s.Cache)
	}
	if s.HasCycle {
		if s.Cycle {
			b.WriteString(" CYCLE")
		} else {
			b.WriteString(" NO CYCLE")
		}
	}
	b.WriteString(";")
	t.res.Plan.PreActions = append(t.res.Plan.PreActions, b.String())

	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "sequence." + s.Name,
		Source: "CREATE SEQUENCE",
		Target: "PG CREATE SEQUENCE",
		Reason: "MariaDB and PostgreSQL share the same sequence option vocabulary; the definition maps 1:1 (OR REPLACE is rewritten as DROP IF EXISTS + CREATE).",
		Level:  "info",
	})
	if len(s.IgnoredOptions) > 0 {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "sequence." + s.Name,
			Source: "CREATE SEQUENCE … " + strings.Join(s.IgnoredOptions, " "),
			Target: "(no PG equivalent — option dropped)",
			Reason: "PostgreSQL sequences have no equivalent of the Oracle-only options " + strings.Join(s.IgnoredOptions, ", ") + " (cluster-ordering / RAC affinity / scalable-sequence / sharing model). The bulk of the spec was emitted; these knobs were dropped.",
			Level:  "info",
		})
	}
}

// translateAlterSequence emits a Postgres ALTER SEQUENCE pre-action for
// each Oracle sequence option that has a PG counterpart. Most specs map
// 1:1; the Oracle-only knobs (ORDER/NOORDER/KEEP/NOKEEP/SCALE/NOSCALE/
// SHARD/SHARING/SESSION/GLOBAL) are listed in IgnoredOptions and surface
// as a single info-level explanation. Oracle's plain `ALTER SEQUENCE …
// START WITH N` is rewritten to PG's `RESTART WITH N` since PG ALTER
// SEQUENCE accepts START only at CREATE time.
func (t *translator) translateAlterSequence(s *ast.AlterSequence) {
	schema := s.Schema
	if schema == "" {
		schema = t.opt.TargetSchema
	}
	if dialects.IsOracle(t.opt.SourceKind) {
		schema = normalizeOracleIdent(schema)
	}
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	qname := fmt.Sprintf("%s.%s", quoteIdent(schema), quoteIdent(name))

	var b strings.Builder
	b.WriteString("ALTER SEQUENCE ")
	b.WriteString(qname)
	wrote := 0
	if s.HasIncr {
		fmt.Fprintf(&b, " INCREMENT BY %d", s.Increment)
		wrote++
	}
	switch {
	case s.NoMin:
		b.WriteString(" NO MINVALUE")
		wrote++
	case s.HasMin:
		fmt.Fprintf(&b, " MINVALUE %d", s.MinValue)
		wrote++
	}
	switch {
	case s.NoMax:
		b.WriteString(" NO MAXVALUE")
		wrote++
	case s.HasMax:
		fmt.Fprintf(&b, " MAXVALUE %d", s.MaxValue)
		wrote++
	}
	if s.HasRestart {
		if s.HasStartWith {
			fmt.Fprintf(&b, " RESTART WITH %d", s.StartWith)
		} else {
			b.WriteString(" RESTART")
		}
		wrote++
	}
	switch {
	case s.NoCache:
		b.WriteString(" CACHE 1")
		wrote++
	case s.HasCache:
		fmt.Fprintf(&b, " CACHE %d", s.Cache)
		wrote++
	}
	if s.HasCycle {
		if s.Cycle {
			b.WriteString(" CYCLE")
		} else {
			b.WriteString(" NO CYCLE")
		}
		wrote++
	}
	if wrote == 0 && len(s.IgnoredOptions) == 0 {
		// Nothing actionable in the ALTER — Oracle accepts a no-op `ALTER
		// SEQUENCE name;` (effectively a recompile); PG doesn't, so skip.
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "sequence." + name,
			Source: "ALTER SEQUENCE (no spec)",
			Target: "(skipped)",
			Reason: "Oracle accepts a bare `ALTER SEQUENCE name;` to bump its DDL timestamp / recompile dependencies. PostgreSQL has no equivalent and rejects an empty ALTER, so the statement is dropped.",
			Level:  "info",
		})
		return
	}
	if wrote > 0 {
		b.WriteString(";")
		t.res.Plan.PreActions = append(t.res.Plan.PreActions, b.String())
	}

	if len(s.IgnoredOptions) > 0 {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "sequence." + name,
			Source: "ALTER SEQUENCE " + strings.Join(s.IgnoredOptions, " "),
			Target: "(no PG equivalent — option dropped)",
			Reason: "PostgreSQL sequences have no equivalent of the Oracle-only options " + strings.Join(s.IgnoredOptions, ", ") + ". The remaining ALTER specs were translated; these knobs were dropped.",
			Level:  "info",
		})
	}

	if wrote > 0 {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "sequence." + name,
			Source: "ALTER SEQUENCE",
			Target: "PG ALTER SEQUENCE",
			Reason: "Oracle and PostgreSQL share the bulk of the sequence option vocabulary; the supported specs (INCREMENT/MIN/MAX/CACHE/CYCLE/RESTART) map directly. Oracle's plain `START WITH N` on ALTER is rewritten as PG `RESTART WITH N`.",
			Level:  "info",
		})
	}
}

// translateAlterIndex emits a PG ALTER INDEX pre-action for RENAME TO and
// drops every other Oracle action (REBUILD/COMPILE/ENABLE/DISABLE/UNUSABLE/
// VISIBLE/INVISIBLE/COMPRESS/PARTITION-level) with an info-level
// explanation describing why no PG equivalent is emitted.
func (t *translator) translateAlterIndex(s *ast.AlterIndex) {
	schema := s.Schema
	if schema == "" {
		schema = t.opt.TargetSchema
	}
	if dialects.IsOracle(t.opt.SourceKind) {
		schema = normalizeOracleIdent(schema)
	}
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	qname := fmt.Sprintf("%s.%s", quoteIdent(schema), quoteIdent(name))

	if s.Action == "RENAME" && s.NewName != "" {
		newName := s.NewName
		if dialects.IsOracle(t.opt.SourceKind) {
			newName = normalizeOracleIdent(newName)
		}
		t.res.Plan.PreActions = append(t.res.Plan.PreActions,
			fmt.Sprintf("ALTER INDEX %s RENAME TO %s;", qname, quoteIdent(newName)))
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "index." + name,
			Source: "ALTER INDEX … RENAME TO " + s.NewName,
			Target: "PG ALTER INDEX … RENAME TO …",
			Reason: "Oracle and PostgreSQL share the same RENAME-INDEX syntax — the rewrite is direct.",
			Level:  "info",
		})
		return
	}

	reason := alterIndexExplanation(s.Action)
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "index." + name,
		Source: "ALTER INDEX … " + s.Action + " " + s.RawTail,
		Target: "(no PG equivalent — dropped)",
		Reason: reason,
		Level:  "info",
	})
}

func alterIndexExplanation(action string) string {
	switch action {
	case "REBUILD":
		return "Oracle REBUILD recreates the B-tree to defragment / move tablespace / change compression. PostgreSQL has no DDL equivalent — REINDEX is a maintenance command (run at the psql prompt or via a separate cron) and does not need to be encoded in the schema migration."
	case "COMPILE":
		return "Oracle COMPILE re-validates the index against its base table. PostgreSQL revalidates indexes on demand at planner time and has no equivalent DDL — the statement is dropped."
	case "ENABLE", "DISABLE":
		return "Oracle indexes can be ENABLED / DISABLED to skip them at planner time. PostgreSQL indexes are always enabled (the planner uses them when costed favourably); use `SET enable_indexscan = off` per session for the same effect or DROP INDEX for permanent removal."
	case "VISIBLE", "INVISIBLE":
		return "Oracle's VISIBLE/INVISIBLE planner-visibility hint has no PostgreSQL counterpart. If you need the same effect, DROP INDEX leaves the table in the same state."
	case "UNUSABLE":
		return "Oracle UNUSABLE marks the index as unusable until rebuilt. PostgreSQL has no such state — the equivalent is DROP INDEX (then re-CREATE when ready)."
	case "COMPRESS", "NOCOMPRESS":
		return "Oracle key-compression for indexes has no direct PG equivalent — PG's btree storage is uncompressed. Consider lowering FILLFACTOR for less waste, or use a partial index if compression was meant to keep cardinality down."
	case "PARTITION":
		return "Oracle partition-level index DDL (ADD/MODIFY/SPLIT/COALESCE/DROP/MOVE PARTITION/SUBPARTITION) is dropped: PostgreSQL declarative partitioning manages child indexes implicitly via the parent partitioned index."
	default:
		return "Oracle ALTER INDEX action with no direct PostgreSQL equivalent — dropped from the migration plan."
	}
}

// translateAlterTrigger maps Oracle's `ALTER TRIGGER name <action>` to its
// PG counterpart. ENABLE/DISABLE → `ALTER TABLE tbl ENABLE/DISABLE TRIGGER
// name` (PG attaches triggers to tables). RENAME TO new → `ALTER TRIGGER
// name ON tbl RENAME TO new`. COMPILE has no PG equivalent (PG re-parses
// trigger functions on every CREATE — no recompile DDL needed) and is
// dropped with an info-level explanation.
//
// The PG syntax requires the table name. We resolve it from the
// translator's triggerTable map (populated by translateTrigger). If the
// trigger isn't in the migration plan (e.g. ALTER TRIGGER on a trigger
// that lives in a different schema or wasn't dumped), we surface a
// manual-review prereq instead of guessing.
func (t *translator) translateAlterTrigger(s *ast.AlterTrigger) {
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	tbl := ""
	if t.triggerTable != nil {
		tbl = t.triggerTable[strings.ToLower(name)]
	}

	if s.Action == "COMPILE" {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "trigger." + name,
			Source: "ALTER TRIGGER … COMPILE",
			Target: "(no PG equivalent — dropped)",
			Reason: "Oracle COMPILE re-validates the trigger function against its base table. PostgreSQL re-parses trigger functions on every CREATE FUNCTION, so there is no equivalent DDL — the statement is dropped.",
			Level:  "info",
		})
		return
	}

	if tbl == skippedTriggerMarker {
		// The matching CREATE TRIGGER was emitted as a no-op (XDB-internal,
		// nested-table INSTEAD OF, owning relation absent from the plan).
		// The ALTER TRIGGER … ENABLE that DBMS_METADATA emits alongside it
		// has nothing to attach to, but it's also not the user's problem
		// — surface as info instead of as a blocking prereq.
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "trigger." + name,
			Source: "ALTER TRIGGER " + s.Name + " " + s.Action,
			Target: "(no PG equivalent — dropped alongside CREATE TRIGGER)",
			Reason: "Matching CREATE TRIGGER was already skipped (Oracle XDB-internal trigger, INSTEAD OF on a nested table, or owning relation outside the migration plan). The ALTER TRIGGER side is dropped for the same reason.",
			Level:  "info",
		})
		return
	}
	if tbl == "" {
		// CREATE TRIGGER is not in this dump (different schema, missed
		// during export, etc.). Rather than block the user, emit a
		// runtime DO block that queries `pg_trigger` at apply time to
		// find the owning relation and execute the ALTER. If the
		// trigger doesn't exist either, it logs a NOTICE and exits
		// cleanly — non-blocking by design.
		stmt := emitAlterTriggerRuntimeFallback(s, name, t.opt.TargetSchema)
		if stmt == "" {
			// COMPILE etc. handled earlier; defensive fallthrough to a
			// soft note rather than a blocking prereq.
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "trigger." + name,
				Source: "ALTER TRIGGER " + s.Name + " " + s.Action,
				Target: "(no PG equivalent — dropped)",
				Reason: "Action has no PG analogue and the trigger's owning table couldn't be resolved from the dump.",
				Level:  "info",
			})
			return
		}
		t.res.Plan.PostActions = append(t.res.Plan.PostActions, stmt)
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "trigger." + name,
			Source: "ALTER TRIGGER " + s.Name + " " + s.Action,
			Target: "Runtime DO block (looks up owning table via pg_trigger)",
			Reason: "Le CREATE TRIGGER associé n'est pas dans ce dump, donc squishy ne connaît pas la table propriétaire au plan-time. Un bloc DO interroge pg_trigger à l'apply pour résoudre la relation et exécuter l'ALTER ; si le trigger n'existe pas non plus côté cible, le bloc émet un NOTICE et sort proprement (non bloquant).",
			Level:  "info",
		})
		return
	}

	// PG triggers on views (INSTEAD OF) are always enabled; ENABLE/DISABLE
	// TRIGGER is only valid for tables. If tbl resolves to a view in this
	// migration, drop the ALTER … ENABLE/DISABLE with an info-level
	// explanation rather than emitting DDL that PG rejects with SQLSTATE
	// 42809 ("ALTER action ENABLE TRIGGER cannot be performed on relation").
	if s.Action == "ENABLE" || s.Action == "DISABLE" {
		tblLower := strings.ToLower(tbl)
		for _, v := range t.res.Plan.Views {
			if strings.ToLower(v.Name) == tblLower {
				t.res.Explanations = append(t.res.Explanations, Explanation{
					Object: "trigger." + name,
					Source: "ALTER TRIGGER " + s.Name + " " + s.Action,
					Target: "(no PG equivalent — INSTEAD OF triggers on views are always enabled)",
					Reason: "PostgreSQL does not support ALTER … ENABLE/DISABLE TRIGGER on a view (SQLSTATE 42809). INSTEAD OF triggers fire whenever the view is written to; there is no enable/disable toggle. The ALTER is dropped — the trigger stays active.",
					Level:  "info",
				})
				return
			}
		}
	}

	tblQ := fmt.Sprintf("%s.%s", quoteIdent(t.opt.TargetSchema), quoteIdent(tbl))
	// Find the trigger's matching routine entry (added by translateTrigger
	// for the prior CREATE TRIGGER). The PGRoutine.Name field carries the
	// exact identifier used when emitting the CREATE TRIGGER — use it
	// verbatim so the ALTER statement quotes the same case-folded name as
	// the CREATE. Mismatched case (`"SECURE_EMPLOYEES"` vs
	// `"secure_employees"`) makes PG raise "trigger does not exist" because
	// quoted identifiers are case-sensitive.
	emitName := name
	routineIdx := -1
	for i := range t.res.Plan.Routines {
		r := &t.res.Plan.Routines[i]
		if r.Kind != "trigger" {
			continue
		}
		if !strings.EqualFold(r.Name, name) {
			continue
		}
		routineIdx = i
		emitName = r.Name
		break
	}

	var stmt string
	switch s.Action {
	case "ENABLE":
		stmt = fmt.Sprintf("ALTER TABLE %s ENABLE TRIGGER %s;", tblQ, quoteIdent(emitName))
	case "DISABLE":
		stmt = fmt.Sprintf("ALTER TABLE %s DISABLE TRIGGER %s;", tblQ, quoteIdent(emitName))
	case "RENAME":
		newName := s.NewName
		if dialects.IsOracle(t.opt.SourceKind) {
			newName = normalizeOracleIdent(newName)
		}
		stmt = fmt.Sprintf("ALTER TRIGGER %s ON %s RENAME TO %s;",
			quoteIdent(emitName), tblQ, quoteIdent(newName))
	}
	if stmt == "" {
		return
	}
	// Attach the ALTER statement to the matching trigger routine's DDL so
	// it lands in the same `create_routine` step as the CREATE TRIGGER it
	// follows (level 5 of the planner DAG — after CREATE TABLE at level 1
	// and FK validation at level 4). If for some reason the trigger is no
	// longer in t.res.Plan.Routines (e.g. translateTrigger emitted it under
	// a different name normalization), fall back to PreActions so the DDL
	// is at least preserved.
	if routineIdx >= 0 {
		r := &t.res.Plan.Routines[routineIdx]
		r.DDL = strings.TrimRight(r.DDL, " \t\r\n") + "\n" + stmt + "\n"
	} else {
		// No matching routine in this dump — the CREATE TRIGGER lives
		// elsewhere or is created at runtime (e.g. by an application
		// procedure like PRC_MAJ_TRC). Wrap in a runtime DO block that
		// no-ops if the trigger doesn't exist, so we don't sink the
		// migration with `relation does not exist` when the static
		// ALTER tries to run before the trigger is created. Lands in
		// PostActions (create_fk step level — still pre-routine, but
		// the DO block is self-resolving via pg_trigger).
		t.res.Plan.PostActions = append(t.res.Plan.PostActions,
			emitAlterTriggerRuntimeFallback(s, name, t.opt.TargetSchema))
	}
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "trigger." + name,
		Source: "ALTER TRIGGER … " + s.Action,
		Target: "PG ALTER TABLE/TRIGGER (" + s.Action + ")",
		Reason: "Oracle's schema-level ALTER TRIGGER is rewritten to PG's table-aware equivalent (PG attaches triggers to tables). Emitted alongside the matching CREATE TRIGGER so it runs after the owning table exists.",
		Level:  "info",
	})
}

// emitAlterTriggerRuntimeFallback returns a self-contained DO block that
// resolves the trigger's owning table via pg_trigger at apply time and
// runs the ALTER. Used when squishy didn't see the matching CREATE
// TRIGGER during translation — the trigger may still exist on the
// target (e.g. provisioned by a previous migration or by hand). The
// block is harmless if the trigger doesn't exist: it emits a NOTICE
// and returns. Returns "" for actions with no PG mapping (COMPILE).
//
// Generated form (for ENABLE, with `mig` as target schema):
//
//	DO $$
//	DECLARE r record;
//	BEGIN
//	  SELECT n.nspname AS schema, c.relname AS tbl
//	    INTO r
//	  FROM pg_trigger t
//	  JOIN pg_class c ON c.oid = t.tgrelid
//	  JOIN pg_namespace n ON n.oid = c.relnamespace
//	  WHERE t.tgname = 'aiu_art' AND NOT t.tgisinternal
//	  LIMIT 1;
//	  IF r IS NULL THEN
//	    RAISE NOTICE 'Trigger % introuvable — ALTER ignoré', 'aiu_art';
//	    RETURN;
//	  END IF;
//	  EXECUTE format('ALTER TABLE %I.%I ENABLE TRIGGER %I', r.schema, r.tbl, 'aiu_art');
//	END $$;
func emitAlterTriggerRuntimeFallback(s *ast.AlterTrigger, name, _targetSchema string) string {
	if s.Action == "COMPILE" {
		return ""
	}
	lower := strings.ToLower(name)
	var execStmt string
	switch s.Action {
	case "ENABLE":
		execStmt = fmt.Sprintf(
			"EXECUTE format('ALTER TABLE %%I.%%I ENABLE TRIGGER %%I', r.schema, r.tbl, %s);",
			sqlString(lower))
	case "DISABLE":
		execStmt = fmt.Sprintf(
			"EXECUTE format('ALTER TABLE %%I.%%I DISABLE TRIGGER %%I', r.schema, r.tbl, %s);",
			sqlString(lower))
	case "RENAME":
		newName := s.NewName
		// Apply the same Oracle-ident normalisation the static path uses.
		newName = strings.ReplaceAll(strings.ToLower(newName), "#", "_")
		execStmt = fmt.Sprintf(
			"EXECUTE format('ALTER TRIGGER %%I ON %%I.%%I RENAME TO %%I', %s, r.schema, r.tbl, %s);",
			sqlString(lower), sqlString(newName))
	default:
		return ""
	}
	return "DO $$\n" +
		"DECLARE r record;\n" +
		"BEGIN\n" +
		"  SELECT n.nspname AS schema, c.relname AS tbl\n" +
		"    INTO r\n" +
		"  FROM pg_trigger t\n" +
		"  JOIN pg_class c ON c.oid = t.tgrelid\n" +
		"  JOIN pg_namespace n ON n.oid = c.relnamespace\n" +
		"  WHERE t.tgname = " + sqlString(lower) + " AND NOT t.tgisinternal\n" +
		"  LIMIT 1;\n" +
		"  IF r IS NULL THEN\n" +
		"    RAISE NOTICE 'Trigger % not found — ALTER skipped', " + sqlString(lower) + ";\n" +
		"    RETURN;\n" +
		"  END IF;\n" +
		"  " + execStmt + "\n" +
		"END $$;\n"
}

// alterTriggerHint produces a PG-syntax template for the manual-review
// prerequisite when the trigger's owning table couldn't be auto-resolved.
func alterTriggerHint(s *ast.AlterTrigger) string {
	switch s.Action {
	case "ENABLE":
		return "ALTER TABLE <schema>.<table> ENABLE TRIGGER " + s.Name + ";"
	case "DISABLE":
		return "ALTER TABLE <schema>.<table> DISABLE TRIGGER " + s.Name + ";"
	case "RENAME":
		return "ALTER TRIGGER " + s.Name + " ON <schema>.<table> RENAME TO " + s.NewName + ";"
	}
	return "-- ALTER TRIGGER " + s.Name + " " + s.Action + " (no direct PG mapping)"
}

// translateAlterView drops Oracle's ALTER VIEW (COMPILE / EDITIONABLE /
// NONEDITIONABLE / ADD/MODIFY/DROP CONSTRAINT) with an info-level
// explanation. PostgreSQL's ALTER VIEW only supports RENAME / OWNER / SET
// SCHEMA / SET OPTIONS — none match Oracle's semantics, so re-running the
// CREATE OR REPLACE VIEW from the dump is the correct path.
func (t *translator) translateAlterView(s *ast.AlterView) {
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	reason := alterViewExplanation(s.Action)
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "view." + name,
		Source: "ALTER VIEW … " + s.Action,
		Target: "(no PG equivalent — dropped)",
		Reason: reason,
		Level:  "info",
	})
}

func alterViewExplanation(action string) string {
	switch action {
	case "COMPILE":
		return "Oracle COMPILE re-validates the view body against its base tables. PostgreSQL re-resolves view bodies on every CREATE OR REPLACE VIEW and reports invalid references immediately, so there is no equivalent DDL — the statement is dropped."
	case "EDITIONABLE", "NONEDITIONABLE":
		return "Oracle's edition-based redefinition (EBR) has no PostgreSQL equivalent — editioning is an Oracle-specific feature for online application upgrades. The flag is dropped from the migration plan."
	case "CONSTRAINT":
		return "Oracle ALTER VIEW … {ADD|MODIFY|DROP} CONSTRAINT manages declarative-but-unenforced view constraints. PostgreSQL has no corresponding feature; redefine the view via CREATE OR REPLACE VIEW if the constraint metadata matters to your tooling."
	default:
		return "Oracle ALTER VIEW action with no direct PostgreSQL equivalent — dropped from the migration plan."
	}
}

// translateAlterType handles Oracle's ALTER TYPE. COMPILE drops with an
// explanation. ATTRIBUTE evolution (ADD/MODIFY/DROP) emits a blocking
// manual-review prereq because Oracle's object-with-methods type system
// doesn't round-trip cleanly to PG composite types — even when the bare
// ATTRIBUTE syntax matches, dependent triggers/methods/inheritance
// references break under the rewrite.
func (t *translator) translateAlterType(s *ast.AlterType) {
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	if s.Action == "COMPILE" {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "type." + name,
			Source: "ALTER TYPE … COMPILE",
			Target: "(no PG equivalent — dropped)",
			Reason: "Oracle COMPILE re-validates the type body against its dependents. PostgreSQL re-checks composite-type usage at function-creation time, so there is no equivalent DDL — the statement is dropped.",
			Level:  "info",
		})
		return
	}
	t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
		Severity:    SeverityBlocking,
		Category:    CatManualReview,
		Object:      "type." + name,
		Title:       "Reapply Oracle ALTER TYPE manually",
		Description: "The Oracle dump issues 'ALTER TYPE " + s.Name + " " + s.Action + "'. Oracle's object-type system (with member methods, inheritance, and dependent table columns) doesn't round-trip to PostgreSQL composite types — even ATTRIBUTE-only changes break dependent functions and tables under the PG rewrite. squishy emitted the original CREATE TYPE; the post-CREATE evolution must be reapplied by hand.",
		Remediation: `For each Oracle ALTER TYPE … {ADD|MODIFY|DROP} ATTRIBUTE …:
  - PG composite types: ALTER TYPE name ADD/RENAME/DROP ATTRIBUTE …; CASCADE
  - PG cannot ALTER a composite type that's used as a column type if dependent
    rows exist — drop & recreate the table or migrate column data first.
For OVERRIDING METHOD / MEMBER PROCEDURE alterations: rewrite as standalone
PL/pgSQL functions (no methods on PG composite types).`,
	})
}

// translateAlterRoutine drops Oracle's `ALTER {PROCEDURE|FUNCTION|PACKAGE}
// … COMPILE …` with an info-level explanation. PostgreSQL re-parses
// PL/pgSQL function bodies on every CREATE FUNCTION (they're stored as
// text), so there is nothing to recompile after the fact.
func (t *translator) translateAlterRoutine(s *ast.AlterRoutine) {
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	objKind := strings.ToLower(s.Kind)
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: objKind + "." + name,
		Source: "ALTER " + s.Kind + " … COMPILE",
		Target: "(no PG equivalent — dropped)",
		Reason: "Oracle COMPILE re-validates the routine body against its dependents and stores the parse tree. PostgreSQL re-parses PL/pgSQL bodies on every CREATE FUNCTION (bodies are stored as text and reparsed at execution time), so there is no equivalent DDL — the statement is dropped.",
		Level:  "info",
	})
}

// translateCreateType maps Oracle's CREATE [OR REPLACE] TYPE to a PG
// composite type when the source is an OBJECT with only attribute
// declarations (no MEMBER FUNCTION/PROCEDURE/MAP/STATIC). Object types
// with methods, VARRAY, and TABLE OF surface a manual-review prereq:
//
//   - OBJECT (no methods)        → CREATE TYPE name AS (col type, …);
//   - OBJECT (with methods)      → blocking prereq (PG composite types
//     have no methods — promote each method to a standalone function)
//   - VARRAY(N) OF elem          → blocking prereq (PG arrays have no
//     declared maximum cardinality nor a stored "named array type")
//   - TABLE OF elem              → blocking prereq (similar — PG arrays
//     are anonymous; for true variable-length collections use plain
//     `<elem>[]` directly on the column / parameter)
func (t *translator) translateCreateType(s *ast.CreateType) {
	// CREATE TYPE always lands in the target migration schema — the source
	// schema (e.g. "OE") doesn't exist on the PG side. Tables already follow
	// this rule; without doing the same here, the emitted DDL referenced
	// "oe"."actions_t" etc. and failed at apply time with `schema "oe" does
	// not exist`.
	schema := t.opt.TargetSchema
	if dialects.IsOracle(t.opt.SourceKind) {
		schema = normalizeOracleIdent(schema)
	}
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	qname := fmt.Sprintf("%s.%s", quoteIdent(schema), quoteIdent(name))

	switch strings.ToUpper(s.Kind) {
	case "OBJECT":
		// Resolve attributes: parent attrs (if UNDER subtype) + own attrs.
		// Methods are dropped — TYPE BODY translation re-emits them as
		// standalone PG functions, so we don't need to remember them here.
		attrs := t.flattenObjectAttrs(s)
		if len(attrs) == 0 {
			// No data attributes anywhere in the inheritance chain — PG
			// composite types must have at least one column. Surface this
			// as the only legitimate "manual review" case for OBJECT
			// (Oracle marker / abstract types with no data).
			t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
				Severity:    SeverityInfo,
				Category:    CatManualReview,
				Object:      "type." + name,
				Title:       "Oracle OBJECT type " + s.Name + " has no data attributes",
				Description: "The OBJECT type carries only methods (MAP/ORDER/MEMBER FUNCTION/PROCEDURE), no attributes. PG composite types must declare at least one column, so no CREATE TYPE was emitted — methods translated by TYPE BODY land as standalone functions.",
			})
			return
		}
		// emitTypesInTopoOrder already pushed the CREATE TYPE DDL into
		// Plan.PreActions in dependency order (a composite that
		// references another composite as an attribute lands after its
		// dependency). Re-emitting here would create the type twice and
		// inflate the migration plan; only fall back to inline emission
		// if topo emission was skipped (defensive — emittedTypes is
		// always populated in the normal path).
		if !t.emittedTypes[userTypeKey(s.Name)] {
			parts := make([]string, 0, len(attrs))
			for _, attr := range attrs {
				attrName := attr.Name
				if dialects.IsOracle(t.opt.SourceKind) {
					attrName = normalizeOracleIdent(attrName)
				}
				typeRes := MapType(t.opt.SourceKind, attr.Type, attr.Name, t.caps())
				parts = append(parts, fmt.Sprintf("%s %s", quoteIdent(attrName), typeRes.PG))
			}
			ddl := fmt.Sprintf("CREATE TYPE %s AS (%s);", qname, strings.Join(parts, ", "))
			t.res.Plan.PreActions = append(t.res.Plan.PreActions, ddl)
			if t.emittedTypes != nil {
				t.emittedTypes[userTypeKey(s.Name)] = true
			}
		}
		t.registerUserType(s.Name, UserTypeRef{
			Kind:   "composite",
			Schema: schema,
			Name:   name,
		})
		reason := "Oracle attribute-only OBJECT types map directly to PG composite types — same field-list shape, distinct type identity."
		switch {
		case s.ParentType != "" && s.HasMethods:
			reason = "Oracle OBJECT subtype `UNDER " + s.ParentType + "` flattened into a PG composite (parent attributes inherited, methods dropped — TYPE BODY translation emits them as standalone functions)."
		case s.ParentType != "":
			reason = "Oracle OBJECT subtype `UNDER " + s.ParentType + "` flattened into a PG composite — PG composites have no inheritance, so the parent attributes are inlined."
		case s.HasMethods:
			reason = "Oracle OBJECT type with methods → PG composite (data-only, methods dropped here and re-emitted as standalone PG functions by TYPE BODY translation)."
		}
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "type." + name,
			Source: "CREATE TYPE … AS OBJECT",
			Target: "CREATE TYPE … AS (composite)",
			Reason: reason,
			Level:  "info",
		})
	case "VARRAY", "TABLE":
		// PG has no named-collection type, but every column / parameter
		// declared as INVENTORY_LIST_TYP / PHONE_LIST_TYP / … is rewritten
		// to an anonymous `<elem>[]` at the use site by the type-mapper
		// (see Caps.UserTypes wiring). Register the mapping for downstream
		// resolution, then surface this as info (not blocking) since the
		// translation is already automatic.
		elemPG, kind := t.renderCollectionElement(s.ElementType)
		severity := SeverityInfo
		if elemPG != "" {
			t.registerUserType(s.Name, UserTypeRef{
				Kind:   kind, // "array_scalar" | "array_composite"
				ElemPG: elemPG,
			})
		} else {
			// Couldn't classify the element (REF chains we don't model,
			// nested object-of-object, …) — keep blocking so the user
			// rewrites the use sites by hand.
			severity = SeverityBlocking
		}
		// Two flavours of prereq, depending on whether squishy could
		// resolve the element type to a concrete PG array form:
		//   - severity=info  : auto-replaced everywhere (use-site
		//                      rewriting via Caps.UserTypes). The user
		//                      doesn't have to do anything; the prereq
		//                      is purely informational so they know
		//                      where the rewrite landed.
		//   - severity=blocking: element couldn't be classified (REF
		//                      chains we don't model, nested object-of-
		//                      object, …). The user must rewrite the
		//                      use sites by hand.
		// Auto-resolved cases (severity=info, elemPG resolved) are emitted
		// as Explanations — there is no user action, so they should not
		// appear in the prerequisites checklist. Only the unclassifiable
		// (blocking) variant stays in Prerequisites.
		if severity == SeverityInfo && elemPG != "" {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "type." + name,
				Source: "CREATE TYPE " + s.Name + " AS " + s.Kind + " OF " + s.ElementType,
				Target: elemPG + "[] (rewritten at every use site)",
				Reason: "Oracle " + s.Kind + " OF " + s.ElementType + " est un type collection nommé sans équivalent direct en PostgreSQL (les tableaux PG sont anonymes). squishy a déjà réécrit chaque colonne/paramètre déclaré '" + name + "' en `" + elemPG + "[]` au site d'utilisation — aucune action manuelle requise.",
				Level:  "info",
			})
		} else {
			t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
				Severity:    severity,
				Category:    CatManualReview,
				Object:      "type." + name,
				Title:       "Remplacer manuellement le type Oracle " + s.Kind + " " + s.Name,
				Description: "Oracle " + s.Kind + " OF " + s.ElementType + " est un type collection nommé. squishy n'a pas su classer l'élément (REF chaîné, objet-d'objet imbriqué, …) — la réécriture automatique en `<elem>[]` n'a pas pu s'appliquer ; les sites d'utilisation de '" + name + "' sont à corriger à la main.",
				Remediation: `Supprimer la déclaration de type standalone. À chaque site d'utilisation, remplacer
  ` + name + `
par la forme tableau PG
  <type_élément>[]

Pour le type ELEMENT Oracle ` + s.ElementType + ` :
  - Scalaire (NUMBER/VARCHAR2/DATE) → numeric[] / text[] / date[]
  - Type objet → <type_objet>[] (composite-of-array PG)

Les bornes de cardinalité (le N du VARRAY) ne sont pas appliquées en PG ;
ajouter manuellement un CHECK si tu veux limiter la longueur.`,
			})
		}
	default:
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "type." + name,
			Source: "CREATE TYPE",
			Target: "(unrecognised TYPE kind — dropped)",
			Reason: "The CREATE TYPE didn't match any known kind (OBJECT/VARRAY/TABLE) — likely an Oracle 23c construct (subtype, ANYTYPE…) we don't model yet.",
			Level:  "warn",
		})
	}
}

// translateCreateTypeBody splits Oracle's TYPE BODY (member-function
// implementations) into one standalone PG function per member and emits each
// as a routine the migration plan creates after the composite type. The
// rewrite:
//
//   - Method  → CREATE FUNCTION <type>_<method>(self <type>, …)
//   - Body    → translated PL/SQL → PL/pgSQL via TranslateRoutineBody
//   - SELF    → preserved as the first parameter; PG composite-attribute
//               access (`self.attr`) works the same as Oracle's qualified
//               SELF reference.
//
// Methods that don't parse cleanly fall through to an info explanation
// carrying the original raw block, so a user can still finish the rewrite
// by hand without losing the source.
func (t *translator) translateCreateTypeBody(s *ast.CreateTypeBody) {
	name := s.Name
	if dialects.IsOracle(t.opt.SourceKind) {
		name = normalizeOracleIdent(name)
	}
	schema := t.opt.TargetSchema
	if dialects.IsOracle(t.opt.SourceKind) {
		schema = normalizeOracleIdent(schema)
	}
	composite := pgQualified(schema, name)

	methods := splitTypeBodyMethods(s.Body)
	if len(methods) == 0 {
		// Empty / non-method body — surface as info (PG has nothing to
		// emit) instead of a blocker.
		t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
			Severity:    SeverityInfo,
			Category:    CatManualReview,
			Object:      "type_body." + name,
			Title:       "TYPE BODY " + s.Name + " contained no extractable methods",
			Description: "squishy couldn't isolate any MEMBER FUNCTION / MEMBER PROCEDURE blocks inside this TYPE BODY — the body may use a construct (MAP, ORDER, CONSTRUCTOR with overloaded signatures, …) we don't yet split. The full body is preserved on the prerequisite so it can be reviewed by hand.",
		})
		return
	}
	for _, m := range methods {
		fnName := strings.ToLower(name + "_" + m.Name)
		var paramParts []string
		paramParts = append(paramParts, "self "+composite)
		for _, p := range m.Params {
			pgType := mapInlineOracleType(p.RawType)
			dir := strings.ToUpper(strings.TrimSpace(p.Direction))
			pname := p.Name
			if dialects.IsOracle(t.opt.SourceKind) {
				pname = normalizeOracleIdent(pname)
			}
			if dir == "" || dir == "IN" {
				paramParts = append(paramParts, quoteIdent(pname)+" "+pgType)
			} else {
				paramParts = append(paramParts, dir+" "+quoteIdent(pname)+" "+pgType)
			}
		}
		returns := ""
		if m.Kind == "FUNCTION" {
			rt := strings.TrimSpace(m.Return)
			if rt == "" {
				returns = "text"
			} else {
				returns = mapInlineOracleType(rt)
			}
		}
		pgBody, untranslated, notes, usedAdmin := TranslateRoutineBodyExt(m.Body, t.opt.SourceKind)
		pgBody = rewritePackageVarRefs(pgBody, t.packageVars)
	if usedAdmin {
		t.usedAdminpack = true
	}
		body := wrapProcedureBody(pgBody)

		var ddl string
		if returns != "" {
			ddl = fmt.Sprintf(
				"CREATE OR REPLACE FUNCTION %s.%s(%s) RETURNS %s LANGUAGE plpgsql AS $$\n%s\n$$;",
				quoteIdent(schema), quoteIdent(fnName),
				strings.Join(paramParts, ", "), returns, body,
			)
		} else {
			ddl = fmt.Sprintf(
				"CREATE OR REPLACE PROCEDURE %s.%s(%s) LANGUAGE plpgsql AS $$\n%s\n$$;",
				quoteIdent(schema), quoteIdent(fnName),
				strings.Join(paramParts, ", "), body,
			)
		}
		t.res.Plan.Routines = append(t.res.Plan.Routines, PGRoutine{
			Kind:    strings.ToLower(m.Kind),
			Schema:  schema,
			Name:    fnName,
			RawBody: m.Body,
			DDL:     ddl,
		})
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "type_body." + name + "." + m.Name,
			Source: m.Kind + " " + m.Name + " (Oracle TYPE BODY member)",
			Target: "CREATE FUNCTION " + fnName + "(self " + composite + ", …)",
			Reason: "Oracle TYPE BODY methods promoted to standalone PG functions: PG composite types have no method storage, so the receiver becomes the first explicit argument and the body is translated to PL/pgSQL.",
			Level:  "info",
		})
		for _, u := range untranslated {
			t.warn("type_body."+name+"."+m.Name, "routine.untranslated_construct", u)
		}
		for _, n := range notes {
			t.warnSev("type_body."+name+"."+m.Name, "routine.translated_with_remarks", n, SeverityInfo)
		}
	}
	// One info-level Explanation per body so the user sees what was done
	// without it polluting the prerequisites checklist (these are
	// auto-resolved — there is nothing for the user to do).
	plural := ""
	if len(methods) > 1 {
		plural = "s"
	}
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "type_body." + name,
		Source: fmt.Sprintf("TYPE BODY %s (%d MEMBER FUNCTION/PROCEDURE)", s.Name, len(methods)),
		Target: fmt.Sprintf("%d standalone PG function%s (`<type>_<method>(self <type>, …)`)", len(methods), plural),
		Reason: "Chaque MEMBER FUNCTION / MEMBER PROCEDURE a été émise comme `<type>_<méthode>(self <type>, …)` dans le plan. Aucune action obligatoire — relire les routines générées pour vérifier les références qualifiées par SELF et les éventuels constructs PL/SQL Oracle marqués comme non traduits dans les explanations.",
		Level:  "info",
	})
}

// extractedMethod is one MEMBER FUNCTION / PROCEDURE pulled out of an
// Oracle TYPE BODY by splitTypeBodyMethods.
type extractedMethod struct {
	Kind   string         // "FUNCTION" | "PROCEDURE"
	Name   string
	Params []extractedParam // best-effort; empty on parse failure
	Return string         // raw return-type text for FUNCTION; empty for PROCEDURE
	Body   string         // PL/SQL body between IS|AS … END[;]
}

// extractedParam is the lightweight (name, direction, raw-type-text) shape
// we lift out of TYPE BODY method headers. Avoids round-tripping through a
// proper ast.DataType — TYPE BODY signatures only need to land verbatim
// in the emitted CREATE FUNCTION header.
type extractedParam struct {
	Name      string
	Direction string // "" | "IN" | "OUT" | "IN OUT"
	RawType   string
}

// splitTypeBodyMethods scans the raw text of a TYPE BODY and returns one
// `extractedMethod` per MEMBER FUNCTION / PROCEDURE / MAP / ORDER / STATIC
// implementation found. Best-effort regex-style scanner: it tracks BEGIN/END
// nesting to find each method's terminating END, but doesn't attempt to
// parse complex PL/SQL constructs the surrounding pipeline already handles.
func splitTypeBodyMethods(body string) []extractedMethod {
	if body == "" {
		return nil
	}
	src := body
	upper := strings.ToUpper(src)
	var out []extractedMethod
	i := 0
	for i < len(src) {
		// Scan for the next method header keyword. Method specs may be
		// preceded by OVERRIDING / FINAL / NOT / INSTANTIABLE / STATIC /
		// MEMBER / MAP / ORDER / CONSTRUCTOR. We look for FUNCTION /
		// PROCEDURE and walk back to confirm a method-like prefix.
		idxFn := indexKeyword(upper, "FUNCTION", i)
		idxPr := indexKeyword(upper, "PROCEDURE", i)
		idx := -1
		kind := ""
		switch {
		case idxFn >= 0 && (idxPr < 0 || idxFn < idxPr):
			idx, kind = idxFn, "FUNCTION"
		case idxPr >= 0:
			idx, kind = idxPr, "PROCEDURE"
		default:
			break
		}
		if idx < 0 {
			break
		}
		// Skip past the keyword to read the method name.
		nameStart := idx + len(kind)
		for nameStart < len(src) && isSpaceByte(src[nameStart]) {
			nameStart++
		}
		nameEnd := nameStart
		for nameEnd < len(src) && (isIdentByte(src[nameEnd]) || src[nameEnd] == '"') {
			nameEnd++
		}
		if nameStart == nameEnd {
			i = idx + len(kind)
			continue
		}
		methodName := strings.Trim(src[nameStart:nameEnd], `"`)
		// Optional parameter list.
		j := nameEnd
		for j < len(src) && isSpaceByte(src[j]) {
			j++
		}
		var rawParams string
		if j < len(src) && src[j] == '(' {
			depth := 1
			k := j + 1
			for k < len(src) && depth > 0 {
				switch src[k] {
				case '(':
					depth++
				case ')':
					depth--
				}
				k++
			}
			rawParams = src[j+1 : k-1]
			j = k
		}
		for j < len(src) && isSpaceByte(src[j]) {
			j++
		}
		// Optional `RETURN <type>` for FUNCTION.
		var retRaw string
		if kind == "FUNCTION" {
			if matchKeywordAt(upper, "RETURN", j) {
				j += len("RETURN")
				for j < len(src) && isSpaceByte(src[j]) {
					j++
				}
				rs := j
				for j < len(src) && !isSpaceByte(src[j]) && src[j] != ';' {
					j++
				}
				retRaw = src[rs:j]
				for j < len(src) && isSpaceByte(src[j]) {
					j++
				}
			}
		}
		// `IS` or `AS`.
		if matchKeywordAt(upper, "IS", j) {
			j += 2
		} else if matchKeywordAt(upper, "AS", j) {
			j += 2
		} else {
			// No body — declaration only (shouldn't happen in TYPE BODY,
			// but be defensive). Skip.
			i = j
			continue
		}
		// Body: walk to matching END at depth 0 (counting BEGIN/END).
		bodyStart := j
		depth := 0
		k := j
		for k < len(src) {
			if matchKeywordAt(upper, "BEGIN", k) {
				depth++
				k += len("BEGIN")
				continue
			}
			if matchKeywordAt(upper, "END", k) {
				if depth == 0 {
					// outer END for this method — eat name, ;
					end := k + len("END")
					for end < len(src) && isSpaceByte(src[end]) {
						end++
					}
					// optional method name after END
					for end < len(src) && (isIdentByte(src[end]) || src[end] == '"') {
						end++
					}
					if end < len(src) && src[end] == ';' {
						end++
					}
					out = append(out, extractedMethod{
						Kind:   kind,
						Name:   methodName,
						Params: parseSimpleParams(rawParams),
						Return: retRaw,
						Body:   strings.TrimSpace(src[bodyStart:k]),
					})
					i = end
					goto next
				}
				depth--
				k += len("END")
				// optional label/name then `;`
				for k < len(src) && isSpaceByte(src[k]) {
					k++
				}
				for k < len(src) && (isIdentByte(src[k]) || src[k] == '"') {
					k++
				}
				if k < len(src) && src[k] == ';' {
					k++
				}
				continue
			}
			// Skip strings to avoid mistaking 'BEGIN' inside a literal.
			if src[k] == '\'' {
				k++
				for k < len(src) && src[k] != '\'' {
					k++
				}
				if k < len(src) {
					k++
				}
				continue
			}
			k++
		}
		// Reached EOF without closing END — bail.
		break
	next:
	}
	return out
}

// parseSimpleParams parses an Oracle MEMBER-method parameter list of the
// shape `(p1 [IN|OUT|IN OUT] type1, p2 type2, …)`. Returns the lightweight
// extractedParam shape — the type text is preserved verbatim so the
// caller's mapInlineOracleType() can render the PG counterpart.
func parseSimpleParams(raw string) []extractedParam {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []extractedParam
	parts, err := splitTopLevel(raw, ',')
	if err != nil {
		// Unbalanced parens — fall back to a naive comma split rather than
		// dropping the param list entirely.
		parts = strings.Split(raw, ",")
	}
	for _, part := range parts {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		p := extractedParam{Name: fields[0]}
		i := 1
		var dirTokens []string
		for i < len(fields) {
			up := strings.ToUpper(fields[i])
			if up == "IN" || up == "OUT" || up == "INOUT" {
				dirTokens = append(dirTokens, up)
				i++
				continue
			}
			break
		}
		if len(dirTokens) > 0 {
			p.Direction = strings.Join(dirTokens, " ")
		}
		p.RawType = strings.TrimSpace(strings.Join(fields[i:], " "))
		out = append(out, p)
	}
	return out
}


// mapInlineOracleType is a small stringly-typed mapper used when the parser
// gives us raw type text (e.g. RETURN clause of a TYPE BODY method) rather
// than a structured ast.DataType. Covers the cases the OE schema exercises;
// anything unknown falls through to "text".
func mapInlineOracleType(raw string) string {
	r := strings.ToUpper(strings.TrimSpace(raw))
	r = strings.TrimSuffix(r, ";")
	switch {
	case strings.HasPrefix(r, "VARCHAR2") || strings.HasPrefix(r, "VARCHAR") || strings.HasPrefix(r, "NVARCHAR2") || strings.HasPrefix(r, "NVARCHAR"):
		return "text"
	case strings.HasPrefix(r, "CHAR"), strings.HasPrefix(r, "NCHAR"):
		return "text"
	case strings.HasPrefix(r, "NUMBER"), strings.HasPrefix(r, "NUMERIC"), strings.HasPrefix(r, "DECIMAL"):
		return "numeric"
	case r == "INTEGER" || r == "INT" || r == "PLS_INTEGER" || r == "BINARY_INTEGER":
		return "integer"
	case r == "DATE":
		return "timestamp(0)"
	case strings.HasPrefix(r, "TIMESTAMP"):
		return "timestamp"
	case r == "BOOLEAN":
		return "boolean"
	case r == "CLOB" || r == "NCLOB" || r == "LONG":
		return "text"
	case r == "BLOB" || r == "RAW":
		return "bytea"
	case r == "FLOAT" || r == "REAL" || r == "BINARY_FLOAT" || r == "BINARY_DOUBLE":
		return "double precision"
	}
	return "text"
}

// indexKeyword returns the byte offset of a whole-word keyword match in
// upper-cased haystack starting from off, or -1 if not found.
func indexKeyword(upper, kw string, off int) int {
	if off < 0 {
		off = 0
	}
	for i := off; ; {
		j := strings.Index(upper[i:], kw)
		if j < 0 {
			return -1
		}
		k := i + j
		left := k == 0 || !isIdentByte(upper[k-1])
		right := k+len(kw) == len(upper) || !isIdentByte(upper[k+len(kw)])
		if left && right {
			return k
		}
		i = k + len(kw)
	}
}

// matchKeywordAt is true when the (already upper-cased) haystack contains
// kw as a whole word starting at offset off.
func matchKeywordAt(upper, kw string, off int) bool {
	if off < 0 || off+len(kw) > len(upper) {
		return false
	}
	if upper[off:off+len(kw)] != kw {
		return false
	}
	right := off+len(kw) == len(upper) || !isIdentByte(upper[off+len(kw)])
	left := off == 0 || !isIdentByte(upper[off-1])
	return left && right
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// translateNoop surfaces statements that the parser captured but that have no
// PG counterpart. Most NoopStmts (GRANT, FLUSH, START SLAVE, …) are intentional
// drops — silent is fine. A handful (CREATE PACKAGE / PACKAGE BODY) carry
// real semantic content the user almost certainly cares about; for those we
// emit a structured warning so the prerequisites checklist surfaces them.
func (t *translator) translateNoop(s *ast.NoopStmt) {
	switch strings.ToUpper(s.Kind) {
	case "CREATE PACKAGE", "CREATE PACKAGE BODY":
		// Extract the package name from the captured text (parseCreatePackageNoop
		// places it as the third whitespace-separated token: e.g. "CREATE PACKAGE
		// foo AS …" or "CREATE PACKAGE BODY foo AS …").
		name := extractPackageName(s.Text, s.Kind)
		object := "package"
		if name != "" {
			object = "package." + name
		}
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: object,
			Source: s.Kind,
			Target: "(dropped — Oracle-compat MariaDB package)",
			Reason: "MariaDB Oracle-compatibility CREATE PACKAGE / PACKAGE BODY (sql_mode=ORACLE) has no PG equivalent. Each routine in the package must be split into a top-level PG function/procedure manually.",
			Level:  "warn",
		})
		t.warn(object, "package.unsupported",
			"MariaDB CREATE PACKAGE/PACKAGE BODY has no PG equivalent — split into individual PG routines manually")
	case "CREATE MATERIALIZED VIEW LOG":
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "mview_log",
			Source: "CREATE MATERIALIZED VIEW LOG",
			Target: "(no PG equivalent — dropped)",
			Reason: "Oracle MATERIALIZED VIEW LOG is a per-base-table change journal that powers FAST refresh of materialized views. PostgreSQL materialized views only support full REFRESH (REFRESH MATERIALIZED VIEW [CONCURRENTLY]) — there is no incremental refresh, so the per-base log has nothing to feed. Drop or schedule the refresh appropriately for your workload.",
			Level:  "info",
		})
	case "ALTER MATERIALIZED VIEW", "ALTER MATERIALIZED":
		// ALTER MATERIALIZED VIEW <name> {COMPILE|REFRESH ...} — refresh
		// policy changes have no PG counterpart (REFRESH is a separate
		// command, not stored on the MV). COMPILE → same story as views.
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "mview",
			Source: s.Kind,
			Target: "(no PG equivalent — dropped)",
			Reason: "Oracle ALTER MATERIALIZED VIEW manages per-MV refresh policies (REFRESH FAST/COMPLETE/FORCE ON COMMIT/DEMAND), build options, and COMPILE. PostgreSQL has no stored refresh policy on the MV — refresh is invoked explicitly via `REFRESH MATERIALIZED VIEW [CONCURRENTLY]`. The statement is dropped; schedule refreshes via cron / pg_cron / your application's job runner instead.",
			Level:  "info",
		})
	case "ALTER MATERIALIZED VIEW LOG":
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "mview_log",
			Source: s.Kind,
			Target: "(no PG equivalent — dropped)",
			Reason: "PostgreSQL materialized views have no per-base-table change-log sidecar — see the matching CREATE MATERIALIZED VIEW LOG explanation. The ALTER is dropped.",
			Level:  "info",
		})
	case "CREATE CLUSTER":
		t.noopExplain("cluster", s.Kind,
			"Oracle CLUSTERs co-locate rows from multiple tables sharing a key. PostgreSQL has no equivalent storage-level construct — the same locality can be approximated with `CLUSTER tbl USING <pk_index>` (one-shot reordering) per table.")
	case "CREATE CONTEXT":
		t.noopExplain("context", s.Kind,
			"Oracle CONTEXT (sys_context namespace) has no PG counterpart. Use `current_setting('squishy.<key>', true)` per session, or a custom GUC class registered via `ALTER DATABASE … SET …`.")
	case "CREATE DIRECTORY":
		t.noopExplain("directory", s.Kind,
			"Oracle DIRECTORYs name OS paths for external table / UTL_FILE access. PG has no DDL equivalent — server-side path access goes through the postgres user's filesystem permissions plus extensions like file_fdw or pg_read_server_files.")
	case "CREATE LIBRARY":
		t.noopExplain("library", s.Kind,
			"Oracle LIBRARYs alias OS shared libraries for external procedures. PostgreSQL loads C extensions via `CREATE EXTENSION` from preinstalled .so files; the dynamic-load DDL doesn't translate.")
	case "CREATE JAVA":
		t.noopExplain("java", s.Kind,
			"Oracle JVM-stored Java has no PG counterpart. Reimplement the Java procedure as a PL/pgSQL or PL/Python function, or expose it via `CREATE EXTENSION pljava` (third-party).")
	case "CREATE DATABASE LINK":
		t.noopExplain("dblink", s.Kind,
			"Oracle DATABASE LINK ↔ PG postgres_fdw / dblink. Auto-translation is out of scope (different connection-string format, auth model). Install postgres_fdw + CREATE SERVER + CREATE USER MAPPING + CREATE FOREIGN TABLE manually for cross-database access.")
	case "CREATE DATABASE", "CREATE PLUGGABLE DATABASE":
		t.noopExplain("database", s.Kind,
			"Oracle's CREATE DATABASE / CREATE PLUGGABLE DATABASE provisions a multi-tenant container. PG databases are created out-of-band (`createdb` / `CREATE DATABASE` at the cluster level) — the dump's CREATE DATABASE statement is dropped; create the target DB before running the migration.")
	case "CREATE PROFILE", "CREATE LOCKDOWN PROFILE":
		t.noopExplain("profile", s.Kind,
			"Oracle PROFILE / LOCKDOWN PROFILE manages per-user resource limits and feature lockdown. PostgreSQL exposes resource limits via `ALTER ROLE` (CONNECTION LIMIT, statement_timeout, etc.); reimplement the relevant limits role-by-role.")
	case "CREATE EDITION":
		t.noopExplain("edition", s.Kind,
			"Oracle Edition-Based Redefinition (EBR) is an Oracle-specific online-upgrade feature. PG has no equivalent — production upgrades use logical replication, pg_repack, or planned downtime.")
	case "CREATE ATTRIBUTE DIMENSION", "CREATE HIERARCHY", "CREATE ANALYTIC VIEW":
		t.noopExplain("olap", s.Kind,
			"Oracle OLAP catalogue objects (ATTRIBUTE DIMENSION / HIERARCHY / ANALYTIC VIEW) have no PG counterpart. PG OLAP needs a separate engine — Citus / TimescaleDB hyperfunctions / Apache AGE — or a star-schema rewrite in app code.")
	case "CREATE FLASHBACK ARCHIVE", "FLASHBACK":
		t.noopExplain("flashback", s.Kind,
			"Oracle FLASHBACK (ARCHIVE / TABLE / AS OF TIMESTAMP|SCN) reads or restores past versions of data. PG has no built-in temporal tables — install the temporal_tables extension or roll your own history-table pattern with audit triggers.")
	case "CREATE AUDIT POLICY", "AUDIT":
		t.noopExplain("audit", s.Kind,
			"Oracle AUDIT / NOAUDIT / CREATE AUDIT POLICY emit per-event audit records. PG has no built-in DDL — use pg_audit (extension), event triggers, or row-level audit triggers, all configured outside the schema migration.")
	case "CREATE TABLESPACE":
		t.noopExplain("tablespace", s.Kind,
			"Oracle TABLESPACE bundles datafiles + storage attributes. PG TABLESPACE just maps a name to a directory — semantics are too different for an auto-translation. Create the PG tablespace manually via `CREATE TABLESPACE name LOCATION '/path'` if you want to keep the per-segment placement.")
	case "CREATE ROLE", "CREATE USER", "CREATE SCHEMA":
		t.noopExplain("auth", s.Kind,
			"Oracle ROLE / USER / SCHEMA are linked (a USER owns a SCHEMA of the same name). PG separates roles, users (a role with LOGIN), and schemas — auth migration is out of scope. Reimplement via `CREATE ROLE / CREATE SCHEMA` after the data migration.")
	case "CREATE INDEXTYPE", "CREATE OPERATOR":
		t.noopExplain("operator", s.Kind,
			"Oracle Data Cartridge constructs (INDEXTYPE / OPERATOR / domain index) extend the optimiser with custom indexes. PG supports custom operators and operator classes (`CREATE OPERATOR` / `CREATE OPERATOR CLASS`) — reimplement at the C-extension or pure-SQL level if needed.")
	case "PURGE":
		t.noopExplain("recyclebin", s.Kind,
			"Oracle PURGE empties the recycle bin. PG has no recycle bin — DROP TABLE is permanent — so the statement is dropped silently.")
	case "LOCK", "ASSOCIATE", "DISASSOCIATE":
		t.noopExplain("misc", s.Kind,
			"Oracle "+s.Kind+" has no PG counterpart in the schema-migration scope; relevant DDL/DCL is handled out-of-band on the PG side.")
	case "CREATE RESTORE POINT":
		t.noopExplain("restore_point", s.Kind,
			"Oracle RESTORE POINT marks an SCN for FLASHBACK. PG has no equivalent — use base backups + WAL archive points instead.")
	}
}

// noopExplain is a translateNoop helper that emits a uniform info-level
// Explanation for a noop'd Oracle/MySQL DDL kind without a PG equivalent.
func (t *translator) noopExplain(object, source, reason string) {
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: object,
		Source: source,
		Target: "(no PG equivalent — dropped)",
		Reason: reason,
		Level:  "info",
	})
}

// extractPackageName returns the package name following "CREATE PACKAGE" or
// "CREATE PACKAGE BODY" inside text. Returns "" when the parser failed to
// capture a name.
func extractPackageName(text, kind string) string {
	prefix := strings.ToUpper(kind) + " "
	upper := strings.ToUpper(text)
	idx := strings.Index(upper, prefix)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(text[idx+len(prefix):])
	// Take the first whitespace-separated token, stripping a trailing IS/AS keyword
	// or punctuation if any.
	for i, r := range rest {
		if r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '(' {
			rest = rest[:i]
			break
		}
	}
	return strings.TrimSpace(rest)
}

// ---------------------------------------------------------------------------
// DDL emission
// ---------------------------------------------------------------------------

// buildDDL assembles the PG DDL by populating a PostgreSQL AST
// (dialects/postgres) and rendering it through postgres.Write. This is the
// symmetric counterpart of dialects/mysql's Parse: the MySQL grammar drives
// the input AST, the PostgreSQL grammar drives the output AST — both anchored
// to vendored .g4 references.
func (t *translator) buildDDL() {
	var pre []pgast.Stmt

	// Include `public` in search_path so extension-provided types (geometry
	// from PostGIS, hstore, citext, …) resolve without qualification when
	// the target schema is something other than public.
	pre = append(pre,
		&pgast.CreateSchema{Name: t.opt.TargetSchema, IfNotExists: true},
		&pgast.SetSearchPath{Schema: t.opt.TargetSchema, Additional: []string{"public"}},
	)
	// Pre-actions (e.g. sequences feeding DEFAULT nextval in table columns).
	for _, a := range t.res.Plan.PreActions {
		pre = append(pre, &pgast.Raw{Text: a})
	}

	for _, tbl := range t.res.Plan.Tables {
		ct := &pgast.CreateTable{
			Schema: tbl.Schema, Name: tbl.Name,
			PrimaryKey: tbl.PK, Checks: tbl.Checks,
		}
		for _, c := range tbl.Columns {
			col := pgast.ColumnDef{
				Name:      c.Name,
				Type:      c.Type,
				NotNull:   c.NotNull,
				Default:   c.Default,
				Check:     c.Check,
				Collation: c.Collation,
			}
			if c.Identity {
				col.Identity = pgast.IdentityByDefault
			}
			if c.Generated != nil {
				col.Generated = &pgast.GeneratedCol{Expr: c.Generated.Expr}
			}
			ct.Columns = append(ct.Columns, col)
		}
		if tbl.Partitioning != nil {
			ct.PartitionBy = &pgast.PartitionSpec{
				Method:  tbl.Partitioning.Method,
				Columns: tbl.Partitioning.Columns,
			}
		}
		pre = append(pre, ct)
		// Emit child partitions, chaining bounds: partition N starts at the
		// upper bound of partition N-1 (or MINVALUE for the first), and ends
		// at its own upper bound.
		if tbl.Partitioning != nil {
			method := tbl.Partitioning.Method
			switch method {
			case "RANGE":
				prev := ""
				for _, part := range tbl.Partitioning.Partitions {
					upper := part.UpperBound
					if upper == "" {
						upper = "MAXVALUE"
					}
					pre = append(pre, &pgast.CreatePartition{
						Schema:      tbl.Schema,
						Name:        part.Name,
						ParentTable: tbl.Name,
						Method:      "RANGE",
						From:        prev, // empty → MINVALUE in writer
						To:          upper,
					})
					prev = upper
				}
			case "LIST":
				for _, part := range tbl.Partitioning.Partitions {
					pre = append(pre, &pgast.CreatePartition{
						Schema:      tbl.Schema,
						Name:        part.Name,
						ParentTable: tbl.Name,
						Method:      "LIST",
						Values:      part.Values,
						IsDefault:   part.IsDefault,
					})
				}
			case "HASH":
				for _, part := range tbl.Partitioning.Partitions {
					pre = append(pre, &pgast.CreatePartition{
						Schema:      tbl.Schema,
						Name:        part.Name,
						ParentTable: tbl.Name,
						Method:      "HASH",
						Modulus:     part.Modulus,
						Remainder:   part.Remainder,
					})
				}
			}
		}

		if tbl.Comment != "" {
			pre = append(pre, &pgast.CommentOn{
				Object: "TABLE",
				Target: fmt.Sprintf("%s.%s", pgQuote(tbl.Schema), pgQuote(tbl.Name)),
				Body:   tbl.Comment,
			})
		}
		for _, c := range tbl.Columns {
			if c.Comment == "" {
				continue
			}
			pre = append(pre, &pgast.CommentOn{
				Object: "COLUMN",
				Target: fmt.Sprintf("%s.%s.%s",
					pgQuote(tbl.Schema), pgQuote(tbl.Name), pgQuote(c.Name)),
				Body: c.Comment,
			})
		}
	}

	// Post-copy AST: indexes, FKs, then post-actions.
	// MySQL scopes index names per-table but PG scopes them per-schema, so
	// rewrite any duplicate names by prefixing the owning table.
	dedupeIndexNames(t.res.Plan.Indexes)
	var post []pgast.Stmt
	for _, idx := range t.res.Plan.Indexes {
		post = append(post, &pgast.CreateIndex{
			Schema: idx.Schema, Table: idx.Table, Name: idx.Name,
			Unique: idx.Unique, Columns: idx.Columns, Method: idx.Using,
			ColumnDirs: idx.ColumnDirs, ColumnIsExpr: idx.ColumnIsExpr,
		})
	}
	// PG rejects `ADD CONSTRAINT … NOT VALID FOREIGN KEY` when the table is
	// partitioned (SQLSTATE 42809). For partitioned-source tables we emit a
	// single validating ADD CONSTRAINT instead of the NOT VALID + VALIDATE
	// pair the unpartitioned path uses to skip a full table scan.
	partitioned := make(map[string]bool)
	for _, tbl := range t.res.Plan.Tables {
		if tbl.Partitioning != nil {
			partitioned[strings.ToLower(tbl.Name)] = true
		}
	}
	// Build a set of tables this migration is creating, so we can skip FKs
	// pointing at tables that aren't in scope (e.g. cross-schema refs to a
	// schema that isn't being migrated alongside this one — Oracle OE has
	// FKs referencing HR.employees, but we only migrated OE, so the
	// post-copy ADD CONSTRAINT would fail with "relation … does not exist").
	// We surface a manual-review prereq so the user knows the FK was
	// dropped and can recreate it after migrating the parent schema.
	migratedTables := make(map[string]bool, len(t.res.Plan.Tables))
	for _, tbl := range t.res.Plan.Tables {
		migratedTables[strings.ToLower(tbl.Name)] = true
	}
	for _, fk := range t.res.Plan.ForeignKeys {
		if !migratedTables[strings.ToLower(fk.RefTable)] {
			// Resolve the source-side schema and table name that own the
			// missing reference. fk.RefSchema/fk.RefTable may have been
			// rewritten to the PG target schema (lowercased); fall back to
			// fk.RefSchema only if it differs from the target schema we are
			// migrating into.
			refSchema := ""
			refTable := fk.RefTable
			if fk.RefSchema != "" &&
				!strings.EqualFold(fk.RefSchema, t.opt.TargetSchema) {
				refSchema = strings.ToUpper(fk.RefSchema)
			}
			qualified := refTable
			schemaHint := "the schema that owns " + refTable
			migrateHint := "Migrate the source schema that owns " + refTable
			if refSchema != "" {
				qualified = refSchema + "." + refTable
				schemaHint = "schema " + refSchema
				migrateHint = "Migrate source schema " + refSchema + " (which owns " + refTable + ")"
			}
			t.res.Prerequisites = append(t.res.Prerequisites, Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualReview,
				Object:      "fk." + fk.Table + "." + fk.Name,
				Title:       "Cross-schema FK references missing table: " + qualified,
				Description: "Foreign key " + fk.Name + " on " + fk.Table + " references " + qualified + ", which is not in this migration's scope. The referenced table must already exist in the target schema before this migration can run, otherwise `ADD CONSTRAINT … REFERENCES " + fk.RefTable + "` fails with `relation does not exist` in PG.",
				Remediation: migrateHint + " into the same target PG schema first, then re-run this migration. Alternative: drop the FK from the source dump (or remove the column from scope) if you do not want " + schemaHint + " enforced in PG.",
			})
			continue
		}
		// PG performance optimisation: ADD … NOT VALID skips the full-table
		// validation that would otherwise hold AccessExclusiveLock on the
		// child for the duration of the ADD. We then VALIDATE in a separate
		// step (RowShare lock instead). Partitioned tables don't support
		// NOT VALID on FK at all (SQLSTATE 42809), so we always pay the
		// upfront validation there — even when Oracle had NOVALIDATE, since
		// PG offers no way to express "trust the existing rows" on a
		// partitioned FK.
		oracleNoValidate := fk.NotValid
		isPartitioned := partitioned[strings.ToLower(fk.Table)]
		notValid := !isPartitioned
		if oracleNoValidate && isPartitioned {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: "table." + fk.Table + ".constraint." + fk.Name,
				Source: "FOREIGN KEY … NOVALIDATE",
				Target: "(validates at ADD time — PG cannot NOT VALID FK on partitioned table)",
				Reason: "Oracle NOVALIDATE asked PG to skip checking existing rows, but PostgreSQL rejects `ADD CONSTRAINT … NOT VALID FOREIGN KEY` on partitioned tables (SQLSTATE 42809). The constraint will validate every existing row at creation time; if any row violates the FK the migration fails at this step.",
				Level:  "warn",
			})
		}
		post = append(post,
			&pgast.AlterTableAddFK{
				Schema: fk.Schema, Table: fk.Table, Name: fk.Name,
				Columns: fk.Columns, RefSchema: fk.RefSchema,
				RefTable: fk.RefTable, RefColumns: fk.RefColumns,
				OnDelete: fk.OnDelete, OnUpdate: fk.OnUpdate,
				NotValid:          notValid,
				Deferrable:        fk.Deferrable,
				InitiallyDeferred: fk.InitiallyDeferred,
			},
		)
		if notValid && !oracleNoValidate {
			post = append(post, &pgast.AlterTableValidateFK{
				Schema: fk.Schema, Table: fk.Table, Name: fk.Name,
			})
		}
		// Partitioned tables: the ADD already validated, no extra step needed.
		// Oracle NOVALIDATE: caller explicitly opted out of validation.
	}
	// Post-actions (sequence setval, ON UPDATE CURRENT_TIMESTAMP triggers,
	// informational comments). Routines and views are handled as their own
	// create_routine:<name> steps so that a broken body doesn't sink the
	// whole FK-creation block.
	for _, a := range t.res.Plan.PostActions {
		post = append(post, &pgast.Raw{Text: a})
	}

	t.res.DDLScript = "-- Generated by squishy.\n" + pgast.Write(pre)
	t.res.DDLPostCopy = pgast.Write(post)
}

// pgQuote quotes an identifier exactly like dialects/postgres writer does.
// Kept locally to avoid exposing it from the postgres package.
func pgQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// dropDisabledConstraint surfaces an Oracle DISABLE'd constraint as an
// info-level explanation and returns true to tell the caller to skip
// emitting the constraint. PostgreSQL has no DISABLE state — the
// constraint either exists and is enforced, or it doesn't exist.
func (t *translator) dropDisabledConstraint(table, kind, name string, st ast.ConstraintState) bool {
	if !st.Disabled {
		return false
	}
	objName := name
	if objName == "" {
		objName = "<unnamed>"
	}
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "table." + table + ".constraint." + objName,
		Source: kind + " … DISABLE",
		Target: "(constraint not emitted)",
		Reason: "Oracle DISABLE keeps the constraint metadata around but does not enforce it. PostgreSQL has no equivalent state — the constraint is dropped from the migration plan. Enable it explicitly later by re-adding the constraint with `ALTER TABLE … ADD CONSTRAINT`.",
		Level:  "info",
	})
	return true
}

// warnUnsupportedConstraintState surfaces NOVALIDATE / RELY / DEFERRABLE on
// PK / UQ / CHECK constraints that PostgreSQL can't fully express. (FK is
// handled directly via PGForeignKey fields and skipped here.)
func (t *translator) warnUnsupportedConstraintState(table, kind, name string, st ast.ConstraintState) {
	objName := name
	if objName == "" {
		objName = "<unnamed>"
	}
	if st.NoValidate {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "table." + table + ".constraint." + objName,
			Source: kind + " … NOVALIDATE",
			Target: "(constraint always validates new rows in PG)",
			Reason: "Oracle NOVALIDATE skips checking existing rows when the constraint is added. PostgreSQL's PG cannot mark a PRIMARY KEY / UNIQUE / CHECK as NOT VALID — the constraint will validate every existing row at creation time. If your data violates the constraint, the migration will fail at the COPY/CREATE step.",
			Level:  "warn",
		})
	}
	if st.Rely {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "table." + table + ".constraint." + objName,
			Source: kind + " … RELY",
			Target: "(no PG equivalent)",
			Reason: "Oracle RELY tells the cost-based optimiser to trust an unenforced or unvalidated constraint when rewriting queries. PostgreSQL's planner has no equivalent hint.",
			Level:  "info",
		})
	}
	if st.Deferrable {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "table." + table + ".constraint." + objName,
			Source: kind + " … DEFERRABLE",
			Target: "(deferrable not propagated to inline PK/UQ)",
			Reason: "PostgreSQL supports DEFERRABLE on PRIMARY KEY / UNIQUE constraints (with `INITIALLY DEFERRED`), but the inline-CREATE-TABLE writer does not yet emit it. Add `DEFERRABLE INITIALLY DEFERRED` manually to the constraint definition if cross-row deferred-check semantics matter.",
			Level:  "warn",
		})
	}
	if st.HasUsingIndex {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "table." + table + ".constraint." + objName,
			Source: kind + " … USING INDEX (…)",
			Target: "(spec dropped — PG synthesises the index implicitly)",
			Reason: "Oracle USING INDEX lets you point the constraint at an existing index or specify per-index storage (TABLESPACE, COMPRESS, INITRANS, …). PostgreSQL always synthesises a fresh B-tree index for a PRIMARY KEY / UNIQUE constraint and exposes no spec hooks at constraint-creation time. The Oracle index spec was dropped; the PG default index is functionally equivalent for query planning.",
			Level:  "info",
		})
	}
}

func (t *translator) warn(object, kind, msg string) {
	t.res.Warnings = append(t.res.Warnings, Warning{Object: object, Kind: kind, Message: msg})
}

// warnSev is warn + explicit severity. Use for intrinsic target-engine
// limitations where the user has nothing actionable to do (mapping is
// best-effort and documented). Empty severity falls back to warn's default.
func (t *translator) warnSev(object, kind, msg string, sev Severity) {
	t.res.Warnings = append(t.res.Warnings, Warning{
		Object:   object,
		Kind:     kind,
		Message:  msg,
		Severity: string(sev),
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func pgProcSignature(params []ast.Param, function bool, caps Caps, kind dialects.Kind) string {
	// PG enforces "every parameter after one with a default value must
	// also have a default" — Oracle is laxer (defaults can sit anywhere
	// in the list because Oracle supports named-notation calls). When we
	// see a defaulted param followed by an undefaulted IN/INOUT, pad the
	// trailing ones with `DEFAULT NULL` so the routine compiles. This
	// preserves the "callable with fewer args" behaviour that Oracle
	// callers rely on, at the cost of accepting NULL where Oracle would
	// have raised PLS-00306. OUT parameters can't take a default in PG,
	// so they don't get padded.
	seenDefault := false
	if dialects.IsOracle(kind) {
		for _, p := range params {
			if p.Default != nil {
				seenDefault = true
				break
			}
		}
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		typ := MapType(kind, p.Type, p.Name, caps).PG
		// Oracle stores unquoted identifiers in uppercase; quoting them in the
		// PG signature would force callers and body references to use the
		// quoted form ("C") or they'd get "not a known variable". Apply the
		// same normalization the rest of the Oracle pipeline uses (all-caps
		// → lowercase, mixed-case preserved via quoting) so param names in
		// the signature match what the body references.
		name := p.Name
		if dialects.IsOracle(kind) {
			name = normalizeOracleIdent(name)
		}
		rendered := quoteIdent(name)
		defClause := ""
		if p.Default != nil {
			expr := translateDefault(p.Default)
			if dialects.IsOracle(kind) {
				expr = rewriteOracleExpr(expr)
			}
			if expr != "" {
				defClause = " DEFAULT " + expr
			}
		}
		dir := p.Direction
		if dir == "" {
			dir = "IN"
		}
		// Pad missing defaults on Oracle IN/INOUT params once any
		// param in the list carries one.
		if seenDefault && defClause == "" && (dir == "IN" || dir == "INOUT") {
			defClause = " DEFAULT NULL"
		}
		if function {
			parts = append(parts, fmt.Sprintf("%s %s%s", rendered, typ, defClause))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s %s%s", dir, rendered, typ, defClause))
	}
	return strings.Join(parts, ", ")
}

func mysqlTypeRepr(t ast.DataType) string {
	return t.TypeName()
}

// dedupeIndexNames renames indexes whose names collide within the target
// schema. Only duplicates are touched; the first occurrence of each name
// keeps its original label. A colliding name becomes "<table>_<name>"; if
// that still collides (pathological case) we append a numeric suffix.
func dedupeIndexNames(idxs []PGIndex) {
	seen := make(map[string]bool, len(idxs))
	key := func(schema, name string) string { return schema + "." + name }
	for i := range idxs {
		k := key(idxs[i].Schema, idxs[i].Name)
		if !seen[k] {
			seen[k] = true
			continue
		}
		candidate := idxs[i].Table + "_" + idxs[i].Name
		k2 := key(idxs[i].Schema, candidate)
		for n := 2; seen[k2]; n++ {
			candidate = fmt.Sprintf("%s_%s_%d", idxs[i].Table, idxs[i].Name, n)
			k2 = key(idxs[i].Schema, candidate)
		}
		idxs[i].Name = candidate
		seen[k2] = true
	}
}

func columnsNames(cs []ast.IndexedCol) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.IsExpr {
			out = append(out, c.Expr)
			continue
		}
		out = append(out, c.Name)
	}
	return out
}

// indexColumnPayload turns a slice of IndexedCol into the parallel slices
// (Columns, Dirs, IsExpr) that PGIndex/pgast.CreateIndex expect, returning
// `false` for `dirsHaveAny` when no per-column direction was set so callers
// can keep the slice nil and avoid noisy DDL output.
func indexColumnPayload(cs []ast.IndexedCol) (cols []string, dirs []string, isExpr []bool, dirsHaveAny bool, exprHaveAny bool) {
	cols = make([]string, len(cs))
	dirs = make([]string, len(cs))
	isExpr = make([]bool, len(cs))
	for i, c := range cs {
		if c.IsExpr {
			cols[i] = c.Expr
			isExpr[i] = true
			exprHaveAny = true
		} else {
			cols[i] = c.Name
		}
		if c.Order != "" {
			dirs[i] = c.Order
			dirsHaveAny = true
		}
	}
	return cols, dirs, isExpr, dirsHaveAny, exprHaveAny
}

func joinCols(cs []ast.IndexedCol) string {
	return strings.Join(columnsNames(cs), "_")
}

// translateDefault translates a MySQL DEFAULT expression into a PG one.
// Handles the common idioms: CURRENT_TIMESTAMP[(n)] → now(), UUID() → gen_random_uuid(),
// literal passthrough, parenthesized expression passthrough.
func translateDefault(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Literal:
		switch x.Kind {
		case "null":
			return "NULL"
		case "string":
			return sqlString(x.Text)
		case "bool":
			return x.Text
		default:
			return x.Text
		}
	case *ast.FuncCall:
		name := strings.ToUpper(x.Name)
		switch name {
		case "CURRENT_TIMESTAMP", "NOW", "LOCALTIMESTAMP":
			return "now()"
		case "UUID":
			return "gen_random_uuid()"
		default:
			return rawExpr(e)
		}
	case *ast.ParenExpr:
		inner := translateDefault(x.Inner)
		return "(" + inner + ")"
	}
	return rawExpr(e)
}

// rawExpr renders an expression as SQL text, best-effort. Covers every Expr
// node defined in internal/sqlparse/ast — including the typed DML nodes
// (CaseExpr, BetweenExpr, InExpr, ExistsExpr, SubqueryExpr, WindowedAgg,
// OuterJoinHint, IntervalLit, CastExpr) emitted by the parser DML pipeline.
//
// Identifier handling for unquoted Oracle uppercase names is left to the
// post-translation rewriteOraclePLpgSQL pass, so this function stays
// dialect-neutral. A literal `*` Ident is rendered as a bare star (used in
// COUNT(*) and `t.*` projections).
func rawExpr(e ast.Expr) string {
	if e == nil {
		return ""
	}
	switch x := e.(type) {
	case *ast.Literal:
		if x.Kind == "string" {
			return sqlString(x.Text)
		}
		return x.Text
	case *ast.Ident:
		parts := make([]string, 0, len(x.Parts))
		for _, p := range x.Parts {
			if p == "*" {
				parts = append(parts, "*")
				continue
			}
			parts = append(parts, quoteIdent(p))
		}
		return strings.Join(parts, ".")
	case *ast.CursorAttr:
		// PG plpgsql equivalents for Oracle cursor attributes. The legacy
		// text pass `rewriteOracleCursorAttributes` performed the same
		// substitution by scanning `%` in the rendered body; once the
		// parser produces *CursorAttr the typed path takes precedence
		// and the text pass becomes a no-op for this pattern.
		switch strings.ToUpper(x.Attr) {
		case "FOUND":
			return "FOUND"
		case "NOTFOUND":
			return "NOT FOUND"
		case "ISOPEN":
			// PG refcursors don't expose a generic "is open" boolean; the
			// closest portable check is whether the cursor exists in the
			// session-wide pg_cursors view.
			return "EXISTS (SELECT 1 FROM pg_cursors WHERE name = " + x.Cursor + "::text)"
		case "ROWCOUNT":
			// PG exposes the last DML's row count via GET DIAGNOSTICS,
			// not as an inline expression — surface a TODO so the
			// reviewer rewrites this site by hand.
			return "0 /* TODO: replace with `GET DIAGNOSTICS x = ROW_COUNT` after the FETCH/EXECUTE referencing " + x.Cursor + " */"
		}
		// Unknown attribute name: emit the source form to surface as a
		// PG syntax error rather than a silent miss.
		return x.Cursor + "%" + x.Attr
	case *ast.SequenceRef:
		// PG: nextval / currval take the sequence name as a string lit
		// resolvable through search_path. Schema-qualified Oracle refs
		// (`schema.seq.NEXTVAL`) round-trip with the schema embedded
		// in the literal: `nextval('schema.seq')`.
		name := x.Name
		if x.Schema != "" {
			name = x.Schema + "." + name
		}
		fn := "nextval"
		if strings.EqualFold(x.Op, "CURRVAL") {
			fn = "currval"
		}
		return fn + "(" + sqlString(name) + ")"
	case *ast.BinaryExpr:
		op := x.Op
		if strings.EqualFold(op, "DIV") {
			op = "/"
		}
		return rawExpr(x.Lhs) + " " + op + " " + rawExpr(x.Rhs)
	case *ast.UnaryExpr:
		// Word-shaped operators (NOT, PRIOR, CONNECT_BY_ROOT) need a separating
		// space; punctuation operators (+, -, !, ~) do not.
		if isWordOp(x.Op) {
			return x.Op + " " + rawExpr(x.Rhs)
		}
		return x.Op + rawExpr(x.Rhs)
	case *ast.ParenExpr:
		return "(" + rawExpr(x.Inner) + ")"
	case *ast.FuncCall:
		args := make([]string, 0, len(x.Args))
		for _, a := range x.Args {
			args = append(args, rawExpr(a))
		}
		name := mapFunction(x.Name)
		// Oracle niladic pseudocolumns: render WITHOUT trailing parens.
		// `rewriteOracleExpr` later rewrites SYSDATE → CURRENT_TIMESTAMP::
		// timestamp(0); appending `()` would leave `…(0)()` (a syntax error
		// in PG). The parser produces a FuncCall with 0 args for these
		// pseudocolumns regardless of whether the source had `SYSDATE` or
		// `SYSDATE()` (Oracle accepts both forms but the function shape
		// has no arguments either way).
		if len(args) == 0 {
			switch strings.ToUpper(x.Name) {
			case "SYSDATE", "SYSTIMESTAMP", "CURRENT_TIMESTAMP", "CURRENT_DATE",
				"LOCALTIMESTAMP", "UID", "ROWNUM", "LEVEL":
				return name
			}
		}
		return name + "(" + strings.Join(args, ", ") + ")"
	case *ast.CaseExpr:
		var b strings.Builder
		b.WriteString("CASE")
		if x.Operand != nil {
			b.WriteByte(' ')
			b.WriteString(rawExpr(x.Operand))
		}
		for _, w := range x.Whens {
			b.WriteString(" WHEN ")
			b.WriteString(rawExpr(w.Match))
			b.WriteString(" THEN ")
			b.WriteString(rawExpr(w.Then))
		}
		if x.Else != nil {
			b.WriteString(" ELSE ")
			b.WriteString(rawExpr(x.Else))
		}
		b.WriteString(" END")
		return b.String()
	case *ast.BetweenExpr:
		op := "BETWEEN"
		if x.Not {
			op = "NOT BETWEEN"
		}
		return rawExpr(x.Expr) + " " + op + " " + rawExpr(x.Low) + " AND " + rawExpr(x.High)
	case *ast.InExpr:
		op := "IN"
		if x.Not {
			op = "NOT IN"
		}
		var rhs string
		if x.Subquery != nil {
			rhs = "(" + emitSelectStmt(x.Subquery) + ")"
		} else {
			parts := make([]string, len(x.List))
			for i, e := range x.List {
				parts[i] = rawExpr(e)
			}
			rhs = "(" + strings.Join(parts, ", ") + ")"
		}
		return rawExpr(x.Expr) + " " + op + " " + rhs
	case *ast.ExistsExpr:
		op := "EXISTS"
		if x.Not {
			op = "NOT EXISTS"
		}
		return op + " (" + emitSelectStmt(x.Subquery) + ")"
	case *ast.SubqueryExpr:
		return "(" + emitSelectStmt(x.Stmt) + ")"
	case *ast.WindowedAgg:
		var b strings.Builder
		b.WriteString(rawExpr(x.Func))
		if len(x.Within) > 0 {
			b.WriteString(" WITHIN GROUP (ORDER BY ")
			items := make([]string, len(x.Within))
			for i, oi := range x.Within {
				items[i] = renderOrderItem(oi)
			}
			b.WriteString(strings.Join(items, ", "))
			b.WriteString(")")
		}
		if x.Over != nil {
			b.WriteString(" OVER ")
			b.WriteString(renderWindowSpec(x.Over))
		}
		return b.String()
	case *ast.OuterJoinHint:
		// `(+)` is Oracle-only; in the canonical PG output it is removed by
		// the outer-join rewriter (rewriteOraclePlusOuterJoins). When this
		// node still reaches the writer we drop the hint and emit only the
		// inner expression — the translator will have already surfaced a
		// warning at parse time.
		return rawExpr(x.Inner)
	case *ast.IntervalLit:
		val := x.Value
		if !strings.HasPrefix(val, "'") {
			val = "'" + val + "'"
		}
		if x.Unit == "" {
			return "INTERVAL " + val
		}
		return "INTERVAL " + val + " " + x.Unit
	case *ast.CastExpr:
		typ := ""
		if x.Type != nil {
			typ = MapType(dialectKindFromCtx(), x.Type, "", Caps{}).PG
		}
		if typ == "" {
			typ = "text"
		}
		return "CAST(" + rawExpr(x.Expr) + " AS " + typ + ")"
	case *ast.RawExpr:
		return x.Text
	}
	return ""
}

// isWordOp reports whether op is a word-shaped unary operator that needs a
// trailing space in front of its operand.
func isWordOp(op string) bool {
	switch strings.ToUpper(op) {
	case "NOT", "PRIOR", "CONNECT_BY_ROOT":
		return true
	}
	return false
}

// dialectKindFromCtx returns a placeholder dialect.Kind for CastExpr type
// rendering. The CastExpr writer does not have access to the translator's
// kind field; PG types pass through unchanged regardless of source dialect,
// which is the only case CAST is currently emitted on.
func dialectKindFromCtx() dialects.Kind {
	return dialects.KindPostgres
}

// renderOrderItem renders one ast.OrderItem as PG-flavoured ORDER BY text.
func renderOrderItem(oi ast.OrderItem) string {
	out := rawExpr(oi.Expr)
	if oi.Desc {
		out += " DESC"
	}
	if oi.NullsLast != nil {
		if *oi.NullsLast {
			out += " NULLS LAST"
		} else {
			out += " NULLS FIRST"
		}
	}
	return out
}

// renderWindowSpec renders the body of an OVER (...) clause.
func renderWindowSpec(w *ast.WindowSpec) string {
	if w.RawSpec != "" && len(w.PartitionBy) == 0 && len(w.OrderBy) == 0 && w.Frame == "" {
		// Already a complete `(spec)` — don't double-wrap.
		s := strings.TrimSpace(w.RawSpec)
		if !strings.HasPrefix(s, "(") {
			s = "(" + s + ")"
		}
		return s
	}
	var parts []string
	if len(w.PartitionBy) > 0 {
		exprs := make([]string, len(w.PartitionBy))
		for i, e := range w.PartitionBy {
			exprs[i] = rawExpr(e)
		}
		parts = append(parts, "PARTITION BY "+strings.Join(exprs, ", "))
	}
	if len(w.OrderBy) > 0 {
		items := make([]string, len(w.OrderBy))
		for i, oi := range w.OrderBy {
			items[i] = renderOrderItem(oi)
		}
		parts = append(parts, "ORDER BY "+strings.Join(items, ", "))
	}
	if w.Frame != "" {
		parts = append(parts, w.Frame)
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// mapFunction rewrites the most common MySQL function names to their PG
// counterparts (used from rawExpr when emitting DDL). Unknown functions are
// kept as-is with a warning surfaced elsewhere.
func mapFunction(name string) string {
	switch strings.ToUpper(name) {
	case "IFNULL":
		return "COALESCE"
	case "CHAR_LENGTH", "CHARACTER_LENGTH":
		return "length"
	case "NOW", "CURRENT_TIMESTAMP", "LOCALTIMESTAMP":
		return "now"
	case "UUID":
		return "gen_random_uuid"
	case "CONCAT":
		return "concat"
	case "JSON_EXTRACT":
		return "jsonb_extract_path"
	case "JSON_UNQUOTE":
		return "jsonb_extract_path_text"
	}
	return name
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// isJSONValidCheck reports whether the given CHECK expression text is the
// MariaDB/MySQL idiom `JSON_VALID(col)` (case-insensitive, backtick- or
// double-quote-quoted column ref accepted). Used to detect the legacy
// LONGTEXT-backed JSON column pattern and promote it to native PG JSONB.
func isJSONValidCheck(expr, col string) bool {
	s := strings.TrimSpace(expr)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "JSON_VALID(") || !strings.HasSuffix(s, ")") {
		return false
	}
	inner := strings.TrimSpace(s[len("JSON_VALID(") : len(s)-1])
	// strip wrapping quotes around the identifier
	for _, q := range []byte{'`', '"'} {
		if len(inner) >= 2 && inner[0] == q && inner[len(inner)-1] == q {
			inner = inner[1 : len(inner)-1]
			break
		}
	}
	return strings.EqualFold(inner, col)
}

func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// renderCollectionElement maps an Oracle VARRAY/TABLE-OF element-type string
// to its PG equivalent and classifies the result for UserTypeRef.Kind.
//
//   "VARCHAR2(25)"       → "varchar(25)", "array_scalar"
//   "NUMBER(6)"          → "integer",      "array_scalar"
//   "NUMBER(20,4)"       → "numeric(20,4)","array_scalar"
//   "DATE"               → "timestamp(0)", "array_scalar"
//   "inventory_typ"      → "\"mig\".\"inventory_typ\"", "array_composite"
//                          (looked up in t.userTypes if present, otherwise
//                          emitted as a forward reference to the migrated
//                          composite — the table-creation order ensures the
//                          composite exists by the time the table is built)
//   "REF category_typ"   → "\"mig\".\"category_typ\"", "array_composite"
//
// Returns ("", "") when the element type can't be classified — the caller
// then leaves the user-type unmapped (column falls back to TEXT and the
// existing "user-defined type … not translated" warning fires).
func (t *translator) renderCollectionElement(raw string) (string, string) {
	el := strings.TrimSpace(raw)
	if el == "" {
		return "", ""
	}
	upper := strings.ToUpper(el)
	if strings.HasPrefix(upper, "REF ") {
		el = strings.TrimSpace(el[4:])
		upper = strings.ToUpper(el)
	}
	// Composite element: identifier-only (optionally schema-qualified, no
	// parens). Matches "inventory_typ", `"OE"."INVENTORY_TYP"`, … but NOT
	// bare scalar Oracle keywords like NUMBER/DATE/INTEGER which also
	// have no parens — those drop down to the scalar switch below.
	if !strings.ContainsAny(el, "() ") {
		bare := strings.Trim(el, `"`)
		if i := strings.LastIndex(bare, "."); i >= 0 {
			bare = strings.Trim(bare[i+1:], `"`)
		}
		switch strings.ToUpper(bare) {
		case "NUMBER", "NUMERIC", "DECIMAL",
			"INTEGER", "INT", "SMALLINT", "BIGINT", "PLS_INTEGER", "BINARY_INTEGER",
			"DATE", "TIMESTAMP",
			"FLOAT", "REAL", "BINARY_FLOAT", "BINARY_DOUBLE", "DOUBLE",
			"CLOB", "NCLOB", "LONG", "TEXT",
			"BLOB", "RAW", "BFILE", "BYTEA",
			"BOOLEAN",
			"VARCHAR", "VARCHAR2", "NVARCHAR", "NVARCHAR2", "CHAR", "NCHAR":
			// fall through to the scalar switch
		default:
			bare = strings.ToLower(bare)
			schema := t.opt.TargetSchema
			if dialects.IsOracle(t.opt.SourceKind) {
				schema = normalizeOracleIdent(schema)
			}
			return pgQualified(schema, bare), "array_composite"
		}
	}
	// Scalar element: dispatch on the leading keyword and reuse PG-native
	// equivalents for the well-known Oracle types we already cover in the
	// type mapper. Only the cases the OE schema actually exercises are
	// listed; anything else returns "" and falls through to the warning.
	head := upper
	if i := strings.IndexAny(head, "(, "); i >= 0 {
		head = head[:i]
	}
	switch head {
	case "VARCHAR", "VARCHAR2", "NVARCHAR", "NVARCHAR2", "CHAR", "NCHAR":
		// Preserve the (N) length suffix verbatim, lowercase the head.
		rest := el[len(head):]
		pg := strings.ToLower(head)
		if pg == "varchar2" || pg == "nvarchar2" {
			pg = "varchar"
		}
		return pg + rest, "array_scalar"
	case "NUMBER", "NUMERIC", "DECIMAL":
		// NUMBER(p) → integer family; NUMBER(p,s) → numeric(p,s); NUMBER → numeric.
		rest := strings.TrimSpace(el[len(head):])
		if rest == "" {
			return "numeric", "array_scalar"
		}
		inside := strings.Trim(rest, "()")
		parts := strings.Split(inside, ",")
		if len(parts) == 1 {
			// scale defaulted to 0 → integer family by precision
			p := strings.TrimSpace(parts[0])
			switch p {
			case "1", "2", "3", "4":
				return "smallint", "array_scalar"
			case "5", "6", "7", "8", "9":
				return "integer", "array_scalar"
			default:
				return "bigint", "array_scalar"
			}
		}
		return "numeric(" + strings.TrimSpace(parts[0]) + "," + strings.TrimSpace(parts[1]) + ")", "array_scalar"
	case "DATE":
		return "timestamp(0)", "array_scalar"
	case "TIMESTAMP":
		// keep optional precision verbatim
		return "timestamp" + el[len(head):], "array_scalar"
	case "FLOAT", "REAL", "BINARY_FLOAT", "BINARY_DOUBLE":
		return "double precision", "array_scalar"
	case "CLOB", "NCLOB", "LONG":
		return "text", "array_scalar"
	case "BLOB", "RAW", "BFILE":
		return "bytea", "array_scalar"
	case "BOOLEAN":
		return "boolean", "array_scalar"
	}
	return "", ""
}

// flattenOfTypeAttrs resolves an Oracle object-view's `OF <type_name>`
// clause to the underlying composite's flattened attribute list (parent
// chain inlined). Returns nil when the type name doesn't match anything
// in the registry — caller should fall back to the SELECT-implied
// column names.
func (t *translator) flattenOfTypeAttrs(typeName string) []ast.ColumnDef {
	if t.objectAttrs == nil {
		return nil
	}
	key := userTypeKey(typeName)
	info, ok := t.objectAttrs[key]
	if !ok {
		return nil
	}
	stub := &ast.CreateType{
		Name:       typeName,
		Kind:       "OBJECT",
		Attributes: info.Attributes,
		ParentType: info.ParentType,
	}
	return t.flattenObjectAttrs(stub)
}

// compositeColumnNames returns the set of lowercase column names that the
// migration emitted with a composite type — used to decide where Oracle's
// dotted composite-attribute access (`alias.col.attr`) needs PG parens
// (`(alias.col).attr`). Walks Plan.Tables and matches column types whose
// PG rendering is a `"mig"."<typename>"` qualified composite reference.
func (t *translator) compositeColumnNames() map[string]bool {
	out := map[string]bool{}
	for _, tbl := range t.res.Plan.Tables {
		for _, c := range tbl.Columns {
			if _, ok := compositeKeyFromPG(strings.TrimSuffix(c.Type, "[]")); ok {
				out[strings.ToLower(c.Name)] = true
			}
		}
	}
	return out
}

// rewriteOracleMakeRef replaces every `MAKE_REF(<table_or_view>, <key>)`
// invocation in `body` with just the second argument. Oracle's MAKE_REF
// constructs a typed REF to an object identity; PostgreSQL has no REF
// type, so we surface the underlying key value instead — call sites that
// previously did `DEREF(MAKE_REF(...)).attr` then degrade to `key.attr`,
// which is fine when callers just want the value.
//
// The scan is identifier-aware (must be preceded by a non-identifier
// byte) and skips matches inside single-quoted string literals so we
// don't rewrite `MAKE_REF` inside a comment or constant.
func rewriteOracleMakeRef(body string) string {
	return rewriteIdentityFunc2(body, "MAKE_REF", 2 /* keep arg index */ -1)
}

// rewriteOracleDeref replaces every `DEREF(<expr>)` with just `<expr>` —
// PG has nothing to dereference, the value is already in hand.
func rewriteOracleDeref(body string) string {
	return rewriteIdentityFunc1(body, "DEREF")
}

// rewriteIdentityFunc1 collapses `<funcname>(<expr>)` to `<expr>`.
func rewriteIdentityFunc1(body, fnName string) string {
	return rewriteFuncCall(body, fnName, func(args string) string {
		return strings.TrimSpace(args)
	})
}

// rewriteIdentityFunc2 collapses a 2-arg call by returning the
// 1-indexed `keepArg`th argument verbatim. `keepArg` is 1-based; pass
// 2 - 1 = 1-indexed of which arg to keep. (Helper kept simple — both
// callers want arg 2.)
func rewriteIdentityFunc2(body, fnName string, _ int) string {
	return rewriteFuncCall(body, fnName, func(args string) string {
		parts, err := splitTopLevel(args, ',')
		if err != nil || len(parts) < 2 {
			return strings.TrimSpace(args)
		}
		return strings.TrimSpace(parts[1])
	})
}

// rewriteFuncCall finds whole-word `fnName(...)` calls in `body` and
// replaces each with `transform(args)` where `args` is the raw text
// between the matching parens (single-quoted literals respected). Used
// to retract Oracle-specific helpers like MAKE_REF / DEREF that have no
// PG counterpart and should collapse to one of their arguments.
func rewriteFuncCall(body, fnName string, transform func(args string) string) string {
	if body == "" || fnName == "" {
		return body
	}
	upper := strings.ToUpper(body)
	upperFn := strings.ToUpper(fnName)
	var out strings.Builder
	out.Grow(len(body))
	i := 0
	for i < len(body) {
		// Pass single-quoted literals through verbatim.
		if body[i] == '\'' {
			out.WriteByte(body[i])
			i++
			for i < len(body) {
				out.WriteByte(body[i])
				if body[i] == '\'' {
					if i+1 < len(body) && body[i+1] == '\'' {
						out.WriteByte(body[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		end := i + len(upperFn)
		if end > len(upper) || upper[i:end] != upperFn {
			out.WriteByte(body[i])
			i++
			continue
		}
		left := i == 0 || !isIdentByte(body[i-1])
		if !left {
			out.WriteByte(body[i])
			i++
			continue
		}
		// Skip optional whitespace then require '('.
		j := end
		for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\r' || body[j] == '\n') {
			j++
		}
		if j >= len(body) || body[j] != '(' {
			// Not a function call — treat as identifier, emit verbatim.
			out.WriteString(body[i:end])
			i = end
			continue
		}
		// Find matching ')'.
		depth := 1
		k := j + 1
		inStr := false
		for k < len(body) && depth > 0 {
			c := body[k]
			if inStr {
				if c == '\'' {
					if k+1 < len(body) && body[k+1] == '\'' {
						k += 2
						continue
					}
					inStr = false
				}
				k++
				continue
			}
			switch c {
			case '\'':
				inStr = true
			case '(':
				depth++
			case ')':
				depth--
			}
			k++
		}
		args := body[j+1 : k-1]
		out.WriteString(transform(args))
		i = k
	}
	return out.String()
}

// rewriteOracleMultisetCast finds `CAST(MULTISET(<inner_select>) AS
// <list_typ>)` invocations and rewrites each into PG's `ARRAY(...)`
// form. The element shape comes from the userTypes registry:
//
//   - array_scalar  → ARRAY(<inner_select>)        (single-column SELECT)
//   - array_composite → ARRAY(SELECT ROW(__row.*)::"mig"."<elem>"
//                              FROM (<inner_select>) __row)
//
// Unknown list-type names (not in userTypes) leave the original CAST
// alone so a later pass / manual review can spot them.
func rewriteOracleMultisetCast(body string, types map[string]UserTypeRef) string {
	if body == "" || len(types) == 0 {
		return body
	}
	upper := strings.ToUpper(body)
	var out strings.Builder
	out.Grow(len(body))
	i := 0
	for i < len(body) {
		if body[i] == '\'' {
			// Pass literals through.
			out.WriteByte(body[i])
			i++
			for i < len(body) {
				out.WriteByte(body[i])
				if body[i] == '\'' {
					if i+1 < len(body) && body[i+1] == '\'' {
						out.WriteByte(body[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// Look for whole-word CAST.
		const castKW = "CAST"
		if i+len(castKW) > len(upper) || upper[i:i+len(castKW)] != castKW ||
			(i > 0 && isIdentByte(body[i-1])) || (i+len(castKW) < len(body) && isIdentByte(body[i+len(castKW)])) {
			out.WriteByte(body[i])
			i++
			continue
		}
		// CAST must be followed (after ws) by '('.
		j := i + len(castKW)
		for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\r' || body[j] == '\n') {
			j++
		}
		if j >= len(body) || body[j] != '(' {
			out.WriteByte(body[i])
			i++
			continue
		}
		// Find the matching ')' for the CAST's outer parens.
		castOpen := j
		castClose := matchParenAt(body, castOpen)
		if castClose < 0 {
			out.WriteByte(body[i])
			i++
			continue
		}
		inner := body[castOpen+1 : castClose]
		// inner should be `MULTISET(...) AS <list_typ>`.
		multUpper := strings.ToUpper(inner)
		const mkw = "MULTISET"
		mPos := strings.Index(multUpper, mkw)
		if mPos < 0 {
			out.WriteByte(body[i])
			i++
			continue
		}
		// Whole-word + followed by '('.
		if (mPos > 0 && isIdentByte(inner[mPos-1])) ||
			(mPos+len(mkw) < len(inner) && isIdentByte(inner[mPos+len(mkw)])) {
			out.WriteByte(body[i])
			i++
			continue
		}
		mAfter := mPos + len(mkw)
		for mAfter < len(inner) && (inner[mAfter] == ' ' || inner[mAfter] == '\t' || inner[mAfter] == '\r' || inner[mAfter] == '\n') {
			mAfter++
		}
		if mAfter >= len(inner) || inner[mAfter] != '(' {
			out.WriteByte(body[i])
			i++
			continue
		}
		mClose := matchParenAt(inner, mAfter)
		if mClose < 0 {
			out.WriteByte(body[i])
			i++
			continue
		}
		innerSelect := strings.TrimSpace(inner[mAfter+1 : mClose])
		// Recursively rewrite nested CAST(MULTISET(…)) inside the SELECT
		// — Oracle frequently nests collections two levels deep
		// (CUSTOMER_TYP.cust_orders ⇒ ORDER_LIST_TYP, each element being
		// ORDER_TYP.order_item_list ⇒ ORDER_ITEM_LIST_TYP). Without the
		// recursion the inner CAST would land verbatim in the emitted
		// PG body and trigger a syntax error at apply time.
		innerSelect = rewriteOracleMultisetCast(innerSelect, types)
		// After the MULTISET(...), expect `AS <list_typ>`.
		tail := strings.TrimSpace(inner[mClose+1:])
		if !strings.HasPrefix(strings.ToUpper(tail), "AS ") && strings.ToUpper(tail) != "AS" {
			out.WriteByte(body[i])
			i++
			continue
		}
		typName := strings.TrimSpace(tail[2:])
		typName = strings.Trim(typName, `"`)
		if k := strings.LastIndex(typName, "."); k >= 0 {
			typName = strings.Trim(typName[k+1:], `"`)
		}
		ref, ok := types[strings.ToLower(typName)]
		if !ok {
			out.WriteByte(body[i])
			i++
			continue
		}
		// Emit the PG ARRAY form.
		switch ref.Kind {
		case "array_scalar":
			out.WriteString("ARRAY(")
			out.WriteString(innerSelect)
			out.WriteString(")")
		case "array_composite":
			out.WriteString("ARRAY(SELECT ROW(__row.*)::")
			out.WriteString(ref.ElemPG)
			out.WriteString(" FROM (")
			out.WriteString(innerSelect)
			out.WriteString(") __row)")
		default:
			// Unknown collection kind — leave original CAST verbatim.
			out.WriteString(body[i : castClose+1])
		}
		i = castClose + 1
	}
	return out.String()
}

// matchParenAt returns the offset of the ')' that closes the '(' at
// position `open` in `s`, honoring nested parens and skipping single-
// quoted string literals. Returns -1 when there is no matching ')'.
func matchParenAt(s string, open int) int {
	if open < 0 || open >= len(s) || s[open] != '(' {
		return -1
	}
	depth := 1
	inStr := false
	for k := open + 1; k < len(s); k++ {
		c := s[k]
		if inStr {
			if c == '\'' {
				if k+1 < len(s) && s[k+1] == '\'' {
					k++
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
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return -1
}

// rewriteOracleCompositeAttrAccess wraps the receiver in parens for every
// `<x>.<col>.<attr>` pattern where `<col>` matches one of the migration's
// known composite columns. Oracle accepts dotted composite-attribute
// access without parens; PG parses `c.cust_address.country_id` as a
// 3-part qualified column reference (schema.table.col), so the body must
// be rewritten to `(c.cust_address).country_id`.
func rewriteOracleCompositeAttrAccess(body string, compositeCols map[string]bool) string {
	if body == "" || len(compositeCols) == 0 {
		return body
	}
	var out strings.Builder
	out.Grow(len(body))
	i := 0
	for i < len(body) {
		// Pass literals through.
		if body[i] == '\'' {
			out.WriteByte(body[i])
			i++
			for i < len(body) {
				out.WriteByte(body[i])
				if body[i] == '\'' {
					if i+1 < len(body) && body[i+1] == '\'' {
						out.WriteByte(body[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// Look for an identifier start.
		if !isIdentByte(body[i]) || (i > 0 && isIdentByte(body[i-1])) {
			out.WriteByte(body[i])
			i++
			continue
		}
		// Read first segment.
		j := i
		for j < len(body) && isIdentByte(body[j]) {
			j++
		}
		first := body[i:j]
		if j >= len(body) || body[j] != '.' {
			out.WriteString(first)
			i = j
			continue
		}
		// Read second segment.
		k := j + 1
		for k < len(body) && isIdentByte(body[k]) {
			k++
		}
		if k == j+1 {
			out.WriteString(first)
			i = j
			continue
		}
		second := body[j+1 : k]
		// Need a third dotted segment to actually need parens.
		if k >= len(body) || body[k] != '.' {
			out.WriteString(first)
			out.WriteByte('.')
			out.WriteString(second)
			i = k
			continue
		}
		l := k + 1
		for l < len(body) && isIdentByte(body[l]) {
			l++
		}
		if l == k+1 {
			out.WriteString(first)
			out.WriteByte('.')
			out.WriteString(second)
			i = k
			continue
		}
		third := body[k+1 : l]
		// Rewrite only when the middle segment names a composite column.
		if compositeCols[strings.ToLower(second)] {
			out.WriteByte('(')
			out.WriteString(first)
			out.WriteByte('.')
			out.WriteString(second)
			out.WriteByte(')')
			out.WriteByte('.')
			out.WriteString(third)
		} else {
			out.WriteString(first)
			out.WriteByte('.')
			out.WriteString(second)
			out.WriteByte('.')
			out.WriteString(third)
		}
		i = l
	}
	return out.String()
}

// findOracleObjectRelationalMarker returns the first Oracle-specific
// object-relational keyword found in the view body (whole-word, ignoring
// content inside string literals), or "" if none is present. Used to skip
// view emission for bodies whose semantics can't translate to PG without
// a structured rewrite of the SELECT.
func findOracleObjectRelationalMarker(body string) string {
	if body == "" {
		return ""
	}
	upper := strings.ToUpper(body)
	for _, kw := range []string{"MULTISET", "MAKE_REF", "DEREF"} {
		i := 0
		for i < len(upper) {
			k := strings.Index(upper[i:], kw)
			if k < 0 {
				break
			}
			k += i
			leftOK := k == 0 || !isIdentByte(upper[k-1])
			rightOK := k+len(kw) == len(upper) || !isIdentByte(upper[k+len(kw)])
			if leftOK && rightOK && !insideStringLiteral(body, k) {
				return kw
			}
			i = k + len(kw)
		}
	}
	return ""
}

// insideStringLiteral returns true when offset `at` falls inside a
// single-quoted SQL literal in `s`. Doubled-up single quotes (PG-escaped
// `''`) toggle in/out symmetrically, matching SQL semantics.
func insideStringLiteral(s string, at int) bool {
	in := false
	for i := 0; i < at && i < len(s); i++ {
		if s[i] == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			in = !in
		}
	}
	return in
}

// rewriteOracleCompositeConstructors replaces Oracle composite-type
// constructor invocations like `warehouse_typ(w.warehouse_id,
// w.warehouse_name, w.location_id)` with the PG equivalent
// `ROW(w.warehouse_id, w.warehouse_name, w.location_id)::"mig"."warehouse_typ"`.
// Only types that the same migration emitted as composites
// (`UserTypeRef.Kind == "composite"`) are rewritten; everything else
// (genuine function calls, casts, …) is left alone.
//
// The scan is identifier-aware: matches must be preceded by a non-ident
// byte (so `my_warehouse_typ(` doesn't trigger), and must be followed by
// a `(` (so `warehouse_typ.col` doesn't trigger). Single-quoted literals
// are skipped — `'warehouse_typ('` in a comment or constant won't fire.
func rewriteOracleCompositeConstructors(body string, types map[string]UserTypeRef, targetSchema string) string {
	if body == "" || len(types) == 0 {
		return body
	}
	// Build a sorted list of composite type names for deterministic
	// matching (longer first to avoid `cat` matching inside `catalog_typ`).
	var names []string
	for k, ref := range types {
		if ref.Kind == "composite" {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return body
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })

	upper := strings.ToUpper(body)
	var out strings.Builder
	out.Grow(len(body))
	i := 0
	for i < len(body) {
		// Skip single-quoted string literals verbatim.
		if body[i] == '\'' {
			out.WriteByte(body[i])
			i++
			for i < len(body) {
				out.WriteByte(body[i])
				if body[i] == '\'' {
					if i+1 < len(body) && body[i+1] == '\'' {
						out.WriteByte(body[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// Identifier boundary: previous byte must be non-ident.
		left := i == 0 || !isIdentByte(body[i-1])
		matched := ""
		if left {
			for _, n := range names {
				up := strings.ToUpper(n)
				end := i + len(up)
				if end > len(upper) {
					continue
				}
				if upper[i:end] != up {
					continue
				}
				// Right boundary: must be followed (after optional ws) by `(`.
				j := end
				for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\r' || body[j] == '\n') {
					j++
				}
				if j >= len(body) || body[j] != '(' {
					continue
				}
				// Right boundary part 2: ident byte right after the name
				// kills the match — e.g. `warehouse_typX(` shouldn't match
				// `warehouse_typ`.
				if isIdentByte(body[end]) {
					continue
				}
				matched = n
				break
			}
		}
		if matched == "" {
			out.WriteByte(body[i])
			i++
			continue
		}
		// Skip past the type name + optional whitespace to the `(`.
		end := i + len(matched)
		j := end
		for body[j] != '(' {
			j++
		}
		// Find the matching `)` for the constructor's argument list.
		depth := 1
		k := j + 1
		inStr := false
		for k < len(body) && depth > 0 {
			c := body[k]
			if inStr {
				if c == '\'' {
					if k+1 < len(body) && body[k+1] == '\'' {
						k += 2
						continue
					}
					inStr = false
				}
				k++
				continue
			}
			switch c {
			case '\'':
				inStr = true
			case '(':
				depth++
			case ')':
				depth--
			}
			k++
		}
		// k now points just past the matching ')'. Emit the rewritten form.
		args := body[j+1 : k-1]
		out.WriteString("ROW(")
		out.WriteString(args)
		out.WriteString(")::")
		out.WriteString(pgQualified(targetSchema, matched))
		i = k
	}
	return out.String()
}
