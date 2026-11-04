package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestTSQLTypeMap(t *testing.T) {
	cases := []struct {
		name  string
		in    ast.DataType
		caps  Caps
		want  string
		check string
		warn  string // WarningKind, "" when no warning
		sev   Severity
	}{
		{name: "int", in: &ast.IntType{Name: "INT"}, want: "integer"},
		{name: "bigint", in: &ast.IntType{Name: "BIGINT"}, want: "bigint"},
		{name: "smallint", in: &ast.IntType{Name: "SMALLINT"}, want: "smallint"},
		{name: "tinyint", in: &ast.IntType{Name: "TINYINT"}, want: "smallint", check: `"col" BETWEEN 0 AND 255`},
		{name: "bit", in: &ast.TSQLType{Name: "BIT"}, want: "boolean"},
		{name: "decimal(12,4)", in: &ast.DecimalType{Name: "DECIMAL", Precision: 12, Scale: 4, HasPrec: true}, want: "numeric(12,4)"},
		{name: "decimal default", in: &ast.DecimalType{Name: "DECIMAL", Precision: 18}, want: "numeric(18,0)"},
		{name: "money", in: &ast.TSQLType{Name: "MONEY"}, want: "numeric(19,4)"},
		{name: "smallmoney", in: &ast.TSQLType{Name: "SMALLMONEY"}, want: "numeric(10,4)"},
		{name: "float", in: &ast.FloatType{Name: "FLOAT", Precision: 53}, want: "double precision"},
		{name: "float(24)", in: &ast.FloatType{Name: "FLOAT", Precision: 24, HasPS: true}, want: "real"},
		{name: "float(25)", in: &ast.FloatType{Name: "FLOAT", Precision: 25, HasPS: true}, want: "double precision"},
		{name: "real", in: &ast.FloatType{Name: "REAL", Precision: 24}, want: "real"},
		{name: "date", in: &ast.DateType{}, want: "date"},
		{name: "time(7)", in: &ast.TimeType{Fsp: 7}, want: "time(6)"},
		{name: "time(3)", in: &ast.TimeType{Fsp: 3}, want: "time(3)"},
		{name: "datetime", in: &ast.TSQLType{Name: "DATETIME"}, want: "timestamp(3)"},
		{name: "smalldatetime", in: &ast.TSQLType{Name: "SMALLDATETIME"}, want: "timestamp(0)"},
		{name: "datetime2", in: &ast.TSQLType{Name: "DATETIME2", Precision: 7}, want: "timestamp(6)"},
		{name: "datetime2(0)", in: &ast.TSQLType{Name: "DATETIME2", Precision: 0, HasPrecision: true}, want: "timestamp(0)"},
		{name: "datetimeoffset(3)", in: &ast.TSQLType{Name: "DATETIMEOFFSET", Precision: 3, HasPrecision: true}, want: "timestamptz(3)"},
		{name: "char(10)", in: &ast.CharType{Name: "CHAR", Length: 10, HasLength: true}, want: "char(10)"},
		{name: "nchar(10)", in: &ast.CharType{Name: "NCHAR", Length: 10, HasLength: true}, want: "char(10)"},
		{name: "char", in: &ast.CharType{Name: "CHAR"}, want: "char(1)"},
		{name: "varchar(50)", in: &ast.CharType{Name: "VARCHAR", Length: 50, HasLength: true}, want: "varchar(50)"},
		{name: "nvarchar(4000)", in: &ast.CharType{Name: "NVARCHAR", Length: 4000, HasLength: true}, want: "varchar(4000)"},
		{name: "varchar(max)", in: &ast.CharType{Name: "VARCHAR", Max: true}, want: "text"},
		{name: "nvarchar(max)", in: &ast.CharType{Name: "NVARCHAR", Max: true}, want: "text"},
		{name: "text", in: &ast.TextType{Name: "TEXT"}, want: "text"},
		{name: "ntext", in: &ast.TSQLType{Name: "NTEXT"}, want: "text"},
		{name: "sysname", in: &ast.TSQLType{Name: "SYSNAME"}, want: "varchar(128)"},
		{name: "binary(16)", in: &ast.BinaryType{Name: "BINARY", Length: 16}, want: "bytea", check: `octet_length("col") = 16`},
		{name: "varbinary(50)", in: &ast.BinaryType{Name: "VARBINARY", Length: 50}, want: "bytea"},
		{name: "varbinary(max)", in: &ast.BinaryType{Name: "VARBINARY", Max: true}, want: "bytea"},
		{name: "image", in: &ast.TSQLType{Name: "IMAGE"}, want: "bytea"},
		{name: "uniqueidentifier", in: &ast.TSQLType{Name: "UNIQUEIDENTIFIER"}, want: "uuid"},
		{name: "xml", in: &ast.TSQLType{Name: "XML"}, want: "xml"},
		{name: "typed xml", in: &ast.TSQLType{Name: "XML", XMLSchema: "Production.ManuInstructionsSchemaCollection"}, want: "xml"},
		{name: "rowversion", in: &ast.TSQLType{Name: "ROWVERSION"}, want: "bytea", warn: tsqlWarnRowversion, sev: SeverityInfo},
		{name: "timestamp is rowversion", in: &ast.TSQLType{Name: "TIMESTAMP"}, want: "bytea", warn: tsqlWarnRowversion, sev: SeverityInfo},
		{name: "hierarchyid", in: &ast.TSQLType{Name: "HIERARCHYID"}, want: "text", warn: tsqlWarnHierarchy, sev: SeverityInfo},
		{name: "sql_variant", in: &ast.TSQLType{Name: "SQL_VARIANT"}, want: "text", warn: tsqlWarnSQLVariant, sev: SeverityInfo},
		{name: "geography without PostGIS", in: &ast.TSQLType{Name: "GEOGRAPHY"}, want: "text", warn: tsqlWarnSpatial, sev: SeverityBlocking},
		{name: "geometry without PostGIS", in: &ast.TSQLType{Name: "GEOMETRY"}, want: "text", warn: tsqlWarnSpatial, sev: SeverityBlocking},
		{name: "geography with PostGIS", in: &ast.TSQLType{Name: "GEOGRAPHY"}, caps: Caps{HasPostGIS: true}, want: "geography"},
		{name: "geometry with PostGIS", in: &ast.TSQLType{Name: "GEOMETRY"}, caps: Caps{HasPostGIS: true}, want: "geometry"},
		{name: "cursor", in: &ast.TSQLType{Name: "CURSOR"}, want: "refcursor"},
		{
			name: "alias type", in: &ast.UserDefinedType{Schema: "dbo", Name: "Name"},
			caps: Caps{UserTypes: map[string]UserTypeRef{"dbo.name": {Kind: "domain", Schema: "dbo", Name: "name"}}},
			want: `"dbo"."name"`,
		},
		{
			name: "unqualified alias type resolves to dbo", in: &ast.UserDefinedType{Name: "Flag"},
			caps: Caps{UserTypes: map[string]UserTypeRef{"dbo.flag": {Kind: "domain", Schema: "dbo", Name: "flag"}}},
			want: `"dbo"."flag"`,
		},
		{
			name: "unqualified alias type of another schema by bare name", in: &ast.UserDefinedType{Name: "Code"},
			caps: Caps{UserTypes: map[string]UserTypeRef{"code": {Kind: "domain", Schema: "sales", Name: "code"}}},
			want: `"sales"."code"`,
		},
		{
			name: "table type", in: &ast.UserDefinedType{Schema: "dbo", Name: "IdList"},
			caps: Caps{UserTypes: map[string]UserTypeRef{"dbo.idlist": {Kind: "composite", Schema: "dbo", Name: "idlist"}}},
			want: `"dbo"."idlist"`, warn: tsqlWarnTableType, sev: SeverityInfo,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := MapType(dialects.KindSQLServer, c.in, "col", c.caps)
			require.Equal(t, c.want, r.PG, "note=%q", r.Note)
			require.Equal(t, c.check, r.Check)
			require.Equal(t, c.warn, r.WarningKind)
			if c.warn != "" {
				require.NotEmpty(t, r.Warning)
				require.Equal(t, c.sev, r.WarningSeverity)
			} else {
				require.Empty(t, r.Warning)
			}
		})
	}
}

// An unknown user type falls back to text with the generic warning.
func TestTSQLTypeMap_UnknownUserType(t *testing.T) {
	r := MapType(dialects.KindSQLServer, &ast.UserDefinedType{Schema: "dbo", Name: "Missing"}, "c", Caps{})
	require.Equal(t, "TEXT", r.PG)
	require.Contains(t, r.Warning, "unknown")
}

// A bare name used by two schemas is not resolved by its bare key.
func TestLookupTSQLUserType_Ambiguous(t *testing.T) {
	types := map[string]UserTypeRef{
		"dbo.code":   {Kind: "domain", Schema: "dbo", Name: "code"},
		"sales.code": {Kind: "domain", Schema: "sales", Name: "code"},
		"code":       {Kind: "ambiguous", Schema: "dbo", Name: "code"},
	}
	ref, ok := lookupTSQLUserType(types, "Sales", "Code")
	require.True(t, ok)
	require.Equal(t, "sales", ref.Schema)
	ref, ok = lookupTSQLUserType(types, "", "Code")
	require.True(t, ok)
	require.Equal(t, "dbo", ref.Schema) // dbo is the unqualified default
	r := MapType(dialects.KindSQLServer, &ast.UserDefinedType{Name: "Other"}, "c", Caps{UserTypes: types})
	require.Equal(t, "TEXT", r.PG)
}
