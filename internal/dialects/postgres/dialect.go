package postgres

import (
	"errors"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Dialect registers PostgreSQL for the squishy dialect registry. For v1 it is
// used as a target engine only — the Parse method is a stub that can be
// filled in later as the PostgreSQL parser (matching reference/PostgreSQLParser.g4)
// grows. The emitter (ast.go + writer.go) is the active surface today.
type Dialect struct {
	name string
	kind dialects.Kind
}

func New(kind dialects.Kind, name string) *Dialect {
	return &Dialect{kind: kind, name: name}
}

func (d *Dialect) Kind() dialects.Kind { return d.kind }
func (d *Dialect) Name() string        { return d.name }

// Parse is not implemented for v1: squishy does not (yet) need to read
// PostgreSQL DDL from the source side. When PG is introduced as a source
// engine we'll implement a parser here that follows reference/PostgreSQLParser.g4.
func (d *Dialect) Parse(src string) ([]ast.Stmt, error) {
	return nil, errors.New("postgres parser: not implemented (v1 uses PG only as target)")
}

func init() {
	dialects.Register("postgres", New(dialects.KindPostgres, "PostgreSQL 17"))
}
