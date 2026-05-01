package ast

// CreateTable — CREATE TABLE statement.
type CreateTable struct {
	Schema       string // may be empty
	Name         string
	IfNotExists  bool
	Temporary    bool
	NameBacktick bool

	// OrReplace is true when the source uses MariaDB's
	// `CREATE OR REPLACE TABLE`. PG has no such syntax — the translator
	// emits a `DROP TABLE IF EXISTS` pre-action ahead of CREATE.
	OrReplace bool

	Columns     []*ColumnDef
	Constraints []TableConstraint
	Indexes     []*IndexDef
	Options     TableOptions

	// Partitioning carries a structured representation of the source-side
	// PARTITION BY clause (MySQL/MariaDB grammar). Nil when the table is
	// unpartitioned. Oracle keeps its raw form in Options.OraclePartitioning
	// because DBMS_METADATA's syntax is captured wholesale.
	Partitioning *Partitioning

	// SystemVersioned is true when the source carries `WITH SYSTEM VERSIONING`
	// (MariaDB 10.3+). PG has no native system-versioning, so the translator
	// surfaces this as a blocking prerequisite for manual remediation.
	SystemVersioned bool

	// Periods captures `PERIOD FOR <name> (start_col, end_col)` declarations
	// (MariaDB application-time and system-time periods). When name is
	// "SYSTEM_TIME" the period is system-versioning metadata; otherwise it is
	// an application-time period. PG has no native PERIOD FOR clause.
	Periods []PeriodDef

	P Position
}

// PeriodDef captures a MariaDB `PERIOD FOR <name> (start_col, end_col)` clause
// attached to a table.
type PeriodDef struct {
	Name     string // "SYSTEM_TIME" for system-versioning, otherwise an application name
	StartCol string
	EndCol   string
}

func (s *CreateTable) Pos() Position { return s.P }
func (s *CreateTable) stmtNode()     {}

type TableOptions struct {
	Engine         string
	Charset        string
	Collate        string
	AutoIncrement  int64
	HasAutoInc     bool
	RowFormat      string
	KeyBlockSize   int
	Comment        string
	Extras         []string // preserved raw "K=V" pairs for unknown options

	// SystemVersioning is true when the table-options clause contained
	// `WITH SYSTEM VERSIONING` (MariaDB 10.3+). Folded into
	// CreateTable.SystemVersioned by the parser caller for consumer
	// convenience.
	SystemVersioning bool

	// Oracle-specific, preserved raw for round-trip / diagnostics.
	OracleTablespace   string
	OracleStorage      string // full STORAGE(...) clause
	OraclePartitioning string // full PARTITION BY ... clause
	OracleOrganization string // HEAP|INDEX|EXTERNAL
	OracleLob          string // LOB (col) STORE AS ...
	// Oracle 21c+/23c table-level qualifiers that appear before the table
	// keyword (`CREATE IMMUTABLE BLOCKCHAIN TABLE …`, `CREATE SHARDED TABLE
	// …`, `CREATE PRIVATE TEMPORARY TABLE …`). None translate directly —
	// the table is emitted as a regular PG heap and the translator surfaces
	// the per-qualifier semantic loss via an explanation.
	OracleImmutable     bool
	OracleBlockchain    bool
	OracleSharded       bool
	OracleDuplicated    bool
	OraclePrivateTemp   bool

	// DB2-specific, preserved raw for round-trip / diagnostics. None of these
	// translate to PG: the translator strips them and emits an info-level
	// warning describing the storage / compression / partition decision lost.
	DB2Tablespace        string   // IN <tablespace>
	DB2IndexInTablespace string   // INDEX IN <tablespace>
	DB2LongInTablespace  string   // LONG IN <tablespace>
	DB2CompressionYES    bool     // COMPRESS YES [STATIC|ADAPTIVE]
	DB2CompressionMethod string   // STATIC|ADAPTIVE|""
	DB2OrganizeBy        string   // ORGANIZE BY KEY SEQUENCE | ROW | COLUMN
	DB2PartitionByRange  string   // raw `PARTITION BY RANGE (...) (PARTITION ...)` clause
	DB2DataCapture       string   // CHANGES|NONE|""
	DB2DistributeBy      string   // DISTRIBUTE BY HASH (...) | REPLICATION
	DB2VolatileCardinality bool   // VOLATILE [CARDINALITY]
	DB2NotLoggedInitially bool    // NOT LOGGED INITIALLY
	DB2InRow              bool    // VALUE COMPRESSION (LUW) — folds into CompressionYES
}

type ColumnDef struct {
	Name         string
	NameBacktick bool
	Type         DataType
	NotNull      bool
	HasNullable  bool // true if NULL/NOT NULL was explicit
	Default      Expr // may be nil
	HasDefault   bool
	// DefaultOnNull is Oracle's `DEFAULT ON NULL <expr>` modifier: the
	// default also fires for explicit INSERT … VALUES (…, NULL, …). PG's
	// plain DEFAULT only fires when the column is omitted, so the
	// translator has to emit a BEFORE INSERT/UPDATE trigger to coalesce
	// NULLs to the default value.
	DefaultOnNull bool
	OnUpdate     Expr
	AutoInc      bool
	Unique       bool
	PrimaryKey   bool
	Comment      string
	Charset      string
	Collation    string
	Generated    *Generated
	Check        Expr // column-level CHECK
	Invisible    bool
	// Compressed is true when the source carries `COMPRESSED [=method]` on a
	// TEXT/BLOB column (MariaDB transparent per-column compression). PostgreSQL
	// applies TOAST compression automatically, so the flag is informational.
	Compressed   bool
	// MariaDB SYSTEM VERSIONING period columns
	// (`GENERATED ALWAYS AS ROW START|END`). No PG equivalent — these are
	// dropped by the translator.
	SystemVersioning bool
	P                Position
}

func (c *ColumnDef) Pos() Position { return c.P }

type Generated struct {
	Expr       Expr
	Virtual    bool // else STORED
	HasVirtual bool
}

// ConstraintState captures the trailing Oracle constraint-state clauses
// (`ENABLE/DISABLE`, `VALIDATE/NOVALIDATE`, `RELY/NORELY`, `DEFERRABLE
// [INITIALLY DEFERRED|IMMEDIATE]`). Defaults match Oracle's defaults:
// constraints are enabled and validated unless explicitly disabled.
//
//   - Disabled = DISABLE  (Oracle creates the constraint metadata but
//     doesn't enforce it; PG has no equivalent and the translator drops
//     the constraint entirely with an info explanation).
//   - NoValidate = NOVALIDATE (existing rows are not checked; new rows
//     are. Maps to PG `NOT VALID` for FK and CHECK; PK/UQ can't be NOT
//     VALID in PG so the translator warns).
//   - Rely = RELY (cost-based optimizer hint, no PG analogue — surfaced
//     as an info note).
//   - Deferrable / InitiallyDeferred mirror PG's `DEFERRABLE [INITIALLY
//     DEFERRED]` directly.
type ConstraintState struct {
	Disabled          bool
	NoValidate        bool
	Rely              bool
	Deferrable        bool
	InitiallyDeferred bool
	// HasUsingIndex records whether the source DDL carried a `USING INDEX
	// [<spec>]` clause. PG always synthesises the index implicitly for
	// PK/UQ, so the Oracle index name / tablespace / storage attributes
	// are dropped — surface as an info note.
	HasUsingIndex bool
}

type PKConstraint struct {
	Name    string // may be empty
	Columns []IndexedCol
	State   ConstraintState
	P       Position
}

func (c *PKConstraint) Pos() Position         { return c.P }
func (c *PKConstraint) tableConstraintNode()  {}

type UQConstraint struct {
	Name    string
	Columns []IndexedCol
	State   ConstraintState
	P       Position
}

func (c *UQConstraint) Pos() Position        { return c.P }
func (c *UQConstraint) tableConstraintNode() {}

type FKConstraint struct {
	Name       string
	Columns    []string
	RefSchema  string
	RefTable   string
	RefColumns []string
	OnDelete   string // RESTRICT|CASCADE|SET NULL|NO ACTION|SET DEFAULT|"" (unspecified)
	OnUpdate   string
	State      ConstraintState
	P          Position
}

func (c *FKConstraint) Pos() Position        { return c.P }
func (c *FKConstraint) tableConstraintNode() {}

type CheckConstraint struct {
	Name     string
	Expr     Expr
	Enforced bool
	State    ConstraintState
	P        Position
}

func (c *CheckConstraint) Pos() Position        { return c.P }
func (c *CheckConstraint) tableConstraintNode() {}

type IndexedCol struct {
	Name      string
	PrefixLen int    // 0 if none
	Order     string // "ASC"|"DESC"|""

	// Expr captures the raw source text of a functional/expression key
	// like `(LOWER(col))` or `(col1 + col2)`. When non-empty, Name is
	// empty and IsExpr is true; the translator emits a PG expression
	// index `((expr))`.
	Expr   string
	IsExpr bool
}

// Partitioning is the MySQL/MariaDB PARTITION BY clause attached to a
// CREATE TABLE. It mirrors the grammar in MySqlParser.g4 closely enough that
// the translator can lift the supported subsets to PG declarative partitioning
// and warn on the rest.
type Partitioning struct {
	// Method is one of: "HASH", "KEY", "RANGE", "LIST", "RANGE COLUMNS",
	// "LIST COLUMNS". Always uppercase.
	Method string

	// Linear is true for `PARTITION BY LINEAR HASH/KEY (...)`. Has no PG
	// equivalent — informational only.
	Linear bool

	// KeyAlgorithm captures `KEY ALGORITHM = {1|2}` (0 if unset). Informational.
	KeyAlgorithm int

	// ExprText is the raw source text of the partition function for HASH or
	// non-COLUMNS RANGE/LIST (e.g. "id" or "YEAR(d)"). Empty for KEY/COLUMNS
	// variants which expose Columns instead.
	ExprText string

	// Columns is the column list for KEY, RANGE COLUMNS, and LIST COLUMNS.
	Columns []string

	// Count is the `PARTITIONS N` count when supplied without an explicit
	// definition list (mostly HASH/KEY shorthand). 0 means unspecified.
	Count int

	// Subpartition, when non-nil, captures the SUBPARTITION BY clause. PG
	// has no native subpartitioning — informational/warning-only.
	Subpartition *Subpartitioning

	// Definitions lists the PARTITION definitions in source order.
	Definitions []PartitionDefinition
}

// Subpartitioning mirrors the SUBPARTITION BY clause (always HASH or KEY in
// MySQL).
type Subpartitioning struct {
	Method       string // "HASH"|"KEY"
	Linear       bool
	KeyAlgorithm int
	ExprText     string
	Columns      []string
	Count        int // SUBPARTITIONS N
}

// PartitionDefinition captures one PARTITION clause inside the definition list.
type PartitionDefinition struct {
	Name string

	// HasLessThan is true for `VALUES LESS THAN (...)`. LessThan stores the
	// raw atom texts (one per partition column when COLUMNS form is used).
	// "MAXVALUE" is preserved as a literal token.
	HasLessThan bool
	LessThan    []string

	// HasIn is true for `VALUES IN (...)`. Mutually exclusive with HasLessThan.
	// InAtoms holds simple atom values; InVectors holds tuple values for
	// LIST COLUMNS multi-col form. Only one of them is populated.
	HasIn     bool
	InAtoms   []string
	InVectors [][]string

	// Subpartitions is the optional inline SUBPARTITION list. Names only —
	// option clauses (ENGINE, COMMENT, …) are silently consumed.
	Subpartitions []SubpartitionDef
}

// SubpartitionDef carries a single inline SUBPARTITION declaration.
type SubpartitionDef struct {
	Name string
}

type IndexDef struct {
	Name      string
	Kind      string // ""|"FULLTEXT"|"SPATIAL"|"VECTOR"
	Unique    bool
	Columns   []IndexedCol
	Using     string // BTREE|HASH|""
	Comment   string
	Invisible bool   // MySQL 8 INVISIBLE / MariaDB 10.6 IGNORED
	P         Position
}

func (i *IndexDef) Pos() Position { return i.P }

// ---------------------------------------------------------------------------
// Other DDL statements
// ---------------------------------------------------------------------------

// TruncateTable — `TRUNCATE [TABLE] name`. Maps directly to PG's
// `TRUNCATE TABLE name` (PG also supports the form with TABLE optional).
type TruncateTable struct {
	Table TableRef
	P     Position
}

func (s *TruncateTable) Pos() Position { return s.P }
func (s *TruncateTable) stmtNode()     {}

// RenameTable — `RENAME TABLE a TO b, c TO d, …`. PG supports renaming
// only one table per statement, so the translator emits N PG `ALTER TABLE
// … RENAME TO …` pre-actions.
type RenameTable struct {
	Pairs []RenameTablePair
	P     Position
}

type RenameTablePair struct {
	From TableRef
	To   TableRef
}

func (s *RenameTable) Pos() Position { return s.P }
func (s *RenameTable) stmtNode()     {}

// CreateTableLike — `CREATE TABLE [IF NOT EXISTS] dst LIKE src` (or the
// equivalent paren form `CREATE TABLE dst (LIKE src)`). PG has the same
// `CREATE TABLE … (LIKE other INCLUDING ALL)` clause; the translator
// emits it directly.
type CreateTableLike struct {
	Schema       string
	Name         string
	NameBacktick bool
	IfNotExists  bool
	Temporary    bool

	LikeSchema string
	LikeName   string

	P Position
}

func (s *CreateTableLike) Pos() Position { return s.P }
func (s *CreateTableLike) stmtNode()     {}

// CreateTableAs — `CREATE TABLE [IF NOT EXISTS] dst [(col list)] [opts]
// [IGNORE|REPLACE] [AS] <select>`. The SELECT body is preserved verbatim;
// the translator emits a manual-review prerequisite because the source
// SELECT typically references MySQL-only functions/identifiers that need
// human review before running against PG.
type CreateTableAs struct {
	Schema       string
	Name         string
	NameBacktick bool
	IfNotExists  bool
	Temporary    bool

	Columns []*ColumnDef
	Options TableOptions

	// KeyConflict captures `IGNORE` / `REPLACE`. PG has no equivalent.
	KeyConflict string

	// SelectBody is the raw SELECT statement source (no leading AS, no
	// trailing delimiter).
	SelectBody string

	P Position
}

func (s *CreateTableAs) Pos() Position { return s.P }
func (s *CreateTableAs) stmtNode()     {}

type DropTable struct {
	IfExists bool
	Tables   []TableRef
	P        Position
}

func (s *DropTable) Pos() Position { return s.P }
func (s *DropTable) stmtNode()     {}

// DropObject is the structured form of `DROP {INDEX|VIEW|PROCEDURE|FUNCTION
// |TRIGGER|EVENT}`. The translator maps these to their PG equivalents (with
// warnings for forms PG can't express, like DROP EVENT).
type DropObject struct {
	Kind     string // "INDEX"|"VIEW"|"PROCEDURE"|"FUNCTION"|"TRIGGER"|"EVENT"
	IfExists bool

	// For INDEX: Name + OnTable (MySQL form `DROP INDEX ix ON tbl`).
	Name    string
	Schema  string
	OnTable TableRef

	// For VIEW (and any kind that accepts a list): Names is the full
	// targets list when multiple identifiers were given (DROP VIEW a, b, c).
	// When parsed from a single-target form, Names is empty and Name carries
	// the target.
	Names []TableRef

	Cascade  bool
	Restrict bool
	P        Position
}

func (s *DropObject) Pos() Position { return s.P }
func (s *DropObject) stmtNode()     {}

type TableRef struct {
	Schema       string
	Name         string
	NameBacktick bool
}

type CreateIndex struct {
	Name     string
	Unique   bool
	Kind     string // ""|"FULLTEXT"|"SPATIAL"
	Table    TableRef
	Columns  []IndexedCol
	Using    string
	P        Position
}

func (s *CreateIndex) Pos() Position { return s.P }
func (s *CreateIndex) stmtNode()     {}

type AlterTable struct {
	Table   TableRef
	Actions []AlterAction
	P       Position
}

func (s *AlterTable) Pos() Position { return s.P }
func (s *AlterTable) stmtNode()     {}

type AlterAction struct {
	// Kind is one of:
	//   "ADD_CONSTRAINT"|"ADD_COLUMN"|"DROP_COLUMN"|"DROP_CONSTRAINT"
	//   |"RENAME_TABLE"|"RENAME_COLUMN"|"MODIFY_COLUMN"|"CHANGE_COLUMN"
	//   |"SET_DEFAULT"|"DROP_DEFAULT"|"NOOP"
	Kind       string
	Constraint TableConstraint
	Column     *ColumnDef
	// DropName: target name for DROP_COLUMN / DROP_CONSTRAINT.
	DropName string
	// OldName / NewName: used by RENAME_TABLE (NewName), RENAME_COLUMN
	// (OldName, NewName), CHANGE_COLUMN (OldName + Column.Name carries NewName).
	OldName string
	NewName string
	// DefaultExpr: text of the SET DEFAULT expression (raw, MySQL-shaped).
	DefaultExpr string
	// NoopText: human-readable description of an unsupported ALTER
	// alternative (CONVERT TO charset, ALGORITHM=, LOCK=, FORCE, …) so the
	// translator can surface it as an info-level explanation.
	NoopText string
	// Following holds extra ADD_COLUMN actions parsed from Oracle's
	// parenthesised multi-column form `ADD (c1 t1, c2 t2)`. The
	// statement-level Actions slice already flattens these in (the
	// Oracle parser's parseAlterActions does the spread); Following
	// is set internally by the parser and consumed at statement-build
	// time, so the translator never sees a non-empty Following slice.
	Following []AlterAction
}

// ---------------------------------------------------------------------------
// Routines: view, trigger, procedure, function, event
// ---------------------------------------------------------------------------

type CreateView struct {
	OrReplace    bool
	Algorithm    string // MERGE|TEMPTABLE|UNDEFINED|""
	Definer      string
	SQLSecurity  string // DEFINER|INVOKER|""
	View         TableRef
	Columns      []string // optional column list (X, Y)
	SelectBody   string   // raw SELECT text
	CheckOption  string   // ""|"CASCADED CHECK OPTION"|"LOCAL CHECK OPTION"|"CHECK OPTION"
	// OfType, when set, captures the Oracle object-view's `OF <type_name>`
	// clause. The view's projected columns then bind positionally to the
	// type's attribute list — squishy needs that mapping at translation
	// time to emit explicit `AS <attr>` aliases on the PG side, since PG
	// has no equivalent positional binding and would otherwise produce
	// columns named after the SELECT expression (e.g. `row` for a
	// `ROW(...)::type` cast).
	OfType string
	P      Position
}

func (s *CreateView) Pos() Position { return s.P }
func (s *CreateView) stmtNode()     {}

type CreateTrigger struct {
	Definer  string
	Name     string
	Time     string // BEFORE|AFTER|INSTEAD OF|COMPOUND
	Event    string // INSERT|UPDATE|DELETE
	Table    TableRef
	Order    string // FOLLOWS|PRECEDES|""
	OrderRef string // name of the trigger referenced by FOLLOWS/PRECEDES
	// ForEachRow is true when `FOR EACH ROW` was present (row-level
	// trigger). Oracle defaults to STATEMENT-level when the clause is
	// absent — PG also has FOR EACH STATEMENT, mapped 1:1.
	ForEachRow bool
	// HasForEach is true when either `FOR EACH ROW` or implicit STATEMENT
	// was explicitly classified by the parser. Lets the translator
	// distinguish "explicit statement-level" from "didn't know" (currently
	// treated as row to preserve historical behaviour).
	HasForEach bool
	// WhenCond is the raw text of the optional `WHEN (cond)` clause —
	// PostgreSQL accepts the same syntax inline on CREATE TRIGGER.
	WhenCond string
	// NewAlias / OldAlias capture Oracle's `REFERENCING NEW AS n OLD AS o`
	// renaming so the body's references can be rewritten to PG's fixed
	// NEW/OLD names. Empty means the source used the default names.
	NewAlias string
	OldAlias string
	// SystemTrigger flags Oracle's system-event triggers (BEFORE/AFTER
	// CREATE/ALTER/DROP/LOGON/LOGOFF/STARTUP/SHUTDOWN/SERVERERROR …,
	// attached to ON DATABASE / ON SCHEMA / ON <user>.SCHEMA). Has no
	// direct PG counterpart — the translator surfaces a blocking
	// manual-review prereq with PG event-trigger guidance.
	SystemTrigger bool
	// SystemEvent captures the verb (CREATE/LOGON/SHUTDOWN/…) so the
	// prereq's remediation can hint at the closest PG event-trigger fit.
	SystemEvent string
	// SystemScope is "DATABASE" / "SCHEMA" / "" — Oracle system triggers
	// attach to one of these scopes rather than a table.
	SystemScope string
	// NestedTableTrigger flags Oracle's `ON NESTED TABLE col OF parent`
	// row-level triggers. PostgreSQL has no equivalent — the translator
	// surfaces a manual-review prereq. Table is set to the parent
	// view/table so triggerTable resolution still works for downstream
	// ALTER TRIGGER statements.
	NestedTableTrigger bool
	Body               string // raw
	P                  Position
}

func (s *CreateTrigger) Pos() Position { return s.P }
func (s *CreateTrigger) stmtNode()     {}

type Param struct {
	Direction string // IN|OUT|INOUT (for procedures); "" for functions
	Name      string
	Type      DataType
	Default   Expr // nil if no DEFAULT/:= clause; populated by Oracle parser only today
}

type RoutineCharacteristics struct {
	Deterministic    bool
	HasDeterministic bool
	SQLDataAccess    string // CONTAINS SQL | NO SQL | READS SQL DATA | MODIFIES SQL DATA
	SQLSecurity      string // DEFINER|INVOKER
	Language         string // SQL (only)
	Comment          string
}

type CreateProcedure struct {
	Definer         string
	Name            string
	Params          []Param
	Characteristics RoutineCharacteristics
	Body            string // raw (BEGIN ... END or single stmt)
	P               Position
}

func (s *CreateProcedure) Pos() Position { return s.P }
func (s *CreateProcedure) stmtNode()     {}

type CreateFunction struct {
	Definer         string
	Name            string
	Params          []Param
	Returns         DataType
	Characteristics RoutineCharacteristics
	Body            string
	P               Position
}

func (s *CreateFunction) Pos() Position { return s.P }
func (s *CreateFunction) stmtNode()     {}

type CreateEvent struct {
	Definer      string
	Name         string
	IfNotExists  bool
	ScheduleKind string // AT|EVERY
	At           string // raw expr
	EveryN       int64
	EveryUnit    string // SECOND|MINUTE|HOUR|DAY|...
	Starts       string
	Ends         string
	OnCompletion string // PRESERVE|NOT PRESERVE|""
	Enable       string // ENABLE|DISABLE|DISABLE ON SLAVE|""
	Comment      string
	Body         string
	P            Position
}

func (s *CreateEvent) Pos() Position { return s.P }
func (s *CreateEvent) stmtNode()     {}

// Other statements we recognize but do not semantically exploit (SET NAMES, USE, CALL).
type NoopStmt struct {
	Kind string
	Text string
	P    Position
}

func (s *NoopStmt) Pos() Position { return s.P }
func (s *NoopStmt) stmtNode()     {}

// ---------------------------------------------------------------------------
// Oracle-specific top-level DDL statements
// ---------------------------------------------------------------------------

// CreateSequence — CREATE SEQUENCE [schema.]name [START WITH n] [INCREMENT BY n]
//
//	[MAXVALUE n|NOMAXVALUE] [MINVALUE n|NOMINVALUE] [CYCLE|NOCYCLE]
//	[CACHE n|NOCACHE] [ORDER|NOORDER].
type CreateSequence struct {
	OrReplace   bool
	IfNotExists bool
	Temporary   bool // MariaDB CREATE TEMPORARY SEQUENCE
	Schema      string
	Name        string
	Start       int64
	HasStart    bool
	Increment   int64
	HasIncr     bool
	MinValue    int64
	HasMin      bool
	NoMin       bool
	MaxValue    int64
	HasMax      bool
	NoMax       bool
	Cache       int64
	HasCache    bool
	NoCache     bool
	Cycle       bool // true = CYCLE, false = NOCYCLE (or unspecified)
	HasCycle    bool
	// IgnoredOptions captures Oracle-spec sequence knobs without PG
	// counterparts (ORDER/NOORDER/KEEP/NOKEEP/SCALE/NOSCALE/EXTEND/SHARD/
	// NOSHARD/SHARING/SESSION). Surfaced as a single info-level explanation.
	IgnoredOptions []string
	P              Position
}

func (s *CreateSequence) Pos() Position { return s.P }
func (s *CreateSequence) stmtNode()     {}

// AlterSequence — Oracle's `ALTER SEQUENCE [schema.]name <spec>+` where
// each spec is one of the same options accepted by CREATE SEQUENCE except
// START WITH (which Oracle disallows on plain ALTER and only allows with
// RESTART in 12.2+). Most specs map directly to PG's ALTER SEQUENCE; the
// Oracle-only knobs (ORDER/NOORDER/KEEP/NOKEEP/SCALE/SHARING/SESSION/
// GLOBAL/SHARD) carry no PG equivalent and the translator surfaces them as
// info-level explanations.
type AlterSequence struct {
	Schema       string
	Name         string
	Increment    int64
	HasIncr      bool
	MinValue     int64
	HasMin       bool
	NoMin        bool
	MaxValue     int64
	HasMax       bool
	NoMax        bool
	Cache        int64
	HasCache     bool
	NoCache      bool
	Cycle        bool
	HasCycle     bool
	Restart      bool  // bare RESTART (PG: ALTER SEQUENCE … RESTART)
	HasRestart   bool
	StartWith    int64 // RESTART WITH N — Oracle 12.2+
	HasStartWith bool
	// IgnoredOptions records Oracle-only specs that have no PG equivalent
	// (ORDER, NOORDER, KEEP, NOKEEP, SCALE, NOSCALE, SHARING, SESSION,
	// GLOBAL, SHARD, NOSHARD). Captured verbatim for the explanation log.
	IgnoredOptions []string
	P              Position
}

func (s *AlterSequence) Pos() Position { return s.P }
func (s *AlterSequence) stmtNode()     {}

// AlterIndex — Oracle's `ALTER INDEX [schema.]name <action>`. Most actions
// (REBUILD / COMPILE / ENABLE / DISABLE / VISIBLE / INVISIBLE / UNUSABLE /
// COMPRESS / partition-level) have no PG counterpart and are dropped with
// an info-level explanation. RENAME TO maps directly to PG's ALTER INDEX.
type AlterIndex struct {
	Schema  string
	Name    string
	Action  string // "RENAME"|"REBUILD"|"COMPILE"|"ENABLE"|"DISABLE"|"VISIBLE"|"INVISIBLE"|"UNUSABLE"|"COMPRESS"|"NOCOMPRESS"|"PARTITION"|"OTHER"
	NewName string // populated for RENAME (and case-folded by the translator)
	RawTail string // remainder of the statement, for the explanation log
	P       Position
}

func (s *AlterIndex) Pos() Position { return s.P }
func (s *AlterIndex) stmtNode()     {}

// AlterTrigger — Oracle's `ALTER TRIGGER [schema.]name <action>`. Action
// is one of ENABLE / DISABLE / RENAME (TO new) / COMPILE. Oracle triggers
// don't carry the table name in their own DDL — the translator resolves
// it from the migration plan; if the trigger isn't in the plan it surfaces
// as a manual-review prereq.
type AlterTrigger struct {
	Schema  string
	Name    string
	Action  string // "ENABLE"|"DISABLE"|"RENAME"|"COMPILE"
	NewName string // for RENAME
	P       Position
}

func (s *AlterTrigger) Pos() Position { return s.P }
func (s *AlterTrigger) stmtNode()     {}

// AlterView — Oracle's `ALTER VIEW [schema.]name <action>` (COMPILE,
// EDITIONABLE, NONEDITIONABLE, ADD/MODIFY/DROP CONSTRAINT). None map to
// PG's ALTER VIEW (which only supports RENAME / OWNER / SET SCHEMA / SET
// OPTIONS). Captured here so the translator can surface a single info-level
// explanation per statement instead of silently dropping it via the noop
// dispatcher.
type AlterView struct {
	Schema string
	Name   string
	Action string // "COMPILE"|"EDITIONABLE"|"NONEDITIONABLE"|"CONSTRAINT"|"OTHER"
	P      Position
}

func (s *AlterView) Pos() Position { return s.P }
func (s *AlterView) stmtNode()     {}

// AlterRoutine — Oracle's `ALTER {PROCEDURE|FUNCTION|PACKAGE} [schema.]name
// COMPILE [BODY|SPECIFICATION] …`. PG has no recompile DDL — the function
// is reparsed every CREATE FUNCTION. Dropped with an explanation.
type AlterRoutine struct {
	Kind   string // "PROCEDURE"|"FUNCTION"|"PACKAGE"
	Schema string
	Name   string
	P      Position
}

func (s *AlterRoutine) Pos() Position { return s.P }
func (s *AlterRoutine) stmtNode()     {}

// AlterType — Oracle's `ALTER TYPE [schema.]name {COMPILE | ADD/MODIFY/DROP
// ATTRIBUTE | OVERRIDING METHOD | …}`. PG's ALTER TYPE accepts ADD/RENAME/
// DROP ATTRIBUTE for composite types but Oracle's object-with-methods type
// system doesn't map cleanly — surface a manual-review prereq.
type AlterType struct {
	Schema string
	Name   string
	Action string // "COMPILE"|"ADD_ATTRIBUTE"|"DROP_ATTRIBUTE"|"MODIFY_ATTRIBUTE"|"OTHER"
	P      Position
}

func (s *AlterType) Pos() Position { return s.P }
func (s *AlterType) stmtNode()     {}

// CreatePackage — CREATE [OR REPLACE] PACKAGE [schema.]name {IS|AS} <spec> END [name];
// Body holds the verbatim package spec (declarations only, no implementations).
type CreatePackage struct {
	OrReplace bool
	Schema    string
	Name      string
	Spec      string // raw spec block
	P         Position
}

func (s *CreatePackage) Pos() Position { return s.P }
func (s *CreatePackage) stmtNode()     {}

// CreatePackageBody — CREATE [OR REPLACE] PACKAGE BODY [schema.]name {IS|AS} <body> END [name];
type CreatePackageBody struct {
	OrReplace bool
	Schema    string
	Name      string
	Body      string // raw body block
	P         Position
}

func (s *CreatePackageBody) Pos() Position { return s.P }
func (s *CreatePackageBody) stmtNode()     {}

// CreateType — CREATE [OR REPLACE] TYPE [schema.]name {IS|AS}
//
//	OBJECT(...) | VARRAY(n) OF t | TABLE OF t.
type CreateType struct {
	OrReplace bool
	Schema    string
	Name      string
	Kind      string // "OBJECT"|"VARRAY"|"TABLE"
	Body      string // raw after the kind keyword (always populated)
	// Attributes is populated for OBJECT types when the parser was able to
	// extract per-attribute column definitions from the body. Empty when
	// the body contains methods (MEMBER FUNCTION/PROCEDURE) or other
	// constructs we don't yet model — the translator falls back to a
	// manual-review prerequisite in that case.
	Attributes []ColumnDef
	// HasMethods is set when the OBJECT body declares MEMBER FUNCTION /
	// MEMBER PROCEDURE / MAP MEMBER / ORDER MEMBER. Triggers a blocking
	// prereq because PG composite types have no methods.
	HasMethods bool
	// ElementType is populated for VARRAY / TABLE OF: the inner element
	// type captured as raw text. Empty for OBJECT.
	ElementType string
	// ParentType is set for Oracle subtype declarations
	// (`CREATE TYPE name UNDER parent_type …`). PostgreSQL has no type
	// inheritance — the translator surfaces a manual-review note and
	// flattens the subtype into a standalone composite when possible.
	ParentType string
	P          Position
}

func (s *CreateType) Pos() Position { return s.P }
func (s *CreateType) stmtNode()     {}

// CreateTypeBody — CREATE [OR REPLACE] TYPE BODY [schema.]name {IS|AS} <body> END;
type CreateTypeBody struct {
	OrReplace bool
	Schema    string
	Name      string
	Body      string
	P         Position
}

func (s *CreateTypeBody) Pos() Position { return s.P }
func (s *CreateTypeBody) stmtNode()     {}

// CreateMaterializedView — CREATE MATERIALIZED VIEW [schema.]name AS <select>.
type CreateMaterializedView struct {
	OrReplace    bool
	View         TableRef
	Columns      []string
	SelectBody   string
	BuildMode    string // IMMEDIATE|DEFERRED|""
	RefreshMode  string // COMPLETE|FAST|FORCE|NEVER|""
	RefreshOn    string // DEMAND|COMMIT|""
	Extras       []string // preserved raw storage/index clauses
	P            Position
}

func (s *CreateMaterializedView) Pos() Position { return s.P }
func (s *CreateMaterializedView) stmtNode()     {}
