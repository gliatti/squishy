// Package db2 implements an IBM Db2 (LUW + z/OS) DDL + SQL PL parser.
//
// The canonical grammar reference is grammars-v4 (Db2Lexer.g4 / Db2Parser.g4,
// MIT, Michał Lorek 2023), vendored under reference/. This Go code is a
// hand-rolled recursive-descent parser implementing the subset of that
// grammar required by squishy (DDL + routine headers + SQL PL bodies + raw
// DML passthrough). The .g4 files are the source of truth; when the parser
// diverges from them, the .g4 wins and the Go code is updated to match.
// NO ANTLR runtime is linked.
//
// Entry point: Parse(src string) (stmts []ast.Stmt, errs ErrorList).
package db2

import (
	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Dialect implements dialects.Dialect for IBM Db2.
type Dialect struct {
	name string
	kind dialects.Kind
}

func New(kind dialects.Kind, name string) *Dialect {
	return &Dialect{kind: kind, name: name}
}

func (d *Dialect) Kind() dialects.Kind { return d.kind }
func (d *Dialect) Name() string        { return d.name }

func (d *Dialect) Parse(src string) ([]ast.Stmt, error) {
	stmts, errs := Parse(src)
	if len(errs) > 0 {
		return stmts, errs
	}
	return stmts, nil
}

// init registers DB2 LUW 11.5 ("db2", "db2luw") and DB2 for z/OS ("db2zos")
// against the same parser. z/OS DDL and SQL PL are a strict subset of LUW —
// the divergence is in catalog views (SYSIBM.* vs SYSCAT.*) and a few types
// (no native BOOLEAN, no TIMESTAMP WITH TIME ZONE on z/OS); both are handled
// downstream by the type mapper and the dataxfer SourceDialect.
func init() {
	dialects.Register("db2", New(dialects.KindDB2, "IBM DB2 11.5 (LUW)"))
	dialects.Register("db2luw", New(dialects.KindDB2, "IBM DB2 11.5 (LUW)"))
	dialects.Register("db2zos", New(dialects.KindDB2zOS, "IBM DB2 for z/OS"))
}
