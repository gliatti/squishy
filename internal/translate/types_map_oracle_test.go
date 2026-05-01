package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// TestMapOracleType locks the Oracle → PostgreSQL type mapping against the
// Cybertec reference table
// (https://www.cybertec-postgresql.com/en/mapping-oracle-datatypes-to-postgresql/).
// Each case reproduces one row of that table.
func TestMapOracleType(t *testing.T) {
	cases := []struct {
		name string
		in   ast.DataType
		caps Caps
		pg   string
	}{
		// NUMBER family
		{"NUMBER", &ast.OracleNumberType{}, Caps{}, "NUMERIC"},
		{"NUMBER(4)", &ast.OracleNumberType{Precision: 4, HasPrec: true}, Caps{}, "SMALLINT"},
		{"NUMBER(9)", &ast.OracleNumberType{Precision: 9, HasPrec: true}, Caps{}, "INTEGER"},
		{"NUMBER(18)", &ast.OracleNumberType{Precision: 18, HasPrec: true}, Caps{}, "BIGINT"},
		{"NUMBER(20)", &ast.OracleNumberType{Precision: 20, HasPrec: true}, Caps{}, "NUMERIC(20,0)"},
		{"NUMBER(10,2)", &ast.OracleNumberType{Precision: 10, Scale: 2, HasPrec: true, HasScale: true}, Caps{}, "NUMERIC(10,2)"},

		// Character types
		{"VARCHAR2(30)", &ast.Varchar2Type{Length: 30, HasLength: true, SemanticsChar: true}, Caps{}, "VARCHAR(30)"},
		{"VARCHAR2 unbounded", &ast.Varchar2Type{}, Caps{}, "VARCHAR"},
		{"NVARCHAR2(30)", &ast.NVarchar2Type{Length: 30, HasLength: true}, Caps{}, "VARCHAR(30)"},
		{"NCHAR(5)", &ast.NCharType{Length: 5, HasLength: true}, Caps{}, "CHAR(5)"},
		{"CHAR(8)", &ast.CharType{Name: "CHAR", Length: 8, HasLength: true}, Caps{}, "CHAR(8)"},

		// LOB / LONG
		{"CLOB", &ast.ClobType{}, Caps{}, "TEXT"},
		{"NCLOB", &ast.ClobType{National: true}, Caps{}, "TEXT"},
		{"LONG", &ast.ClobType{Long: true}, Caps{}, "TEXT"},

		// Binary family
		{"BLOB", &ast.BlobType{Name: "BLOB"}, Caps{}, "BYTEA"},
		{"BFILE", &ast.BFileType{}, Caps{}, "BYTEA"},
		{"RAW", &ast.RawType{}, Caps{}, "BYTEA"},
		{"LONG RAW", &ast.RawType{Long: true}, Caps{}, "BYTEA"},

		// Floating-point
		{"FLOAT", &ast.FloatType{Name: "FLOAT"}, Caps{}, "DOUBLE PRECISION"},
		{"REAL", &ast.FloatType{Name: "REAL"}, Caps{}, "DOUBLE PRECISION"},
		{"BINARY_FLOAT", &ast.BinaryFloatType{}, Caps{}, "REAL"},
		{"BINARY_DOUBLE", &ast.BinaryDoubleType{}, Caps{}, "DOUBLE PRECISION"},

		// Date / time
		{"DATE", &ast.DateTimeType{}, Caps{}, "TIMESTAMP(0)"},
		{"TIMESTAMP", &ast.TimestampType{}, Caps{}, "TIMESTAMP"},
		{"TIMESTAMP(6)", &ast.TimestampType{Fsp: 6}, Caps{}, "TIMESTAMP(6)"},
		{"TIMESTAMP WITH TIME ZONE", &ast.TimestampTZType{}, Caps{}, "TIMESTAMPTZ"},
		{"TIMESTAMP(6) WITH TIME ZONE", &ast.TimestampTZType{Fsp: 6}, Caps{}, "TIMESTAMPTZ(6)"},
		{"TIMESTAMP WITH LOCAL TIME ZONE", &ast.TimestampTZType{Local: true}, Caps{}, "TIMESTAMPTZ"},
		{"TIMESTAMP(3) WITH LOCAL TIME ZONE", &ast.TimestampTZType{Fsp: 3, Local: true}, Caps{}, "TIMESTAMPTZ(3)"},

		// Intervals
		{"INTERVAL YEAR TO MONTH", &ast.IntervalYMType{}, Caps{}, "INTERVAL"},
		{"INTERVAL DAY TO SECOND", &ast.IntervalDSType{}, Caps{}, "INTERVAL"},

		// XML (dedicated node + user-defined fallback — DBMS_METADATA emits it quoted)
		{"XMLTYPE", &ast.XmlType{}, Caps{}, "xml"},
		{"XMLTYPE (user-defined alias)", &ast.UserDefinedType{Name: "XMLTYPE"}, Caps{}, "xml"},

		// SDO_GEOMETRY — PostGIS-gated
		{"SDO_GEOMETRY + PostGIS", &ast.UserDefinedType{Name: "SDO_GEOMETRY"}, Caps{HasPostGIS: true}, "geometry"},
		{"MDSYS.SDO_GEOMETRY + PostGIS", &ast.UserDefinedType{Name: "MDSYS.SDO_GEOMETRY"}, Caps{HasPostGIS: true}, "geometry"},
		{"SDO_GEOMETRY without PostGIS", &ast.UserDefinedType{Name: "SDO_GEOMETRY"}, Caps{}, "TEXT"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := mapOracleType(tc.in, "col", tc.caps)
			require.Equal(t, tc.pg, got.PG)
		})
	}

	t.Run("RAW(16) emits octet_length CHECK", func(t *testing.T) {
		got := mapOracleType(&ast.RawType{Length: 16, HasLength: true}, "col", Caps{})
		require.Equal(t, "BYTEA", got.PG)
		require.Contains(t, got.Check, "octet_length")
	})

	t.Run("SDO_GEOMETRY without PostGIS emits a warning", func(t *testing.T) {
		got := mapOracleType(&ast.UserDefinedType{Name: "SDO_GEOMETRY"}, "geom", Caps{})
		require.NotEmpty(t, got.Warning)
	})

	t.Run("SDO_GEOMETRY with PostGIS emits copy-pipeline warning", func(t *testing.T) {
		got := mapOracleType(&ast.UserDefinedType{Name: "SDO_GEOMETRY"}, "geom", Caps{HasPostGIS: true})
		require.NotEmpty(t, got.Warning, "users need to know the copy pipeline skips SDO payloads")
	})
}
