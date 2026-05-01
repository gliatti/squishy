package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestParsePartitionByHashSimple(t *testing.T) {
	src := `CREATE TABLE t (id INT NOT NULL, payload VARCHAR(40))
		PARTITION BY HASH(id) PARTITIONS 4;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.Equal(t, "HASH", ct.Partitioning.Method)
	require.Equal(t, "id", ct.Partitioning.ExprText)
	require.Equal(t, 4, ct.Partitioning.Count)
	require.Empty(t, ct.Partitioning.Definitions)
}

func TestParsePartitionByLinearKey(t *testing.T) {
	src := `CREATE TABLE t (id INT, k INT) PARTITION BY LINEAR KEY ALGORITHM=2 (id, k) PARTITIONS 8;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.Equal(t, "KEY", ct.Partitioning.Method)
	require.True(t, ct.Partitioning.Linear)
	require.Equal(t, 2, ct.Partitioning.KeyAlgorithm)
	require.Equal(t, []string{"id", "k"}, ct.Partitioning.Columns)
	require.Equal(t, 8, ct.Partitioning.Count)
}

func TestParsePartitionByRangeWithDefs(t *testing.T) {
	src := `CREATE TABLE sales (id INT, ts DATETIME)
		PARTITION BY RANGE (YEAR(ts)) (
		  PARTITION p2018 VALUES LESS THAN (2019),
		  PARTITION p2019 VALUES LESS THAN (2020),
		  PARTITION pmax  VALUES LESS THAN MAXVALUE
		);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.Equal(t, "RANGE", ct.Partitioning.Method)
	require.Equal(t, "YEAR(ts)", ct.Partitioning.ExprText)
	require.Len(t, ct.Partitioning.Definitions, 3)
	require.Equal(t, "p2018", ct.Partitioning.Definitions[0].Name)
	require.True(t, ct.Partitioning.Definitions[0].HasLessThan)
	require.Equal(t, []string{"2019"}, ct.Partitioning.Definitions[0].LessThan)
	require.Equal(t, []string{"MAXVALUE"}, ct.Partitioning.Definitions[2].LessThan)
}

func TestParsePartitionByRangeColumnsMulti(t *testing.T) {
	src := `CREATE TABLE t (a INT, b INT)
		PARTITION BY RANGE COLUMNS(a, b) (
		  PARTITION p1 VALUES LESS THAN (10, 100),
		  PARTITION p2 VALUES LESS THAN (MAXVALUE, MAXVALUE)
		);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.Equal(t, "RANGE COLUMNS", ct.Partitioning.Method)
	require.Equal(t, []string{"a", "b"}, ct.Partitioning.Columns)
	require.Len(t, ct.Partitioning.Definitions, 2)
	require.Equal(t, []string{"10", "100"}, ct.Partitioning.Definitions[0].LessThan)
	require.Equal(t, []string{"MAXVALUE", "MAXVALUE"}, ct.Partitioning.Definitions[1].LessThan)
}

func TestParsePartitionByListSingleCol(t *testing.T) {
	src := `CREATE TABLE t (region VARCHAR(2))
		PARTITION BY LIST COLUMNS(region) (
		  PARTITION p_eu VALUES IN ('FR','DE','IT'),
		  PARTITION p_as VALUES IN ('JP','CN')
		);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.Equal(t, "LIST COLUMNS", ct.Partitioning.Method)
	require.Len(t, ct.Partitioning.Definitions, 2)
	require.Equal(t, []string{"'FR'", "'DE'", "'IT'"}, ct.Partitioning.Definitions[0].InAtoms)
}

func TestParsePartitionByListColumnsVector(t *testing.T) {
	src := `CREATE TABLE t (a INT, b INT)
		PARTITION BY LIST COLUMNS(a, b) (
		  PARTITION p1 VALUES IN ((1,10),(1,20)),
		  PARTITION p2 VALUES IN ((2,10),(2,20))
		);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.Len(t, ct.Partitioning.Definitions, 2)
	require.Len(t, ct.Partitioning.Definitions[0].InVectors, 2)
	require.Equal(t, []string{"1", "10"}, ct.Partitioning.Definitions[0].InVectors[0])
	require.Equal(t, []string{"2", "20"}, ct.Partitioning.Definitions[1].InVectors[1])
}

func TestParsePartitionByHashWithSubpartition(t *testing.T) {
	src := `CREATE TABLE t (id INT, ts DATETIME)
		PARTITION BY RANGE (YEAR(ts))
		SUBPARTITION BY HASH(id) SUBPARTITIONS 2 (
		  PARTITION p1 VALUES LESS THAN (2020) (SUBPARTITION s1, SUBPARTITION s2),
		  PARTITION p2 VALUES LESS THAN (2021) (SUBPARTITION s3, SUBPARTITION s4)
		);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.NotNil(t, ct.Partitioning.Subpartition)
	require.Equal(t, "HASH", ct.Partitioning.Subpartition.Method)
	require.Equal(t, 2, ct.Partitioning.Subpartition.Count)
	require.Len(t, ct.Partitioning.Definitions[0].Subpartitions, 2)
	require.Equal(t, "s1", ct.Partitioning.Definitions[0].Subpartitions[0].Name)
}

func TestParsePartitionDefinitionsWithEngineOption(t *testing.T) {
	// MySQL emits ENGINE=InnoDB on each partition definition; we must
	// silently consume it without breaking parsing.
	src := `CREATE TABLE t (id INT)
		PARTITION BY RANGE(id) (
		  PARTITION p1 VALUES LESS THAN (10) ENGINE=InnoDB,
		  PARTITION p2 VALUES LESS THAN (20) ENGINE=InnoDB
		);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.NotNil(t, ct.Partitioning)
	require.Len(t, ct.Partitioning.Definitions, 2)
}

func TestParsePartitionAfterTableOptions(t *testing.T) {
	// A real mysqldump will mix ENGINE/CHARSET/COMMENT options before the
	// PARTITION BY clause; the option loop must yield to the partition
	// parser instead of swallowing PARTITION as an unknown K=V pair.
	src := `CREATE TABLE t (id INT)
		ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='hi'
		PARTITION BY HASH(id) PARTITIONS 4;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.Equal(t, "InnoDB", ct.Options.Engine)
	require.Equal(t, "utf8mb4", ct.Options.Charset)
	require.NotNil(t, ct.Partitioning)
	require.Equal(t, "HASH", ct.Partitioning.Method)
	require.Equal(t, 4, ct.Partitioning.Count)
}
