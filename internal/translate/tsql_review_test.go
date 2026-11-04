package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Review fixes of the SQL Server path: each test pins one construct the
// translation used to drop or change silently.

const tsqlReviewTables = `
CREATE TABLE dbo.t (id int IDENTITY(1,1) NOT NULL PRIMARY KEY, v int NULL, active bit NOT NULL DEFAULT 1,
    data varbinary(10) NULL, code varchar(3) NULL, created datetime NOT NULL DEFAULT getdate());
GO
CREATE TABLE dbo.s (id int NOT NULL PRIMARY KEY, v int NULL);
GO
`

// INSERT … DEFAULT VALUES, MERGE … WHEN NOT MATCHED BY SOURCE, ~bit and
// CONVERT(varbinary, …, 1) in a procedure body.
func TestTSQLReview_StatementShapes(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlReviewTables+`
CREATE PROCEDURE dbo.flip @b bit, @hex varchar(20) AS
BEGIN
    INSERT INTO dbo.t DEFAULT VALUES;
    MERGE dbo.t AS tgt USING dbo.s AS src ON tgt.id = src.id
        WHEN MATCHED THEN UPDATE SET v = src.v
        WHEN NOT MATCHED BY SOURCE AND tgt.v > 0 THEN DELETE;
    SET @b = ~@b;
    UPDATE dbo.t SET active = ~active;
    UPDATE dbo.t SET data = CONVERT(varbinary(10), @hex, 1);
END;
GO
`)
	ddl := tsqlRoutineDDL(t, res, "procedure", "dbo", "flip")
	for _, frag := range []string{
		`INSERT INTO "dbo"."t" DEFAULT VALUES;`,
		`WHEN NOT MATCHED BY SOURCE AND "tgt"."v" > 0 THEN DELETE;`,
		`"flip"."b" := (NOT "flip"."b");`,
		`SET "active" = (NOT "active")`,
		`SET "data" = decode(substr("flip"."hex", 3), 'hex')`,
	} {
		require.Contains(t, ddl, frag)
	}
	require.Empty(t, tsqlObjectWarnings(res, "procedure.dbo.flip", "routine.untranslated_construct"))
	notes := strings.Join(tsqlObjectWarnings(res, "procedure.dbo.flip", tsqlWarnNote), "\n")
	require.Contains(t, notes, "PostgreSQL 17")
}

// Computed columns: decimal division and precision past 38 are not proven
// identical (SQL Server's result scale differs); ISNULL is typed like its
// first argument.
func TestTSQLReview_ComputedProof(t *testing.T) {
	res := tsqlRoutinesTranslate(t, `
CREATE TABLE dbo.c (
    id int NOT NULL,
    price decimal(10,2) NOT NULL,
    qty decimal(10,2) NOT NULL,
    total money NOT NULL,
    n int NOT NULL,
    code varchar(3) NULL,
    big decimal(38,10) NOT NULL,
    tiny decimal(38,30) NOT NULL,
    unit_cost AS (price / qty) PERSISTED,
    third AS (total / 3) PERSISTED,
    sq AS (total * total) PERSISTED,
    line AS (price * n) PERSISTED,
    half AS (n / 2) PERSISTED,
    huge AS (big * price) PERSISTED,
    wide AS (big + tiny) PERSISTED,
    code_label AS (ISNULL(code, 'UNKNOWN')) PERSISTED,
    code_short AS (ISNULL(code, 'NA')) PERSISTED
);
GO
`)
	tbl := tsqlTableOf(res, "dbo", "c")
	require.NotNil(t, tbl)
	require.ElementsMatch(t, []string{"unit_cost", "third", "sq", "wide"}, tbl.CopiedGenerated)
	gen := map[string]string{}
	for _, c := range tbl.Columns {
		if c.Generated != nil {
			gen[c.Name] = c.Generated.Expr
		}
	}
	require.Contains(t, gen, "line")
	require.Contains(t, gen, "half")
	// Past precision 38 SQL Server rounds a product half away from zero
	// to the reduced scale: a CAST to numeric(38, 6) does the same.
	require.Equal(t, `CAST(("big" * "price") AS numeric(38,6))`, gen["huge"])
	// coalesce would return 'UNKNOWN' whole, which varchar(3) rejects;
	// SQL Server stores 'UNK'.
	require.Equal(t, `CAST(coalesce("code", 'UNKNOWN') AS varchar(3))`, gen["code_label"])
	require.Equal(t, `coalesce("code", 'NA')`, gen["code_short"])
	msgs := ""
	for _, w := range tsqlWarnings(res, "table.computed_not_generated") {
		msgs += w.Message + "\n"
	}
	require.Contains(t, msgs, "decimal division")
	require.Contains(t, msgs, "money")
	require.Contains(t, msgs, "precision 38")
}

// ISNULL outside DDL: cast back to the first argument's type when the
// replacement may not fit.
func TestTSQLReview_IsnullTruncates(t *testing.T) {
	out, res := tsqlExprOut(t, "ISNULL(code, 'UNKNOWN')", tsqlTestCtx(map[string]string{"code": "varchar(3)"}))
	require.Equal(t, `CAST(coalesce("code", 'UNKNOWN') AS varchar(3))`, out)
	require.Empty(t, res.Labels)
	out, _ = tsqlExprOut(t, "ISNULL(code, 'AB')", tsqlTestCtx(map[string]string{"code": "varchar(3)"}))
	require.Equal(t, `coalesce("code", 'AB')`, out)
	out, _ = tsqlExprOut(t, "ISNULL(code, other)", tsqlTestCtx(map[string]string{"code": "varchar(3)", "other": "varchar(3)"}))
	require.Equal(t, `coalesce("code", "other")`, out)
	out, _ = tsqlExprOut(t, "ISNULL(name, 'UNKNOWN')", tsqlTestCtx(map[string]string{"name": "text"}))
	require.Equal(t, `coalesce("name", 'UNKNOWN')`, out)
}

// Names folding to one PG name — by case in a case-sensitive database,
// or past 63 bytes — are blocking collisions, never an info duplicate.
func TestTSQLReview_FoldCollisions(t *testing.T) {
	long := strings.Repeat("Résumé", 11) // 77 bytes
	res := tsqlRoutinesTranslate(t, `
CREATE TABLE dbo.[Order] (id int NOT NULL PRIMARY KEY);
GO
CREATE TABLE dbo.[order] (id int NOT NULL PRIMARY KEY);
GO
CREATE TABLE dbo.k ([ID] int NOT NULL, [id] int NULL, [`+long+`A] int NULL, [`+long+`B] int NULL);
GO
`)
	ws := tsqlWarnings(res, tsqlWarnFoldCollision)
	require.Len(t, ws, 3)
	for _, w := range ws {
		require.Equal(t, string(SeverityBlocking), w.Severity)
	}
	require.Contains(t, ws[0].Message, "dbo.Order and dbo.order")
	require.Empty(t, tsqlWarnings(res, "table.duplicate"))
	k := tsqlTableOf(res, "dbo", "k")
	require.NotNil(t, k)
	require.Len(t, k.Columns, 2)
	require.LessOrEqual(t, len(k.Columns[1].Name), 63)
	// The same table twice (same source name) stays an info duplicate.
	res = tsqlRoutinesTranslate(t, `
CREATE TABLE dbo.d (id int NOT NULL);
GO
CREATE TABLE dbo.d (id int NOT NULL);
GO
`)
	require.Empty(t, tsqlWarnings(res, tsqlWarnFoldCollision))
	require.Len(t, tsqlWarnings(res, "table.duplicate"), 1)
}

// Several data modifications in a TRY without an explicit transaction:
// PG rolls back what SQL Server keeps.
func TestTSQLReview_TryPartialRollback(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlReviewTables+`
CREATE PROCEDURE dbo.two AS
BEGIN
    BEGIN TRY
        INSERT INTO dbo.s (id, v) VALUES (1, 1);
        UPDATE dbo.t SET v = v - 1;
    END TRY
    BEGIN CATCH
        PRINT 'failed';
    END CATCH
END;
GO
CREATE PROCEDURE dbo.twotran AS
BEGIN
    BEGIN TRY
        BEGIN TRANSACTION;
        INSERT INTO dbo.s (id, v) VALUES (1, 1);
        UPDATE dbo.t SET v = v - 1;
        COMMIT TRANSACTION;
    END TRY
    BEGIN CATCH
        IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION;
    END CATCH
END;
GO
CREATE PROCEDURE dbo.branches @x int AS
BEGIN
    BEGIN TRY
        IF @x > 0 INSERT INTO dbo.s (id, v) VALUES (1, 1);
        ELSE UPDATE dbo.t SET v = 0;
    END TRY
    BEGIN CATCH
        PRINT 'failed';
    END CATCH
END;
GO
`)
	has := func(obj string) bool {
		for _, w := range tsqlObjectWarnings(res, obj, "routine.untranslated_construct") {
			if strings.Contains(w, "outside an explicit transaction") {
				return true
			}
		}
		return false
	}
	require.True(t, has("procedure.dbo.two"))
	require.False(t, has("procedure.dbo.twotran"))
	require.False(t, has("procedure.dbo.branches"))
}

// sp_executesql binds its arguments to the declared parameter types, and
// a caller variable bound to an OUTPUT parameter and another one is not
// inlined.
func TestTSQLReview_DynamicSQLBinding(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlReviewTables+`
CREATE PROCEDURE dbo.dyn @key varchar(10), @x int OUTPUT, @y int OUTPUT AS
BEGIN
    EXEC sp_executesql N'SELECT @n = COUNT(*) FROM dbo.t WHERE code = @c', N'@c char(2), @n int OUTPUT', 'ABC', @n = @x OUTPUT;
    EXEC sp_executesql N'SELECT @n = COUNT(*) FROM dbo.t WHERE v = @p', N'@p int, @n int OUTPUT', @key, @n = @x OUTPUT;
    EXEC sp_executesql N'SET @o = @i + 1; SET @o2 = @i', N'@i int, @o int OUTPUT, @o2 int OUTPUT', @x, @x OUTPUT, @y OUTPUT;
END;
GO
`)
	ddl := tsqlRoutineDDL(t, res, "procedure", "dbo", "dyn")
	require.Contains(t, ddl, `WHERE "code" = CAST('ABC' AS char(2))`)
	require.Contains(t, ddl, `WHERE "v" = CAST("dyn"."key" AS integer)`)
	// Aliased: kept dynamic (EXECUTE + warning).
	require.Contains(t, ddl, "EXECUTE ")
	require.NotContains(t, ddl, `"dyn"."x" := "dyn"."x" + 1`)
}

// Table-variable temp tables carry the routine's schema: same-named
// routines of two schemas never share one.
func TestTSQLReview_TempTablePerSchema(t *testing.T) {
	res := tsqlRoutinesTranslate(t, `
CREATE PROCEDURE sales.GetList AS
BEGIN
    DECLARE @t TABLE (id int);
    INSERT INTO @t (id) VALUES (1);
END;
GO
CREATE PROCEDURE dbo.GetList AS
BEGIN
    DECLARE @t TABLE (id int);
    INSERT INTO @t (id) VALUES (2);
END;
GO
`)
	require.Contains(t, tsqlRoutineDDL(t, res, "procedure", "sales", "getlist"), `"pg_temp"."sales_getlist_t"`)
	require.Contains(t, tsqlRoutineDDL(t, res, "procedure", "dbo", "getlist"), `"pg_temp"."dbo_getlist_t"`)
}

// A % in an object name is doubled in the RAISE format strings the
// translation builds itself.
func TestTSQLReview_RaiseTextEscapesPercent(t *testing.T) {
	require.Equal(t, "dbo.discount_50%%_rates failed", tsqlRaiseText("dbo.discount_50%_rates failed"))
	require.Equal(t, "plain", tsqlRaiseText("plain"))
}
