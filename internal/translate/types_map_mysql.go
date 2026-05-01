package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func init() {
	RegisterTypeMapper(dialects.KindMySQL, mapMySQLType)
	RegisterTypeMapper(dialects.KindMariaDB, mapMySQLType)
}

// mapMySQLType is the MySQL/MariaDB type-mapping rule. Historically the only
// mapper in squishy — kept as the default fallback.
func mapMySQLType(t ast.DataType, colName string, caps Caps) typeResult {
	switch x := t.(type) {

	case *ast.IntType:
		return mapMySQLInt(x, colName)

	case *ast.FloatType:
		return mapMySQLFloat(x)

	case *ast.DecimalType:
		// DECIMAL(P,S) maps to NUMERIC(P,S); defaults in MySQL are (10,0)
		if x.HasPrec {
			return typeResult{PG: fmt.Sprintf("NUMERIC(%d,%d)", x.Precision, x.Scale)}
		}
		return typeResult{PG: "NUMERIC(10,0)"}

	case *ast.BitType:
		if x.Width <= 1 {
			return typeResult{PG: "BOOLEAN", Note: "BIT(1)→BOOLEAN"}
		}
		nBytes := (x.Width + 7) / 8
		return typeResult{
			PG:    "BYTEA",
			Check: fmt.Sprintf("octet_length(%s) = %d", quoteIdent(colName), nBytes),
			Note:  fmt.Sprintf("BIT(%d) → BYTEA + CHECK octet_length=%d", x.Width, nBytes),
		}

	case *ast.CharType:
		l := x.Length
		if !x.HasLength {
			l = 1
		}
		if strings.EqualFold(x.Name, "VARCHAR") {
			return typeResult{PG: fmt.Sprintf("VARCHAR(%d)", l), Note: mapMySQLCollation(x.Collation)}
		}
		return typeResult{PG: fmt.Sprintf("CHAR(%d)", l), Note: mapMySQLCollation(x.Collation)}

	case *ast.TextType:
		return typeResult{PG: "TEXT", Note: "TINYTEXT/MEDIUMTEXT/LONGTEXT → TEXT (PG has no size tier)"}

	case *ast.BlobType:
		return typeResult{PG: "BYTEA", Note: "BLOB family → BYTEA (PG is size-agnostic)"}

	case *ast.BinaryType:
		if strings.EqualFold(x.Name, "BINARY") {
			return typeResult{
				PG:    "BYTEA",
				Check: fmt.Sprintf("octet_length(%s) = %d", quoteIdent(colName), x.Length),
				Note:  "fixed-width BINARY(n) → BYTEA + CHECK octet_length",
			}
		}
		return typeResult{PG: "BYTEA", Note: "VARBINARY → BYTEA"}

	case *ast.EnumType:
		vals := make([]string, 0, len(x.Values))
		for _, v := range x.Values {
			vals = append(vals, "'"+strings.ReplaceAll(v, "'", "''")+"'")
		}
		return typeResult{
			PG:    "TEXT",
			Check: fmt.Sprintf("%s IN (%s)", quoteIdent(colName), strings.Join(vals, ",")),
			Note:  "ENUM → TEXT + CHECK (v1: CHECK; v2 option: CREATE TYPE enum)",
		}

	case *ast.SetType:
		vals := make([]string, 0, len(x.Values))
		for _, v := range x.Values {
			vals = append(vals, "'"+strings.ReplaceAll(v, "'", "''")+"'")
		}
		return typeResult{
			PG:    "TEXT[]",
			Check: fmt.Sprintf("%s <@ ARRAY[%s]::text[]", quoteIdent(colName), strings.Join(vals, ",")),
			Note:  "SET → TEXT[] + CHECK subset",
		}

	case *ast.JSONType:
		return typeResult{PG: "JSONB", Note: "JSON → JSONB"}

	case *ast.DateType:
		return typeResult{PG: "DATE"}

	case *ast.TimeType:
		if x.Fsp > 0 {
			return typeResult{PG: fmt.Sprintf("TIME(%d)", x.Fsp)}
		}
		return typeResult{PG: "TIME"}

	case *ast.DateTimeType:
		if x.Fsp > 0 {
			return typeResult{PG: fmt.Sprintf("TIMESTAMP(%d)", x.Fsp), Note: "DATETIME → TIMESTAMP (no TZ)"}
		}
		return typeResult{PG: "TIMESTAMP", Note: "DATETIME → TIMESTAMP (no TZ)"}

	case *ast.TimestampType:
		if x.Fsp > 0 {
			return typeResult{PG: fmt.Sprintf("TIMESTAMPTZ(%d)", x.Fsp), Note: "TIMESTAMP → TIMESTAMPTZ"}
		}
		return typeResult{PG: "TIMESTAMPTZ", Note: "TIMESTAMP → TIMESTAMPTZ"}

	case *ast.YearType:
		return typeResult{
			PG:    "SMALLINT",
			Check: fmt.Sprintf("%s BETWEEN 1901 AND 2155", quoteIdent(colName)),
			Note:  "YEAR → SMALLINT + CHECK 1901..2155",
		}

	case *ast.SpatialType:
		if caps.HasPostGIS {
			subtype := strings.ToUpper(x.Name)
			if subtype == "GEOMETRY" {
				return typeResult{PG: "geometry", Note: "GEOMETRY → PostGIS geometry"}
			}
			return typeResult{
				PG:   fmt.Sprintf("geometry(%s)", subtype),
				Note: fmt.Sprintf("%s → PostGIS geometry(%s)", x.Name, subtype),
			}
		}
		return typeResult{
			PG:      "TEXT",
			Note:    x.Name + " → TEXT (PostGIS not installed on target)",
			Warning: "spatial type " + x.Name + " requires PostGIS — mapped to TEXT placeholder; migrate data manually",
		}

	case *ast.VectorType:
		// MariaDB 11.7+ VECTOR — only pgvector preserves operator + indexing
		// semantics, so the type is always emitted as pgvector's `vector(N)`
		// and the missing extension is a blocking prerequisite.
		var pg string
		if x.HasDim {
			pg = fmt.Sprintf("vector(%d)", x.Dim)
		} else {
			pg = "vector"
		}
		if !caps.HasPgVector {
			return typeResult{
				PG:              pg,
				Note:            "VECTOR → vector (pgvector required)",
				Warning:         "VECTOR column requires the pgvector extension — install it on the target before running the migration",
				WarningSeverity: SeverityBlocking,
			}
		}
		return typeResult{PG: pg, Note: "VECTOR → vector (pgvector)"}
	}
	return typeResult{} // caller fills in TEXT + warning
}

func mapMySQLInt(x *ast.IntType, colName string) typeResult {
	if strings.EqualFold(x.Name, "TINYINT") && x.HasWidth && x.Width == 1 && !x.Unsigned {
		return typeResult{PG: "BOOLEAN", Note: "TINYINT(1) → BOOLEAN (MySQL convention)"}
	}
	if x.Unsigned {
		// MySQL's UNSIGNED is a full type-system constraint: the column may
		// never hold a negative value. Widening alone preserves the range,
		// but a value like -1 would be silently accepted on insert. Add a
		// CHECK to mirror MySQL's enforcement.
		nonNegCheck := fmt.Sprintf("%s >= 0", quoteIdent(colName))
		zfNote := zerofillNote(x.Zerofill)
		switch strings.ToUpper(x.Name) {
		case "TINYINT":
			return typeResult{PG: "SMALLINT", Check: nonNegCheck,
				Note: "TINYINT UNSIGNED → SMALLINT + CHECK ≥ 0 (range widened)" + zfNote}
		case "SMALLINT":
			return typeResult{PG: "INTEGER", Check: nonNegCheck,
				Note: "SMALLINT UNSIGNED → INTEGER + CHECK ≥ 0" + zfNote}
		case "MEDIUMINT":
			return typeResult{PG: "INTEGER", Check: nonNegCheck,
				Note: "MEDIUMINT UNSIGNED → INTEGER + CHECK ≥ 0" + zfNote}
		case "INT", "INTEGER":
			return typeResult{PG: "BIGINT", Check: nonNegCheck,
				Note: "INT UNSIGNED → BIGINT + CHECK ≥ 0 (PG INTEGER tops out at 2^31-1)" + zfNote}
		case "BIGINT":
			return typeResult{PG: "NUMERIC(20,0)", Check: nonNegCheck,
				Note: "BIGINT UNSIGNED → NUMERIC(20,0) + CHECK ≥ 0 (full 0..2^64-1 range preserved; PG BIGINT would truncate)" + zfNote}
		}
	}
	switch strings.ToUpper(x.Name) {
	case "TINYINT":
		return typeResult{PG: "SMALLINT", Note: "TINYINT → SMALLINT"}
	case "SMALLINT":
		return typeResult{PG: "SMALLINT"}
	case "MEDIUMINT":
		return typeResult{PG: "INTEGER", Note: "MEDIUMINT → INTEGER"}
	case "INT", "INTEGER":
		return typeResult{PG: "INTEGER"}
	case "BIGINT":
		return typeResult{PG: "BIGINT"}
	}
	return typeResult{PG: "INTEGER"}
}

// zerofillNote returns a trailing remark for the type-mapping note when the
// source column carried ZEROFILL. ZEROFILL is purely a display attribute in
// MySQL (left-pads numeric output with zeroes up to the declared width); PG
// has no equivalent and applications should format on read instead.
func zerofillNote(zf bool) string {
	if !zf {
		return ""
	}
	return " (ZEROFILL display attribute dropped — format on read)"
}

func mapMySQLFloat(x *ast.FloatType) typeResult {
	switch strings.ToUpper(x.Name) {
	case "FLOAT":
		if x.HasPS {
			return typeResult{PG: fmt.Sprintf("NUMERIC(%d,%d)", x.Precision, x.Scale),
				Note: "FLOAT(p,s) fixed-point → NUMERIC"}
		}
		return typeResult{PG: "REAL"}
	case "DOUBLE", "REAL":
		return typeResult{PG: "DOUBLE PRECISION"}
	}
	return typeResult{PG: "DOUBLE PRECISION"}
}

func mapMySQLCollation(coll string) string {
	switch strings.ToLower(coll) {
	case "":
		return ""
	case "utf8mb4_bin":
		return "collation utf8mb4_bin → \"C\" (byte-wise)"
	case "utf8mb4_unicode_ci", "utf8mb4_0900_ai_ci":
		return "case-insensitive collation; PG default uses DB collation — consider CITEXT"
	}
	return "collation " + coll + " preserved at DB level"
}
