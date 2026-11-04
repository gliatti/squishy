package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsqlTestRun is a HookRun over a fake translation: schemas map to
// aw_<schema>, and the script defines dbo.Customers (name varchar,
// active bit → boolean, id identity) and Sales.Orders.
func tsqlTestRun() *HookRun {
	tr := &translator{
		res: &Result{Plan: SchemaPlan{Tables: []PGTable{
			{Schema: "aw_dbo", Name: "customers", Columns: []PGColumn{
				{Name: "id", Type: "integer", Identity: true},
				{Name: "name", Type: "varchar(50)"},
				{Name: "active", Type: "boolean"},
				{Name: "created", Type: "timestamp(3)"},
			}},
			{Schema: "aw_sales", Name: "orders", Columns: []PGColumn{
				{Name: "id", Type: "integer"},
				{Name: "customerid", Type: "integer"},
				{Name: "total", Type: "numeric(19,4)"},
				{Name: "code", Type: "char(4)"},
			}},
		}}},
		sourceObjects: map[string]bool{"dbo.customers": true, "sales.orders": true, "dbo.seq": true},
	}
	return &HookRun{
		Kind:   dialects.KindSQLServer,
		Schema: func(s string) string { return "aw_" + normalizeTSQLIdent(s) },
		t:      tr,
	}
}

func tsqlTestCtx(cols map[string]string) *ExprCtx {
	return &ExprCtx{Run: tsqlTestRun(), Object: "test.obj", DefaultSchema: "dbo", Columns: cols}
}

func tsqlExprOut(t *testing.T, src string, ctx *ExprCtx) (string, tsqlRewriteResult) {
	t.Helper()
	e, errs := tsqldialect.ParseExpr(src)
	require.Empty(t, errs, "ParseExpr(%q)", src)
	out, res := rewriteTSQLExpr(e, ctx)
	return pgast.WriteExpr(out), res
}

func tsqlSelectOut(t *testing.T, src string, ctx *ExprCtx) (string, tsqlRewriteResult) {
	t.Helper()
	s, errs := tsqldialect.ParseSelect(src)
	require.Empty(t, errs, "ParseSelect(%q)", src)
	out, res := rewriteTSQLSelect(s, ctx)
	return pgast.WriteSelectStmt(out), res
}

func hasLabel(res tsqlRewriteResult, sub string) bool {
	for _, l := range res.Labels {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func hasNote(res tsqlRewriteResult, sub string) bool {
	for _, n := range res.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestTSQLVisitors_Expressions(t *testing.T) {
	typed := map[string]string{"name": "varchar(50)", "flag": "boolean", "d": "timestamp(3)", "qty": "integer", "code": "char(4)",
		"m": "numeric(19,4)", "f": "double precision", "hx": "varchar(20)", "bin": "bytea"}
	cases := []struct {
		name, src, want string
		cols            map[string]string
		label, note     string
	}{
		{name: "isnull", src: "ISNULL(a, 0)", want: `coalesce("a", 0)`},
		{name: "isnull bit", src: "ISNULL(flag, 1)", cols: typed, want: `coalesce("flag", true)`},
		{name: "getdate", src: "GETDATE()", want: `now()`},
		{name: "current_timestamp", src: "CURRENT_TIMESTAMP", want: `now()`},
		{name: "sysdatetime", src: "SYSDATETIME()", want: `now()`},
		{name: "getutcdate", src: "GETUTCDATE()", want: `timezone('UTC', now())`},
		{name: "newid", src: "NEWID()", want: `gen_random_uuid()`},
		{name: "newsequentialid", src: "NEWSEQUENTIALID()", want: `gen_random_uuid()`, note: "not increasing"},
		{name: "len untyped", src: "LEN(n)", want: `length(rtrim(CAST("n" AS text)))`},
		{name: "len typed", src: "LEN([Name])", cols: typed, want: `length(rtrim("name"))`},
		{name: "datalength", src: "DATALENGTH(name)", cols: typed, want: `octet_length("name")`, note: "UTF-8"},
		{name: "charindex", src: "CHARINDEX('x', name)", cols: typed, want: `strpos("name", 'x')`},
		{name: "charindex start", src: "CHARINDEX('x', name, 3)", cols: typed,
			want: `CASE WHEN strpos(substr("name", 3), 'x') = 0 THEN 0 ELSE strpos(substr("name", 3), 'x') + 3 - 1 END`},
		{name: "charindex start 1", src: "CHARINDEX('x', name, 1)", cols: typed, want: `strpos("name", 'x')`},
		{name: "charindex start expr", src: "CHARINDEX('x', name, @p)", cols: typed,
			want: `CASE WHEN strpos(substr("name", greatest("p", 1)), 'x') = 0 THEN 0 ELSE strpos(substr("name", greatest("p", 1)), 'x') + greatest("p", 1) - 1 END`},
		{name: "stuff", src: "STUFF(name, 2, 3, 'abc')", cols: typed, want: `overlay("name", 'abc', 2, 3)`},
		{name: "stuff null insert", src: "STUFF(name, 2, 3, @r)", cols: typed, want: `overlay("name", coalesce(CAST("r" AS text), ''), 2, 3)`},
		{name: "replicate", src: "REPLICATE('0', 5)", want: `repeat('0', 5)`},
		{name: "space", src: "SPACE(3)", want: `repeat(' ', 3)`},
		{name: "left on int", src: "LEFT(qty, 2)", cols: typed, want: `left(CAST("qty" AS text), 2)`},
		{name: "upper", src: "UPPER(name)", cols: typed, want: `upper("name")`},
		{name: "iif", src: "IIF(qty > 1, 'x', 'y')", want: `CASE WHEN "qty" > 1 THEN 'x' ELSE 'y' END`},
		{name: "choose", src: "CHOOSE(2, 'a', 'b')", want: `CASE 2 WHEN 1 THEN 'a' WHEN 2 THEN 'b' END`},
		{name: "format", src: "FORMAT(d, 'yyyy-MM-dd')", want: `to_char("d", 'yyyy-MM-dd')`, label: "FORMAT"},
		{name: "str", src: "STR(x, 6, 2)", want: `lpad(CAST(round(CAST("x" AS numeric), 2) AS text), 6)`},
		{name: "string_agg", src: "STRING_AGG(name, ',') WITHIN GROUP (ORDER BY name)", cols: typed, want: `string_agg("name", ',' ORDER BY "name")`},
		{name: "convert int", src: "CONVERT(int, x)", want: `CAST("x" AS integer)`},
		{name: "cast nvarchar default length", src: "CAST(x AS nvarchar)", want: `CAST("x" AS varchar(30))`},
		{name: "cast nvarchar max", src: "CAST(x AS nvarchar(max))", want: `CAST("x" AS text)`},
		{name: "cast bit", src: "CAST(1 AS bit)", want: `CAST(true AS boolean)`},
		{name: "convert style 112", src: "CONVERT(varchar(8), d, 112)", cols: typed, want: `CAST(tsql.convert_style("d", 112) AS varchar(8))`, note: "convert_style"},
		{name: "convert style 103 to date", src: "CONVERT(datetime, '01/02/2003', 103)", want: `CAST(tsql.str_to_timestamp_dmy('01/02/2003') AS timestamp(3))`},
		{name: "convert style 101 to date", src: "CONVERT(datetime, REPLACE(s, 'Z', ''), 101)", want: `CAST(tsql.str_to_timestamp_mdy(replace(CAST("s" AS text), 'Z', '')) AS timestamp(3))`},
		{name: "convert style on date input", src: "CONVERT(date, d, 112)", cols: typed, want: `CAST("d" AS date)`},
		{name: "convert unknown style", src: "CONVERT(varchar(30), d, 9)", cols: typed, want: `CAST("d" AS varchar(30))`, label: "style 9"},
		{name: "try_convert", src: "TRY_CONVERT(int, s)", want: `CAST("s" AS integer)`, note: "TRY_CAST"},
		{name: "concat proven literal", src: "'a' + b", want: `'a' || "b"`},
		{name: "concat proven N literal chain", src: "FirstName + N' ' + LastName", want: `"firstname" || ' ' || "lastname"`},
		{name: "concat proven column", src: "name + code", cols: typed, want: `"name" || "code"`},
		{name: "concat proven function", src: "UPPER(a) + b", want: `upper(CAST("a" AS text)) || "b"`},
		{name: "concat proven cast", src: "CAST(qty AS varchar(10)) + b", cols: typed, want: `CAST("qty" AS varchar(10)) || "b"`},
		{name: "addition proven numeric", src: "qty + 1", cols: typed, want: `"qty" + 1`},
		{name: "plus unproven", src: "a + b", want: `"a" + "b"`, label: "could not be proven text"},
		{name: "datetime plus days", src: "GETDATE() + 1", want: `now() + 1 * INTERVAL '1 day'`},
		{name: "xor", src: "a ^ b", want: `"a" # "b"`},
		{name: "bit not", src: "~a", want: `-"a" - 1`, label: "bitwise NOT"},
		{name: "hex in bitwise", src: "flags & 0x0F", want: `"flags" & 15`},
		{name: "hex binary", src: "0xFF00", want: `CAST('\xFF00' AS bytea)`},
		{name: "bit compare", src: "flag = 1", cols: typed, want: `"flag" = true`},
		{name: "bit compare paren", src: "[flag] = (0)", cols: typed, want: `"flag" = false`},
		{name: "bit in", src: "flag IN (0, 1)", cols: typed, want: `"flag" IN (false, true)`},
		{name: "int compare untouched", src: "qty = 1", cols: typed, want: `"qty" = 1`},
		{name: "like plain", src: "name LIKE 'ab%'", cols: typed, want: `"name" LIKE 'ab%'`, note: "case-insensitive"},
		{name: "like class", src: "code LIKE '[A-Z][A-Z]%'", cols: typed, want: `tsql.like("code", '[A-Z][A-Z]%')`},
		{name: "like escape", src: "name NOT LIKE 'a!%' ESCAPE '!'", cols: typed, want: `NOT tsql.like("name", 'a!%', '!')`},
		{name: "like backslash", src: `x LIKE 'a\b'`, want: `tsql.like(CAST("x" AS text), 'a\b')`},
		{name: "like variable pattern", src: "name LIKE @p + '%'", cols: typed, want: `tsql.like("name", "p" || '%')`},
		{name: "identity", src: "@@IDENTITY", want: `lastval()`, note: "@@IDENTITY"},
		{name: "scope_identity", src: "SCOPE_IDENTITY()", want: `lastval()`},
		{name: "rowcount untouched", src: "@@ROWCOUNT", want: `"@@ROWCOUNT"`},
		{name: "variable", src: "@CustomerID", want: `"customerid"`},
		{name: "ident fold", src: "[Order Details].[UnitPrice]", want: `"order details"."unitprice"`},
		{name: "three-part schema mapped", src: "[Sales].[Orders].[Total]", cols: typed, want: `"aw_sales"."orders"."total"`},
		{name: "inserted untouched", src: "inserted.Qty", want: `"inserted"."qty"`},
		{name: "user function", src: "dbo.ufnGetStock(@id)", want: `"aw_dbo"."ufngetstock"("id")`},
		{name: "sequence", src: "NEXT VALUE FOR dbo.seq", want: `nextval('"aw_dbo"."seq"')`},
		{name: "collate dropped", src: "name COLLATE Latin1_General_BIN", cols: typed, want: `"name"`, note: "COLLATE"},
		{name: "object_id", src: "OBJECT_ID('dbo.Customers')", want: `CAST(to_regclass('"aw_dbo"."customers"') AS oid)`},
		{name: "object_id proc", src: "OBJECT_ID(N'[dbo].[uspX]', N'P')", want: `CAST(to_regproc('"aw_dbo"."uspx"') AS oid)`},
		{name: "ident_current", src: "IDENT_CURRENT('dbo.Customers')", want: `pg_sequence_last_value(CAST(pg_get_serial_sequence('"aw_dbo"."customers"', 'id') AS regclass))`},
		{name: "suser_sname", src: "SUSER_SNAME()", want: `pg_catalog.current_user()`},
		{name: "system_user", src: "SYSTEM_USER", want: `pg_catalog.current_user()`},
		{name: "host_name", src: "HOST_NAME()", want: `CAST(inet_client_addr() AS text)`},
		{name: "db_name", src: "DB_NAME()", want: `current_database()`},
		{name: "isnumeric", src: "ISNUMERIC(qty)", cols: typed, want: `tsql.isnumeric(CAST("qty" AS text))`},
		{name: "isdate", src: "ISDATE(name)", cols: typed, want: `tsql.isdate("name")`},
		{name: "dateadd", src: "DATEADD(dd, 1, d)", cols: typed, want: `tsql.dateadd('day', 1, "d")`},
		{name: "datediff literals", src: "DATEDIFF(year, '2024-12-31', '2025-01-01')",
			want: `tsql.datediff('year', CAST('2024-12-31' AS timestamp), CAST('2025-01-01' AS timestamp))`},
		{name: "datediff_big", src: "DATEDIFF_BIG(ms, a, b)", want: `tsql.datediff('millisecond', "a", "b")`},
		{name: "datepart", src: "DATEPART(dw, d)", cols: typed, want: `tsql.datepart('weekday', "d")`},
		{name: "datename", src: "DATENAME(month, d)", cols: typed, want: `tsql.datename('month', "d")`},
		{name: "dateadd unknown part", src: "DATEADD(foo, 1, d)", cols: typed, want: `DATEADD("foo", 1, "d")`, label: "unknown date part"},
		{name: "year", src: "YEAR(d)", cols: typed, want: `CAST(date_part('year', "d") AS integer)`},
		{name: "eomonth", src: "EOMONTH(d)", cols: typed, want: `CAST(date_trunc('month', "d") + INTERVAL '1 month - 1 day' AS date)`},
		{name: "eomonth offset", src: "EOMONTH(d, @n + 1)", cols: typed,
			want: `CAST(date_trunc('month', "d") + ("n" + 1) * INTERVAL '1 month' + INTERVAL '1 month - 1 day' AS date)`},
		{name: "datefromparts", src: "DATEFROMPARTS(2024, 1, 31)", want: `make_date(2024, 1, 31)`},
		{name: "square", src: "SQUARE(x)", want: `power("x", 2)`},
		{name: "round truncate", src: "ROUND(x, 2, 1)", want: `trunc(CAST("x" AS numeric), 2)`, note: "trunc"},
		{name: "round", src: "ROUND(x, 2)", want: `round(CAST("x" AS numeric), 2)`},
		{name: "round numeric column", src: "ROUND(m, 2)", cols: typed, want: `round("m", 2)`},
		{name: "round float column", src: "ROUND(f, 1)", cols: typed, want: `CAST(round(CAST("f" AS numeric), 1) AS double precision)`},
		{name: "round float avg", src: "ROUND(AVG(CAST(s AS float)), 2)", want: `CAST(round(CAST(avg(CAST("s" AS double precision)) AS numeric), 2) AS double precision)`},
		{name: "rand", src: "RAND()", want: `random()`},
		{name: "log", src: "LOG(x)", want: `ln("x")`},
		{name: "log base", src: "LOG(x, 2)", want: `log(2, "x")`},
		{name: "log10", src: "LOG10(x)", want: `log("x")`},
		{name: "ceiling", src: "CEILING(x)", want: `ceil("x")`},
		{name: "checksum residual", src: "CHECKSUM(a, b)", want: `checksum("a", "b")`, label: "CHECKSUM"},
		{name: "hashbytes residual", src: "HASHBYTES('SHA2_256', a)", want: `hashbytes('SHA2_256', "a")`, label: "HASHBYTES"},
		{name: "error residual", src: "@@ERROR", want: `"@@ERROR"`, label: "@@ERROR"},
		{name: "method residual", src: "g.STDistance(h)", want: ``, label: "STDistance"},
		{name: "hierarchyid ancestor", src: "e.[OrganizationNode].GetAncestor(1)", want: `tsql.hierarchyid_getancestor("e"."organizationnode", 1)`},
		{name: "hierarchyid level", src: "OrganizationNode.GetLevel()", want: `tsql.hierarchyid_getlevel("organizationnode")`},
		{name: "hierarchyid descendant", src: "n.IsDescendantOf(@p) = 1", want: `tsql.hierarchyid_isdescendantof("n", "p") = true`},
		{name: "hierarchyid tostring", src: "n.ToString()", want: `CAST("n" AS text)`},
		{name: "hierarchyid root", src: "hierarchyid::GetRoot()", want: `'/'`},
		{name: "hierarchyid parse", src: "hierarchyid::Parse('/1/')", want: `CAST('/1/' AS text)`},
		{name: "xml value", src: `Demographics.value('declare default element namespace "urn:s"; (/StoreSurvey/AnnualSales)[1]', 'money')`,
			want: `CAST((SELECT "v" FROM XMLTABLE(XMLNAMESPACES('urn:s' AS "dns"), '/' PASSING ("demographics") COLUMNS "v" numeric(19,4) PATH '(/dns:StoreSurvey/dns:AnnualSales)[1]') AS "_x") AS numeric(19,4))`},
		{name: "xml exist", src: `x.exist('declare namespace p="urn:p"; /p:a/@b') = 1`,
			want: `CAST((SELECT "v" FROM XMLTABLE(XMLNAMESPACES('urn:p' AS "p"), '/' PASSING ("x") COLUMNS "v" boolean PATH 'boolean(/p:a/@b)') AS "_x") AS boolean) = true`},
		{name: "xml query", src: `x.query('/a/b')`,
			want: `(SELECT xmlagg("v") FROM XMLTABLE('/a/b' PASSING ("x") COLUMNS "v" xml PATH '.') AS "_x")`},
		{name: "xml data", src: `x.value('data(/a)', 'int')`,
			want: `CAST((SELECT "v" FROM XMLTABLE('/' PASSING ("x") COLUMNS "v" integer PATH 'string(/a)') AS "_x") AS integer)`},
		{name: "xml modify residual", src: `x.modify('insert <a/> into (/r)[1]')`, label: "XML DML"},
		{name: "xml flwor residual", src: `x.query('for $i in /a return $i')`, label: "FLWOR"},
		{name: "xml non-xpath function", src: `x.value('fn:upper-case((/a)[1])', 'varchar(9)')`, label: "upper-case"},
		{name: "at time zone windows", src: "d AT TIME ZONE 'Pacific Standard Time'", cols: typed, want: `"d" AT TIME ZONE 'Pacific Standard Time'`, label: "Windows"},
		{name: "precedence after rewrite", src: "a & b + c", want: `("a" & "b") + "c"`, label: "could not be proven text"},
		{name: "precedence typed", src: "qty & 3 + qty", cols: typed, want: `("qty" & 3) + "qty"`},
		{name: "kept trigger function", src: "UPDATE(Qty)", want: `UPDATE("qty")`},
		// ~ on a bit (boolean) is NOT; on an integer -x - 1.
		{name: "bitwise not bit", src: "~flag", cols: typed, want: `(NOT "flag")`},
		{name: "bitwise not int", src: "~qty", cols: typed, want: `-"qty" - 1`},
		{name: "bitwise not unknown", src: "~x", want: `-"x" - 1`, label: "bitwise NOT"},
		// CONVERT to binary: styles 1 and 2 decode hex digits.
		{name: "convert binary style 1", src: "CONVERT(varbinary(10), hx, 1)", cols: typed, want: `decode(substr("hx", 3), 'hex')`},
		{name: "convert binary style 2", src: "CONVERT(varbinary(10), hx, 2)", cols: typed, want: `decode("hx", 'hex')`},
		{name: "convert binary style 0", src: "CONVERT(varbinary(10), hx, 0)", cols: typed, want: `CAST("hx" AS bytea)`},
		{name: "convert binary from binary", src: "CONVERT(varbinary(10), bin, 1)", cols: typed, want: `CAST("bin" AS bytea)`},
		{name: "convert binary other style", src: "CONVERT(varbinary(10), hx, 3)", cols: typed, label: "style 3"},
		{name: "convert binary unknown source", src: "CONVERT(varbinary(10), x, 1)", label: "type is unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := tsqlTestCtx(c.cols)
			got, res := tsqlExprOut(t, c.src, ctx)
			if c.want != "" {
				require.Equal(t, c.want, got)
			}
			if c.label != "" {
				require.True(t, hasLabel(res, c.label), "want label %q, got %q", c.label, res.Labels)
			} else {
				require.Empty(t, res.Labels)
			}
			if c.note != "" {
				require.True(t, hasNote(res, c.note), "want note %q, got %q", c.note, res.Notes)
			}
		})
	}
}

// The context-free pipeline folds schemas to lower case and records
// nothing.
func TestTSQLVisitors_ContextFree(t *testing.T) {
	e, errs := tsqldialect.ParseExpr("ISNULL([Sales].[Orders].[Total], 0) + LEN(@Name)")
	require.Empty(t, errs)
	out, ok := ast.Rewrite(e, RewriteTSQLAST).(ast.Expr)
	require.True(t, ok)
	require.Equal(t, `coalesce("sales"."orders"."total", 0) + length(rtrim(CAST("name" AS text)))`, pgast.WriteExpr(out))
	require.Nil(t, tsqlStatic.seen)
	require.Empty(t, tsqlStatic.labels)

	sel, errs := tsqldialect.ParseSelect("SELECT TOP 3 a FROM [Sales].[T] WITH (NOLOCK)")
	require.Empty(t, errs)
	rs, ok := ast.Rewrite(sel, RewriteTSQLAST).(*ast.SelectStmt)
	require.True(t, ok)
	require.Equal(t, `SELECT "a" FROM "sales"."t" LIMIT 3`, pgast.WriteSelectStmt(rs))
}

func TestTSQLVisitors_Selects(t *testing.T) {
	cases := []struct {
		name, src, want, label string
	}{
		{name: "top", src: "SELECT TOP 5 Name FROM dbo.Customers ORDER BY Name",
			want: `SELECT "name" FROM "aw_dbo"."customers" ORDER BY "name" LIMIT 5`},
		{name: "top paren variable", src: "SELECT TOP (@n) Name FROM Customers",
			want: `SELECT "name" FROM "aw_dbo"."customers" LIMIT "n"`},
		{name: "top with ties", src: "SELECT TOP (10) WITH TIES Name FROM Customers ORDER BY Name",
			want: `SELECT "name" FROM "aw_dbo"."customers" ORDER BY "name" FETCH FIRST (10) ROWS WITH TIES`},
		{name: "top 100 percent", src: "SELECT TOP 100 PERCENT Name FROM Customers ORDER BY Name",
			want: `SELECT "name" FROM "aw_dbo"."customers" ORDER BY "name"`},
		{name: "offset fetch", src: "SELECT Name FROM Customers ORDER BY Name OFFSET 10 ROWS FETCH NEXT 5 ROWS ONLY",
			want: `SELECT "name" FROM "aw_dbo"."customers" ORDER BY "name" OFFSET 10 LIMIT 5`},
		{name: "hints dropped", src: "SELECT c.Name FROM dbo.Customers c WITH (NOLOCK) JOIN Sales.Orders o (NOLOCK) ON o.CustomerID = c.ID",
			want: `SELECT "c"."name" FROM "aw_dbo"."customers" "c" INNER JOIN "aw_sales"."orders" "o" ON "o"."customerid" = "c"."id"`},
		{name: "unknown table unqualified", src: "SELECT x FROM Other",
			want: `SELECT "x" FROM "other"`},
		{name: "cte not qualified", src: "WITH Customers AS (SELECT 1 AS One) SELECT One FROM Customers",
			want: `WITH "customers" AS (SELECT 1 AS "one") SELECT "one" FROM "customers"`},
		{name: "aliases folded", src: "SELECT [Name] AS [Full Name], c.* FROM Customers AS C",
			want: `SELECT "name" AS "full name", "c".* FROM "aw_dbo"."customers" "c"`},
		{name: "scope types prove concat and bit", src: "SELECT c.Name + c.Name AS n FROM Customers c WHERE c.Active = 1",
			want: `SELECT "c"."name" || "c"."name" AS "n" FROM "aw_dbo"."customers" "c" WHERE "c"."active" = true`},
		{name: "option clause dropped", src: "SELECT Name FROM Customers OPTION (MAXRECURSION 10)",
			want: `SELECT "name" FROM "aw_dbo"."customers"`},
		{name: "pivot", src: "SELECT * FROM (SELECT a, b FROM t) s PIVOT (SUM(b) FOR a IN ([x], [y])) p",
			want: `SELECT * FROM (SELECT sum(CASE WHEN "s"."a" = 'x' THEN "s"."b" END) AS "x", sum(CASE WHEN "s"."a" = 'y' THEN "s"."b" END) AS "y" FROM (SELECT "a", "b" FROM "t") "s") "p"`},
		{name: "unpivot residual", src: "SELECT * FROM t UNPIVOT (v FOR k IN ([x], [y])) u", label: "UNPIVOT"},
		{name: "select into residual", src: "SELECT a INTO #tmp FROM t", label: "SELECT … INTO"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, res := tsqlSelectOut(t, c.src, tsqlTestCtx(nil))
			if c.want != "" {
				require.Equal(t, c.want, got)
			}
			if c.label != "" {
				require.True(t, hasLabel(res, c.label), "want label %q, got %q", c.label, res.Labels)
			} else {
				require.Empty(t, res.Labels)
			}
		})
	}
}

// CROSS / OUTER APPLY keep their join kind (the PG writer renders them
// LATERAL); a lateral subquery always carries an alias.
func TestTSQLVisitors_Apply(t *testing.T) {
	s, errs := tsqldialect.ParseSelect(`SELECT c.Name, o.Total FROM dbo.Customers c
		CROSS APPLY (SELECT TOP 1 Total FROM Sales.Orders WHERE CustomerID = c.ID ORDER BY Total DESC) o
		OUTER APPLY dbo.ufnLast(c.ID) l`)
	require.Empty(t, errs)
	out, res := rewriteTSQLSelect(s, tsqlTestCtx(nil))
	require.Empty(t, res.Labels)
	outer, ok := out.From[0].(*ast.FromJoin)
	require.True(t, ok)
	require.Equal(t, ast.OuterApply, outer.Kind)
	ff, ok := outer.Right.(*ast.FromFunc)
	require.True(t, ok)
	require.Equal(t, "l", ff.Alias)
	require.Equal(t, `"aw_dbo"."ufnlast"("c"."id")`, pgast.WriteExpr(ff.Func))
	cross, ok := outer.Left.(*ast.FromJoin)
	require.True(t, ok)
	require.Equal(t, ast.CrossApply, cross.Kind)
	sq, ok := cross.Right.(*ast.FromSubquery)
	require.True(t, ok)
	require.Equal(t, "o", sq.Alias)
	require.Equal(t, `SELECT "total" FROM "aw_sales"."orders" WHERE "customerid" = "c"."id" ORDER BY "total" DESC LIMIT 1`, pgast.WriteSelectStmt(sq.Stmt))
}

// inserted / deleted keep their names inside trigger bodies, and their
// columns are typed from the trigger table.
func TestTSQLVisitors_TriggerTransitionTables(t *testing.T) {
	ctx := tsqlTestCtx(nil)
	ctx.Routine = &RoutineCtx{Run: ctx.Run, Kind: RoutineTrigger, Trigger: &TriggerInfo{Schema: "aw_dbo", Table: "customers"}}
	got, _ := tsqlSelectOut(t, "SELECT i.Name + 'x' FROM inserted i JOIN deleted d ON i.ID = d.ID WHERE i.Active = 0", ctx)
	require.Equal(t, `SELECT "i"."name" || 'x' FROM "inserted" "i" INNER JOIN "deleted" "d" ON "i"."id" = "d"."id" WHERE "i"."active" = false`, got)
}

func TestTSQLVisitors_DML(t *testing.T) {
	ctx := tsqlTestCtx(nil)
	u, errs := tsqldialect.ParseUpdate("UPDATE c SET c.Active = 0, [Name] = i.Name FROM dbo.Customers c JOIN inserted i ON c.ID = i.ID WHERE i.Name IS NOT NULL")
	require.Empty(t, errs)
	out, res := rewriteTSQLBody([]ast.PLStmt{u}, ctx)
	require.Empty(t, res.Labels)
	require.Equal(t,
		`UPDATE "aw_dbo"."customers" "c" SET "active" = false, "name" = "i"."name" FROM "inserted" "i" WHERE "c"."id" = "i"."id" AND "i"."name" IS NOT NULL`,
		pgast.WriteStmt(out[0].(ast.Stmt)))

	u2, errs := tsqldialect.ParseUpdate("UPDATE dbo.Customers SET Active = 1 FROM dbo.Customers JOIN inserted ON Customers.ID = inserted.ID")
	require.Empty(t, errs)
	out, _ = rewriteTSQLBody([]ast.PLStmt{u2}, ctx)
	require.Equal(t,
		`UPDATE "aw_dbo"."customers" SET "active" = true FROM "inserted" WHERE "customers"."id" = "inserted"."id"`,
		pgast.WriteStmt(out[0].(ast.Stmt)))

	u3, errs := tsqldialect.ParseUpdate("UPDATE c SET Active = 0 FROM dbo.Customers c LEFT JOIN inserted i ON c.ID = i.ID")
	require.Empty(t, errs)
	_, res = rewriteTSQLBody([]ast.PLStmt{u3}, ctx)
	require.True(t, hasLabel(res, "outer-joined"), "%q", res.Labels)

	d, errs := tsqldialect.ParseDelete("DELETE o FROM Sales.Orders o JOIN deleted d ON o.CustomerID = d.ID")
	require.Empty(t, errs)
	out, _ = rewriteTSQLBody([]ast.PLStmt{d}, ctx)
	require.Equal(t,
		`DELETE FROM "aw_sales"."orders" "o" USING "deleted" "d" WHERE "o"."customerid" = "d"."id"`,
		pgast.WriteStmt(out[0].(ast.Stmt)))

	ins, errs := tsqldialect.ParseInsert("INSERT INTO dbo.Customers (Name, Active) VALUES (N'x', 1)")
	require.Empty(t, errs)
	out, _ = rewriteTSQLBody([]ast.PLStmt{ins}, ctx)
	require.Equal(t, `INSERT INTO "aw_dbo"."customers" ("name", "active") VALUES ('x', true)`, pgast.WriteStmt(out[0].(ast.Stmt)))

	top, errs := tsqldialect.ParseDelete("DELETE TOP (10) FROM Sales.Orders")
	require.Empty(t, errs)
	_, res = rewriteTSQLBody([]ast.PLStmt{top}, ctx)
	require.True(t, hasLabel(res, "DELETE TOP"), "%q", res.Labels)
}

// Variables, parameters and the procedural statement targets use the PG
// spelling of their declaration; shim calls are recorded on the run.
func TestTSQLVisitors_Body(t *testing.T) {
	stmts, errs := tsqldialect.ParseRoutineBody(`
DECLARE @Done bit = 0, @When datetime;
SET @Done = 1;
SET @When = DATEADD(day, 1, GETDATE());
SELECT @N = COUNT(*) FROM dbo.Customers WHERE Name LIKE '[A-C]%';
EXEC dbo.uspLog @Msg = N'x';
`)
	require.Empty(t, errs)
	ctx := tsqlTestCtx(nil)
	ctx.Routine = &RoutineCtx{Run: ctx.Run, Kind: RoutineProcedure, Params: []RoutineParam{{SourceName: "@N", Name: "n", Type: "integer"}}}
	out, res := rewriteTSQLBody(stmts, ctx)
	require.Empty(t, res.Labels)

	var assigns []*ast.AssignStmt
	var into *ast.SelectInto
	var decls []*ast.DeclareVar
	var named []*ast.NamedArg
	for _, s := range out {
		ast.Walk(tsqlTestCollector(func(n ast.Node) {
			switch x := n.(type) {
			case *ast.AssignStmt:
				assigns = append(assigns, x)
			case *ast.SelectInto:
				into = x
			case *ast.DeclareVar:
				decls = append(decls, x)
			case *ast.NamedArg:
				named = append(named, x)
			}
		}), s)
	}
	require.Len(t, decls, 2)
	require.Equal(t, "done", decls[0].Name)
	require.Equal(t, `false`, pgast.WriteExpr(decls[0].Default))
	require.Equal(t, "when", decls[1].Name)
	require.Len(t, assigns, 2)
	require.Equal(t, "done", assigns[0].Target)
	require.Equal(t, `true`, pgast.WriteExpr(assigns[0].Expr))
	require.Equal(t, "when", assigns[1].Target)
	require.Equal(t, `tsql.dateadd('day', 1, now())`, pgast.WriteExpr(assigns[1].Expr))
	require.NotNil(t, into)
	require.Equal(t, []string{"n"}, into.Vars)
	require.Equal(t, `SELECT count(*) FROM "aw_dbo"."customers" WHERE tsql.like("name", '[A-C]%')`, pgast.WriteSelectStmt(into.Stmt))
	if len(named) > 0 {
		require.Equal(t, "msg", named[0].Name)
	}
	require.True(t, ctx.Run.Used("tsql.dateadd"))
	require.True(t, ctx.Run.Used("tsql.like"))
	require.False(t, ctx.Run.Used("tsql.datediff"))
}

type tsqlTestCollector func(ast.Node)

func (f tsqlTestCollector) Visit(n ast.Node) ast.Visitor {
	f(n)
	return f
}

// The rewriters return copies: a node reachable twice is not modified.
func TestTSQLVisitors_DoNotMutateLeaves(t *testing.T) {
	id := &ast.Ident{Parts: []string{"[X]"}}
	fc := &ast.FuncCall{Name: "ISNULL", Args: []ast.Expr{id, ast.BuildIntLit(0)}}
	got := RewriteTSQLAST(fc)
	require.Equal(t, "ISNULL", fc.Name)
	require.NotSame(t, fc, got)
	got = RewriteTSQLAST(id)
	require.Equal(t, "[X]", id.Parts[0])
	require.Equal(t, "[x]", got.(*ast.Ident).Parts[0])
}

func TestTSQLResidualIdioms(t *testing.T) {
	s, errs := tsqldialect.ParseSelect("SELECT CHECKSUM(a), x.query('for $i in /a return $i'), @@ERROR FROM t FOR XML PATH('')")
	require.Empty(t, errs)
	labels := tsqlResidualIdioms(ast.Rewrite(s, RewriteTSQLAST))
	require.Contains(t, labels, "CHECKSUM() (SQL Server hash, no PG equivalent)")
	require.Contains(t, labels, "XML data-type method query() (XQuery: FLWOR expression is not supported)")
	require.Contains(t, labels, "@@ERROR system variable")
	require.Contains(t, labels, "FOR XML clause")

	// The select hook reports FOR XML and TOP … PERCENT at every depth:
	// the PG writer drops them from a subquery as well.
	for _, src := range []string{
		"SELECT a FROM t FOR XML PATH('')",
		"SELECT g.grp, STUFF((SELECT ',' + x.name FROM dbo.t x WHERE x.grp = g.grp FOR XML PATH('')), 1, 1, '') AS names FROM dbo.t g",
	} {
		s, errs = tsqldialect.ParseSelect(src)
		require.Empty(t, errs)
		_, hookLabels := tsqlSelectHook(s, tsqlTestCtx(nil))
		require.Equal(t, []string{"FOR XML clause"}, hookLabels, src)
	}
	s, errs = tsqldialect.ParseSelect("SELECT a.id FROM (SELECT TOP 10 PERCENT id FROM dbo.t ORDER BY id) a")
	require.Empty(t, errs)
	_, hookLabels := tsqlSelectHook(s, tsqlTestCtx(nil))
	require.Equal(t, []string{"TOP … PERCENT"}, hookLabels)
}

// A view with a nested FOR XML gets one blocking warning naming it (the
// translator does not repeat the hook's label).
func TestTSQLView_NestedForXMLReported(t *testing.T) {
	res := tsqlRoutinesTranslate(t, `CREATE TABLE dbo.t (grp int NOT NULL, name varchar(20) NOT NULL);
GO
CREATE VIEW dbo.v_list AS SELECT g.grp, STUFF((SELECT ',' + x.name FROM dbo.t x WHERE x.grp = g.grp FOR XML PATH('')), 1, 1, '') AS names FROM dbo.t g
GO`)
	msgs := tsqlObjectWarnings(res, "view.dbo.v_list", "view.functions")
	require.Len(t, msgs, 1)
	require.Equal(t, 1, strings.Count(msgs[0], "FOR XML clause"), msgs[0])
}

// End to end through Translate: the hooks fold DDL expressions and the
// shim functions the objects call are created first.
func TestTSQLVisitors_TranslateWiring(t *testing.T) {
	stmts, errs := tsqldialect.Parse(`
CREATE TABLE dbo.T (
    ID int NOT NULL PRIMARY KEY,
    Code char(2) NULL CONSTRAINT CK_T_Code CHECK (Code LIKE '[A-Z][A-Z]'),
    Flag bit NOT NULL CONSTRAINT CK_T_Flag CHECK ([Flag] = (0) OR [Flag] = (1)),
    Created datetime NOT NULL DEFAULT (getdate())
);
GO
CREATE VIEW dbo.V AS SELECT ID, DATEDIFF(day, Created, GETDATE()) AS Age FROM dbo.T WHERE Flag = 1;
GO
`)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
	require.Contains(t, res.DDLScript, `tsql.like("code", '[A-Z][A-Z]')`)
	require.Contains(t, res.DDLScript, `"flag" = false OR "flag" = true`)
	require.Contains(t, res.DDLScript, `DEFAULT now()`)
	require.NotEmpty(t, res.Plan.PreActions)
	require.Contains(t, res.Plan.PreActions[0], "CREATE SCHEMA IF NOT EXISTS tsql;")
	pre := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, pre, "FUNCTION tsql.like(")
	require.Contains(t, pre, "FUNCTION tsql.datediff(")
	require.Contains(t, pre, "FUNCTION tsql.datepart_canon(")
	require.NotContains(t, pre, "FUNCTION tsql.isnumeric(")
	require.Len(t, res.Plan.Views, 1)
	require.Contains(t, res.Plan.Views[0].DDL, `tsql.datediff('day', "created", now()) AS "age"`)
	require.Contains(t, res.Plan.Views[0].DDL, `WHERE "flag" = true`)
	for _, w := range res.Warnings {
		require.NotEqual(t, tsqlWarnUntranslatedExpr, w.Kind, "%+v", w)
		require.NotEqual(t, "view.functions", w.Kind, "%+v", w)
	}
}
