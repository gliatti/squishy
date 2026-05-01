package ast

// Literal covers number/string/bool/null literals. Kind is informational; the raw
// token is kept in Text for verbatim emission in generated SQL.
type Literal struct {
	Kind string // "number"|"string"|"null"|"bool"|"hex"|"bit"
	Text string
	P    Position
}

func (e *Literal) Pos() Position { return e.P }
func (e *Literal) exprNode()     {}

// Ident is a bare or qualified identifier reference.
type Ident struct {
	Parts    []string // e.g. ["db","t","c"]
	Backtick bool
	P        Position
}

func (e *Ident) Pos() Position { return e.P }
func (e *Ident) exprNode()     {}

// FuncCall — e.g. CURRENT_TIMESTAMP(3), UUID(), JSON_EXTRACT(col, '$.x')
type FuncCall struct {
	Name string
	Args []Expr
	P    Position
}

func (e *FuncCall) Pos() Position { return e.P }
func (e *FuncCall) exprNode()     {}

// BinaryExpr — e.g. (10 + 5), c = 0
type BinaryExpr struct {
	Op       string
	Lhs, Rhs Expr
	P        Position
}

func (e *BinaryExpr) Pos() Position { return e.P }
func (e *BinaryExpr) exprNode()     {}

type UnaryExpr struct {
	Op  string
	Rhs Expr
	P   Position
}

func (e *UnaryExpr) Pos() Position { return e.P }
func (e *UnaryExpr) exprNode()     {}

// ParenExpr — preserved explicitly so that `DEFAULT (10+5)` round-trips correctly in PG.
type ParenExpr struct {
	Inner Expr
	P     Position
}

func (e *ParenExpr) Pos() Position { return e.P }
func (e *ParenExpr) exprNode()     {}

// RawExpr stores the verbatim text of an expression fragment when we don't want
// to descend further (e.g. inside a raw routine body). Keeps the parser shallow
// while preserving roundtrip content.
//
// Deprecated: kept for transition only. New code should produce typed Expr
// nodes (CaseExpr, FuncCall, BinaryExpr, etc.) so the AST→AST translator can
// rewrite them without falling back on text manipulation. RawExpr will be
// removed once every parser produces a fully-typed expression tree.
type RawExpr struct {
	Text string
	P    Position
}

func (e *RawExpr) Pos() Position { return e.P }
func (e *RawExpr) exprNode()     {}

// CursorAttr — Oracle PL/SQL cursor attribute reference:
//
//	cursor_name%FOUND     SQL%FOUND
//	cursor_name%NOTFOUND  SQL%NOTFOUND
//	cursor_name%ISOPEN    (SQL%ISOPEN is always FALSE in Oracle)
//	cursor_name%ROWCOUNT  SQL%ROWCOUNT
//
// Cursor is the explicit cursor variable's name (lowercase preserved
// from source) for an explicit cursor, or "SQL" for the implicit
// cursor (last DML statement's stats). Attr is the upper-cased
// attribute keyword.
//
// PG plpgsql equivalents:
//
//	%FOUND      → FOUND (after FETCH/UPDATE/INSERT/DELETE)
//	%NOTFOUND   → NOT FOUND
//	%ISOPEN     → cursor_name IS NOT NULL (PL/pgSQL refcursor idiom)
//	%ROWCOUNT   → exposed via GET DIAGNOSTICS … = ROW_COUNT (after the
//	              triggering DML); not directly substitutable inline.
//
// The Phase 3 cursor-attributes visitor consumes this node and emits
// the matching PG construct or surfaces a blocking prerequisite.
type CursorAttr struct {
	Cursor string // "SQL" for the implicit cursor; otherwise the cursor var name
	Attr   string // "FOUND" | "NOTFOUND" | "ISOPEN" | "ROWCOUNT"
	P      Position
}

func (e *CursorAttr) Pos() Position { return e.P }
func (e *CursorAttr) exprNode()     {}

// SequenceRef — Oracle's pseudo-column form for sequence access:
//
//	seq.NEXTVAL                    schema.seq.NEXTVAL
//	seq.CURRVAL                    schema.seq.CURRVAL
//
// PG exposes the same operations as functions: nextval('seq'),
// currval('seq'). Translates via the Phase 3.4 visitor.
//
// Schema is the optional owning schema (empty when the source used a
// bare sequence name); Name is the sequence name in source casing
// (the Phase 3 normalisation visitor lower-cases unquoted Oracle
// uppers in place when needed). Op is upper-cased ("NEXTVAL" |
// "CURRVAL").
type SequenceRef struct {
	Schema string
	Name   string
	Op     string
	P      Position
}

func (e *SequenceRef) Pos() Position { return e.P }
func (e *SequenceRef) exprNode()     {}

// CaseExpr — `CASE [operand] WHEN m THEN r [WHEN ...] [ELSE e] END`.
// Operand is non-nil for the simple form (`CASE x WHEN 1 THEN 'a'`); nil for
// the searched form (`CASE WHEN x = 1 THEN 'a'`).
type CaseExpr struct {
	Operand Expr
	Whens   []CaseExprWhen
	Else    Expr
	P       Position
}

type CaseExprWhen struct {
	Match Expr
	Then  Expr
}

func (e *CaseExpr) Pos() Position { return e.P }
func (e *CaseExpr) exprNode()     {}

// BetweenExpr — `expr [NOT] BETWEEN lo AND hi`.
type BetweenExpr struct {
	Expr Expr
	Low  Expr
	High Expr
	Not  bool
	P    Position
}

func (e *BetweenExpr) Pos() Position { return e.P }
func (e *BetweenExpr) exprNode()     {}

// InExpr — `expr [NOT] IN (list)` or `expr [NOT] IN (subquery)`.
// Exactly one of List or Subquery is set.
type InExpr struct {
	Expr     Expr
	List     []Expr
	Subquery *SelectStmt
	Not      bool
	P        Position
}

func (e *InExpr) Pos() Position { return e.P }
func (e *InExpr) exprNode()     {}

// ExistsExpr — `[NOT] EXISTS (subquery)`.
type ExistsExpr struct {
	Subquery *SelectStmt
	Not      bool
	P        Position
}

func (e *ExistsExpr) Pos() Position { return e.P }
func (e *ExistsExpr) exprNode()     {}

// SubqueryExpr — a scalar subquery used as an expression: `(SELECT ...)`.
type SubqueryExpr struct {
	Stmt *SelectStmt
	P    Position
}

func (e *SubqueryExpr) Pos() Position { return e.P }
func (e *SubqueryExpr) exprNode()     {}

// WindowedAgg wraps a FuncCall with optional `WITHIN GROUP (ORDER BY ...)` and
// `OVER (...)` modifiers — the shape required for Oracle LISTAGG and SQL
// window/aggregate functions in general.
//
// Within is set for ordered-set aggregates (LISTAGG, PERCENTILE_*); Over is
// set when the aggregate is used as a window function (`SUM(x) OVER (...)`).
// Both may be set, neither must be.
type WindowedAgg struct {
	Func   *FuncCall
	Within []OrderItem
	Over   *WindowSpec
	P      Position
}

func (e *WindowedAgg) Pos() Position { return e.P }
func (e *WindowedAgg) exprNode()     {}

// OuterJoinHint marks an inner expression as carrying Oracle's `(+)`
// outer-join annotation (e.g. `t.col(+)`). The hint is consumed by the
// translator's outer-join rewriter, which lifts the predicate out of WHERE
// and converts the FROM list into ANSI LEFT/RIGHT JOIN form. After that
// pass no OuterJoinHint should remain in the tree.
type OuterJoinHint struct {
	Inner Expr
	P     Position
}

func (e *OuterJoinHint) Pos() Position { return e.P }
func (e *OuterJoinHint) exprNode()     {}

// IntervalLit — `INTERVAL '<value>' <unit>`. Unit captures the trailing
// qualifier (DAY, MONTH, "YEAR TO MONTH", "DAY(2) TO SECOND(6)", …) verbatim
// so dialects with rich precision specs round-trip correctly.
type IntervalLit struct {
	Value string
	Unit  string
	P     Position
}

func (e *IntervalLit) Pos() Position { return e.P }
func (e *IntervalLit) exprNode()     {}

// CastExpr — `CAST(expr AS type)` and dialect equivalents (Oracle's
// `TREAT(expr AS type)` is parsed into a separate node since its semantics
// differ; CONVERT(...) maps here when the syntax is the SQL-standard form).
type CastExpr struct {
	Expr Expr
	Type DataType
	P    Position
}

func (e *CastExpr) Pos() Position { return e.P }
func (e *CastExpr) exprNode()     {}
