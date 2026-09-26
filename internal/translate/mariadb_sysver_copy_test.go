package translate

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
)

// mariadb_sysver_copy_test.go — reviewer round on the MariaDB SYSTEM
// VERSIONING emulation: the copied column sets, generated columns in the
// history table, spatial columns, PostgreSQL's 63-byte identifier limit
// on the derived names, and the ALTER TABLE key path. Sources are the
// exact MariaDB 11.8 SHOW CREATE TABLE forms (scratch database on the
// sample, since dropped).

// mariadbSysverGenerated is a system-versioned table with a STORED and a
// VIRTUAL generated column.
const mariadbSysverGenerated = "CREATE TABLE `t_gen` (\n  `id` int(11) NOT NULL,\n  `a` int(11) NOT NULL,\n  `g` int(11) GENERATED ALWAYS AS (`a` * 2) STORED,\n  `v` int(11) GENERATED ALWAYS AS (`a` + 1) VIRTUAL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n"

const mariadbSysverTrxPrecise = "CREATE TABLE `t_trx` (\n  `id` int(11) NOT NULL,\n  `x` int(11) DEFAULT NULL,\n  `rs` bigint(20) unsigned GENERATED ALWAYS AS ROW START,\n  `re` bigint(20) unsigned GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n"

const mariadbSysverLongName = "a_very_long_system_versioned_table_name_for_truncation_x"

const mariadbSysverLong = "CREATE TABLE `" + mariadbSysverLongName + "` (\n  `id` int(11) NOT NULL,\n  `a` int(11) DEFAULT NULL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n"

// Reviewer bug (silent data loss): the history copy used the source
// catalog's default column list, which skips generated columns, while
// the history table stores them as plain columns — archived versions got
// NULL there. The copied column sets now come from the translated
// table: the current-table copy transfers the non-generated columns
// (period columns included; PG recomputes g and v), the history copy
// every column, generated ones included.
func TestMariaDBSysver_GeneratedColumnsCopiedIntoHistory(t *testing.T) {
	res := translateMariaDBSysver(t, mariadbSysverGenerated, mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	cur := planTable(t, res, "t_gen")
	sv := cur.SystemVersioning
	require.NotNil(t, sv)
	require.NotNil(t, planColumn(t, cur, "g").Generated)
	require.NotNil(t, planColumn(t, cur, "v").Generated)
	require.Equal(t, []string{"id", "a", "rs", "re"}, cur.CopyColumns)
	require.Empty(t, cur.CopiedGenerated)
	require.Equal(t, []string{"id", "a", "g", "v", "rs", "re"}, sv.HistoryCopyColumns)

	// Every copied column exists in the table it is copied into, and
	// the history table stores the generated values as plain columns.
	for _, c := range cur.CopyColumns {
		require.Nil(t, planColumn(t, cur, c).Generated, "current-table copy column %s", c)
	}
	hist := planTable(t, res, sv.HistoryTable)
	require.Len(t, hist.Columns, len(sv.HistoryCopyColumns))
	for _, c := range sv.HistoryCopyColumns {
		require.Nil(t, planColumn(t, hist, c).Generated, "history column %s must be plain storage", c)
	}
	// Rows archived by the trigger carry the generated values too.
	require.Contains(t, res.DDLPostCopy, `VALUES (OLD."id", OLD."a", OLD."g", OLD."v", OLD."rs", NEW."rs");`)
}

// Non-emulated forms have no copy column lists: their copies use the
// catalog default, which skips the period columns the translator
// dropped (reviewer regression: `column "rs" does not exist`).
func TestMariaDBSysver_NotEmulatedHasNoCopyColumns(t *testing.T) {
	res := translateMariaDBSysver(t, mariadbSysverTrxPrecise, mariadb118RowEndMax)
	tbl := planTable(t, res, "t_trx")
	require.Nil(t, tbl.SystemVersioning)
	require.Len(t, tbl.Columns, 2)
	require.Nil(t, tbl.CopyColumns)

	res = translateMariaDBSysver(t, mariadbFeaturesXLEmployees, "")
	require.Nil(t, planTable(t, res, "employees").SystemVersioning)
}

// A spatial column cannot be transferred by the data copier, so the
// archived versions would lose it: the table is not emulated.
func TestMariaDBSysver_SpatialColumnNotEmulated(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `t_geo` (\n  `id` int(11) NOT NULL,\n  `pos` point DEFAULT NULL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n",
		mariadb118RowEndMax)
	requireSysverNotEmulated(t, res, "t_geo")
	var found bool
	for _, w := range res.Warnings {
		if w.Kind == "table.system_versioning" {
			require.Contains(t, w.Message, "spatial column")
			found = true
		}
	}
	require.True(t, found)
}

// Reviewer bug: derived names ignored PostgreSQL's 63-byte identifier
// limit. With a 56-character table name both trigger functions
// (<t>_sysver_stamp_fn, <t>_sysver_history_fn) truncated to the same
// identifier, so the second CREATE OR REPLACE FUNCTION replaced the
// first.
func TestMariaDBSysver_LongTableNameDerivedNamesFit(t *testing.T) {
	name := mariadbSysverLongName
	require.Len(t, name, 56)
	res := translateMariaDBSysver(t, mariadbSysverLong, mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	sv := planTable(t, res, name).SystemVersioning
	require.NotNil(t, sv)
	planTable(t, res, sv.HistoryTable)

	stampFn := pgIdentWithSuffix(name, "_sysver_stamp_fn")
	histFn := pgIdentWithSuffix(name, "_sysver_history_fn")
	stampTg := pgIdentWithSuffix(name, "_sysver_stamp")
	histTg := pgIdentWithSuffix(name, "_sysver_history")
	names := []string{stampFn, histFn, stampTg, histTg, sv.HistoryTable}
	for i, n := range names {
		require.LessOrEqual(t, len(n), pgMaxIdentLen, n)
		for _, m := range names[i+1:] {
			require.NotEqual(t, n, m)
		}
	}
	// The function names used to collide once PG cut them to 63 bytes.
	require.NotEqual(t, pgClipIdent(name+"_sysver_stamp_fn"), stampFn)
	require.Equal(t, pgClipIdent(name+"_sysver_stamp_fn"), pgClipIdent(name+"_sysver_history_fn"))

	post := res.DDLPostCopy
	require.Contains(t, post, `FUNCTION "mig"."`+stampFn+`"()`)
	require.Contains(t, post, `FUNCTION "mig"."`+histFn+`"()`)
	require.Contains(t, post, `CREATE TRIGGER "`+stampTg+`" BEFORE INSERT OR UPDATE ON "mig"."`+name+`"`)
	require.Contains(t, post, `CREATE TRIGGER "`+histTg+`" AFTER UPDATE OR DELETE ON "mig"."`+name+`"`)
	require.Contains(t, post, `EXECUTE FUNCTION "mig"."`+stampFn+`"()`)
	require.Contains(t, post, `EXECUTE FUNCTION "mig"."`+histFn+`"()`)
}

// pgIdentWithSuffix keeps short names as they are, keeps the suffix of
// a long one whole, and tells apart long names that share a prefix.
func TestPGIdentWithSuffix(t *testing.T) {
	require.Equal(t, "emp_history", pgIdentWithSuffix("emp", "_history"))
	long := strings.Repeat("x", 60)
	a := pgIdentWithSuffix(long+"_a", "_history")
	b := pgIdentWithSuffix(long+"_b", "_history")
	require.NotEqual(t, a, b)
	require.Len(t, a, pgMaxIdentLen)
	require.True(t, strings.HasSuffix(a, "_history"))
	// Multi-byte names are cut on a character boundary.
	u := pgIdentWithSuffix(strings.Repeat("é", 40), "_history")
	require.LessOrEqual(t, len(u), pgMaxIdentLen)
	require.True(t, utf8.ValidString(u))
	require.Equal(t, "abc", pgClipIdent("abc"))
	require.Len(t, pgClipIdent(strings.Repeat("y", 64)), pgMaxIdentLen)
	require.True(t, utf8.ValidString(pgClipIdent(strings.Repeat("é", 40))))
}

// The history-name collision check compares the names PostgreSQL
// stores: a 64-character source table whose first 63 bytes equal the
// derived history name forces another name.
func TestMariaDBSysver_HistoryNameCollisionAfterTruncation(t *testing.T) {
	name := mariadbSysverLongName
	hist := pgIdentWithSuffix(name, "_history")
	require.Len(t, hist, pgMaxIdentLen)
	other := hist + "z" // PG stores it as hist
	src := mariadbSysverLong + "CREATE TABLE `" + other + "` (\n  `id` int(11) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n"
	res := translateMariaDBSysver(t, src, mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	sv := planTable(t, res, name).SystemVersioning
	require.NotNil(t, sv)
	require.Equal(t, pgIdentWithSuffix(name, "_history2"), sv.HistoryTable)
	require.NotEqual(t, strings.ToLower(pgClipIdent(other)), strings.ToLower(sv.HistoryTable))
	require.LessOrEqual(t, len(sv.HistoryTable), pgMaxIdentLen)
}

// Reviewer bug: ALTER TABLE ADD PRIMARY KEY / ADD UNIQUE on an emulated
// system-versioned table kept ROW END (only CREATE TABLE stripped it),
// and the history key (pk…, ROW END) then listed it twice. Both paths
// now filter the key parts alike; so does a non-emulated table, whose
// period columns are dropped.
func TestMariaDBSysver_AlterTableKeysStripRowEnd(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `p` (\n  `id` int(11) NOT NULL,\n  `email` varchar(80) NOT NULL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n"+
		"ALTER TABLE `p` ADD PRIMARY KEY (`id`,`RE`);\n"+
		"ALTER TABLE `p` ADD CONSTRAINT `uq_p_email` UNIQUE (`email`,`re`);\n",
		mariadb118RowEndMax)
	require.Empty(t, res.Warnings)
	cur := planTable(t, res, "p")
	require.NotNil(t, cur.SystemVersioning)
	require.Equal(t, []string{"id"}, cur.PK)
	require.Equal(t, []string{"id", "re"}, planTable(t, res, "p_history").PK)
	var uq bool
	for _, idx := range res.Plan.Indexes {
		if idx.Name == "uq_p_email" {
			require.Equal(t, []string{"email"}, idx.Columns)
			uq = true
		}
	}
	require.True(t, uq, "uq_p_email not in plan: %+v", res.Plan.Indexes)

	// Non-emulated (transaction-precise): the dropped columns are left out.
	res = translateMariaDBSysver(t, "CREATE TABLE `q` (\n  `id` int(11) NOT NULL,\n  `rs` bigint(20) unsigned GENERATED ALWAYS AS ROW START,\n  `re` bigint(20) unsigned GENERATED ALWAYS AS ROW END,\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING;\n"+
		"ALTER TABLE `q` ADD PRIMARY KEY (`id`,`re`);\n",
		mariadb118RowEndMax)
	require.Equal(t, []string{"id"}, planTable(t, res, "q").PK)
}

// emitSystemVersioning never lists ROW END twice in the history key,
// even when the current table's PK still carries it.
func TestMariaDBSysver_HistoryPKDedupesRowEnd(t *testing.T) {
	tr := &translator{opt: Options{SourceKind: dialects.KindMariaDB, TargetSchema: "mig"}, res: &Result{}}
	tr.res.Plan.Tables = []PGTable{{
		Schema: "mig", Name: "p", PK: []string{"id", "RE"},
		Columns: []PGColumn{
			{Name: "id", Type: "INTEGER"},
			{Name: "rs", Type: "TIMESTAMPTZ(6)"},
			{Name: "re", Type: "TIMESTAMPTZ(6)"},
		},
		SystemVersioning: &PGSystemVersioning{RowStart: "rs", RowEnd: "re", RowEndMax: mariadb118RowEndMax},
	}}
	tr.emitSystemVersioning()
	require.Equal(t, []string{"id", "re"}, planTable(t, tr.res, "p_history").PK)
}
