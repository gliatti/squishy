package mysql

import (
	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Dialect implements dialects.Dialect for MySQL.
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

// init registers MySQL and MariaDB under their canonical ids. MariaDB shares
// the same parser — dialect-specific quirks (AS OF SYSTEM_TIME, PRAGMA-like
// options) can be layered via a distinct file when needed.
func init() {
	dialects.Register("mysql",   New(dialects.KindMySQL,   "MySQL 8.0"))
	dialects.Register("mariadb", New(dialects.KindMariaDB, "MariaDB 10.x"))
}
