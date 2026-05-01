// Package translate transforms a MySQL AST into a PostgreSQL SchemaPlan + a
// set of human-readable explanations that drive the wizard's "transformation
// d'architecture" and "transformation données" screens.
package translate

import (
	"encoding/json"
)

// SchemaPlan is the serializable output of the translator, persisted in the
// migrations.target_plan JSONB column and emitted to the UI.
type SchemaPlan struct {
	Tables           []PGTable      `json:"tables"`
	Indexes          []PGIndex      `json:"indexes"`
	ForeignKeys      []PGForeignKey `json:"foreign_keys"`
	Routines         []PGRoutine    `json:"routines"`
	Views            []PGView       `json:"views"`
	Events           []PGEvent      `json:"events"`
	PreActions       []string       `json:"pre_actions"`  // executed before CREATE TABLE (e.g. CREATE SEQUENCE)
	PostActions      []string       `json:"post_actions"` // executed after COPY (triggers, identity resets, …)
	TargetExtensions []string       `json:"target_extensions"` // extensions detected on target at plan time
}

type PGTable struct {
	Schema  string     `json:"schema"`
	Name    string     `json:"name"`
	Columns []PGColumn `json:"columns"`
	PK      []string   `json:"pk,omitempty"`
	Checks  []string   `json:"checks,omitempty"`
	Comment string     `json:"comment,omitempty"`
	// Partitioning carries the declarative partitioning lifted from the
	// source DDL (Oracle PARTITION BY RANGE/LIST/HASH). When set, buildDDL
	// emits a partitioned-table parent + one CREATE TABLE … PARTITION OF
	// … FOR VALUES FROM (…) TO (…) per child.
	Partitioning *PGPartitioning `json:"partitioning,omitempty"`
}

// PGPartitioning is the squishy-side description of a PG declarative
// partitioning clause. `Partitions` carries the children in source-DDL order;
// for RANGE the bound chaining (lower = previous upper) is computed at
// emission time so this struct only needs to remember the upper bound of
// each partition (the form Oracle naturally provides via VALUES LESS THAN).
type PGPartitioning struct {
	Method     string         `json:"method"`  // "RANGE" | "LIST" | "HASH"
	Columns    []string       `json:"columns"` // partition key cols (already PG-cased)
	Partitions []PGPartition  `json:"partitions,omitempty"`
}

type PGPartition struct {
	Name string `json:"name"`
	// UpperBound is the PG expression for `VALUES LESS THAN (X)` translated
	// to PG (e.g. `'2019-04-01'`). Empty means MAXVALUE (unbounded).
	// Used only for RANGE partitioning.
	UpperBound string `json:"upper_bound,omitempty"`
	// Values carries the PG-rendered list of literals for LIST
	// partitioning (`FOR VALUES IN (v1, v2, …)`). Used only for LIST.
	// A `DEFAULT` partition is encoded as Values=["DEFAULT"]; emission
	// switches to `FOR VALUES IN (DEFAULT)` then.
	Values []string `json:"values,omitempty"`
	// IsDefault marks the catch-all LIST partition (`PARTITION P_DEF
	// VALUES (DEFAULT)`), mapped to PG's `FOR VALUES IN (DEFAULT)`.
	IsDefault bool `json:"is_default,omitempty"`
	// Modulus / Remainder describe a HASH partition. squishy assigns
	// remainders 0..N-1 in source-DDL order (Oracle doesn't expose them
	// explicitly — `PARTITIONS N` just spawns N children).
	Modulus   int `json:"modulus,omitempty"`
	Remainder int `json:"remainder,omitempty"`
}

type PGColumn struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	NotNull   bool            `json:"not_null,omitempty"`
	Default   string          `json:"default,omitempty"`
	Identity  bool            `json:"identity,omitempty"`
	Generated *PGGenerated    `json:"generated,omitempty"`
	Comment   string          `json:"comment,omitempty"`
	Check     string          `json:"check,omitempty"`
	// Collation is the PG collation identifier (already quoted) to attach
	// to this column. Empty when the source had no explicit collation, or
	// when the source collation has no safe PG equivalent and was instead
	// surfaced as an informational explanation.
	Collation string          `json:"collation,omitempty"`
	Extras    json.RawMessage `json:"extras,omitempty"` // future-proof
}

type PGGenerated struct {
	Expr   string `json:"expr"`
	Stored bool   `json:"stored"`
}

type PGIndex struct {
	Schema  string   `json:"schema"`
	Table   string   `json:"table"`
	Name    string   `json:"name"`
	Unique  bool     `json:"unique,omitempty"`
	Columns []string `json:"columns"`
	Using   string   `json:"using,omitempty"`

	// ColumnDirs is parallel to Columns: per-column "ASC"|"DESC"|"" sort
	// direction. Empty entries are written without an explicit direction
	// (PG defaults to ASC). Empty slice means "no per-column direction
	// captured" which keeps the existing JSON output stable.
	ColumnDirs []string `json:"column_dirs,omitempty"`

	// ColumnIsExpr is parallel to Columns: when true at index i, Columns[i]
	// is a raw SQL expression (e.g. `LOWER("title")`) and the writer emits
	// it inside an extra paren pair as PG requires for expression indexes.
	ColumnIsExpr []bool `json:"column_is_expr,omitempty"`
}

type PGForeignKey struct {
	Schema     string   `json:"schema"`
	Table      string   `json:"table"`
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RefSchema  string   `json:"ref_schema"`
	RefTable   string   `json:"ref_table"`
	RefColumns []string `json:"ref_columns"`
	OnDelete   string   `json:"on_delete,omitempty"`
	OnUpdate   string   `json:"on_update,omitempty"`
	// NotValid emits PG's `NOT VALID` so existing rows aren't checked at
	// constraint-creation time. Maps from Oracle NOVALIDATE.
	NotValid bool `json:"not_valid,omitempty"`
	// Deferrable / InitiallyDeferred mirror PG's `DEFERRABLE [INITIALLY
	// DEFERRED]` clause. Maps from Oracle's DEFERRABLE [INITIALLY
	// DEFERRED|IMMEDIATE].
	Deferrable        bool `json:"deferrable,omitempty"`
	InitiallyDeferred bool `json:"initially_deferred,omitempty"`
}

type PGRoutine struct {
	Kind      string `json:"kind"` // procedure|function|trigger
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Returns   string `json:"returns,omitempty"`
	Volatile  string `json:"volatile,omitempty"` // VOLATILE|STABLE|IMMUTABLE
	Security  string `json:"security,omitempty"` // DEFINER|INVOKER
	RawBody   string `json:"raw_body"`           // MySQL body, preserved in comment
	DDL       string `json:"ddl"`                // PG skeleton DDL
}

type PGView struct {
	Schema      string `json:"schema"`
	Name        string `json:"name"`
	SelectBody  string `json:"select_body"`
	CheckOption string `json:"check_option,omitempty"`
	Security    string `json:"security,omitempty"`
	DDL         string `json:"ddl"`
}

type PGEvent struct {
	Name    string `json:"name"`
	PgCron  string `json:"pg_cron"` // snippet suggestion
	Comment string `json:"comment,omitempty"`
}

// Explanation is a human-readable bullet shown in the wizard UI.
type Explanation struct {
	Object string `json:"object"` // e.g. "orders.status"
	Source string `json:"source"` // "ENUM('pending',...)"
	Target string `json:"target"` // "TEXT + CHECK (status IN (...))"
	Reason string `json:"reason"` // narrative
	Level  string `json:"level"`  // info|warn
}

// Warning flags something that needs attention (body to translate, spatial types…).
// Severity mirrors Prerequisite.Severity semantics:
//   - "blocking" — the run should not start until the user acts
//   - "info"     — intrinsic target-engine limitations (BFILE/ROWID/UROWID
//                  have no PG counterpart, FOLLOWS/PRECEDES isn't a PG feature,
//                  …). Surfaced so the user is aware, never gate the run.
// Empty value means unset (historical default; callers treat it as blocking).
type Warning struct {
	Object   string `json:"object"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Severity string `json:"severity,omitempty"`
}

// TypeMapping records one row of the MySQL → PG mapping table applied to
// concrete columns — shown verbatim in the wizard so the user can audit rules.
type TypeMapping struct {
	Object  string `json:"object"`
	MySQL   string `json:"mysql"`
	PG      string `json:"pg"`
	Note    string `json:"note,omitempty"`
}

// Severity classifies a prerequisite.
//   - SeverityBlocking: the user MUST act (install extension / translate body
//     / fix parse error) before launching the run. The API refuses to start
//     a run until every blocking prereq is acknowledged.
//   - SeverityInfo: purely informational — documented mapping choices the
//     user might want to review but that don't block the migration.
type Severity string

const (
	SeverityBlocking Severity = "blocking"
	SeverityInfo     Severity = "info"
)

// Category groups prereqs by remediation type in the UI.
type Category string

const (
	CatInstallExtension Category = "install_extension"
	CatManualSQL        Category = "manual_sql"
	CatManualReview     Category = "manual_review"
	CatFixSource        Category = "fix_source"
)

// Prerequisite is a structured action the user must take (or explicitly ack)
// before the migration runs. Emitted by the translator and surfaced as a
// checklist between wizard steps "data transformation" and "summary".
type Prerequisite struct {
	ID          string   `json:"id"`            // stable hash, used as ack key
	Severity    Severity `json:"severity"`
	Category    Category `json:"category"`
	Object      string   `json:"object,omitempty"` // e.g. "t_spatial.c_geom"
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Remediation string   `json:"remediation"`  // multi-line: install command / SQL snippet / review steps
}
