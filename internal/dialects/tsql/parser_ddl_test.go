package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestCreateTable_Columns(t *testing.T) {
	ct := mustParseOne[*ast.CreateTable](t, `
CREATE TABLE [Sales].[SalesOrderDetail](
    [SalesOrderID] [int] IDENTITY (100, 5) NOT FOR REPLICATION NOT NULL,
    [Name] [dbo].[Name] NOT NULL,
    [Note] nvarchar(max) COLLATE Latin1_General_CI_AS SPARSE NULL,
    [UnitPrice] [money] NOT NULL CONSTRAINT [DF_UnitPrice] DEFAULT (0.0),
    [LineTotal] AS ISNULL([UnitPrice] * [OrderQty], 0.0),
    [TotalDue] AS ([UnitPrice] + 1) PERSISTED NOT NULL,
    [rowguid] uniqueidentifier ROWGUIDCOL NOT NULL DEFAULT NEWID(),
    [Flag] bit NULL,
) ON [PRIMARY] TEXTIMAGE_ON [PRIMARY];`)
	if ct.Schema != "Sales" || ct.Name != "SalesOrderDetail" || !ct.NameBacktick {
		t.Fatalf("name: %+v", ct)
	}
	if len(ct.Columns) != 8 {
		t.Fatalf("want 8 columns, got %d", len(ct.Columns))
	}
	id := ct.Columns[0]
	if !id.AutoInc || !id.HasIdentityArgs || id.IdentitySeed != 100 || id.IdentityIncrement != 5 || !id.NotForReplication || !id.NotNull {
		t.Errorf("identity column: %+v", id)
	}
	if udt := as[*ast.UserDefinedType](t, ct.Columns[1].Type); udt.Schema != "dbo" || udt.Name != "Name" {
		t.Errorf("alias type: %+v", udt)
	}
	note := ct.Columns[2]
	if ch := as[*ast.CharType](t, note.Type); ch.Name != "NVARCHAR" || !ch.Max {
		t.Errorf("nvarchar(max): %+v", ch)
	}
	if note.Collation != "Latin1_General_CI_AS" || !note.Sparse || note.NotNull || !note.HasNullable {
		t.Errorf("note attributes: %+v", note)
	}
	price := ct.Columns[3]
	if !price.HasDefault || price.DefaultName != "DF_UnitPrice" {
		t.Errorf("named default: %+v", price)
	}
	as[*ast.ParenExpr](t, price.Default)
	virt := ct.Columns[4]
	if virt.Type != nil || virt.Generated == nil || !virt.Generated.Virtual || !virt.Generated.HasVirtual {
		t.Errorf("computed column: %+v", virt)
	}
	as[*ast.FuncCall](t, virt.Generated.Expr)
	pers := ct.Columns[5]
	if pers.Generated == nil || pers.Generated.Virtual || !pers.NotNull {
		t.Errorf("persisted column: %+v %+v", pers, pers.Generated)
	}
	if g := ct.Columns[6]; !g.RowGUIDCol || !g.HasDefault || g.DefaultName != "" {
		t.Errorf("rowguid column: %+v", g)
	}
	if bt := as[*ast.TSQLType](t, ct.Columns[7].Type); bt.Name != "BIT" {
		t.Errorf("bit: %+v", bt)
	}
	if len(ct.Options.Extras) != 2 || ct.Options.Extras[0] != "ON=PRIMARY" || ct.Options.Extras[1] != "TEXTIMAGE_ON=PRIMARY" {
		t.Errorf("options: %v", ct.Options.Extras)
	}
}

func TestCreateTable_Constraints(t *testing.T) {
	ct := mustParseOne[*ast.CreateTable](t, `
CREATE TABLE dbo.Orders (
    OrderID int CONSTRAINT PK_Orders PRIMARY KEY CLUSTERED,
    CustomerID nchar(5) NULL REFERENCES dbo.Customers(CustomerID),
    Qty smallint NOT NULL CHECK (Qty > 0),
    Code char(4) NOT NULL CONSTRAINT UQ_Code UNIQUE NONCLUSTERED,
    CONSTRAINT FK_Orders_Emp FOREIGN KEY (EmployeeID, X) REFERENCES dbo.Employees (EmployeeID, X)
        ON DELETE CASCADE ON UPDATE SET NULL NOT FOR REPLICATION,
    CONSTRAINT CK_Dates CHECK NOT FOR REPLICATION ([ShippedDate] >= [OrderDate]),
    CONSTRAINT PK2 PRIMARY KEY NONCLUSTERED (OrderID DESC, Code) WITH (FILLFACTOR = 80) ON [PRIMARY]
)`)
	if len(ct.Columns) != 4 || len(ct.Constraints) != 7 {
		t.Fatalf("want 4 columns / 7 constraints, got %d / %d", len(ct.Columns), len(ct.Constraints))
	}
	pk := as[*ast.PKConstraint](t, ct.Constraints[0])
	if pk.Name != "PK_Orders" || pk.Clustered != "CLUSTERED" || len(pk.Columns) != 1 || pk.Columns[0].Name != "OrderID" {
		t.Errorf("column-level PK: %+v", pk)
	}
	fk := as[*ast.FKConstraint](t, ct.Constraints[1])
	if fk.Name != "" || fk.Columns[0] != "CustomerID" || fk.RefSchema != "dbo" || fk.RefTable != "Customers" || fk.RefColumns[0] != "CustomerID" {
		t.Errorf("column-level FK: %+v", fk)
	}
	as[*ast.CheckConstraint](t, ct.Constraints[2])
	uq := as[*ast.UQConstraint](t, ct.Constraints[3])
	if uq.Name != "UQ_Code" || uq.Clustered != "NONCLUSTERED" {
		t.Errorf("column-level UNIQUE: %+v", uq)
	}
	fk2 := as[*ast.FKConstraint](t, ct.Constraints[4])
	if fk2.OnDelete != "CASCADE" || fk2.OnUpdate != "SET NULL" || !fk2.NotForReplication || len(fk2.Columns) != 2 {
		t.Errorf("table FK: %+v", fk2)
	}
	ck := as[*ast.CheckConstraint](t, ct.Constraints[5])
	if ck.Name != "CK_Dates" || !ck.NotForReplication || !ck.Enforced {
		t.Errorf("table CHECK: %+v", ck)
	}
	as[*ast.BinaryExpr](t, ck.Expr)
	pk2 := as[*ast.PKConstraint](t, ct.Constraints[6])
	if len(pk2.Columns) != 2 || pk2.Columns[0].Order != "DESC" {
		t.Errorf("table PK: %+v", pk2)
	}
}

func TestCreateTable_SystemVersioning(t *testing.T) {
	ct := mustParseOne[*ast.CreateTable](t, `
CREATE TABLE dbo.Emp (
    id int NOT NULL PRIMARY KEY,
    ValidFrom datetime2(0) GENERATED ALWAYS AS ROW START HIDDEN NOT NULL,
    ValidTo datetime2(0) GENERATED ALWAYS AS ROW END NOT NULL,
    PERIOD FOR SYSTEM_TIME (ValidFrom, ValidTo)
) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.EmpHistory, DATA_CONSISTENCY_CHECK = ON), DATA_COMPRESSION = PAGE)`)
	if !ct.SystemVersioned || ct.HistoryTable == nil || ct.HistoryTable.Schema != "dbo" || ct.HistoryTable.Name != "EmpHistory" {
		t.Errorf("system versioning: %+v history %+v", ct, ct.HistoryTable)
	}
	if len(ct.Periods) != 1 || ct.Periods[0] != (ast.PeriodDef{Name: "SYSTEM_TIME", StartCol: "ValidFrom", EndCol: "ValidTo"}) {
		t.Errorf("periods: %+v", ct.Periods)
	}
	from := ct.Columns[1]
	if !from.SystemVersioning || from.PeriodRow != "START" || !from.Invisible {
		t.Errorf("ROW START column: %+v", from)
	}
	if dt := as[*ast.TSQLType](t, from.Type); dt.Name != "DATETIME2" || dt.Precision != 0 || !dt.HasPrecision {
		t.Errorf("datetime2(0): %+v", dt)
	}
	if ct.Columns[2].PeriodRow != "END" {
		t.Errorf("ROW END column: %+v", ct.Columns[2])
	}
	if len(ct.Options.Extras) != 2 || ct.Options.Extras[1] != "DATA_COMPRESSION = PAGE" {
		t.Errorf("extras: %q", ct.Options.Extras)
	}
}

func TestCreateTable_TempAndInlineIndex(t *testing.T) {
	ct := mustParseOne[*ast.CreateTable](t, "CREATE TABLE #work (a int, INDEX ix_a NONCLUSTERED (a))")
	if !ct.Temporary || ct.Name != "#work" || len(ct.Indexes) != 1 || ct.Indexes[0].Name != "ix_a" {
		t.Errorf("temp table: %+v", ct)
	}
}

func TestDataTypes(t *testing.T) {
	cases := []struct {
		src   string
		check func(t *testing.T, dt ast.DataType)
	}{
		{"int", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.IntType](t, dt).Name, "INT") }},
		{"[bigint]", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.IntType](t, dt).Name, "BIGINT") }},
		{`"tinyint"`, func(t *testing.T, dt ast.DataType) { want(t, as[*ast.IntType](t, dt).Name, "TINYINT") }},
		{"decimal(8, 2)", func(t *testing.T, dt ast.DataType) {
			d := as[*ast.DecimalType](t, dt)
			if d.Precision != 8 || d.Scale != 2 || !d.HasPrec {
				t.Errorf("%+v", d)
			}
		}},
		{"numeric", func(t *testing.T, dt ast.DataType) {
			d := as[*ast.DecimalType](t, dt)
			if d.Name != "NUMERIC" || d.Precision != 18 || d.HasPrec {
				t.Errorf("%+v", d)
			}
		}},
		{"float(24)", func(t *testing.T, dt ast.DataType) {
			if f := as[*ast.FloatType](t, dt); f.Precision != 24 || !f.HasPS {
				t.Errorf("%+v", f)
			}
		}},
		{"double precision", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.FloatType](t, dt).Name, "FLOAT") }},
		{"real", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.FloatType](t, dt).Name, "REAL") }},
		{"varchar(50)", func(t *testing.T, dt ast.DataType) {
			if c := as[*ast.CharType](t, dt); c.Name != "VARCHAR" || c.Length != 50 || !c.HasLength || c.Max {
				t.Errorf("%+v", c)
			}
		}},
		{"[nvarchar](max)", func(t *testing.T, dt ast.DataType) {
			if c := as[*ast.CharType](t, dt); c.Name != "NVARCHAR" || !c.Max {
				t.Errorf("%+v", c)
			}
		}},
		{"national character varying(10)", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.CharType](t, dt).Name, "NVARCHAR") }},
		{"varbinary(max)", func(t *testing.T, dt ast.DataType) {
			if b := as[*ast.BinaryType](t, dt); b.Name != "VARBINARY" || !b.Max {
				t.Errorf("%+v", b)
			}
		}},
		{"binary(16)", func(t *testing.T, dt ast.DataType) { wantInt(t, as[*ast.BinaryType](t, dt).Length, 16) }},
		{"text", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TextType](t, dt).Name, "TEXT") }},
		{"ntext", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "NTEXT") }},
		{"image", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "IMAGE") }},
		{"date", func(t *testing.T, dt ast.DataType) { as[*ast.DateType](t, dt) }},
		{"time", func(t *testing.T, dt ast.DataType) { wantInt(t, as[*ast.TimeType](t, dt).Fsp, 7) }},
		{"time(3)", func(t *testing.T, dt ast.DataType) { wantInt(t, as[*ast.TimeType](t, dt).Fsp, 3) }},
		{"datetime", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "DATETIME") }},
		{"smalldatetime", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "SMALLDATETIME") }},
		{"datetime2", func(t *testing.T, dt ast.DataType) {
			if d := as[*ast.TSQLType](t, dt); d.Name != "DATETIME2" || d.Precision != 7 || d.HasPrecision {
				t.Errorf("%+v", d)
			}
		}},
		{"datetimeoffset(3)", func(t *testing.T, dt ast.DataType) { wantInt(t, as[*ast.TSQLType](t, dt).Precision, 3) }},
		{"money", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "MONEY") }},
		{"smallmoney", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "SMALLMONEY") }},
		{"uniqueidentifier", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "UNIQUEIDENTIFIER") }},
		{"rowversion", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "ROWVERSION") }},
		{"timestamp", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "TIMESTAMP") }},
		{"hierarchyid", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "HIERARCHYID") }},
		{"sql_variant", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "SQL_VARIANT") }},
		{"geography", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "GEOGRAPHY") }},
		{"geometry", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "GEOMETRY") }},
		{"sysname", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "SYSNAME") }},
		{"xml", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "XML") }},
		{"xml(CONTENT Person.InfoSchema)", func(t *testing.T, dt ast.DataType) {
			if x := as[*ast.TSQLType](t, dt); x.XMLSchema != "Person.InfoSchema" || x.XMLDocument {
				t.Errorf("%+v", x)
			}
		}},
		{"[Flag]", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.UserDefinedType](t, dt).Name, "Flag") }},
		{"sys.sysname", func(t *testing.T, dt ast.DataType) { want(t, as[*ast.TSQLType](t, dt).Name, "SYSNAME") }},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			ct := mustParseOne[*ast.CreateTable](t, "CREATE TABLE t (c "+c.src+")")
			c.check(t, ct.Columns[0].Type)
		})
	}
}

func want(t *testing.T, got, w string) {
	t.Helper()
	if got != w {
		t.Errorf("want %q, got %q", w, got)
	}
}

func wantInt(t *testing.T, got, w int) {
	t.Helper()
	if got != w {
		t.Errorf("want %d, got %d", w, got)
	}
}

func TestCreateIndex(t *testing.T) {
	ix := mustParseOne[*ast.CreateIndex](t, `CREATE UNIQUE NONCLUSTERED INDEX [IX_a] ON [Sales].[T] ([a] ASC, [b] DESC)
INCLUDE ([c], [d]) WHERE [a] IS NOT NULL WITH (FILLFACTOR = 90, ONLINE = ON) ON [PRIMARY]`)
	if ix.Name != "IX_a" || !ix.Unique || ix.Clustered != "NONCLUSTERED" || ix.Table.Schema != "Sales" || ix.Table.Name != "T" {
		t.Errorf("index: %+v", ix)
	}
	if len(ix.Columns) != 2 || ix.Columns[1].Order != "DESC" {
		t.Errorf("columns: %+v", ix.Columns)
	}
	if len(ix.Include) != 2 || ix.Include[1] != "d" {
		t.Errorf("include: %v", ix.Include)
	}
	if be := as[*ast.BinaryExpr](t, ix.Where); be.Op != "IS NOT" {
		t.Errorf("filter: %+v", be)
	}
	if len(ix.Options) != 2 {
		t.Errorf("options: %q", ix.Options)
	}
	cl := mustParseOne[*ast.CreateIndex](t, "CREATE CLUSTERED INDEX employee_ind ON employee(lname, fname, minit)")
	if cl.Clustered != "CLUSTERED" || cl.Unique || len(cl.Columns) != 3 {
		t.Errorf("clustered index: %+v", cl)
	}
	n := mustParseOne[*ast.NoopStmt](t, "CREATE NONCLUSTERED COLUMNSTORE INDEX ix ON t (a)")
	want(t, n.Kind, "CREATE COLUMNSTORE INDEX")
}

func TestAlterTable(t *testing.T) {
	stmts := mustParse(t, `
ALTER TABLE [dbo].[ErrorLog] WITH CHECK ADD CONSTRAINT [PK_ErrorLog] PRIMARY KEY CLUSTERED ([ErrorLogID]) ON [PRIMARY];
ALTER TABLE t WITH NOCHECK ADD CONSTRAINT FK_a FOREIGN KEY (a) REFERENCES u (a), CONSTRAINT CK_b CHECK (b > 0);
ALTER TABLE t ADD CONSTRAINT [DF_t_c] DEFAULT (GETDATE()) FOR [c];
ALTER TABLE t ADD DEFAULT 0 FOR d WITH VALUES;
ALTER TABLE t ADD e int NULL, f varchar(10) NOT NULL CONSTRAINT UQ_f UNIQUE;
ALTER TABLE t ALTER COLUMN g nvarchar(100) NOT NULL;
ALTER TABLE t DROP CONSTRAINT FK_a, COLUMN e, f;
ALTER TABLE "Categories" NOCHECK CONSTRAINT ALL;
ALTER TABLE t DISABLE TRIGGER ALL;`)
	if len(stmts) != 9 {
		t.Fatalf("want 9 statements, got %d", len(stmts))
	}
	a0 := as[*ast.AlterTable](t, stmts[0])
	if a0.Table.Name != "ErrorLog" || len(a0.Actions) != 1 || a0.Actions[0].Kind != "ADD_CONSTRAINT" {
		t.Fatalf("PK alter: %+v", a0)
	}
	as[*ast.PKConstraint](t, a0.Actions[0].Constraint)
	a1 := as[*ast.AlterTable](t, stmts[1])
	if len(a1.Actions) != 2 {
		t.Fatalf("two constraints: %+v", a1.Actions)
	}
	if fk := as[*ast.FKConstraint](t, a1.Actions[0].Constraint); !fk.State.NoValidate {
		t.Errorf("WITH NOCHECK FK must be NoValidate: %+v", fk)
	}
	if ck := as[*ast.CheckConstraint](t, a1.Actions[1].Constraint); !ck.State.NoValidate {
		t.Errorf("WITH NOCHECK CHECK must be NoValidate: %+v", ck)
	}
	d := as[*ast.AlterTable](t, stmts[2]).Actions[0]
	if d.Kind != "SET_DEFAULT" || d.DropName != "c" || d.ConstraintName != "DF_t_c" || d.DefaultExpr != "(GETDATE())" {
		t.Errorf("named default: %+v", d)
	}
	as[*ast.ParenExpr](t, d.Default)
	d2 := as[*ast.AlterTable](t, stmts[3]).Actions[0]
	if d2.Kind != "SET_DEFAULT" || d2.DropName != "d" || d2.ConstraintName != "" {
		t.Errorf("unnamed default: %+v", d2)
	}
	add := as[*ast.AlterTable](t, stmts[4]).Actions
	if len(add) != 3 || add[0].Kind != "ADD_COLUMN" || add[1].Kind != "ADD_COLUMN" || add[2].Kind != "ADD_CONSTRAINT" {
		t.Errorf("add columns: %+v", add)
	}
	mod := as[*ast.AlterTable](t, stmts[5]).Actions[0]
	if mod.Kind != "MODIFY_COLUMN" || mod.Column.Name != "g" || !mod.Column.NotNull {
		t.Errorf("alter column: %+v", mod)
	}
	drops := as[*ast.AlterTable](t, stmts[6]).Actions
	if len(drops) != 3 || drops[0].Kind != "DROP_CONSTRAINT" || drops[1].Kind != "DROP_COLUMN" || drops[2].Kind != "DROP_COLUMN" || drops[2].DropName != "f" {
		t.Errorf("drops: %+v", drops)
	}
	for _, i := range []int{7, 8} {
		if a := as[*ast.AlterTable](t, stmts[i]).Actions; len(a) != 1 || a[0].Kind != "NOOP" {
			t.Errorf("stmt %d: want one NOOP action, got %+v", i, a)
		}
	}
}

func TestCreateView(t *testing.T) {
	stmts := mustParse(t, `CREATE VIEW [Person].[vState] (a, b)
WITH SCHEMABINDING, VIEW_METADATA
AS
SELECT sp.[StateProvinceID], cr.[Name]
FROM [Person].[StateProvince] sp INNER JOIN [Person].[CountryRegion] cr ON sp.c = cr.c
WITH CHECK OPTION;
GO
create view "Current Product List" AS
SELECT ProductID FROM Products
--ORDER BY x
GO`)
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d", len(stmts))
	}
	v := as[*ast.CreateView](t, stmts[0])
	if v.View.Schema != "Person" || v.View.Name != "vState" || !v.SchemaBinding || len(v.ViewOptions) != 1 || v.CheckOption != "CHECK OPTION" {
		t.Errorf("view header: %+v", v)
	}
	if len(v.Columns) != 2 || v.Select == nil || v.SelectParseError != "" {
		t.Fatalf("view body: %+v", v)
	}
	if v.SelectBody[:6] != "SELECT" || v.SelectBody[len(v.SelectBody)-4:] != "cr.c" {
		t.Errorf("SelectBody: %q", v.SelectBody)
	}
	v2 := as[*ast.CreateView](t, stmts[1])
	if v2.View.Name != "Current Product List" || v2.Select == nil {
		t.Errorf("quoted view: %+v", v2)
	}
	alt := mustParseOne[*ast.CreateView](t, "ALTER VIEW v AS SELECT 1 AS x")
	if !alt.OrAlter || !alt.OrReplace {
		t.Errorf("ALTER VIEW: %+v", alt)
	}
}

func TestCreateView_BadBodyKeepsBoundary(t *testing.T) {
	stmts, errs := Parse("CREATE VIEW v AS SELECT FROM WHERE\nGO\nCREATE TABLE t (a int)\nGO")
	if len(errs) != 0 {
		t.Fatalf("a view body failure must stay on the node, got %v", errs)
	}
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d", len(stmts))
	}
	v := as[*ast.CreateView](t, stmts[0])
	if v.Select != nil || v.SelectParseError == "" {
		t.Errorf("want untyped body with an error, got %+v", v)
	}
	as[*ast.CreateTable](t, stmts[1])
}

func TestCreateProcedure(t *testing.T) {
	p := mustParseOne[*ast.CreateProcedure](t, `CREATE PROCEDURE [dbo].[uspLogError]
    @ErrorLogID [int] = 0 OUTPUT, -- contains the ErrorLogID
    @Name nvarchar(50) = N'x',
    @Lines dbo.OrderLineList READONLY,
    @language[int]=0
WITH EXECUTE AS CALLER, RECOMPILE
AS
BEGIN
    SET NOCOUNT ON;
END;
GO`)
	if p.Schema != "dbo" || p.Name != "uspLogError" || len(p.Params) != 4 {
		t.Fatalf("procedure header: %+v", p)
	}
	e := p.Params[0]
	if e.Name != "@ErrorLogID" || !e.Output || e.Direction != "INOUT" || e.Default == nil {
		t.Errorf("OUTPUT param: %+v", e)
	}
	if n := p.Params[1]; n.Direction != "IN" || as[*ast.Literal](t, n.Default).Text != "x" {
		t.Errorf("default param: %+v", n)
	}
	if l := p.Params[2]; !l.ReadOnly || as[*ast.UserDefinedType](t, l.Type).Name != "OrderLineList" {
		t.Errorf("table-valued param: %+v", l)
	}
	if len(p.Options) != 2 || p.Options[0] != "EXECUTE AS CALLER" {
		t.Errorf("options: %q", p.Options)
	}
	want(t, p.Body, "BEGIN\n    SET NOCOUNT ON;\nEND;")

	bare := mustParseOne[*ast.CreateProcedure](t, "create procedure \"Sales by Year\" \n\t@Beginning_Date DateTime, @Ending_Date DateTime AS\nSELECT 1\nGO")
	if bare.Name != "Sales by Year" || len(bare.Params) != 2 || bare.Body != "SELECT 1" {
		t.Errorf("unparenthesised procedure: %+v", bare)
	}
	alt := mustParseOne[*ast.CreateProcedure](t, "CREATE OR ALTER PROC p (@a int) AS RETURN @a")
	if !alt.OrAlter || len(alt.Params) != 1 || alt.Body != "RETURN @a" {
		t.Errorf("CREATE OR ALTER PROC: %+v", alt)
	}
}

func TestCreateFunction(t *testing.T) {
	stmts := mustParse(t, `CREATE FUNCTION [dbo].[ufnLeadingZeros](@Value int)
RETURNS varchar(8)
WITH SCHEMABINDING
AS
BEGIN
    RETURN (@Value);
END;
GO
CREATE FUNCTION sales.fn_orders_of (@customer_id int)
RETURNS TABLE
AS
RETURN (SELECT o.id FROM sales.Orders o WHERE o.customer_id = @customer_id);
GO
CREATE FUNCTION [dbo].[ufnGetContactInformation](@PersonID int)
RETURNS @retContactInformation TABLE
(
    [PersonID] int NOT NULL PRIMARY KEY,
    [FirstName] [nvarchar](50) NULL
)
AS
BEGIN
    RETURN;
END;
GO`)
	if len(stmts) != 3 {
		t.Fatalf("want 3 statements, got %d", len(stmts))
	}
	sc := as[*ast.CreateFunction](t, stmts[0])
	if sc.Schema != "dbo" || sc.Name != "ufnLeadingZeros" || len(sc.Params) != 1 || sc.Params[0].Direction != "" || len(sc.Options) != 1 {
		t.Errorf("scalar header: %+v", sc)
	}
	if c := as[*ast.CharType](t, sc.Returns); c.Length != 8 {
		t.Errorf("returns: %+v", c)
	}
	inl := as[*ast.CreateFunction](t, stmts[1])
	if !inl.ReturnsInlineTable || inl.Returns != nil || inl.Select == nil || inl.SelectParseError != "" {
		t.Fatalf("inline TVF: %+v", inl)
	}
	if inl.Body[:6] != "RETURN" {
		t.Errorf("inline body: %q", inl.Body)
	}
	mt := as[*ast.CreateFunction](t, stmts[2])
	if mt.ReturnsTableVar != "@retContactInformation" || len(mt.ReturnsTableCols) != 2 || len(mt.ReturnsTableConstraints) != 1 || mt.Returns != nil {
		t.Errorf("multi-statement TVF: %+v", mt)
	}
	if mt.Body != "BEGIN\n    RETURN;\nEND;" {
		t.Errorf("multi-statement body: %q", mt.Body)
	}
	bad := mustParseOne[*ast.CreateFunction](t, "CREATE FUNCTION f() RETURNS TABLE AS RETURN SELECT FROM\nGO")
	if bad.Select != nil || bad.SelectParseError == "" {
		t.Errorf("unparseable inline query must be reported on the node: %+v", bad)
	}
}

func TestCreateTrigger(t *testing.T) {
	stmts := mustParse(t, `CREATE VIEW dbo.vp AS SELECT 1 AS a
GO
CREATE TRIGGER [Sales].[iduSalesOrderDetail] ON [Sales].[SalesOrderDetail]
AFTER INSERT, DELETE, UPDATE NOT FOR REPLICATION AS
BEGIN
    SET NOCOUNT ON;
END;
GO
CREATE TRIGGER employee_insupd
ON employee
FOR insert, UPDATE
AS
declare @x int
GO
CREATE TRIGGER trg ON vp INSTEAD OF INSERT AS SELECT 1
GO
CREATE TRIGGER [ddlLog] ON DATABASE FOR DDL_DATABASE_LEVEL_EVENTS AS
BEGIN
    SET NOCOUNT ON;
END;
GO`)
	if len(stmts) != 5 {
		t.Fatalf("want 5 statements, got %d", len(stmts))
	}
	tr := as[*ast.CreateTrigger](t, stmts[1])
	if tr.Schema != "Sales" || tr.Name != "iduSalesOrderDetail" || tr.Table.Name != "SalesOrderDetail" || tr.Time != "AFTER" ||
		len(tr.Events) != 3 || tr.Events[1] != "DELETE" || tr.Event != "INSERT OR DELETE OR UPDATE" ||
		tr.ForEachRow || !tr.HasForEach || !tr.NotForReplication || tr.OnView {
		t.Errorf("AFTER trigger: %+v", tr)
	}
	if tr.Body[:5] != "BEGIN" {
		t.Errorf("body: %q", tr.Body)
	}
	tf := as[*ast.CreateTrigger](t, stmts[2])
	if tf.Time != "AFTER" || len(tf.Events) != 2 || tf.Events[0] != "INSERT" {
		t.Errorf("FOR trigger: %+v", tf)
	}
	ti := as[*ast.CreateTrigger](t, stmts[3])
	if ti.Time != "INSTEAD OF" || !ti.OnView {
		t.Errorf("INSTEAD OF trigger on a view: %+v", ti)
	}
	ddl := as[*ast.NoopStmt](t, stmts[4])
	want(t, ddl.Kind, "CREATE DDL TRIGGER")
}

func TestCreateTypeSequenceSchema(t *testing.T) {
	stmts := mustParse(t, `CREATE TYPE [Flag] FROM bit NOT NULL;
CREATE TYPE dbo.Phone FROM nvarchar(25) NULL;
CREATE TYPE dbo.OrderLineList AS TABLE (product_id int NOT NULL, qty smallint NOT NULL DEFAULT 1, PRIMARY KEY (product_id));
CREATE SEQUENCE dbo.invoice_seq AS bigint START WITH 1000 INCREMENT BY 1 MINVALUE 1 NO MAXVALUE NO CYCLE CACHE 20;
ALTER SEQUENCE dbo.invoice_seq RESTART WITH 5000 INCREMENT BY -2 NO CACHE;
CREATE SCHEMA [HumanResources] AUTHORIZATION [dbo];
CREATE SCHEMA sales;`)
	if len(stmts) != 7 {
		t.Fatalf("want 7 statements, got %d", len(stmts))
	}
	f := as[*ast.CreateType](t, stmts[0])
	if f.Kind != "alias" || !f.NotNull || !f.HasNullable || as[*ast.TSQLType](t, f.BaseType).Name != "BIT" {
		t.Errorf("alias type: %+v", f)
	}
	ph := as[*ast.CreateType](t, stmts[1])
	if ph.Schema != "dbo" || ph.NotNull || !ph.HasNullable {
		t.Errorf("nullable alias type: %+v", ph)
	}
	tt := as[*ast.CreateType](t, stmts[2])
	if tt.Kind != "table" || len(tt.TableCols) != 2 || len(tt.TableConstraints) != 1 {
		t.Errorf("table type: %+v", tt)
	}
	sq := as[*ast.CreateSequence](t, stmts[3])
	if sq.Schema != "dbo" || as[*ast.IntType](t, sq.AsType).Name != "BIGINT" || sq.Start != 1000 || !sq.HasStart ||
		sq.Increment != 1 || sq.MinValue != 1 || !sq.NoMax || !sq.HasCycle || sq.Cycle || sq.Cache != 20 {
		t.Errorf("sequence: %+v", sq)
	}
	as2 := as[*ast.AlterSequence](t, stmts[4])
	if !as2.HasRestart || as2.StartWith != 5000 || !as2.HasStartWith || as2.Increment != -2 || !as2.NoCache {
		t.Errorf("alter sequence: %+v", as2)
	}
	sc := as[*ast.CreateSchema](t, stmts[5])
	if sc.Name != "HumanResources" || sc.Authorization != "dbo" {
		t.Errorf("schema: %+v", sc)
	}
	want(t, as[*ast.CreateSchema](t, stmts[6]).Name, "sales")
}

func TestExtendedProperties(t *testing.T) {
	stmts := mustParse(t, `
EXECUTE [sys].[sp_addextendedproperty] N'MS_Description', N'Date and time.', N'SCHEMA', [Person], N'TABLE', [StateProvince], N'COLUMN', [ModifiedDate];
EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'Customers table.',
     @level0type = N'SCHEMA', @level0name = N'dbo', @level1type = N'TABLE', @level1name = N'Customers';
exec sp_addextendedproperty 'MS_Description', 'A view', 'SCHEMA', 'dbo', 'VIEW', 'v', NULL, NULL
EXECUTE sp_addextendedproperty N'MS_Description', N'Sample database', NULL, NULL, NULL, NULL;
EXECUTE sp_addextendedproperty N'MS_Description', N'Primary filegroup.', N'FILEGROUP', [PRIMARY];
EXEC sys.sp_addextendedproperty @name = N'Caption', @value = N'x', @level0type = N'SCHEMA', @level0name = N'dbo';
EXEC sys.sp_addextendedproperty N'MS_Description', NULL, N'SCHEMA', N'dbo';`)
	if len(stmts) != 7 {
		t.Fatalf("want 7 statements, got %d", len(stmts))
	}
	c0 := as[*ast.CommentStmt](t, stmts[0])
	if c0.ObjectKind != "COLUMN" || c0.ParentKind != "TABLE" || c0.Schema != "Person" || c0.Object != "StateProvince" || c0.Column != "ModifiedDate" || c0.Text != "Date and time." {
		t.Errorf("column comment: %+v", c0)
	}
	c1 := as[*ast.CommentStmt](t, stmts[1])
	if c1.ObjectKind != "TABLE" || c1.Schema != "dbo" || c1.Object != "Customers" || c1.Column != "" || c1.Text != "Customers table." {
		t.Errorf("named-argument table comment: %+v", c1)
	}
	c2 := as[*ast.CommentStmt](t, stmts[2])
	if c2.ObjectKind != "VIEW" || c2.Object != "v" {
		t.Errorf("view comment: %+v", c2)
	}
	if c3 := as[*ast.CommentStmt](t, stmts[3]); c3.ObjectKind != "DATABASE" || c3.Text != "Sample database" {
		t.Errorf("database comment: %+v", c3)
	}
	if c4 := as[*ast.CommentStmt](t, stmts[4]); c4.ObjectKind != "FILEGROUP" || c4.Object != "PRIMARY" || c4.Schema != "" {
		t.Errorf("filegroup comment: %+v", c4)
	}
	as[*ast.NoopStmt](t, stmts[5])
	if c6 := as[*ast.CommentStmt](t, stmts[6]); !c6.IsNull || c6.ObjectKind != "SCHEMA" || c6.Schema != "dbo" {
		t.Errorf("NULL comment: %+v", c6)
	}
}

func TestDrop(t *testing.T) {
	stmts := mustParse(t, `DROP TABLE IF EXISTS dbo.a, b;
drop procedure "dbo"."Sales by Year"
DROP VIEW IF EXISTS v1, s.v2;
DROP INDEX IX_a ON dbo.t;
DROP INDEX t.IX_b;
DROP FUNCTION dbo.f;
DROP TRIGGER trg;
DROP TYPE dbo.Flag;
DROP SEQUENCE s;
DROP SCHEMA sales;
DROP DATABASE pubs`)
	if len(stmts) != 11 {
		t.Fatalf("want 11 statements, got %d", len(stmts))
	}
	dt := as[*ast.DropTable](t, stmts[0])
	if !dt.IfExists || len(dt.Tables) != 2 || dt.Tables[0].Schema != "dbo" {
		t.Errorf("drop table: %+v", dt)
	}
	dp := as[*ast.DropObject](t, stmts[1])
	if dp.Kind != "PROCEDURE" || dp.Schema != "dbo" || dp.Name != "Sales by Year" {
		t.Errorf("drop procedure: %+v", dp)
	}
	dv := as[*ast.DropObject](t, stmts[2])
	if dv.Kind != "VIEW" || !dv.IfExists || len(dv.Names) != 2 {
		t.Errorf("drop views: %+v", dv)
	}
	di := as[*ast.DropObject](t, stmts[3])
	if di.Kind != "INDEX" || di.Name != "IX_a" || di.OnTable.Name != "t" {
		t.Errorf("drop index: %+v", di)
	}
	dl := as[*ast.DropObject](t, stmts[4])
	if dl.Name != "IX_b" || dl.OnTable.Name != "t" {
		t.Errorf("legacy drop index: %+v", dl)
	}
	for i, k := range []string{"FUNCTION", "TRIGGER", "TYPE", "SEQUENCE", "SCHEMA"} {
		want(t, as[*ast.DropObject](t, stmts[5+i]).Kind, k)
	}
	want(t, as[*ast.NoopStmt](t, stmts[10]).Kind, "DROP DATABASE")
}

func TestScriptLevelNoops(t *testing.T) {
	src := ` :setvar DatabaseName "AdventureWorks"
GO
USE [master];
SET NOCOUNT ON
set quoted_identifier on
SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
SET IDENTITY_INSERT "Categories" ON
PRINT 'Started - ' + CONVERT(varchar, GETDATE(), 121);
IF OBJECT_ID('dbo.t') IS NOT NULL DROP TABLE dbo.t;
if exists (select * from sysobjects where id = object_id('dbo.v') and sysstat & 0xf = 2)
	drop view "dbo"."v"
GO
BEGIN TRANSACTION
INSERT "Categories"("CategoryID","CategoryName") VALUES(1,'Beverages')
Insert Into Region Values (2,'Western')
COMMIT
BULK INSERT [Person].[Address] FROM '$(Path)Address.csv' WITH (CODEPAGE = '65001', TABLOCK);
CREATE XML SCHEMA COLLECTION [Person].[S] AS N'<xsd:schema/>';
CREATE PARTITION FUNCTION pf (int) AS RANGE LEFT FOR VALUES (1, 100);
CREATE PARTITION SCHEME ps AS PARTITION pf ALL TO ([PRIMARY]);
CREATE SYNONYM dbo.AllOrders FOR sales.Orders;
CREATE FULLTEXT INDEX ON Production.Document(Document TYPE COLUMN FileExtension) KEY INDEX PK_Doc;
ALTER DATABASE $(DatabaseName)
SET RECOVERY SIMPLE,
    ANSI_NULLS ON;
CREATE DATABASE $(DatabaseName);
UPDATE STATISTICS publishers
CHECKPOINT
DECLARE @x int = 1
EXEC dbo.p 1, 2
sp_helpdb
GO
`
	stmts := mustParse(t, src)
	wantKinds := []string{
		":SETVAR", "USE", "SET", "SET", "SET", "SET", "PRINT", "IF", "IF",
		"BEGIN", "INSERT", "INSERT", "COMMIT", "BULK INSERT", "CREATE XML SCHEMA COLLECTION",
		"CREATE PARTITION FUNCTION", "CREATE PARTITION SCHEME", "CREATE SYNONYM", "CREATE FULLTEXT INDEX",
		"ALTER DATABASE", "CREATE DATABASE", "UPDATE", "CHECKPOINT", "DECLARE", "EXEC", "EXEC",
	}
	if len(stmts) != len(wantKinds) {
		for _, s := range stmts {
			t.Logf("%T %+v", s, s)
		}
		t.Fatalf("want %d statements, got %d", len(wantKinds), len(stmts))
	}
	for i, k := range wantKinds {
		n := as[*ast.NoopStmt](t, stmts[i])
		if n.Kind != k {
			t.Errorf("stmt %d: want Kind %q, got %q (%q)", i, k, n.Kind, n.Text)
		}
	}
	ins := as[*ast.NoopStmt](t, stmts[10])
	want(t, ins.Text, `INSERT "Categories"("CategoryID","CategoryName") VALUES(1,'Beverages')`)
	alter := as[*ast.NoopStmt](t, stmts[19])
	want(t, alter.Text, "ALTER DATABASE $(DatabaseName)\nSET RECOVERY SIMPLE,\n    ANSI_NULLS ON")
}

func TestDialectRegistered(t *testing.T) {
	d := New("sqlserver", "x")
	if d.Kind() != "sqlserver" || d.Name() != "x" {
		t.Errorf("dialect: %+v", d)
	}
	if _, err := d.Parse("CREATE TABLE t (a int)"); err != nil {
		t.Errorf("valid script: %v", err)
	}
	if _, err := d.Parse("CREATE TABLE t (a"); err == nil {
		t.Errorf("invalid script must return an error")
	}
}
