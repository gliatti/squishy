package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// mariadb_sysver_names_test.go — reviewer round on the names the MariaDB
// SYSTEM VERSIONING emulation derives: the history table shares
// PostgreSQL's relation namespace with views, sequences and indexes, the
// trigger functions share the function namespace with the migrated
// routines, and the triggers share the per-table trigger namespace with
// the migrated triggers. Each derived name must avoid every one of them.

const mariadbSysverEmp = "CREATE TABLE `emp` (\n  `id` int(11) NOT NULL,\n  `a` int(11) DEFAULT NULL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n"

// planViewNames lists the view names of the plan.
func planViewNames(res *Result) []string {
	var out []string
	for _, v := range res.Plan.Views {
		out = append(out, v.Name)
	}
	return out
}

// Reviewer bug: a source view named <t>_history — the usual MariaDB
// idiom over `FOR SYSTEM_TIME ALL` — got the same name as the emulated
// history table, and the migration failed at create_routine.
func TestMariaDBSysver_HistoryNameAvoidsView(t *testing.T) {
	res := translateMariaDBSysver(t, mariadbSysverEmp+
		"CREATE VIEW `emp_history` AS select `emp`.`id` AS `id`,`emp`.`a` AS `a` from `emp`;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	sv := planTable(t, res, "emp").SystemVersioning
	require.NotNil(t, sv)
	require.Equal(t, "emp_history2", sv.HistoryTable)
	require.Equal(t, []string{"emp_history"}, planViewNames(res))
	planTable(t, res, "emp_history2")
	for _, tbl := range res.Plan.Tables {
		require.NotEqual(t, "emp_history", tbl.Name)
	}
	require.Contains(t, res.DDLScript, `CREATE TABLE "mig"."emp_history2"`)
	require.Contains(t, res.DDLPostCopy, `INSERT INTO "mig"."emp_history2"`)
	var noted bool
	for _, e := range res.Explanations {
		if e.Object == "emp" && e.Target == `history table "emp_history2"` {
			noted = true
		}
	}
	require.True(t, noted, "the renamed history table is explained")
}

// Reviewer bug: CREATE SEQUENCE emp_history (a PreActions statement)
// and the history table got the same relation name; create_ddl failed
// with "relation already exists".
func TestMariaDBSysver_HistoryNameAvoidsSequence(t *testing.T) {
	res := translateMariaDBSysver(t,
		"CREATE SEQUENCE `emp_history` start with 1 minvalue 1 maxvalue 9223372036854775806 increment by 1 cache 1000 nocycle ENGINE=InnoDB;\n"+
			mariadbSysverEmp, mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	sv := planTable(t, res, "emp").SystemVersioning
	require.NotNil(t, sv)
	require.Equal(t, "emp_history2", sv.HistoryTable)
	require.Contains(t, res.Plan.PreActions[0], `CREATE SEQUENCE "mig"."emp_history"`)
	require.Contains(t, res.DDLScript, `CREATE TABLE "mig"."emp_history2"`)
}

// The sequence may come after the table in the dump: the names are
// collected once the whole dump is translated.
func TestMariaDBSysver_HistoryNameAvoidsLaterSequenceAndIndex(t *testing.T) {
	res := translateMariaDBSysver(t, mariadbSysverEmp+
		"CREATE TABLE `other` (\n  `id` int(11) NOT NULL,\n  PRIMARY KEY (`id`),\n  KEY `emp_history2` (`id`)\n) ENGINE=InnoDB;\n"+
		"CREATE SEQUENCE `emp_history` start with 1 minvalue 1 maxvalue 9223372036854775806 increment by 1 cache 1000 nocycle ENGINE=InnoDB;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	sv := planTable(t, res, "emp").SystemVersioning
	require.NotNil(t, sv)
	// emp_history is the sequence, emp_history2 the index of `other`.
	require.Equal(t, "emp_history3", sv.HistoryTable)
}

// Same class of bug in the other namespaces: a migrated trigger named
// emp_sysver_stamp on emp creates the trigger function
// emp_sysver_stamp_fn, and a migrated function may carry the name of the
// history trigger function. The emulation used to reuse those names —
// CREATE OR REPLACE FUNCTION silently replaced the migrated routine, and
// CREATE TRIGGER failed on the duplicate trigger name.
func TestMariaDBSysver_TriggerNamesAvoidMigratedRoutines(t *testing.T) {
	res := translateMariaDBSysver(t, mariadbSysverEmp+
		"CREATE TRIGGER `emp_sysver_stamp` BEFORE INSERT ON `emp` FOR EACH ROW SET NEW.a = 1;\n"+
		"CREATE FUNCTION `emp_sysver_history_fn`() RETURNS int(11) DETERMINISTIC RETURN 1;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	var userTrigger, userFn *PGRoutine
	for i := range res.Plan.Routines {
		r := &res.Plan.Routines[i]
		switch r.Name {
		case "emp_sysver_stamp":
			userTrigger = r
		case "emp_sysver_history_fn":
			userFn = r
		}
	}
	require.NotNil(t, userTrigger)
	require.Equal(t, "emp", userTrigger.Table)
	require.Equal(t, "emp_sysver_stamp_fn", userTrigger.FnName)
	require.NotNil(t, userFn)

	post := res.DDLPostCopy
	require.Contains(t, post, `CREATE OR REPLACE FUNCTION "mig"."emp_sysver_stamp_fn2"() RETURNS trigger`)
	require.Contains(t, post, `CREATE OR REPLACE FUNCTION "mig"."emp_sysver_history_fn2"() RETURNS trigger`)
	require.Contains(t, post, `CREATE TRIGGER "emp_sysver_stamp2" BEFORE INSERT OR UPDATE ON "mig"."emp"`)
	require.Contains(t, post, `CREATE TRIGGER "emp_sysver_history" AFTER UPDATE OR DELETE ON "mig"."emp"`)
	require.Contains(t, post, `EXECUTE FUNCTION "mig"."emp_sysver_stamp_fn2"()`)
	require.Contains(t, post, `EXECUTE FUNCTION "mig"."emp_sysver_history_fn2"()`)
	require.NotContains(t, post, `FUNCTION "mig"."emp_sysver_stamp_fn"()`)
	require.NotContains(t, post, `FUNCTION "mig"."emp_sysver_history_fn"()`)
}

// Two system-versioned tables whose derived names meet still get
// distinct names (each claimed name joins the taken set).
func TestClaimIdentNumbersUntilFree(t *testing.T) {
	taken := map[string]bool{"t_history": true, "t_history2": true}
	require.Equal(t, "t_history3", claimIdent("t", "_history", taken))
	require.True(t, taken["t_history3"])
	require.Equal(t, "t_history4", claimIdent("t", "_history", taken))
	require.Equal(t, "u_history", claimIdent("u", "_history", taken))
}
