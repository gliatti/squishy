package ast

// ---------------------------------------------------------------------------
// Microsoft SQL Server (T-SQL) nodes.
//
// The T-SQL parser (dialects/tsql) maps onto the generic nodes wherever the
// semantics match; the nodes below cover the constructs that have no
// generic counterpart. Every expression / statement slot they carry is
// reached by Rewrite and Walk.
// ---------------------------------------------------------------------------

// CollateExpr — `expr COLLATE collation_name`. T-SQL collations have no
// PG counterpart (PG collations are named differently and the default
// T-SQL collations are case-insensitive), so translators usually unwrap
// it to Expr and report the collation.
type CollateExpr struct {
	Expr      Expr
	Collation string
	P         Position
}

func (e *CollateExpr) Pos() Position { return e.P }
func (e *CollateExpr) exprNode()     {}

// MethodCall — a CLR type method call: `col.GetAncestor(1)`,
// `@h.ToString()`, `geo.STDistance(@p)` (Target set), or a static method
// `hierarchyid::GetRoot()` / `geography::Point(…)` (StaticType set,
// Target nil). Method keeps its source spelling. XML data-type methods
// (value / query / exist / nodes / modify) are not MethodCalls: their
// XQuery arguments are opaque, so the parser keeps them as a RawExpr
// with Kind "XML_METHOD".
type MethodCall struct {
	Target     Expr
	StaticType string
	Method     string
	Args       []Expr
	P          Position
}

func (e *MethodCall) Pos() Position { return e.P }
func (e *MethodCall) exprNode()     {}

// NamedArg — an argument with a name or a marker:
//
//	EXEC p @x = 1            → Name "@x", Value 1
//	EXEC p @y OUTPUT         → Value @y, Output true
//	EXEC p @x = @y OUTPUT    → Name "@x", Value @y, Output true
//	CONTAINSTABLE(t, *, @s, LANGUAGE @l) → Name "LANGUAGE", Value @l
//
// Name keeps the @ of a parameter name.
type NamedArg struct {
	Name   string
	Value  Expr
	Output bool
	P      Position
}

func (e *NamedArg) Pos() Position { return e.P }
func (e *NamedArg) exprNode()     {}

// FromFunc — a table-valued function in a FROM clause / APPLY:
// `dbo.ufnGetContactInformation(@id) AS c`, `OPENJSON(@j) WITH (…)`,
// `CONTAINSTABLE(t, *, @s) AS k`, or an XML `x.nodes('/a') AS T(c)` (Func
// is then a RawExpr with Kind "XML_METHOD"). Cols is the optional column
// alias list.
type FromFunc struct {
	Func  Expr
	Alias string
	Cols  []string
	P     Position
}

func (f *FromFunc) Pos() Position { return f.P }
func (f *FromFunc) fromItemNode() {}

// FromPivot — T-SQL `source PIVOT (agg(col) FOR pivot_col IN ([a], [b]))
// AS alias` or `source UNPIVOT (value_col FOR name_col IN ([a], [b])) AS
// alias`. For PIVOT, Agg is the aggregate call and ForCol the pivot
// column; for UNPIVOT (Unpivot true) ValueCol is the produced value column
// and ForCol the produced name column. InCols are the bracketed column
// names of the IN list.
type FromPivot struct {
	Source   FromItem
	Unpivot  bool
	Agg      Expr
	ValueCol string
	ForCol   string
	InCols   []string
	Alias    string
	P        Position
}

func (f *FromPivot) Pos() Position { return f.P }
func (f *FromPivot) fromItemNode() {}

// FromValues — a table value constructor used as a source:
// `(VALUES (1, 'a'), (2, 'b')) AS t (id, name)`.
type FromValues struct {
	Rows  [][]Expr
	Alias string
	Cols  []string
	P     Position
}

func (f *FromValues) Pos() Position { return f.P }
func (f *FromValues) fromItemNode() {}

// CommentStmt — an `MS_Description` extended property, the T-SQL way of
// commenting an object:
//
//	EXEC sys.sp_addextendedproperty @name = N'MS_Description',
//	     @value = N'text', @level0type = N'SCHEMA', @level0name = N'dbo',
//	     @level1type = N'TABLE', @level1name = N't',
//	     @level2type = N'COLUMN', @level2name = N'c';
//
// ObjectKind is the most specific level type, upper-cased: "DATABASE"
// (no level given), "SCHEMA", "FILEGROUP", a level-1 kind ("TABLE",
// "VIEW", "PROCEDURE", "FUNCTION", "TYPE", "SEQUENCE", …) or a level-2
// kind ("COLUMN", "INDEX", "CONSTRAINT", "TRIGGER", "PARAMETER", …).
// For a SCHEMA level 0, Schema is the schema name, Object the level-1
// name, ParentKind the level-1 type (set for level-2 comments) and Column
// the level-2 name (whatever its kind). For another level-0 type
// (FILEGROUP, ASSEMBLY, USER, a database DDL TRIGGER, …) Object is the
// level-0 name and Schema is empty. Text is the property value; IsNull
// marks an explicit NULL value.
type CommentStmt struct {
	ObjectKind string
	Schema     string
	Object     string
	ParentKind string
	Column     string
	Text       string
	IsNull     bool
	P          Position
}

func (s *CommentStmt) Pos() Position { return s.P }
func (s *CommentStmt) stmtNode()     {}

// CreateSchema — `CREATE SCHEMA name [AUTHORIZATION owner]`. Schema
// elements declared inline in the same statement are not modelled.
type CreateSchema struct {
	Name          string
	Authorization string
	P             Position
}

func (s *CreateSchema) Pos() Position { return s.P }
func (s *CreateSchema) stmtNode()     {}

// DeclareStmt — a T-SQL DECLARE statement in statement position. T-SQL
// declarations may appear anywhere in a body and may initialise their
// variables (`DECLARE @a int = 1, @b varchar(10), @t TABLE (…)`), so the
// parser keeps them in place as a statement holding one PLDecl per
// declared name (*DeclareVar, *DeclareCursor). Translators hoist the
// declarations and turn each initialiser into an assignment at this
// position.
type DeclareStmt struct {
	Decls []PLDecl
	P     Position
}

func (s *DeclareStmt) Pos() Position { return s.P }
func (s *DeclareStmt) plStmtNode()   {}

// TransactionStmt — T-SQL transaction control:
//
//	BEGIN TRAN[SACTION] [name]   → Kind "BEGIN"
//	COMMIT [TRAN[SACTION]|WORK] [name] → Kind "COMMIT"
//	ROLLBACK [TRAN[SACTION]|WORK] [name] → Kind "ROLLBACK" (Name set:
//	                                rollback to a savepoint)
//	SAVE TRAN[SACTION] name      → Kind "SAVE"
//
// PL/pgSQL has no BEGIN TRANSACTION and allows COMMIT / ROLLBACK only in
// procedures, so translators decide per routine kind.
type TransactionStmt struct {
	Kind        string
	Name        string
	Distributed bool
	P           Position
}

func (s *TransactionStmt) Pos() Position { return s.P }
func (s *TransactionStmt) stmtNode()     {}
func (s *TransactionStmt) plStmtNode()   {}

// LabelStmt — a T-SQL GOTO target `label:`.
type LabelStmt struct {
	Label string
	P     Position
}

func (s *LabelStmt) Pos() Position { return s.P }
func (s *LabelStmt) plStmtNode()   {}

// EmbeddedDDL — a DDL statement inside a T-SQL routine body
// (`CREATE TABLE #t (…)`, `DROP TABLE #t`, `CREATE INDEX …`,
// `ALTER TABLE …`). Stmt is the typed DDL node.
type EmbeddedDDL struct {
	Stmt Stmt
	P    Position
}

func (s *EmbeddedDDL) Pos() Position { return s.P }
func (s *EmbeddedDDL) plStmtNode()   {}
