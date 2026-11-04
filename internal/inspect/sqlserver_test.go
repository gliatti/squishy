package inspect

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSSQuoting(t *testing.T) {
	require.Equal(t, "[Order Details]", ssQuoteIdent("Order Details"))
	require.Equal(t, "[a]]b]", ssQuoteIdent("a]b"))
	require.Equal(t, "[x[y]", ssQuoteIdent("x[y"))
	require.Equal(t, "[Sales].[Order]]s]", ssQuoteName("Sales", "Order]s"))
	require.Equal(t, "[t]", ssQuoteName("", "t"))
	require.Equal(t, "N'it''s'", ssNString("it's"))
	require.Equal(t, "N''", ssNString(""))
	require.Equal(t, "a b  c", ssCommentSafe("a\nb\r\nc"))
	require.Equal(t, "evil x", ssCommentSafe("evil\nx"))
	require.True(t, ssNumeric("1"))
	require.True(t, ssNumeric("-10"))
	require.True(t, ssNumeric("12.0"))
	require.False(t, ssNumeric(""))
	require.False(t, ssNumeric("1); DROP TABLE x --"))
}

func TestSSFormatType(t *testing.T) {
	cases := []struct {
		in   ssType
		want string
	}{
		{ssType{Name: "int"}, "int"},
		{ssType{Name: "varchar", MaxLength: 50}, "varchar(50)"},
		{ssType{Name: "varchar", MaxLength: -1}, "varchar(max)"},
		{ssType{Name: "nvarchar", MaxLength: 100}, "nvarchar(50)"},
		{ssType{Name: "nvarchar", MaxLength: -1}, "nvarchar(max)"},
		{ssType{Name: "nchar", MaxLength: 30}, "nchar(15)"},
		{ssType{Name: "varbinary", MaxLength: -1}, "varbinary(max)"},
		{ssType{Name: "binary", MaxLength: 16}, "binary(16)"},
		{ssType{Name: "decimal", Precision: 19, Scale: 4}, "decimal(19,4)"},
		{ssType{Name: "numeric", Precision: 38, Scale: 0}, "numeric(38,0)"},
		{ssType{Name: "datetime2", Scale: 7}, "datetime2(7)"},
		{ssType{Name: "time", Scale: 0}, "time(0)"},
		{ssType{Name: "datetimeoffset", Scale: 3}, "datetimeoffset(3)"},
		{ssType{Name: "float", Precision: 53}, "float"},
		{ssType{Name: "float", Precision: 24}, "float(24)"},
		{ssType{Name: "timestamp"}, "rowversion"},
		{ssType{Name: "money"}, "money"},
		{ssType{Name: "uniqueidentifier"}, "uniqueidentifier"},
		{ssType{Name: "hierarchyid"}, "hierarchyid"},
		{ssType{Name: "Name", Schema: "dbo", UserDefined: true, MaxLength: 100}, "[dbo].[Name]"},
		{ssType{Name: "some future type"}, "[some future type]"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, ssFormatType(c.in), c.want)
	}
}

func TestSSColumnDef(t *testing.T) {
	cases := []struct {
		name string
		in   ssColumn
		want string
	}{
		{
			name: "identity",
			in:   ssColumn{Name: "ProductID", Type: ssType{Name: "int"}, Identity: true, Seed: "1", Increment: "1"},
			want: "[ProductID] int IDENTITY(1,1) NOT NULL",
		},
		{
			name: "identity with a negative step",
			in:   ssColumn{Name: "id", Type: ssType{Name: "bigint"}, Identity: true, Seed: "-1", Increment: "-1"},
			want: "[id] bigint IDENTITY(-1,-1) NOT NULL",
		},
		{
			name: "identity seed that is not a number is dropped",
			in:   ssColumn{Name: "id", Type: ssType{Name: "int"}, Identity: true, Seed: "1)x", Increment: "1"},
			want: "[id] int IDENTITY NOT NULL",
		},
		{
			name: "named default",
			in: ssColumn{Name: "ModifiedDate", Type: ssType{Name: "datetime"},
				DefaultName: "DF_Product_ModifiedDate", DefaultDef: "(getdate())"},
			want: "[ModifiedDate] datetime NOT NULL CONSTRAINT [DF_Product_ModifiedDate] DEFAULT ((getdate()))",
		},
		{
			name: "rowguidcol with default",
			in: ssColumn{Name: "rowguid", Type: ssType{Name: "uniqueidentifier"}, RowGUID: true,
				DefaultName: "DF_rowguid", DefaultDef: "(newid())"},
			want: "[rowguid] uniqueidentifier ROWGUIDCOL NOT NULL CONSTRAINT [DF_rowguid] DEFAULT ((newid()))",
		},
		{
			name: "alias type, nullable, sparse",
			in: ssColumn{Name: "MiddleName", Type: ssType{Name: "Name", Schema: "dbo", UserDefined: true},
				Nullable: true, Sparse: true},
			want: "[MiddleName] [dbo].[Name] SPARSE NULL",
		},
		{
			name: "persisted computed",
			in: ssColumn{Name: "SalesOrderNumber", Computed: true, Persisted: true,
				ComputedDef: "(isnull(N'SO'+CONVERT([nvarchar](23),[SalesOrderID]),N'*** ERROR ***'))"},
			want: "[SalesOrderNumber] AS ((isnull(N'SO'+CONVERT([nvarchar](23),[SalesOrderID]),N'*** ERROR ***'))) PERSISTED NOT NULL",
		},
		{
			name: "virtual computed never carries NOT NULL",
			in:   ssColumn{Name: "Total", Computed: true, ComputedDef: "([a]+[b])"},
			want: "[Total] AS (([a]+[b]))",
		},
		{
			name: "period start column",
			in:   ssColumn{Name: "ValidFrom", Type: ssType{Name: "datetime2", Scale: 7}, GeneratedAlways: 1, Hidden: true},
			want: "[ValidFrom] datetime2(7) GENERATED ALWAYS AS ROW START HIDDEN NOT NULL",
		},
		{
			name: "period end column",
			in:   ssColumn{Name: "ValidTo", Type: ssType{Name: "datetime2", Scale: 7}, GeneratedAlways: 2},
			want: "[ValidTo] datetime2(7) GENERATED ALWAYS AS ROW END NOT NULL",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, ssColumnDef(c.in))
		})
	}
}

func TestSSTableDDL(t *testing.T) {
	tbl := &ssTable{
		Schema: "Sales", Name: "Order Details",
		Columns: []ssColumn{
			{ID: 1, Name: "OrderID", Type: ssType{Name: "int"}},
			{ID: 2, Name: "ProductID", Type: ssType{Name: "int"}},
			{ID: 3, Name: "Qty", Type: ssType{Name: "smallint"},
				DefaultName: "DF_Qty", DefaultDef: "((1))"},
			{ID: 4, Name: "Note", Type: ssType{Name: "nvarchar", MaxLength: -1}, Nullable: true},
		},
		Keys: []ssKey{
			{Name: "PK_Order_Details", Primary: true, Clustered: true,
				Cols: []ssKeyCol{{Name: "OrderID"}, {Name: "ProductID", Desc: true}}},
			{Name: "UQ_Note", Cols: []ssKeyCol{{Name: "Note"}}},
		},
		FKs: []ssFK{
			{Name: "FK_Orders", Cols: []string{"OrderID"}, RefSchema: "dbo", RefTable: "Orders",
				RefCols: []string{"OrderID"}, OnDelete: 1, OnUpdate: 0},
			{Name: "FK_Products", Cols: []string{"ProductID"}, RefSchema: "Production", RefTable: "Product",
				RefCols: []string{"ProductID"}, OnDelete: 2, OnUpdate: 3},
		},
		Checks: []ssCheck{{Name: "CK_Qty", Def: "([Qty]>(0))"}},
		Indexes: []ssIndex{
			{Name: "IX_Product", Type: 2, Keys: []ssKeyCol{{Name: "ProductID"}},
				Include: []string{"Qty", "Note"}, Filter: "([Qty]>(10))"},
			{Name: "AK_Pair", Type: 2, Unique: true, Keys: []ssKeyCol{{Name: "Qty", Desc: true}, {Name: "OrderID"}}},
			{Name: "PXML_Note", Type: 3, TypeDesc: "XML"},
			{Name: "CCI", Type: 5, TypeDesc: "CLUSTERED COLUMNSTORE"},
		},
		Description: "Line items, one per product of an order.",
		ColumnDescriptions: map[int]string{
			3: "Ordered quantity; can't be 0.",
		},
		Fulltext: true,
	}
	want := "CREATE TABLE [Sales].[Order Details] (\n" +
		"    [OrderID] int NOT NULL,\n" +
		"    [ProductID] int NOT NULL,\n" +
		"    [Qty] smallint NOT NULL CONSTRAINT [DF_Qty] DEFAULT (((1))),\n" +
		"    [Note] nvarchar(max) NULL,\n" +
		"    CONSTRAINT [PK_Order_Details] PRIMARY KEY CLUSTERED ([OrderID] ASC, [ProductID] DESC),\n" +
		"    CONSTRAINT [UQ_Note] UNIQUE ([Note] ASC),\n" +
		"    CONSTRAINT [FK_Orders] FOREIGN KEY ([OrderID]) REFERENCES [dbo].[Orders] ([OrderID]) ON DELETE CASCADE,\n" +
		"    CONSTRAINT [FK_Products] FOREIGN KEY ([ProductID]) REFERENCES [Production].[Product] ([ProductID]) ON DELETE SET NULL ON UPDATE SET DEFAULT,\n" +
		"    CONSTRAINT [CK_Qty] CHECK (([Qty]>(0)))\n" +
		");\n" +
		"CREATE NONCLUSTERED INDEX [IX_Product] ON [Sales].[Order Details] ([ProductID] ASC) INCLUDE ([Qty], [Note]) WHERE (([Qty]>(10)));\n" +
		"CREATE UNIQUE NONCLUSTERED INDEX [AK_Pair] ON [Sales].[Order Details] ([Qty] DESC, [OrderID] ASC);\n" +
		"-- skipped index [PXML_Note] on [Sales].[Order Details] (XML)\n" +
		"-- skipped index [CCI] on [Sales].[Order Details] (CLUSTERED COLUMNSTORE)\n" +
		"-- skipped fulltext index on [Sales].[Order Details] (FULLTEXT)\n" +
		"EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'Line items, one per product of an order.'," +
		" @level0type=N'SCHEMA', @level0name=N'Sales', @level1type=N'TABLE', @level1name=N'Order Details';\n" +
		"EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'Ordered quantity; can''t be 0.'," +
		" @level0type=N'SCHEMA', @level0name=N'Sales', @level1type=N'TABLE', @level1name=N'Order Details'," +
		" @level2type=N'COLUMN', @level2name=N'Qty';\n"
	require.Equal(t, want, ssTableDDL(tbl))
}

func TestSSTableDDLSystemVersioned(t *testing.T) {
	tbl := &ssTable{
		Schema: "dbo", Name: "Employee", TemporalType: 2,
		HistorySchema: "hist", HistoryName: "Employee]History",
		Columns: []ssColumn{
			{ID: 1, Name: "id", Type: ssType{Name: "int"}},
			{ID: 2, Name: "ValidFrom", Type: ssType{Name: "datetime2", Scale: 7}, GeneratedAlways: 1},
			{ID: 3, Name: "ValidTo", Type: ssType{Name: "datetime2", Scale: 7}, GeneratedAlways: 2},
		},
		Keys:        []ssKey{{Name: "PK_Employee", Primary: true, Cols: []ssKeyCol{{Name: "id"}}}},
		PeriodStart: "ValidFrom", PeriodEnd: "ValidTo",
	}
	want := "CREATE TABLE [dbo].[Employee] (\n" +
		"    [id] int NOT NULL,\n" +
		"    [ValidFrom] datetime2(7) GENERATED ALWAYS AS ROW START NOT NULL,\n" +
		"    [ValidTo] datetime2(7) GENERATED ALWAYS AS ROW END NOT NULL,\n" +
		"    CONSTRAINT [PK_Employee] PRIMARY KEY NONCLUSTERED ([id] ASC),\n" +
		"    PERIOD FOR SYSTEM_TIME ([ValidFrom], [ValidTo])\n" +
		") WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = [hist].[Employee]]History]));\n"
	require.Equal(t, want, ssTableDDL(tbl))
}

// An object name carrying a line break must not escape the comment line.
func TestSSSkippedIndexCommentIsOneLine(t *testing.T) {
	got := ssIndexDDL("[dbo].[t]", ssIndex{Name: "x\nDROP TABLE y", Type: 4, TypeDesc: "SPATIAL"})
	require.Equal(t, "-- skipped index [x DROP TABLE y] on [dbo].[t] (SPATIAL)\n", got)
}

func TestSSTypeAndSequenceDDL(t *testing.T) {
	require.Equal(t, "CREATE TYPE [dbo].[Name] FROM nvarchar(50) NULL;\n",
		ssAliasTypeDDL(ssAliasType{Schema: "dbo", Name: "Name", Base: ssType{Name: "nvarchar", MaxLength: 100}, Nullable: true}))
	require.Equal(t, "CREATE TYPE [dbo].[Flag] FROM bit NOT NULL;\n",
		ssAliasTypeDDL(ssAliasType{Schema: "dbo", Name: "Flag", Base: ssType{Name: "bit"}}))

	tt := &ssTable{
		Schema: "dbo", Name: "IdList",
		Columns: []ssColumn{{ID: 1, Name: "id", Type: ssType{Name: "int"}}},
		Keys:    []ssKey{{Name: "PK__IdList", Primary: true, Clustered: true, Cols: []ssKeyCol{{Name: "id"}}}},
	}
	require.Equal(t, "CREATE TYPE [dbo].[IdList] AS TABLE (\n"+
		"    [id] int NOT NULL,\n"+
		"    CONSTRAINT [PK__IdList] PRIMARY KEY CLUSTERED ([id] ASC)\n"+
		");\n", ssTableTypeDDL(tt))

	cases := []struct {
		name string
		in   ssSequence
		want string
	}{
		{
			name: "used sequence restarts after current",
			in: ssSequence{Schema: "dbo", Name: "OrderNo", Type: ssType{Name: "bigint"},
				Start: "1", Increment: "1", Min: "1", Max: "9223372036854775807", Current: "41"},
			want: "CREATE SEQUENCE [dbo].[OrderNo] AS bigint START WITH 42 INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 NO CYCLE;\n",
		},
		{
			name: "decimal(38) beyond int64",
			in: ssSequence{Schema: "s", Name: "big", Type: ssType{Name: "decimal", Precision: 38, Scale: 0},
				Start: "1", Increment: "10", Min: "1", Max: "99999999999999999999999999999999999999",
				Current: "10000000000000000000000"},
			want: "CREATE SEQUENCE [s].[big] AS decimal(38,0) START WITH 10000000000000000000010 INCREMENT BY 10 MINVALUE 1 MAXVALUE 99999999999999999999999999999999999999 NO CYCLE;\n",
		},
		{
			name: "descending cycling sequence wraps to its maximum",
			in: ssSequence{Schema: "dbo", Name: "down", Type: ssType{Name: "int"},
				Start: "10", Increment: "-5", Min: "0", Max: "10", Current: "0", Cycle: true},
			want: "CREATE SEQUENCE [dbo].[down] AS int START WITH 10 INCREMENT BY -5 MINVALUE 0 MAXVALUE 10 CYCLE;\n",
		},
		{
			name: "exhausted sequence keeps its bound and says so",
			in: ssSequence{Schema: "dbo", Name: "done", Type: ssType{Name: "tinyint"},
				Start: "0", Increment: "1", Min: "0", Max: "255", Current: "255"},
			want: "-- [dbo].[done]: exhausted on the source (current value 255 is its bound)\n" +
				"CREATE SEQUENCE [dbo].[done] AS tinyint START WITH 255 INCREMENT BY 1 MINVALUE 0 MAXVALUE 255 NO CYCLE;\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, ssSequenceDDL(c.in))
		})
	}
}

func TestSSModuleDDL(t *testing.T) {
	require.Equal(t, "CREATE VIEW v AS SELECT 1\n", ssModuleDDL("CREATE VIEW v AS SELECT 1"))
	require.Equal(t, "CREATE VIEW v AS SELECT 1\n", ssModuleDDL("CREATE VIEW v AS SELECT 1\n"))
	require.Equal(t, "", ssModuleDDL(""))
}

// Definitions the login cannot read (NULL without VIEW DEFINITION) fail
// the inspection with the objects and the grant to issue, instead of
// dropping defaults or passing modules off as encrypted.
func TestSSHiddenDefinitions(t *testing.T) {
	h := &ssHidden{}
	require.NoError(t, h.err("AdventureWorks", nil))
	byID := map[int64]*ssTable{7: {Schema: "Sales", Name: "Orders"}}
	h.add(7, "default DF_Orders_Status")
	h.add(0, "view dbo.vOrders")
	err := h.err("AdventureWorks", byID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "default DF_Orders_Status of Sales.Orders")
	require.Contains(t, err.Error(), "view dbo.vOrders")
	require.Contains(t, err.Error(), "GRANT VIEW DEFINITION ON DATABASE::[AdventureWorks]")
	for i := 0; i < 12; i++ {
		h.add(0, "procedure dbo.p")
	}
	require.Contains(t, h.err("AdventureWorks", byID).Error(), "and 4 more")
}
