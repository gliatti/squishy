package ast

// ---------------------------------------------------------------------------
// DML AST — SELECT / INSERT / UPDATE / DELETE / MERGE.
//
// These nodes are produced by the per-dialect DML parsers and consumed by the
// AST→AST translator and the PostgreSQL writer. They replace the "DML kept as
// raw string" decision originally taken in plsql.go: every embedded SELECT,
// trigger body or routine body now decomposes into typed nodes so rewriting
// can happen by visitor instead of regex.
//
// Where a node currently has a raw-text fallback (e.g. WindowSpec.RawSpec)
// it is documented as transitional and slated for replacement once the
// parser surfaces the structured form.
// ---------------------------------------------------------------------------

// SelectStmt — a SELECT query with all the usual clauses.
//
// SetOps lets the same struct represent UNION/INTERSECT/EXCEPT chains: the
// "head" SELECT lives in the struct itself, and additional combined branches
// are appended as SetOp entries in source order.
type SelectStmt struct {
	With *WithClause // optional WITH (CTE) prelude

	Distinct bool   // SELECT DISTINCT ...
	AllOnly  bool   // SELECT ALL ... (informational; default behaviour)
	Cols     []SelectItem
	From     []FromItem
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr

	// SetOps is the chain of combined queries appended to this SELECT
	// (UNION/INTERSECT/EXCEPT/MINUS), in source order.
	SetOps []SetOp

	// ForUpdate captures `FOR UPDATE` / `FOR SHARE` row-locking clauses. The
	// raw text is preserved verbatim — Oracle and MySQL dialects diverge on
	// the clause grammar and a structured model is out of scope for v1.
	ForUpdate string

	P Position
}

func (s *SelectStmt) Pos() Position { return s.P }
func (s *SelectStmt) stmtNode()     {}
func (s *SelectStmt) plStmtNode()   {} // can appear inside a PL block

// SelectItem — one entry of the projection list.
type SelectItem struct {
	// Star is true for `*` or `t.*`. When set, Expr is nil and Qualifier
	// holds the optional table qualifier ("" for plain `*`).
	Star      bool
	Qualifier string

	Expr  Expr
	Alias string // optional `AS alias`
}

// SetOp combines a follow-on SELECT with a set operator.
type SetOp struct {
	Op   string // "UNION" | "UNION ALL" | "INTERSECT" | "EXCEPT" | "MINUS"
	Stmt *SelectStmt
}

// WithClause — `WITH [RECURSIVE] cte1 AS (...), cte2 AS (...)`.
type WithClause struct {
	Recursive bool
	CTEs      []CTE
}

// CTE — a single common-table expression.
type CTE struct {
	Name    string
	Columns []string // optional explicit column list
	Body    *SelectStmt
}

// OrderItem — `expr [ASC|DESC] [NULLS FIRST|LAST]`.
type OrderItem struct {
	Expr      Expr
	Desc      bool   // true for DESC
	NullsLast *bool  // tri-state: nil = unspecified, *true = NULLS LAST, *false = NULLS FIRST
}

// WindowSpec — the body of an OVER(...) clause.
//
// PartitionBy/OrderBy/Frame are populated when the parser fully decomposes
// the spec; for Oracle's exotic `OVER (...)` shapes that we don't yet model
// (RANGE BETWEEN ... PRECEDING with frame-exclusion), RawSpec preserves the
// verbatim text. RawSpec is a transitional escape hatch and will go away as
// the parser grows.
type WindowSpec struct {
	PartitionBy []Expr
	OrderBy     []OrderItem
	Frame       string // raw frame clause, e.g. "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW"
	RawSpec     string // full body when not yet decomposed
}

// ---------------------------------------------------------------------------
// FROM-clause sources
// ---------------------------------------------------------------------------

// FromItem represents one entry in a FROM list — a table reference, a
// subquery, or a JOIN tree. It is implemented by FromTable, FromSubquery and
// FromJoin.
type FromItem interface {
	Node
	fromItemNode()
}

// FromTable — a base table reference with optional alias.
type FromTable struct {
	Schema       string
	Name         string
	NameBacktick bool
	Alias        string // empty when no alias
	P            Position
}

func (f *FromTable) Pos() Position  { return f.P }
func (f *FromTable) fromItemNode() {}

// FromSubquery — `(SELECT ...) [AS] alias`.
type FromSubquery struct {
	Stmt  *SelectStmt
	Alias string // PG requires an alias; the parser must enforce it
	Cols  []string // optional column-alias list
	P     Position
}

func (f *FromSubquery) Pos() Position  { return f.P }
func (f *FromSubquery) fromItemNode() {}

// FromJoin — an explicit JOIN of two FROM items.
//
// On is set for `JOIN ... ON cond`; Using is set for `JOIN ... USING (cols)`;
// Natural is set for `NATURAL JOIN ...`. Exactly one of these three is
// populated.
type FromJoin struct {
	Kind    JoinKind
	Left    FromItem
	Right   FromItem
	On      Expr
	Using   []string
	Natural bool
	P       Position
}

func (f *FromJoin) Pos() Position  { return f.P }
func (f *FromJoin) fromItemNode() {}

// JoinKind enumerates the supported JOIN forms.
type JoinKind int

const (
	InnerJoin JoinKind = iota
	LeftJoin
	RightJoin
	FullJoin
	CrossJoin
)

func (k JoinKind) String() string {
	switch k {
	case InnerJoin:
		return "INNER JOIN"
	case LeftJoin:
		return "LEFT JOIN"
	case RightJoin:
		return "RIGHT JOIN"
	case FullJoin:
		return "FULL JOIN"
	case CrossJoin:
		return "CROSS JOIN"
	}
	return "JOIN"
}

// ---------------------------------------------------------------------------
// Mutation statements
// ---------------------------------------------------------------------------

// InsertStmt — INSERT INTO table [(cols)] {VALUES (...) | SELECT ...}
//             [ON CONFLICT/ON DUPLICATE KEY UPDATE ...].
type InsertStmt struct {
	Table   TableRef
	Cols    []string // empty means "all columns in declared order"
	Values  [][]Expr // populated for VALUES form
	Select  *SelectStmt // populated for INSERT ... SELECT
	OnConflict *OnConflict
	Returning  []SelectItem // PG/Oracle RETURNING; empty when absent
	P         Position
}

func (s *InsertStmt) Pos() Position { return s.P }
func (s *InsertStmt) stmtNode()     {}
func (s *InsertStmt) plStmtNode()   {}

// OnConflict captures the upsert tail — covers MySQL's `ON DUPLICATE KEY
// UPDATE` and PG's `ON CONFLICT (cols) DO UPDATE/NOTHING`. The translator
// normalises MySQL form into PG form by inferring the conflict target from
// the table's primary/unique keys.
type OnConflict struct {
	Target  []string // conflict columns; empty means "any unique"
	DoNothing bool
	Sets    []Assign // when DoNothing is false
	Where   Expr     // optional `WHERE` on the update branch
}

// UpdateStmt — UPDATE [t] SET ... [FROM ...] [WHERE ...].
type UpdateStmt struct {
	Table TableRef
	Alias string
	Sets  []Assign
	From  []FromItem // PG-style multi-table update; MySQL's comma-list maps here
	Where Expr
	Returning []SelectItem
	P     Position
}

func (s *UpdateStmt) Pos() Position { return s.P }
func (s *UpdateStmt) stmtNode()     {}
func (s *UpdateStmt) plStmtNode()   {}

// Assign — `col = expr` in an UPDATE/SET list, OR
// `(col1, col2, ...) = (subquery)` (the tuple form, supported by both
// Oracle and PostgreSQL). When TupleCols is non-empty, Col is unused and
// Expr is the row-source expression (typically a parenthesised SELECT
// returning the same arity as TupleCols).
type Assign struct {
	Col       string
	TupleCols []string
	Expr      Expr
}

// DeleteStmt — DELETE FROM t [WHERE ...].
type DeleteStmt struct {
	Table TableRef
	Alias string
	Using []FromItem // PG `USING` / MySQL multi-table delete sources
	Where Expr
	Returning []SelectItem
	P     Position
}

func (s *DeleteStmt) Pos() Position { return s.P }
func (s *DeleteStmt) stmtNode()     {}
func (s *DeleteStmt) plStmtNode()   {}

// MergeStmt — MERGE INTO target USING source ON cond WHEN [NOT] MATCHED ...
type MergeStmt struct {
	Target       TableRef
	TargetAlias  string
	Source       FromItem // table or subquery
	On           Expr
	WhenMatched  []MergeAction
	WhenNotMatched []MergeAction

	// HasLogErrors is true when the source MERGE carried a `LOG ERRORS
	// [INTO …] [REJECT LIMIT …]` trailer. The parser swallows the clause
	// (PG has no equivalent); this flag lets the translator surface a
	// remediation warning.
	HasLogErrors bool

	P            Position
}

func (s *MergeStmt) Pos() Position { return s.P }
func (s *MergeStmt) stmtNode()     {}
func (s *MergeStmt) plStmtNode()   {}

// MergeAction is a WHEN [NOT] MATCHED [AND cond] THEN <action> branch.
//
// Kind: "UPDATE" | "DELETE" | "INSERT" | "DO NOTHING".
//   * UPDATE  — Sets is populated.
//   * DELETE  — no payload.
//   * INSERT  — InsertCols + InsertValues are populated (NOT MATCHED branch).
//   * DO NOTHING — Oracle 23ai / PG MERGE; no payload.
type MergeAction struct {
	Kind         string
	Cond         Expr // optional `AND cond`
	Sets         []Assign
	InsertCols   []string
	InsertValues []Expr

	// HasInlineDelete is true when the source had Oracle's `WHEN MATCHED
	// THEN UPDATE SET … DELETE WHERE …` combo on this UPDATE branch. The
	// inline DELETE is dropped during parsing (PG MERGE has no equivalent
	// shape — it would need a second `WHEN MATCHED AND <cond> THEN DELETE`
	// branch); the translator surfaces a manual-remediation warning.
	HasInlineDelete bool
}
