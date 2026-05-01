package db2

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func parseOne(t *testing.T, src string) ast.Stmt {
	t.Helper()
	stmts, errs := Parse(src)
	if len(errs) > 0 {
		t.Fatalf("unexpected parse errors: %v", errs.Error())
	}
	if len(stmts) != 1 {
		t.Fatalf("expected 1 stmt, got %d", len(stmts))
	}
	return stmts[0]
}

func TestCreateTable_BasicTypes(t *testing.T) {
	src := `
		CREATE TABLE squishy.customers (
		    id        INTEGER NOT NULL,
		    name      VARCHAR(40) NOT NULL,
		    balance   DECIMAL(12,2) DEFAULT 0,
		    created   TIMESTAMP(6) NOT NULL,
		    CONSTRAINT pk_cust PRIMARY KEY (id)
		);`
	stmt := parseOne(t, src)
	ct, ok := stmt.(*ast.CreateTable)
	if !ok {
		t.Fatalf("got %T, want *ast.CreateTable", stmt)
	}
	if ct.Schema != "SQUISHY" || ct.Name != "CUSTOMERS" {
		t.Errorf("unexpected qualified name: %q.%q", ct.Schema, ct.Name)
	}
	if len(ct.Columns) != 4 {
		t.Errorf("expected 4 columns, got %d", len(ct.Columns))
	}
	// integer PK
	if _, ok := ct.Columns[0].Type.(*ast.IntType); !ok {
		t.Errorf("col 0 type = %T, want *IntType", ct.Columns[0].Type)
	}
	// VARCHAR(40)
	c1, ok := ct.Columns[1].Type.(*ast.CharType)
	if !ok || c1.Name != "VARCHAR" || c1.Length != 40 {
		t.Errorf("col 1 unexpected: %+v", ct.Columns[1].Type)
	}
	// DECIMAL(12,2) → DecimalType
	c2, ok := ct.Columns[2].Type.(*ast.DecimalType)
	if !ok || c2.Precision != 12 || c2.Scale != 2 {
		t.Errorf("col 2 unexpected: %+v", ct.Columns[2].Type)
	}
	// TIMESTAMP(6)
	c3, ok := ct.Columns[3].Type.(*ast.TimestampType)
	if !ok || c3.Fsp != 6 {
		t.Errorf("col 3 unexpected: %+v", ct.Columns[3].Type)
	}
	if len(ct.Constraints) != 1 {
		t.Fatalf("expected 1 constraint, got %d", len(ct.Constraints))
	}
	pk, ok := ct.Constraints[0].(*ast.PKConstraint)
	if !ok || pk.Name != "PK_CUST" || len(pk.Columns) != 1 {
		t.Errorf("PK unexpected: %+v", ct.Constraints[0])
	}
}

func TestCreateTable_IdentityAndForBitData(t *testing.T) {
	src := `
		CREATE TABLE t (
		    id    BIGINT GENERATED ALWAYS AS IDENTITY (START WITH 1 INCREMENT BY 1) NOT NULL,
		    code  CHAR(2) FOR BIT DATA NOT NULL,
		    PRIMARY KEY (id)
		);`
	ct := parseOne(t, src).(*ast.CreateTable)
	if !ct.Columns[0].AutoInc {
		t.Errorf("expected col 0 (id) to carry AutoInc")
	}
	if _, ok := ct.Columns[1].Type.(*ast.DB2ForBitDataType); !ok {
		t.Errorf("col 1 type = %T, want *DB2ForBitDataType", ct.Columns[1].Type)
	}
}

func TestCreateTable_DecFloatGraphicXmlBoolean(t *testing.T) {
	src := `
		CREATE TABLE t (
		    amount DECFLOAT(34) NOT NULL,
		    notes  VARGRAPHIC(200),
		    payload XML,
		    active BOOLEAN DEFAULT TRUE,
		    rid    ROWID
		);`
	ct := parseOne(t, src).(*ast.CreateTable)
	if df, ok := ct.Columns[0].Type.(*ast.DB2DecFloatType); !ok || df.Width != 34 {
		t.Errorf("col 0 unexpected: %+v", ct.Columns[0].Type)
	}
	if g, ok := ct.Columns[1].Type.(*ast.DB2GraphicType); !ok || g.Name != "VARGRAPHIC" || g.Length != 200 {
		t.Errorf("col 1 unexpected: %+v", ct.Columns[1].Type)
	}
	if _, ok := ct.Columns[2].Type.(*ast.DB2XmlType); !ok {
		t.Errorf("col 2 type = %T, want *DB2XmlType", ct.Columns[2].Type)
	}
	if _, ok := ct.Columns[3].Type.(*ast.DB2BooleanType); !ok {
		t.Errorf("col 3 type = %T, want *DB2BooleanType", ct.Columns[3].Type)
	}
	if _, ok := ct.Columns[4].Type.(*ast.DB2RowIDType); !ok {
		t.Errorf("col 4 type = %T, want *DB2RowIDType", ct.Columns[4].Type)
	}
}

func TestCreateTable_TimestampWithTimeZone(t *testing.T) {
	src := `CREATE TABLE t (at TIMESTAMP(6) WITH TIME ZONE);`
	ct := parseOne(t, src).(*ast.CreateTable)
	tz, ok := ct.Columns[0].Type.(*ast.DB2TimestampTZType)
	if !ok {
		t.Fatalf("col 0 type = %T, want *DB2TimestampTZType", ct.Columns[0].Type)
	}
	if tz.Fsp != 6 {
		t.Errorf("Fsp = %d, want 6", tz.Fsp)
	}
}

func TestCreateTable_FK_OnDeleteCascade(t *testing.T) {
	src := `
		CREATE TABLE child (
		    id INTEGER,
		    parent_id INTEGER,
		    CONSTRAINT fk_child_parent FOREIGN KEY (parent_id)
		        REFERENCES parent (id) ON DELETE CASCADE
		);`
	ct := parseOne(t, src).(*ast.CreateTable)
	if len(ct.Constraints) != 1 {
		t.Fatalf("expected 1 constraint, got %d", len(ct.Constraints))
	}
	fk, ok := ct.Constraints[0].(*ast.FKConstraint)
	if !ok {
		t.Fatalf("got %T, want *FKConstraint", ct.Constraints[0])
	}
	if fk.OnDelete != "CASCADE" {
		t.Errorf("OnDelete = %q, want CASCADE", fk.OnDelete)
	}
	if fk.RefTable != "PARENT" || len(fk.RefColumns) != 1 || fk.RefColumns[0] != "ID" {
		t.Errorf("FK ref unexpected: %+v", fk)
	}
}

func TestCreateIndex(t *testing.T) {
	src := `CREATE UNIQUE INDEX idx_email ON squishy.customers (email ASC);`
	stmt := parseOne(t, src)
	idx, ok := stmt.(*ast.CreateIndex)
	if !ok {
		t.Fatalf("got %T, want *CreateIndex", stmt)
	}
	if !idx.Unique {
		t.Errorf("expected Unique=true")
	}
	if idx.Name != "IDX_EMAIL" {
		t.Errorf("Name = %q", idx.Name)
	}
	if idx.Table.Schema != "SQUISHY" || idx.Table.Name != "CUSTOMERS" {
		t.Errorf("Table = %+v", idx.Table)
	}
	if len(idx.Columns) != 1 || idx.Columns[0].Name != "EMAIL" || idx.Columns[0].Order != "ASC" {
		t.Errorf("Columns = %+v", idx.Columns)
	}
}

func TestCreateView(t *testing.T) {
	src := `CREATE VIEW v_active AS SELECT * FROM customers WHERE is_active = TRUE;`
	stmt := parseOne(t, src)
	v, ok := stmt.(*ast.CreateView)
	if !ok {
		t.Fatalf("got %T, want *CreateView", stmt)
	}
	if v.View.Name != "V_ACTIVE" {
		t.Errorf("View.Name = %q", v.View.Name)
	}
	if !strings.Contains(strings.ToUpper(v.SelectBody), "SELECT") {
		t.Errorf("SelectBody = %q", v.SelectBody)
	}
}

func TestCreateSequence(t *testing.T) {
	src := `CREATE SEQUENCE order_seq AS BIGINT START WITH 1000 INCREMENT BY 1 NO CYCLE NO CACHE;`
	stmt := parseOne(t, src)
	s, ok := stmt.(*ast.CreateSequence)
	if !ok {
		t.Fatalf("got %T, want *CreateSequence", stmt)
	}
	if s.Name != "ORDER_SEQ" {
		t.Errorf("Name = %q", s.Name)
	}
	if !s.HasStart || s.Start != 1000 {
		t.Errorf("Start = %d", s.Start)
	}
	if !s.HasCycle || s.Cycle {
		t.Errorf("expected NOCYCLE")
	}
	if !s.NoCache {
		t.Errorf("expected NoCache=true")
	}
}

func TestCreateProcedure_BeginAtomicHandler(t *testing.T) {
	src := `
		CREATE PROCEDURE p1 (IN x INTEGER)
		LANGUAGE SQL
		MODIFIES SQL DATA
		BEGIN ATOMIC
		    DECLARE v INTEGER;
		    DECLARE EXIT HANDLER FOR SQLEXCEPTION
		        RESIGNAL SQLSTATE '38001' SET MESSAGE_TEXT = 'failed';
		    SET v = x + 1;
		END
		`
	stmt := parseOne(t, src)
	p, ok := stmt.(*ast.CreateProcedure)
	if !ok {
		t.Fatalf("got %T, want *CreateProcedure", stmt)
	}
	if p.Name != "P1" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.Characteristics.Language != "SQL" {
		t.Errorf("Language = %q", p.Characteristics.Language)
	}
	if p.Characteristics.SQLDataAccess != "MODIFIES SQL DATA" {
		t.Errorf("SQLDataAccess = %q", p.Characteristics.SQLDataAccess)
	}
	if !strings.Contains(p.Body, "BEGIN ATOMIC") {
		t.Errorf("Body should preserve BEGIN ATOMIC: %q", p.Body)
	}
	if !strings.Contains(p.Body, "RESIGNAL") {
		t.Errorf("Body should preserve RESIGNAL: %q", p.Body)
	}
}

func TestAlterTable_AddColumn_DropColumn(t *testing.T) {
	src := `ALTER TABLE customers ADD COLUMN nickname VARCHAR(20) DROP COLUMN balance;`
	stmt := parseOne(t, src)
	at, ok := stmt.(*ast.AlterTable)
	if !ok {
		t.Fatalf("got %T, want *AlterTable", stmt)
	}
	if len(at.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(at.Actions))
	}
	if at.Actions[0].Kind != "ADD_COLUMN" || at.Actions[0].Column.Name != "NICKNAME" {
		t.Errorf("action 0 unexpected: %+v", at.Actions[0])
	}
	if at.Actions[1].Kind != "DROP_COLUMN" || at.Actions[1].DropName != "BALANCE" {
		t.Errorf("action 1 unexpected: %+v", at.Actions[1])
	}
}

func TestDropTable_IfExistsCascade(t *testing.T) {
	src := `DROP TABLE IF EXISTS customers CASCADE;`
	stmt := parseOne(t, src)
	dt, ok := stmt.(*ast.DropTable)
	if !ok {
		t.Fatalf("got %T, want *DropTable", stmt)
	}
	if !dt.IfExists {
		t.Errorf("expected IfExists=true")
	}
	if len(dt.Tables) != 1 || dt.Tables[0].Name != "CUSTOMERS" {
		t.Errorf("Tables = %+v", dt.Tables)
	}
}

func TestDML_RawPassthrough(t *testing.T) {
	src := `SELECT * FROM customers WITH UR;`
	stmt := parseOne(t, src)
	n, ok := stmt.(*ast.NoopStmt)
	if !ok {
		t.Fatalf("got %T, want *NoopStmt", stmt)
	}
	if n.Kind != "DML" {
		t.Errorf("Kind = %q, want DML", n.Kind)
	}
	if !strings.Contains(strings.ToUpper(n.Text), "SELECT") {
		t.Errorf("Text should contain SELECT: %q", n.Text)
	}
}
