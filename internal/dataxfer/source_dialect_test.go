package dataxfer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The history variant reads the closed versions of a MariaDB
// system-versioned table; catalog lookups stay the base table's.
func TestMySQLSystemTimeHistorySourceQueries(t *testing.T) {
	d := MySQLSystemTimeHistorySource("valid_to")
	require.Equal(t, "mysql", d.Kind())
	require.Equal(t,
		"SELECT COUNT(*) FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6)",
		d.CountQuery("db", "employees"))
	require.Equal(t,
		"SELECT MIN(`id`), MAX(`id`) FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6)",
		d.MinMaxQuery("db", "employees", "id"))
	require.Equal(t,
		"SELECT `id`,`valid_to` FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6) AND `id` BETWEEN ? AND ? ORDER BY `id`",
		d.SelectRangeQuery("db", "employees", []string{"id", "valid_to"}, "id"))
	require.Equal(t,
		"SELECT `id`,`valid_to` FROM `db`.`employees` FOR SYSTEM_TIME ALL WHERE `valid_to` <= NOW(6) ORDER BY `id`,`valid_to` LIMIT ? OFFSET ?",
		d.SelectOffsetQuery("db", "employees", []string{"id", "valid_to"}))
	require.Equal(t, MySQLSource().PKColumnsQuery(), d.PKColumnsQuery())
	require.Equal(t, MySQLSource().ListColumnsQuery(), d.ListColumnsQuery())
}

// ROW END is left out of the partitioning key. The default column list
// skips every STORED GENERATED column, ROW START / ROW END included:
// reviewer regression — keeping them there made the copier SELECT and
// COPY `rs`/`re` into tables the translator had dropped them from (the
// non-emulated transaction-precise and unknown-sentinel forms), failing
// with `column "rs" does not exist`. Emulated tables pass an explicit
// column list instead (planner / worker copy payload).
func TestMySQLCatalogQueriesSystemVersioning(t *testing.T) {
	d := MySQLSource()
	require.Contains(t, d.ListColumnsQuery(), "EXTRA NOT LIKE '%STORED GENERATED%'")
	require.NotContains(t, d.ListColumnsQuery(), "ROW START")
	require.NotContains(t, d.ListColumnsQuery(), "ROW END")
	require.Contains(t, d.PKColumnsQuery(), "<> 'ROW END'")
}
