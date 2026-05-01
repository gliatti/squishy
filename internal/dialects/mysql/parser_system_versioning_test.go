package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestSystemVersionedTableCapturesAll(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		name VARCHAR(50),
		valid_from TIMESTAMP(6) GENERATED ALWAYS AS ROW START,
		valid_to TIMESTAMP(6) GENERATED ALWAYS AS ROW END,
		PERIOD FOR SYSTEM_TIME (valid_from, valid_to)
	) WITH SYSTEM VERSIONING;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.True(t, ct.SystemVersioned, "WITH SYSTEM VERSIONING must flag the table")
	require.Len(t, ct.Periods, 1)
	require.Equal(t, "SYSTEM_TIME", ct.Periods[0].Name)
	require.Equal(t, "valid_from", ct.Periods[0].StartCol)
	require.Equal(t, "valid_to", ct.Periods[0].EndCol)

	var sawStart, sawEnd bool
	for _, c := range ct.Columns {
		if c.SystemVersioning {
			if c.Name == "valid_from" {
				sawStart = true
			}
			if c.Name == "valid_to" {
				sawEnd = true
			}
		}
	}
	require.True(t, sawStart && sawEnd, "ROW START/END columns must be flagged")
}

func TestApplicationTimePeriodCaptured(t *testing.T) {
	src := `CREATE TABLE rooms (
		room_id INT,
		valid_from DATE,
		valid_to DATE,
		PERIOD FOR booking (valid_from, valid_to)
	);`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.False(t, ct.SystemVersioned, "non-SYSTEM_TIME period must not flag the table")
	require.Len(t, ct.Periods, 1)
	require.Equal(t, "booking", ct.Periods[0].Name)
	require.Equal(t, "valid_from", ct.Periods[0].StartCol)
	require.Equal(t, "valid_to", ct.Periods[0].EndCol)
}

func TestWithoutSystemVersioning(t *testing.T) {
	src := `CREATE TABLE t (id INT) WITHOUT SYSTEM VERSIONING;`
	stmts, errs := Parse(src)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	require.False(t, ct.SystemVersioned, "WITHOUT SYSTEM VERSIONING must not set the flag")
}
