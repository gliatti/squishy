package translate

import (
	"reflect"
	"strings"
	"testing"

	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// mysqlRewriteExpr parses a MySQL expression, runs the full MySQL → PG
// pipeline and renders the result with the PG writer.
func mysqlRewriteExpr(t *testing.T, src string) string {
	t.Helper()
	e, errs := mysqldialect.ParseExpr(src)
	if len(errs) > 0 {
		t.Fatalf("ParseExpr(%q): %v", src, errs)
	}
	return pgast.WriteExpr(rewriteMySQLExpr(e))
}

// mysqlRewriteSelect parses a MySQL SELECT, runs the pipeline and
// renders the result.
func mysqlRewriteSelect(t *testing.T, src string) string {
	t.Helper()
	sel, errs := mysqldialect.ParseSelect(src)
	if len(errs) > 0 {
		t.Fatalf("ParseSelect(%q): %v", src, errs)
	}
	return pgast.WriteSelectStmt(rewriteMySQLSelect(sel))
}

func runMySQLExprCases(t *testing.T, cases []struct{ in, want string }) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := mysqlRewriteExpr(t, c.in); got != c.want {
				t.Errorf("%s\n got: %s\nwant: %s", c.in, got, c.want)
			}
		})
	}
}

func TestMySQLVisitNullSafeEq(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"a <=> b", `"a" IS NOT DISTINCT FROM "b"`},
		{"a = b", `"a" = "b"`},
	})
}

func TestMySQLVisitFuncRenames(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"IFNULL(x, 0)", `coalesce("x", 0)`},
		{"ifnull(x, 0)", `coalesce("x", 0)`},
		{"CHAR_LENGTH(s)", `length("s")`},
		{"CHARACTER_LENGTH(s)", `length("s")`},
		{"CURRENT_TIMESTAMP(3)", `now()`},
		{"CURRENT_TIMESTAMP", `now()`},
		{"NOW()", `now()`},
		{"NOW(6)", `now()`},
		{"LOCALTIMESTAMP()", `now()`},
		{"SYSDATE()", `now()`},
		{"UUID()", `gen_random_uuid()`},
		{"CONCAT(a, '-', b)", `concat("a", '-', "b")`},
		{"TRUNCATE(x, 2)", `trunc("x", 2)`},
		{"CURDATE()", `CURRENT_DATE`},
		{"CURTIME()", `CURRENT_TIME`},
		{"CURRENT_DATE", `CURRENT_DATE`},
		{"CURRENT_TIME", `CURRENT_TIME`},
		// Untouched.
		{"COALESCE(a, b)", `COALESCE("a", "b")`},
		{"UPPER(a)", `UPPER("a")`},
	})
}

func TestMySQLVisitGroupConcat(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"GROUP_CONCAT(o.status)", `string_agg(CAST("o"."status" AS text), ',')`},
		{"GROUP_CONCAT(o.status SEPARATOR ',')", `string_agg(CAST("o"."status" AS text), ',')`},
		{"GROUP_CONCAT(DISTINCT name ORDER BY name DESC SEPARATOR '; ')",
			`string_agg(DISTINCT CAST("name" AS text), '; ' ORDER BY "name" DESC)`},
		{"GROUP_CONCAT(a, b)", `string_agg(CAST(concat("a", "b") AS text), ',')`},
		{"GROUP_CONCAT(IFNULL(a, '') ORDER BY a)", `string_agg(CAST(coalesce("a", '') AS text), ',' ORDER BY "a")`},
	})
}

func TestMySQLVisitJSONExtract(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"JSON_EXTRACT(o.metadata, '$.vendor')", `jsonb_extract_path("o"."metadata", 'vendor')`},
		{"JSON_UNQUOTE(JSON_EXTRACT(m, '$.a.b[0]'))", `jsonb_extract_path_text("m", 'a', 'b', '0')`},
		{"JSON_EXTRACT(m, '$[2].x')", `jsonb_extract_path("m", '2', 'x')`},
		{"JSON_EXTRACT(m, '$.a.*')", `JSON_EXTRACT("m", '$.a.*')`},
		{`JSON_EXTRACT(m, '$."quoted key"')`, `JSON_EXTRACT("m", '$."quoted key"')`},
		{"JSON_EXTRACT(m, '$**.a')", `JSON_EXTRACT("m", '$**.a')`},
		{"JSON_EXTRACT(m, '$')", `JSON_EXTRACT("m", '$')`},
		{"JSON_EXTRACT(m, p)", `JSON_EXTRACT("m", "p")`},
		{"JSON_UNQUOTE(m)", `"m" #>> '{}'`},
		{"JSON_UNQUOTE(IFNULL(a, b))", `coalesce("a", "b") #>> '{}'`},
	})
}

func TestMySQLVisitJSONPathParser(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		ok   bool
	}{
		{"$.a", []string{"a"}, true},
		{"$.a.b[0]", []string{"a", "b", "0"}, true},
		{"$[10]", []string{"10"}, true},
		{"$.a_1.B2", []string{"a_1", "B2"}, true},
		{"$", []string{}, true},
		{"a.b", nil, false},
		{"$.", nil, false},
		{"$.a.*", nil, false},
		{"$[*]", nil, false},
		{"$**.a", nil, false},
		{`$."k"`, nil, false},
		{"$[last]", nil, false},
		{"$[1 to 3]", nil, false},
		{"$[1", nil, false},
		{"$ .a", nil, false},
	}
	for _, c := range cases {
		got, ok := parseMySQLJSONPath(c.in)
		if ok != c.ok {
			t.Errorf("parseMySQLJSONPath(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseMySQLJSONPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMySQLVisitBareJoin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT 1 FROM a JOIN b", `SELECT 1 FROM "a" CROSS JOIN "b"`},
		{"SELECT 1 FROM a INNER JOIN b", `SELECT 1 FROM "a" CROSS JOIN "b"`},
		{"SELECT 1 FROM a JOIN b ON a.id = b.id", `SELECT 1 FROM "a" INNER JOIN "b" ON "a"."id" = "b"."id"`},
		{"SELECT 1 FROM a JOIN b USING (id)", `SELECT 1 FROM "a" INNER JOIN "b" USING ("id")`},
		{"SELECT 1 FROM a LEFT JOIN b ON a.id = b.id", `SELECT 1 FROM "a" LEFT JOIN "b" ON "a"."id" = "b"."id"`},
		{"SELECT 1 FROM a NATURAL JOIN b", `SELECT 1 FROM "a" NATURAL INNER JOIN "b"`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := mysqlRewriteSelect(t, c.in); got != c.want {
				t.Errorf("\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

// TestMySQLVisitBareJoinEmpDetailsView uses the FROM shape mysqldump
// emits for the HR sample's emp_details_view: nested, parenthesised
// bare joins with every predicate in WHERE.
func TestMySQLVisitBareJoinEmpDetailsView(t *testing.T) {
	src := "select `e`.`employee_id` AS `employee_id`,`r`.`region_name` AS `region_name` " +
		"from (((((`employees` `e` join `departments` `d`) join `jobs` `j`) join `locations` `l`) " +
		"join `countries` `c`) join `regions` `r`) " +
		"where ((`e`.`department_id` = `d`.`department_id`) and (`c`.`region_id` = `r`.`region_id`))"
	got := mysqlRewriteSelect(t, src)
	want := `SELECT "e"."employee_id" AS "employee_id", "r"."region_name" AS "region_name" ` +
		`FROM "employees" "e" CROSS JOIN "departments" "d" CROSS JOIN "jobs" "j" ` +
		`CROSS JOIN "locations" "l" CROSS JOIN "countries" "c" CROSS JOIN "regions" "r" ` +
		`WHERE (("e"."department_id" = "d"."department_id") AND ("c"."region_id" = "r"."region_id"))`
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
	if n := strings.Count(got, "CROSS JOIN"); n != 5 {
		t.Errorf("want 5 CROSS JOINs, got %d", n)
	}
	if strings.Contains(got, "INNER JOIN") {
		t.Errorf("bare INNER JOIN left behind: %s", got)
	}
}

func TestMySQLVisitInterval(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"INTERVAL 1 HOUR", `INTERVAL '1 hour'`},
		{"INTERVAL 1 HOURS", `INTERVAL '1 hour'`},
		{"INTERVAL '5' DAY", `INTERVAL '5 day'`},
		{"INTERVAL -1 DAY", `INTERVAL '-1 day'`},
		{"INTERVAL 1.5 SECOND", `INTERVAL '1.5 second'`},
		{"INTERVAL 3 WEEK", `INTERVAL '3 week'`},
		{"INTERVAL 10 MICROSECOND", `INTERVAL '10 microsecond'`},
		{"INTERVAL 2 QUARTER", `INTERVAL '6 months'`},
		{"INTERVAL n QUARTER", `"n" * INTERVAL '3 months'`},
		{"INTERVAL n DAY", `"n" * INTERVAL '1 day'`},
		{"INTERVAL t.n MINUTE", `"t"."n" * INTERVAL '1 minute'`},
		{"INTERVAL (x + 1) DAY", `("x" + 1) * INTERVAL '1 day'`},
		{"INTERVAL n * 7 DAY", `("n" * 7) * INTERVAL '1 day'`},
		{"INTERVAL '1-2' YEAR_MONTH", `INTERVAL '1-2' YEAR_MONTH`},
		{"INTERVAL '1:30' HOUR", `INTERVAL '1:30' HOUR`},
		// A string quantity is a literal, never a column reference: it
		// is left untouched (and reported) rather than guessed at.
		{"INTERVAL 'abc' DAY", `INTERVAL 'abc' DAY`},
		{"d + INTERVAL 'abc' DAY", `"d" + INTERVAL 'abc' DAY`},
	})
}

func TestMySQLVisitDateAddSub(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"DATE_ADD(d, INTERVAL 1 DAY)", `"d" + INTERVAL '1 day'`},
		{"ADDDATE(d, INTERVAL 2 HOUR)", `"d" + INTERVAL '2 hour'`},
		{"DATE_SUB(d, INTERVAL n MONTH)", `"d" - "n" * INTERVAL '1 month'`},
		{"SUBDATE(NOW(), INTERVAL 1 YEAR)", `now() - INTERVAL '1 year'`},
		{"DATE_ADD(CURDATE(), INTERVAL 2 QUARTER)", `CURRENT_DATE + INTERVAL '6 months'`},
		// Not an interval: MySQL ADDDATE(d, days) is left untouched.
		{"ADDDATE(d, 3)", `ADDDATE("d", 3)`},
		// Compound unit: untouched (reported as residual).
		{"DATE_ADD(d, INTERVAL '1-2' YEAR_MONTH)", `DATE_ADD("d", INTERVAL '1-2' YEAR_MONTH)`},
	})
}

func TestMySQLVisitCastType(t *testing.T) {
	got := mysqlRewriteExpr(t, "CAST(x AS UNSIGNED)")
	if !strings.HasPrefix(got, `CAST("x" AS `) {
		t.Fatalf("unexpected render: %s", got)
	}
	if strings.Contains(strings.ToLower(got), "text") {
		t.Errorf("CAST AS UNSIGNED must map to a numeric PG type, got %s", got)
	}
	if !strings.Contains(strings.ToUpper(got), "NUMERIC") && !strings.Contains(strings.ToUpper(got), "INT") {
		t.Errorf("CAST AS UNSIGNED must map to a numeric PG type, got %s", got)
	}
	runMySQLExprCases(t, []struct{ in, want string }{
		{"CAST(x AS SIGNED)", `CAST("x" AS BIGINT)`},
		{"CAST(d AS DATE)", `CAST("d" AS DATE)`},
		{"CAST(x AS DECIMAL(10,2))", `CAST("x" AS NUMERIC(10,2))`},
		// MySQL CAST AS CHAR is an unbounded string, CHAR(N) a
		// variable-length string truncated to N: never PG char(1) /
		// blank-padded char(N).
		{"CAST(x AS CHAR)", `CAST("x" AS text)`},
		{"CAST(x AS CHAR(10))", `CAST("x" AS varchar(10))`},
		{"CONCAT('id-', CAST(id AS CHAR))", `concat('id-', CAST("id" AS text))`},
	})
}

// TestMySQLVisitOperandPrecedence checks that calls rewritten into
// operators keep their grouping once rendered by the PG writer, which
// does not parenthesise BinaryExpr operands itself.
func TestMySQLVisitOperandPrecedence(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		// Regressions: the call used to render bare and regroup.
		{"x - DATE_SUB(d, INTERVAL 1 DAY)", `"x" - ("d" - INTERVAL '1 day')`},
		{"DATE_ADD(a, INTERVAL 1 DAY) - DATE_SUB(d, INTERVAL 1 DAY)",
			`"a" + INTERVAL '1 day' - ("d" - INTERVAL '1 day')`},
		{"x - DATE_ADD(d, INTERVAL n DAY)", `"x" - ("d" + "n" * INTERVAL '1 day')`},
		{"JSON_UNQUOTE(m) + 1", `("m" #>> '{}') + 1`},
		{"1 + JSON_UNQUOTE(m)", `1 + ("m" #>> '{}')`},
		{"a <=> b = c", `("a" IS NOT DISTINCT FROM "b") = "c"`},
		// Already correct under PG precedence: no parentheses added.
		{"JSON_UNQUOTE(m) = 'x'", `"m" #>> '{}' = 'x'`},
		{"a = b <=> c", `"a" = "b" IS NOT DISTINCT FROM "c"`},
		{"NOT a <=> b", `NOT "a" IS NOT DISTINCT FROM "b"`},
		{"DATE_SUB(d, INTERVAL 1 DAY) BETWEEN a AND b", `"d" - INTERVAL '1 day' BETWEEN "a" AND "b"`},
		{"DATE_ADD(d, INTERVAL 1 DAY) < NOW()", `"d" + INTERVAL '1 day' < now()`},
		{"a + b * c - d", `"a" + "b" * "c" - "d"`},
		{"a OR b AND c", `"a" OR "b" AND "c"`},
		{"(a OR b) AND c", `("a" OR "b") AND "c"`},
		{"-(a - b)", `-("a" - "b")`},
		// PG comparison operators are non-associative.
		{"a < b = c", `("a" < "b") = "c"`},
	})
}

func TestMySQLVisitDoubleQuotedString(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"`a` = \"b\"", `"a" = 'b'`},
	})
}

// TestRewriteMySQLAST_ReachesCTEAndOnDuplicateKey proves the pipeline
// reaches CTE bodies and the ON DUPLICATE KEY UPDATE payload.
func TestRewriteMySQLAST_ReachesCTEAndOnDuplicateKey(t *testing.T) {
	got := mysqlRewriteSelect(t, "WITH c AS (SELECT IFNULL(x, 0) AS y FROM t JOIN u) SELECT GROUP_CONCAT(y) FROM c")
	want := `WITH "c" AS (SELECT coalesce("x", 0) AS "y" FROM "t" CROSS JOIN "u") SELECT string_agg(CAST("y" AS text), ',') FROM "c"`
	if got != want {
		t.Errorf("CTE:\n got: %s\nwant: %s", got, want)
	}

	ins, errs := mysqldialect.ParseInsert("INSERT INTO t (a, b) VALUES (1, NOW()) ON DUPLICATE KEY UPDATE b = IFNULL(b, CURRENT_TIMESTAMP(3))")
	if len(errs) > 0 {
		t.Fatalf("ParseInsert: %v", errs)
	}
	out, ok := ast.Rewrite(ins, RewriteMySQLAST).(*ast.InsertStmt)
	if !ok {
		t.Fatalf("Rewrite returned %T", out)
	}
	if out.OnConflict == nil || len(out.OnConflict.Sets) != 1 {
		t.Fatalf("ON DUPLICATE KEY UPDATE not modelled: %+v", out.OnConflict)
	}
	if got := pgast.WriteExpr(out.OnConflict.Sets[0].Expr); got != `coalesce("b", now())` {
		t.Errorf("ON DUPLICATE KEY UPDATE payload: got %s", got)
	}
	if got := pgast.WriteExpr(out.Values[0][1]); got != `now()` {
		t.Errorf("VALUES payload: got %s", got)
	}
}

// TestRewriteMySQLAST_RoutineBody checks applyMySQLASTRewrites reaches
// procedural statement slots.
func TestRewriteMySQLAST_RoutineBody(t *testing.T) {
	stmts, errs := mysqldialect.ParseRoutineBody(`BEGIN
  DECLARE v INT DEFAULT IFNULL(p, 0);
  SET v = v + CHAR_LENGTH(s);
  IF a <=> b THEN
    SET d = DATE_ADD(d, INTERVAL 1 DAY);
  END IF;
END`)
	if len(errs) > 0 {
		t.Fatalf("ParseRoutineBody: %v", errs)
	}
	out := applyMySQLASTRewrites(stmts)
	var rendered []string
	for _, s := range out {
		rendered = append(rendered, pgast.WritePLStmt(s))
	}
	joined := strings.Join(rendered, "\n")
	for _, want := range []string{
		`coalesce("p", 0)`,
		`length("s")`,
		`IS NOT DISTINCT FROM`,
		`INTERVAL '1 day'`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("routine body missing %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"IFNULL", "CHAR_LENGTH", "<=>", "DATE_ADD"} {
		if strings.Contains(joined, bad) {
			t.Errorf("routine body still contains %q:\n%s", bad, joined)
		}
	}
}

// TestRewriteMySQLAST_NoSharedMutation checks the rewriters build new
// nodes instead of mutating the ones they match.
func TestRewriteMySQLAST_NoSharedMutation(t *testing.T) {
	orig := &ast.FuncCall{Name: "IFNULL", Args: []ast.Expr{ast.BuildIdent("x"), ast.BuildIntLit(0)}}
	got := VisitMySQLFuncRenames(orig)
	if got == ast.Node(orig) {
		t.Fatal("expected a replacement node")
	}
	if orig.Name != "IFNULL" {
		t.Errorf("original FuncCall mutated: %s", orig.Name)
	}
	bin := &ast.BinaryExpr{Op: "<=>", Lhs: ast.BuildIdent("a"), Rhs: ast.BuildIdent("b")}
	VisitMySQLNullSafeEq(bin)
	if bin.Op != "<=>" {
		t.Errorf("original BinaryExpr mutated: %s", bin.Op)
	}
	j := &ast.FromJoin{Kind: ast.InnerJoin, Left: &ast.FromTable{Name: "a"}, Right: &ast.FromTable{Name: "b"}}
	VisitMySQLBareJoin(j)
	if j.Kind != ast.InnerJoin {
		t.Errorf("original FromJoin mutated")
	}
}

func TestMySQLResidualIdioms(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"JSON_EXTRACT(m, '$.a.*')", []string{"JSON_EXTRACT: JSON path uses wildcards/quoted keys — manual review"}},
		{"DATE_FORMAT(d, '%Y-%m')", []string{"DATE_FORMAT: format codes differ from PG to_char/to_date — manual review"}},
		{"STR_TO_DATE(s, '%d/%m/%Y')", []string{"STR_TO_DATE: format codes differ from PG to_char/to_date — manual review"}},
		{"DATE_ADD(d, INTERVAL '1-2' YEAR_MONTH)", []string{
			"DATE_ADD: second argument is not an interval PG can express (day count or compound unit) — manual review",
			"INTERVAL … YEAR_MONTH: compound/unsupported interval unit has no direct PG form — manual review",
		}},
		{"ADDDATE(d, 3)", []string{"ADDDATE: second argument is not an interval PG can express (day count or compound unit) — manual review"}},
		{"d + INTERVAL 'abc' DAY", []string{"INTERVAL … DAY: non-numeric interval quantity has no direct PG form — manual review"}},
		{"GROUP_CONCAT(DISTINCT x ORDER BY x)", []string{"GROUP_CONCAT(DISTINCT … ORDER BY …): PG string_agg rejects ORDER BY keys that are not the DISTINCT argument — manual review"}},
		{"GROUP_CONCAT(DISTINCT x)", nil},
		{"GROUP_CONCAT(x ORDER BY x)", nil},
		{"CONVERT(x, CHAR)", []string{"CONVERT: MySQL CONVERT(expr, type) / CONVERT(expr USING charset) has no direct PG form — manual review"}},
		{"JSON_UNQUOTE(JSON_EXTRACT(m, '$.a')) = GROUP_CONCAT(x)", nil},
		{"DATE_ADD(d, INTERVAL 1 DAY)", nil},
		// Duplicates are reported once.
		{"CONCAT(DATE_FORMAT(a, '%Y'), DATE_FORMAT(b, '%Y'))", []string{"DATE_FORMAT: format codes differ from PG to_char/to_date — manual review"}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			e, errs := mysqldialect.ParseExpr(c.in)
			if len(errs) > 0 {
				t.Fatalf("ParseExpr: %v", errs)
			}
			got := mysqlResidualIdioms(rewriteMySQLExpr(e))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	if got := mysqlResidualIdioms(nil); got != nil {
		t.Errorf("nil node: got %q", got)
	}
}

func TestCollectTableRefs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{
			// v_complex from test/fixtures/mysql_sample.sql.
			`SELECT c.id AS customer_id, c.email, COUNT(o.id) AS n_orders,
			        GROUP_CONCAT(o.status SEPARATOR ',') AS statuses
			   FROM customers c
			   LEFT JOIN orders o ON o.customer_id = c.id
			  WHERE c.id IN (SELECT customer_id FROM orders WHERE total > 0)
			  GROUP BY c.id, c.email`,
			[]string{"customers", "orders"},
		},
		{
			"WITH recent AS (SELECT * FROM orders) SELECT * FROM recent JOIN customers c ON c.id = recent.customer_id " +
				"WHERE EXISTS (SELECT 1 FROM refunds r WHERE r.order_id = recent.id)",
			[]string{"orders", "customers", "refunds"},
		},
		{
			"SELECT (SELECT MAX(x) FROM s.t2) AS m FROM (SELECT * FROM t1) d",
			[]string{"t2", "t1"},
		},
		{"SELECT 1", nil},
	}
	for _, c := range cases {
		sel, errs := mysqldialect.ParseSelect(c.in)
		if len(errs) > 0 {
			t.Fatalf("ParseSelect(%q): %v", c.in, errs)
		}
		if got := collectTableRefs(sel); !reflect.DeepEqual(got, c.want) {
			t.Errorf("collectTableRefs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := collectTableRefs(nil); got != nil {
		t.Errorf("nil select: got %q", got)
	}
}
