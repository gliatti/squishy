package translate

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// A cursor whose PG local is renamed (a parameter @cur already took the
// folded name cur) is still opened SCROLL when it is fetched LAST: the
// scroll use is recorded and read back under the same key (the folded
// T-SQL name), not the PG local name.
func TestTSQLRoutines_ScrollCursorRenamed(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlOrdersSchema+`
CREATE PROCEDURE dbo.LastRenamed @cur int AS
BEGIN
    DECLARE @id int;
    DECLARE cur CURSOR FOR SELECT OrderID FROM dbo.Orders WHERE CustomerID = @cur;
    OPEN cur;
    FETCH LAST FROM cur INTO @id;
    CLOSE cur;
    DEALLOCATE cur;
END;
GO
CREATE PROCEDURE dbo.LastPlain @cust int AS
BEGIN
    DECLARE @id int;
    DECLARE cur CURSOR FOR SELECT OrderID FROM dbo.Orders WHERE CustomerID = @cust;
    OPEN cur;
    FETCH PRIOR FROM cur INTO @id;
    CLOSE cur;
END;
GO
CREATE PROCEDURE dbo.NextOnly @cur int AS
BEGIN
    DECLARE @id int;
    DECLARE cur CURSOR FOR SELECT OrderID FROM dbo.Orders WHERE CustomerID = @cur;
    OPEN cur;
    FETCH NEXT FROM cur INTO @id;
    CLOSE cur;
END;
GO
`)
	tsqlRequireNoVariables(t, res)

	renamed := tsqlRoutineDDL(t, res, "procedure", "dbo", "lastrenamed")
	require.Contains(t, renamed, `"cur_1" refcursor;`)
	require.Contains(t, renamed, `OPEN "cur_1" SCROLL FOR SELECT "orderid" FROM "dbo"."orders"`)
	require.Contains(t, renamed, `FETCH LAST FROM "cur_1" INTO`)

	plain := tsqlRoutineDDL(t, res, "procedure", "dbo", "lastplain")
	require.Contains(t, plain, `OPEN "cur" SCROLL FOR SELECT "orderid" FROM "dbo"."orders"`)

	next := tsqlRoutineDDL(t, res, "procedure", "dbo", "nextonly")
	require.Contains(t, next, `OPEN "cur_1" FOR SELECT`)
	require.NotContains(t, next, "SCROLL")
}

// tsqlTruncIdent cuts like normalizeTSQLIdent (the package's clipUTF8 at
// pgMaxIdentLen): at most 63 bytes, never inside a UTF-8 sequence.
func TestTSQLTruncIdentRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 35) // 70 bytes; byte 63 is a continuation byte
	require.Len(t, s, 70)
	got := tsqlTruncIdent(s)
	require.Len(t, got, 62)
	require.True(t, utf8.ValidString(got))
	require.Equal(t, normalizeTSQLIdent(s), got)

	ascii := strings.Repeat("a", 70)
	require.Equal(t, strings.Repeat("a", 63), tsqlTruncIdent(ascii))
	require.Equal(t, normalizeTSQLIdent(ascii), tsqlTruncIdent(ascii))
	require.Equal(t, "short", tsqlTruncIdent("short"))
}
