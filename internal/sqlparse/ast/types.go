package ast

// ---------------------------------------------------------------------------
// Numeric types
// ---------------------------------------------------------------------------

type IntType struct {
	Name     string // TINYINT|SMALLINT|MEDIUMINT|INT|INTEGER|BIGINT
	Width    int    // 0 if unspecified
	HasWidth bool
	Unsigned bool
	Zerofill bool
	P        Position
}

func (t *IntType) Pos() Position    { return t.P }
func (t *IntType) dataTypeNode()    {}
func (t *IntType) TypeName() string { return t.Name }

type FloatType struct {
	Name      string // FLOAT|DOUBLE|REAL
	Precision int    // for FLOAT(p,s) only
	Scale     int
	HasPS     bool
	Unsigned  bool
	Zerofill  bool
	P         Position
}

func (t *FloatType) Pos() Position    { return t.P }
func (t *FloatType) dataTypeNode()    {}
func (t *FloatType) TypeName() string { return t.Name }

type DecimalType struct {
	Name      string // DECIMAL|NUMERIC|DEC
	Precision int    // default 10 in MySQL
	Scale     int    // default 0
	HasPrec   bool
	Unsigned  bool
	Zerofill  bool
	P         Position
}

func (t *DecimalType) Pos() Position    { return t.P }
func (t *DecimalType) dataTypeNode()    {}
func (t *DecimalType) TypeName() string { return t.Name }

type BitType struct {
	Width int // 1 by default
	P     Position
}

func (t *BitType) Pos() Position    { return t.P }
func (t *BitType) dataTypeNode()    {}
func (t *BitType) TypeName() string { return "BIT" }

// ---------------------------------------------------------------------------
// String / binary types
// ---------------------------------------------------------------------------

type CharType struct {
	Name      string // CHAR|VARCHAR
	Length    int
	HasLength bool
	Charset   string
	Collation string
	P         Position
}

func (t *CharType) Pos() Position    { return t.P }
func (t *CharType) dataTypeNode()    {}
func (t *CharType) TypeName() string { return t.Name }

type TextType struct {
	Name      string // TINYTEXT|TEXT|MEDIUMTEXT|LONGTEXT
	Length    int
	HasLength bool
	Charset   string
	Collation string
	P         Position
}

func (t *TextType) Pos() Position    { return t.P }
func (t *TextType) dataTypeNode()    {}
func (t *TextType) TypeName() string { return t.Name }

type BlobType struct {
	Name string // TINYBLOB|BLOB|MEDIUMBLOB|LONGBLOB
	P    Position
}

func (t *BlobType) Pos() Position    { return t.P }
func (t *BlobType) dataTypeNode()    {}
func (t *BlobType) TypeName() string { return t.Name }

type BinaryType struct {
	Name   string // BINARY|VARBINARY
	Length int
	P      Position
}

func (t *BinaryType) Pos() Position    { return t.P }
func (t *BinaryType) dataTypeNode()    {}
func (t *BinaryType) TypeName() string { return t.Name }

type EnumType struct {
	Values []string
	P      Position
}

func (t *EnumType) Pos() Position    { return t.P }
func (t *EnumType) dataTypeNode()    {}
func (t *EnumType) TypeName() string { return "ENUM" }

type SetType struct {
	Values []string
	P      Position
}

func (t *SetType) Pos() Position    { return t.P }
func (t *SetType) dataTypeNode()    {}
func (t *SetType) TypeName() string { return "SET" }

type JSONType struct{ P Position }

func (t *JSONType) Pos() Position    { return t.P }
func (t *JSONType) dataTypeNode()    {}
func (t *JSONType) TypeName() string { return "JSON" }

// ---------------------------------------------------------------------------
// Temporal types
// ---------------------------------------------------------------------------

type DateType struct{ P Position }

func (t *DateType) Pos() Position    { return t.P }
func (t *DateType) dataTypeNode()    {}
func (t *DateType) TypeName() string { return "DATE" }

type TimeType struct {
	Fsp int // fractional seconds precision
	P   Position
}

func (t *TimeType) Pos() Position    { return t.P }
func (t *TimeType) dataTypeNode()    {}
func (t *TimeType) TypeName() string { return "TIME" }

type DateTimeType struct {
	Fsp int
	P   Position
}

func (t *DateTimeType) Pos() Position    { return t.P }
func (t *DateTimeType) dataTypeNode()    {}
func (t *DateTimeType) TypeName() string { return "DATETIME" }

type TimestampType struct {
	Fsp int
	P   Position
}

func (t *TimestampType) Pos() Position    { return t.P }
func (t *TimestampType) dataTypeNode()    {}
func (t *TimestampType) TypeName() string { return "TIMESTAMP" }

type YearType struct{ P Position }

func (t *YearType) Pos() Position    { return t.P }
func (t *YearType) dataTypeNode()    {}
func (t *YearType) TypeName() string { return "YEAR" }

// ---------------------------------------------------------------------------
// Spatial types (represented as a family)
// ---------------------------------------------------------------------------

type SpatialType struct {
	Name string // GEOMETRY|POINT|LINESTRING|POLYGON|...
	P    Position
}

func (t *SpatialType) Pos() Position    { return t.P }
func (t *SpatialType) dataTypeNode()    {}
func (t *SpatialType) TypeName() string { return t.Name }

// ---------------------------------------------------------------------------
// Oracle-specific data types
// ---------------------------------------------------------------------------

// OracleNumberType — NUMBER[(p[,s])]. Distinct from DecimalType because
// NUMBER with no precision means "up to 38 digits, any scale" (full NUMERIC),
// whereas DECIMAL without precision in MySQL defaults to (10,0).
type OracleNumberType struct {
	Precision int
	Scale     int
	HasPrec   bool
	HasScale  bool
	P         Position
}

func (t *OracleNumberType) Pos() Position    { return t.P }
func (t *OracleNumberType) dataTypeNode()    {}
func (t *OracleNumberType) TypeName() string { return "NUMBER" }

// Varchar2Type — VARCHAR2(n [CHAR|BYTE]).
type Varchar2Type struct {
	Length        int
	HasLength     bool
	SemanticsChar bool // true = CHAR semantics; false = BYTE (or unspecified)
	P             Position
}

func (t *Varchar2Type) Pos() Position    { return t.P }
func (t *Varchar2Type) dataTypeNode()    {}
func (t *Varchar2Type) TypeName() string { return "VARCHAR2" }

// NVarchar2Type — NVARCHAR2(n).
type NVarchar2Type struct {
	Length    int
	HasLength bool
	P         Position
}

func (t *NVarchar2Type) Pos() Position    { return t.P }
func (t *NVarchar2Type) dataTypeNode()    {}
func (t *NVarchar2Type) TypeName() string { return "NVARCHAR2" }

// NCharType — NCHAR(n).
type NCharType struct {
	Length    int
	HasLength bool
	P         Position
}

func (t *NCharType) Pos() Position    { return t.P }
func (t *NCharType) dataTypeNode()    {}
func (t *NCharType) TypeName() string { return "NCHAR" }

// ClobType — CLOB / NCLOB / LONG.
type ClobType struct {
	National bool // NCLOB
	Long     bool // LONG (deprecated, pre-8i)
	P        Position
}

func (t *ClobType) Pos() Position { return t.P }
func (t *ClobType) dataTypeNode() {}
func (t *ClobType) TypeName() string {
	if t.National {
		return "NCLOB"
	}
	if t.Long {
		return "LONG"
	}
	return "CLOB"
}

// BFileType — BFILE (external file locator).
type BFileType struct{ P Position }

func (t *BFileType) Pos() Position    { return t.P }
func (t *BFileType) dataTypeNode()    {}
func (t *BFileType) TypeName() string { return "BFILE" }

// RawType — RAW(n) / LONG RAW.
type RawType struct {
	Length    int
	HasLength bool
	Long      bool
	P         Position
}

func (t *RawType) Pos() Position { return t.P }
func (t *RawType) dataTypeNode() {}
func (t *RawType) TypeName() string {
	if t.Long {
		return "LONG RAW"
	}
	return "RAW"
}

// RowIdType — ROWID / UROWID(n).
type RowIdType struct {
	Urowid    bool
	Length    int
	HasLength bool
	P         Position
}

func (t *RowIdType) Pos() Position { return t.P }
func (t *RowIdType) dataTypeNode() {}
func (t *RowIdType) TypeName() string {
	if t.Urowid {
		return "UROWID"
	}
	return "ROWID"
}

// IntervalYMType — INTERVAL YEAR[(p)] TO MONTH.
type IntervalYMType struct {
	Precision int // year precision, 2 by default
	HasPrec   bool
	P         Position
}

func (t *IntervalYMType) Pos() Position    { return t.P }
func (t *IntervalYMType) dataTypeNode()    {}
func (t *IntervalYMType) TypeName() string { return "INTERVAL YEAR TO MONTH" }

// IntervalDSType — INTERVAL DAY[(p)] TO SECOND[(f)].
type IntervalDSType struct {
	DayPrec  int
	FracPrec int
	HasDay   bool
	HasFrac  bool
	P        Position
}

func (t *IntervalDSType) Pos() Position    { return t.P }
func (t *IntervalDSType) dataTypeNode()    {}
func (t *IntervalDSType) TypeName() string { return "INTERVAL DAY TO SECOND" }

// TimestampTZType — TIMESTAMP[(f)] WITH [LOCAL] TIME ZONE.
type TimestampTZType struct {
	Fsp   int
	Local bool // WITH LOCAL TIME ZONE
	P     Position
}

func (t *TimestampTZType) Pos() Position { return t.P }
func (t *TimestampTZType) dataTypeNode() {}
func (t *TimestampTZType) TypeName() string {
	if t.Local {
		return "TIMESTAMP WITH LOCAL TIME ZONE"
	}
	return "TIMESTAMP WITH TIME ZONE"
}

// XmlType — XMLTYPE.
type XmlType struct{ P Position }

func (t *XmlType) Pos() Position    { return t.P }
func (t *XmlType) dataTypeNode()    {}
func (t *XmlType) TypeName() string { return "XMLTYPE" }

// BinaryFloatType — BINARY_FLOAT (IEEE 754 single).
type BinaryFloatType struct{ P Position }

func (t *BinaryFloatType) Pos() Position    { return t.P }
func (t *BinaryFloatType) dataTypeNode()    {}
func (t *BinaryFloatType) TypeName() string { return "BINARY_FLOAT" }

// BinaryDoubleType — BINARY_DOUBLE (IEEE 754 double).
type BinaryDoubleType struct{ P Position }

func (t *BinaryDoubleType) Pos() Position    { return t.P }
func (t *BinaryDoubleType) dataTypeNode()    {}
func (t *BinaryDoubleType) TypeName() string { return "BINARY_DOUBLE" }

// VectorType — VECTOR(dim [, elem_kind]) (Oracle 23ai).
// ElemKind is one of "FLOAT32"|"FLOAT64"|"INT8"|"BINARY"|"" (unspecified).
type VectorType struct {
	Dim      int
	HasDim   bool
	ElemKind string
	P        Position
}

func (t *VectorType) Pos() Position    { return t.P }
func (t *VectorType) dataTypeNode()    {}
func (t *VectorType) TypeName() string { return "VECTOR" }

// OracleJsonType — JSON (native, Oracle 21c+).
type OracleJsonType struct{ P Position }

func (t *OracleJsonType) Pos() Position    { return t.P }
func (t *OracleJsonType) dataTypeNode()    {}
func (t *OracleJsonType) TypeName() string { return "JSON" }

// OracleBooleanType — BOOLEAN (native, Oracle 23c+).
type OracleBooleanType struct{ P Position }

func (t *OracleBooleanType) Pos() Position    { return t.P }
func (t *OracleBooleanType) dataTypeNode()    {}
func (t *OracleBooleanType) TypeName() string { return "BOOLEAN" }

// UserDefinedType — reference to an object type, %TYPE or %ROWTYPE.
// Anchored is true for table.col%TYPE or table%ROWTYPE.
type UserDefinedType struct {
	Schema   string // may be empty
	Name     string // object type name, or anchor (table / table.col)
	Anchored string // ""|"TYPE"|"ROWTYPE"
	P        Position
}

func (t *UserDefinedType) Pos() Position { return t.P }
func (t *UserDefinedType) dataTypeNode() {}
func (t *UserDefinedType) TypeName() string {
	if t.Anchored != "" {
		return t.Name + "%" + t.Anchored
	}
	return t.Name
}

// ---------------------------------------------------------------------------
// DB2-specific data types (LUW + z/OS)
// ---------------------------------------------------------------------------

// DB2DecFloatType — DECFLOAT(16|34). IEEE 754-2008 decimal floating point;
// PG has no equivalent so the type mapper renders NUMERIC + info warning.
type DB2DecFloatType struct {
	Width   int // 16 or 34, 0 if unspecified (DB2 default = 34)
	HasWidth bool
	P       Position
}

func (t *DB2DecFloatType) Pos() Position    { return t.P }
func (t *DB2DecFloatType) dataTypeNode()    {}
func (t *DB2DecFloatType) TypeName() string { return "DECFLOAT" }

// DB2GraphicType — GRAPHIC(n) / VARGRAPHIC(n) / DBCLOB(n) (UCS-2 strings).
// Mapped to PG TEXT (UTF-8 native, round-trip preserved).
type DB2GraphicType struct {
	Name      string // GRAPHIC|VARGRAPHIC|DBCLOB|LONG VARGRAPHIC
	Length    int
	HasLength bool
	P         Position
}

func (t *DB2GraphicType) Pos() Position    { return t.P }
func (t *DB2GraphicType) dataTypeNode()    {}
func (t *DB2GraphicType) TypeName() string { return t.Name }

// DB2ForBitDataType — CHAR(n) FOR BIT DATA / VARCHAR(n) FOR BIT DATA.
// Inner is the wrapped *CharType. Mapped to PG BYTEA.
type DB2ForBitDataType struct {
	Inner DataType // *CharType (CHAR or VARCHAR)
	P     Position
}

func (t *DB2ForBitDataType) Pos() Position { return t.P }
func (t *DB2ForBitDataType) dataTypeNode() {}
func (t *DB2ForBitDataType) TypeName() string {
	if t.Inner != nil {
		return t.Inner.TypeName() + " FOR BIT DATA"
	}
	return "FOR BIT DATA"
}

// DB2RowIDType — ROWID. No PG equivalent; the type mapper emits TEXT plus
// a diagnostic warning suggesting a UUID column.
type DB2RowIDType struct{ P Position }

func (t *DB2RowIDType) Pos() Position    { return t.P }
func (t *DB2RowIDType) dataTypeNode()    {}
func (t *DB2RowIDType) TypeName() string { return "ROWID" }

// DB2TimestampTZType — TIMESTAMP[(n)] WITH TIME ZONE (LUW only). Distinct
// from the generic TimestampType because the existing node has no WithTZ
// flag and z/OS does not support this variant (the mapper drops the TZ
// with an info warning when Kind == KindDB2zOS).
type DB2TimestampTZType struct {
	Fsp    int
	HasFsp bool
	P      Position
}

func (t *DB2TimestampTZType) Pos() Position    { return t.P }
func (t *DB2TimestampTZType) dataTypeNode()    {}
func (t *DB2TimestampTZType) TypeName() string { return "TIMESTAMP WITH TIME ZONE" }

// DB2BooleanType — BOOLEAN. LUW 11+ supports a native BOOLEAN; z/OS does
// not, and the mapper falls back to SMALLINT + CHECK (col IN (0,1)).
type DB2BooleanType struct{ P Position }

func (t *DB2BooleanType) Pos() Position    { return t.P }
func (t *DB2BooleanType) dataTypeNode()    {}
func (t *DB2BooleanType) TypeName() string { return "BOOLEAN" }

// DB2XmlType — XML (DB2 native). Mapped to PG xml (builtin).
type DB2XmlType struct{ P Position }

func (t *DB2XmlType) Pos() Position    { return t.P }
func (t *DB2XmlType) dataTypeNode()    {}
func (t *DB2XmlType) TypeName() string { return "XML" }
