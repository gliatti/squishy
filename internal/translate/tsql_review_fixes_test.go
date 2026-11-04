package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Two columns folding to one PG name ([ID] and [id] of a case-sensitive
// database): only the first is created, and the table gets an explicit
// copy list naming the kept source columns — the copier's catalog
// default would select both, which fold to the same PG column ("column
// "id" specified more than once").
func TestTSQLReviewFixes_FoldedColumnDupCopyColumns(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.K ([ID] int NOT NULL, [id] int NULL, Name nvarchar(20) NULL);
GO
`, preserved())
	ws := tsqlWarnings(res, tsqlWarnFoldCollision)
	require.Len(t, ws, 1)
	require.Equal(t, string(SeverityBlocking), ws[0].Severity)
	// The collision gates the run: a blocking prerequisite (the warning
	// alone has no prerequisite mapping in buildPrerequisites).
	p := tsqlPrereq(res, tsqlFoldCollisionTitle)
	require.NotNil(t, p, "the fold collision must be a blocking prerequisite")
	require.Equal(t, SeverityBlocking, p.Severity)
	require.Equal(t, "table.dbo.k.id", p.Object)
	require.NotEmpty(t, p.ID)
	k := tsqlTableOf(res, "dbo", "k")
	require.NotNil(t, k)
	require.Len(t, k.Columns, 2)
	require.Equal(t, []string{"ID", "Name"}, k.CopyColumns)

	// Without a collision the table keeps the catalog default.
	res = tsqlTranslate(t, `
CREATE TABLE dbo.K ([ID] int NOT NULL, Name nvarchar(20) NULL);
GO
`, preserved())
	require.Nil(t, tsqlTableOf(res, "dbo", "k").CopyColumns)
	require.Nil(t, tsqlPrereq(res, tsqlFoldCollisionTitle))

	// Several collisions (two tables, two columns) share one blocking
	// prerequisite listing every object.
	res = tsqlTranslate(t, `
CREATE TABLE dbo.[Orders] (id int NOT NULL PRIMARY KEY);
GO
CREATE TABLE dbo.[orders] (id int NOT NULL PRIMARY KEY);
GO
CREATE TABLE dbo.K ([ID] int NOT NULL, [id] int NULL);
GO
`, preserved())
	require.Len(t, tsqlWarnings(res, tsqlWarnFoldCollision), 2)
	n := 0
	for _, q := range res.Prerequisites {
		if q.Title == tsqlFoldCollisionTitle {
			n++
			require.Equal(t, "table.dbo.orders, table.dbo.k.id", q.Object)
			require.Contains(t, q.Description, "dbo.Orders and dbo.orders")
			require.Contains(t, q.Description, "columns ID and id")
		}
	}
	require.Equal(t, 1, n)
}

// The history table of an emulated temporal table copies every column
// of the current table by its source name, rowversion and geography
// included (the copier's catalog default leaves them out while the PG
// history table declares them).
func TestTSQLReviewFixes_TemporalHistoryCopyColumns(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.Emp (
    id int NOT NULL PRIMARY KEY,
    rv rowversion NOT NULL,
    loc geography NULL,
    ValidFrom datetime2 GENERATED ALWAYS AS ROW START HIDDEN NOT NULL,
    ValidTo datetime2 GENERATED ALWAYS AS ROW END HIDDEN NOT NULL,
    PERIOD FOR SYSTEM_TIME (ValidFrom, ValidTo)
) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.EmpHistory));
GO
`, preserved())
	want := []string{"id", "rv", "loc", "ValidFrom", "ValidTo"}
	cur := tsqlTableOf(res, "dbo", "emp")
	require.NotNil(t, cur)
	require.NotNil(t, cur.TSQLTemporal)
	require.True(t, cur.TSQLTemporal.CopyHistory)
	require.Equal(t, want, cur.CopyColumns)
	hist := tsqlTableOf(res, "dbo", "emphistory")
	require.NotNil(t, hist)
	require.Equal(t, want, hist.CopyColumns)

	// A temporal table without rowversion / spatial columns: the history
	// copy still names every column (as the MariaDB emulation does).
	res = tsqlTranslate(t, `
CREATE TABLE dbo.T (
    id int NOT NULL PRIMARY KEY,
    s1 datetime2 GENERATED ALWAYS AS ROW START NOT NULL,
    e1 datetime2 GENERATED ALWAYS AS ROW END NOT NULL,
    PERIOD FOR SYSTEM_TIME (s1, e1)
) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.THist));
GO
`, preserved())
	require.Nil(t, tsqlTableOf(res, "dbo", "t").CopyColumns)
	require.Equal(t, []string{"id", "s1", "e1"}, tsqlTableOf(res, "dbo", "thist").CopyColumns)
}
