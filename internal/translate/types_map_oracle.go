package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func init() {
	RegisterTypeMapper(dialects.KindOracle, mapOracleType)
	RegisterTypeMapper(dialects.KindOracle19, mapOracleType)
}

// mapOracleType rewrites Oracle AST data types into PostgreSQL types.
// Rules of thumb:
//   - NUMBER(p,s) → narrow to SMALLINT/INTEGER/BIGINT when scale=0 and p fits;
//     otherwise NUMERIC(p,s). Bare NUMBER → NUMERIC (arbitrary precision).
//   - VARCHAR2(n [CHAR|BYTE]) → VARCHAR(n); BYTE semantics preserved as a note.
//   - CLOB/NCLOB/LONG → TEXT; RAW/LONG RAW/BLOB → BYTEA.
//   - DATE (Oracle includes time) → TIMESTAMP(0) (no TZ) + note.
//   - TIMESTAMP[(n)] → TIMESTAMP(n); WITH [LOCAL] TIME ZONE → TIMESTAMPTZ.
//   - INTERVAL YM / DS → INTERVAL (PG has a single unified type).
//   - XMLTYPE → xml (builtin). JSON (23c) → JSONB. BOOLEAN (23c) → BOOLEAN.
//   - VECTOR(n, kind) → vector(n) when pgvector is installed on the target,
//     otherwise TEXT + Warning + prereq.
//   - ROWID/UROWID → TEXT with a diagnostic warning (no PG equivalent).
//   - BFILE → TEXT with a blocking warning (manual intervention required).
func mapOracleType(t ast.DataType, colName string, caps Caps) typeResult {
	switch x := t.(type) {

	// ---- Oracle-specific nodes ----

	case *ast.OracleNumberType:
		// NUMBER with no precision → arbitrary-precision NUMERIC.
		if !x.HasPrec {
			return typeResult{PG: "NUMERIC", Note: "NUMBER → NUMERIC (arbitrary precision)"}
		}
		// NUMBER(p,0) or NUMBER(p) → narrow to integer family when it fits.
		if !x.HasScale || x.Scale == 0 {
			switch {
			case x.Precision <= 4:
				return typeResult{PG: "SMALLINT", Note: fmt.Sprintf("NUMBER(%d) → SMALLINT", x.Precision)}
			case x.Precision <= 9:
				return typeResult{PG: "INTEGER", Note: fmt.Sprintf("NUMBER(%d) → INTEGER", x.Precision)}
			case x.Precision <= 18:
				return typeResult{PG: "BIGINT", Note: fmt.Sprintf("NUMBER(%d) → BIGINT", x.Precision)}
			}
			return typeResult{
				PG:   fmt.Sprintf("NUMERIC(%d,0)", x.Precision),
				Note: fmt.Sprintf("NUMBER(%d) → NUMERIC(%d,0)", x.Precision, x.Precision),
			}
		}
		return typeResult{
			PG:   fmt.Sprintf("NUMERIC(%d,%d)", x.Precision, x.Scale),
			Note: fmt.Sprintf("NUMBER(%d,%d) → NUMERIC(%d,%d)", x.Precision, x.Scale, x.Precision, x.Scale),
		}

	case *ast.Varchar2Type:
		l := x.Length
		if !x.HasLength {
			return typeResult{PG: "VARCHAR", Note: "VARCHAR2 → VARCHAR (unbounded)"}
		}
		note := "VARCHAR2 → VARCHAR"
		if !x.SemanticsChar {
			note = "VARCHAR2 BYTE semantics → VARCHAR (PG stores chars; byte length may differ)"
		}
		return typeResult{PG: fmt.Sprintf("VARCHAR(%d)", l), Note: note}

	case *ast.NVarchar2Type:
		if !x.HasLength {
			return typeResult{PG: "VARCHAR", Note: "NVARCHAR2 → VARCHAR (PG is Unicode-native)"}
		}
		return typeResult{PG: fmt.Sprintf("VARCHAR(%d)", x.Length), Note: "NVARCHAR2 → VARCHAR"}

	case *ast.NCharType:
		l := x.Length
		if !x.HasLength {
			l = 1
		}
		return typeResult{PG: fmt.Sprintf("CHAR(%d)", l), Note: "NCHAR → CHAR"}

	case *ast.ClobType:
		if x.National {
			return typeResult{PG: "TEXT", Note: "NCLOB → TEXT"}
		}
		if x.Long {
			return typeResult{PG: "TEXT", Note: "LONG (deprecated) → TEXT"}
		}
		return typeResult{PG: "TEXT", Note: "CLOB → TEXT"}

	case *ast.BFileType:
		// Oracle BFILE is a pointer to an external file (directory + name)
		// with read-only access. PG has no equivalent locator type, but the
		// canonical migration (per Cybertec's mapping guide) is BYTEA: the
		// new column holds the actual binary content, which app code can
		// hydrate on the PG side via pg_read_binary_file() if the files
		// remain accessible to the database server. The COPY pipeline stores
		// NULL when the source BFILE is unset (the usual case for opaque
		// locator columns), so this mapping is migration-safe by default.
		return typeResult{
			PG:   "BYTEA",
			Note: "BFILE → BYTEA (binary content; populate via pg_read_binary_file() on the PG side — the opaque Oracle locator is not preserved)",
		}

	case *ast.RawType:
		if x.Long {
			return typeResult{PG: "BYTEA", Note: "LONG RAW (deprecated) → BYTEA"}
		}
		if x.HasLength {
			return typeResult{
				PG:    "BYTEA",
				Check: fmt.Sprintf("octet_length(%s) <= %d", quoteIdent(colName), x.Length),
				Note:  fmt.Sprintf("RAW(%d) → BYTEA + CHECK octet_length", x.Length),
			}
		}
		return typeResult{PG: "BYTEA", Note: "RAW → BYTEA"}

	case *ast.RowIdType:
		// Oracle ROWID/UROWID is an opaque physical (or logical for UROWID)
		// row pointer — a unique per-row identifier. PG has no equivalent
		// persistent type (CTID is a pseudo-column, not storable). Rather
		// than fall back to TEXT with a "placeholder" warning, we materialize
		// the semantic intent — "give every row a unique, monotonic
		// identifier" — as a BIGINT column with DEFAULT nextval(seq).
		// translateColumn attaches the sequence + default using the same
		// machinery as AUTO_INCREMENT / GENERATED AS IDENTITY.
		name := "ROWID"
		if x.Urowid {
			name = "UROWID"
		}
		return typeResult{
			PG:   "BIGINT",
			Note: name + " → BIGINT DEFAULT nextval(seq) (surrogate row identifier; Oracle ROWID values are dropped on copy)",
		}

	case *ast.IntervalYMType:
		return typeResult{PG: "INTERVAL", Note: "INTERVAL YEAR TO MONTH → INTERVAL"}

	case *ast.IntervalDSType:
		return typeResult{PG: "INTERVAL", Note: "INTERVAL DAY TO SECOND → INTERVAL"}

	case *ast.TimestampTZType:
		if x.Local {
			if x.Fsp > 0 {
				return typeResult{
					PG:   fmt.Sprintf("TIMESTAMPTZ(%d)", x.Fsp),
					Note: "TIMESTAMP WITH LOCAL TIME ZONE → TIMESTAMPTZ (session-tz semantics differ: PG returns stored UTC, Oracle converts on SELECT)",
				}
			}
			return typeResult{
				PG:   "TIMESTAMPTZ",
				Note: "TIMESTAMP WITH LOCAL TIME ZONE → TIMESTAMPTZ (session-tz semantics differ)",
			}
		}
		if x.Fsp > 0 {
			return typeResult{PG: fmt.Sprintf("TIMESTAMPTZ(%d)", x.Fsp), Note: "TIMESTAMP WITH TIME ZONE → TIMESTAMPTZ"}
		}
		return typeResult{PG: "TIMESTAMPTZ", Note: "TIMESTAMP WITH TIME ZONE → TIMESTAMPTZ"}

	case *ast.XmlType:
		return typeResult{PG: "xml", Note: "XMLTYPE → xml (PG built-in)"}

	case *ast.BinaryFloatType:
		return typeResult{PG: "REAL", Note: "BINARY_FLOAT → REAL (IEEE 754 single)"}

	case *ast.BinaryDoubleType:
		return typeResult{PG: "DOUBLE PRECISION", Note: "BINARY_DOUBLE → DOUBLE PRECISION"}

	case *ast.VectorType:
		// VECTOR has no usable substitute in PG — TEXT would silently break
		// distance operators and indexing. Always emit the pgvector type and
		// surface a blocking prerequisite when the extension is missing.
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

	case *ast.OracleJsonType:
		return typeResult{PG: "JSONB", Note: "Oracle JSON (23c) → JSONB"}

	case *ast.OracleBooleanType:
		return typeResult{PG: "BOOLEAN", Note: "BOOLEAN (23c) → BOOLEAN"}

	case *ast.UserDefinedType:
		// Anchored types (%TYPE / %ROWTYPE) handled below; for plain
		// user-type references, first see if the same migration registered
		// a translation in caps.UserTypes — handled in the post-switch
		// fallback so we don't have to thread it through every named-type
		// branch above.
		// %TYPE / %ROWTYPE — PG supports the same syntax natively in
		// PL/pgSQL DECLARE blocks AND (since PG 11+) in CREATE FUNCTION
		// argument lists. Emit verbatim with the name lowercased to match
		// the rest of the Oracle pipeline (DBMS_METADATA returns Oracle
		// uppercase identifiers; we map them to PG lowercase). The anchor
		// resolves at routine creation time against pg_attribute, so a
		// missing target table surfaces a clear "relation does not exist"
		// rather than the previous opaque TEXT-with-warning.
		if x.Anchored != "" {
			return typeResult{
				PG:   strings.ToLower(x.Name) + "%" + x.Anchored,
				Note: x.Name + "%" + x.Anchored + " → PG anchored type (resolves at CREATE FUNCTION time against pg_attribute)",
			}
		}
		// DBMS_METADATA.GET_DDL emits Oracle built-in types double-quoted
		// (e.g. `"C_XML" "XMLTYPE"`), so they arrive here as UserDefinedType.
		// Resolve well-known built-ins to their proper PG counterparts rather
		// than fall back to TEXT + warning.
		switch strings.ToUpper(strings.Trim(x.Name, `"`)) {
		case "XMLTYPE", "SYS.XMLTYPE":
			return typeResult{PG: "xml", Note: "XMLTYPE → xml (PG built-in)"}
		case "SDO_GEOMETRY", "MDSYS.SDO_GEOMETRY":
			if caps.HasPostGIS {
				return typeResult{
					PG:      "geometry",
					Note:    "SDO_GEOMETRY → PostGIS geometry",
					Warning: "SDO_GEOMETRY column emitted as geometry but the copy pipeline skips SDO payloads — convert with SDO_UTIL.TO_WKTGEOMETRY on the source side and ST_GeomFromText on the target",
				}
			}
			return typeResult{
				PG:      "TEXT",
				Note:    "SDO_GEOMETRY → TEXT (install PostGIS and remap to geometry for spatial support)",
				Warning: "Oracle Spatial SDO_GEOMETRY has no automatic PG translation — consider PostGIS",
			}
		}
		// Oracle SYS_REFCURSOR — generic ref cursor for dynamic queries.
		// PG plpgsql exposes the same shape under the name `refcursor`, so
		// the substitution is a 1:1 type rename. Without this, the value
		// would land as TEXT and PG raises `must be of type cursor or
		// refcursor` at the first OPEN/FETCH against the variable.
		switch strings.ToUpper(strings.Trim(x.Name, `"`)) {
		case "SYS_REFCURSOR", "SYS.SYS_REFCURSOR", "REF_CURSOR":
			return typeResult{PG: "refcursor", Note: "SYS_REFCURSOR → refcursor (PG plpgsql native)"}
		}
		// Oracle Multimedia types (ORDIMAGE / ORDAUDIO / ORDVIDEO / ORDDOC)
		// — opaque object types whose payload is a BLOB. Map to BYTEA so the
		// data round-trips; metadata methods (ORDImage.getWidth(), …) need
		// to be re-implemented in app code.
		switch strings.ToUpper(strings.Trim(x.Name, `"`)) {
		case "ORDSYS.ORDIMAGE", "ORDIMAGE",
			"ORDSYS.ORDAUDIO", "ORDAUDIO",
			"ORDSYS.ORDVIDEO", "ORDVIDEO",
			"ORDSYS.ORDDOC", "ORDDOC":
			return typeResult{
				PG:      "BYTEA",
				Note:    x.Name + " → BYTEA (Oracle Multimedia metadata accessors lost)",
				Warning: "Oracle Multimedia type " + x.Name + " stored as BYTEA — payload preserved, but ORD*.getWidth/getHeight/getCompression methods have no PG counterpart",
			}
		case "SYS.ANYDATA", "ANYDATA",
			"SYS.ANYTYPE", "ANYTYPE",
			"SYS.ANYDATASET", "ANYDATASET":
			return typeResult{
				PG:      "JSONB",
				Note:    x.Name + " → JSONB (Oracle ANYTYPE family — type tag carried in JSONB envelope)",
				Warning: "Oracle " + x.Name + " stored as JSONB; the source type tag (ANYTYPE.GETTYPENAME) needs to be carried in the JSONB envelope manually",
			}
		}
		// Migrated user types: same dump emitted a CREATE TYPE earlier and
		// recorded the rendered PG form on caps.UserTypes. Resolve the
		// column to the migrated form rather than falling back to TEXT.
		if u, ok := lookupUserType(caps.UserTypes, x.Name); ok {
			switch u.Kind {
			case "composite":
				ref := pgQualified(u.Schema, u.Name)
				return typeResult{
					PG:   ref,
					Note: x.Name + " → " + ref + " (PG composite emitted earlier in this migration)",
				}
			case "array_scalar", "array_composite":
				return typeResult{
					PG:   u.ElemPG + "[]",
					Note: x.Name + " → " + u.ElemPG + "[] (Oracle " + u.Kind + " collection flattened to PG array)",
				}
			}
		}
		return typeResult{
			PG:      "TEXT",
			Note:    "user-defined type " + x.Name + " → TEXT (resolve manually)",
			Warning: "user-defined type " + x.Name + " not translated automatically",
		}

	// ---- Shared AST nodes that the Oracle parser also emits ----

	case *ast.CharType:
		l := x.Length
		if !x.HasLength {
			l = 1
		}
		if strings.EqualFold(x.Name, "VARCHAR") {
			return typeResult{PG: fmt.Sprintf("VARCHAR(%d)", l)}
		}
		return typeResult{PG: fmt.Sprintf("CHAR(%d)", l)}

	case *ast.DateTimeType:
		// Oracle DATE includes time — we emit DateTimeType for it.
		return typeResult{PG: "TIMESTAMP(0)", Note: "Oracle DATE (includes time) → TIMESTAMP(0)"}

	case *ast.TimestampType:
		if x.Fsp > 0 {
			return typeResult{PG: fmt.Sprintf("TIMESTAMP(%d)", x.Fsp)}
		}
		return typeResult{PG: "TIMESTAMP"}

	case *ast.FloatType:
		// Oracle FLOAT(p) is a binary precision NUMBER — map to DOUBLE PRECISION.
		switch strings.ToUpper(x.Name) {
		case "REAL":
			return typeResult{PG: "DOUBLE PRECISION", Note: "Oracle REAL → DOUBLE PRECISION (38-digit decimal)"}
		}
		return typeResult{PG: "DOUBLE PRECISION", Note: "Oracle FLOAT → DOUBLE PRECISION"}

	case *ast.BlobType:
		return typeResult{PG: "BYTEA", Note: "BLOB → BYTEA"}
	}
	return typeResult{} // fallthrough → generic TEXT + warning
}

// lookupUserType resolves a possibly schema-qualified Oracle type name (e.g.
// "OE.CUST_ADDRESS_TYP", "phone_list_typ", `"OE"."PHONE_LIST_TYP"`) against
// the registry the translator built up while emitting CREATE TYPE statements.
// Matching is case-insensitive and ignores the source schema — Oracle
// identifiers fold to lowercase on the PG side and the source schema is
// always rewritten to the migration target schema.
func lookupUserType(reg map[string]UserTypeRef, raw string) (UserTypeRef, bool) {
	if reg == nil {
		return UserTypeRef{}, false
	}
	name := strings.Trim(raw, `"`)
	// Strip a leading "schema." (also potentially quoted) — we keyed the
	// registry on the bare type name in lowercase.
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = strings.Trim(name[i+1:], `"`)
	}
	r, ok := reg[strings.ToLower(name)]
	return r, ok
}

// pgQualified renders a `[schema.]name` reference with PG identifier quoting.
// Empty schema produces a bare `"name"`.
func pgQualified(schema, name string) string {
	if schema == "" {
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
	return `"` + strings.ReplaceAll(schema, `"`, `""`) + `"."` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
