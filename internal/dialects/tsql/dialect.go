package tsql

import (
	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Dialect implements dialects.Dialect for Microsoft SQL Server (T-SQL).
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

// init registers SQL Server under its canonical id.
func init() {
	dialects.Register("sqlserver", New(dialects.KindSQLServer, "Microsoft SQL Server 2016-2022"))
}
