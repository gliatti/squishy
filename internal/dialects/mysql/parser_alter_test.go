package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestAlterTableAddDropColumn(t *testing.T) {
	src := "ALTER TABLE t ADD COLUMN c INT NOT NULL, DROP COLUMN d;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 2)
	require.Equal(t, "ADD_COLUMN", at.Actions[0].Kind)
	require.Equal(t, "c", at.Actions[0].Column.Name)
	require.Equal(t, "DROP_COLUMN", at.Actions[1].Kind)
	require.Equal(t, "d", at.Actions[1].DropName)
}

func TestAlterTableAddColumnFirstAfter(t *testing.T) {
	src := "ALTER TABLE t ADD COLUMN c INT FIRST, ADD COLUMN d INT AFTER c;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 2)
	require.Equal(t, "c", at.Actions[0].Column.Name)
	require.Equal(t, "d", at.Actions[1].Column.Name)
}

func TestAlterTableModifyColumn(t *testing.T) {
	src := "ALTER TABLE t MODIFY COLUMN c BIGINT NOT NULL;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 1)
	require.Equal(t, "MODIFY_COLUMN", at.Actions[0].Kind)
	require.Equal(t, "c", at.Actions[0].Column.Name)
	require.True(t, at.Actions[0].Column.NotNull)
}

func TestAlterTableChangeColumn(t *testing.T) {
	src := "ALTER TABLE t CHANGE COLUMN old_c new_c INT NOT NULL;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 1)
	require.Equal(t, "CHANGE_COLUMN", at.Actions[0].Kind)
	require.Equal(t, "old_c", at.Actions[0].OldName)
	require.Equal(t, "new_c", at.Actions[0].Column.Name)
}

func TestAlterTableRenameColumn(t *testing.T) {
	src := "ALTER TABLE t RENAME COLUMN old_c TO new_c;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 1)
	require.Equal(t, "RENAME_COLUMN", at.Actions[0].Kind)
	require.Equal(t, "old_c", at.Actions[0].OldName)
	require.Equal(t, "new_c", at.Actions[0].NewName)
}

func TestAlterTableRenameTable(t *testing.T) {
	src := "ALTER TABLE t RENAME TO u;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 1)
	require.Equal(t, "RENAME_TABLE", at.Actions[0].Kind)
	require.Equal(t, "u", at.Actions[0].NewName)
}

func TestAlterTableSetDropDefault(t *testing.T) {
	src := "ALTER TABLE t ALTER COLUMN c SET DEFAULT 7, ALTER d DROP DEFAULT;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 2)
	require.Equal(t, "SET_DEFAULT", at.Actions[0].Kind)
	require.Equal(t, "c", at.Actions[0].DropName)
	require.Equal(t, "7", at.Actions[0].DefaultExpr)
	require.Equal(t, "DROP_DEFAULT", at.Actions[1].Kind)
	require.Equal(t, "d", at.Actions[1].DropName)
}

func TestAlterTableDropPrimaryAndForeignKey(t *testing.T) {
	src := "ALTER TABLE t DROP PRIMARY KEY, DROP FOREIGN KEY fk_x;"
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 2)
	require.Equal(t, "DROP_CONSTRAINT", at.Actions[0].Kind)
	require.Empty(t, at.Actions[0].DropName, "DROP PRIMARY KEY has no name")
	require.Equal(t, "DROP_CONSTRAINT", at.Actions[1].Kind)
	require.Equal(t, "fk_x", at.Actions[1].DropName)
}

func TestAlterTableNoopVariants(t *testing.T) {
	// All of these used to break the parser. They must now consume cleanly
	// and surface as NOOP actions so the translator can flag them.
	src := `ALTER TABLE t
		ALGORITHM=INSTANT,
		LOCK=NONE,
		FORCE,
		WITHOUT VALIDATION,
		DISABLE KEYS,
		ENABLE KEYS,
		CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci,
		DEFAULT CHARACTER SET = utf8mb4,
		ORDER BY id,
		DISCARD TABLESPACE,
		IMPORT TABLESPACE;`
	stmts, errs := Parse(src)
	require.Empty(t, errs, "errors: %v", errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 11)
	for i, a := range at.Actions {
		require.Equal(t, "NOOP", a.Kind, "action %d: %+v", i, a)
		require.NotEmpty(t, a.NoopText)
	}
}

func TestAlterTablePartitionOps(t *testing.T) {
	// The full alter_partition_specification grammar — we don't model it,
	// but the parser must consume each variant cleanly.
	cases := []string{
		"ALTER TABLE t ADD PARTITION (PARTITION p3 VALUES LESS THAN (30));",
		"ALTER TABLE t DROP PARTITION p1;",
		"ALTER TABLE t TRUNCATE PARTITION ALL;",
		"ALTER TABLE t COALESCE PARTITION 2;",
		"ALTER TABLE t REORGANIZE PARTITION p1 INTO (PARTITION p1a VALUES LESS THAN (5), PARTITION p1b VALUES LESS THAN (10));",
		"ALTER TABLE t REMOVE PARTITIONING;",
		"ALTER TABLE t REBUILD PARTITION ALL;",
	}
	for _, src := range cases {
		stmts, errs := Parse(src)
		require.Empty(t, errs, "src=%q errs=%v", src, errs)
		require.Len(t, stmts, 1, "src=%q", src)
		at, ok := stmts[0].(*ast.AlterTable)
		require.True(t, ok, "src=%q", src)
		require.NotEmpty(t, at.Actions)
		require.Equal(t, "NOOP", at.Actions[0].Kind, "src=%q", src)
	}
}

func TestAlterTableMixedRealistic(t *testing.T) {
	// A representative cocktail that mysqldump or schema-diff scripts emit.
	src := `ALTER TABLE orders
		ADD COLUMN tracking_id VARCHAR(64),
		MODIFY COLUMN status VARCHAR(32) NOT NULL,
		ADD INDEX idx_tracking (tracking_id),
		ADD CONSTRAINT fk_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
		ALGORITHM=INPLACE, LOCK=NONE;`
	stmts, errs := Parse(src)
	require.Empty(t, errs, "errors: %v", errs)
	at := stmts[0].(*ast.AlterTable)
	require.Len(t, at.Actions, 6)
	require.Equal(t, "ADD_COLUMN", at.Actions[0].Kind)
	require.Equal(t, "MODIFY_COLUMN", at.Actions[1].Kind)
	require.Equal(t, "NOOP", at.Actions[2].Kind)
	require.Equal(t, "ADD_CONSTRAINT", at.Actions[3].Kind)
	require.Equal(t, "NOOP", at.Actions[4].Kind)
	require.Equal(t, "NOOP", at.Actions[5].Kind)
}
