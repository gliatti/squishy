package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// mariadb_sysver_emulation_test.go — the limits of the MariaDB SYSTEM
// VERSIONING emulation (mariadb_temporal.go): which forms are emulated,
// which keep the warning + blocking-prerequisite path, and how the
// emulation honours the source's ROW END sentinel, column-name case and
// WITHOUT SYSTEM VERSIONING columns. Sources are the exact MariaDB 11.8
// SHOW CREATE TABLE forms.

func translateMariaDBSysver(t *testing.T, src, rowEndMax string) *Result {
	t.Helper()
	stmts, errs := mysqldialect.Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	return Translate(stmts, Options{SourceKind: dialects.KindMariaDB, TargetSchema: "mig",
		MariaDBRowEndMax: rowEndMax})
}

func requireSysverNotEmulated(t *testing.T, res *Result, table string) {
	t.Helper()
	var warned bool
	for _, w := range res.Warnings {
		if w.Kind == "table.system_versioning" && w.Object == table {
			warned = true
		}
	}
	require.True(t, warned, "table.system_versioning warning expected, got %+v", res.Warnings)
	var blocking bool
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && p.Object == table {
			blocking = true
		}
	}
	require.True(t, blocking, "blocking prerequisite expected, got %+v", res.Prerequisites)
	require.Nil(t, planTable(t, res, table).SystemVersioning)
	for _, tbl := range res.Plan.Tables {
		require.NotEqual(t, table+"_history", tbl.Name, "no history table for a non-emulated table")
	}
	require.NotContains(t, res.DDLPostCopy, "sysver")
}

// Reviewer bug: transaction-precise versioning (BIGINT UNSIGNED ROW
// START / ROW END holding transaction ids) was emulated like the
// timestamp form: `"rs" NUMERIC(20,0)` columns stamped with
// statement_timestamp() by the trigger — every later INSERT/UPDATE
// failed with a type error, and the history copy compared transaction
// ids with NOW(6). It must keep the warning + blocking prerequisite.
func TestMariaDBSysver_TransactionPreciseNotEmulated(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `t_trx` (\n  `id` int(11) NOT NULL,\n  `x` int(11) DEFAULT NULL,\n  `rs` bigint(20) unsigned GENERATED ALWAYS AS ROW START,\n  `re` bigint(20) unsigned GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	requireSysverNotEmulated(t, res, "t_trx")
	require.Contains(t, res.Warnings[0].Message, "transaction-precise")
	tbl := planTable(t, res, "t_trx")
	require.Equal(t, []string{"id"}, tbl.PK)
	for _, c := range tbl.Columns {
		require.NotEqual(t, "rs", c.Name)
		require.NotEqual(t, "re", c.Name)
	}
	require.NotContains(t, res.DDLScript, "NUMERIC(20,0)")
	require.NotContains(t, res.DDLPostCopy, "statement_timestamp")
	// The PERIOD note used to claim the columns were "kept as plain
	// columns" while they are dropped.
	var period bool
	for _, e := range res.Explanations {
		if e.Object == "t_trx" && e.Source == "PERIOD FOR SYSTEM_TIME (rs, re)" {
			period = true
			require.Equal(t, "(dropped)", e.Target)
			require.Contains(t, e.Reason, "rs / re are dropped")
			require.NotContains(t, e.Reason, "kept as plain columns")
		}
	}
	require.True(t, period, "PERIOD FOR SYSTEM_TIME explanation expected")
}

// Reviewer bug: the current-row sentinel was hard-coded to MariaDB
// 11.5+'s 2106 value. It now comes from the source (the inspector's
// probe): a pre-11.5 source stamps 2038, like its copied current rows.
func TestMariaDBSysver_SentinelComesFromSource(t *testing.T) {
	res := translateMariaDBSysver(t, mariadbFeaturesXLEmployees, "2038-01-19 03:14:07.999999")
	require.Empty(t, res.Warnings)
	cur := planTable(t, res, "employees")
	require.Equal(t, "2038-01-19 03:14:07.999999", cur.SystemVersioning.RowEndMax)
	require.Contains(t, res.DDLPostCopy, `NEW."valid_to" := CAST('2038-01-19 03:14:07.999999+00' AS timestamptz);`)
	require.NotContains(t, res.DDLPostCopy, "2106")

	res = translateMariaDBSysver(t, mariadbFeaturesXLEmployees, mariadb118RowEndMax)
	require.Contains(t, res.DDLPostCopy, `NEW."valid_to" := CAST('2106-02-07 06:28:15.999999+00' AS timestamptz);`)
}

// Without a probed sentinel the emulation would have to guess the
// current-row ROW END; it reports instead.
func TestMariaDBSysver_UnknownSentinelNotEmulated(t *testing.T) {
	res := translateMariaDBSysver(t, mariadbFeaturesXLEmployees, "")
	requireSysverNotEmulated(t, res, "employees")
	require.Contains(t, res.Warnings[0].Message, "ROW END value for current rows could not be determined")
}

// Reviewer bug: ROW START / ROW END matching was case-sensitive.
// `PRIMARY KEY (id, RE)` over column `re` produced PRIMARY KEY
// ("id","RE") on the current table and ("id","RE","re") on the history
// table, neither of which can be created.
func TestMariaDBSysver_PeriodColumnsMatchedCaseInsensitively(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `p` (\n  `id` int(11) NOT NULL,\n  `email` varchar(80) NOT NULL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`RE`),\n  UNIQUE KEY `uq_email` (`email`,`Re`),\n  PERIOD FOR SYSTEM_TIME (`RS`, `RE`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	cur := planTable(t, res, "p")
	require.NotNil(t, cur.SystemVersioning)
	// The descriptor carries the columns' declared spelling.
	require.Equal(t, "rs", cur.SystemVersioning.RowStart)
	require.Equal(t, "re", cur.SystemVersioning.RowEnd)
	require.Equal(t, []string{"id"}, cur.PK)
	require.True(t, planColumn(t, cur, "rs").NotNull)
	require.True(t, planColumn(t, cur, "re").NotNull)
	require.Equal(t, []string{"id", "re"}, planTable(t, res, "p_history").PK)
	for _, idx := range res.Plan.Indexes {
		if idx.Name == "uq_email" {
			require.Equal(t, []string{"email"}, idx.Columns)
		}
	}
	require.Contains(t, res.DDLPostCopy, `NEW."re" := CAST(`)
	require.Contains(t, res.DDLPostCopy, `VALUES (OLD."id", OLD."email", OLD."rs", NEW."rs");`)
	require.NotContains(t, res.DDLScript, `"RE"`)
	require.NotContains(t, res.DDLPostCopy, `"RE"`)
}

// Reviewer gap: `WITHOUT SYSTEM VERSIONING` columns failed to parse,
// and the versioning triggers would archive every UPDATE. MariaDB 11.8
// rewrites the row in place — no history row, ROW START unchanged —
// when an UPDATE only assigns unversioned columns (checked on the
// sample), which PG's `UPDATE OF <versioned columns>` reproduces.
func TestMariaDBSysver_WithoutSystemVersioningColumns(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `t_wo` (\n  `id` int(11) NOT NULL,\n  `a` int(11) DEFAULT NULL,\n  `b` int(11) DEFAULT NULL WITHOUT SYSTEM VERSIONING,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	cur := planTable(t, res, "t_wo")
	require.NotNil(t, cur.SystemVersioning)
	require.Equal(t, []string{"b"}, cur.SystemVersioning.Unversioned)
	planColumn(t, cur, "b")
	post := res.DDLPostCopy
	require.Contains(t, post, `CREATE TRIGGER "t_wo_sysver_stamp" BEFORE INSERT OR UPDATE OF "id", "a" ON "mig"."t_wo"`)
	require.Contains(t, post, `CREATE TRIGGER "t_wo_sysver_history" AFTER UPDATE OF "id", "a" OR DELETE ON "mig"."t_wo"`)
	// The archived row still carries every column, unversioned ones too.
	require.Contains(t, post, `VALUES (OLD."id", OLD."a", OLD."b", OLD."rs", NEW."rs");`)
	for _, e := range res.Explanations {
		require.NotEqual(t, "warn", e.Level, "unexpected warn explanation on %s: %s", e.Object, e.Reason)
	}
}

// Every non-period column unversioned: an UPDATE never versions, only
// INSERT stamps and DELETE archives.
func TestMariaDBSysver_AllColumnsUnversioned(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `u` (\n  `id` int(11) NOT NULL WITHOUT SYSTEM VERSIONING,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	require.Contains(t, res.DDLPostCopy, `CREATE TRIGGER "u_sysver_stamp" BEFORE INSERT ON "mig"."u"`)
	require.Contains(t, res.DDLPostCopy, `CREATE TRIGGER "u_sysver_history" AFTER DELETE ON "mig"."u"`)
}

// The implicit form with WITHOUT SYSTEM VERSIONING columns (SHOW CREATE
// of `CREATE TABLE t_col (id int primary key, a int WITH SYSTEM
// VERSIONING, b int)`) parses and is emulated: the hidden row_start /
// row_end columns are declared (withImplicitSystemTime) and only `a`
// versions an UPDATE.
func TestMariaDBSysver_ImplicitFormWithUnversionedColumnsEmulated(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `t_col` (\n  `id` int(11) NOT NULL WITHOUT SYSTEM VERSIONING,\n  `a` int(11) DEFAULT NULL,\n  `b` int(11) DEFAULT NULL WITHOUT SYSTEM VERSIONING,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	require.Empty(t, res.Prerequisites)
	cur := planTable(t, res, "t_col")
	require.Len(t, cur.Columns, 5)
	require.NotNil(t, cur.SystemVersioning)
	require.True(t, cur.SystemVersioning.Implicit)
	require.Equal(t, []string{"id", "b"}, cur.SystemVersioning.Unversioned)
	post := res.DDLPostCopy
	require.Contains(t, post, `CREATE TRIGGER "t_col_sysver_stamp" BEFORE INSERT OR UPDATE OF "a" ON "mig"."t_col"`)
	require.Contains(t, post, `CREATE TRIGGER "t_col_sysver_history" AFTER UPDATE OF "a" OR DELETE ON "mig"."t_col"`)
	require.Contains(t, post, `VALUES (OLD."id", OLD."a", OLD."b", OLD."row_start", NEW."row_start");`)
}

// The implicit form — `CREATE TABLE … WITH SYSTEM VERSIONING` without
// period columns, and what `ALTER TABLE … ADD SYSTEM VERSIONING` leaves
// (SHOW CREATE TABLE output of the mariadb-sysver-history sample's
// sv_notes / sv_customers) — used to be migrated without its history.
// MariaDB keeps the versions in hidden row_start / row_end TIMESTAMP(6)
// columns; they become regular PG columns and the table is emulated
// like the explicit form.
func TestMariaDBSysver_ImplicitFormEmulated(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `sv_customers` (\n  `id` int(11) NOT NULL,\n  `name` varchar(80) NOT NULL,\n  `email` varchar(120) DEFAULT NULL,\n  `tier` char(1) NOT NULL DEFAULT 'B',\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	require.Empty(t, res.Prerequisites)
	cur := planTable(t, res, "sv_customers")
	sv := cur.SystemVersioning
	require.NotNil(t, sv)
	require.True(t, sv.Implicit)
	require.Equal(t, "row_start", sv.RowStart)
	require.Equal(t, "row_end", sv.RowEnd)
	require.Equal(t, "sv_customers_history", sv.HistoryTable)
	require.Empty(t, sv.Unversioned)
	require.Equal(t, []string{"id"}, cur.PK)
	names := make([]string, 0, len(cur.Columns))
	for _, c := range cur.Columns {
		names = append(names, c.Name)
	}
	require.Equal(t, []string{"id", "name", "email", "tier", "row_start", "row_end"}, names)
	for _, n := range []string{"row_start", "row_end"} {
		c := planColumn(t, cur, n)
		require.True(t, c.NotNull)
		require.Equal(t, "TIMESTAMPTZ(6)", c.Type)
	}
	// Both copies read the hidden columns by name.
	require.Equal(t, []string{"id", "name", "email", "tier", "row_start", "row_end"}, sv.CopyColumns)
	require.Equal(t, []string{"id", "name", "email", "tier", "row_start", "row_end"}, sv.HistoryCopyColumns)
	hist := planTable(t, res, "sv_customers_history")
	require.Equal(t, []string{"id", "row_end"}, hist.PK)
	post := res.DDLPostCopy
	require.Contains(t, post, `CREATE TRIGGER "sv_customers_sysver_stamp" BEFORE INSERT OR UPDATE ON "mig"."sv_customers"`)
	require.Contains(t, post, `CREATE TRIGGER "sv_customers_sysver_history" AFTER UPDATE OR DELETE ON "mig"."sv_customers"`)
	require.Contains(t, post, `NEW."row_end" := CAST('2106-02-07 06:28:15.999999+00' AS timestamptz);`)
	require.Contains(t, post, `VALUES (OLD."id", OLD."name", OLD."email", OLD."tier", OLD."row_start", NEW."row_start");`)
	var hidden bool
	for _, e := range res.Explanations {
		require.NotEqual(t, "warn", e.Level, "unexpected warn explanation on %s: %s", e.Object, e.Reason)
		if e.Object == "sv_customers" && e.Target == `regular columns "row_start", "row_end" (TIMESTAMPTZ(6) NOT NULL)` {
			hidden = true
		}
	}
	require.True(t, hidden, "the hidden-column materialisation must be explained")
}

// Without a probed ROW END sentinel the implicit form is not emulated,
// and the hidden columns the source never listed are not reported as
// dropped columns.
func TestMariaDBSysver_ImplicitFormUnknownSentinelNotEmulated(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `n` (\n  `id` int(11) NOT NULL,\n  `x` int(11) DEFAULT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n", "")
	requireSysverNotEmulated(t, res, "n")
	require.Contains(t, res.Warnings[0].Message, "ROW END value for current rows could not be determined")
	require.Len(t, planTable(t, res, "n").Columns, 2)
	for _, e := range res.Explanations {
		require.NotEqual(t, "n.row_start", e.Object)
		require.NotEqual(t, "n.row_end", e.Object)
	}
}

// A column already named row_start cannot coexist with MariaDB's
// hidden one (ERROR 1060), so such a statement is not a SHOW CREATE
// TABLE output: its versioning is not guessed, it keeps the warning.
func TestMariaDBSysver_ImplicitFormNameClashNotEmulated(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `c` (\n  `id` int(11) NOT NULL,\n  `row_start` int(11) DEFAULT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	requireSysverNotEmulated(t, res, "c")
	require.Contains(t, res.Warnings[0].Message, "hidden row_start / row_end columns could not be declared")
	require.Len(t, planTable(t, res, "c").Columns, 2)
}

// PARTITION BY SYSTEM_TIME (SHOW CREATE TABLE of the sample's
// sv_events) failed to parse ("unsupported partition function") and
// the table was emitted unpartitioned with a partitioning warning, its
// history lost. History partitions only store the closed versions apart
// from the current rows: the table is emulated, the PG tables are
// unpartitioned, and the note is informational.
func TestMariaDBSysver_PartitionBySystemTime(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `sv_events` (\n  `id` int(11) NOT NULL,\n  `kind` varchar(20) NOT NULL,\n  `payload` varchar(200) DEFAULT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING\n PARTITION BY SYSTEM_TIME LIMIT 400\n(PARTITION `p_hist0` HISTORY ENGINE = InnoDB,\n PARTITION `p_hist1` HISTORY ENGINE = InnoDB,\n PARTITION `p_hist2` HISTORY ENGINE = InnoDB,\n PARTITION `p_cur` CURRENT ENGINE = InnoDB);\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	require.Empty(t, res.Prerequisites)
	cur := planTable(t, res, "sv_events")
	require.Nil(t, cur.Partitioning)
	require.NotNil(t, cur.SystemVersioning)
	require.True(t, cur.SystemVersioning.Implicit)
	require.Nil(t, planTable(t, res, "sv_events_history").Partitioning)
	require.NotContains(t, res.DDLScript, "PARTITION")
	var noted bool
	for _, e := range res.Explanations {
		require.NotEqual(t, "warn", e.Level, "unexpected warn explanation on %s: %s", e.Object, e.Reason)
		if e.Object == "table.sv_events" && e.Source == "PARTITION BY SYSTEM_TIME LIMIT 400 (3 HISTORY partition(s) + CURRENT)" {
			noted = true
			require.Equal(t, "info", e.Level)
		}
	}
	require.True(t, noted, "history partitioning must be explained")

	// Not emulated (unknown sentinel): the history-partition note stays
	// informational, the loss is reported once by the versioning warning.
	res = translateMariaDBSysver(t, "CREATE TABLE `e2` (\n  `id` int(11) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING\n PARTITION BY SYSTEM_TIME INTERVAL 1 HOUR STARTS TIMESTAMP'2026-01-01 00:00:00' AUTO\nPARTITIONS 3;\n", "")
	requireSysverNotEmulated(t, res, "e2")
	require.Len(t, res.Warnings, 1)
	require.Nil(t, planTable(t, res, "e2").Partitioning)
	for _, e := range res.Explanations {
		if e.Object == "table.e2" {
			require.Equal(t, "PARTITION BY SYSTEM_TIME INTERVAL 1 HOUR AUTO (2 HISTORY partition(s) + CURRENT)", e.Source)
			require.Equal(t, "info", e.Level)
		}
	}
}

// Review finding: the parser folds a column-level `WITH SYSTEM
// VERSIONING` into CreateTable.SystemVersioned, and the emulation then
// versioned every column. Without the table-level clause MariaDB
// versions only the WITH columns (checked on MariaDB 11.8: `CREATE
// TABLE c1 (a int, b int WITH SYSTEM VERSIONING)` is shown as `a …
// WITHOUT SYSTEM VERSIONING, b …` + table-level WITH SYSTEM VERSIONING,
// and an UPDATE of a alone writes no history row). Hand-written DDL of
// that form, implicit or with an explicit period, is emulated the same.
func TestMariaDBSysver_ColumnLevelWithVersionsOnlyThoseColumns(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `c1` (\n  `id` int NOT NULL,\n  `a` int DEFAULT NULL,\n  `b` int DEFAULT NULL WITH SYSTEM VERSIONING,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	sv := planTable(t, res, "c1").SystemVersioning
	require.NotNil(t, sv)
	require.True(t, sv.Implicit)
	require.Equal(t, []string{"id", "a"}, sv.Unversioned)
	require.Contains(t, res.DDLPostCopy, `CREATE TRIGGER "c1_sysver_history" AFTER UPDATE OF "b" OR DELETE ON "mig"."c1"`)

	res = translateMariaDBSysver(t, "CREATE TABLE `p1` (\n  `id` int NOT NULL,\n  `a` int DEFAULT NULL,\n  `b` int DEFAULT NULL WITH SYSTEM VERSIONING,\n  `s` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `e` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`e`),\n  PERIOD FOR SYSTEM_TIME (`s`, `e`)\n) ENGINE=InnoDB;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	sv = planTable(t, res, "p1").SystemVersioning
	require.NotNil(t, sv)
	require.False(t, sv.Implicit)
	require.Equal(t, []string{"id", "a"}, sv.Unversioned)

	// With the table-level clause as well, a column-level WITH changes
	// nothing: every column but the WITHOUT ones is versioned (MariaDB
	// 11.8 SHOW CREATE TABLE prints no WITHOUT for a).
	res = translateMariaDBSysver(t, "CREATE TABLE `c2` (\n  `id` int NOT NULL,\n  `a` int DEFAULT NULL,\n  `b` int DEFAULT NULL WITH SYSTEM VERSIONING,\n  `c` int DEFAULT NULL WITHOUT SYSTEM VERSIONING,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	require.Equal(t, []string{"c"}, planTable(t, res, "c2").SystemVersioning.Unversioned)
}
