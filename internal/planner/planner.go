// Package planner decomposes a migration into an ordered DAG of steps and
// initial step_batches entries ready for enqueueing.
package planner

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	oracle "gitlab.com/dalibo/squishy/internal/dialects/oracle"
	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// Step is an in-memory representation of a squishy.steps row before persistence.
type Step struct {
	ID        uuid.UUID
	Seq       int
	Kind      string // inspect|create_ddl|copy_table|create_index|create_fk|create_routine|validate
	Target    string
	Priority  int16
	Payload   map[string]any
	DependsOn []uuid.UUID
	RowsTotal int64
	Level     int
}

// Plan is the full in-memory plan: steps with resolved dependencies.
type Plan struct {
	Steps []Step
}

// BuildOptions controls plan-shape variants that depend on
// instance-creation choices the user made (target DB / schema names,
// whether squishy must create the target DB itself, etc).
type BuildOptions struct {
	TargetSchema   string // PG schema (e.g. "public", "hr")
	TargetDBName   string // PG database (e.g. "hr"). Used by create_target_db step payload.
	CreateTargetDB bool   // emit a leading create_target_db step
	// SkipData omits copy_table + create_index steps. Useful when iterating
	// on the routine/trigger translator after a successful first run: PG
	// already has the rows + indexes, only the routines DDL changed. With
	// SkipData=true, create_fk depends directly on create_ddl (so FK and
	// routine creation still run in order on the existing schema).
	SkipData bool
}

// copySourceVariantSystemTimeHistory is the copy_table payload
// source_variant read by the worker (internal/worker, same constant name)
// to copy the history rows of a MariaDB system-versioned table.
const copySourceVariantSystemTimeHistory = "system_time_history"

// Build produces a DAG from an inspected source schema + a translated PG plan.
// The planner is deterministic: steps are assigned sequential seq numbers and
// dependencies are encoded by UUID references.
//
// Ordering:
//  0. create_target_db (optional; only when opts.CreateTargetDB is true)
//  1. inspect (the freshly-captured source is already in DB; the step re-validates drift)
//  2. create_ddl (all CREATE TABLE, no indexes / no FKs yet)
//  3. copy_table:<name> for each table, parallelizable
//  4. create_index:<table> per table (depends on copy_table of the same table)
//  5. create_fk (depends on all create_index steps)
//  6. create_routine for each view/trigger/procedure/function (post-FK, once the schema is stable)
//  7. validate (depends on everything)
func Build(source *inspect.SourceSchema, pg *translate.Result, opts BuildOptions) *Plan {
	p := &Plan{}

	addStep := func(kind, target string, prio int16, payload map[string]any, deps ...uuid.UUID) uuid.UUID {
		s := Step{
			ID: uuid.New(), Seq: len(p.Steps), Kind: kind, Target: target,
			Priority: prio, Payload: payload, DependsOn: deps,
		}
		p.Steps = append(p.Steps, s)
		return s.ID
	}

	// Optional create_target_db step: when the user chose the dedicated_db
	// mapping strategy, squishy must CREATE DATABASE on the admin pool
	// before anything else (the per-migration target pool isn't connected
	// to a database that exists yet). Idempotent on the worker side.
	var preDeps []uuid.UUID
	if opts.CreateTargetDB {
		createDBID := addStep("create_target_db", "", 5, map[string]any{
			"db_name": opts.TargetDBName,
		})
		preDeps = []uuid.UUID{createDBID}
	}

	// targetSchema is the PG schema where the migrated objects live. The
	// translator already qualifies its DDL with this name; we propagate it
	// into every step payload so the worker handlers can target the same
	// schema (instead of the legacy hard-coded "mig").
	targetSchema := opts.TargetSchema
	if targetSchema == "" {
		targetSchema = "public"
	}

	inspectID := addStep("inspect", "", 10, map[string]any{"database": source.Database}, preDeps...)
	ddlID := addStep("create_ddl", "", 20, map[string]any{
		"script": pg.DDLScript,
	}, inspectID)

	// One copy_table step per base table — skipped under SkipData.
	copyIDs := make(map[string]uuid.UUID)
	if !opts.SkipData {
		for _, tbl := range source.Tables {
			payload := map[string]any{
				"schema":        tbl.Database,
				"table":         tbl.Name,
				"rows":          tbl.Rows,
				"target_schema": targetSchema,
			}
			// An emulated MariaDB system-versioned table copies the
			// column set of its translated table (ROW START / ROW END
			// included), which the source catalog's default list —
			// every non-generated column — does not give.
			if sv := emulatedSystemVersioning(pg, tbl.Name); sv != nil && len(sv.CopyColumns) > 0 {
				payload["columns"] = append([]string{}, sv.CopyColumns...)
			}
			id := addStep("copy_table", tbl.Name, 100, payload, ddlID)
			// Rows estimated from information_schema; actual count computed by the worker.
			p.Steps[len(p.Steps)-1].RowsTotal = tbl.Rows
			copyIDs[tbl.Name] = id
		}
		// MariaDB system-versioned tables emulated by the translator: the
		// closed row versions (`FOR SYSTEM_TIME ALL`, ROW END in the past)
		// go to the PG history table. Same DAG position as a table copy,
		// so create_fk — which installs the versioning triggers — waits
		// for it.
		for _, tbl := range pg.Plan.Tables {
			sv := tbl.SystemVersioning
			if sv == nil || sv.HistoryTable == "" {
				continue
			}
			id := addStep("copy_table", sv.HistoryTable, 100, map[string]any{
				"schema":         source.Database,
				"table":          tbl.Name,
				"target_schema":  targetSchema,
				"target_table":   sv.HistoryTable,
				"source_variant": copySourceVariantSystemTimeHistory,
				"row_end_col":    sv.RowEnd,
				// Every history-table column, generated ones included:
				// the history table stores their archived values.
				"columns": append([]string{}, sv.HistoryCopyColumns...),
			}, ddlID)
			copyIDs[sv.HistoryTable] = id
		}
	}

	// One create_index step per table — depends on copy_table of the same
	// table. Skipped under SkipData (the existing indexes carry over).
	indexIDs := []uuid.UUID{}
	if !opts.SkipData {
		for tname, copyID := range copyIDs {
			id := addStep("create_index", tname, 200, map[string]any{
				"table":         tname,
				"target_schema": targetSchema,
			}, copyID)
			indexIDs = append(indexIDs, id)
		}
	}

	// Single create_fk step — depends on every create_index normally, or
	// directly on create_ddl when SkipData is set (no per-table indexes).
	fkDeps := indexIDs
	if opts.SkipData {
		fkDeps = []uuid.UUID{ddlID}
	}
	fkID := addStep("create_fk", "", 210, map[string]any{
		"script": pg.DDLPostCopy,
	}, fkDeps...)

	// Views first — they may be referenced by triggers (INSTEAD OF on a view)
	// or by other routines, so the trigger/routine dependency wiring below
	// can read the view IDs.
	viewIDs := make(map[string]uuid.UUID, len(pg.Plan.Views))
	for _, v := range pg.Plan.Views {
		id := addStep("create_routine", "view:"+v.Name, 220, map[string]any{
			"name":          v.Name,
			"kind":          "view",
			"ddl":           v.DDL,
			"schema":        v.Schema,
			"target_schema": targetSchema,
		}, fkID)
		viewIDs[v.Name] = id
	}
	// Routines (triggers, procedures, functions). Each step carries the
	// target schema so hCreateRoutine can set search_path and resolve
	// unqualified identifiers in the MySQL-born body. Triggers that
	// reference a view (INSTEAD OF on a view, or bodies that touch a view)
	// must wait for the view to be created — otherwise the CREATE TRIGGER
	// fails with "relation does not exist".
	for _, r := range pg.Plan.Routines {
		deps := []uuid.UUID{fkID}
		for vname, vID := range viewIDs {
			if referencesIdent(r.DDL, vname) {
				deps = append(deps, vID)
			}
		}
		addStep("create_routine", r.Kind+":"+r.Name, 220, map[string]any{
			"name":          r.Name,
			"kind":          r.Kind,
			"ddl":           r.DDL,
			"schema":        r.Schema,
			"target_schema": targetSchema,
		}, deps...)
	}
	// Views can reference other views (e.g. the employees sample's
	// current_dept_emp joins dept_emp_latest_date). Wire view→view deps
	// so creation order is deterministic instead of relying on job
	// retries. A view translated from a typed body (MySQL / MariaDB)
	// carries References — the relations its SELECT reads, collected
	// from the AST — and is matched against the other view names by
	// set lookup. Views without References (Oracle, DB2, bodies the
	// parser could not type) fall back to the token walk of
	// referencesIdent over the DDL + source body.
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Kind != "create_routine" || !strings.HasPrefix(s.Target, "view:") {
			continue
		}
		vname := strings.TrimPrefix(s.Target, "view:")
		var body string
		var refs []string
		for _, v := range pg.Plan.Views {
			if v.Name == vname {
				body = v.DDL + "\n" + v.SelectBody
				refs = v.References
				break
			}
		}
		for other, otherID := range viewIDs {
			if other == vname {
				continue
			}
			var dep bool
			if refs != nil {
				dep = referencesName(refs, other)
			} else {
				dep = referencesIdent(body, other)
			}
			if dep {
				s.DependsOn = append(s.DependsOn, otherID)
			}
		}
	}
	// Events become a no-op here but land in warnings/explanations.

	// validate — depends on everything.
	depAll := make([]uuid.UUID, 0, len(p.Steps))
	for _, s := range p.Steps {
		depAll = append(depAll, s.ID)
	}
	addStep("validate", "", 300, map[string]any{
		"tables":         tableNames(source.Tables),
		"history_tables": historyValidations(pg),
		"source_schema":  source.Database,
		"target_schema":  targetSchema,
	}, depAll...)

	assignLevels(p.Steps)
	return p
}

// assignLevels computes each step's topological level in place.
// A step with no deps is level 0; otherwise 1 + max(level of deps).
func assignLevels(steps []Step) {
	byID := make(map[uuid.UUID]int, len(steps))
	for i, s := range steps {
		byID[s.ID] = i
	}
	var resolve func(i int) int
	resolve = func(i int) int {
		s := &steps[i]
		if len(s.DependsOn) == 0 {
			s.Level = 0
			return 0
		}
		if s.Level > 0 {
			return s.Level
		}
		max := -1
		for _, d := range s.DependsOn {
			idx, ok := byID[d]
			if !ok {
				continue
			}
			if pl := resolve(idx); pl > max {
				max = pl
			}
		}
		s.Level = max + 1
		return s.Level
	}
	for i := range steps {
		resolve(i)
	}
}

// emulatedSystemVersioning returns the system-versioning emulation of
// the translated table named table, or nil. The name is matched
// exactly: the translator names a MySQL/MariaDB table after its SHOW
// CREATE TABLE, like the inspector's snapshot, and two tables may differ
// only by case on a case-sensitive source (lower_case_table_names=0).
func emulatedSystemVersioning(pg *translate.Result, table string) *translate.PGSystemVersioning {
	if pg == nil {
		return nil
	}
	for i := range pg.Plan.Tables {
		if pg.Plan.Tables[i].Name == table {
			return pg.Plan.Tables[i].SystemVersioning
		}
	}
	return nil
}

// referencesName reports whether refs (the relation names a typed view
// body reads, see translate.PGView.References) contains name. Names are
// single identifiers, compared case-insensitively.
func referencesName(refs []string, name string) bool {
	for _, r := range refs {
		if strings.EqualFold(r, name) {
			return true
		}
	}
	return false
}

// referencesIdent reports whether body contains ident as a standalone SQL
// identifier token (case-insensitive). Used to detect view→view and
// routine→view references in PG-emitted DDL.
//
// The body is walked with the Oracle lexer, whose lexical rules match the
// PG DDL squishy emits ("quoted idents", 'strings', -- and /* */
// comments). A match is an IDENT, QUOTED_IDENT or KEYWORD token (an
// identifier that happens to be an Oracle keyword, e.g. a view named
// `level`, lexes as KEYWORD) whose literal equals ident
// case-insensitively. Consequently a name that only appears inside a
// string literal or a comment is NOT a reference, and a name that is
// merely a prefix of a longer identifier (dept_emp vs
// dept_emp_latest_date) never matches. Runes the lexer does not know
// (e.g. the `$` of a `$body$` dollar-quote) come out as single-rune error
// tokens, so the walk always advances and terminates.
func referencesIdent(body, ident string) bool {
	if body == "" || ident == "" {
		return false
	}
	l := oracle.NewLexer(body)
	for {
		t := l.Next()
		switch t.Kind {
		case oracle.TOK_EOF:
			return false
		case oracle.TOK_IDENT, oracle.TOK_QUOTED_IDENT, oracle.TOK_KEYWORD:
			if strings.EqualFold(t.Lit, ident) {
				return true
			}
		}
	}
}

// ValidateHistoryTable is one entry of the validate step's
// history_tables payload: an emulated MariaDB system-versioned table
// whose closed versions were copied into TargetTable. The worker counts
// the source through the same `FOR SYSTEM_TIME ALL` history read as the
// copy (row_end_col in the past) and compares it with TargetTable.
type ValidateHistoryTable struct {
	SourceTable string `json:"source_table"`
	TargetTable string `json:"target_table"`
	RowEndCol   string `json:"row_end_col"`
}

// historyValidations lists the history tables of pg's emulated
// system-versioned tables, in plan order.
func historyValidations(pg *translate.Result) []ValidateHistoryTable {
	out := []ValidateHistoryTable{}
	if pg == nil {
		return out
	}
	for _, tbl := range pg.Plan.Tables {
		sv := tbl.SystemVersioning
		if sv == nil || sv.HistoryTable == "" {
			continue
		}
		out = append(out, ValidateHistoryTable{
			SourceTable: tbl.Name, TargetTable: sv.HistoryTable, RowEndCol: sv.RowEnd,
		})
	}
	return out
}

func tableNames(ts []inspect.ObjectSnapshot) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// Summary returns a one-line tally for logs.
func (p *Plan) Summary() string {
	counts := map[string]int{}
	for _, s := range p.Steps {
		counts[s.Kind]++
	}
	return fmt.Sprintf("plan: %d steps (%v)", len(p.Steps), counts)
}
