// Package postgres hosts a narrow PostgreSQL AST + writer used by the
// translator to emit target DDL. The AST mirrors the shape of the productions
// defined in reference/PostgreSQLParser.g4 for the subset squishy generates
// during a migration (schemas, sequences, tables, indexes, FKs, comments,
// triggers, functions, views).
//
// This package intentionally does NOT implement a full PG parser. grammars-v4
// is the canonical spec; we follow it but restrict ourselves to what the
// translator emits. A future version can expand coverage without changing
// this file's shape.
package postgres

// Node is the minimal marker for all PG AST nodes.
type Node interface {
	pgNode()
}

// Stmt is a top-level PG statement.
type Stmt interface {
	Node
	stmtNode()
}

// ---------------------------------------------------------------------------
// Schema / sequences
// ---------------------------------------------------------------------------

type CreateSchema struct {
	Name        string
	IfNotExists bool
}

func (*CreateSchema) pgNode()   {}
func (*CreateSchema) stmtNode() {}

// SetSearchPath renders a `SET search_path TO <first>, <rest...>` statement.
// `Schema` is the primary schema; `Additional` are appended after it (used
// for extension-provided types like public.geometry from PostGIS).
type SetSearchPath struct {
	Schema     string
	Additional []string
}

func (*SetSearchPath) pgNode()   {}
func (*SetSearchPath) stmtNode() {}

type CreateSequence struct {
	Schema      string
	Name        string
	IfNotExists bool
	As          string // "bigint" | "integer" | ""
}

func (*CreateSequence) pgNode()   {}
func (*CreateSequence) stmtNode() {}

// SelectSetval — emitted as a post-copy action to align a sequence with
// copied data: SELECT setval('<seq>', COALESCE((SELECT max(col) FROM tbl),1));
type SelectSetval struct {
	Schema    string
	SeqName   string
	TableName string
	Column    string
}

func (*SelectSetval) pgNode()   {}
func (*SelectSetval) stmtNode() {}

// ---------------------------------------------------------------------------
// Tables and columns
// ---------------------------------------------------------------------------

type CreateTable struct {
	Schema      string
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	PrimaryKey  []string // PK column names (inline constraint)
	Checks      []string // raw CHECK expressions (table-level)
	// PartitionBy, when non-nil, turns the emission into a partitioned-table
	// parent (PG declarative partitioning). Children are emitted via a
	// separate `CreatePartition` statement.
	PartitionBy *PartitionSpec
}

// PartitionSpec describes a PG declarative partitioning clause:
//
//	PARTITION BY RANGE (col1, col2)
//	PARTITION BY LIST  (col)
//	PARTITION BY HASH  (col)
type PartitionSpec struct {
	Method  string // "RANGE" | "LIST" | "HASH" (uppercase)
	Columns []string
}

// CreatePartition emits a PG child partition. The shape depends on the
// parent's partitioning method:
//
//	RANGE: CREATE TABLE … PARTITION OF … FOR VALUES FROM (From) TO (To);
//	LIST:  CREATE TABLE … PARTITION OF … FOR VALUES IN (Values…);
//	       (with Values=["DEFAULT"] → FOR VALUES IN (DEFAULT) — bare keyword)
//	HASH:  CREATE TABLE … PARTITION OF …
//	          FOR VALUES WITH (MODULUS m, REMAINDER r);
//
// Set `Method` to "RANGE" / "LIST" / "HASH" so the writer picks the right
// shape. For RANGE, empty From → MINVALUE, empty To → MAXVALUE. For LIST,
// `IsDefault=true` overrides Values and emits `(DEFAULT)`.
type CreatePartition struct {
	Schema      string
	Name        string
	ParentTable string
	Method      string
	// RANGE
	From string
	To   string
	// LIST
	Values    []string
	IsDefault bool
	// HASH
	Modulus   int
	Remainder int
}

func (*CreatePartition) pgNode()   {}
func (*CreatePartition) stmtNode() {}

func (*CreateTable) pgNode()   {}
func (*CreateTable) stmtNode() {}

// ColumnDef — subset of the PG column grammar relevant to squishy.
type ColumnDef struct {
	Name      string
	Type      string        // rendered PG type text (e.g. "BIGINT", "NUMERIC(20,0)", "TIMESTAMPTZ(3)")
	NotNull   bool
	Default   string        // raw PG expression text; empty = no default
	Identity  IdentityMode  // "" | "ALWAYS" | "BY DEFAULT"
	Check     string        // raw column-level CHECK expression
	Generated *GeneratedCol // STORED generated column

	// Collation is the PG collation identifier to attach to the column
	// (e.g. `"C"`, `"und-x-icu"`). Empty = no explicit COLLATE clause.
	// Already quoted exactly as it should appear in DDL.
	Collation string
}

type IdentityMode string

const (
	IdentityNone      IdentityMode = ""
	IdentityAlways    IdentityMode = "ALWAYS"
	IdentityByDefault IdentityMode = "BY DEFAULT"
)

type GeneratedCol struct {
	Expr string // raw expression text
}

// ---------------------------------------------------------------------------
// Indexes / foreign keys / comments
// ---------------------------------------------------------------------------

type CreateIndex struct {
	Schema  string
	Table   string
	Name    string
	Unique  bool
	Columns []string
	Method  string // "btree" | "hash" | "gin" | ""

	// ColumnDirs is parallel to Columns: "ASC"|"DESC"|"" per column.
	ColumnDirs []string

	// ColumnIsExpr is parallel to Columns: when true at index i,
	// Columns[i] is a raw expression (no qIdent quoting; wrapped in extra
	// parens by the writer).
	ColumnIsExpr []bool
}

func (*CreateIndex) pgNode()   {}
func (*CreateIndex) stmtNode() {}

type AlterTableAddFK struct {
	Schema     string
	Table      string
	Name       string
	Columns    []string
	RefSchema  string
	RefTable   string
	RefColumns []string
	OnDelete   string // "RESTRICT" | "CASCADE" | "SET NULL" | ""
	OnUpdate   string
	NotValid   bool // append NOT VALID (to be followed by ValidateFK)
	// Deferrable / InitiallyDeferred emit PG's `DEFERRABLE [INITIALLY
	// DEFERRED]` clause for FK constraints lifted from Oracle.
	Deferrable        bool
	InitiallyDeferred bool
}

func (*AlterTableAddFK) pgNode()   {}
func (*AlterTableAddFK) stmtNode() {}

type AlterTableValidateFK struct {
	Schema string
	Table  string
	Name   string
}

func (*AlterTableValidateFK) pgNode()   {}
func (*AlterTableValidateFK) stmtNode() {}

type CommentOn struct {
	Object string // "TABLE" | "COLUMN" | …
	Target string // rendered qualified name (already quoted)
	Body   string // raw (unquoted) comment text; writer will SQL-escape
}

func (*CommentOn) pgNode()   {}
func (*CommentOn) stmtNode() {}

// ---------------------------------------------------------------------------
// Functions / triggers (stubs emitted during migration)
// ---------------------------------------------------------------------------

type CreateFunction struct {
	Schema   string
	Name     string
	Params   string // rendered, e.g. `IN "a" INTEGER, OUT "b" INTEGER`
	Returns  string // e.g. "trigger", "BIGINT"
	Language string // default "plpgsql"
	Volatile string // "VOLATILE" | "STABLE" | "IMMUTABLE" | ""
	Security string // "DEFINER" | "INVOKER" | ""
	Body     string // PL/pgSQL body including BEGIN/END
}

func (*CreateFunction) pgNode()   {}
func (*CreateFunction) stmtNode() {}

type CreateTrigger struct {
	Name    string
	Timing  string // "BEFORE" | "AFTER" | "INSTEAD OF"
	Event   string // "INSERT" | "UPDATE" | "DELETE"
	Schema  string
	Table   string
	FnName  string // referenced function (in same schema)
	ForEach string // "ROW" | "STATEMENT" — default "ROW"
	// WhenCond is the optional `WHEN (cond)` clause text — PG accepts the
	// same expression syntax inline on CREATE TRIGGER. Empty means no
	// WHEN clause.
	WhenCond string
}

func (*CreateTrigger) pgNode()   {}
func (*CreateTrigger) stmtNode() {}

// CreateView renders `CREATE OR REPLACE VIEW <schema>.<name> AS <body>
// [WITH <opt> CHECK OPTION]`. The body is raw SELECT text — in v1 we do not
// re-parse the SELECT since the source engine (MySQL) has its own syntax that
// requires manual review before the view becomes valid PG SQL.
type CreateView struct {
	Schema      string
	Name        string
	Columns     []string // optional explicit column list
	Body        string   // raw SELECT text (unquoted, writer emits verbatim)
	CheckOption string   // ""|"CHECK OPTION"|"CASCADED CHECK OPTION"|"LOCAL CHECK OPTION"
	Security    string   // ""|"DEFINER"|"INVOKER" — rendered as a trailing comment
}

func (*CreateView) pgNode()   {}
func (*CreateView) stmtNode() {}

// CreateProcedure renders `CREATE OR REPLACE PROCEDURE <schema>.<name>(<params>)
// LANGUAGE <lang> [<security>] AS $body$ <body> $body$;`.
type CreateProcedure struct {
	Schema   string
	Name     string
	Params   string // e.g. `IN "a" INTEGER, INOUT "b" INTEGER`
	Language string // default "plpgsql"
	Security string // ""|"DEFINER"|"INVOKER"
	Body     string // PL/pgSQL body including BEGIN/END
}

func (*CreateProcedure) pgNode()   {}
func (*CreateProcedure) stmtNode() {}

// Raw is an escape hatch for statements we don't fully model yet. The writer
// emits the Text verbatim with a trailing newline; use sparingly.
type Raw struct {
	Text string
}

func (*Raw) pgNode()   {}
func (*Raw) stmtNode() {}
