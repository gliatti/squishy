// Package oracle implements an Oracle Database 23ai DDL + PL/SQL parser.
//
// The canonical grammar reference is grammars-v4 (PlSqlLexer.g4 / PlSqlParser.g4,
// MIT), vendored under reference/. This Go code is a hand-rolled recursive-descent
// parser implementing the subset of that grammar required by squishy (DDL +
// routine headers + PL/SQL bodies + raw DML passthrough). The .g4 files are the
// source of truth; when the parser diverges from them, the .g4 wins and the Go
// code is updated to match. NO ANTLR runtime is linked.
//
// Entry point: Parse(src string) (stmts []ast.Stmt, errs ErrorList).
package oracle

import (
	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Dialect implements dialects.Dialect for Oracle Database.
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

// init registers Oracle 23ai and 19c (19.25) against the same parser — 19c
// DDL and PL/SQL are a proper subset of 23ai, and the ALL_* dictionary views
// used by the introspector are stable across versions. All variants share
// KindOracle at the translate/type-mapping layer (the connection kind string
// "oracle" vs "oracle19" is distinct enough for DB routing in the switches).
// 11g/12c remain to be registered in a subsequent release.
func init() {
	dialects.Register("oracle", New(dialects.KindOracle, "Oracle Database 23ai"))
	dialects.Register("oracle23c", New(dialects.KindOracle, "Oracle Database 23ai"))
	dialects.Register("oracle19", New(dialects.KindOracle19, "Oracle Database 19c"))
}
