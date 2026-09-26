package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// mysql_employees_view_test.go — regression coverage for the two views of
// the canonical MySQL `employees` sample (dumps/mysql/employees.sql), in the
// exact form MySQL 8.4 hands back through SHOW CREATE VIEW (ALGORITHM /
// DEFINER / SQL SECURITY clauses, fully backticked + self-aliased columns,
// the redundant `on(((...)))` parenthesization of the join predicate). The
// end-to-end run of `/squishy-migrate mysql employees` must stay at zero
// warnings, so both views have to go through parse → RewriteMySQLAST →
// pgast writer without a view.parse / view.functions fallback.

func TestMySQLView_EmployeesDeptEmpLatestDate(t *testing.T) {
	res := translateMySQL(t, "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `dept_emp_latest_date` AS select `dept_emp`.`emp_no` AS `emp_no`,max(`dept_emp`.`from_date`) AS `from_date`,max(`dept_emp`.`to_date`) AS `to_date` from `dept_emp` group by `dept_emp`.`emp_no`;")
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)

	require.Contains(t, v.DDL, `CREATE OR REPLACE VIEW "mig"."dept_emp_latest_date" AS`)
	require.Contains(t, v.DDL, `MAX("dept_emp"."from_date") AS "from_date"`)
	require.Contains(t, v.DDL, `MAX("dept_emp"."to_date") AS "to_date"`)
	require.Contains(t, v.DDL, `FROM "dept_emp"`)
	require.Contains(t, v.DDL, `GROUP BY "dept_emp"."emp_no"`)
	require.NotContains(t, v.DDL, "`")
	require.NotContains(t, v.DDL, "ALGORITHM")
	require.NotContains(t, v.DDL, "DEFINER=")
	require.Equal(t, "DEFINER", v.Security)
	require.Equal(t, []string{"dept_emp"}, v.References)
	require.Empty(t, res.Warnings, "employees views must translate warning-free")
}

func TestMySQLView_EmployeesCurrentDeptEmp(t *testing.T) {
	res := translateMySQL(t, "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `current_dept_emp` AS select `l`.`emp_no` AS `emp_no`,`d`.`dept_no` AS `dept_no`,`l`.`from_date` AS `from_date`,`l`.`to_date` AS `to_date` from (`dept_emp` `d` join `dept_emp_latest_date` `l` on(((`d`.`emp_no` = `l`.`emp_no`) and (`d`.`from_date` = `l`.`from_date`) and (`l`.`to_date` = `d`.`to_date`))));")
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)

	require.Contains(t, v.DDL, `CREATE OR REPLACE VIEW "mig"."current_dept_emp" AS`)
	require.Contains(t, v.DDL, `"l"."emp_no" AS "emp_no", "d"."dept_no" AS "dept_no"`)
	// MySQL's bare JOIN is an inner join; the parenthesized join tree
	// must be flattened into a valid PG FROM clause.
	require.Contains(t, v.DDL, `FROM "dept_emp" "d" INNER JOIN "dept_emp_latest_date" "l" ON`)
	require.Contains(t, v.DDL, `("d"."emp_no" = "l"."emp_no") AND ("d"."from_date" = "l"."from_date") AND ("l"."to_date" = "d"."to_date")`)
	require.NotContains(t, v.DDL, "`")
	// The dependency on the sibling view drives the create_routine DAG
	// ordering (dept_emp_latest_date must be created first).
	require.ElementsMatch(t, []string{"dept_emp", "dept_emp_latest_date"}, v.References)
	require.Empty(t, res.Warnings, "employees views must translate warning-free")
}
