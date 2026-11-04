package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func init() {
	RegisterTypeMapper(dialects.KindSQLServer, mapTSQLType)
}

// Warning kinds of the SQL Server type mapper (typeResult.WarningKind),
// each folded into its own prerequisite by buildPrerequisites.
const (
	tsqlWarnSpatial    = "type.spatial_no_postgis"
	tsqlWarnHierarchy  = "type.hierarchyid"
	tsqlWarnSQLVariant = "type.sql_variant"
	tsqlWarnRowversion = "type.rowversion"
	tsqlWarnTableType  = "type.table_type"
)

// pgMaxFsp is the largest fractional-seconds precision of PostgreSQL's
// time / timestamp types (microseconds). T-SQL goes to 7 (100 ns).
const pgMaxFsp = 6

// mapTSQLType maps a T-SQL data type (as parsed by dialects/tsql) onto a
// PostgreSQL type. PG type names are written in lower case. Rules:
//
//   - int / bigint / smallint → integer / bigint / smallint; tinyint
//     (unsigned 0..255) → smallint + CHECK (0..255).
//   - bit → boolean; decimal / numeric(p,s) → numeric(p,s);
//     money → numeric(19,4); smallmoney → numeric(10,4);
//     float(n ≤ 24) / real → real, float(n > 24) → double precision.
//   - date → date; time(n) → time(n); datetime → timestamp(3);
//     smalldatetime → timestamp(0); datetime2(n) → timestamp(n);
//     datetimeoffset(n) → timestamptz(n) (n capped at 6).
//   - char / nchar(n) → char(n); varchar / nvarchar(n) → varchar(n),
//     (max) → text; text / ntext → text; sysname → varchar(128).
//   - binary(n) → bytea + CHECK octet_length = n; varbinary / image →
//     bytea.
//   - uniqueidentifier → uuid; xml → xml; rowversion / timestamp → bytea
//     (info: not maintained by PG); hierarchyid → text (the copy reads
//     its canonical '/1/2/' form); sql_variant → text (warning);
//     geography / geometry → PostGIS types when installed, else text +
//     blocking prerequisite.
//   - alias types → the domain the translator emitted for them; table
//     types → their composite type (warning).
func mapTSQLType(t ast.DataType, colName string, caps Caps) typeResult {
	switch x := t.(type) {
	case *ast.IntType:
		switch strings.ToUpper(x.Name) {
		case "TINYINT":
			return typeResult{
				PG:    "smallint",
				Check: fmt.Sprintf("%s BETWEEN 0 AND 255", quoteIdent(colName)),
				Note:  "tinyint (unsigned 0..255) → smallint + CHECK (0..255): PG has no 1-byte integer",
			}
		case "SMALLINT":
			return typeResult{PG: "smallint"}
		case "BIGINT":
			return typeResult{PG: "bigint"}
		default:
			return typeResult{PG: "integer"}
		}

	case *ast.BitType:
		return typeResult{PG: "boolean", Note: "bit → boolean"}

	case *ast.DecimalType:
		p, s := x.Precision, x.Scale
		if !x.HasPrec {
			p, s = 18, 0 // T-SQL default
		}
		return typeResult{PG: fmt.Sprintf("numeric(%d,%d)", p, s)}

	case *ast.FloatType:
		if strings.EqualFold(x.Name, "REAL") || (x.HasPS && x.Precision > 0 && x.Precision <= 24) {
			return typeResult{PG: "real"}
		}
		return typeResult{PG: "double precision"}

	case *ast.DateType:
		return typeResult{PG: "date"}

	case *ast.TimeType:
		fsp, note := tsqlFsp(x.Fsp, "time")
		return typeResult{PG: fmt.Sprintf("time(%d)", fsp), Note: note}

	case *ast.CharType:
		name := strings.ToUpper(x.Name)
		if x.Max {
			return typeResult{PG: "text", Note: strings.ToLower(name) + "(max) → text"}
		}
		l := x.Length
		if !x.HasLength || l <= 0 {
			l = 1 // T-SQL default length in a column definition
		}
		if name == "VARCHAR" || name == "NVARCHAR" {
			return typeResult{PG: fmt.Sprintf("varchar(%d)", l)}
		}
		return typeResult{PG: fmt.Sprintf("char(%d)", l)}

	case *ast.TextType:
		return typeResult{PG: "text", Note: "text → text (deprecated T-SQL large-object type)"}

	case *ast.BinaryType:
		if strings.EqualFold(x.Name, "BINARY") && !x.Max {
			n := x.Length
			if n <= 0 {
				n = 1
			}
			return typeResult{
				PG:    "bytea",
				Check: fmt.Sprintf("octet_length(%s) = %d", quoteIdent(colName), n),
				Note:  fmt.Sprintf("binary(%d) → bytea + CHECK octet_length = %d (T-SQL pads fixed binary values to their length)", n, n),
			}
		}
		return typeResult{PG: "bytea", Note: "varbinary → bytea"}

	case *ast.TSQLType:
		return mapTSQLBuiltin(x, caps)

	case *ast.UserDefinedType:
		if ref, ok := lookupTSQLUserType(caps.UserTypes, x.Schema, x.Name); ok {
			pg := quoteIdent(ref.Schema) + "." + quoteIdent(ref.Name)
			if ref.Kind == "composite" {
				return typeResult{
					PG:              pg,
					Note:            "table type → composite type " + pg,
					Warning:         "T-SQL table type " + x.Name + " mapped to the composite type " + pg + ": a table-valued value becomes a single composite (an array of it for a READONLY table-valued parameter)",
					WarningSeverity: SeverityInfo,
					WarningKind:     tsqlWarnTableType,
				}
			}
			return typeResult{PG: pg, Note: "alias type → domain " + pg}
		}
	}
	return typeResult{} // caller fills in TEXT + warning
}

// mapTSQLBuiltin maps the T-SQL built-ins that have no generic AST node.
func mapTSQLBuiltin(x *ast.TSQLType, caps Caps) typeResult {
	switch x.Name {
	case "BIT":
		return typeResult{PG: "boolean", Note: "bit → boolean"}
	case "MONEY":
		return typeResult{PG: "numeric(19,4)", Note: "money → numeric(19,4) (same range and scale, no currency semantics)"}
	case "SMALLMONEY":
		return typeResult{PG: "numeric(10,4)", Note: "smallmoney → numeric(10,4)"}
	case "DATETIME":
		return typeResult{PG: "timestamp(3)", Note: "datetime (1/300 s accuracy) → timestamp(3)"}
	case "SMALLDATETIME":
		return typeResult{PG: "timestamp(0)", Note: "smalldatetime (minute accuracy) → timestamp(0)"}
	case "DATETIME2":
		fsp, note := tsqlFsp(tsqlPrecision(x), "datetime2")
		return typeResult{PG: fmt.Sprintf("timestamp(%d)", fsp), Note: note}
	case "DATETIMEOFFSET":
		fsp, note := tsqlFsp(tsqlPrecision(x), "datetimeoffset")
		n := "datetimeoffset → timestamptz: the instant is preserved, the source UTC offset is not stored (PG keeps UTC and renders in the session time zone)"
		if note != "" {
			n += "; " + note
		}
		return typeResult{PG: fmt.Sprintf("timestamptz(%d)", fsp), Note: n}
	case "UNIQUEIDENTIFIER":
		return typeResult{PG: "uuid", Note: "uniqueidentifier → uuid"}
	case "XML":
		note := "xml → xml"
		if x.XMLSchema != "" {
			note = "typed xml(" + x.XMLSchema + ") → xml: PG does not validate XML against a schema collection"
		}
		return typeResult{PG: "xml", Note: note}
	case "ROWVERSION", "TIMESTAMP":
		return typeResult{
			PG:              "bytea",
			Note:            "rowversion → bytea (values copied as they are)",
			Warning:         "rowversion / timestamp column mapped to bytea: the source values are copied, but PostgreSQL has no automatic update on PG — rows written on PostgreSQL keep the value the application gives (NULL by default); use xmin or an update trigger for optimistic concurrency",
			WarningSeverity: SeverityInfo,
			WarningKind:     tsqlWarnRowversion,
		}
	case "HIERARCHYID":
		return typeResult{
			PG:              "text",
			Note:            "hierarchyid → text (canonical '/1/2/' path form)",
			Warning:         "hierarchyid column mapped to text holding the canonical path ('/1/3/'): the hierarchy methods (GetLevel, GetAncestor, IsDescendantOf, GetReparentedValue, GetDescendant with integer labels) are translated to the tsql.hierarchyid_* functions over that text, but ORDER BY / < / > compare the text, not the depth-first hierarchy order",
			WarningSeverity: SeverityInfo,
			WarningKind:     tsqlWarnHierarchy,
		}
	case "SQL_VARIANT":
		return typeResult{
			PG:              "text",
			Note:            "sql_variant → text",
			Warning:         "sql_variant column mapped to text: every value is copied as its text form and its base type is lost",
			WarningSeverity: SeverityInfo,
			WarningKind:     tsqlWarnSQLVariant,
		}
	case "GEOGRAPHY", "GEOMETRY":
		pgName := strings.ToLower(x.Name)
		if caps.HasPostGIS {
			return typeResult{PG: pgName, Note: pgName + " → PostGIS " + pgName + " (copied as EWKT: SRID and Z / M coordinates kept)"}
		}
		return typeResult{
			PG:              "text",
			Note:            pgName + " → text (PostGIS not installed on the target)",
			Warning:         "spatial type " + pgName + " requires PostGIS: mapped to a text placeholder holding the EWKT form",
			WarningSeverity: SeverityBlocking,
			WarningKind:     tsqlWarnSpatial,
		}
	case "IMAGE":
		return typeResult{PG: "bytea", Note: "image → bytea"}
	case "NTEXT":
		return typeResult{PG: "text", Note: "ntext → text"}
	case "SYSNAME":
		return typeResult{PG: "varchar(128)", Note: "sysname → varchar(128)"}
	case "CURSOR":
		return typeResult{PG: "refcursor"}
	}
	return typeResult{}
}

// tsqlPrecision returns the fractional-seconds precision of a datetime2 /
// datetimeoffset type (T-SQL default 7).
func tsqlPrecision(x *ast.TSQLType) int {
	if x.HasPrecision || x.Precision > 0 {
		return x.Precision
	}
	return 7
}

// tsqlFsp caps a T-SQL fractional-seconds precision to PG's maximum and
// returns the note explaining the cap ("" when none applies).
func tsqlFsp(fsp int, typ string) (int, string) {
	if fsp < 0 {
		fsp = 0
	}
	if fsp > pgMaxFsp {
		return pgMaxFsp, fmt.Sprintf("%s(%d) → precision %d: PG stores microseconds, the 100 ns digit is truncated", typ, fsp, pgMaxFsp)
	}
	return fsp, ""
}

// tsqlUserTypeKey is the caps.UserTypes key of a T-SQL alias / table
// type: folded schema (dbo when empty) + "." + folded name.
func tsqlUserTypeKey(schema, name string) string {
	return sourceObjectKey(schema, name)
}

// lookupTSQLUserType resolves a T-SQL user type reference: schema-
// qualified first (dbo when unqualified), then by bare name (a type of
// another schema used unqualified, registered by its bare name when that
// name is unique).
func lookupTSQLUserType(types map[string]UserTypeRef, schema, name string) (UserTypeRef, bool) {
	if types == nil || name == "" {
		return UserTypeRef{}, false
	}
	if ref, ok := types[tsqlUserTypeKey(schema, name)]; ok {
		return ref, true
	}
	if schema == "" {
		// Kind "ambiguous": several schemas define the name
		// (registerTSQLUserType), an unqualified use cannot pick one.
		if ref, ok := types[normalizeTSQLIdent(name)]; ok && ref.Kind != "ambiguous" {
			return ref, true
		}
	}
	return UserTypeRef{}, false
}
