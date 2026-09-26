package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// mysql_view_test.go — MySQL / MariaDB views and events go through
// parse → ast.Rewrite(RewriteMySQLAST) → dialects/postgres writer. No
// re-lexing of SQL text; a body the parser could not type is copied
// verbatim with an explicit view.parse warning.

// translateMySQL parses src with the MySQL dialect parser (no parse
// errors allowed) and translates it into the "mig" schema.
func translateMySQL(t *testing.T, src string, exts ...string) *Result {
	t.Helper()
	stmts, errs := mysqldialect.Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotEmpty(t, stmts)
	return Translate(stmts, Options{
		SourceKind:       dialects.KindMySQL,
		TargetSchema:     "mig",
		TargetExtensions: exts,
	})
}

// onlyView returns the single view of the plan.
func onlyView(t *testing.T, res *Result) PGView {
	t.Helper()
	require.Len(t, res.Plan.Views, 1)
	return res.Plan.Views[0]
}

// warningKinds returns the kinds of the warnings raised on object.
func warningKinds(res *Result, object string) []string {
	var out []string
	for _, w := range res.Warnings {
		if w.Object == object {
			out = append(out, w.Kind)
		}
	}
	return out
}

// v_complex from test/fixtures/mysql_sample.sql: GROUP_CONCAT with
// SEPARATOR, LEFT JOIN and an IN subquery.
func TestMySQLView_Complex(t *testing.T) {
	res := translateMySQL(t, `CREATE OR REPLACE VIEW v_complex AS
SELECT c.id           AS customer_id,
       c.email,
       COUNT(o.id)    AS n_orders,
       GROUP_CONCAT(o.status SEPARATOR ',') AS statuses
FROM customers c
LEFT JOIN orders o ON o.customer_id = c.id
WHERE c.id IN (SELECT customer_id FROM orders WHERE total > 0)
GROUP BY c.id, c.email;`)
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)

	require.Contains(t, v.DDL, `string_agg(CAST("o"."status" AS text), ',') AS "statuses"`)
	require.Contains(t, v.DDL, `LEFT JOIN "orders" "o" ON`)
	require.Contains(t, v.DDL, `"c"."id" IN (SELECT`)
	require.NotContains(t, v.DDL, "`")
	require.NotContains(t, strings.ToUpper(v.DDL), "GROUP_CONCAT")
	require.Equal(t, []string{"customers", "orders"}, v.References)
	require.NotContains(t, warningKinds(res, "view.v_complex"), "view.functions")
	require.NotContains(t, warningKinds(res, "view.v_complex"), "view.parse")
}

// v_order_totals from test/fixtures/mysql_sample.sql: simple JSON path.
func TestMySQLView_OrderTotalsJSONPath(t *testing.T) {
	res := translateMySQL(t, `CREATE OR REPLACE SQL SECURITY INVOKER VIEW v_order_totals AS
SELECT o.id,
       JSON_EXTRACT(o.metadata, '$.vendor') AS vendor,
       o.total
FROM orders o;`)
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)

	require.Contains(t, v.DDL, `jsonb_extract_path("o"."metadata", 'vendor') AS "vendor"`)
	require.Equal(t, []string{"orders"}, v.References)
	require.Empty(t, warningKinds(res, "view.v_order_totals"))
}

// The emp_details_view DDL of dalibo_emp_details_view_test.go: nested
// parenthesised bare joins become CROSS JOINs, the WHERE clause (which
// carries the join conditions) is preserved, the column list is quoted.
func TestMySQLView_EmpDetailsCrossJoins(t *testing.T) {
	ddl := "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER " +
		"VIEW `emp_details_view` (`employee_id`,`job_id`,`manager_id`,`department_id`," +
		"`location_id`,`country_id`,`first_name`,`last_name`,`salary`,`commission_pct`," +
		"`department_name`,`job_title`,`city`,`state_province`,`country_name`,`region_name`) AS " +
		"select `e`.`employee_id` AS `employee_id`,`e`.`job_id` AS `job_id`," +
		"`e`.`manager_id` AS `manager_id`,`e`.`department_id` AS `department_id`," +
		"`d`.`location_id` AS `location_id`,`l`.`country_id` AS `country_id`," +
		"`e`.`first_name` AS `first_name`,`e`.`last_name` AS `last_name`," +
		"`e`.`salary` AS `salary`,`e`.`commission_pct` AS `commission_pct`," +
		"`d`.`department_name` AS `department_name`,`j`.`job_title` AS `job_title`," +
		"`l`.`city` AS `city`,`l`.`state_province` AS `state_province`," +
		"`c`.`country_name` AS `country_name`,`r`.`region_name` AS `region_name` " +
		"from (((((`employees` `e` join `departments` `d`) join `jobs` `j`) join `locations` `l`) " +
		"join `countries` `c`) join `regions` `r`) " +
		"where ((`e`.`department_id` = `d`.`department_id`) and (`d`.`location_id` = `l`.`location_id`) " +
		"and (`l`.`country_id` = `c`.`country_id`) and (`c`.`region_id` = `r`.`region_id`) " +
		"and (`j`.`job_id` = `e`.`job_id`))"
	res := translateMySQL(t, ddl)
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)

	require.Equal(t, 5, strings.Count(v.DDL, "CROSS JOIN"), "five bare joins → five CROSS JOINs")
	require.Contains(t, v.DDL, `WHERE`)
	require.Contains(t, v.DDL, `"e"."department_id" = "d"."department_id"`)
	require.Contains(t, v.DDL, `"j"."job_id" = "e"."job_id"`)
	require.Contains(t, v.DDL, `"mig"."emp_details_view" ("employee_id","job_id",`)
	require.NotContains(t, v.DDL, "`")
	require.Equal(t,
		[]string{"employees", "departments", "jobs", "locations", "countries", "regions"},
		v.References)
	require.Empty(t, warningKinds(res, "view.emp_details_view"))
}

// A JSON path the rewriter cannot express (wildcard) survives as
// JSON_EXTRACT and is flagged as view.functions.
func TestMySQLView_UnrewritableJSONPathWarns(t *testing.T) {
	res := translateMySQL(t, `CREATE VIEW v_json AS SELECT JSON_EXTRACT(m, '$.a.*') AS x FROM t;`)
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)
	require.Contains(t, warningKinds(res, "view.v_json"), "view.functions")
	require.NotNil(t, v.References)
}

// A body the MySQL parser cannot type is copied verbatim with an
// explicit view.parse warning (blocking prerequisite) — never passed
// through silently.
func TestMySQLView_UnparseableBodyWarns(t *testing.T) {
	res := translateMySQL(t, `CREATE VIEW v_ft AS SELECT a FROM t WHERE MATCH (a) AGAINST ('x');`)
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)
	require.Contains(t, warningKinds(res, "view.v_ft"), "view.parse")
	require.Contains(t, v.DDL, "SELECT a FROM t WHERE MATCH (a) AGAINST ('x')")
	require.Nil(t, v.References, "no typed body → planner falls back to the token walk")

	var found bool
	for _, p := range res.Prerequisites {
		if p.Object == "view.v_ft" && p.Severity == SeverityBlocking {
			found = true
		}
	}
	require.True(t, found, "a view copied verbatim must raise a blocking prerequisite: %+v", res.Prerequisites)
	for _, e := range res.Explanations {
		if e.Object == "view.v_ft" {
			require.Contains(t, e.Reason, "copied verbatim")
			require.Equal(t, "warn", e.Level)
		}
	}
}

// v_with_check from test/fixtures/mysql_sample.sql.
func TestMySQLView_CheckOption(t *testing.T) {
	res := translateMySQL(t, `CREATE OR REPLACE VIEW v_with_check AS
SELECT id, customer_id, total, status
  FROM orders
 WHERE total > 0
WITH CASCADED CHECK OPTION;`)
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)
	require.Contains(t, v.DDL, "WITH CASCADED CHECK OPTION")
	require.Contains(t, v.DDL, `FROM "orders" WHERE "total" > 0`)
	require.Equal(t, []string{"orders"}, v.References)
	for _, e := range res.Explanations {
		if e.Object == "view.v_with_check" {
			require.Contains(t, e.Reason, "parsed and translated")
		}
	}
}

// A view with no FROM still carries a non-nil (empty) References so the
// planner trusts the typed body.
func TestMySQLView_NoRelationReferencesEmpty(t *testing.T) {
	v := onlyView(t, translateMySQL(t, `CREATE VIEW v_one AS SELECT 1 AS one;`))
	require.NotNil(t, v.References)
	require.Empty(t, v.References)
}

// ev_once from test/fixtures/mysql_sample.sql with pg_cron installed:
// the typed AT expression is rewritten and written by the PG writer.
func TestMySQLEvent_AtExpr(t *testing.T) {
	res := translateMySQL(t, `CREATE EVENT ev_once
ON SCHEDULE AT CURRENT_TIMESTAMP + INTERVAL 1 HOUR
DO
  CALL p_recalc_total(42);`, "pg_cron")
	var found bool
	for _, a := range res.Plan.PostActions {
		if strings.Contains(a, "fire_at   TIMESTAMPTZ := now() + INTERVAL '1 hour';") {
			found = true
		}
	}
	require.True(t, found, "post actions: %q", res.Plan.PostActions)
	require.NotContains(t, warningKinds(res, "event.ev_once"), "event.at")
}

// An AT expression the parser could not type is copied verbatim and
// flagged with event.at.
func TestMySQLEvent_AtExprUnparseableWarns(t *testing.T) {
	res := translateMySQL(t, `CREATE EVENT ev_bad ON SCHEDULE AT x y DO DELETE FROM t;`, "pg_cron")
	var found bool
	for _, a := range res.Plan.PostActions {
		if strings.Contains(a, "fire_at   TIMESTAMPTZ := x y;") {
			found = true
		}
	}
	require.True(t, found, "post actions: %q", res.Plan.PostActions)
	require.Contains(t, warningKinds(res, "event.ev_bad"), "event.at")
}

// Event bodies and names are source text: a body holding the dollar-quote
// tag the emitter would use, or a name holding a quote, must not break out
// of the pg_cron snippet.
func TestMySQLEvent_DollarQuoteAndNameEscaping(t *testing.T) {
	res := translateMySQL(t, "CREATE EVENT `ev'x` ON SCHEDULE EVERY 1 DAY DO INSERT INTO t VALUES ('$$');", "pg_cron")
	var snippet string
	for _, a := range res.Plan.PostActions {
		if strings.Contains(a, "cron.schedule(") {
			snippet = a
		}
	}
	require.NotEmpty(t, snippet, "post actions: %q", res.Plan.PostActions)
	require.Contains(t, snippet, "cron.schedule('ev''x', ")
	require.Contains(t, snippet, "$_1$ INSERT INTO")
	require.True(t, strings.HasSuffix(snippet, " $_1$);"), snippet)

	res = translateMySQL(t, "CREATE EVENT ev_at ON SCHEDULE AT CURRENT_TIMESTAMP + INTERVAL 1 HOUR DO INSERT INTO t VALUES ('$job$ $do$');", "pg_cron")
	snippet = ""
	for _, a := range res.Plan.PostActions {
		if strings.Contains(a, "fire_at") {
			snippet = a
		}
	}
	require.NotEmpty(t, snippet, "post actions: %q", res.Plan.PostActions)
	require.True(t, strings.HasPrefix(snippet, "DO $do_1$\n"), snippet)
	require.True(t, strings.HasSuffix(snippet, "$do_1$;"), snippet)
	require.Contains(t, snippet, "$job_1$ INSERT INTO")
}
