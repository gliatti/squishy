package ast

// ---------------------------------------------------------------------------
// PL/SQL (procedural) AST — shared shape for MySQL procedural bodies and
// PL/pgSQL blocks. The MySQL parser (dialects/mysql/parser_plsql.go)
// produces these nodes; the translator rewrites them into PL/pgSQL via the
// dialects/postgres writer.
//
// The AST is intentionally a narrow procedural core (block, declare,
// assign, if, while, for, case, return, call, cursor ops, signal). SQL DML
// statements that appear inside a body (SELECT/INSERT/UPDATE/DELETE) are
// captured as RawSQL so we don't re-parse a full DML grammar in v2 — the
// body rewriter takes care of the intra-SQL lexical differences (backticks,
// function renames, JSON paths).
// ---------------------------------------------------------------------------

// PLStmt is a procedural statement inside a routine body.
type PLStmt interface {
	Node
	plStmtNode()
}

// Block — BEGIN [label] DECLARE <vars> ... <stmts> END [label].
type Block struct {
	Label string // optional label for LEAVE/ITERATE targets
	Decls []PLDecl
	Stmts []PLStmt
	// Except carries the optional `EXCEPTION WHEN … THEN …;` tail. nil
	// when the block has no exception section. Phase 1.10 populates it
	// in parallel with the legacy RawSQL "/*EXCEPTION*/" marker that
	// the translator's text path still consumes — the typed form is
	// strictly additive during the transition window.
	Except *ExceptionBlock
	P      Position
}

func (b *Block) Pos() Position { return b.P }
func (b *Block) plStmtNode()   {}

// PLDecl is a local declaration: variable, cursor or handler.
type PLDecl interface {
	Node
	plDeclNode()
}

// DeclareVar — DECLARE name TYPE [DEFAULT expr];
type DeclareVar struct {
	Name    string
	Type    DataType
	Default Expr
	P       Position
}

func (d *DeclareVar) Pos() Position { return d.P }
func (d *DeclareVar) plDeclNode()   {}

// DeclareCursor — DECLARE name CURSOR FOR <select stmt (raw SQL)>;
type DeclareCursor struct {
	Name       string
	Params     string // raw text of the parenthesized param list, e.g. "p_lot VARCHAR2, p_tra NUMBER"; empty for parameterless cursors. Oracle's `CURSOR c (p1 t1, p2 t2) IS SELECT …` maps to PG's `c CURSOR (p1 t1, p2 t2) FOR SELECT …`.
	SelectBody string // raw SELECT text (SQL passthrough)
	P          Position
}

func (d *DeclareCursor) Pos() Position { return d.P }
func (d *DeclareCursor) plDeclNode()   {}

// DeclareHandler — DECLARE {CONTINUE|EXIT} HANDLER FOR <cond> <stmt>;
// Conditions covered: NOT FOUND, SQLEXCEPTION, SQLWARNING, named SQLSTATE.
type DeclareHandler struct {
	Kind      string // "CONTINUE" | "EXIT"
	Condition string // "NOT FOUND" | "SQLEXCEPTION" | "SQLWARNING" | "SQLSTATE '...'"
	Action    PLStmt
	P         Position
}

func (d *DeclareHandler) Pos() Position { return d.P }
func (d *DeclareHandler) plDeclNode()   {}

// AssignStmt — SET x = expr  OR  x := expr;
type AssignStmt struct {
	Target string // name; may be prefixed with NEW./OLD. or @ for session vars
	Expr   Expr
	P      Position
}

func (a *AssignStmt) Pos() Position { return a.P }
func (a *AssignStmt) plStmtNode()   {}

// IfStmt — IF cond THEN ... ELSEIF cond THEN ... ELSE ... END IF;
type IfStmt struct {
	Branches []IfBranch
	Else     []PLStmt
	P        Position
}

type IfBranch struct {
	Cond Expr
	Body []PLStmt
}

func (i *IfStmt) Pos() Position { return i.P }
func (i *IfStmt) plStmtNode()   {}

// CaseStmt — CASE [expr] WHEN ... THEN ... ELSE ... END CASE;
type CaseStmt struct {
	Expr    Expr   // nil when searched CASE
	When    []CaseWhen
	Else    []PLStmt
	P       Position
}

type CaseWhen struct {
	Match Expr
	Body  []PLStmt
}

func (c *CaseStmt) Pos() Position { return c.P }
func (c *CaseStmt) plStmtNode()   {}

// WhileStmt — [label:] WHILE cond DO stmts END WHILE [label];
type WhileStmt struct {
	Label string
	Cond  Expr
	Body  []PLStmt
	P     Position
}

func (w *WhileStmt) Pos() Position { return w.P }
func (w *WhileStmt) plStmtNode()   {}

// LoopStmt — [label:] LOOP stmts END LOOP [label];
type LoopStmt struct {
	Label string
	Body  []PLStmt
	P     Position
}

func (l *LoopStmt) Pos() Position { return l.P }
func (l *LoopStmt) plStmtNode()   {}

// RepeatStmt — [label:] REPEAT stmts UNTIL cond END REPEAT [label];
type RepeatStmt struct {
	Label string
	Body  []PLStmt
	Cond  Expr
	P     Position
}

func (r *RepeatStmt) Pos() Position { return r.P }
func (r *RepeatStmt) plStmtNode()   {}

// LeaveStmt — LEAVE label;  (→ PG EXIT label;)
type LeaveStmt struct {
	Label string
	// WhenCond carries the optional `WHEN <cond>` guard for Oracle's
	// `EXIT [label] WHEN <cond>;` form. Nil for an unconditional EXIT.
	// Translates to PG `EXIT [label] WHEN <cond>;` (a single PLExitWhen
	// node when the cond is non-nil).
	WhenCond Expr
	P        Position
}

func (l *LeaveStmt) Pos() Position { return l.P }
func (l *LeaveStmt) plStmtNode()   {}

// IterateStmt — ITERATE label;  (→ PG CONTINUE label;)
type IterateStmt struct {
	Label string
	// WhenCond carries the optional `WHEN <cond>` guard for Oracle's
	// `CONTINUE [label] WHEN <cond>;` form. Nil for an unconditional
	// CONTINUE. Translates to PG `CONTINUE [label] WHEN <cond>;`.
	WhenCond Expr
	P        Position
}

func (i *IterateStmt) Pos() Position { return i.P }
func (i *IterateStmt) plStmtNode()   {}

// ReturnStmt — RETURN expr;
type ReturnStmt struct {
	Expr Expr // nil in procedures; present in functions
	P    Position
}

func (r *ReturnStmt) Pos() Position { return r.P }
func (r *ReturnStmt) plStmtNode()   {}

// CallStmt — CALL name(args);
type CallStmt struct {
	Schema string
	Name   string
	Args   []Expr
	P      Position
}

func (c *CallStmt) Pos() Position { return c.P }
func (c *CallStmt) plStmtNode()   {}

// Cursor ops: OPEN / FETCH INTO / CLOSE.
type OpenStmt struct {
	Cursor string
	// Args is the raw text of the parenthesised actuals when the cursor
	// was declared with parameters: `OPEN cur(a1, a2)`. Empty for
	// parameterless cursors. Stored verbatim so the PG writer can
	// re-emit `OPEN cur(a1, a2)` without re-parsing the expression.
	Args string
	// ForQuery is the body of `OPEN cur FOR <query>` (Oracle dynamic cursor
	// open). Empty for static `OPEN cur` (cursor declared in DECLARE block).
	ForQuery string
	// IsDynamic is true when ForQuery is an expression (string built at
	// runtime) rather than a literal SELECT. Maps to PG's `OPEN cur FOR
	// EXECUTE …`. False (default) means the body is a literal SELECT and
	// maps to PG's `OPEN cur FOR SELECT …`.
	IsDynamic bool
	// UsingArgs are the bind variables passed via `USING <args>`. Empty
	// when no USING clause was present.
	UsingArgs []string
	P         Position
}

func (o *OpenStmt) Pos() Position { return o.P }
func (o *OpenStmt) plStmtNode()   {}

type FetchStmt struct {
	Cursor string
	Into   []string // variable names receiving columns
	P      Position
}

func (f *FetchStmt) Pos() Position { return f.P }
func (f *FetchStmt) plStmtNode()   {}

type CloseStmt struct {
	Cursor string
	P      Position
}

func (c *CloseStmt) Pos() Position { return c.P }
func (c *CloseStmt) plStmtNode()   {}

// SignalStmt — SIGNAL SQLSTATE 'XXXXX' [SET MESSAGE_TEXT = '...'];
type SignalStmt struct {
	SQLState   string
	Message    string
	P          Position
}

func (s *SignalStmt) Pos() Position { return s.P }
func (s *SignalStmt) plStmtNode()   {}

// SelectInto — SELECT <select list> INTO var1[,var2,...] FROM ...;
// The select text is captured as raw SQL; the translator passes it through
// the DML body rewriter before emission.
type SelectInto struct {
	Vars     []string // target variables
	// RawQuery is the legacy text-level capture of the reassembled
	// `SELECT <list> INTO <vars> <rest>` statement; consumed by the
	// translator's text pipeline. New code should consult Stmt first
	// and fall back to RawQuery only when Stmt is nil (e.g. when the
	// DML parser couldn't parse the body).
	RawQuery string
	// Stmt is the typed SELECT body (without the INTO clause). Populated
	// by the Phase 1.9 migration via parseSelectFromText. nil when the
	// re-parse failed; consumers must handle both forms during the
	// transition window.
	Stmt *SelectStmt
	P    Position
}

func (s *SelectInto) Pos() Position { return s.P }
func (s *SelectInto) plStmtNode()   {}

// RawSQL — any DML / DDL statement we pass through to PG verbatim (after the
// body rewriter has normalized identifiers and function names).
type RawSQL struct {
	Text string // statement without trailing ';'
	P    Position
}

func (r *RawSQL) Pos() Position { return r.P }
func (r *RawSQL) plStmtNode()   {}

// ---------------------------------------------------------------------------
// Oracle / PL/SQL additions
// ---------------------------------------------------------------------------

// CursorForStmt — FOR rec IN {cursor_name|(select ...)} LOOP stmts END LOOP;
type CursorForStmt struct {
	Label      string
	Record     string // record variable name
	CursorName string // non-empty if FOR rec IN cursor_name[(args)]
	CursorArgs []Expr // arguments for parameterized cursor call
	SelectBody string // non-empty if FOR rec IN (SELECT ...)
	Body       []PLStmt
	P          Position
}

func (c *CursorForStmt) Pos() Position { return c.P }
func (c *CursorForStmt) plStmtNode()   {}

// NumericForStmt — FOR i IN [REVERSE] lo .. hi LOOP stmts END LOOP;
type NumericForStmt struct {
	Label   string
	Var     string
	Reverse bool
	Low     Expr
	High    Expr
	Body    []PLStmt
	P       Position
}

func (n *NumericForStmt) Pos() Position { return n.P }
func (n *NumericForStmt) plStmtNode()   {}

// ForallStmt — FORALL i IN lo..hi [SAVE EXCEPTIONS] <dml>;
type ForallStmt struct {
	Var           string
	Low           Expr
	High          Expr
	IndicesOf     string // "" | "INDICES OF <coll>"
	ValuesOf      string // "" | "VALUES OF <coll>"
	SaveExcept    bool
	RawBody       string // the single DML following FORALL
	P             Position
}

func (f *ForallStmt) Pos() Position { return f.P }
func (f *ForallStmt) plStmtNode()   {}

// ExecuteImmediateStmt — EXECUTE IMMEDIATE stmt [INTO vars] [USING args];
type ExecuteImmediateStmt struct {
	SQL   Expr     // the dynamic SQL expression (usually a string literal or concatenation)
	Into  []string // target variable names
	Using []Expr   // bind arguments
	P     Position
}

func (e *ExecuteImmediateStmt) Pos() Position { return e.P }
func (e *ExecuteImmediateStmt) plStmtNode()   {}

// RaiseStmt — RAISE [exception_name]; (re-raise when name is empty)
type RaiseStmt struct {
	// Name is the Oracle exception name (e.g. "fk_violation" from a
	// `RAISE fk_violation;` statement). Empty for a bare RAISE that
	// re-raises the current handler's exception.
	Name string
	// SQLState is the PG SQLSTATE the exception name resolved to via
	// the routine's PRAGMA EXCEPTION_INIT bindings. When non-empty
	// the writer emits `RAISE SQLSTATE '<code>';` instead of the
	// Oracle-shaped `RAISE name;` (which PG plpgsql rejects). Set
	// by VisitOracleExceptionInit before the writer runs; left
	// empty for bare RAISE or for names without a binding (those
	// surface a separate prerequisite).
	SQLState string
	P        Position
}

func (r *RaiseStmt) Pos() Position { return r.P }
func (r *RaiseStmt) plStmtNode()   {}

// ExceptionBlock — the EXCEPTION section of a PL/SQL block.
type ExceptionBlock struct {
	Handlers []ExceptionHandler
	P        Position
}

type ExceptionHandler struct {
	Names []string // exception names, OTHERS = ["OTHERS"]
	Body  []PLStmt
}

// PragmaStmt — PRAGMA directive. Raw text preserved.
type PragmaStmt struct {
	Kind string // AUTONOMOUS_TRANSACTION | EXCEPTION_INIT | ...
	Text string
	P    Position
}

func (p *PragmaStmt) Pos() Position { return p.P }
func (p *PragmaStmt) plDeclNode()   {}
func (p *PragmaStmt) plStmtNode()   {}

// BulkCollectInto — SELECT ... BULK COLLECT INTO vars FROM ...;
// Stored as a marker; the raw query is kept for the body rewriter.
type BulkCollectInto struct {
	Vars     []string
	RawQuery string
	Limit    Expr // optional LIMIT n clause
	P        Position
}

func (b *BulkCollectInto) Pos() Position { return b.P }
func (b *BulkCollectInto) plStmtNode()   {}

// GotoStmt — GOTO label;
type GotoStmt struct {
	Label string
	P     Position
}

func (g *GotoStmt) Pos() Position { return g.P }
func (g *GotoStmt) plStmtNode()   {}

// NullStmt — NULL; (no-op in PL/SQL; becomes empty in PL/pgSQL)
type NullStmt struct{ P Position }

func (n *NullStmt) Pos() Position { return n.P }
func (n *NullStmt) plStmtNode()   {}
