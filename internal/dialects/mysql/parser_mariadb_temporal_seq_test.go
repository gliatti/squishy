package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// MariaDB 10.5+ `UNIQUE (key, period WITHOUT OVERLAPS)`: the period key
// part used to stop the key-part loop and fail the whole CREATE TABLE.
func TestParseUniqueWithoutOverlaps(t *testing.T) {
	stmts, errs := Parse(`CREATE TABLE rooms (
		room_id INT,
		s DATE,
		e DATE,
		PERIOD FOR booking (s, e),
		UNIQUE KEY uq_room (room_id, booking WITHOUT OVERLAPS)
	);`)
	require.Empty(t, errs)
	ct := stmts[0].(*ast.CreateTable)
	var uq *ast.UQConstraint
	for _, c := range ct.Constraints {
		if u, ok := c.(*ast.UQConstraint); ok {
			uq = u
		}
	}
	require.NotNil(t, uq)
	require.Len(t, uq.Columns, 2)
	require.Equal(t, "room_id", uq.Columns[0].Name)
	require.False(t, uq.Columns[0].WithoutOverlaps)
	require.Equal(t, "booking", uq.Columns[1].Name)
	require.True(t, uq.Columns[1].WithoutOverlaps)
}

// MariaDB ALTER SEQUENCE used to be swallowed as a no-op; the inspector
// relies on `ALTER SEQUENCE s RESTART WITH n` to carry the position.
func TestParseAlterSequenceMariaDB(t *testing.T) {
	stmts, errs := Parse("ALTER SEQUENCE `db`.`order_seq` RESTART WITH 42;\n" +
		"ALTER SEQUENCE IF EXISTS s INCREMENT BY 5 MINVALUE=2 NO MAXVALUE CACHE 10 CYCLE START WITH 3 RESTART;\n")
	require.Empty(t, errs)
	require.Len(t, stmts, 2)

	a := stmts[0].(*ast.AlterSequence)
	require.Equal(t, "db", a.Schema)
	require.Equal(t, "order_seq", a.Name)
	require.True(t, a.HasRestart)
	require.True(t, a.HasStartWith)
	require.EqualValues(t, 42, a.StartWith)

	b := stmts[1].(*ast.AlterSequence)
	require.Equal(t, "s", b.Name)
	require.True(t, b.HasIncr)
	require.EqualValues(t, 5, b.Increment)
	require.True(t, b.HasMin)
	require.EqualValues(t, 2, b.MinValue)
	require.True(t, b.NoMax)
	require.True(t, b.HasCache)
	require.EqualValues(t, 10, b.Cache)
	require.True(t, b.HasCycle && b.Cycle)
	require.True(t, b.HasStart)
	require.EqualValues(t, 3, b.Start)
	require.True(t, b.HasRestart)
	require.False(t, b.HasStartWith)
}
