package dataxfer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The history variant reads the closed versions of a MariaDB
// system-versioned table; catalog lookups stay the base table's.
func TestMySQLSystemTimeHistorySourceQueries(t *testing.T) {
	d := MySQLSystemTimeHistorySource("valid_to")
	require.Equal(t, "mysql", d.Kind())
	require.Equal(t,
		"SELECT COUNT(*) FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6)",
		d.CountQuery("db", "employees"))
	require.Equal(t,
		"SELECT MIN(`id`), MAX(`id`) FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6)",
		d.MinMaxQuery("db", "employees", "id"))
	require.Equal(t,
		"SELECT `id`,`valid_to` FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6) AND `id` BETWEEN ? AND ? ORDER BY `id`",
		d.SelectRangeQuery("db", "employees", []string{"id", "valid_to"}, "id"))
	require.Equal(t,
		"SELECT `id`,`valid_to` FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6) ORDER BY `id`,`valid_to` LIMIT ? OFFSET ?",
		d.SelectOffsetQuery("db", "employees", []string{"id", "valid_to"}))
	require.Equal(t, MySQLSource().PKColumnsQuery(), d.PKColumnsQuery())
	require.Equal(t, MySQLSource().ListColumnsQuery(), d.ListColumnsQuery())
}

// ROW END is left out of the partitioning key. The default column list
// skips every STORED GENERATED column, ROW START / ROW END included:
// reviewer regression — keeping them there made the copier SELECT and
// COPY `rs`/`re` into tables the translator had dropped them from (the
// non-emulated transaction-precise and unknown-sentinel forms), failing
// with `column "rs" does not exist`. Emulated tables pass an explicit
// column list instead (planner / worker copy payload).
func TestMySQLCatalogQueriesSystemVersioning(t *testing.T) {
	d := MySQLSource()
	require.Contains(t, d.ListColumnsQuery(), "EXTRA NOT LIKE '%STORED GENERATED%'")
	require.NotContains(t, d.ListColumnsQuery(), "ROW START")
	require.NotContains(t, d.ListColumnsQuery(), "ROW END")
	require.Contains(t, d.PKColumnsQuery(), "<> 'ROW END'")
}

// SQL Server quoting doubles the closing bracket (QUOTENAME rule) and binds
// go-mssqldb's @pN placeholders; OFFSET paging needs an ORDER BY, by the
// primary key when the copier knows it.
func TestSQLServerSourceQueries(t *testing.T) {
	d := SQLServerSource()
	require.Equal(t, "sqlserver", d.Kind())
	require.Equal(t, "[Order Details]", d.Quote("Order Details"))
	require.Equal(t, "[a]]b]", d.Quote("a]b"))
	require.Equal(t, "[dbo].[Order Details]", d.Qualify("dbo", "Order Details"))
	require.Equal(t, "[t]", d.Qualify("", "t"))
	require.Equal(t, "@p1", d.PlaceholderAt(1))
	require.Equal(t, "@p12", d.PlaceholderAt(12))
	require.Equal(t,
		"SELECT COUNT_BIG(*) FROM [Sales].[SalesOrderHeader]",
		d.CountQuery("Sales", "SalesOrderHeader"))
	require.Equal(t,
		"SELECT MIN([SalesOrderID]), MAX([SalesOrderID]) FROM [Sales].[SalesOrderHeader]",
		d.MinMaxQuery("Sales", "SalesOrderHeader", "SalesOrderID"))
	require.Equal(t,
		"SELECT [OrderID],[ProductID] FROM [dbo].[Order Details] WHERE [OrderID] BETWEEN @p1 AND @p2 ORDER BY [OrderID]",
		d.SelectRangeQuery("dbo", "Order Details", []string{"OrderID", "ProductID"}, "OrderID"))
	require.Equal(t,
		"SELECT [a],[b] FROM [dbo].[heap] ORDER BY (SELECT NULL) OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY",
		d.SelectOffsetQuery("dbo", "heap", []string{"a", "b"}))
	kd, ok := d.(keyedOffsetDialect)
	require.True(t, ok, "SQL Server must page OFFSET batches in key order")
	require.Equal(t,
		"SELECT [OrderID],[ProductID],[Qty] FROM [dbo].[Order Details] ORDER BY [dbo].[Order Details].[OrderID],[dbo].[Order Details].[ProductID] OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY",
		kd.SelectOffsetOrderedQuery("dbo", "Order Details",
			[]string{"OrderID", "ProductID", "Qty"}, []string{"OrderID", "ProductID"}))
	// The other dialects keep their unordered OFFSET form.
	_, ok = MySQLSource().(keyedOffsetDialect)
	require.False(t, ok)
	_, ok = OracleSource().(keyedOffsetDialect)
	require.False(t, ok)
}

// The default column list skips what the copier must not write (computed
// columns, rowversion) and the spatial types; hierarchyid and sql_variant
// stay, converted server-side.
func TestSQLServerCatalogQueries(t *testing.T) {
	d := SQLServerSource()
	q := d.ListColumnsQuery()
	require.Contains(t, q, "TABLE_SCHEMA = @p1 AND c.TABLE_NAME = @p2")
	require.Contains(t, q, "sys.computed_columns")
	require.Contains(t, q, "NOT IN ('timestamp','rowversion','geography','geometry')")
	for _, ty := range []string{"'hierarchyid'", "'sql_variant'"} {
		require.NotContains(t, q, ty, "converted server-side, not dropped")
	}
	require.Contains(t, q, "ORDER BY c.ORDINAL_POSITION")
	require.Contains(t, d.ColumnDataTypeQuery(), "COLUMN_NAME = @p3")
	require.Contains(t, d.PKColumnsQuery(), "CONSTRAINT_TYPE = 'PRIMARY KEY'")
	require.Contains(t, d.PKColumnsQuery(), "ORDER BY k.ORDINAL_POSITION")
}

// Only keys the integer range split cannot skip a value of are numeric for
// the partitioner.
func TestSQLServerIsNumericDataType(t *testing.T) {
	d := SQLServerSource()
	for _, dt := range []string{"tinyint", "smallint", "int", "INT", "bigint", "decimal", "numeric"} {
		require.True(t, d.IsNumericDataType(dt), dt)
	}
	for _, dt := range []string{"decimal_scaled", "money", "smallmoney", "float", "real", "uniqueidentifier", "nvarchar"} {
		require.False(t, d.IsNumericDataType(dt), dt)
	}
}

// The PG names of a SQL Server copy target are the whole identifier
// lowercased; other kinds keep their own rules.
func TestNormalizeDstIdentSQLServer(t *testing.T) {
	cases := []struct{ kind, in, want string }{
		{"sqlserver", "SalesOrderHeader", "salesorderheader"},
		{"sqlserver", "Order Details", "order details"},
		{"sqlserver", "ID", "id"},
		{"sqlserver", "Ünïcode_Ça", "ünïcode_ça"},
		{"sqlserver", "", ""},
		{"oracle", "CUSTOMERS", "customers"},
		{"oracle", "t_Case", "t_Case"},
		{"mysql", "Customers", "Customers"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, normalizeDstIdent(c.kind, c.in), c.kind+"/"+c.in)
	}
}

// go-mssqldb returns uniqueidentifier as 16 bytes in SQL Server's
// mixed-endian order. The guid 6F9619FF-8B86-D011-B42D-00C04FC964FF is
// FF19966F 868B 11D0 B42D 00C04FC964FF on the wire.
func TestCoerceFromSQLServer(t *testing.T) {
	wire := []byte{0xFF, 0x19, 0x96, 0x6F, 0x86, 0x8B, 0x11, 0xD0,
		0xB4, 0x2D, 0x00, 0xC0, 0x4F, 0xC9, 0x64, 0xFF}
	require.Equal(t, "6f9619ff-8b86-d011-b42d-00c04fc964ff", coerceFrom("sqlserver", wire, "uuid"))
	// The byte swap is SQL Server specific: a MySQL BINARY(16) is not
	// reinterpreted.
	require.NotEqual(t, "6f9619ff-8b86-d011-b42d-00c04fc964ff", coerceFrom("mysql", wire, "uuid"))
	// A uuid already in text form, or bytes bound for bytea, pass through.
	require.Equal(t, "6F9619FF-8B86-D011-B42D-00C04FC964FF",
		coerceFrom("sqlserver", "6F9619FF-8B86-D011-B42D-00C04FC964FF", "uuid"))
	require.Equal(t, wire, coerceFrom("sqlserver", wire, "bytea"))
	require.Nil(t, coerceFrom("sqlserver", nil, "uuid"))

	// money / decimal arrive as decimal text bytes.
	require.Equal(t, "12.3400", coerceFrom("sqlserver", []byte("12.3400"), "numeric"))
	// bit arrives as bool.
	require.Equal(t, true, coerceFrom("sqlserver", true, "bool"))
	// tinyint arrives as int64.
	require.Equal(t, int64(255), coerceFrom("sqlserver", int64(255), "int2"))
	// datetimeoffset keeps its instant.
	ts := time.Date(2026, 11, 4, 10, 30, 0, 0, time.FixedZone("", 2*3600))
	require.Equal(t, ts, coerceFrom("sqlserver", ts, "timestamptz"))
}

// CLR types and sql_variant are converted to text in the batch SELECT;
// every other column is selected as is, and the conversion keeps the
// column name so the COPY column list still matches.
func TestSQLServerSelectExprs(t *testing.T) {
	d := sqlserverDialect{}.withTypes(
		[]string{"DocumentNode", "Title", "v", "Shape", "Loc]x"},
		map[string]string{"DocumentNode": "hierarchyid", "Title": "nvarchar", "v": "sql_variant",
			"Shape": "geometry", "Loc]x": "geography"})
	require.Equal(t,
		"SELECT CAST([DocumentNode] AS nvarchar(4000)) AS [DocumentNode],[Title],CAST([v] AS nvarchar(4000)) AS [v],"+
			"[Shape].STAsText() AS [Shape],[Loc]]x].STAsText() AS [Loc]]x] FROM [Production].[Document]"+
			" ORDER BY [Production].[Document].[DocumentNode] OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY",
		d.SelectOffsetOrderedQuery("Production", "Document",
			[]string{"DocumentNode", "Title", "v", "Shape", "Loc]x"}, []string{"DocumentNode"}))
	require.Equal(t, "sqlserver", d.Kind())

	plain := sqlserverDialect{}.withTypes([]string{"id", "name"}, map[string]string{"id": "int", "name": "nvarchar"})
	require.Nil(t, plain.exprs)
	require.Equal(t,
		"SELECT [id],[name] FROM [dbo].[t] WHERE [id] BETWEEN @p1 AND @p2 ORDER BY [id]",
		plain.SelectRangeQuery("dbo", "t", []string{"id", "name"}, "id"))

	_, ok := SourceDialect(d).(selectExprDialect)
	require.True(t, ok)
	_, ok = MySQLSource().(selectExprDialect)
	require.False(t, ok)
}
