package translate

// mariadb_temporal.go — MariaDB temporal tables → PostgreSQL.
//
// Two MariaDB features are covered:
//
//   - Application-time periods: `PERIOD FOR p (s, e)`. At the schema
//     level this is two plain columns plus an implicit CHECK (s < e)
//     named after the period — replicated as a named PG CHECK built from
//     AST nodes. A key part `p WITHOUT OVERLAPS` (MariaDB 10.5+) needs an
//     EXCLUDE USING gist constraint that squishy does not generate; the
//     key is reported (table.application_period → blocking prerequisite)
//     instead of being emitted as a wrong plain UNIQUE.
//
//   - System versioning: `WITH SYSTEM VERSIONING` with an explicit
//     `PERIOD FOR SYSTEM_TIME (row_start, row_end)` over two
//     `GENERATED ALWAYS AS ROW START|END` TIMESTAMP columns, when the
//     inspector could probe the ROW END value the source stores on
//     current rows (Options.MariaDBRowEndMax). PostgreSQL has no native
//     equivalent; it is emulated:
//       * the current table keeps both columns (TIMESTAMPTZ NOT NULL) and
//         the copier transfers their source values (the MySQL session is
//         pinned to UTC, see connection.Params.MySQLDSN, so the copied
//         ROW END of current rows equals the probed sentinel);
//       * a `<table>_history` table receives the closed versions — the
//         source's `FOR SYSTEM_TIME ALL` rows whose ROW END is in the past
//         (planner copy step, see planner.Build);
//       * post-copy, a BEFORE INSERT OR UPDATE row trigger stamps
//         row_start = statement_timestamp() / row_end = the MariaDB
//         open-ended sentinel, and an AFTER UPDATE OR DELETE row trigger
//         archives OLD into the history table closed at the change time.
//         MariaDB uses the statement start time for both stamps and
//         versions every UPDATE, even one that leaves the data unchanged
//         (checked on MariaDB 11.8: `UPDATE t SET c = c` adds a history
//         row); the triggers do the same. A zero-length version (created
//         and replaced within one statement) is not archived. Columns
//         declared WITHOUT SYSTEM VERSIONING restrict the UPDATE events
//         to `UPDATE OF <versioned columns>`.
//     The implicit form (hidden row_start / row_end columns, absent from
//     SHOW CREATE TABLE — also what `ALTER TABLE … ADD SYSTEM
//     VERSIONING` leaves) is first rewritten to the equivalent explicit
//     declaration (withImplicitSystemTime) and emulated the same way,
//     the hidden columns becoming regular PG columns. `PARTITION BY
//     SYSTEM_TIME` only splits the source's storage between HISTORY and
//     CURRENT partitions: the emulation's history table is that split
//     (see systemTimePartitioning).
//     Transaction-precise versioning (BIGINT UNSIGNED period columns
//     holding transaction ids) and a source whose ROW END sentinel is
//     unknown are not emulated: they keep the drop-and-warn path
//     (table.system_versioning → blocking prerequisite).
//
// The trigger bodies are built as typed PL AST nodes and rendered by the
// PG writer (pgast.WriteBlock / pgast.Write); nothing here inspects SQL
// text.

import (
	"fmt"
	"hash/fnv"
	"strings"
	"unicode/utf8"

	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Why a system-versioned table is not emulated (mariadbSystemVersioning).
const (
	sysverImplicit   = "implicit"    // no explicit ROW START / ROW END column pair
	sysverTrxPrecise = "trx_precise" // period columns are not TIMESTAMP
	sysverNoSentinel = "no_sentinel" // source ROW END maximum unknown
	sysverSpatial    = "spatial"     // a spatial column the copier cannot transfer
)

// mariadbSystemVersioning returns the emulation descriptor of a
// system-versioned table. It returns (nil, "") for a table that is not
// system-versioned, and (nil, reason) for one that cannot be emulated:
//
//   - sysverImplicit: no explicit ROW START / ROW END column pair — the
//     implicit form, when withImplicitSystemTime could not declare its
//     hidden columns (a column already uses one of their names);
//   - sysverTrxPrecise: the period columns are not TIMESTAMP — MariaDB
//     only accepts TIMESTAMP(6) (timestamp-based versioning) or BIGINT
//     UNSIGNED (transaction-precise versioning, where ROW START / ROW
//     END hold transaction ids resolved through
//     mysql.transaction_registry, which PostgreSQL cannot reproduce);
//   - sysverNoSentinel: rowEndMax (the ROW END value of current rows on
//     the source server) is unknown, so rows written in PostgreSQL could
//     not be stamped like the copied current rows;
//   - sysverSpatial: a column has a spatial type. The data copier cannot
//     transfer spatial values (dataxfer ListColumnsQuery skips them), so
//     the archived versions copied into the history table would silently
//     lose them.
//
// Column names are matched case-insensitively (MySQL/MariaDB column
// identifiers are); the descriptor carries each column's declared
// spelling, the one the PG table is created with. HistoryTable is filled
// by emitSystemVersioning.
func mariadbSystemVersioning(s *ast.CreateTable, rowEndMax string) (*PGSystemVersioning, string) {
	if s == nil || !s.SystemVersioned {
		return nil, ""
	}
	var start, end string
	for _, pd := range s.Periods {
		if strings.EqualFold(pd.Name, "SYSTEM_TIME") {
			start, end = pd.StartCol, pd.EndCol
		}
	}
	if start == "" || end == "" || strings.EqualFold(start, end) {
		return nil, sysverImplicit
	}
	var startCol, endCol *ast.ColumnDef
	for _, c := range s.Columns {
		if !c.SystemVersioning {
			continue
		}
		switch {
		case strings.EqualFold(c.Name, start):
			startCol = c
		case strings.EqualFold(c.Name, end):
			endCol = c
		}
	}
	if startCol == nil || endCol == nil {
		return nil, sysverImplicit
	}
	if !isTimestampType(startCol.Type) || !isTimestampType(endCol.Type) {
		return nil, sysverTrxPrecise
	}
	if rowEndMax == "" {
		return nil, sysverNoSentinel
	}
	for _, c := range s.Columns {
		if _, spatial := c.Type.(*ast.SpatialType); spatial {
			return nil, sysverSpatial
		}
	}
	sv := &PGSystemVersioning{RowStart: startCol.Name, RowEnd: endCol.Name, RowEndMax: rowEndMax}
	// Column-level form: without the table-level WITH SYSTEM VERSIONING
	// clause, MariaDB versions only the columns declared WITH SYSTEM
	// VERSIONING (checked on MariaDB 11.8: `CREATE TABLE t (a int, b int
	// WITH SYSTEM VERSIONING)` is shown by SHOW CREATE TABLE as `a …
	// WITHOUT SYSTEM VERSIONING, b …` + table-level WITH SYSTEM
	// VERSIONING, and an UPDATE of a alone writes no history row).
	columnLevel := false
	if !s.Options.SystemVersioning {
		for _, c := range s.Columns {
			if c.WithSystemVersioning {
				columnLevel = true
			}
		}
	}
	for _, c := range s.Columns {
		if c.SystemVersioning {
			continue
		}
		if c.WithoutSystemVersioning || (columnLevel && !c.WithSystemVersioning) {
			sv.Unversioned = append(sv.Unversioned, c.Name)
		}
	}
	return sv, ""
}

// Names of the hidden period columns MariaDB maintains on a table
// versioned in the implicit form.
const (
	mariadbImplicitRowStart = "row_start"
	mariadbImplicitRowEnd   = "row_end"
)

// withImplicitSystemTime returns s unchanged, or — for a table
// system-versioned in the implicit form — a shallow copy that declares
// the period MariaDB maintains behind the scenes, so the emulation
// (mariadbSystemVersioning) handles it like the explicit form.
//
// The implicit form is `WITH SYSTEM VERSIONING` without a PERIOD FOR
// SYSTEM_TIME: SHOW CREATE TABLE renders it for `CREATE TABLE …
// WITH SYSTEM VERSIONING` without period columns and for `ALTER TABLE
// … ADD SYSTEM VERSIONING`. MariaDB then keeps two hidden columns,
// always named row_start / row_end and typed TIMESTAMP(6) (checked on
// MariaDB 11.8: they are absent from SHOW CREATE TABLE and
// information_schema.COLUMNS, selectable by name — `SELECT row_start,
// row_end FROM t FOR SYSTEM_TIME ALL` — and a user column with either
// name is rejected with ERROR 1060 "Duplicate column name"). The copy
// appends them as `GENERATED ALWAYS AS ROW START|END` TIMESTAMP(6)
// columns plus `PERIOD FOR SYSTEM_TIME (row_start, row_end)`.
//
// A table that already has a column of either name (not a MariaDB
// SHOW CREATE TABLE output) is returned unchanged and keeps the
// not-emulated warning (sysverImplicit). The parsed statement is never
// modified.
func withImplicitSystemTime(s *ast.CreateTable) (*ast.CreateTable, bool) {
	if s == nil || !s.SystemVersioned {
		return s, false
	}
	for _, pd := range s.Periods {
		if strings.EqualFold(pd.Name, "SYSTEM_TIME") {
			return s, false
		}
	}
	for _, c := range s.Columns {
		if c.SystemVersioning || isColumn(c.Name, mariadbImplicitRowStart) || isColumn(c.Name, mariadbImplicitRowEnd) {
			return s, false
		}
	}
	cp := *s
	cp.Columns = make([]*ast.ColumnDef, 0, len(s.Columns)+2)
	cp.Columns = append(cp.Columns, s.Columns...)
	for _, name := range []string{mariadbImplicitRowStart, mariadbImplicitRowEnd} {
		cp.Columns = append(cp.Columns, &ast.ColumnDef{
			Name:             name,
			Type:             &ast.TimestampType{Fsp: 6},
			NotNull:          true,
			HasNullable:      true,
			SystemVersioning: true,
		})
	}
	cp.Periods = make([]ast.PeriodDef, 0, len(s.Periods)+1)
	cp.Periods = append(cp.Periods, s.Periods...)
	cp.Periods = append(cp.Periods, ast.PeriodDef{
		Name:     "SYSTEM_TIME",
		StartCol: mariadbImplicitRowStart,
		EndCol:   mariadbImplicitRowEnd,
	})
	return &cp, true
}

// systemTimePartitioning explains how MariaDB's `PARTITION BY
// SYSTEM_TIME` of table s is migrated. sv is the table's emulation
// descriptor (nil when its versioning is not emulated).
//
// History partitioning does not change which rows a system-versioned
// table holds, only where MariaDB stores them: the CURRENT partition
// keeps the current rows, the HISTORY partitions the closed versions,
// rotated by the LIMIT / INTERVAL rule. The emulation makes the same
// split — current rows in the table, every closed version (read through
// `FOR SYSTEM_TIME ALL`, whichever HISTORY partition holds it) in its
// history table — so nothing is lost and the PG tables are created
// unpartitioned; only the rotation, an administration aid for purging
// old history by partition, has no counterpart. When the versioning is
// not emulated, the history loss is already reported by the
// table.system_versioning warning and blocking prerequisite; the
// partitioning adds nothing to report beyond a note.
func (t *translator) systemTimePartitioning(s *ast.CreateTable, sv *PGSystemVersioning) {
	pn := s.Partitioning
	rule := "PARTITION BY SYSTEM_TIME"
	switch {
	case pn.SystemTimeLimit > 0:
		rule += fmt.Sprintf(" LIMIT %d", pn.SystemTimeLimit)
	case pn.SystemTimeInterval != nil && pn.SystemTimeInterval.Value != "":
		rule += " INTERVAL " + pn.SystemTimeInterval.Value + " " + pn.SystemTimeInterval.Unit
	case pn.SystemTimeInterval != nil:
		rule += " INTERVAL … " + pn.SystemTimeInterval.Unit
	}
	if pn.SystemTimeAuto {
		rule += " AUTO"
	}
	history := 0
	for _, d := range pn.Definitions {
		if d.History {
			history++
		}
	}
	if history == 0 && pn.Count > 1 {
		history = pn.Count - 1 // PARTITIONS n: n-1 HISTORY + 1 CURRENT
	}
	source := fmt.Sprintf("%s (%d HISTORY partition(s) + CURRENT)", rule, history)
	if sv == nil {
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: "table." + s.Name,
			Source: source,
			Target: "(unpartitioned table)",
			Reason: "MariaDB history partitioning only stores the closed row versions of a system-versioned table apart from its current rows. This table's versioning is not emulated (see its table.system_versioning warning), so only the current rows are migrated, into an unpartitioned table.",
			Level:  "info",
		})
		return
	}
	t.res.Explanations = append(t.res.Explanations, Explanation{
		Object: "table." + s.Name,
		Source: source,
		Target: "unpartitioned " + pgQuote(s.Name) + " (current rows) + its history table (every closed version)",
		Reason: "MariaDB history partitioning changes where the versions are stored, not which versions exist: the CURRENT partition holds the current rows, the HISTORY partitions the closed versions, rotated by the " + rule + " rule. The system-versioning emulation makes the same split — current rows in " + s.Name + ", every closed version (copied through `FOR SYSTEM_TIME ALL`, whichever HISTORY partition holds it) in the history table — so no row is lost. The rotation itself is not reproduced: purging old history (MariaDB `ALTER TABLE … DROP PARTITION <history partition>` or `DELETE HISTORY … BEFORE SYSTEM_TIME`) becomes a `DELETE` on the history table filtered on " + sv.RowEnd + ".",
		Level:  "info",
	})
}

// isTimestampType reports whether dt is a MySQL/MariaDB TIMESTAMP.
func isTimestampType(dt ast.DataType) bool {
	_, ok := dt.(*ast.TimestampType)
	return ok
}

// isColumn reports whether name designates column col (MySQL/MariaDB
// column identifiers are case-insensitive).
func isColumn(name, col string) bool {
	return col != "" && strings.EqualFold(name, col)
}

// sysverNotEmulatedReason explains, for the table.system_versioning
// warning, why a system-versioned table is migrated without history.
func sysverNotEmulatedReason(reason string) string {
	switch reason {
	case sysverTrxPrecise:
		return "transaction-precise versioning (ROW START / ROW END are BIGINT UNSIGNED transaction ids resolved through mysql.transaction_registry) cannot be emulated in PostgreSQL; the period columns are dropped"
	case sysverNoSentinel:
		return "the source server's ROW END value for current rows could not be determined at inspection, so rows written in PostgreSQL could not be stamped like the copied ones; the period columns are dropped"
	case sysverSpatial:
		return "the table has a spatial column, which squishy's data copier cannot transfer, so the archived versions would lose its values; the period columns are dropped"
	}
	return "the SYSTEM_TIME period does not name a ROW START / ROW END column pair, and the implicit form's hidden row_start / row_end columns could not be declared because the table already has a column of that name"
}

// periodCheck appends MariaDB's implicit application-time period
// constraint `CONSTRAINT <period> CHECK (<start> < <end>)` to tbl.
func (t *translator) periodCheck(tbl *PGTable, tableName string, pd ast.PeriodDef) {
	if pd.StartCol == "" || pd.EndCol == "" {
		return
	}
	cond := &ast.BinaryExpr{
		Op:  "<",
		Lhs: &ast.Ident{Parts: []string{pd.StartCol}, Backtick: true},
		Rhs: &ast.Ident{Parts: []string{pd.EndCol}, Backtick: true},
	}
	tbl.Checks = append(tbl.Checks, rawExpr(cond))
	tbl.CheckNames = append(tbl.CheckNames, pd.Name)
}

// skipWithoutOverlapsKey reports a PRIMARY KEY / UNIQUE whose key parts
// include `<period> WITHOUT OVERLAPS` and returns true so the caller
// drops it: emitting it as a plain key would reference the period name
// as if it were a column, and would not forbid overlapping ranges.
func (t *translator) skipWithoutOverlapsKey(table, kind, name string, cols []ast.IndexedCol) bool {
	var period string
	for _, c := range cols {
		if c.WithoutOverlaps {
			period = c.Name
			break
		}
	}
	if period == "" {
		return false
	}
	label := kind
	if name != "" {
		label += " " + name
	}
	t.warn(table, "table.application_period",
		fmt.Sprintf("%s (… %s WITHOUT OVERLAPS) needs an EXCLUDE USING gist constraint over the period range; the key was not created", label, period))
	return true
}

// withoutColumn returns cols minus the key parts that name a dropped
// column or the ROW END column of an emulated system-versioned table.
// dropped is keyed by lower-cased column name: matching is
// case-insensitive, like MySQL/MariaDB column identifiers.
func withoutColumn(cols []ast.IndexedCol, dropped map[string]bool, sv *PGSystemVersioning) []ast.IndexedCol {
	out := make([]ast.IndexedCol, 0, len(cols))
	for _, c := range cols {
		if !c.IsExpr && (dropped[strings.ToLower(c.Name)] || (sv != nil && isColumn(c.Name, sv.RowEnd))) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// emitSystemVersioning materialises the emulation of every
// system-versioned table: the history table (appended to Plan.Tables so
// it is created with the schema) and the post-copy trigger DDL.
//
// Every name it derives from a table name (history table, trigger
// functions, triggers) is built by pgIdentWithSuffix, so it fits
// PostgreSQL's 63-byte identifier limit without two derived names of
// one table truncating to the same identifier. Each derived name is
// also kept clear of the names the rest of the plan creates in the same
// PostgreSQL namespace (sysverTakenNames), compared as PostgreSQL will
// store them: a clash gets a numbered suffix instead of a failing
// CREATE TABLE or a CREATE OR REPLACE FUNCTION that silently replaces a
// migrated routine.
func (t *translator) emitSystemVersioning() {
	taken := t.sysverTakenNames()
	n := len(t.res.Plan.Tables)
	for i := 0; i < n; i++ {
		cur := &t.res.Plan.Tables[i]
		sv := cur.SystemVersioning
		if sv == nil {
			continue
		}
		hist := claimIdent(cur.Name, "_history", taken.relations)
		sv.HistoryTable = hist
		trg := taken.triggersOn(cur.Name)
		names := sysverNames{
			stampFn:      claimIdent(cur.Name, "_sysver_stamp_fn", taken.functions),
			histFn:       claimIdent(cur.Name, "_sysver_history_fn", taken.functions),
			stampTrigger: claimIdent(cur.Name, "_sysver_stamp", trg),
			histTrigger:  claimIdent(cur.Name, "_sysver_history", trg),
		}

		h := PGTable{
			Schema:  cur.Schema,
			Name:    hist,
			Comment: "Closed row versions of " + cur.Name + " (MariaDB SYSTEM VERSIONING emulation).",
		}
		cols := make([]string, 0, len(cur.Columns))
		for _, c := range cur.Columns {
			// Plain storage for archived values: no identity, default,
			// generated expression or check — the row is a snapshot.
			h.Columns = append(h.Columns, PGColumn{
				Name:      c.Name,
				Type:      c.Type,
				NotNull:   c.NotNull,
				Collation: c.Collation,
			})
			cols = append(cols, c.Name)
		}
		if len(cur.PK) > 0 {
			// MariaDB's own key over all versions: (pk…, ROW END). The
			// current PK normally has no ROW END (translateTable and
			// translateAlterTable strip it); skip it anyway so the
			// history key can never list it twice.
			for _, c := range cur.PK {
				if !isColumn(c, sv.RowEnd) {
					h.PK = append(h.PK, c)
				}
			}
			h.PK = append(h.PK, sv.RowEnd)
		}
		t.res.Plan.Tables = append(t.res.Plan.Tables, h)
		// cur may have moved with the append.
		cur = &t.res.Plan.Tables[i]

		t.res.Plan.PostActions = append(t.res.Plan.PostActions,
			systemVersioningDDL(cur.Schema, cur.Name, hist, cols, sv, names))

		versioning := "one version per UPDATE"
		if len(sv.Unversioned) > 0 {
			versioning = "one version per UPDATE that assigns a versioned column — an UPDATE assigning only the WITHOUT SYSTEM VERSIONING column(s) " + strings.Join(sv.Unversioned, ", ") + " changes the current row in place, as in MariaDB (PG `UPDATE OF` trigger events)"
		}
		if def := pgIdentWithSuffix(cur.Name, "_history"); hist != def {
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: cur.Name,
				Source: "WITH SYSTEM VERSIONING",
				Target: "history table " + pgQuote(hist),
				Reason: "The history table is named " + hist + " because " + def + " is already the name of another table, view, sequence or index of the migrated schema (PostgreSQL relations share one namespace per schema).",
				Level:  "info",
			})
		}
		source := "WITH SYSTEM VERSIONING, PERIOD FOR SYSTEM_TIME (" + sv.RowStart + ", " + sv.RowEnd + ")"
		if sv.Implicit {
			source = "WITH SYSTEM VERSIONING (implicit: hidden " + sv.RowStart + " / " + sv.RowEnd + " columns)"
			t.res.Explanations = append(t.res.Explanations, Explanation{
				Object: cur.Name,
				Source: source,
				Target: "regular columns " + pgQuote(sv.RowStart) + ", " + pgQuote(sv.RowEnd) + " (TIMESTAMPTZ(6) NOT NULL)",
				Reason: "MariaDB keeps the ROW START / ROW END of an implicitly versioned table (`WITH SYSTEM VERSIONING` without PERIOD FOR SYSTEM_TIME, or `ALTER TABLE … ADD SYSTEM VERSIONING`) in hidden columns named " + sv.RowStart + " / " + sv.RowEnd + ", absent from `SELECT *`. PostgreSQL has no hidden columns: they become regular columns of " + cur.Name + " and " + hist + " (last, in that order) and appear in `SELECT *`. An INSERT without a column list that supplies only the other columns still works: the omitted trailing period columns are stamped by the versioning trigger.",
				Level:  "info",
			})
		}
		t.res.Explanations = append(t.res.Explanations, Explanation{
			Object: cur.Name,
			Source: source,
			Target: "current table + " + pgQuote(hist) + " history table + versioning triggers",
			Reason: "PostgreSQL has no native system versioning; it is emulated. The ROW START / ROW END columns are kept (TIMESTAMPTZ, NOT NULL) and copied with their source values (the source session is pinned to UTC); historical versions (`FOR SYSTEM_TIME ALL` rows whose ROW END is in the past) are copied into " + hist + ", keyed like MariaDB on the primary key plus " + sv.RowEnd + ". After the copy, a BEFORE trigger stamps " + sv.RowStart + " = statement_timestamp() and " + sv.RowEnd + " = '" + sv.RowEndMax + "+00' (the ROW END value the source server stores on current rows, probed at inspection), and an AFTER UPDATE / DELETE trigger archives the previous version into " + hist + " closed at the change time — the per-statement timestamps MariaDB uses, " + versioning + ". `SELECT … FOR SYSTEM_TIME AS OF / BETWEEN / ALL` and `DELETE HISTORY` are query syntax PostgreSQL lacks: application queries using them must read " + hist + " (UNION ALL " + cur.Name + ") with the equivalent " + sv.RowStart + " / " + sv.RowEnd + " predicates.",
			Level:  "info",
		})
	}
}

// sysverNames are the trigger and trigger-function names of one
// emulated system-versioned table.
type sysverNames struct {
	stampFn, histFn           string
	stampTrigger, histTrigger string
}

// sysverTaken holds, per PostgreSQL namespace, the lower-cased names
// (as PostgreSQL stores them, see pgClipIdent) the plan already uses.
type sysverTaken struct {
	// relations: tables, views, sequences and indexes of the target
	// schema — one pg_class namespace.
	relations map[string]bool
	// functions: functions, procedures and trigger functions.
	functions map[string]bool
	// triggers: trigger names per lower-cased table name (PostgreSQL
	// scopes trigger names per table).
	triggers map[string]map[string]bool
}

// triggersOn returns the trigger-name set of table, creating it.
func (s sysverTaken) triggersOn(table string) map[string]bool {
	k := strings.ToLower(pgClipIdent(table))
	m := s.triggers[k]
	if m == nil {
		m = make(map[string]bool)
		s.triggers[k] = m
	}
	return m
}

// sysverTakenNames collects the names the plan creates, from the plan's
// typed entries and the translator's sequence list — never from DDL
// text.
func (t *translator) sysverTakenNames() sysverTaken {
	s := sysverTaken{
		relations: make(map[string]bool),
		functions: make(map[string]bool),
		triggers:  make(map[string]map[string]bool),
	}
	add := func(set map[string]bool, name string) {
		if name != "" {
			set[strings.ToLower(pgClipIdent(name))] = true
		}
	}
	plan := &t.res.Plan
	for _, tbl := range plan.Tables {
		add(s.relations, tbl.Name)
	}
	for _, v := range plan.Views {
		add(s.relations, v.Name)
	}
	for _, name := range t.sequenceNames {
		add(s.relations, name)
	}
	// Index names as buildDDL will emit them: dedupeIndexNames is
	// idempotent, so running it here leaves buildDDL's pass a no-op.
	dedupeIndexNames(plan.Indexes)
	for _, idx := range plan.Indexes {
		add(s.relations, idx.Name)
	}
	for _, r := range plan.Routines {
		switch r.Kind {
		case "function", "procedure":
			add(s.functions, r.Name)
		case "trigger":
			add(s.functions, r.FnName)
			if r.Table != "" {
				add(s.triggersOn(r.Table), r.Name)
			}
		}
	}
	return s
}

// claimIdent derives base+suffix with pgIdentWithSuffix, numbering the
// suffix (suffix2, suffix3, …) until the name is not in taken, and
// records the name it returns in taken.
func claimIdent(base, suffix string, taken map[string]bool) string {
	name := pgIdentWithSuffix(base, suffix)
	for k := 2; taken[strings.ToLower(name)]; k++ {
		name = pgIdentWithSuffix(base, fmt.Sprintf("%s%d", suffix, k))
	}
	taken[strings.ToLower(name)] = true
	return name
}

// systemVersioningDDL renders the two trigger functions and triggers of
// the emulation, built from typed PL AST nodes.
//
// When some columns are declared WITHOUT SYSTEM VERSIONING, the UPDATE
// events are restricted to `UPDATE OF <versioned columns>`: MariaDB
// versions an UPDATE whose SET list names a versioned column (even
// `SET c = c`), and rewrites the row in place — no history row, ROW
// START unchanged — when it only names unversioned ones (checked on
// MariaDB 11.8). PG's `UPDATE OF` fires exactly when a listed column is
// an UPDATE target, which is the same rule.
func systemVersioningDDL(schema, table, hist string, cols []string, sv *PGSystemVersioning, names sysverNames) string {
	newCol := func(c string) string { return "NEW." + pgQuote(c) }
	oldRef := func(c string) ast.Expr { return &ast.Ident{Parts: []string{"OLD", c}} }
	newRef := func(c string) ast.Expr { return &ast.Ident{Parts: []string{"NEW", c}} }
	stmtTS := func() ast.Expr { return &ast.FuncCall{Name: "statement_timestamp"} }

	// BEFORE INSERT OR UPDATE: stamp the new current version.
	stamp := &ast.Block{Stmts: []ast.PLStmt{
		&ast.AssignStmt{Target: newCol(sv.RowStart), Expr: stmtTS()},
		&ast.AssignStmt{Target: newCol(sv.RowEnd), Expr: &ast.CastExpr{
			Expr: &ast.Literal{Kind: "string", Text: sv.RowEndMax + "+00"},
			Type: &ast.PGType{Name: "timestamptz"},
		}},
		&ast.ReturnStmt{Expr: &ast.Ident{Parts: []string{"NEW"}}},
	}}

	// AFTER UPDATE OR DELETE: archive OLD, closed at closeAt.
	archive := func(closeAt ast.Expr) ast.PLStmt {
		vals := make([]ast.Expr, 0, len(cols))
		for _, c := range cols {
			if isColumn(c, sv.RowEnd) {
				vals = append(vals, closeAt)
				continue
			}
			vals = append(vals, oldRef(c))
		}
		return &ast.IfStmt{Branches: []ast.IfBranch{{
			Cond: &ast.BinaryExpr{Op: "<", Lhs: oldRef(sv.RowStart), Rhs: closeAt},
			Body: []ast.PLStmt{&ast.InsertStmt{
				Table:  ast.TableRef{Schema: schema, Name: hist},
				Cols:   cols,
				Values: [][]ast.Expr{vals},
			}},
		}}}
	}
	history := &ast.Block{Stmts: []ast.PLStmt{
		&ast.IfStmt{
			Branches: []ast.IfBranch{{
				Cond: &ast.BinaryExpr{
					Op:  "=",
					Lhs: &ast.Ident{Parts: []string{"TG_OP"}},
					Rhs: &ast.Literal{Kind: "string", Text: "UPDATE"},
				},
				Body: []ast.PLStmt{archive(newRef(sv.RowStart))},
			}},
			Else: []ast.PLStmt{archive(stmtTS())},
		},
		&ast.ReturnStmt{Expr: &ast.Literal{Kind: "null", Text: "NULL"}},
	}}

	// Trigger events. updateOf == nil means every UPDATE versions.
	var updateOf []string
	withUpdate := true
	if len(sv.Unversioned) > 0 {
		for _, c := range cols {
			if isColumn(c, sv.RowStart) || isColumn(c, sv.RowEnd) {
				continue // GENERATED ALWAYS: never an UPDATE target
			}
			unversioned := false
			for _, u := range sv.Unversioned {
				if isColumn(c, u) {
					unversioned = true
					break
				}
			}
			if !unversioned {
				updateOf = append(updateOf, c)
			}
		}
		// Every column unversioned: no UPDATE ever creates a version.
		withUpdate = len(updateOf) > 0
	}
	stampEvents := []string{"INSERT"}
	histEvents := []string{"DELETE"}
	if withUpdate {
		stampEvents = []string{"INSERT", "UPDATE"}
		histEvents = []string{"UPDATE", "DELETE"}
	}

	stampFn, histFn := names.stampFn, names.histFn
	return pgast.Write([]pgast.Stmt{
		&pgast.CreateFunction{Schema: schema, Name: stampFn, Returns: "trigger", Body: pgast.WriteBlock(stamp)},
		&pgast.CreateTrigger{
			Name: names.stampTrigger, Timing: "BEFORE",
			Events: stampEvents, UpdateOf: updateOf,
			Schema: schema, Table: table, FnName: stampFn, ForEach: "ROW",
		},
		&pgast.CreateFunction{Schema: schema, Name: histFn, Returns: "trigger", Body: pgast.WriteBlock(history)},
		&pgast.CreateTrigger{
			Name: names.histTrigger, Timing: "AFTER",
			Events: histEvents, UpdateOf: updateOf,
			Schema: schema, Table: table, FnName: histFn, ForEach: "ROW",
		},
	})
}

// pgMaxIdentLen is PostgreSQL's identifier length limit in bytes
// (NAMEDATALEN - 1); longer names are silently truncated.
const pgMaxIdentLen = 63

// pgClipIdent returns name as PostgreSQL stores it: cut to at most
// pgMaxIdentLen bytes, on a UTF-8 character boundary (PG's
// pg_mbcliplen). name is a single identifier, never SQL text.
func pgClipIdent(name string) string {
	if len(name) <= pgMaxIdentLen {
		return name
	}
	return clipUTF8(name, pgMaxIdentLen)
}

// clipUTF8 returns the longest prefix of s of at most n bytes that ends
// on a character boundary.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// pgIdentWithSuffix derives the identifier base+suffix (base a table
// name, suffix an ASCII marker such as "_history") within PostgreSQL's
// 63-byte limit. When base+suffix fits it is returned unchanged.
// Otherwise base is cut and followed by "_" and 8 hex digits of the FNV-1a
// hash of the full base, then the suffix is kept whole: two suffixes of
// one table never truncate to the same name, and two long table names
// sharing their first bytes still get distinct derived names.
func pgIdentWithSuffix(base, suffix string) string {
	if len(base)+len(suffix) <= pgMaxIdentLen {
		return base + suffix
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(base))
	tag := fmt.Sprintf("_%08x", h.Sum32())
	keep := pgMaxIdentLen - len(suffix) - len(tag)
	if keep < 0 {
		keep = 0
	}
	return clipUTF8(base, keep) + tag + suffix
}
