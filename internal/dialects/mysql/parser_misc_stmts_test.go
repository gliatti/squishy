package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestTruncateTable(t *testing.T) {
	src := "TRUNCATE TABLE orders;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	tr, ok := stmts[0].(*ast.TruncateTable)
	require.True(t, ok)
	require.Equal(t, "orders", tr.Table.Name)
}

func TestTruncateWithoutTableKeyword(t *testing.T) {
	src := "TRUNCATE orders;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	_, ok := stmts[0].(*ast.TruncateTable)
	require.True(t, ok)
}

func TestRenameTableMulti(t *testing.T) {
	src := "RENAME TABLE a TO b, c TO d, e TO f;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	rt, ok := stmts[0].(*ast.RenameTable)
	require.True(t, ok)
	require.Len(t, rt.Pairs, 3)
	require.Equal(t, "a", rt.Pairs[0].From.Name)
	require.Equal(t, "b", rt.Pairs[0].To.Name)
}

func TestLoadDataInfileNoop(t *testing.T) {
	// LOAD DATA INFILE has no PG equivalent — must consume cleanly.
	src := "LOAD DATA INFILE '/tmp/data.csv' INTO TABLE t FIELDS TERMINATED BY ',';"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	require.Len(t, stmts, 1)
	_, ok := stmts[0].(*ast.NoopStmt)
	require.True(t, ok)
}

func TestLoadXmlNoop(t *testing.T) {
	src := "LOAD XML INFILE '/tmp/data.xml' INTO TABLE t;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	_, ok := stmts[0].(*ast.NoopStmt)
	require.True(t, ok)
}

func TestCreateNonTableNoopVariants(t *testing.T) {
	cases := []string{
		"CREATE LOGFILE GROUP lg1 ADD UNDOFILE 'undo01.dat' ENGINE=NDB;",
		"CREATE TABLESPACE ts1 ADD DATAFILE 'ts1.dbf' ENGINE=InnoDB;",
		"CREATE SERVER s FOREIGN DATA WRAPPER mysql OPTIONS (HOST 'h');",
		"CREATE ROLE 'admin';",
		"CREATE USER 'u'@'h' IDENTIFIED BY 'p';",
		"CREATE DATABASE app CHARACTER SET utf8mb4;",
		"CREATE SCHEMA app;",
	}
	for _, src := range cases {
		stmts, errs := Parse(src)
		require.Empty(t, errs, "src=%q errs=%v", src, errs)
		require.Len(t, stmts, 1, "src=%q", src)
		_, ok := stmts[0].(*ast.NoopStmt)
		require.True(t, ok, "src=%q produced %T", src, stmts[0])
	}
}

func TestAlterNonTableNoopVariants(t *testing.T) {
	cases := []string{
		"ALTER USER 'u'@'h' IDENTIFIED BY 'newp';",
		"ALTER DATABASE app CHARACTER SET utf8mb4;",
		"ALTER SCHEMA app DEFAULT COLLATE utf8mb4_general_ci;",
		"ALTER SERVER s OPTIONS (HOST 'h2');",
	}
	for _, src := range cases {
		stmts, errs := Parse(src)
		require.Empty(t, errs, "src=%q errs=%v", src, errs)
		require.Len(t, stmts, 1, "src=%q", src)
		_, ok := stmts[0].(*ast.NoopStmt)
		require.True(t, ok, "src=%q produced %T", src, stmts[0])
	}
}

func TestAdminAndReplicationCommandsNoop(t *testing.T) {
	cases := []string{
		"GRANT ALL ON *.* TO 'u'@'h';",
		"REVOKE ALL ON *.* FROM 'u'@'h';",
		"FLUSH PRIVILEGES;",
		"START SLAVE;",
		"STOP SLAVE;",
		"CHANGE MASTER TO MASTER_HOST='h';",
		"XA START 'xid';",
		"PREPARE stmt FROM 'SELECT 1';",
		"EXECUTE stmt;",
		"DEALLOCATE PREPARE stmt;",
		"LOCK TABLES t WRITE;",
		"UNLOCK TABLES;",
		"ANALYZE TABLE t;",
		"OPTIMIZE TABLE t;",
		"REPAIR TABLE t;",
		"CHECKSUM TABLE t;",
		"RESET QUERY CACHE;",
		"PURGE BINARY LOGS BEFORE NOW();",
	}
	for _, src := range cases {
		stmts, errs := Parse(src)
		require.Empty(t, errs, "src=%q errs=%v", src, errs)
		require.Len(t, stmts, 1, "src=%q", src)
		_, ok := stmts[0].(*ast.NoopStmt)
		require.True(t, ok, "src=%q produced %T", src, stmts[0])
	}
}
