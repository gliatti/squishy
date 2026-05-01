package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// typeResult carries the PG column type plus any side-effects produced by the
// mapping (check constraints, notes, warnings).
type typeResult struct {
	PG      string // PG type expression, e.g. "NUMERIC(12,4)"
	Check   string // optional column-level CHECK expression (bare, no CHECK(...))
	Note    string // short note for TypeMapping.Note / Explanation.Reason
	Warning string // non-empty means Warnings[] should carry a message
	// WarningSeverity controls how the emitted warning is categorized.
	// Empty → blocking (historical default). "info" → intrinsic target-engine
	// limitation the user only needs to be aware of (e.g. BFILE/ROWID/UROWID
	// have no PG equivalent; the mapping is best-effort and nothing the user
	// can "fix"). Mirrors Prerequisite.Severity.
	WarningSeverity Severity
}

// Caps describes target PG capabilities consulted by the type mapper. Exposed
// so that extension-gated decisions can be taken inline.
type Caps struct {
	HasPostGIS bool
	HasPgCron  bool
	HasPgVector bool
	// UserTypes is the set of user-defined types that the same migration
	// emits earlier in the DDL stream (composite types from Oracle OBJECT,
	// arrays from VARRAY/TABLE OF). Looking it up here lets the column-type
	// resolver reach the migrated PG name instead of falling back to TEXT
	// with a "user type not translated" warning.
	UserTypes map[string]UserTypeRef
}

// UserTypeRef describes how a source-side user type was rendered on the PG
// side, so the type mapper can swap a column reference into the right PG
// expression (e.g. composite, array of scalar, array of composite).
type UserTypeRef struct {
	Kind       string // "composite" | "array_scalar" | "array_composite" | "unsupported"
	Schema     string // PG target schema (e.g. "mig"), empty for plain arrays
	Name       string // PG type name (lowercased)
	ElemPG     string // for array_*: rendered element type ("varchar(25)" or "\"mig\".\"foo\"")
}

// TypeMapper converts a source-dialect AST data type into a PostgreSQL type
// expression. Each source dialect registers one in init().
type TypeMapper func(t ast.DataType, colName string, caps Caps) typeResult

// Registry of per-dialect type mappers. Populated via RegisterTypeMapper()
// from types_map_mysql.go, types_map_oracle.go, etc.
var typeMappers = map[dialects.Kind]TypeMapper{}

// RegisterTypeMapper wires a per-dialect type mapping function. Called from
// init() in types_map_<dialect>.go.
func RegisterTypeMapper(k dialects.Kind, fn TypeMapper) {
	typeMappers[k] = fn
}

// MapType dispatches to the registered mapper for the given source kind.
// An empty kind or one without a registered mapper falls back to MySQL,
// which historically was squishy's only supported source dialect.
func MapType(kind dialects.Kind, t ast.DataType, colName string, caps Caps) typeResult {
	fn, ok := typeMappers[kind]
	if !ok {
		fn = typeMappers[dialects.KindMySQL]
	}
	if fn == nil {
		return typeResult{PG: "TEXT", Warning: "no type mapper registered"}
	}
	r := fn(t, colName, caps)
	if r.PG == "" {
		r.PG = "TEXT"
		if r.Warning == "" {
			r.Warning = fmt.Sprintf("unknown %s type, defaulted to TEXT", kindLabel(kind))
		}
	}
	return r
}

func kindLabel(k dialects.Kind) string {
	if k == "" {
		return "MySQL"
	}
	return strings.Title(string(k))
}

// quoteIdent returns a PG-safe quoted identifier (always double-quoted — safe
// across reserved words and case preservation).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
