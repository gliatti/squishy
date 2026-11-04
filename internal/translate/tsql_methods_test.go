package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// xml.nodes() row sources: an XMLTABLE per APPLY, the value() reads of a
// row anchored on the row's element, the default namespace prefixed.
func TestTSQLMethods_XMLNodes(t *testing.T) {
	const ns = `declare default element namespace "urn:r";`
	src := `SELECT jc.JobCandidateID,
	    [Resume].ref.value(N'` + ns + ` (/Resume/Name/Name.First)[1]', 'nvarchar(30)') AS [Name.First],
	    [Resume].ref.value(N'` + ns + ` (Address/Addr.Type)[1]', 'nvarchar(30)') AS [Addr.Type]
	FROM HumanResources.JobCandidate jc
	CROSS APPLY jc.[Resume].nodes(N'` + ns + ` /Resume') AS Resume(ref)`
	got, res := tsqlSelectOut(t, src, tsqlTestCtx(nil))
	require.Empty(t, res.Labels)
	require.Equal(t, `SELECT "jc"."jobcandidateid", `+
		`CAST((SELECT "v" FROM XMLTABLE(XMLNAMESPACES('urn:r' AS "dns"), '/' PASSING ("resume"."ref") COLUMNS "v" text PATH '(/dns:Resume/dns:Name/dns:Name.First)[1]') AS "_x") AS varchar(30)) AS "name.first", `+
		`CAST((SELECT "v" FROM XMLTABLE(XMLNAMESPACES('urn:r' AS "dns"), '/' PASSING ("resume"."ref") COLUMNS "v" text PATH '(/*/dns:Address/dns:Addr.Type)[1]') AS "_x") AS varchar(30)) AS "addr.type" `+
		`FROM "aw_humanresources"."jobcandidate" "jc" CROSS JOIN LATERAL `+
		`XMLTABLE(XMLNAMESPACES('urn:r' AS "dns"), '/dns:Resume' PASSING ("jc"."resume") COLUMNS "ref" xml PATH '.') AS "resume"`, got)

	// Nested nodes(): the inner row source reads the outer row; its
	// relative paths, attributes and `.` are anchored on the row element.
	src = `SELECT [MfgInstructions].ref.value('@LocationID[1]', 'int') AS LocationID,
	    [Steps].ref.value('string(.)[1]', 'nvarchar(1024)') AS Step
	FROM Production.ProductModel
	CROSS APPLY [Instructions].nodes(N'` + ns + ` /root/Location') MfgInstructions(ref)
	CROSS APPLY [MfgInstructions].ref.nodes('` + ns + ` step') Steps(ref)`
	got, res = tsqlSelectOut(t, src, tsqlTestCtx(nil))
	require.Empty(t, res.Labels)
	require.Contains(t, got, `COLUMNS "v" integer PATH '/*/@LocationID[1]'`)
	require.Contains(t, got, `COLUMNS "v" text PATH 'string(/*)') AS "_x") AS varchar(1024))`, "read as text, cast (truncating) to the type")
	require.Contains(t, got, `XMLTABLE(XMLNAMESPACES('urn:r' AS "dns"), '/*/dns:step' PASSING ("mfginstructions"."ref") COLUMNS "ref" xml PATH '.') AS "steps"`)

	// An absolute path read from a row that is not its document's element
	// would select other nodes on PG: reported.
	src = `SELECT s.ref.value('(/root/x)[1]', 'int') FROM t CROSS APPLY t.doc.nodes('/root/Location') AS s(ref)`
	_, res = tsqlSelectOut(t, src, tsqlTestCtx(nil))
	require.True(t, hasLabel(res, "not its document's element"), "labels: %q", res.Labels)

	// OUTER APPLY keeps the rows without a node.
	src = `SELECT c.ref.value('declare namespace act="urn:a"; (act:number)[1]', 'nvarchar(50)') FROM Person.Person OUTER APPLY AdditionalContactInfo.nodes('declare namespace ci="urn:c"; /ci:AdditionalContactInfo') AS c(ref)`
	got, res = tsqlSelectOut(t, src, tsqlTestCtx(nil))
	require.Empty(t, res.Labels)
	require.Contains(t, got, `LEFT JOIN LATERAL XMLTABLE(XMLNAMESPACES('urn:c' AS "ci"), '/ci:AdditionalContactInfo' PASSING ("additionalcontactinfo") COLUMNS "ref" xml PATH '.') AS "c" ON true`)
	require.Contains(t, got, `XMLNAMESPACES('urn:a' AS "act"), '/' PASSING ("c"."ref") COLUMNS "v" text PATH '(/*/act:number)[1]'`)
}

// The default-namespace prefix never collides with a declared one.
func TestTSQLMethods_FreePrefix(t *testing.T) {
	got, res := tsqlExprOut(t, `x.value('declare namespace dns="urn:a"; declare default element namespace "urn:d"; (/dns:a/b/@c)[1]', 'int')`, tsqlTestCtx(nil))
	require.Empty(t, res.Labels)
	require.Contains(t, got, `XMLNAMESPACES('urn:a' AS "dns", 'urn:d' AS "dns_")`)
	require.Contains(t, got, `PATH '(/dns:a/dns_:b/@c)[1]'`)
}

// PIVOT becomes conditional aggregation grouped by the other columns.
func TestTSQLPivot(t *testing.T) {
	src := `SELECT pvt.[SalesPersonID], pvt.[FullName], pvt.[2002], pvt.[2003]
	FROM (SELECT soh.[SalesPersonID], p.[FirstName] AS [FullName], soh.[SubTotal], YEAR(soh.[OrderDate]) AS [FiscalYear]
	      FROM Sales.SalesOrderHeader soh JOIN Person.Person p ON p.BusinessEntityID = soh.SalesPersonID) AS soh
	PIVOT (SUM([SubTotal]) FOR [FiscalYear] IN ([2002], [2003])) AS pvt`
	got, res := tsqlSelectOut(t, src, tsqlTestCtx(nil))
	require.Empty(t, res.Labels)
	require.Contains(t, got, `FROM (SELECT "soh"."salespersonid", "soh"."fullname", `+
		`sum(CASE WHEN "soh"."fiscalyear" = '2002' THEN "soh"."subtotal" END) AS "2002", `+
		`sum(CASE WHEN "soh"."fiscalyear" = '2003' THEN "soh"."subtotal" END) AS "2003" FROM (SELECT `)
	require.Contains(t, got, `) "soh" GROUP BY "soh"."salespersonid", "soh"."fullname") "pvt"`)

	// A source without a known column list stays reported.
	_, res = tsqlSelectOut(t, `SELECT * FROM (SELECT * FROM t) AS s PIVOT (MAX(v) FOR k IN ([a])) AS p`, tsqlTestCtx(nil))
	require.True(t, hasLabel(res, "PIVOT"), "labels: %q", res.Labels)
}

// A computed column calling a deterministic (schema-bound, table-free)
// function of the script is computed by a trigger; GetLevel() of a
// hierarchyid stays a generated column.
func TestTSQLComputedColumns_FunctionsAndHierarchy(t *testing.T) {
	res := tsqlRoutinesTranslate(t, `
CREATE FUNCTION dbo.ufnLeadingZeros(@Value int) RETURNS varchar(8) WITH SCHEMABINDING AS
BEGIN
    DECLARE @ReturnValue varchar(8);
    SET @ReturnValue = CONVERT(varchar(8), @Value);
    SET @ReturnValue = REPLICATE('0', 8 - DATALENGTH(@ReturnValue)) + @ReturnValue;
    RETURN (@ReturnValue);
END;
GO
CREATE FUNCTION dbo.ufnNow(@v int) RETURNS datetime AS BEGIN RETURN GETDATE(); END;
GO
CREATE TABLE Sales.Customer (
    CustomerID int IDENTITY(1,1) NOT NULL PRIMARY KEY,
    AccountNumber AS (ISNULL('AW' + dbo.ufnLeadingZeros(CustomerID), '')),
    Stamp AS (dbo.ufnNow(CustomerID)));
GO
CREATE TABLE HumanResources.Employee (
    BusinessEntityID int NOT NULL PRIMARY KEY,
    OrganizationNode hierarchyid NULL,
    OrganizationLevel AS (OrganizationNode.GetLevel()));
GO
`)
	post := strings.Join(res.Plan.PostActions, "\n")
	require.Contains(t, post, `NEW."accountnumber" := coalesce('AW' || "dbo"."ufnleadingzeros"(NEW."customerid"), '');`)
	require.Contains(t, post, `CREATE TRIGGER "customer_accountnumber_compute" BEFORE INSERT OR UPDATE ON "sales"."customer"`)
	require.Empty(t, tsqlObjectWarnings(res, "sales.customer.accountnumber", "table.computed_not_generated"))
	// GETDATE() is not deterministic: still refused.
	require.NotEmpty(t, tsqlObjectWarnings(res, "sales.customer.stamp", "table.computed_not_generated"))
	require.NotContains(t, post, "stamp_compute")

	var emp *PGTable
	for i := range res.Plan.Tables {
		if res.Plan.Tables[i].Name == "employee" {
			emp = &res.Plan.Tables[i]
		}
	}
	require.NotNil(t, emp)
	lvl := emp.Columns[2]
	require.Equal(t, "smallint", lvl.Type)
	require.NotNil(t, lvl.Generated, "GetLevel() is a generated column")
	require.Equal(t, `tsql.hierarchyid_getlevel("organizationnode")`, lvl.Generated.Expr)
	require.Contains(t, strings.Join(res.Plan.PreActions, "\n"), "CREATE OR REPLACE FUNCTION tsql.hierarchyid_getlevel(h text)")
}
