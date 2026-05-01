// Package dialects defines the registry of SQL dialects supported by squishy.
//
// Each supported source SGBD × version is implemented as its own subpackage
// (e.g. dialects/mysql, dialects/mariadb, dialects/oracle12c). Each subpackage
// vendors its canonical grammar reference under reference/ (typically one or
// more .g4 files from https://github.com/antlr/grammars-v4, with original
// license and attribution preserved) and exposes a Dialect implementation.
//
// The .g4 files are the grammar source of truth and are kept as specification
// documents. squishy does NOT embed an ANTLR runtime; each dialect ships a
// hand-rolled recursive-descent Go parser implementing the subset of rules
// actually needed for migration (DDL, routine headers, selected DML).
package dialects

import (
	"fmt"
	"sync"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Kind identifies a source or target database engine family.
type Kind string

const (
	KindMySQL    Kind = "mysql"
	KindMariaDB  Kind = "mariadb"
	KindOracle   Kind = "oracle" // alias 23ai historique
	KindOracle19 Kind = "oracle19"
	KindDB2      Kind = "db2"    // DB2 LUW 11.5
	KindDB2zOS   Kind = "db2zos" // DB2 for z/OS
	KindPostgres Kind = "postgres"
)

// IsOracle reports whether k is any Oracle variant (23ai, 19c, …). Use this
// wherever Oracle-family behavior applies regardless of version (PL/SQL
// translation, identifier folding, ALL_* introspection quirks). Prefer
// equality with a specific KindOracleXX constant only for version-gated
// features (VECTOR, native BOOLEAN/JSON in 23ai, etc.).
func IsOracle(k Kind) bool {
	return k == KindOracle || k == KindOracle19
}

// IsMySQLFamily reports whether k is MySQL or MariaDB (they share the parser
// and type mapper; version-specific quirks are gated on Kind equality).
func IsMySQLFamily(k Kind) bool {
	return k == KindMySQL || k == KindMariaDB
}

// IsDB2Family reports whether k is any DB2 variant. The two share the SQL PL
// dialect and SYSCAT/SYSIBM catalog shape; they diverge on type availability
// (BOOLEAN, TIMESTAMP WITH TIME ZONE, DECFLOAT semantics) which is gated in
// the type mapper via Kind equality.
func IsDB2Family(k Kind) bool {
	return k == KindDB2 || k == KindDB2zOS
}

// Dialect is the integration contract for a supported SQL dialect.
type Dialect interface {
	// Kind returns the engine family.
	Kind() Kind

	// Name returns a human-readable identifier, e.g. "MySQL 8.0".
	Name() string

	// Parse parses a DDL/routine script into AST statements.
	Parse(src string) ([]ast.Stmt, error)
}

// Registry holds the set of enabled dialects.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Dialect
}

// Default registry populated via Register() from dialect subpackages.
var defaultRegistry = &Registry{byID: map[string]Dialect{}}

// Register makes a dialect available under id (case-insensitive, e.g. "mysql",
// "mariadb", "oracle12c"). Panics on duplicate registration.
func Register(id string, d Dialect) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	if _, dup := defaultRegistry.byID[id]; dup {
		panic("dialects: duplicate registration for " + id)
	}
	defaultRegistry.byID[id] = d
}

// Get returns the dialect registered under id, or an error.
func Get(id string) (Dialect, error) {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	d, ok := defaultRegistry.byID[id]
	if !ok {
		return nil, fmt.Errorf("unknown dialect %q", id)
	}
	return d, nil
}

// List returns the ids of all registered dialects (useful for UI).
func List() []string {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	out := make([]string, 0, len(defaultRegistry.byID))
	for id := range defaultRegistry.byID {
		out = append(out, id)
	}
	return out
}
