package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// mariadb_features_xl_test.go — regression coverage for
// dumps/mariadb/mariadb-features-xl.sql in the exact form MariaDB 11.8
// hands back through SHOW CREATE TABLE / SHOW CREATE SEQUENCE (plus the
// `ALTER SEQUENCE … RESTART WITH n` the inspector appends once the
// sequence has been used). `/squishy-migrate mariadb mariadb-features-xl`
// must plan with zero warnings and zero blocking prerequisites.

const mariadbFeaturesXLAuditLog = "CREATE TABLE `audit_log` (\n  `id` bigint(20) NOT NULL AUTO_INCREMENT,\n  `event` varchar(200) NOT NULL,\n  `payload` text DEFAULT NULL,\n  `inserted` timestamp NULL DEFAULT current_timestamp() INVISIBLE,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB AUTO_INCREMENT=100001 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;\n"

const mariadbFeaturesXLContracts = "CREATE TABLE `contracts` (\n  `id` bigint(20) NOT NULL,\n  `employee_id` int(11) NOT NULL,\n  `terms` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`terms`)),\n  `start_date` date NOT NULL,\n  `end_date` date NOT NULL,\n  PERIOD FOR `contract_period` (`start_date`, `end_date`),\n  PRIMARY KEY (`id`),\n  KEY `idx_contracts_emp` (`employee_id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;\n"

const mariadbFeaturesXLEmployees = "CREATE TABLE `employees` (\n  `id` int(11) NOT NULL,\n  `name` varchar(120) NOT NULL,\n  `salary` decimal(10,2) NOT NULL,\n  `hired_at` date NOT NULL,\n  `valid_from` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `valid_to` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`valid_to`),\n  PERIOD FOR SYSTEM_TIME (`valid_from`, `valid_to`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n"

const mariadbFeaturesXLSequence = "CREATE SEQUENCE `order_seq` start with 1 minvalue 1 maxvalue 9223372036854775806 increment by 1 nocache nocycle ENGINE=InnoDB;\n"

// mariadb118RowEndMax is what the inspector probes on the MariaDB 11.8
// (64-bit) sample: the ROW END value of current rows.
const mariadb118RowEndMax = "2106-02-07 06:28:15.999999"

func translateMariaDBFeaturesXL(t *testing.T, src string) *Result {
	t.Helper()
	stmts, errs := mysqldialect.Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	return Translate(stmts, Options{SourceKind: dialects.KindMariaDB, TargetSchema: "mig",
		MariaDBRowEndMax: mariadb118RowEndMax})
}

func planTable(t *testing.T, res *Result, name string) PGTable {
	t.Helper()
	for _, tbl := range res.Plan.Tables {
		if tbl.Name == name {
			return tbl
		}
	}
	t.Fatalf("table %s not in plan", name)
	return PGTable{}
}

func planColumn(t *testing.T, tbl PGTable, name string) PGColumn {
	t.Helper()
	for _, c := range tbl.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("column %s.%s not in plan", tbl.Name, name)
	return PGColumn{}
}

func TestMariaDBFeaturesXL_WholeDumpIsWarningFree(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, mariadbFeaturesXLAuditLog+mariadbFeaturesXLContracts+
		mariadbFeaturesXLEmployees+mariadbFeaturesXLSequence)
	require.Empty(t, res.Warnings)
	for _, p := range res.Prerequisites {
		require.NotEqual(t, SeverityBlocking, p.Severity, "unexpected blocking prerequisite %q", p.Title)
	}
	for _, e := range res.Explanations {
		require.NotEqual(t, "warn", e.Level, "unexpected warn explanation on %s: %s", e.Object, e.Reason)
	}
}

// Bug: the MariaDB JSON idiom (longtext utf8mb4_bin + CHECK(json_valid))
// was promoted to JSONB but kept its collation, emitting
// `"terms" JSONB COLLATE "C"`, which PG rejects ("collations are not
// supported by type jsonb") — the whole create_ddl step failed.
func TestMariaDBFeaturesXL_JSONColumnHasNoCollation(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, mariadbFeaturesXLContracts)
	c := planColumn(t, planTable(t, res, "contracts"), "terms")
	require.Equal(t, "JSONB", c.Type)
	require.Empty(t, c.Collation)
	require.Contains(t, res.DDLScript, `"terms" JSONB NOT NULL`)
	require.NotContains(t, res.DDLScript, "COLLATE")
}

// Bug: an application-time period was reported as a warning + blocking
// prerequisite while its only schema-level effect — the implicit
// CHECK (start < end) MariaDB enforces under the period's name — was
// silently dropped.
func TestMariaDBFeaturesXL_ApplicationPeriodBecomesNamedCheck(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, mariadbFeaturesXLContracts)
	require.Empty(t, res.Warnings)
	tbl := planTable(t, res, "contracts")
	planColumn(t, tbl, "start_date")
	planColumn(t, tbl, "end_date")
	require.Equal(t, []string{"contract_period"}, tbl.CheckNames)
	require.Contains(t, res.DDLScript, `CONSTRAINT "contract_period" CHECK ("start_date" < "end_date")`)
}

// Bug: WITH SYSTEM VERSIONING dropped the ROW START / ROW END columns,
// warned, and required an ack for losing the history. It is now
// emulated: columns kept, history table, versioning triggers.
func TestMariaDBFeaturesXL_SystemVersioningEmulated(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, mariadbFeaturesXLEmployees)
	require.Empty(t, res.Warnings)

	cur := planTable(t, res, "employees")
	require.NotNil(t, cur.SystemVersioning)
	require.Equal(t, "valid_from", cur.SystemVersioning.RowStart)
	require.Equal(t, "valid_to", cur.SystemVersioning.RowEnd)
	require.Equal(t, "employees_history", cur.SystemVersioning.HistoryTable)
	for _, name := range []string{"valid_from", "valid_to"} {
		c := planColumn(t, cur, name)
		require.Equal(t, "TIMESTAMPTZ(6)", c.Type)
		require.True(t, c.NotNull, "%s must be NOT NULL", name)
		require.Nil(t, c.Generated)
	}
	// MariaDB's implicit (id, valid_to) key: the current table keeps one
	// version per id, the history table the full key.
	require.Equal(t, []string{"id"}, cur.PK)

	hist := planTable(t, res, "employees_history")
	require.Nil(t, hist.SystemVersioning)
	require.Equal(t, []string{"id", "valid_to"}, hist.PK)
	require.Len(t, hist.Columns, len(cur.Columns))
	for i, c := range hist.Columns {
		require.Equal(t, cur.Columns[i].Name, c.Name)
		require.Equal(t, cur.Columns[i].Type, c.Type)
		require.False(t, c.Identity)
		require.Empty(t, c.Default)
	}
	require.Contains(t, res.DDLScript, `CREATE TABLE "mig"."employees_history"`)

	// Triggers run post-copy (create_fk script), after the source values
	// of valid_from / valid_to were copied.
	require.NotContains(t, res.DDLScript, "sysver")
	post := res.DDLPostCopy
	require.Contains(t, post, `CREATE OR REPLACE FUNCTION "mig"."employees_sysver_stamp_fn"() RETURNS trigger`)
	require.Contains(t, post, `NEW."valid_from" := statement_timestamp();`)
	require.Contains(t, post, `NEW."valid_to" := CAST('2106-02-07 06:28:15.999999+00' AS timestamptz);`)
	require.Contains(t, post, `CREATE TRIGGER "employees_sysver_stamp" BEFORE INSERT OR UPDATE ON "mig"."employees"`)
	require.Contains(t, post, `CREATE TRIGGER "employees_sysver_history" AFTER UPDATE OR DELETE ON "mig"."employees"`)
	require.Contains(t, post, `IF TG_OP = 'UPDATE' THEN`)
	require.Contains(t, post, `IF OLD."valid_from" < NEW."valid_from" THEN`)
	require.Contains(t, post, `INSERT INTO "mig"."employees_history" ("id", "name", "salary", "hired_at", "valid_from", "valid_to") VALUES (OLD."id", OLD."name", OLD."salary", OLD."hired_at", OLD."valid_from", NEW."valid_from");`)
	require.Contains(t, post, `IF OLD."valid_from" < statement_timestamp() THEN`)
	require.Contains(t, post, `VALUES (OLD."id", OLD."name", OLD."salary", OLD."hired_at", OLD."valid_from", statement_timestamp());`)
	require.Contains(t, post, `RETURN NULL;`)
}

// The history table name must not collide with a source table.
func TestMariaDBFeaturesXL_SystemVersioningHistoryNameCollision(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, mariadbFeaturesXLEmployees+
		"CREATE TABLE `employees_history` (`x` int(11) NOT NULL, PRIMARY KEY (`x`));\n")
	cur := planTable(t, res, "employees")
	require.Equal(t, "employees_history2", cur.SystemVersioning.HistoryTable)
	planTable(t, res, "employees_history2")
}

// Unique keys of a system-versioned table carry ROW END in MariaDB; the
// current table enforces them without it.
func TestMariaDBFeaturesXL_SystemVersioningUniqueKeyDropsRowEnd(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, "CREATE TABLE `p` (\n  `id` int(11) NOT NULL,\n  `email` varchar(80) NOT NULL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  UNIQUE KEY `uq_email` (`email`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n")
	require.Empty(t, res.Warnings)
	var found bool
	for _, idx := range res.Plan.Indexes {
		if idx.Name == "uq_email" {
			found = true
			require.Equal(t, []string{"email"}, idx.Columns)
		}
	}
	require.True(t, found)
}

// Bug: the inspector did not list MariaDB sequences at all (order_seq was
// silently lost). It now emits SHOW CREATE SEQUENCE plus, once the
// sequence has been used, ALTER SEQUENCE … RESTART WITH next value.
func TestMariaDBFeaturesXL_SequenceWithRestart(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, mariadbFeaturesXLSequence+
		"ALTER SEQUENCE `order_seq` RESTART WITH 42;\n")
	require.Empty(t, res.Warnings)
	var create, alter int
	for i, pre := range res.Plan.PreActions {
		if strings.HasPrefix(pre, `CREATE SEQUENCE "mig"."order_seq"`) {
			create = i + 1
			require.Contains(t, pre, "INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775806 START WITH 1 CACHE 1 NO CYCLE")
		}
		if pre == `ALTER SEQUENCE "mig"."order_seq" RESTART WITH 42;` {
			alter = i + 1
		}
	}
	require.NotZero(t, create, "CREATE SEQUENCE missing: %v", res.Plan.PreActions)
	require.NotZero(t, alter, "ALTER SEQUENCE RESTART missing: %v", res.Plan.PreActions)
	require.Less(t, create, alter, "the RESTART must run after the CREATE")
}

// A database-qualified MariaDB ALTER SEQUENCE still targets the PG
// target schema.
func TestMariaDBAlterSequenceQualifiedUsesTargetSchema(t *testing.T) {
	res := translateMariaDBFeaturesXL(t, "ALTER SEQUENCE `srcdb`.`s` INCREMENT BY 5 START WITH 10 RESTART;\n")
	require.Contains(t, res.Plan.PreActions, `ALTER SEQUENCE "mig"."s" INCREMENT BY 5 START WITH 10 RESTART;`)
}
