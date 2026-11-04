package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// smokeScript mixes the shapes SSMS "Generate Scripts" and hand-written
// routines produce; it must parse without errors and its bodies must be
// fully typed. (Unlike the corpus dumps it is embedded, so it always runs.)
const smokeScript = `
/****** Object:  Table [dbo].[Orders]    Script Date: 1/1/2024 ******/
SET ANSI_NULLS ON
GO
SET QUOTED_IDENTIFIER ON
GO
CREATE TABLE [dbo].[Orders](
	[OrderID] [int] IDENTITY(1,1) NOT NULL,
	[Amount] [decimal](18, 2) NULL,
	[Ver] [timestamp] NOT NULL,
	[Doc] [varbinary](max) FILESTREAM NULL,
	[G] [uniqueidentifier] ROWGUIDCOL NOT NULL,
 CONSTRAINT [PK_Orders] PRIMARY KEY CLUSTERED
(
	[OrderID] ASC
)WITH (PAD_INDEX = OFF, STATISTICS_NORECOMPUTE = OFF, IGNORE_DUP_KEY = OFF, ALLOW_ROW_LOCKS = ON, ALLOW_PAGE_LOCKS = ON, OPTIMIZE_FOR_SEQUENTIAL_KEY = OFF) ON [PRIMARY]
) ON [PRIMARY] TEXTIMAGE_ON [PRIMARY] FILESTREAM_ON [FS]
GO
ALTER TABLE [dbo].[Orders] ADD  DEFAULT ((0)) FOR [Amount]
GO
ALTER TABLE [dbo].[Orders]  WITH CHECK ADD  CONSTRAINT [FK_Orders_C] FOREIGN KEY([OrderID])
REFERENCES [dbo].[C] ([ID])
ON DELETE CASCADE
GO
ALTER TABLE [dbo].[Orders] CHECK CONSTRAINT [FK_Orders_C]
GO
CREATE PROCEDURE dbo.p_cursor
AS
BEGIN
	SET NOCOUNT ON;
	DECLARE @id int, @name nvarchar(100), @sql nvarchar(max) = N'', @n int = 0;
	DECLARE cur CURSOR LOCAL FAST_FORWARD FOR
		SELECT id, name FROM dbo.t WITH (NOLOCK) ORDER BY id;
	OPEN cur;
	FETCH NEXT FROM cur INTO @id, @name;
	WHILE @@FETCH_STATUS = 0
	BEGIN
		SET @sql += N'SELECT ' + CAST(@id AS nvarchar(10)) + N';' + CHAR(13) + CHAR(10);
		SET @n = @n + 1;
		IF @n % 100 = 0 WAITFOR DELAY '00:00:01';
		FETCH NEXT FROM cur INTO @id, @name;
	END
	CLOSE cur;
	DEALLOCATE cur;
	EXEC sys.sp_executesql @sql;
	SELECT STUFF((SELECT ',' + name FROM dbo.t FOR XML PATH(''), TYPE).value('.', 'nvarchar(max)'), 1, 1, '') AS names;
	SELECT TOP (1) WITH TIES * FROM dbo.t ORDER BY score DESC;
	SELECT *, ROW_NUMBER() OVER (PARTITION BY a ORDER BY b) AS rn,
		SUM(x) OVER (ORDER BY d ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) AS s,
		LAG(x, 1, 0) OVER (ORDER BY d) AS l,
		FORMAT(GETDATE(), 'yyyy-MM-dd') AS f,
		DATEDIFF(day, '2020-01-01', SYSDATETIME()) AS dd,
		IIF(x > 0, 'pos', 'neg') AS sign,
		CHOOSE(2, 'a', 'b') AS ch,
		CONCAT_WS(',', a, b) AS cw,
		TRY_PARSE_ISH = TRY_CONVERT(date, '2020-13-45', 102),
		x.query('/a') AS q
	FROM dbo.t AS t
	WHERE name LIKE N'%[_]x%' ESCAPE '\' AND d BETWEEN @a AND @b OR NOT EXISTS (SELECT 1 FROM u WHERE u.id = t.id)
	GROUP BY ROLLUP (a, b)
	HAVING COUNT(*) > 1;
	MERGE dbo.target WITH (HOLDLOCK) AS tg
	USING (VALUES (1, N'a'), (2, N'b')) AS src (id, name)
	ON tg.id = src.id
	WHEN MATCHED THEN UPDATE SET tg.name = src.name
	WHEN NOT MATCHED THEN INSERT (id, name) VALUES (src.id, src.name)
	OUTPUT $action, inserted.*;
	INSERT INTO dbo.log (msg) OUTPUT inserted.id INTO @ids SELECT TOP 10 msg FROM dbo.src;
	UPDATE t SET t.x = s.x FROM dbo.t t INNER JOIN dbo.s s ON s.id = t.id WHERE s.flag = 1;
	DELETE t FROM dbo.t AS t LEFT JOIN dbo.s AS s ON s.id = t.id WHERE s.id IS NULL;
	IF OBJECT_ID('tempdb..#tmp') IS NOT NULL DROP TABLE #tmp;
	SELECT a INTO #tmp FROM dbo.t;
	CREATE INDEX ix_tmp ON #tmp (a);
	TRUNCATE TABLE #tmp;
	BEGIN TRY
		RAISERROR(N'boom %d', 16, 1, @n) WITH NOWAIT;
	END TRY
	BEGIN CATCH
		DECLARE @msg nvarchar(4000) = ERROR_MESSAGE(), @sev int = ERROR_SEVERITY();
		IF XACT_STATE() <> 0 ROLLBACK TRANSACTION;
		THROW 50000, @msg, 1;
	END CATCH
	RETURN 0;
END
GO
CREATE FUNCTION dbo.fn_split (@s nvarchar(max), @sep nchar(1))
RETURNS TABLE
WITH SCHEMABINDING
AS
RETURN
	WITH cte AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM cte WHERE n < 10)
	SELECT n, SUBSTRING(@s, n, 1) AS c FROM cte
GO
CREATE OR ALTER VIEW dbo.v_orders WITH SCHEMABINDING AS
SELECT o.OrderID, c.Name, COUNT_BIG(*) AS cnt
FROM dbo.Orders AS o
JOIN dbo.C AS c ON c.ID = o.OrderID
GROUP BY o.OrderID, c.Name
GO
CREATE TRIGGER dbo.trg_orders ON dbo.Orders
WITH EXECUTE AS OWNER
FOR INSERT, UPDATE
AS
SET NOCOUNT ON
IF UPDATE(Amount) AND EXISTS (SELECT 1 FROM inserted i JOIN deleted d ON d.OrderID = i.OrderID WHERE i.Amount <> d.Amount)
BEGIN
	INSERT dbo.audit (id, old_amount, new_amount, at)
	SELECT i.OrderID, d.Amount, i.Amount, SYSUTCDATETIME()
	FROM inserted i
	JOIN deleted d ON d.OrderID = i.OrderID
END
GO
CREATE SEQUENCE dbo.seq START WITH -10 INCREMENT BY 1 MINVALUE -100 MAXVALUE 100 CYCLE CACHE
GO
DECLARE @t TABLE (id int IDENTITY(1,1) PRIMARY KEY, v nvarchar(10) DEFAULT N'x' NOT NULL)
INSERT @t (v) VALUES (N'a')
SELECT * FROM @t
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'Orders table' , @level0type=N'SCHEMA',@level0name=N'dbo', @level1type=N'TABLE',@level1name=N'Orders'
GO
`

func TestSmokeScript(t *testing.T) {
	stmts := mustParse(t, smokeScript)
	allowed := map[string]bool{}
	kinds := map[string]int{}
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.CreateProcedure:
			kinds["proc"]++
			checkBody(t, "procedure "+x.Name, x.Body, allowed)
		case *ast.CreateTrigger:
			kinds["trigger"]++
			checkBody(t, "trigger "+x.Name, x.Body, allowed)
		case *ast.CreateFunction:
			kinds["function"]++
			if x.Select == nil {
				t.Errorf("inline function %s: %s", x.Name, x.SelectParseError)
			}
		case *ast.CreateView:
			kinds["view"]++
			if x.Select == nil {
				t.Errorf("view %s: %s", x.View.Name, x.SelectParseError)
			}
		case *ast.CreateTable:
			kinds["table"]++
		case *ast.AlterTable:
			kinds["alter"]++
		case *ast.CommentStmt:
			kinds["comment"]++
		}
	}
	want := map[string]int{"proc": 1, "trigger": 1, "function": 1, "view": 1, "table": 1, "alter": 3, "comment": 1}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s: want %d, got %d", k, n, kinds[k])
		}
	}
}
