package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// MariaDB history partitioning (`PARTITION BY SYSTEM_TIME`), in the
// forms SHOW CREATE TABLE renders on MariaDB 11.8. It used to fail with
// "unsupported partition function (got SYSTEM_TIME)".

func parseOneCreateTable(t *testing.T, src string) *ast.CreateTable {
	t.Helper()
	stmts, errs := Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	require.Len(t, stmts, 1)
	ct, ok := stmts[0].(*ast.CreateTable)
	require.True(t, ok, "got %T", stmts[0])
	return ct
}

func TestPartitionBySystemTimeLimitWithDefinitions(t *testing.T) {
	ct := parseOneCreateTable(t, "CREATE TABLE `sv_events` (\n  `id` int(11) NOT NULL,\n  `kind` varchar(20) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING\n PARTITION BY SYSTEM_TIME LIMIT 400\n(PARTITION `p_hist0` HISTORY ENGINE = InnoDB,\n PARTITION `p_hist1` HISTORY ENGINE = InnoDB,\n PARTITION `p_cur` CURRENT ENGINE = InnoDB)")
	require.True(t, ct.SystemVersioned)
	pn := ct.Partitioning
	require.NotNil(t, pn)
	require.Equal(t, "SYSTEM_TIME", pn.Method)
	require.Equal(t, int64(400), pn.SystemTimeLimit)
	require.Nil(t, pn.SystemTimeInterval)
	require.False(t, pn.SystemTimeAuto)
	require.Len(t, pn.Definitions, 3)
	require.Equal(t, "p_hist0", pn.Definitions[0].Name)
	require.True(t, pn.Definitions[0].History)
	require.False(t, pn.Definitions[0].Current)
	require.True(t, pn.Definitions[1].History)
	require.Equal(t, "p_cur", pn.Definitions[2].Name)
	require.True(t, pn.Definitions[2].Current)
	require.False(t, pn.Definitions[2].History)
}

func TestPartitionBySystemTimeIntervalStartsAuto(t *testing.T) {
	ct := parseOneCreateTable(t, "CREATE TABLE `t3` (\n  `id` int(11) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING\n PARTITION BY SYSTEM_TIME INTERVAL 1 HOUR STARTS TIMESTAMP'2026-01-01 00:00:00' AUTO\nPARTITIONS 3")
	pn := ct.Partitioning
	require.NotNil(t, pn)
	require.Equal(t, "SYSTEM_TIME", pn.Method)
	require.Zero(t, pn.SystemTimeLimit)
	require.NotNil(t, pn.SystemTimeInterval)
	require.Equal(t, "1", pn.SystemTimeInterval.Value)
	require.Equal(t, "HOUR", pn.SystemTimeInterval.Unit)
	cast, ok := pn.SystemTimeStarts.(*ast.CastExpr)
	require.True(t, ok, "STARTS TIMESTAMP'…' is a typed literal, got %T", pn.SystemTimeStarts)
	require.IsType(t, &ast.TimestampType{}, cast.Type)
	lit, ok := cast.Expr.(*ast.Literal)
	require.True(t, ok)
	require.Equal(t, "string", lit.Kind)
	require.Equal(t, "2026-01-01 00:00:00", lit.Text)
	require.True(t, pn.SystemTimeAuto)
	require.Equal(t, 3, pn.Count)
	require.Empty(t, pn.Definitions)
}

func TestPartitionBySystemTimeBare(t *testing.T) {
	ct := parseOneCreateTable(t, "CREATE TABLE `t4` (\n  `id` int(11) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB WITH SYSTEM VERSIONING\n PARTITION BY SYSTEM_TIME \nPARTITIONS 2")
	pn := ct.Partitioning
	require.NotNil(t, pn)
	require.Equal(t, "SYSTEM_TIME", pn.Method)
	require.Zero(t, pn.SystemTimeLimit)
	require.Nil(t, pn.SystemTimeInterval)
	require.Equal(t, 2, pn.Count)
}

// LIMIT AUTO and a non-reserved `history` / `current` column name.
func TestPartitionBySystemTimeLimitAutoKeepsWordsUsableAsColumns(t *testing.T) {
	ct := parseOneCreateTable(t, "CREATE TABLE `t5` (\n  `history` int(11) DEFAULT NULL,\n  `current` int(11) DEFAULT NULL\n) ENGINE=InnoDB WITH SYSTEM VERSIONING\n PARTITION BY SYSTEM_TIME LIMIT 1000 AUTO\nPARTITIONS 3")
	require.Len(t, ct.Columns, 2)
	require.Equal(t, "history", ct.Columns[0].Name)
	require.Equal(t, "current", ct.Columns[1].Name)
	pn := ct.Partitioning
	require.Equal(t, int64(1000), pn.SystemTimeLimit)
	require.True(t, pn.SystemTimeAuto)
	require.Equal(t, 3, pn.Count)
}

// SYSTEM_TIME is not a SUBPARTITION BY method.
func TestSubpartitionBySystemTimeRejected(t *testing.T) {
	_, errs := Parse("CREATE TABLE t (id INT) PARTITION BY RANGE (id) SUBPARTITION BY SYSTEM_TIME (PARTITION p0 VALUES LESS THAN (10))")
	require.NotEmpty(t, errs)
}
