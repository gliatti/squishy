package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestEmpDetailsViewTranslation(t *testing.T) {
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
	stmts, perr := mysqldialect.Parse(ddl)
	if len(perr) > 0 {
		t.Logf("parse errors: %v", perr)
	}
	if len(stmts) == 0 {
		t.Fatalf("no statements parsed; errors=%v", perr)
	}
	res := Translate(stmts, Options{
		SourceKind:   dialects.KindMySQL,
		TargetSchema: "mig",
	})
	for _, v := range res.Plan.Views {
		t.Logf("=== view DDL ===\n%s", v.DDL)
		if strings.Contains(v.DDL, "ALGORITHM=") {
			t.Errorf("view DDL still contains MySQL ALGORITHM= clause:\n%s", v.DDL)
		}
		if strings.Contains(strings.ToUpper(v.DDL), "DEFINER=") {
			t.Errorf("view DDL still contains MySQL DEFINER= clause:\n%s", v.DDL)
		}
		if strings.Contains(v.DDL, "`") {
			t.Errorf("view DDL still contains MySQL backticks:\n%s", v.DDL)
		}
	}
	t.Logf("warnings: %+v", res.Warnings)
}
