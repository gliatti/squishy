package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Run bug (mariadb-sysver-history sample, sv_orders): `paid tinyint(1)
// NOT NULL DEFAULT 0` became `"paid" BOOLEAN NOT NULL DEFAULT 0` and
// create_ddl failed with `column "paid" is of type boolean but default
// expression is of type integer` (SQLSTATE 42804). Defaults of columns
// mapped to BOOLEAN follow MySQL's truth rule.
func TestMySQLBooleanColumnDefault(t *testing.T) {
	res := translateMariaDBSysver(t, "CREATE TABLE `d` (\n  `id` int NOT NULL,\n"+
		"  `a` tinyint(1) NOT NULL DEFAULT 0,\n"+
		"  `b` tinyint(1) NOT NULL DEFAULT 1,\n"+
		"  `c` tinyint(1) DEFAULT NULL,\n"+
		"  `e` tinyint(1) DEFAULT '2',\n"+
		"  `f` bit(1) NOT NULL DEFAULT b'0',\n"+
		"  `g` bit(1) NOT NULL DEFAULT b'1',\n"+
		"  `h` tinyint(4) NOT NULL DEFAULT 0,\n"+
		"  `i` tinyint(1) NOT NULL DEFAULT (`id` > 5),\n"+
		"  PRIMARY KEY (`id`)\n) ENGINE=InnoDB;\n", "")
	require.Empty(t, res.Warnings)
	tbl := planTable(t, res, "d")
	require.Equal(t, "FALSE", planColumn(t, tbl, "a").Default)
	require.Equal(t, "TRUE", planColumn(t, tbl, "b").Default)
	require.Equal(t, "NULL", planColumn(t, tbl, "c").Default)
	require.Equal(t, "TRUE", planColumn(t, tbl, "e").Default)
	require.Equal(t, "FALSE", planColumn(t, tbl, "f").Default)
	require.Equal(t, "TRUE", planColumn(t, tbl, "g").Default)
	// Not a BOOLEAN column: untouched.
	require.Equal(t, "0", planColumn(t, tbl, "h").Default)
	require.Equal(t, `CAST(CAST(("id" > 5) AS integer) AS boolean)`, planColumn(t, tbl, "i").Default)
}

func TestMySQLBoolDefaultNil(t *testing.T) {
	require.Nil(t, mysqlBoolDefault(nil))
}
