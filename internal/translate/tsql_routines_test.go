package translate

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects"
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
)

// tsqlRoutinesTranslate translates a T-SQL script with the registered
// SQL Server hooks (unlike tsqlTranslate, which clears them), schemas
// preserved as httpapi does for kind sqlserver.
func tsqlRoutinesTranslate(t *testing.T, src string) *Result {
	t.Helper()
	stmts, errs := tsqldialect.Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	return Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
}

// tsqlRoutineDDL returns the DDL of the routine kind / schema.name.
func tsqlRoutineDDL(t *testing.T, res *Result, kind, schema, name string) string {
	t.Helper()
	for _, r := range res.Plan.Routines {
		if r.Kind == kind && r.Schema == schema && r.Name == name {
			return r.DDL
		}
	}
	require.Failf(t, "routine not found", "%s %s.%s", kind, schema, name)
	return ""
}

// tsqlObjectWarnings returns the messages of the warnings of kind on
// object.
func tsqlObjectWarnings(res *Result, object, kind string) []string {
	var out []string
	for _, w := range res.Warnings {
		if w.Object == object && w.Kind == kind {
			out = append(out, w.Message)
		}
	}
	return out
}

// tsqlRequireNoVariables checks that no T-SQL variable reached the PG
// text (the writer would render a leftover one as "@name").
func tsqlRequireNoVariables(t *testing.T, res *Result) {
	t.Helper()
	for _, r := range res.Plan.Routines {
		require.NotContains(t, r.DDL, `"@`, "%s %s.%s", r.Kind, r.Schema, r.Name)
	}
}

// tsqlAWSchema is a cut-down AdventureWorks schema: the tables the
// routines below read and write.
const tsqlAWSchema = `
CREATE TABLE dbo.ErrorLog (ErrorLogID int IDENTITY(1,1) NOT NULL PRIMARY KEY, ErrorTime datetime NOT NULL DEFAULT (getdate()),
    UserName sysname NOT NULL, ErrorNumber int NOT NULL, ErrorSeverity int NULL, ErrorState int NULL,
    ErrorProcedure nvarchar(126) NULL, ErrorLine int NULL, ErrorMessage nvarchar(4000) NOT NULL);
GO
CREATE TABLE Person.Person (BusinessEntityID int NOT NULL PRIMARY KEY, FirstName nvarchar(50) NOT NULL, LastName nvarchar(50) NOT NULL, Demographics xml NULL);
GO
CREATE TABLE HumanResources.Employee (BusinessEntityID int NOT NULL PRIMARY KEY, JobTitle nvarchar(50) NOT NULL, CurrentFlag bit NOT NULL);
GO
CREATE TABLE Production.ProductInventory (ProductID int NOT NULL, LocationID smallint NOT NULL, Quantity smallint NOT NULL, PRIMARY KEY (ProductID, LocationID));
GO
CREATE TABLE Production.TransactionHistory (TransactionID int IDENTITY(1,1) NOT NULL PRIMARY KEY, ProductID int NOT NULL, ReferenceOrderID int NOT NULL,
    ReferenceOrderLineID int NOT NULL, TransactionType nchar(1) NOT NULL, TransactionDate datetime NOT NULL, Quantity int NOT NULL, ActualCost money NOT NULL);
GO
CREATE TABLE Sales.SalesOrderHeader (SalesOrderID int NOT NULL PRIMARY KEY, CustomerID int NOT NULL, SubTotal money NOT NULL);
GO
CREATE TABLE Sales.SalesOrderDetail (SalesOrderID int NOT NULL, SalesOrderDetailID int NOT NULL, ProductID int NOT NULL, OrderQty smallint NOT NULL,
    UnitPrice money NOT NULL, LineTotal numeric(38,6) NOT NULL, PRIMARY KEY (SalesOrderID, SalesOrderDetailID));
GO
`

// tsqlAWProcs are AdventureWorks' error-logging procedures, as shipped
// (instawdb.sql), comments trimmed.
const tsqlAWProcs = `
CREATE PROCEDURE [dbo].[uspPrintError]
AS
BEGIN
    SET NOCOUNT ON;
    PRINT 'Error ' + CONVERT(varchar(50), ERROR_NUMBER()) +
          ', Severity ' + CONVERT(varchar(5), ERROR_SEVERITY()) +
          ', State ' + CONVERT(varchar(5), ERROR_STATE()) +
          ', Procedure ' + ISNULL(ERROR_PROCEDURE(), '-') +
          ', Line ' + CONVERT(varchar(5), ERROR_LINE());
    PRINT ERROR_MESSAGE();
END;
GO
CREATE PROCEDURE [dbo].[uspLogError]
    @ErrorLogID [int] = 0 OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    SET @ErrorLogID = 0;
    BEGIN TRY
        IF ERROR_NUMBER() IS NULL
            RETURN;
        IF XACT_STATE() = -1
        BEGIN
            PRINT 'Cannot log error since the current transaction is in an uncommittable state. '
                + 'Rollback the transaction before executing uspLogError in order to successfully log error information.';
            RETURN;
        END
        INSERT [dbo].[ErrorLog] ([UserName], [ErrorNumber], [ErrorSeverity], [ErrorState], [ErrorProcedure], [ErrorLine], [ErrorMessage])
        VALUES (CONVERT(sysname, CURRENT_USER), ERROR_NUMBER(), ERROR_SEVERITY(), ERROR_STATE(), ERROR_PROCEDURE(), ERROR_LINE(), ERROR_MESSAGE());
        SET @ErrorLogID = @@IDENTITY;
    END TRY
    BEGIN CATCH
        PRINT 'An error occurred in stored procedure uspLogError: ';
        EXECUTE [dbo].[uspPrintError];
        RETURN -1;
    END CATCH
END;
GO
`

func TestTSQLRoutines_ErrorLogProcedures(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlAWSchema+tsqlAWProcs)
	tsqlRequireNoVariables(t, res)

	logErr := tsqlRoutineDDL(t, res, "procedure", "dbo", "usplogerror")
	for _, frag := range []string{
		`CREATE OR REPLACE PROCEDURE "dbo"."usplogerror"(INOUT "errorlogid" integer DEFAULT 0)`,
		"$body$\n#variable_conflict use_column\nBEGIN\n",
		`  "usplogerror"."errorlogid" := 0;`,
		// TRY … CATCH → a nested block with an EXCEPTION handler.
		"  BEGIN\n    IF CAST(NULL AS integer) IS NULL THEN\n      RETURN;\n    END IF;",
		"  EXCEPTION\n    WHEN OTHERS THEN\n      RAISE NOTICE '%', 'An error occurred in stored procedure uspLogError: ';\n      CALL \"dbo\".\"uspprinterror\"();\n      RETURN;\n  END;",
		`IF 1 = -1 THEN`,
		`"usplogerror"."errorlogid" := lastval();`,
	} {
		require.Contains(t, logErr, frag)
	}
	// ERROR_*() in the TRY part read the caller's error: no PG
	// counterpart, one blocking entry.
	ws := tsqlObjectWarnings(res, "procedure.dbo.usplogerror", "routine.untranslated_construct")
	require.Len(t, ws, 1, "%v", ws)
	require.Contains(t, ws[0], "outside a CATCH block")
	notes := tsqlObjectWarnings(res, "procedure.dbo.usplogerror", "routine.translated_with_remarks")
	require.Contains(t, strings.Join(notes, "\n"), "XACT_STATE() reads 1")
	require.Contains(t, strings.Join(notes, "\n"), "return status is dropped")

	printErr := tsqlRoutineDDL(t, res, "procedure", "dbo", "uspprinterror")
	require.Contains(t, printErr, "RAISE NOTICE '%', 'Error ' || CAST(CAST(NULL AS integer) AS varchar(50)) || ', Severity '")
	require.Contains(t, printErr, "RAISE NOTICE '%', CAST(NULL AS text);")
	require.NotContains(t, printErr, "#variable_conflict", "no variable, no directive")
	require.Len(t, tsqlObjectWarnings(res, "procedure.dbo.uspprinterror", "routine.untranslated_construct"), 1)
}

func TestTSQLRoutines_ErrorFunctionsInCatch(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlAWSchema+`
CREATE PROCEDURE dbo.LogIt AS
BEGIN
    BEGIN TRY
        DELETE FROM dbo.ErrorLog WHERE ErrorLogID = 0;
    END TRY
    BEGIN CATCH
        INSERT INTO dbo.ErrorLog (UserName, ErrorNumber, ErrorSeverity, ErrorState, ErrorProcedure, ErrorLine, ErrorMessage)
        VALUES (N'x', ERROR_NUMBER(), ERROR_SEVERITY(), ERROR_STATE(), ERROR_PROCEDURE(), ERROR_LINE(), ERROR_MESSAGE());
        THROW;
    END CATCH
END;
GO
`)
	ddl := tsqlRoutineDDL(t, res, "procedure", "dbo", "logit")
	require.Contains(t, ddl, `VALUES ('x', "sqlstate", 16, 1, CAST(NULL AS text), CAST(NULL AS integer), "sqlerrm");`)
	require.Contains(t, ddl, "      RAISE;\n")
	require.Empty(t, tsqlObjectWarnings(res, "procedure.dbo.logit", "routine.untranslated_construct"))
	notes := strings.Join(tsqlObjectWarnings(res, "procedure.dbo.logit", "routine.translated_with_remarks"), "\n")
	require.Contains(t, notes, "ERROR_NUMBER() is translated to SQLSTATE")
}

func TestTSQLRoutines_Functions(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlAWSchema+`
CREATE FUNCTION [dbo].[ufnGetStock](@ProductID [int])
RETURNS [int]
AS
BEGIN
    DECLARE @ret int;
    SELECT @ret = SUM(p.[Quantity])
    FROM [Production].[ProductInventory] p
    WHERE p.[ProductID] = @ProductID
        AND p.[LocationID] = '6';
    IF (@ret IS NULL)
        SET @ret = 0
    RETURN @ret
END;
GO
CREATE FUNCTION [dbo].[ufnGetContactInformation](@PersonID int)
RETURNS @retContactInformation TABLE
(
    [PersonID] int NOT NULL,
    [FirstName] [nvarchar](50) NULL,
    [LastName] [nvarchar](50) NULL,
    [JobTitle] [nvarchar](50) NULL,
    [BusinessEntityType] [nvarchar](50) NULL
)
AS
BEGIN
	IF @PersonID IS NOT NULL
		BEGIN
		IF EXISTS(SELECT * FROM [HumanResources].[Employee] e
					WHERE e.[BusinessEntityID] = @PersonID)
			INSERT INTO @retContactInformation
				SELECT @PersonID, p.FirstName, p.LastName, e.[JobTitle], 'Employee'
				FROM [HumanResources].[Employee] AS e
					INNER JOIN [Person].[Person] p
					ON p.[BusinessEntityID] = e.[BusinessEntityID]
				WHERE e.[BusinessEntityID] = @PersonID;
		END
	RETURN;
END;
GO
`)
	tsqlRequireNoVariables(t, res)

	stock := tsqlRoutineDDL(t, res, "function", "dbo", "ufngetstock")
	for _, frag := range []string{
		"#variable_conflict use_column\n<<_tsql>>\nDECLARE\n  \"ret\" integer;\nBEGIN\n",
		// The parameter is qualified with the function name: the column
		// p.productid and the parameter productid cannot be confused.
		`SELECT sum("p"."quantity") INTO "_tsql"."ret" FROM "production"."productinventory" "p" WHERE "p"."productid" = "ufngetstock"."productid" AND "p"."locationid" = '6';`,
		"IF (\"_tsql\".\"ret\" IS NULL) THEN\n    \"_tsql\".\"ret\" := 0;\n  END IF;",
		`RETURN "_tsql"."ret";`,
	} {
		require.Contains(t, stock, frag)
	}
	require.Empty(t, tsqlObjectWarnings(res, "function.dbo.ufngetstock", "routine.untranslated_construct"))

	contact := tsqlRoutineDDL(t, res, "function", "dbo", "ufngetcontactinformation")
	tmp := `"pg_temp"."ufngetcontactinformation_retcontactinformation"`
	for _, frag := range []string{
		// @PersonID is renamed: PG declares the RETURNS TABLE column personid
		// as an OUT parameter of the same name (tsqlRenameClashingParams).
		`"ufngetcontactinformation"("p_personid" integer)`,
		`RETURNS TABLE ("personid" integer, "firstname" varchar(50), "lastname" varchar(50), "jobtitle" varchar(50), "businessentitytype" varchar(50))`,
		`CREATE TEMP TABLE IF NOT EXISTS "ufngetcontactinformation_retcontactinformation" ("personid" integer, "firstname" varchar(50), "lastname" varchar(50), "jobtitle" varchar(50), "businessentitytype" varchar(50)) ON COMMIT DROP;`,
		"TRUNCATE TABLE " + tmp + ";",
		`IF EXISTS (SELECT * FROM "humanresources"."employee" "e" WHERE "e"."businessentityid" = "ufngetcontactinformation"."p_personid") THEN`,
		"INSERT INTO " + tmp + ` SELECT "ufngetcontactinformation"."p_personid", "p"."firstname"`,
		"  RETURN QUERY SELECT * FROM " + tmp + ";\n  DROP TABLE " + tmp + ";\n  RETURN;\nEND;",
	} {
		require.Contains(t, contact, frag)
	}
	require.Equal(t, 1, strings.Count(contact, "RETURN QUERY"), "the explicit RETURN is the only one")
	require.Empty(t, tsqlObjectWarnings(res, "function.dbo.ufngetcontactinformation", "routine.untranslated_construct"))
}

// tsqlAWTriggers are AdventureWorks triggers: INSTEAD OF DELETE on a
// table, multi-event AFTER triggers with UPDATE(col), @@ROWCOUNT at the
// top, TRY … CATCH and ROLLBACK.
const tsqlAWTriggers = `
CREATE TRIGGER [HumanResources].[dEmployee] ON [HumanResources].[Employee]
INSTEAD OF DELETE NOT FOR REPLICATION AS
BEGIN
    DECLARE @Count int;
    SET @Count = @@ROWCOUNT;
    IF @Count = 0
        RETURN;
    SET NOCOUNT ON;
    BEGIN
        RAISERROR
            (N'Employees cannot be deleted. They can only be marked as not current.', -- Message
            10, -- Severity.
            1); -- State.
        IF @@TRANCOUNT > 0
        BEGIN
            ROLLBACK TRANSACTION;
        END
    END;
END;
GO
CREATE TRIGGER [Person].[iuPerson] ON [Person].[Person]
AFTER INSERT, UPDATE NOT FOR REPLICATION AS
BEGIN
    DECLARE @Count int;
    SET @Count = @@ROWCOUNT;
    IF @Count = 0
        RETURN;
    SET NOCOUNT ON;
    IF UPDATE([BusinessEntityID]) OR UPDATE([Demographics])
    BEGIN
        UPDATE [Person].[Person]
        SET [Person].[Person].[Demographics] = N'<IndividualSurvey/>'
        FROM inserted
        WHERE [Person].[Person].[BusinessEntityID] = inserted.[BusinessEntityID]
            AND inserted.[Demographics] IS NULL;
    END;
END;
GO
CREATE TRIGGER [Sales].[iduSalesOrderDetail] ON [Sales].[SalesOrderDetail]
AFTER INSERT, DELETE, UPDATE AS
BEGIN
    DECLARE @Count int;
    SET @Count = @@ROWCOUNT;
    IF @Count = 0
        RETURN;
    SET NOCOUNT ON;
    BEGIN TRY
        IF UPDATE([ProductID]) OR UPDATE([OrderQty]) OR UPDATE([UnitPrice])
        BEGIN
            INSERT INTO [Production].[TransactionHistory]
                ([ProductID], [ReferenceOrderID], [ReferenceOrderLineID], [TransactionType], [TransactionDate], [Quantity], [ActualCost])
            SELECT inserted.[ProductID], inserted.[SalesOrderID], inserted.[SalesOrderDetailID], 'S', GETDATE(), inserted.[OrderQty], inserted.[UnitPrice]
            FROM inserted
                INNER JOIN [Sales].[SalesOrderHeader]
                ON inserted.[SalesOrderID] = [Sales].[SalesOrderHeader].[SalesOrderID];
        END;
        UPDATE [Sales].[SalesOrderHeader]
        SET [Sales].[SalesOrderHeader].[SubTotal] =
            (SELECT SUM([Sales].[SalesOrderDetail].[LineTotal])
                FROM [Sales].[SalesOrderDetail]
                WHERE [Sales].[SalesOrderHeader].[SalesOrderID] = [Sales].[SalesOrderDetail].[SalesOrderID])
        WHERE [Sales].[SalesOrderHeader].[SalesOrderID] IN (SELECT inserted.[SalesOrderID] FROM inserted);
    END TRY
    BEGIN CATCH
        EXECUTE [dbo].[uspPrintError];
        IF @@TRANCOUNT > 0
        BEGIN
            ROLLBACK TRANSACTION;
        END
        EXECUTE [dbo].[uspLogError];
    END CATCH;
END;
GO
`

func TestTSQLRoutines_Triggers(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlAWSchema+tsqlAWProcs+tsqlAWTriggers)
	tsqlRequireNoVariables(t, res)

	// INSTEAD OF DELETE on a table: BEFORE … FOR EACH ROW returning NULL;
	// @@ROWCOUNT at the top of a row-level trigger is 1.
	del := tsqlRoutineDDL(t, res, "trigger", "humanresources", "demployee")
	for _, frag := range []string{
		"<<_tsql>>\nDECLARE\n  \"count\" integer;\nBEGIN\n  \"_tsql\".\"count\" := 1;\n",
		"IF \"_tsql\".\"count\" = 0 THEN\n    RETURN NULL;\n  END IF;",
		"RAISE NOTICE USING MESSAGE = 'Employees cannot be deleted. They can only be marked as not current.';",
		"IF 1 > 0 THEN\n    RAISE EXCEPTION 'transaction rolled back by trigger %', TG_NAME;\n  END IF;\n  RETURN NULL;\nEND;",
		"BEFORE DELETE ON \"humanresources\".\"employee\"\n  FOR EACH ROW",
	} {
		require.Contains(t, del, frag)
	}
	require.Empty(t, tsqlObjectWarnings(res, "trigger.humanresources.demployee", "routine.untranslated_construct"))

	// AFTER INSERT, UPDATE: one statement-level trigger per event, with
	// its transition tables; @@ROWCOUNT at the top counts them.
	iu := tsqlRoutineDDL(t, res, "trigger", "person", "iuperson")
	for _, frag := range []string{
		"CREATE TRIGGER \"iuperson_insert\" AFTER INSERT ON \"person\".\"person\"\n  REFERENCING NEW TABLE AS \"inserted\"\n  FOR EACH STATEMENT",
		"CREATE TRIGGER \"iuperson_update\" AFTER UPDATE ON \"person\".\"person\"\n  REFERENCING NEW TABLE AS \"inserted\" OLD TABLE AS \"deleted\"\n  FOR EACH STATEMENT",
		`"_tsql"."count" := (SELECT count(*) FROM "inserted");`,
		"IF TRUE OR TRUE THEN",
		// On UPDATE, UPDATE(col) becomes "the column's values changed".
		`IF EXISTS (SELECT CAST("businessentityid" AS text) FROM "inserted" EXCEPT ALL SELECT CAST("businessentityid" AS text) FROM "deleted") OR EXISTS (SELECT CAST("demographics" AS text) FROM "inserted" EXCEPT ALL SELECT CAST("demographics" AS text) FROM "deleted") THEN`,
		`UPDATE "person"."person" SET "demographics" = '<IndividualSurvey/>' FROM "inserted" WHERE "person"."person"."businessentityid" = "inserted"."businessentityid" AND "inserted"."demographics" IS NULL;`,
	} {
		require.Contains(t, iu, frag)
	}
	require.Equal(t, 2, strings.Count(iu, "RETURN NULL;\nEND;"))
	ws := tsqlObjectWarnings(res, "trigger.person.iuperson", "routine.untranslated_construct")
	require.Len(t, ws, 1, "UPDATE(col) is exact on INSERT, approximated on UPDATE: %v", ws)
	require.Contains(t, ws[0], "UPDATE(column)")

	// The DELETE trigger of a split trigger has no inserted rows: an
	// empty relation of the table's shape, and UPDATE(col) is FALSE.
	idu := tsqlRoutineDDL(t, res, "trigger", "sales", "idusalesorderdetail")
	for _, frag := range []string{
		`"_tsql"."count" := (SELECT count(*) FROM "deleted");`,
		"IF FALSE OR FALSE OR FALSE THEN",
		`FROM (SELECT * FROM "sales"."salesorderdetail" WHERE false) "inserted" INNER JOIN "sales"."salesorderheader"`,
		"  EXCEPTION\n    WHEN OTHERS THEN\n      CALL \"dbo\".\"uspprinterror\"();\n      IF 1 > 0 THEN\n        RAISE EXCEPTION 'transaction rolled back by trigger %: %', TG_NAME, SQLERRM;\n      END IF;\n      CALL \"dbo\".\"usplogerror\"();\n  END;",
		"REFERENCING OLD TABLE AS \"deleted\"",
	} {
		require.Contains(t, idu, frag)
	}
	require.Equal(t, 3, strings.Count(idu, "CREATE TRIGGER"))
}

const tsqlOrdersSchema = `
CREATE TABLE dbo.Orders (OrderID int NOT NULL PRIMARY KEY, CustomerID int NOT NULL, Total money NOT NULL, Shipped bit NOT NULL, Note nvarchar(100) NULL);
GO
CREATE VIEW dbo.vOrders AS SELECT OrderID, CustomerID, Total FROM dbo.Orders;
GO
`

func TestTSQLRoutines_CursorRowcountTableVariable(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlOrdersSchema+`
CREATE PROCEDURE dbo.Walk @cust int, @n int OUTPUT AS
BEGIN
    DECLARE @id int, @total money, @msg nvarchar(200) = N'start', @flag bit = 1;
    DECLARE @t TABLE (id int NOT NULL PRIMARY KEY, amount money NULL DEFAULT 0);
    DECLARE c CURSOR LOCAL FAST_FORWARD FOR SELECT OrderID, Total FROM dbo.Orders WHERE CustomerID = @cust;
    SET @n = 0;
    OPEN c;
    FETCH NEXT FROM c INTO @id, @total;
    WHILE @@FETCH_STATUS = 0
    BEGIN
        IF @total > 100 CONTINUE;
        INSERT INTO @t (id, amount) VALUES (@id, @total);
        SET @n += 1;
        IF @n > 10 BREAK;
        FETCH NEXT FROM c INTO @id, @total;
    END
    CLOSE c;
    DEALLOCATE c;
    UPDATE dbo.Orders SET Shipped = 1 WHERE OrderID IN (SELECT id FROM @t);
    IF @@ROWCOUNT = 0
        RAISERROR(N'no order shipped for customer %d (%s)', 16, 1, @cust, @msg);
    IF @flag = 1 PRINT @msg + N' done';
END;
GO
`)
	tsqlRequireNoVariables(t, res)
	ddl := tsqlRoutineDDL(t, res, "procedure", "dbo", "walk")
	for _, frag := range []string{
		"<<_tsql>>\nDECLARE\n  \"id\" integer;\n  \"total\" numeric(19,4);\n  \"msg\" varchar(200);\n  \"flag\" boolean;\n  \"_fetch_status\" integer := -1;\n  \"_rowcount\" bigint := 0;\n  \"c\" refcursor;\nBEGIN\n",
		`CREATE TEMP TABLE IF NOT EXISTS "walk_t" ("id" integer NOT NULL, "amount" numeric(19,4) DEFAULT 0, PRIMARY KEY ("id")) ON COMMIT DROP;`,
		`"_tsql"."msg" := 'start';`,
		`"_tsql"."flag" := true;`,
		`OPEN "c" FOR SELECT "orderid", "total" FROM "dbo"."orders" WHERE "customerid" = "walk"."cust";`,
		"FETCH NEXT FROM \"c\" INTO \"_tsql\".\"id\", \"_tsql\".\"total\";\n  \"_tsql\".\"_fetch_status\" := CASE WHEN \"found\" THEN 0 ELSE -1 END;\n  WHILE \"_tsql\".\"_fetch_status\" = 0 LOOP",
		"CONTINUE;",
		"INSERT INTO \"pg_temp\".\"walk_t\" (\"id\", \"amount\") VALUES (\"_tsql\".\"id\", \"_tsql\".\"total\");\n    GET DIAGNOSTICS \"_tsql\".\"_rowcount\" = ROW_COUNT;",
		`"walk"."n" := "walk"."n" + 1;`,
		"EXIT;",
		"END LOOP;\n  CLOSE \"c\";\n  UPDATE",
		"(SELECT \"id\" FROM \"pg_temp\".\"walk_t\");\n  GET DIAGNOSTICS \"_tsql\".\"_rowcount\" = ROW_COUNT;\n  IF \"_tsql\".\"_rowcount\" = 0 THEN",
		`RAISE EXCEPTION USING MESSAGE = format('no order shipped for customer %s (%s)', "walk"."cust", "_tsql"."msg");`,
		`RAISE NOTICE '%', "_tsql"."msg" || ' done';`,
	} {
		require.Contains(t, ddl, frag)
	}
	require.NotContains(t, ddl, "DEALLOCATE")
	require.Empty(t, tsqlObjectWarnings(res, "procedure.dbo.walk", "routine.untranslated_construct"))
}

func TestTSQLRoutines_TransactionsExecTempTables(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlOrdersSchema+`
CREATE PROCEDURE dbo.Walk @cust int, @n int OUTPUT AS SET @n = @cust;
GO
CREATE PROCEDURE dbo.Tx @id int AS
BEGIN
    BEGIN TRAN;
    UPDATE dbo.Orders SET Total = Total + 1 WHERE OrderID = @id;
    COMMIT TRAN;
    BEGIN TRY
        BEGIN TRANSACTION;
        DELETE FROM dbo.Orders WHERE OrderID = @id;
        COMMIT;
    END TRY
    BEGIN CATCH
        IF @@TRANCOUNT > 0 ROLLBACK;
        THROW;
    END CATCH
    DECLARE @rc int, @out int, @sql nvarchar(max) = N'DELETE FROM dbo.Orders WHERE OrderID = @p';
    EXEC @rc = dbo.Walk @cust = @id, @n = @out OUTPUT;
    EXEC Walk @id, @out OUTPUT;
    EXEC sp_executesql @sql, N'@p int', @p = @id;
    EXEC (@sql);
    EXEC sp_rename 'a', 'b';
    SELECT * INTO #work FROM dbo.Orders WHERE OrderID = @id;
    CREATE TABLE #t2 (k int NOT NULL);
    INSERT INTO #t2 SELECT OrderID FROM #work;
    DROP TABLE #t2;
    THROW 50001, N'boom', 1;
END;
GO
`)
	tsqlRequireNoVariables(t, res)
	ddl := tsqlRoutineDDL(t, res, "procedure", "dbo", "tx")
	for _, frag := range []string{
		// BEGIN TRAN dropped, COMMIT kept outside TRY…CATCH.
		"BEGIN\n  UPDATE \"dbo\".\"orders\" SET \"total\" = \"total\" + 1 WHERE \"orderid\" = \"tx\".\"id\";\n  COMMIT;\n",
		// Inside TRY…CATCH transaction control is dropped (PG refuses it
		// in a block with an EXCEPTION clause).
		"  BEGIN\n    DELETE FROM \"dbo\".\"orders\" WHERE \"orderid\" = \"tx\".\"id\";\n  EXCEPTION\n    WHEN OTHERS THEN\n      IF 1 > 0 THEN\n        NULL;\n      END IF;\n      RAISE;\n  END;",
		"CALL \"dbo\".\"walk\"(\"cust\" => \"tx\".\"id\", \"n\" => \"_tsql\".\"out\");\n  \"_tsql\".\"rc\" := 0;",
		`CALL "dbo"."walk"("tx"."id", "_tsql"."out");`,
		`EXECUTE "_tsql"."sql" USING "tx"."id";`,
		`EXECUTE "_tsql"."sql";`,
		"DROP TABLE IF EXISTS \"pg_temp\".\"work\";\n  CREATE TEMP TABLE \"work\" AS SELECT * FROM \"dbo\".\"orders\" WHERE \"orderid\" = \"tx\".\"id\";",
		"DROP TABLE IF EXISTS \"pg_temp\".\"t2\";\n  CREATE TEMP TABLE \"t2\" (\"k\" integer NOT NULL);",
		`INSERT INTO "pg_temp"."t2" SELECT "orderid" FROM "pg_temp"."work";`,
		`RAISE EXCEPTION USING MESSAGE = 'boom';`,
	} {
		require.Contains(t, ddl, frag)
	}
	require.NotContains(t, ddl, "sp_rename")
	require.NotContains(t, ddl, "BEGIN TRAN")
	ws := strings.Join(tsqlObjectWarnings(res, "procedure.dbo.tx", "routine.untranslated_construct"), "\n")
	require.Contains(t, ws, "no return status")
	require.Contains(t, ws, "$1, $2")
	require.Contains(t, ws, "dynamic SQL")
	require.Contains(t, ws, "sp_rename")
	require.NotContains(t, ws, "SELECT … INTO", "SELECT … INTO #temp is translated")
}

func TestTSQLRoutines_RowLevelPseudoTables(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlOrdersSchema+`
CREATE TRIGGER dbo.trgVOrders ON dbo.vOrders INSTEAD OF INSERT AS
BEGIN
    INSERT INTO dbo.Orders (OrderID, CustomerID, Total, Shipped, Note) SELECT i.OrderID, i.CustomerID, i.Total, 0, N'x' FROM inserted AS i;
    IF EXISTS (SELECT 1 FROM deleted) PRINT 'never';
    IF @@ROWCOUNT > 1 RETURN;
END;
GO
`)
	ddl := tsqlRoutineDDL(t, res, "trigger", "dbo", "trgvorders")
	require.Contains(t, ddl, `FROM (SELECT NEW.*) "i";`)
	require.Contains(t, ddl, `IF EXISTS (SELECT 1 FROM (SELECT * FROM "dbo"."vorders" WHERE false) "deleted") THEN`)
	// @@ROWCOUNT after a statement: the _rowcount local.
	require.Contains(t, ddl, "GET DIAGNOSTICS \"_tsql\".\"_rowcount\" = ROW_COUNT;")
	require.Contains(t, ddl, "IF \"_tsql\".\"_rowcount\" > 1 THEN\n    RETURN NEW;\n  END IF;\n  RETURN NEW;\nEND;")
	require.Contains(t, ddl, "INSTEAD OF INSERT ON \"dbo\".\"vorders\"\n  FOR EACH ROW")
}

func TestTSQLRoutines_SelectAssignments(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlOrdersSchema+`
CREATE FUNCTION dbo.Accum (@c int) RETURNS nvarchar(4000) AS
BEGIN
    DECLARE @s nvarchar(4000) = N'', @n int;
    SELECT @s = @s + Note FROM dbo.Orders WHERE CustomerID = @c;
    SELECT @n = COUNT(*), @s = MAX(Note) FROM dbo.Orders;
    SELECT @s = @s + N'.', @c = @c + 1;
    SET @n = (SELECT MIN(OrderID) FROM dbo.Orders);
    RETURN @s;
END;
GO
`)
	ddl := tsqlRoutineDDL(t, res, "function", "dbo", "accum")
	for _, frag := range []string{
		`SELECT "_tsql"."s" || "note" INTO "_tsql"."s" FROM "dbo"."orders" WHERE "customerid" = "accum"."c";`,
		`SELECT count(*), max("note") INTO "_tsql"."n", "_tsql"."s" FROM "dbo"."orders";`,
		// No FROM: T-SQL assigns left to right.
		"\"_tsql\".\"s\" := \"_tsql\".\"s\" || '.';\n  \"accum\".\"c\" := \"accum\".\"c\" + 1;",
		`"_tsql"."n" := (SELECT min("orderid") FROM "dbo"."orders");`,
	} {
		require.Contains(t, ddl, frag)
	}
	ws := tsqlObjectWarnings(res, "function.dbo.accum", "routine.untranslated_construct")
	require.Len(t, ws, 1, "%v", ws)
	require.Contains(t, ws[0], "accumulates over every row")
}

func TestTSQLRoutines_Unsupported(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlOrdersSchema+`
CREATE PROCEDURE dbo.Odd AS
BEGIN
    DECLARE @x int = 1;
    IF @x = 1 GOTO done;
    WAITFOR DELAY '00:00:01';
    INSERT INTO dbo.Orders EXEC dbo.Odd;
    SELECT OrderID FROM dbo.Orders;
    done:
    RETURN;
END;
GO
`)
	ddl := tsqlRoutineDDL(t, res, "procedure", "dbo", "odd")
	require.Contains(t, ddl, `PERFORM * FROM (SELECT "orderid" FROM "dbo"."orders") AS "_rs";`)
	ws := strings.Join(tsqlObjectWarnings(res, "procedure.dbo.odd", "routine.untranslated_construct"), "\n")
	require.Contains(t, ws, "GOTO")
	require.Contains(t, ws, "WAITFOR")
	require.Contains(t, ws, "INSERT … EXEC")
}

func TestTSQLFormatPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		in    string
		args  bool
		want  string
		lossy bool
	}{
		{"plain", true, "plain", false},
		{"id %d name %s", true, "id %s name %s", false},
		{"%i/%u", true, "%s/%s", false},
		{"%-10s|%5d", true, "%-10s|%5s", false},
		{"%*s", true, "%*s", false},
		{"%.3s", true, "%s", true},
		{"%+d %05d", true, "%s %5s", true},
		{"%x", true, "%s", true},
		{"%ld %hs", true, "%s %s", false},
		{"100%% sure, %s", true, "100%% sure, %s", false},
		{"50%! %q", true, "50%%! %%q", false},
		{"trailing %", true, "trailing %%", false},
		{"100%% done", false, "100% done", false},
		{"no args %d", false, "no args %d", false},
		{"é %s ü", true, "é %s ü", false},
	} {
		got, lossy := tsqlFormatPlaceholders(tc.in, tc.args)
		require.Equal(t, tc.want, got, tc.in)
		require.Equal(t, tc.lossy, lossy, tc.in)
	}
}

func TestTSQLRoutines_ExceptionHandlerWriter(t *testing.T) {
	blk := &pgast.PLBlock{
		Body:     []pgast.PLStmt{&pgast.PLRawSQL{Text: "NULL"}},
		Handlers: []pgast.PLHandler{{Conditions: []string{"unique_violation", "OTHERS"}, Body: []pgast.PLStmt{&pgast.PLRawSQL{Text: "RAISE"}}}},
	}
	require.Equal(t, "BEGIN\n  NULL;\nEXCEPTION\n  WHEN unique_violation OR OTHERS THEN\n    RAISE;\nEND;\n", pgast.WritePLpgSQL(blk))
	// The pre-rendered form keeps working, and both can be combined.
	blk.Exception = "  WHEN no_data_found THEN\n    NULL;"
	require.Equal(t, "BEGIN\n  NULL;\nEXCEPTION\n  WHEN no_data_found THEN\n    NULL;\n  WHEN unique_violation OR OTHERS THEN\n    RAISE;\nEND;\n", pgast.WritePLpgSQL(blk))
}

// Every routine of the real scripts translates through the hooks:
// bodies are produced, no T-SQL variable reaches the PG text.
func TestTSQLRoutines_Corpus(t *testing.T) {
	files := []string{
		"../../test/fixtures/sqlserver_sample.sql",
		"../dialects/tsql/testdata/instnwnd.sql",
		"../dialects/tsql/testdata/instpubs.sql",
		"../dialects/tsql/testdata/instawdb.sql",
	}
	ran := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		ran++
		t.Run(f, func(t *testing.T) {
			stmts, _ := tsqldialect.Parse(string(src))
			res := Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
			for _, r := range res.Plan.Routines {
				require.NotEmpty(t, r.DDL)
				require.NotContains(t, r.DDL, `"@`, "%s %s.%s", r.Kind, r.Schema, r.Name)
			}
			for _, w := range res.Warnings {
				require.NotContains(t, w.Message, "no SQL Server routine-body translator", w.Object)
			}
		})
	}
	if ran == 0 {
		t.Skip("no T-SQL corpus file present")
	}
}

func TestTSQLRoutines_RowLevelUpdateTest(t *testing.T) {
	res := tsqlRoutinesTranslate(t, tsqlOrdersSchema+`
CREATE TRIGGER dbo.trgVOrdersUpd ON dbo.vOrders INSTEAD OF UPDATE AS
BEGIN
    IF NOT UPDATE(Total)
        UPDATE o SET CustomerID = i.CustomerID FROM dbo.Orders o JOIN inserted i ON i.OrderID = o.OrderID;
END;
GO
CREATE TRIGGER dbo.trgVOrdersAll ON dbo.vOrders INSTEAD OF INSERT, UPDATE AS
BEGIN
    IF UPDATE(Total) PRINT 'total';
END;
GO
`)
	upd := tsqlRoutineDDL(t, res, "trigger", "dbo", "trgvordersupd")
	require.Contains(t, upd, `IF NOT (CAST(NEW."total" AS text) IS DISTINCT FROM CAST(OLD."total" AS text)) THEN`)
	require.Contains(t, upd, `(SELECT NEW.*) "i"`)
	all := tsqlRoutineDDL(t, res, "trigger", "dbo", "trgvordersall")
	require.Contains(t, all, `IF CASE WHEN TG_OP = 'UPDATE' THEN CAST(NEW."total" AS text) IS DISTINCT FROM CAST(OLD."total" AS text) ELSE TG_OP = 'INSERT' END THEN`)
}

// The translator also works outside a Translate run (no HookRun): names
// are folded, unqualified objects resolve to the routine's schema.
func TestTSQLRoutines_WithoutRun(t *testing.T) {
	body, warnings, notes := translateTSQLRoutineBody(&RoutineCtx{
		Kind: RoutineProcedure, Object: "procedure.dbo.p",
		SourceSchema: "Sales", SourceName: "p", Schema: "sales", Name: "p",
		Body: "SET NOCOUNT ON; DECLARE @x int = 1, @t TABLE (k int DEFAULT 0); INSERT @t (k) VALUES (@x); EXEC q @x;",
	})
	require.Empty(t, warnings)
	require.Empty(t, notes)
	for _, frag := range []string{
		"#variable_conflict use_column\n<<_tsql>>\nDECLARE\n  \"x\" integer;\nBEGIN\n",
		`CREATE TEMP TABLE IF NOT EXISTS "p_t" ("k" integer DEFAULT 0) ON COMMIT DROP;`,
		`"_tsql"."x" := 1;`,
		`INSERT INTO "pg_temp"."p_t" ("k") VALUES ("_tsql"."x");`,
		`CALL "sales"."q"("_tsql"."x");`,
	} {
		require.Contains(t, body, frag)
	}

	// An empty body still yields a valid routine, reported.
	body, warnings, _ = translateTSQLRoutineBody(&RoutineCtx{
		Kind: RoutineTrigger, Object: "trigger.dbo.t", SourceSchema: "dbo", Schema: "dbo", Name: "t",
		Trigger: &TriggerInfo{Timing: "AFTER", Events: []string{"INSERT"}, ForEach: "STATEMENT", Schema: "dbo", Table: "x", NewTable: "inserted", ReturnRow: "NULL"},
	})
	require.Contains(t, body, "RAISE EXCEPTION 'routine dbo.t was not translated from T-SQL';\n  RETURN NULL;")
	require.Len(t, warnings, 1)
}
