// Package ast defines the MySQL DDL AST produced by internal/sqlparse.
// All node types implement Node; statement-like nodes additionally implement Stmt.
package ast

type Position struct {
	Line   int
	Col    int
	Offset int
}

type Node interface {
	Pos() Position
}

type Stmt interface {
	Node
	stmtNode()
}

type DataType interface {
	Node
	dataTypeNode()
	TypeName() string // MySQL canonical type name, uppercase (e.g. "VARCHAR")
}

type Expr interface {
	Node
	exprNode()
}

type TableConstraint interface {
	Node
	tableConstraintNode()
}
