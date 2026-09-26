package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// mysql_dalibo_hr_test.go — regression coverage for the MySQL
// dalibo-docker HR sample (dumps/mysql/dalibo-docker/*.sql), in the form
// MySQL 8.4 hands back through SHOW CREATE. The end-to-end run of
// `/squishy-migrate mysql dalibo-docker` succeeded while leaving
// objects that broke at runtime on PG:
//
//   - secure_dml() called TIME_FORMAT / DAYNAME, which PG does not have,
//     so every INSERT/UPDATE/DELETE on employees (whose triggers CALL
//     secure_dml) failed with "function time_format(…) does not exist";
//   - the BEFORE DELETE trigger function ended with RETURN NEW, i.e.
//     NULL in a DELETE trigger, which makes PG silently skip the delete;
//   - AUTO_INCREMENT identity columns were never restarted after the
//     copy (only a comment was emitted), so the next generated key
//     collided with a copied row;
//   - named CHECK constraints (emp_salary_min, jhist_date_interval) lost
//     their names.

func TestMySQLVisitTimeFormat(t *testing.T) {
	runMySQLExprCases(t, []struct{ in, want string }{
		{"TIME_FORMAT(CURRENT_TIME, '%H:%i')",
			`to_char(CAST(CAST(CURRENT_TIME AS time) AS interval), 'HH24:MI')`},
		{"time_format(t, '%h:%i:%s %p')",
			`to_char(CAST(CAST("t" AS time) AS interval), 'HH12:MI:SS AM')`},
		{"TIME_FORMAT(t, '%k.%f')",
			`to_char(CAST(CAST("t" AS time) AS interval), 'FMHH24.US')`},
		{"TIME_FORMAT(t, '%r')",
			`to_char(CAST(CAST("t" AS time) AS interval), 'HH12:MI:SS AM')`},
		// Literal text holding letters is double-quoted so PG does not
		// read it as a pattern; %% is a literal percent sign.
		{"TIME_FORMAT(t, '%Hh%i 100%%')",
			`to_char(CAST(CAST("t" AS time) AS interval), 'HH24"h"MI 100%')`},
		{"DAYNAME(CURRENT_DATE)", `to_char(CAST(CURRENT_DATE AS date), 'FMDay')`},
		{"MONTHNAME(d)", `to_char(CAST("d" AS date), 'FMMonth')`},
		// Date specifiers are not TIME_FORMAT specifiers and a
		// non-literal format cannot be converted: left untouched.
		{"TIME_FORMAT(t, '%Y')", `TIME_FORMAT("t", '%Y')`},
		{"TIME_FORMAT(t, f)", `TIME_FORMAT("t", "f")`},
	})
}

func TestMySQLTimeFormatToPG(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"%H:%i", "HH24:MI", true},
		{"%T", "HH24:MI:SS", true},
		{"%l%p", "FMHH12AM", true},
		{"at %H", `"at "HH24`, true},
		{`say "x"`, `"say \"x\""`, true},
		{"%%", "%", true},
		{"", "", true},
		{"%", "", false},
		{"%H %d", "", false},
		{"%Y", "", false},
	}
	for _, c := range cases {
		got, ok := mysqlTimeFormatToPG(c.in)
		require.Equal(t, c.ok, ok, "format %q", c.in)
		if ok {
			require.Equal(t, c.want, got, "format %q", c.in)
		}
	}
}

// mustParseMySQLExpr parses one MySQL expression or fails the test.
func mustParseMySQLExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, errs := mysqldialect.ParseExpr(src)
	require.Empty(t, errs, "ParseExpr(%q)", src)
	return e
}

func TestMySQLResidualIdioms_TimeFormat(t *testing.T) {
	for _, src := range []string{"TIME_FORMAT(t, '%Y')", "TIME_FORMAT(t, f)"} {
		e := mustParseMySQLExpr(t, src)
		require.Equal(t,
			[]string{"TIME_FORMAT: format is not a literal of hour/minute/second specifiers — manual review"},
			mysqlResidualIdioms(rewriteMySQLExpr(e)), src)
	}
	e := mustParseMySQLExpr(t, "TIME_FORMAT(t, '%H:%i')")
	require.Empty(t, mysqlResidualIdioms(rewriteMySQLExpr(e)))
}

func TestMySQLDaliboHR_SecureDML(t *testing.T) {
	res := translateMySQL(t, "CREATE DEFINER=`hr`@`%` PROCEDURE `secure_dml`()\n"+
		"BEGIN\n"+
		"  IF TIME_FORMAT(CURRENT_TIME, '%H:%i') NOT BETWEEN '08:00' AND '18:00'\n"+
		"        OR DAYNAME(CURRENT_DATE) IN ('Saturday', 'Sunday') THEN\n"+
		"    SIGNAL SQLSTATE '45000'\n"+
		"    SET MESSAGE_TEXT = 'You may only make changes during normal office hours';\n"+
		"  END IF;\n"+
		"END;")
	require.Empty(t, res.Warnings)
	require.Len(t, res.Plan.Routines, 1)
	ddl := res.Plan.Routines[0].DDL
	t.Logf("DDL:\n%s", ddl)
	require.Contains(t, ddl,
		`IF to_char(CAST(CAST(CURRENT_TIME AS time) AS interval), 'HH24:MI') NOT BETWEEN '08:00' AND '18:00' OR to_char(CAST(CURRENT_DATE AS date), 'FMDay') IN ('Saturday', 'Sunday') THEN`)
	require.Contains(t, ddl, `RAISE EXCEPTION 'You may only make changes during normal office hours' USING ERRCODE = '45000';`)
	require.NotContains(t, strings.ToUpper(ddl), "TIME_FORMAT")
	require.NotContains(t, strings.ToUpper(ddl), "DAYNAME")
}

// triggerFunctionDDL returns the DDL of the PG function backing trigger
// name (<name>_fn), wherever the translator placed it.
func triggerFunctionDDL(t *testing.T, res *Result, name string) string {
	t.Helper()
	for _, r := range res.Plan.Routines {
		if strings.Contains(r.DDL, name+"_fn") {
			return r.DDL
		}
	}
	t.Fatalf("no routine for trigger %s in %+v", name, res.Plan.Routines)
	return ""
}

// daliboHREmployeesTable is the minimal employees table the trigger tests
// attach to (a trigger on a table absent from the plan is skipped).
const daliboHREmployeesTable = "CREATE TABLE `employees` (`employee_id` int NOT NULL, PRIMARY KEY (`employee_id`));\n"

func TestMySQLDaliboHR_BeforeDeleteTriggerReturnsOld(t *testing.T) {
	res := translateMySQL(t, daliboHREmployeesTable+"CREATE DEFINER=`root`@`localhost` TRIGGER `secure_del_employees` BEFORE DELETE ON `employees` FOR EACH ROW BEGIN\n  CALL secure_dml();\nEND;")
	require.Empty(t, res.Warnings)
	ddl := triggerFunctionDDL(t, res, "secure_del_employees")
	t.Logf("DDL:\n%s", ddl)
	require.Contains(t, ddl, "RETURN OLD;")
	require.NotContains(t, ddl, "RETURN NEW;")

	for _, ev := range []string{"INSERT", "UPDATE"} {
		name := "secure_" + strings.ToLower(ev)
		res := translateMySQL(t, daliboHREmployeesTable+"CREATE TRIGGER `"+name+"` BEFORE "+ev+" ON `employees` FOR EACH ROW BEGIN\n  CALL secure_dml();\nEND;")
		ddl := triggerFunctionDDL(t, res, name)
		require.Contains(t, ddl, "RETURN NEW;", ev)
		require.NotContains(t, ddl, "RETURN OLD;", ev)
	}

	res = translateMySQL(t, daliboHREmployeesTable+"CREATE TRIGGER `trg_ad` AFTER DELETE ON `employees` FOR EACH ROW BEGIN\n  CALL secure_dml();\nEND;")
	require.Contains(t, triggerFunctionDDL(t, res, "trg_ad"), "RETURN OLD;")
}

func TestTriggerReturnStmt(t *testing.T) {
	require.Equal(t, "RETURN OLD;", triggerReturnStmt(mysqlTriggerReturnRow([]string{"DELETE"})))
	require.Equal(t, "RETURN OLD;", triggerReturnStmt(mysqlTriggerReturnRow([]string{"delete"})))
	require.Equal(t, "RETURN NEW;", triggerReturnStmt(mysqlTriggerReturnRow([]string{"INSERT"})))
	require.Equal(t, "RETURN NEW;", triggerReturnStmt(mysqlTriggerReturnRow([]string{"UPDATE"})))
}

const daliboHREmployeesDDL = "CREATE TABLE `employees` (\n" +
	"  `employee_id` int unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `last_name` varchar(25) COLLATE utf8mb4_unicode_ci NOT NULL,\n" +
	"  `salary` decimal(8,2) DEFAULT NULL,\n" +
	"  PRIMARY KEY (`employee_id`),\n" +
	"  CONSTRAINT `emp_salary_min` CHECK ((`salary` > 0))\n" +
	") ENGINE=InnoDB AUTO_INCREMENT=208 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;"

func TestMySQLDaliboHR_IdentityRestartAfterCopy(t *testing.T) {
	res := translateMySQL(t, daliboHREmployeesDDL)
	require.Empty(t, res.Warnings)
	want := `SELECT setval(pg_get_serial_sequence('"mig"."employees"', 'employee_id'), GREATEST(208, COALESCE((SELECT max("employee_id")::bigint FROM "mig"."employees"), 0) + 1), false);`
	require.Contains(t, res.Plan.PostActions, want)
	require.Contains(t, res.DDLPostCopy, want)
	for _, a := range res.Plan.PostActions {
		require.NotContains(t, a, "restart identity to match", "the comment-only placeholder must be gone")
	}

	// No AUTO_INCREMENT table option (empty source table): the restart
	// still follows the copied data, floor 1.
	res = translateMySQL(t, "CREATE TABLE `T_Mixed` (`Id` int NOT NULL AUTO_INCREMENT, PRIMARY KEY (`Id`));")
	require.Contains(t, res.Plan.PostActions,
		`SELECT setval(pg_get_serial_sequence('"mig"."T_Mixed"', 'Id'), GREATEST(1, COALESCE((SELECT max("Id")::bigint FROM "mig"."T_Mixed"), 0) + 1), false);`)

	// No AUTO_INCREMENT column: nothing to restart.
	res = translateMySQL(t, "CREATE TABLE `jobs` (`job_id` varchar(10) NOT NULL, PRIMARY KEY (`job_id`));")
	for _, a := range res.Plan.PostActions {
		require.NotContains(t, a, "setval")
	}
}

func TestMySQLDaliboHR_NamedCheckConstraints(t *testing.T) {
	res := translateMySQL(t, daliboHREmployeesDDL+"\n"+
		"CREATE TABLE `job_history` (\n"+
		"  `employee_id` int unsigned NOT NULL,\n"+
		"  `start_date` date NOT NULL,\n"+
		"  `end_date` date NOT NULL,\n"+
		"  PRIMARY KEY (`employee_id`,`start_date`),\n"+
		"  CONSTRAINT `jhist_date_interval` CHECK ((`end_date` > `start_date`)),\n"+
		"  CHECK ((`end_date` < '2100-01-01'))\n"+
		") ENGINE=InnoDB;")
	t.Logf("DDL:\n%s", res.DDLScript)
	require.Contains(t, res.DDLScript, `  CONSTRAINT "emp_salary_min" CHECK (("salary" > 0))`)
	require.Contains(t, res.DDLScript, `  CONSTRAINT "jhist_date_interval" CHECK (("end_date" > "start_date")),`)
	// An unnamed CHECK stays unnamed (PG generates the name).
	require.Contains(t, res.DDLScript, "\n  CHECK ((\"end_date\" < '2100-01-01'))\n")
	require.Equal(t, []string{"emp_salary_min"}, res.Plan.Tables[0].CheckNames)
}

// update_job_history (AFTER UPDATE on employees) calls add_job_history,
// whose parameters are INT while employees.employee_id is INT UNSIGNED
// (→ BIGINT on PG). MySQL converts the argument to the parameter type;
// PG resolves CALL by argument types and failed with "procedure
// add_job_history(bigint, date, date, character varying, integer) does
// not exist" on every UPDATE changing job_id. Arguments passed to integer
// parameters of routines defined in the dump are cast explicitly.
func TestMySQLDaliboHR_CallArgsCastToIntegerParams(t *testing.T) {
	res := translateMySQL(t, "CREATE TABLE `employees` (\n"+
		"  `employee_id` int unsigned NOT NULL AUTO_INCREMENT,\n"+
		"  `hire_date` date NOT NULL,\n"+
		"  `job_id` varchar(10) NOT NULL,\n"+
		"  `department_id` smallint unsigned DEFAULT NULL,\n"+
		"  PRIMARY KEY (`employee_id`)\n"+
		") ENGINE=InnoDB;\n"+
		"CREATE DEFINER=`root`@`localhost` TRIGGER `update_job_history` AFTER UPDATE ON `employees` FOR EACH ROW BEGIN\n"+
		"  IF OLD.job_id <> NEW.job_id OR OLD.department_id <> NEW.department_id THEN\n"+
		"    CALL add_job_history(OLD.employee_id, OLD.hire_date, CURRENT_DATE, OLD.job_id, OLD.department_id);\n"+
		"  END IF;\n"+
		"END;\n"+
		"CREATE DEFINER=`hr`@`%` PROCEDURE `add_job_history`(\n"+
		"  p_emp_id INT,\n"+
		"  p_start_date DATE,\n"+
		"  p_end_date DATE,\n"+
		"  p_job_id VARCHAR(10),\n"+
		"  p_department_id INT\n"+
		")\n"+
		"BEGIN\n"+
		"  INSERT INTO job_history (employee_id, start_date, end_date, job_id, department_id)\n"+
		"  VALUES (p_emp_id, p_start_date, p_end_date, p_job_id, p_department_id);\n"+
		"END;\n"+
		"CREATE DEFINER=`hr`@`%` FUNCTION `last_first_name`(empid INT) RETURNS varchar(255)\n"+
		"BEGIN\n  RETURN CONCAT('Employee: ', empid);\nEND;\n"+
		"CREATE DEFINER=`hr`@`%` PROCEDURE `caller`(IN e BIGINT, OUT n INT)\n"+
		"BEGIN\n"+
		"  DECLARE s VARCHAR(255);\n"+
		"  SELECT last_first_name(e) INTO s;\n"+
		"  SET s = last_first_name(7);\n"+
		"  CALL add_job_history(e, CURRENT_DATE, CURRENT_DATE, 'X', NULL);\n"+
		"  CALL raise_salary(e, 10, n);\n"+
		"END;\n"+
		"CREATE DEFINER=`hr`@`%` PROCEDURE `raise_salary`(IN emp_id INT, IN sal_raise DECIMAL(10,2), OUT new_sal DECIMAL(10,2))\n"+
		"BEGIN\n  SELECT 1 INTO new_sal;\nEND;")
	require.Empty(t, res.Warnings)

	trg := triggerFunctionDDL(t, res, "update_job_history")
	t.Logf("trigger:\n%s", trg)
	require.Contains(t, trg,
		`CALL "add_job_history"(CAST(OLD."employee_id" AS integer), CAST(OLD."hire_date" AS date), CAST(CURRENT_DATE AS date), CAST(OLD."job_id" AS varchar), CAST(OLD."department_id" AS integer));`)

	var caller string
	for _, r := range res.Plan.Routines {
		if r.Name == "caller" {
			caller = r.DDL
		}
	}
	t.Logf("caller:\n%s", caller)
	// Function calls are cast too; a small integer literal already
	// resolves to int4, and a NULL or string literal to any type. Every
	// other argument is cast to its parameter's PG type (MySQL converts
	// arguments to the parameter types; PG only applies implicit casts).
	require.Contains(t, caller, `LAST_FIRST_NAME(CAST("e" AS integer))`)
	require.Contains(t, caller, `LAST_FIRST_NAME(7)`)
	require.Contains(t, caller, `CALL "add_job_history"(CAST("e" AS integer), CAST(CURRENT_DATE AS date), CAST(CURRENT_DATE AS date), 'X', NULL);`)
	// OUT arguments stay assignable variables; an int4 literal passed to
	// a DECIMAL parameter widens implicitly to numeric.
	require.Contains(t, caller, `CALL "raise_salary"(CAST("e" AS integer), 10, "n");`)
}

func TestMySQLCallArgCasts_Unit(t *testing.T) {
	sigs := &mysqlRoutineSigs{
		procs: map[string][]mysqlParamConv{"p": {{Target: "smallint"}, {}, {Target: "bigint"}}},
		funcs: map[string][]mysqlParamConv{"f": {{Target: "integer"}}},
	}
	fn := makeMySQLCallArgCastVisitor(sigs, nil)
	call := &ast.CallStmt{Name: "P", Args: []ast.Expr{
		&ast.Literal{Kind: "number", Text: "5"},
		ast.BuildIdent("x"),
		&ast.CastExpr{Expr: ast.BuildIdent("y"), Type: &ast.PGType{Name: "bigint"}},
	}}
	got := fn(call).(*ast.CallStmt)
	// smallint parameters cast even small literals (PG types them int4).
	require.IsType(t, &ast.CastExpr{}, got.Args[0])
	require.Same(t, call.Args[1], got.Args[1])
	require.Same(t, call.Args[2], got.Args[2], "already cast to the target type")
	_, isLit := call.Args[0].(*ast.Literal)
	require.True(t, isLit, "the original call must not be mutated")

	// Arity mismatch or unknown routine: untouched.
	short := &ast.CallStmt{Name: "p", Args: []ast.Expr{ast.BuildIdent("a")}}
	require.Same(t, short, fn(short))
	other := ast.BuildFuncCall("g", ast.BuildIdent("a"))
	require.Same(t, other, fn(other))
	big := ast.BuildFuncCall("f", &ast.Literal{Kind: "number", Text: "12345678901"})
	require.IsType(t, &ast.CastExpr{}, fn(big).(*ast.FuncCall).Args[0])
}
