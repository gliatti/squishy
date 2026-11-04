package translate

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
)

// TestTSQLAdventureWorks_Residuals translates the AdventureWorks install
// script (dumps fetched by scripts/fetch_dumps.sh, copied to
// internal/dialects/tsql/testdata; skipped when absent) and pins what is
// left untranslated: only the constructs PostgreSQL cannot host.
//
//   - XML DML (Demographics.modify()) in two triggers: a raising guard.
//   - uspSearchCandidateResumes: full-text search (CONTAINSTABLE /
//     FREETEXTTABLE) and SERVERPROPERTY('IsFullTextInstalled'); the
//     Linux load of the sample (prep-linux.sh) drops it with full-text.
//   - geography without PostGIS on the target (an extension to install).
//
// Everything else — the XML views (value() / nodes() / exist()), PIVOT,
// the hierarchyid procedures and computed columns, uspLogError's
// ERROR_*(), the computed AccountNumber — translates.
func TestTSQLAdventureWorks_Residuals(t *testing.T) {
	src, err := os.ReadFile("../dialects/tsql/testdata/instawdb.sql")
	if err != nil {
		t.Skip("AdventureWorks install script not present")
	}
	stmts, errs := tsqldialect.Parse(string(src))
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
	allowed := map[string][]string{
		"trigger.person.iuperson":                 {"XML DML has no PostgreSQL counterpart"},
		"trigger.sales.idusalesorderdetail":       {"XML DML has no PostgreSQL counterpart"},
		"procedure.dbo.uspsearchcandidateresumes": {"FREETEXTTABLE() (full-text search)", "CONTAINSTABLE() (full-text search)", "SERVERPROPERTY()"},
		"person.address.spatiallocation":          {"requires PostGIS"},
	}
	seen := map[string]bool{}
	for _, w := range res.Warnings {
		if w.Severity == string(SeverityInfo) {
			continue
		}
		ok := false
		for _, frag := range allowed[w.Object] {
			if strings.Contains(w.Message, frag) {
				ok, seen[w.Object+"|"+frag] = true, true
			}
		}
		require.True(t, ok, "unexpected non-info warning on %s (%s): %s", w.Object, w.Kind, w.Message)
	}
	for obj, frags := range allowed {
		for _, frag := range frags {
			require.True(t, seen[obj+"|"+frag], "allowlist entry no longer fires: %s %q", obj, frag)
		}
	}
	for _, v := range res.Plan.Views {
		require.NotContains(t, v.DDL, ".value(", v.Name)
		require.NotContains(t, v.DDL, "PIVOT", v.Name)
	}
}

// Sales.SalesOrderDetail.LineTotal multiplies money by (1.0 - money):
// decimal(19,4) * decimal(20,4) is decimal(40,8) by T-SQL rules, past
// precision 38, so SQL Server rounds the product to 6 decimals. The
// generated column casts it the same way.
func TestTSQLAdventureWorks_LineTotalRounding(t *testing.T) {
	res := tsqlRoutinesTranslate(t, `
CREATE TABLE Sales.SalesOrderDetail (SalesOrderID int NOT NULL, OrderQty smallint NOT NULL, UnitPrice money NOT NULL,
    UnitPriceDiscount money NOT NULL, LineTotal AS (isnull(([UnitPrice]*((1.0)-[UnitPriceDiscount]))*[OrderQty],(0.0))));
GO`)
	tbl := tsqlTableOf(res, "sales", "salesorderdetail")
	require.NotNil(t, tbl)
	require.Empty(t, tbl.CopiedGenerated)
	var gen string
	for _, c := range tbl.Columns {
		if c.Name == "linetotal" && c.Generated != nil {
			gen = c.Generated.Expr
		}
	}
	require.Contains(t, gen, `CAST(("unitprice" * (1.0 - "unitpricediscount")) AS numeric(38,6))`)
}
