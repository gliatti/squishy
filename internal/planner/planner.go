// Package planner decomposes a migration into an ordered DAG of steps and
// initial step_batches entries ready for enqueueing.
package planner

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// Step is an in-memory representation of a squishy.steps row before persistence.
type Step struct {
	ID         uuid.UUID
	Seq        int
	Kind       string // inspect|create_ddl|copy_table|create_index|create_fk|create_routine|validate
	Target     string
	Priority   int16
	Payload    map[string]any
	DependsOn  []uuid.UUID
	RowsTotal  int64
	Level      int
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
			id := addStep("copy_table", tbl.Name, 100, map[string]any{
				"schema":        tbl.Database,
				"table":         tbl.Name,
				"rows":          tbl.Rows,
				"target_schema": targetSchema,
			}, ddlID)
			// Rows estimated from information_schema; actual count computed by the worker.
			p.Steps[len(p.Steps)-1].RowsTotal = tbl.Rows
			copyIDs[tbl.Name] = id
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
	// based on identifier references so creation order is deterministic
	// instead of relying on job retries.
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Kind != "create_routine" || !strings.HasPrefix(s.Target, "view:") {
			continue
		}
		vname := strings.TrimPrefix(s.Target, "view:")
		var body string
		for _, v := range pg.Plan.Views {
			if v.Name == vname {
				body = v.DDL + "\n" + v.SelectBody
				break
			}
		}
		for other, otherID := range viewIDs {
			if other == vname {
				continue
			}
			if referencesIdent(body, other) {
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
		"tables":        tableNames(source.Tables),
		"source_schema": source.Database,
		"target_schema": targetSchema,
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

// referencesIdent reports whether body contains ident as a standalone SQL
// identifier (word-boundary match, case-insensitive). Used to detect
// view→view references in view bodies.
func referencesIdent(body, ident string) bool {
	if body == "" || ident == "" {
		return false
	}
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(ident) + `\b`)
	return re.MatchString(body)
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
