package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Probe the full tableOption alternatives list from MySqlParser.g4. The
// parser must accept each shape without erroring; the values themselves
// land in the Extras catch-all when we don't model them.
func TestParseAllTableOptions(t *testing.T) {
	cases := []string{
		"CREATE TABLE t (id INT) ENGINE=InnoDB;",
		"CREATE TABLE t (id INT) AUTO_INCREMENT=42;",
		"CREATE TABLE t (id INT) AVG_ROW_LENGTH=200;",
		"CREATE TABLE t (id INT) DEFAULT CHARSET=utf8mb4;",
		"CREATE TABLE t (id INT) DEFAULT CHARACTER SET = utf8mb4;",
		"CREATE TABLE t (id INT) CHECKSUM=1;",
		"CREATE TABLE t (id INT) DEFAULT COLLATE=utf8mb4_general_ci;",
		"CREATE TABLE t (id INT) COMMENT='hello';",
		"CREATE TABLE t (id INT) COMPRESSION='zlib';",
		"CREATE TABLE t (id INT) CONNECTION='server/orders';",
		"CREATE TABLE t (id INT) DATA DIRECTORY='/tmp/data';",
		"CREATE TABLE t (id INT) INDEX DIRECTORY='/tmp/idx';",
		"CREATE TABLE t (id INT) DELAY_KEY_WRITE=1;",
		"CREATE TABLE t (id INT) ENCRYPTION='Y';",
		"CREATE TABLE t (id INT) INSERT_METHOD=NO;",
		"CREATE TABLE t (id INT) KEY_BLOCK_SIZE=8;",
		"CREATE TABLE t (id INT) MAX_ROWS=1000000;",
		"CREATE TABLE t (id INT) MIN_ROWS=1;",
		"CREATE TABLE t (id INT) PACK_KEYS=DEFAULT;",
		"CREATE TABLE t (id INT) PASSWORD='shh';",
		"CREATE TABLE t (id INT) ROW_FORMAT=DYNAMIC;",
		"CREATE TABLE t (id INT) STATS_AUTO_RECALC=1;",
		"CREATE TABLE t (id INT) STATS_PERSISTENT=DEFAULT;",
		"CREATE TABLE t (id INT) STATS_SAMPLE_PAGES=20;",
		"CREATE TABLE t (id INT) TABLESPACE ts1;",
		"CREATE TABLE t (id INT) TABLESPACE ts1 STORAGE DISK;",
		"CREATE TABLE t (id INT) UNION=(t1, t2);",
		"CREATE TABLE t (id INT) SECONDARY_ENGINE=RAPID;",
	}
	for _, src := range cases {
		stmts, errs := Parse(src)
		require.Empty(t, errs, "src=%q errs=%v", src, errs)
		require.Len(t, stmts, 1, "src=%q", src)
		_, ok := stmts[0].(*ast.CreateTable)
		require.True(t, ok, "src=%q produced %T", src, stmts[0])
	}
}

// A real-world cocktail of options must round-trip: the standard mysqldump
// ENGINE/CHARSET/COLLATE/COMMENT prefix plus a STATS_* trailer.
func TestParseRealisticMixedOptions(t *testing.T) {
	src := `CREATE TABLE orders (id INT) ENGINE=InnoDB AUTO_INCREMENT=12345
		DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
		ROW_FORMAT=COMPRESSED KEY_BLOCK_SIZE=8
		STATS_PERSISTENT=1 STATS_AUTO_RECALC=DEFAULT
		COMMENT='order ledger';`
	stmts, errs := Parse(src)
	require.Empty(t, errs, "errors: %v", errs)
	ct := stmts[0].(*ast.CreateTable)
	require.Equal(t, "InnoDB", ct.Options.Engine)
	require.Equal(t, "utf8mb4", ct.Options.Charset)
	require.Equal(t, "utf8mb4_unicode_ci", ct.Options.Collate)
	require.Equal(t, int64(12345), ct.Options.AutoIncrement)
	require.Equal(t, "order ledger", ct.Options.Comment)
}
