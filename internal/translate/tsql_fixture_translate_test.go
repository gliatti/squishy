package translate

import (
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsqlFixtureWarningAllowlist lists the non-info warning kinds the
// end-to-end translation of test/fixtures/sqlserver_sample.sql may
// raise. Every entry is a known gap or a deliberate blocking review;
// the SQL Server campaign shrinks this list as the gaps close. An entry
// that no longer fires fails the test too, so the list never goes
// stale.
var tsqlFixtureWarningAllowlist = map[string]string{
	// usp_count_customers: EXEC (@msg) runs a string built at run time
	// (concatenated with @country / @count), T-SQL that cannot be
	// translated ahead of time. (Its constant sp_executesql statement is
	// inlined as static SQL, the MERGE … OUTPUT result set is dropped
	// like a result-set SELECT, and UPDATE(col) is exact through an
	// UPDATE OF helper trigger.)
	"routine.untranslated_construct": "dynamic SQL built at run time",
	// [Order Details].line_total converts float arithmetic (real
	// discount) to money: not proven identical on PG, so it is a plain
	// copied column until an override provides the expression.
	// (t_computed.c_label / c_is_bulk are proven and generated.)
	"table.computed_not_generated": "computed column over float arithmetic",
	// t_special.c_geography / c_geometry: the test target has no
	// PostGIS, so the spatial columns are EWKT text placeholders.
	"type.spatial_no_postgis": "spatial columns without PostGIS",
}

// TestTSQLFixture_TranslateEndToEnd parses the SQL Server sample fixture
// with the T-SQL dialect and translates it with every SQL Server hook
// registered (visitors, shim, routine bodies) and schemas preserved —
// the options httpapi uses for kind sqlserver.
func TestTSQLFixture_TranslateEndToEnd(t *testing.T) {
	src, err := os.ReadFile("../../test/fixtures/sqlserver_sample.sql")
	require.NoError(t, err)
	stmts, errs := tsqldialect.Parse(string(src))
	require.Empty(t, errs, "the fixture must parse cleanly: %v", errs)

	res := Translate(stmts, Options{
		TargetSchema:          "public",
		SourceKind:            dialects.KindSQLServer,
		PreserveSourceSchemas: true,
	})

	// Every object the fixture creates is in the plan, in its own
	// (folded) schema. The expected set is read from the AST so the
	// test follows the fixture; the counts pin what the fixture holds
	// today, so a parser regression that drops statements shows up.
	want := map[string][]string{}
	add := func(kind, schema, name string) {
		if schema == "" {
			schema = "dbo"
		}
		want[kind] = append(want[kind], normalizeTSQLIdent(schema)+"."+normalizeTSQLIdent(name))
	}
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.CreateTable:
			add("table", x.Schema, x.Name)
		case *ast.CreateView:
			add("view", x.View.Schema, x.View.Name)
		case *ast.CreateProcedure:
			add("procedure", x.Schema, x.Name)
		case *ast.CreateFunction:
			add("function", x.Schema, x.Name)
		case *ast.CreateTrigger:
			// A T-SQL trigger lives in the schema of its table.
			add("trigger", x.Table.Schema, x.Name)
		}
	}
	got := map[string][]string{}
	// The history table of the emulated system-versioned dbo.Employees
	// is not created by the script (SQL Server creates it): it is in the
	// plan on top of the script's tables.
	histories := map[string]bool{}
	for _, tb := range res.Plan.Tables {
		if tv := tb.TSQLTemporal; tv != nil {
			histories[tv.HistorySchema+"."+tv.HistoryTable] = true
		}
	}
	require.Equal(t, map[string]bool{"dbo.employeeshistory": true}, histories)
	for _, tb := range res.Plan.Tables {
		if histories[tb.Schema+"."+tb.Name] {
			continue
		}
		got["table"] = append(got["table"], tb.Schema+"."+tb.Name)
	}
	for _, v := range res.Plan.Views {
		got["view"] = append(got["view"], v.Schema+"."+v.Name)
		require.NotEmpty(t, v.DDL, "view %s.%s", v.Schema, v.Name)
	}
	for _, r := range res.Plan.Routines {
		got[r.Kind] = append(got[r.Kind], r.Schema+"."+r.Name)
		require.NotEmpty(t, r.DDL, "%s %s.%s", r.Kind, r.Schema, r.Name)
	}
	counts := map[string]int{"table": 12, "view": 3, "procedure": 6, "function": 3, "trigger": 3}
	for kind, n := range counts {
		require.Len(t, want[kind], n, "fixture %ss", kind)
		require.ElementsMatch(t, want[kind], got[kind], "%ss in the plan", kind)
	}
	require.ElementsMatch(t, []string{"dbo", "sales"}, schemasOf(res.Plan.Tables))

	// Non-info warnings: only the allowlisted kinds, and each of them
	// still fires.
	seen := map[string]bool{}
	var unexpected []string
	for _, w := range res.Warnings {
		if w.Severity == string(SeverityInfo) {
			continue
		}
		if _, ok := tsqlFixtureWarningAllowlist[w.Kind]; !ok {
			unexpected = append(unexpected, w.Kind+" on "+w.Object+": "+w.Message)
			continue
		}
		seen[w.Kind] = true
	}
	require.Empty(t, unexpected, "non-info warnings outside the allowlist")
	for _, w := range res.Warnings {
		switch w.Kind {
		case "routine.untranslated_construct":
			require.Equal(t, "procedure.dbo.usp_count_customers", w.Object, w.Message)
			require.Contains(t, w.Message, "dynamic SQL")
		case "table.computed_not_generated":
			require.Equal(t, "sales.order details.line_total", w.Object)
		}
	}
	for kind := range tsqlFixtureWarningAllowlist {
		require.True(t, seen[kind], "allowlisted warning kind %q no longer fires: remove it from tsqlFixtureWarningAllowlist", kind)
	}

	// The hooks ran: the CHECKs call tsql.like, so the shim is emitted
	// first, and the seams between the passes left no T-SQL behind.
	require.NotEmpty(t, res.Plan.PreActions)
	require.Contains(t, res.Plan.PreActions[0], "CREATE SCHEMA IF NOT EXISTS tsql;")
	require.Contains(t, res.DDLScript, "FUNCTION tsql.like(")
	var all []string
	all = append(all, res.DDLScript, res.DDLPostCopy)
	for _, v := range res.Plan.Views {
		all = append(all, v.DDL)
	}
	for _, r := range res.Plan.Routines {
		all = append(all, r.DDL)
	}
	for _, ddl := range all {
		// Test-side inspection of the emitted text (never done by the
		// translator itself): a variable left with its @, a FROM item
		// the writer could not render, an APPLY join not turned into a
		// LATERAL one.
		require.NotContains(t, ddl, `"@`)
		require.NotContains(t, ddl, "FROM  ")
		require.NotContains(t, ddl, " APPLY ")
	}

	// Seams checked one by one.
	view := planView(res, "sales", "v_top_customers")
	require.NotNil(t, view)
	require.Contains(t, view.DDL, `CROSS JOIN LATERAL (SELECT "o2"."order_date" FROM "sales"."orders" "o2"`)
	require.Contains(t, view.DDL, `LEFT JOIN LATERAL (SELECT sum("f"."total") AS "spent" FROM "sales"."fn_orders_of"("c"."customer_id") "f" WHERE "f"."total" > 100) "s" ON true`)
	// Inline TVF: the parameter is qualified with the function name, or
	// LANGUAGE sql would read the column of the same name.
	of := tsqlRoutine(res, "function", "fn_orders_of")
	require.NotNil(t, of)
	require.Contains(t, of.DDL, `WHERE "o"."customer_id" = "fn_orders_of"."customer_id";`)
	// READONLY table-valued parameter: unnest() of the composite array.
	lines := tsqlRoutine(res, "procedure", "usp_add_lines")
	require.NotNil(t, lines)
	require.Contains(t, lines.DDL, `FROM unnest("usp_add_lines"."lines") "l" INNER JOIN "dbo"."products" "p"`)
	// INSERT … SELECT of a bit literal into a boolean column.
	ins := tsqlRoutine(res, "trigger", "trg_v_products_active_insert")
	require.NotNil(t, ins)
	require.Contains(t, ins.DDL, `coalesce("i"."list_price", 0), false FROM (SELECT NEW.*) "i"`)
}

func schemasOf(tables []PGTable) []string {
	set := map[string]bool{}
	for _, tb := range tables {
		set[tb.Schema] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func planView(res *Result, schema, name string) *PGView {
	for i := range res.Plan.Views {
		if res.Plan.Views[i].Schema == schema && res.Plan.Views[i].Name == name {
			return &res.Plan.Views[i]
		}
	}
	return nil
}
