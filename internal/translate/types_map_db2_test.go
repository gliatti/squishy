package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestDB2_BasicNumericTypes(t *testing.T) {
	cases := []struct {
		name string
		in   ast.DataType
		want string
	}{
		{"SMALLINT", &ast.IntType{Name: "SMALLINT"}, "SMALLINT"},
		{"INTEGER", &ast.IntType{Name: "INTEGER"}, "INTEGER"},
		{"BIGINT", &ast.IntType{Name: "BIGINT"}, "BIGINT"},
		{"DECIMAL(12,2)", &ast.DecimalType{Precision: 12, Scale: 2, HasPrec: true}, "NUMERIC(12,2)"},
		{"DECFLOAT(34)", &ast.DB2DecFloatType{Width: 34, HasWidth: true}, "NUMERIC"},
		{"REAL", &ast.FloatType{Name: "REAL"}, "REAL"},
		{"DOUBLE", &ast.FloatType{Name: "DOUBLE"}, "DOUBLE PRECISION"},
		{"FLOAT(20)", &ast.FloatType{Name: "FLOAT", Precision: 20, HasPS: true}, "REAL"},
		{"FLOAT(50)", &ast.FloatType{Name: "FLOAT", Precision: 50, HasPS: true}, "DOUBLE PRECISION"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mapDB2Type(c.in, "col", Caps{})
			if r.PG != c.want {
				t.Errorf("PG = %q, want %q (note=%q)", r.PG, c.want, r.Note)
			}
		})
	}
}

func TestDB2_StringTypes(t *testing.T) {
	cases := []struct {
		name string
		in   ast.DataType
		want string
	}{
		{"CHAR(4)", &ast.CharType{Name: "CHAR", Length: 4, HasLength: true}, "CHAR(4)"},
		{"VARCHAR(40)", &ast.CharType{Name: "VARCHAR", Length: 40, HasLength: true}, "VARCHAR(40)"},
		{"VARGRAPHIC(50)", &ast.DB2GraphicType{Name: "VARGRAPHIC", Length: 50, HasLength: true}, "VARCHAR(50)"},
		{"GRAPHIC unbound", &ast.DB2GraphicType{Name: "DBCLOB"}, "TEXT"},
		{"CLOB", &ast.ClobType{}, "TEXT"},
		{"BLOB", &ast.BlobType{Name: "BLOB"}, "BYTEA"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mapDB2Type(c.in, "col", Caps{})
			if r.PG != c.want {
				t.Errorf("PG = %q, want %q (note=%q)", r.PG, c.want, r.Note)
			}
		})
	}
}

func TestDB2_ForBitData(t *testing.T) {
	in := &ast.DB2ForBitDataType{
		Inner: &ast.CharType{Name: "VARCHAR", Length: 100, HasLength: true},
	}
	r := mapDB2Type(in, "payload", Caps{})
	if r.PG != "BYTEA" {
		t.Errorf("PG = %q, want BYTEA", r.PG)
	}
	if !strings.Contains(r.Note, "FOR BIT DATA") {
		t.Errorf("note should mention FOR BIT DATA: %q", r.Note)
	}
}

func TestDB2_TemporalTypes(t *testing.T) {
	cases := []struct {
		name string
		in   ast.DataType
		want string
	}{
		{"DATE", &ast.DateType{}, "DATE"},
		{"TIME", &ast.TimeType{}, "TIME(0) WITHOUT TIME ZONE"},
		{"TIMESTAMP(6)", &ast.TimestampType{Fsp: 6}, "TIMESTAMP(6)"},
		{"TIMESTAMP(0)", &ast.TimestampType{Fsp: 0}, "TIMESTAMP(6)"}, // default 6
		{"TIMESTAMP(12) → cap 6", &ast.TimestampType{Fsp: 12}, "TIMESTAMP(6)"},
		{"TS WITH TZ (LUW)", &ast.DB2TimestampTZType{Fsp: 3, HasFsp: true}, "TIMESTAMPTZ(3)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mapDB2Type(c.in, "col", Caps{})
			if r.PG != c.want {
				t.Errorf("PG = %q, want %q (note=%q)", r.PG, c.want, r.Note)
			}
		})
	}
}

func TestDB2_BoolXmlRowID(t *testing.T) {
	if r := mapDB2Type(&ast.DB2BooleanType{}, "active", Caps{}); r.PG != "BOOLEAN" {
		t.Errorf("BOOLEAN: PG = %q", r.PG)
	}
	if r := mapDB2Type(&ast.DB2XmlType{}, "doc", Caps{}); r.PG != "xml" {
		t.Errorf("XML: PG = %q", r.PG)
	}
	r := mapDB2Type(&ast.DB2RowIDType{}, "rid", Caps{})
	if r.PG != "BIGINT" {
		t.Errorf("ROWID: PG = %q", r.PG)
	}
	if !strings.Contains(strings.ToLower(r.Note), "surrogate") {
		t.Errorf("ROWID note should mention surrogate: %q", r.Note)
	}
}

func TestDB2zOS_BooleanFallsBackToSmallint(t *testing.T) {
	r := mapDB2zOSType(&ast.DB2BooleanType{}, "active", Caps{})
	if r.PG != "SMALLINT" {
		t.Errorf("BOOLEAN on z/OS: PG = %q, want SMALLINT", r.PG)
	}
	if !strings.Contains(r.Check, "IN (0,1)") {
		t.Errorf("z/OS BOOLEAN should emit a 0/1 CHECK: %q", r.Check)
	}
}

func TestDB2zOS_TimestampTZDropsTimeZone(t *testing.T) {
	r := mapDB2zOSType(&ast.DB2TimestampTZType{Fsp: 6, HasFsp: true}, "ts", Caps{})
	if !strings.HasPrefix(r.PG, "TIMESTAMP(") {
		t.Errorf("z/OS TS WITH TZ should fall back to plain TIMESTAMP: %q", r.PG)
	}
	if r.WarningSeverity != SeverityInfo {
		t.Errorf("z/OS TS WITH TZ should warn at info level, got %q", r.WarningSeverity)
	}
}

func TestDB2_BodyRewrite_NvlAndCurrentRegisters(t *testing.T) {
	in := `SELECT NVL(name, 'unknown'), CURRENT DATE FROM SYSIBM.SYSDUMMY1 WITH UR`
	out := rewriteDB2Body(in)
	if !strings.Contains(out, "COALESCE(") {
		t.Errorf("NVL should rewrite to COALESCE: %q", out)
	}
	if !strings.Contains(out, "CURRENT_DATE") {
		t.Errorf("CURRENT DATE should rewrite to CURRENT_DATE: %q", out)
	}
	if strings.Contains(out, "WITH UR") {
		t.Errorf("WITH UR isolation hint should be stripped: %q", out)
	}
}
