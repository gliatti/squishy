package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// Pre-existing gap: the MariaDB column attribute `WITHOUT SYSTEM
// VERSIONING` failed with "expected ')' got WITHOUT". Source: SHOW CREATE
// TABLE on MariaDB 11.8.
func TestParseColumnWithoutSystemVersioning(t *testing.T) {
	src := "CREATE TABLE `t_wo` (\n  `id` int(11) NOT NULL,\n  `a` int(11) DEFAULT NULL,\n  `b` int(11) DEFAULT NULL WITHOUT SYSTEM VERSIONING,\n  `rs` timestamp(6) GENERATED ALWAYS AS ROW START,\n  `re` timestamp(6) GENERATED ALWAYS AS ROW END,\n  PRIMARY KEY (`id`,`re`),\n  PERIOD FOR SYSTEM_TIME (`rs`, `re`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci WITH SYSTEM VERSIONING;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.True(t, ct.SystemVersioned)
	require.Len(t, ct.Columns, 5)
	for _, c := range ct.Columns {
		require.Equal(t, c.Name == "b", c.WithoutSystemVersioning, c.Name)
		require.False(t, c.WithSystemVersioning, c.Name)
	}
	b := ct.Columns[2]
	require.True(t, b.HasDefault)
	require.False(t, b.NotNull)
}

// Column-level `WITH SYSTEM VERSIONING` without the table-level clause
// versions the table for the WITH columns only (MariaDB 11.8 SHOW CREATE
// TABLE renders `CREATE TABLE t (a int, b int WITH SYSTEM VERSIONING)`
// as `a … WITHOUT SYSTEM VERSIONING, b …` + table-level WITH SYSTEM
// VERSIONING). Review finding: the parser only folded the clause into
// SystemVersioned, leaving hand-written DDL with every column versioned
// at the AST level. It now normalises it like SHOW CREATE TABLE: the
// other columns are flagged WITHOUT, the ROW START / ROW END ones are
// left alone.
func TestParseColumnWithSystemVersioningFlagsTable(t *testing.T) {
	stmts, errs := Parse("CREATE TABLE t (id INT PRIMARY KEY, a INT WITH SYSTEM VERSIONING, b INT);")
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.True(t, ct.SystemVersioned)
	require.Len(t, ct.Columns, 3)
	for _, c := range ct.Columns {
		require.Equal(t, c.Name == "a", c.WithSystemVersioning, c.Name)
		require.Equal(t, c.Name != "a", c.WithoutSystemVersioning, c.Name)
	}

	stmts, errs = Parse("CREATE TABLE p (id INT, a INT, b INT WITH SYSTEM VERSIONING, s TIMESTAMP(6) GENERATED ALWAYS AS ROW START, e TIMESTAMP(6) GENERATED ALWAYS AS ROW END, PERIOD FOR SYSTEM_TIME (s, e));")
	require.Empty(t, errs)
	ct = stmts[0].(*ast.CreateTable)
	require.True(t, ct.SystemVersioned)
	for _, c := range ct.Columns {
		require.Equal(t, c.Name == "id" || c.Name == "a", c.WithoutSystemVersioning, c.Name)
	}

	// With the table-level clause, a column-level WITH changes nothing:
	// no column is flagged WITHOUT (MariaDB 11.8 prints none).
	stmts, errs = Parse("CREATE TABLE c (id INT, a INT, b INT WITH SYSTEM VERSIONING) WITH SYSTEM VERSIONING;")
	require.Empty(t, errs)
	ct = stmts[0].(*ast.CreateTable)
	require.True(t, ct.SystemVersioned)
	for _, c := range ct.Columns {
		require.False(t, c.WithoutSystemVersioning, c.Name)
	}
}
