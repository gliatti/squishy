package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// mariadb_employees_test.go — regression coverage for the canonical
// `employees` sample (dumps/mariadb/employees.sql) in the exact form
// MariaDB 11.8 hands back through SHOW CREATE TABLE / SHOW CREATE VIEW.
// It differs from the MySQL 8.4 shape covered by
// mysql_employees_view_test.go: display widths on INT (`int(11)`), a
// table-level COLLATE=utf8mb4_unicode_ci, and a join predicate printed as
// `on(a = b and c = d ...)` without MySQL's per-comparison parentheses.
// `/squishy-migrate mariadb employees` must stay at zero warnings.

func translateMariaDB(t *testing.T, src string) *Result {
	t.Helper()
	stmts, errs := mysqldialect.Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.NotEmpty(t, stmts)
	return Translate(stmts, Options{
		SourceKind:   dialects.KindMariaDB,
		TargetSchema: "mig",
	})
}

func TestMariaDBEmployees_CurrentDeptEmpView(t *testing.T) {
	res := translateMariaDB(t, "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `current_dept_emp` AS select `l`.`emp_no` AS `emp_no`,`d`.`dept_no` AS `dept_no`,`l`.`from_date` AS `from_date`,`l`.`to_date` AS `to_date` from (`dept_emp` `d` join `dept_emp_latest_date` `l` on(`d`.`emp_no` = `l`.`emp_no` and `d`.`from_date` = `l`.`from_date` and `l`.`to_date` = `d`.`to_date`));")
	v := onlyView(t, res)
	t.Logf("DDL:\n%s", v.DDL)

	require.Contains(t, v.DDL, `CREATE OR REPLACE VIEW "mig"."current_dept_emp" AS`)
	require.Contains(t, v.DDL, `FROM "dept_emp" "d" INNER JOIN "dept_emp_latest_date" "l" ON`)
	// The three comparisons form one flat conjunction and must
	// stay that way (no regrouping, no lost operand).
	require.Contains(t, v.DDL, `ON ("d"."emp_no" = "l"."emp_no" AND "d"."from_date" = "l"."from_date" AND "l"."to_date" = "d"."to_date")`)
	require.NotContains(t, v.DDL, "`")
	require.ElementsMatch(t, []string{"dept_emp", "dept_emp_latest_date"}, v.References)
	require.Empty(t, res.Warnings, "MariaDB employees views must translate warning-free")
}

func TestMariaDBEmployees_DeptEmpLatestDateView(t *testing.T) {
	res := translateMariaDB(t, "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `dept_emp_latest_date` AS select `dept_emp`.`emp_no` AS `emp_no`,max(`dept_emp`.`from_date`) AS `from_date`,max(`dept_emp`.`to_date`) AS `to_date` from `dept_emp` group by `dept_emp`.`emp_no`;")
	v := onlyView(t, res)
	require.Contains(t, v.DDL, `MAX("dept_emp"."from_date") AS "from_date"`)
	require.Contains(t, v.DDL, `GROUP BY "dept_emp"."emp_no"`)
	require.Equal(t, []string{"dept_emp"}, v.References)
	require.Empty(t, res.Warnings, "MariaDB employees views must translate warning-free")
}

func TestMariaDBEmployees_Tables(t *testing.T) {
	res := translateMariaDB(t, "CREATE TABLE `employees` (\n  `emp_no` int(11) NOT NULL,\n  `birth_date` date NOT NULL,\n  `first_name` varchar(14) NOT NULL,\n  `last_name` varchar(16) NOT NULL,\n  `gender` enum('M','F') NOT NULL,\n  `hire_date` date NOT NULL,\n  PRIMARY KEY (`emp_no`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;\n"+
		"CREATE TABLE `departments` (\n  `dept_no` char(4) NOT NULL,\n  `dept_name` varchar(40) NOT NULL,\n  PRIMARY KEY (`dept_no`),\n  UNIQUE KEY `dept_name` (`dept_name`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;\n"+
		"CREATE TABLE `titles` (\n  `emp_no` int(11) NOT NULL,\n  `title` varchar(50) NOT NULL,\n  `from_date` date NOT NULL,\n  `to_date` date DEFAULT NULL,\n  PRIMARY KEY (`emp_no`,`title`,`from_date`),\n  CONSTRAINT `titles_ibfk_1` FOREIGN KEY (`emp_no`) REFERENCES `employees` (`emp_no`) ON DELETE CASCADE\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;\n")
	require.Empty(t, res.Warnings, "MariaDB employees tables must translate warning-free")
	ddl := res.DDLScript
	t.Logf("DDL:\n%s", ddl)
	// int(11): the display width is dropped, not turned into a typmod.
	require.Contains(t, ddl, `"emp_no" INTEGER NOT NULL`)
	require.Contains(t, ddl, `"gender" TEXT NOT NULL CHECK ("gender" IN ('M','F'))`)
	require.Contains(t, ddl, `"to_date" DATE DEFAULT NULL`)
	require.NotContains(t, ddl, "COLLATE")
	require.Contains(t, res.DDLPostCopy, `CREATE UNIQUE INDEX "dept_name" ON "mig"."departments" ("dept_name")`)
	require.Contains(t, res.DDLPostCopy, `ADD CONSTRAINT "titles_ibfk_1" FOREIGN KEY ("emp_no") REFERENCES "mig"."employees" ("emp_no") ON DELETE CASCADE`)
}
