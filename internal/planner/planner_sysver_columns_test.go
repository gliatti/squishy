package planner

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// planner_sysver_columns_test.go — the copied column set of a MariaDB
// system-versioned table follows the translated table.
//
// Reviewer regression: the source catalog's default column list had been
// widened to always include ROW START / ROW END, while the translator
// drops them from the tables it does not emulate (transaction-precise
// BIGINT UNSIGNED period columns, unknown ROW END sentinel). The copier
// then COPYed `rs`/`re` into a PG table without them: `column "rs" does
// not exist`, breaking the "ack the prerequisite, migrate the current
// rows" path. The default list is back to skipping every generated
// column; an emulated table carries its own list in the payload.

// Exact MariaDB 11.8 SHOW CREATE TABLE forms (scratch database on the
// sample, since dropped).
const (
	sysverTrxDDL = "CREATE TABLE `t_trx` (\n  `id` int(11) NOT NULL,\n  `x` int(11) DEFAULT NULL,\n  `rs` bigint(20) unsigned GENERATED ALWAYS AS ROW START,\n  `re` bigint(20) unsigned GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n"
	sysverGenDDL = "CREATE TABLE `t_gen` (\n  `id` int(11) NOT NULL,\n  `a` int(11) NOT NULL,\n  `g` int(11) GENERATED ALWAYS AS (`a` * 2) STORED,\n  `v` int(11) GENERATED ALWAYS AS (`a` + 1) VIRTUAL,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;\n"
)

func planSysver(t *testing.T, ddl, rowEndMax string, tables ...string) (*Plan, *translate.Result) {
	t.Helper()
	stmts, errs := mysqldialect.Parse(ddl)
	require.Empty(t, errs)
	res := translate.Translate(stmts, translate.Options{SourceKind: dialects.KindMariaDB,
		TargetSchema: "probe", MariaDBRowEndMax: rowEndMax})
	src := &inspect.SourceSchema{Database: "probe"}
	for _, n := range tables {
		src.Tables = append(src.Tables, inspect.ObjectSnapshot{Name: n, Database: "probe", Rows: 3})
	}
	return Build(src, res, BuildOptions{TargetSchema: "probe"}), res
}

func copyStep(t *testing.T, p *Plan, target string) *Step {
	t.Helper()
	for i := range p.Steps {
		if p.Steps[i].Kind == "copy_table" && p.Steps[i].Target == target {
			return &p.Steps[i]
		}
	}
	t.Fatalf("no copy_table step for %s", target)
	return nil
}

func pgTable(t *testing.T, res *translate.Result, name string) translate.PGTable {
	t.Helper()
	for _, tbl := range res.Plan.Tables {
		if tbl.Name == name {
			return tbl
		}
	}
	t.Fatalf("table %s not in plan", name)
	return translate.PGTable{}
}

// requireColumnsInTable asserts every column of a copy payload exists in
// the PG table the copy writes into.
func requireColumnsInTable(t *testing.T, cols []string, tbl translate.PGTable) {
	t.Helper()
	have := map[string]bool{}
	for _, c := range tbl.Columns {
		have[c.Name] = true
	}
	for _, c := range cols {
		require.True(t, have[c], "copy column %q not in PG table %s", c, tbl.Name)
	}
}

// Non-emulated (transaction-precise, then unknown sentinel): the PG
// table has no rs/re and the copy carries no explicit column list, so
// the copier falls back to the catalog default, which skips every
// generated column — rs/re included.
func TestPlan_SystemVersionedNotEmulatedCopiesCatalogDefault(t *testing.T) {
	for _, tc := range []struct{ ddl, rowEndMax, table string }{
		{sysverTrxDDL, "2106-02-07 06:28:15.999999", "t_trx"},
		{sysverGenDDL, "", "t_gen"},
	} {
		p, res := planSysver(t, tc.ddl, tc.rowEndMax, tc.table)
		tbl := pgTable(t, res, tc.table)
		require.Nil(t, tbl.SystemVersioning)
		for _, c := range tbl.Columns {
			require.NotEqual(t, "rs", c.Name)
			require.NotEqual(t, "re", c.Name)
		}
		cp := copyStep(t, p, tc.table)
		_, has := cp.Payload["columns"]
		require.False(t, has, "non-emulated table must use the catalog default column list")
		for _, s := range p.Steps {
			require.NotEqual(t, "system_time_history", s.Payload["source_variant"])
		}
	}
}

// Emulated: the current-table copy lists the non-generated columns with
// ROW START / ROW END, the history copy every column (generated ones
// included, the history table stores them); each list matches its PG
// table.
func TestPlan_SystemVersionedEmulatedCopyColumns(t *testing.T) {
	p, res := planSysver(t, sysverGenDDL, "2106-02-07 06:28:15.999999", "t_gen")
	cur := pgTable(t, res, "t_gen")
	require.NotNil(t, cur.SystemVersioning)

	cp := copyStep(t, p, "t_gen")
	require.Equal(t, []string{"id", "a", "rs", "re"}, cp.Payload["columns"])
	requireColumnsInTable(t, cp.Payload["columns"].([]string), cur)

	hist := copyStep(t, p, cur.SystemVersioning.HistoryTable)
	require.Equal(t, []string{"id", "a", "g", "v", "rs", "re"}, hist.Payload["columns"])
	requireColumnsInTable(t, hist.Payload["columns"].([]string), pgTable(t, res, cur.SystemVersioning.HistoryTable))
}
