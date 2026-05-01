package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func init() {
	RegisterTypeMapper(dialects.KindDB2, mapDB2Type)
	RegisterTypeMapper(dialects.KindDB2zOS, mapDB2zOSType)
}

// mapDB2Type rewrites DB2 LUW AST data types into PostgreSQL types.
//
// Rules of thumb:
//   - SMALLINT/INTEGER/BIGINT → identical PG types.
//   - DECIMAL(p,s) / NUMERIC(p,s) → NUMERIC(p,s); bare DECIMAL → NUMERIC.
//   - DECFLOAT(16|34) → NUMERIC + info warning (no IEEE 754 decimal in PG).
//   - REAL / DOUBLE → REAL / DOUBLE PRECISION; FLOAT(p) routed by p.
//   - CHAR(n) / VARCHAR(n) → identical PG types; FOR BIT DATA → BYTEA.
//   - GRAPHIC / VARGRAPHIC / DBCLOB / LONG VARGRAPHIC → TEXT (PG est utf8 natif;
//     les UCS-2 round-trippent via la conversion d'encodage côté driver).
//   - CLOB → TEXT, BLOB → BYTEA, BINARY/VARBINARY → BYTEA.
//   - DATE/TIME/TIMESTAMP → identical PG types.
//   - TIMESTAMP WITH TIME ZONE (LUW) → TIMESTAMPTZ.
//   - XML → xml (PG built-in).
//   - BOOLEAN (LUW 11+) → BOOLEAN.
//   - ROWID → BIGINT DEFAULT nextval(seq) — surrogate (DB2 ROWID values are dropped on copy).
func mapDB2Type(t ast.DataType, colName string, caps Caps) typeResult {
	switch x := t.(type) {

	// ---- DB2-specific nodes ----

	case *ast.DB2DecFloatType:
		w := x.Width
		if !x.HasWidth || w == 0 {
			w = 34
		}
		return typeResult{
			PG:              "NUMERIC",
			Note:            fmt.Sprintf("DECFLOAT(%d) → NUMERIC (PG has no IEEE 754-2008 decimal floating point; arbitrary precision preserved, decimal-rounding semantics best-effort)", w),
			WarningSeverity: SeverityInfo,
		}

	case *ast.DB2GraphicType:
		// GRAPHIC(n) / VARGRAPHIC(n) → TEXT. PG is UTF-8 native; lengths in
		// DB2 are double-byte characters, the round-trip preserves data.
		if x.HasLength && (x.Name == "GRAPHIC" || x.Name == "VARGRAPHIC") {
			return typeResult{
				PG:              fmt.Sprintf("VARCHAR(%d)", x.Length),
				Note:            fmt.Sprintf("%s(%d) → VARCHAR(%d) (UCS-2 → utf8; PG est unicode-natif)", x.Name, x.Length, x.Length),
				WarningSeverity: SeverityInfo,
			}
		}
		return typeResult{
			PG:              "TEXT",
			Note:            x.Name + " → TEXT (UCS-2 → utf8 PG est unicode-natif)",
			WarningSeverity: SeverityInfo,
		}

	case *ast.DB2ForBitDataType:
		// FOR BIT DATA → BYTEA. The wrapped CharType length is informational.
		var inner string
		if c, ok := x.Inner.(*ast.CharType); ok && c.HasLength {
			inner = fmt.Sprintf("%s(%d)", c.Name, c.Length)
		} else {
			inner = "CHAR/VARCHAR"
		}
		return typeResult{
			PG:   "BYTEA",
			Note: inner + " FOR BIT DATA → BYTEA",
		}

	case *ast.DB2RowIDType:
		// DB2 ROWID is an opaque per-row identifier — same semantic as Oracle
		// ROWID. We emit a BIGINT surrogate filled by a sequence; copy
		// pipeline will drop the source values.
		return typeResult{
			PG:              "BIGINT",
			Note:            "ROWID → BIGINT DEFAULT nextval(seq) (surrogate row identifier; DB2 ROWID values dropped on copy)",
			Warning:         "DB2 ROWID has no PG equivalent — surrogate BIGINT emitted with a backing sequence; review application code that reads ROWID values",
			WarningSeverity: SeverityInfo,
		}

	case *ast.DB2TimestampTZType:
		fsp := x.Fsp
		if !x.HasFsp {
			fsp = 6
		}
		if fsp >= 0 && fsp <= 6 {
			return typeResult{
				PG:   fmt.Sprintf("TIMESTAMPTZ(%d)", fsp),
				Note: "TIMESTAMP WITH TIME ZONE → TIMESTAMPTZ",
			}
		}
		// PG caps fractional precision at 6; surface info warning.
		return typeResult{
			PG:              "TIMESTAMPTZ(6)",
			Note:            fmt.Sprintf("TIMESTAMP(%d) WITH TIME ZONE → TIMESTAMPTZ(6) (PG caps fractional precision at 6)", fsp),
			WarningSeverity: SeverityInfo,
		}

	case *ast.DB2BooleanType:
		return typeResult{PG: "BOOLEAN", Note: "BOOLEAN → BOOLEAN"}

	case *ast.DB2XmlType:
		return typeResult{PG: "xml", Note: "XML → xml (PG built-in)"}

	// ---- Generic AST nodes shared with other dialects ----

	case *ast.IntType:
		switch strings.ToUpper(x.Name) {
		case "SMALLINT":
			return typeResult{PG: "SMALLINT"}
		case "INTEGER", "INT":
			return typeResult{PG: "INTEGER"}
		case "BIGINT":
			return typeResult{PG: "BIGINT"}
		}

	case *ast.DecimalType:
		if x.HasPrec {
			if x.Scale == 0 {
				return typeResult{PG: fmt.Sprintf("NUMERIC(%d,0)", x.Precision)}
			}
			return typeResult{PG: fmt.Sprintf("NUMERIC(%d,%d)", x.Precision, x.Scale)}
		}
		return typeResult{PG: "NUMERIC", Note: "DECIMAL → NUMERIC (arbitrary precision)"}

	case *ast.FloatType:
		switch strings.ToUpper(x.Name) {
		case "REAL":
			return typeResult{PG: "REAL"}
		case "DOUBLE":
			return typeResult{PG: "DOUBLE PRECISION"}
		case "FLOAT":
			if x.HasPS && x.Precision > 0 && x.Precision <= 24 {
				return typeResult{PG: "REAL", Note: fmt.Sprintf("FLOAT(%d) → REAL", x.Precision)}
			}
			return typeResult{PG: "DOUBLE PRECISION", Note: "FLOAT → DOUBLE PRECISION"}
		}

	case *ast.CharType:
		switch strings.ToUpper(x.Name) {
		case "CHAR":
			if x.HasLength {
				return typeResult{PG: fmt.Sprintf("CHAR(%d)", x.Length)}
			}
			return typeResult{PG: "CHAR(1)"}
		case "VARCHAR":
			if x.HasLength {
				return typeResult{PG: fmt.Sprintf("VARCHAR(%d)", x.Length)}
			}
			return typeResult{PG: "VARCHAR"}
		case "LONGVARCHAR":
			return typeResult{PG: "TEXT", Note: "LONG VARCHAR → TEXT"}
		}

	case *ast.ClobType:
		if x.Long {
			return typeResult{PG: "TEXT", Note: "LONG (deprecated) → TEXT"}
		}
		if x.National {
			return typeResult{PG: "TEXT", Note: "NCLOB → TEXT"}
		}
		return typeResult{PG: "TEXT", Note: "CLOB → TEXT"}

	case *ast.BlobType:
		return typeResult{PG: "BYTEA", Note: "BLOB → BYTEA"}

	case *ast.BinaryType:
		// BINARY(n) and VARBINARY(n) → BYTEA. PG has no fixed-length binary.
		// Add a CHECK on octet_length to preserve the upper bound.
		if x.Length > 0 {
			return typeResult{
				PG:    "BYTEA",
				Check: fmt.Sprintf("octet_length(%s) <= %d", quoteIdent(colName), x.Length),
				Note:  fmt.Sprintf("%s(%d) → BYTEA + CHECK octet_length", x.Name, x.Length),
			}
		}
		return typeResult{PG: "BYTEA", Note: x.Name + " → BYTEA"}

	case *ast.DateType:
		return typeResult{PG: "DATE"}

	case *ast.TimeType:
		return typeResult{PG: "TIME(0) WITHOUT TIME ZONE"}

	case *ast.TimestampType:
		fsp := x.Fsp
		if fsp <= 0 {
			fsp = 6
		}
		if fsp > 6 {
			return typeResult{
				PG:              "TIMESTAMP(6)",
				Note:            fmt.Sprintf("TIMESTAMP(%d) → TIMESTAMP(6) (PG caps fractional precision at 6)", x.Fsp),
				WarningSeverity: SeverityInfo,
			}
		}
		return typeResult{PG: fmt.Sprintf("TIMESTAMP(%d)", fsp)}

	case *ast.UserDefinedType:
		// Delegate to the generic resolver — Caps.UserTypes may have a match
		// emitted earlier in the migration (CREATE [DISTINCT] TYPE → DOMAIN).
		key := strings.ToLower(x.Name)
		if x.Schema != "" {
			key = strings.ToLower(x.Schema) + "." + key
		}
		if ref, ok := caps.UserTypes[key]; ok {
			switch ref.Kind {
			case "composite":
				if ref.Schema != "" {
					return typeResult{PG: quoteIdent(ref.Schema) + "." + quoteIdent(ref.Name)}
				}
				return typeResult{PG: quoteIdent(ref.Name)}
			case "array_scalar", "array_composite":
				if ref.ElemPG != "" {
					return typeResult{PG: ref.ElemPG + "[]"}
				}
			}
		}
		return typeResult{
			PG:              "TEXT",
			Warning:         fmt.Sprintf("user-defined type %q has no PG translation; defaulted to TEXT", x.Name),
			WarningSeverity: SeverityInfo,
		}
	}
	return typeResult{}
}

// mapDB2zOSType is mapDB2Type with z/OS-specific overrides:
//   - BOOLEAN → SMALLINT + CHECK col IN (0,1) (z/OS has no native BOOLEAN).
//   - TIMESTAMP WITH TIME ZONE → TIMESTAMP(n) + info warning (z/OS lacks TZ).
func mapDB2zOSType(t ast.DataType, colName string, caps Caps) typeResult {
	switch x := t.(type) {
	case *ast.DB2BooleanType:
		return typeResult{
			PG:    "SMALLINT",
			Check: fmt.Sprintf("%s IN (0,1)", quoteIdent(colName)),
			Note:  "z/OS BOOLEAN absent → SMALLINT(0,1)",
		}
	case *ast.DB2TimestampTZType:
		fsp := x.Fsp
		if !x.HasFsp {
			fsp = 6
		}
		if fsp > 6 {
			fsp = 6
		}
		return typeResult{
			PG:              fmt.Sprintf("TIMESTAMP(%d)", fsp),
			Warning:         "z/OS does not support TIMESTAMP WITH TIME ZONE; precision preserved, TZ semantics dropped",
			WarningSeverity: SeverityInfo,
		}
	}
	return mapDB2Type(t, colName, caps)
}
