package translate

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dataxfer"
	"gitlab.com/dalibo/squishy/internal/dialects"
	tsqldialect "gitlab.com/dalibo/squishy/internal/dialects/tsql"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// tsqlTranslate parses a T-SQL script with the SQL Server parser and
// translates it with schemas preserved (the httpapi options for kind
// sqlserver), unless opt says otherwise. The registered SQL Server hooks
// are cleared (withTSQLHooks): expressions, queries and bodies pass
// through unchanged, so the assertions cover the translator core only.
func tsqlTranslate(t *testing.T, src string, opt Options) *Result {
	t.Helper()
	withTSQLHooks(t)
	stmts, errs := tsqldialect.Parse(src)
	require.Empty(t, errs, "parse errors: %v", errs)
	opt.SourceKind = dialects.KindSQLServer
	if opt.TargetSchema == "" {
		opt.TargetSchema = "public"
	}
	return Translate(stmts, opt)
}

func preserved() Options { return Options{PreserveSourceSchemas: true} }

// tsqlWarnings returns the warnings of kind.
func tsqlWarnings(res *Result, kind string) []Warning {
	var out []Warning
	for _, w := range res.Warnings {
		if w.Kind == kind {
			out = append(out, w)
		}
	}
	return out
}

// tsqlPrereq returns the prerequisite whose title contains title.
func tsqlPrereq(res *Result, title string) *Prerequisite {
	for i := range res.Prerequisites {
		if strings.Contains(res.Prerequisites[i].Title, title) {
			return &res.Prerequisites[i]
		}
	}
	return nil
}

func tsqlRoutine(res *Result, kind, name string) *PGRoutine {
	for i := range res.Plan.Routines {
		if res.Plan.Routines[i].Kind == kind && res.Plan.Routines[i].Name == name {
			return &res.Plan.Routines[i]
		}
	}
	return nil
}

func tsqlTableOf(res *Result, schema, name string) *PGTable {
	for i := range res.Plan.Tables {
		if res.Plan.Tables[i].Schema == schema && res.Plan.Tables[i].Name == name {
			return &res.Plan.Tables[i]
		}
	}
	return nil
}

// The identifier fold must be the one the copier and the worker use, or
// the copy would target tables the DDL did not create.
func TestNormalizeTSQLIdent_MatchesCopier(t *testing.T) {
	for _, s := range []string{"SalesOrderHeader", "Order Details", "ÉTÉ", "already_lower", "MiXeD123"} {
		require.Equal(t, dataxfer.FoldSQLServerIdent(s), normalizeTSQLIdent(s), s)
	}
	require.Equal(t, "customerid", tsqlParamName("@CustomerID"))
}

const tsqlTwoSchemas = `
CREATE SCHEMA sales AUTHORIZATION dbo;
GO
CREATE TABLE dbo.Customers (
    CustomerID int IDENTITY(1,1) NOT NULL,
    Name nvarchar(50) NOT NULL,
    CONSTRAINT PK_Customers PRIMARY KEY CLUSTERED (CustomerID)
);
GO
CREATE TABLE sales.[Order Details] (
    OrderID int NOT NULL,
    CustomerID int NOT NULL,
    Qty smallint NOT NULL CONSTRAINT DF_Qty DEFAULT ((1)),
    CONSTRAINT PK_Order_Details PRIMARY KEY (OrderID),
    CONSTRAINT FK_OD_Customers FOREIGN KEY (CustomerID) REFERENCES dbo.Customers (CustomerID) ON DELETE CASCADE,
    CONSTRAINT CK_Qty CHECK (([Qty]>(0)))
);
GO
`

func TestTSQL_SchemasPreservedAndFolded(t *testing.T) {
	res := tsqlTranslate(t, tsqlTwoSchemas, preserved())
	ddl := res.DDLScript
	require.Contains(t, ddl, `CREATE SCHEMA IF NOT EXISTS "dbo";`)
	require.Contains(t, ddl, `CREATE SCHEMA IF NOT EXISTS "sales";`)
	require.Contains(t, ddl, `SET search_path TO "dbo", "sales", "public";`)
	require.NotContains(t, ddl, `CREATE SCHEMA IF NOT EXISTS "public"`)
	require.Contains(t, ddl, `CREATE TABLE "dbo"."customers" (`)
	require.Contains(t, ddl, `CREATE TABLE "sales"."order details" (`)
	require.Contains(t, ddl, `"qty" smallint NOT NULL DEFAULT 1,`)
	require.Contains(t, ddl, `CONSTRAINT "pk_order_details" PRIMARY KEY ("orderid")`)
	// Column references inside expressions are the rewriter's to fold
	// (cleared here): only the outer parentheses go.
	require.Contains(t, ddl, `CONSTRAINT "ck_qty" CHECK ("Qty" > 0)`)
	require.Contains(t, res.DDLPostCopy,
		`ALTER TABLE "sales"."order details" ADD CONSTRAINT "fk_od_customers" FOREIGN KEY ("customerid") REFERENCES "dbo"."customers" ("customerid") ON DELETE CASCADE NOT VALID;`)
	// A cross-schema FK whose table is migrated raises no
	// missing-reference prerequisite.
	require.Nil(t, tsqlPrereq(res, "Cross-schema FK"))

	od := tsqlTableOf(res, "sales", "order details")
	require.NotNil(t, od)
	require.Equal(t, "sales", od.SourceSchema)
	require.Equal(t, "Order Details", od.SourceName)

	// dedicated_schema: every schema is prefixed by the database name.
	opt := preserved()
	opt.SchemaPrefix = "shop_"
	res = tsqlTranslate(t, tsqlTwoSchemas, opt)
	require.Contains(t, res.DDLScript, `SET search_path TO "shop_dbo", "shop_sales", "public";`)
	require.Contains(t, res.DDLPostCopy, `REFERENCES "shop_dbo"."customers"`)

	// Without preservation everything lands in the target schema.
	res = tsqlTranslate(t, tsqlTwoSchemas, Options{TargetSchema: "mig"})
	require.Contains(t, res.DDLScript, `CREATE SCHEMA IF NOT EXISTS "mig";`)
	require.Contains(t, res.DDLScript, `CREATE TABLE "mig"."order details" (`)
	require.NotContains(t, res.DDLScript, `"sales"`)
}

func TestTSQL_Identity(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.a (id int IDENTITY(1000,5) NOT NULL);
GO
CREATE TABLE dbo.b (id bigint IDENTITY NOT NULL);
GO
CREATE TABLE dbo.c (id int IDENTITY(0,-1) NOT NULL);
GO
CREATE TABLE dbo.d (id int IDENTITY(-100,1) NOT NULL);
GO
CREATE TABLE dbo.e (id decimal(10,0) IDENTITY(1,1) NOT NULL);
GO
`, preserved())
	ddl := res.DDLScript
	require.Contains(t, ddl, `"id" integer GENERATED BY DEFAULT AS IDENTITY (START WITH 1000 INCREMENT BY 5) NOT NULL`)
	require.Contains(t, ddl, `"id" bigint GENERATED BY DEFAULT AS IDENTITY NOT NULL`)
	require.Contains(t, ddl, `"id" integer GENERATED BY DEFAULT AS IDENTITY (START WITH 0 INCREMENT BY -1 MAXVALUE 0) NOT NULL`)
	require.Contains(t, ddl, `"id" integer GENERATED BY DEFAULT AS IDENTITY (START WITH -100 INCREMENT BY 1 MINVALUE -100) NOT NULL`)
	require.Contains(t, ddl, `CREATE TABLE "dbo"."e" (`+"\n"+`  "id" bigint GENERATED BY DEFAULT AS IDENTITY NOT NULL`)
	post := res.DDLPostCopy
	require.Contains(t, post, `SELECT setval(pg_get_serial_sequence('"dbo"."a"', 'id'), GREATEST(1000, COALESCE((SELECT max("id")::bigint FROM "dbo"."a") + (5), 1000)), false);`)
	require.Contains(t, post, `SELECT setval(pg_get_serial_sequence('"dbo"."c"', 'id'), LEAST(0, COALESCE((SELECT min("id")::bigint FROM "dbo"."c") + (-1), 0)), false);`)
}

func TestTSQL_UserTypes(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.t (
    id int NOT NULL,
    n1 dbo.Name,
    n2 [Name] NULL,
    f dbo.Flag CONSTRAINT DF_f DEFAULT ((0)),
    s dbo.Small
);
GO
CREATE PROCEDURE dbo.p @lines dbo.IdList READONLY AS SELECT 1 AS x;
GO
CREATE TYPE dbo.Name FROM nvarchar(50) NOT NULL;
GO
CREATE TYPE dbo.Flag FROM bit NOT NULL;
GO
CREATE TYPE dbo.Small FROM tinyint NULL;
GO
CREATE TYPE dbo.IdList AS TABLE (id int NOT NULL PRIMARY KEY, label dbo.Name);
GO
`, preserved())
	ddl := res.DDLScript
	// Types come before the tables that use them, domains before the
	// composites built on them.
	dName := strings.Index(ddl, `CREATE DOMAIN "dbo"."name" AS varchar(50);`)
	dComp := strings.Index(ddl, `CREATE TYPE "dbo"."idlist" AS ("id" integer, "label" "dbo"."name");`)
	dTable := strings.Index(ddl, `CREATE TABLE "dbo"."t"`)
	require.True(t, dName >= 0 && dComp > dName && dTable > dComp, "order: domain %d composite %d table %d\n%s", dName, dComp, dTable, ddl)
	require.Contains(t, ddl, `CREATE DOMAIN "dbo"."flag" AS boolean;`)
	// The alias' NOT NULL is the default of a column that does not
	// state its own; an explicit NULL wins.
	require.Contains(t, ddl, `"n1" "dbo"."name" NOT NULL,`)
	require.Contains(t, ddl, `"n2" "dbo"."name",`)
	// bit default through a domain, and the base type range CHECK on
	// the column.
	require.Contains(t, ddl, `"f" "dbo"."flag" NOT NULL DEFAULT false,`)
	require.Contains(t, ddl, `"s" "dbo"."small" CHECK ("s" BETWEEN 0 AND 255)`)
	p := tsqlRoutine(res, "procedure", "p")
	require.NotNil(t, p)
	require.Equal(t, `IN "lines" "dbo"."idlist"[]`, p.Signature)
	require.NotEmpty(t, tsqlWarnings(res, tsqlWarnTableType))
}

func TestTSQL_DefaultsAndSequences(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE SEQUENCE dbo.OrderNo AS int START WITH 42 INCREMENT BY 1 MINVALUE 1 MAXVALUE 2147483647 NO CYCLE;
GO
CREATE TABLE sales.o (
    id int NOT NULL CONSTRAINT DF_o_id DEFAULT (NEXT VALUE FOR dbo.OrderNo),
    neg int NULL DEFAULT ((-1)),
    s varchar(10) NULL DEFAULT ('x'),
    b bit NOT NULL DEFAULT ((1)),
    d datetime NULL DEFAULT (getdate())
);
GO
`, preserved())
	ddl := res.DDLScript
	require.Contains(t, ddl, `CREATE SEQUENCE IF NOT EXISTS "dbo"."orderno" AS integer INCREMENT BY 1 MINVALUE 1 MAXVALUE 2147483647 START WITH 42 NO CYCLE;`)
	require.Contains(t, ddl, `"id" integer NOT NULL DEFAULT nextval('"dbo"."orderno"'),`)
	require.Contains(t, ddl, `"neg" integer DEFAULT -1,`)
	require.Contains(t, ddl, `"s" varchar(10) DEFAULT 'x',`)
	require.Contains(t, ddl, `"b" boolean NOT NULL DEFAULT true,`)
	// getdate() is the expression rewriter's (cleared here): passed
	// through, parentheses gone.
	require.Contains(t, ddl, `"d" timestamp(3) DEFAULT GETDATE()`)
	found := false
	for _, e := range res.Explanations {
		if e.Source == "CONSTRAINT DF_o_id DEFAULT" {
			found = true
		}
	}
	require.True(t, found, "named default explained")
}

func TestTSQL_ComputedColumns(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE FUNCTION dbo.ufnZero (@n int) RETURNS varchar(10) AS BEGIN RETURN '0'; END;
GO
CREATE TABLE dbo.c (
    [ID] int NOT NULL,
    Qty int NOT NULL,
    Price decimal(10,2) NOT NULL,
    Code varchar(20) NOT NULL,
    Total AS ([Qty]*[Price]) PERSISTED,
    Up AS (upper([Code])) PERSISTED,
    Label AS (N'Q'+CONVERT(nvarchar(10),[Qty])),
    Acct AS (isnull('AW'+[dbo].[ufnZero]([ID]),'')) PERSISTED,
    Stamp AS (getdate()) PERSISTED
);
GO
`, preserved())
	ddl := res.DDLScript
	require.Contains(t, ddl, `"total" numeric GENERATED ALWAYS AS ("Qty" * "Price") STORED,`)
	require.Contains(t, ddl, `"up" text GENERATED ALWAYS AS (UPPER("Code")) STORED,`)
	// Refused ones are plain columns of their inferred type.
	require.Contains(t, ddl, `"label" text,`)
	require.Contains(t, ddl, `"acct" text,`)
	require.Contains(t, ddl, `"stamp" timestamp(3)`)
	tbl := tsqlTableOf(res, "dbo", "c")
	require.Equal(t, []string{"label", "acct", "stamp"}, tbl.CopiedGenerated)
	// The copy reads the source spelling of every non-generated column.
	require.Equal(t, []string{"ID", "Qty", "Price", "Code", "Label", "Acct", "Stamp"}, tbl.CopyColumns)
	ws := tsqlWarnings(res, "table.computed_not_generated")
	require.Len(t, ws, 3)
	for _, w := range ws {
		require.Equal(t, string(SeverityBlocking), w.Severity)
	}
	require.NotNil(t, tsqlPrereq(res, "Provide the computation of column dbo.c.label"))
	require.Contains(t, ws[1].Message, "function")
}

// With a rewriter that turns ISNULL into coalesce and a text + into ||,
// a PERSISTED column becomes a generated one; a + left unrewritten is
// refused.
func TestTSQL_ComputedColumnsAfterRewrite(t *testing.T) {
	src := `
CREATE TABLE dbo.h (
    id int NOT NULL,
    SubTotal money NOT NULL,
    Tax money NOT NULL,
    TotalDue AS (isnull([SubTotal]+[Tax],(0))) PERSISTED,
    No AS (isnull(N'SO'+CONVERT(nvarchar(23),[id]),N'*** ERROR ***')) PERSISTED
);
GO
`
	res := tsqlTranslate(t, src, preserved())
	require.Len(t, tsqlWarnings(res, "table.computed_not_generated"), 2)

	withTSQLHooks(t)
	RegisterExprRewriter(dialects.KindSQLServer, func(e ast.Expr, ctx *ExprCtx) ast.Expr {
		require.Equal(t, "table.dbo.h", ctx.Object)
		require.Equal(t, "numeric(19,4)", ctx.Columns["subtotal"])
		out, _ := ast.Rewrite(e, func(n ast.Node) ast.Node {
			switch x := n.(type) {
			case *ast.Ident:
				return &ast.Ident{Parts: []string{normalizeTSQLIdent(x.Parts[len(x.Parts)-1])}}
			case *ast.FuncCall:
				if strings.EqualFold(x.Name, "ISNULL") {
					return &ast.FuncCall{Name: "coalesce", Args: x.Args}
				}
			case *ast.BinaryExpr:
				if x.Op == "+" {
					if l, ok := x.Lhs.(*ast.Literal); ok && l.Kind == "string" {
						return &ast.BinaryExpr{Op: "||", Lhs: x.Lhs, Rhs: x.Rhs}
					}
				}
			case *ast.CastExpr:
				return &ast.CastExpr{Expr: x.Expr, Type: &ast.PGType{Name: "varchar(23)"}}
			}
			return n
		}).(ast.Expr)
		return out
	})
	stmts, errs := tsqldialect.Parse(src)
	require.Empty(t, errs)
	res = Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
	require.Empty(t, tsqlWarnings(res, "table.computed_not_generated"))
	require.Contains(t, res.DDLScript, `"totaldue" numeric GENERATED ALWAYS AS (coalesce("subtotal" + "tax", 0)) STORED`)
	require.Contains(t, res.DDLScript, `"no" text GENERATED ALWAYS AS (coalesce('SO' || CAST("id" AS varchar(23)), '*** ERROR ***')) STORED`)
}

func TestTSQL_GeneratedOverride(t *testing.T) {
	opt := preserved()
	opt.GeneratedOverrides = []GeneratedOverride{{Table: "c", Column: "Label", Expr: `'Q' || qty::text`}}
	res := tsqlTranslate(t, `CREATE TABLE dbo.c (qty int NOT NULL, Label AS (N'Q'+CONVERT(nvarchar(10),[qty])));`+"\nGO\n", opt)
	require.Contains(t, res.DDLScript, `"label" text GENERATED ALWAYS AS ('Q' || qty::text) STORED`)
	require.Empty(t, tsqlWarnings(res, "table.computed_not_generated"))
	require.Empty(t, tsqlWarnings(res, "table.generated_override_unused"))
}

func TestTSQL_IndexesAndConstraints(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.c (
    id int NOT NULL,
    email nvarchar(255) NULL,
    country char(3) NOT NULL,
    status varchar(10) NOT NULL,
    CONSTRAINT PK_c PRIMARY KEY NONCLUSTERED (id),
    CONSTRAINT UQ_c_status UNIQUE (status, country)
);
GO
CREATE UNIQUE NONCLUSTERED INDEX UX_c_email ON dbo.c (email) WHERE ([email] IS NOT NULL);
GO
CREATE CLUSTERED INDEX IX_c_country ON dbo.c (country ASC, status DESC) INCLUDE (email);
GO
CREATE INDEX IX_missing ON dbo.nothere (x);
GO
`, preserved())
	post := res.DDLPostCopy
	require.Contains(t, post, `CREATE UNIQUE INDEX "uq_c_status" ON "dbo"."c" ("status","country");`)
	require.Contains(t, post, `CREATE UNIQUE INDEX "ux_c_email" ON "dbo"."c" ("email") WHERE ("email" IS NOT NULL);`)
	require.Contains(t, post, `CREATE INDEX "ix_c_country" ON "dbo"."c" ("country","status" DESC) INCLUDE ("email");`)
	require.NotContains(t, post, "nothere")
	var clustered, skipped bool
	for _, e := range res.Explanations {
		if e.Object == "index.dbo.c.ix_c_country" && e.Source == "CLUSTERED INDEX" {
			clustered = true
		}
		if e.Object == "index.ix_missing" && e.Target == "(skipped)" {
			skipped = true
		}
	}
	require.True(t, clustered)
	require.True(t, skipped)
}

// Real T-SQL scripts add keys, defaults and FKs with ALTER TABLE and
// reference the primary key without a column list.
func TestTSQL_AlterTableConstraints(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE [Person].[Person] ([BusinessEntityID] [int] NOT NULL, [ModifiedDate] [datetime] NOT NULL);
GO
CREATE TABLE [Person].[Phone] ([BusinessEntityID] [int] NOT NULL, [Kind] [tinyint] NOT NULL);
GO
ALTER TABLE [Person].[Person] WITH CHECK ADD CONSTRAINT [PK_Person_BusinessEntityID] PRIMARY KEY CLUSTERED ([BusinessEntityID]);
GO
ALTER TABLE [Person].[Person] ADD CONSTRAINT [DF_Person_ModifiedDate] DEFAULT (getdate()) FOR [ModifiedDate];
GO
ALTER TABLE [Person].[Phone] WITH CHECK ADD CONSTRAINT [FK_Phone_Person] FOREIGN KEY([BusinessEntityID]) REFERENCES [Person].[Person];
GO
ALTER TABLE [Person].[Phone] WITH CHECK ADD CONSTRAINT [CK_Phone_Kind] CHECK (([Kind]>=(1)));
GO
`, preserved())
	ddl := res.DDLScript
	require.Contains(t, ddl, `CONSTRAINT "pk_person_businessentityid" PRIMARY KEY ("businessentityid")`)
	require.Contains(t, ddl, `"modifieddate" timestamp(3) NOT NULL DEFAULT GETDATE()`)
	require.Contains(t, ddl, `CONSTRAINT "ck_phone_kind" CHECK ("Kind" >= 1)`)
	require.Contains(t, res.DDLPostCopy,
		`ADD CONSTRAINT "fk_phone_person" FOREIGN KEY ("businessentityid") REFERENCES "person"."person" ("businessentityid")`)
}

func TestTSQL_Comments(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.t (id int NOT NULL CONSTRAINT PK_t PRIMARY KEY, n int NULL);
GO
CREATE VIEW dbo.v AS SELECT id FROM dbo.t;
GO
CREATE PROCEDURE dbo.p @a int, @b varchar(10) OUTPUT AS SET @b = 'x';
GO
CREATE NONCLUSTERED INDEX IX_t_n ON dbo.t (n);
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'The table', @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'TABLE', @level1name=N't';
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'It''s n', @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'TABLE', @level1name=N't', @level2type=N'COLUMN', @level2name=N'n';
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'The view', @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'VIEW', @level1name=N'v';
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'The proc', @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'PROCEDURE', @level1name=N'p';
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'The key', @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'TABLE', @level1name=N't', @level2type=N'CONSTRAINT', @level2name=N'PK_t';
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'The index', @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'TABLE', @level1name=N't', @level2type=N'INDEX', @level2name=N'IX_t_n';
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'The schema', @level0type=N'SCHEMA', @level0name=N'dbo';
GO
EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'A parameter', @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'PROCEDURE', @level1name=N'p', @level2type=N'PARAMETER', @level2name=N'@a';
GO
`, preserved())
	require.Contains(t, res.DDLScript, `COMMENT ON TABLE "dbo"."t" IS 'The table';`)
	require.Contains(t, res.DDLScript, `COMMENT ON COLUMN "dbo"."t"."n" IS 'It''s n';`)
	require.Contains(t, res.DDLScript, `COMMENT ON SCHEMA "dbo" IS 'The schema';`)
	require.Contains(t, res.Plan.Views[0].DDL, `COMMENT ON VIEW "dbo"."v" IS 'The view';`)
	p := tsqlRoutine(res, "procedure", "p")
	require.Contains(t, p.DDL, `COMMENT ON PROCEDURE "dbo"."p"(integer, INOUT varchar(10)) IS 'The proc';`)
	require.Contains(t, res.DDLPostCopy, `COMMENT ON CONSTRAINT "pk_t" ON "dbo"."t" IS 'The key';`)
	require.Contains(t, res.DDLPostCopy, `COMMENT ON INDEX "dbo"."ix_t_n" IS 'The index';`)
	// The schema comment runs after CREATE SCHEMA.
	require.Less(t, strings.Index(res.DDLScript, `CREATE SCHEMA IF NOT EXISTS "dbo"`), strings.Index(res.DDLScript, `COMMENT ON SCHEMA`))
	found := false
	for _, e := range res.Explanations {
		if strings.HasPrefix(e.Object, "comment.parameter.") && e.Target == "(not migrated)" {
			found = true
		}
	}
	require.True(t, found, "parameter comment reported")
}

func TestTSQL_Triggers(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE sales.Orders (id int NOT NULL PRIMARY KEY, status varchar(10) NULL);
GO
CREATE VIEW dbo.vOrders AS SELECT id, status FROM sales.Orders;
GO
CREATE TRIGGER sales.trg_audit ON sales.Orders AFTER INSERT, UPDATE AS
BEGIN
    SET NOCOUNT ON;
    IF EXISTS (SELECT 1 FROM deleted) RETURN;
END;
GO
CREATE TRIGGER sales.trg_del ON sales.Orders FOR DELETE AS
    PRINT 'deleted';
GO
CREATE TRIGGER dbo.trg_v ON dbo.vOrders INSTEAD OF INSERT AS
    INSERT INTO sales.Orders (id, status) SELECT id, status FROM inserted;
GO
CREATE TRIGGER sales.trg_nodelete ON sales.Orders INSTEAD OF DELETE AS
    THROW 50000, 'no delete', 1;
GO
`, preserved())
	audit := tsqlRoutine(res, "trigger", "trg_audit")
	require.NotNil(t, audit)
	require.Contains(t, audit.DDL, "CREATE TRIGGER \"trg_audit_insert\" AFTER INSERT ON \"sales\".\"orders\"\n  REFERENCING NEW TABLE AS \"inserted\"\n  FOR EACH STATEMENT EXECUTE FUNCTION \"sales\".\"trg_audit_insert_fn\"();")
	require.Contains(t, audit.DDL, "CREATE TRIGGER \"trg_audit_update\" AFTER UPDATE ON \"sales\".\"orders\"\n  REFERENCING NEW TABLE AS \"inserted\" OLD TABLE AS \"deleted\"\n  FOR EACH STATEMENT")
	require.Contains(t, audit.DDL, `CREATE OR REPLACE FUNCTION "sales"."trg_audit_update_fn"() RETURNS trigger`)
	require.Contains(t, audit.DDL, "RETURN NULL;")
	del := tsqlRoutine(res, "trigger", "trg_del")
	require.Contains(t, del.DDL, "CREATE TRIGGER \"trg_del\" AFTER DELETE ON \"sales\".\"orders\"\n  REFERENCING OLD TABLE AS \"deleted\"\n  FOR EACH STATEMENT")
	v := tsqlRoutine(res, "trigger", "trg_v")
	require.Contains(t, v.DDL, "CREATE TRIGGER \"trg_v\" INSTEAD OF INSERT ON \"dbo\".\"vorders\"\n  FOR EACH ROW")
	require.Contains(t, v.DDL, "RETURN NEW;")
	require.NotContains(t, v.DDL, "REFERENCING")
	nd := tsqlRoutine(res, "trigger", "trg_nodelete")
	require.Contains(t, nd.DDL, "CREATE TRIGGER \"trg_nodelete\" BEFORE DELETE ON \"sales\".\"orders\"\n  FOR EACH ROW")
	require.Contains(t, nd.DDL, "RETURN NULL;")
	require.NotEmpty(t, tsqlWarnings(res, "trigger.instead_of_table"))
}

// The routine-body translator receives the header context it needs.
func TestTSQL_RoutineHooksContext(t *testing.T) {
	withTSQLHooks(t)
	seen := map[string]*RoutineCtx{}
	RegisterRoutineBodyTranslator(dialects.KindSQLServer, func(ctx *RoutineCtx) (string, []string, []string) {
		seen[ctx.Name] = ctx
		if ctx.Trigger != nil {
			return "BEGIN\n  RETURN " + ctx.Trigger.ReturnRow + ";\nEND;", nil, []string{"note " + ctx.Name}
		}
		return "BEGIN\n  NULL;\nEND;", nil, nil
	})
	RegisterPreActionsProvider(dialects.KindSQLServer, func(run *HookRun) []string {
		if run.Used("tsql.like") {
			return []string{"CREATE SCHEMA IF NOT EXISTS tsql;"}
		}
		return nil
	})
	RegisterExprRewriter(dialects.KindSQLServer, func(e ast.Expr, ctx *ExprCtx) ast.Expr {
		if b, ok := e.(*ast.BinaryExpr); ok && strings.EqualFold(b.Op, "LIKE") {
			ctx.Run.Use("tsql.like")
		}
		return e
	})
	stmts, errs := tsqldialect.Parse(`
CREATE TABLE sales.T (id int NOT NULL PRIMARY KEY, code char(2) NULL CHECK (code LIKE '[A-Z][A-Z]'));
GO
CREATE PROCEDURE sales.p @Id int, @Out int = 5 OUTPUT AS SET @Out = @Id;
GO
CREATE FUNCTION dbo.f (@x money) RETURNS decimal(10,2) AS BEGIN RETURN @x; END;
GO
CREATE FUNCTION dbo.tvf (@min int) RETURNS @r TABLE (Id int, Label nvarchar(20)) AS BEGIN INSERT @r SELECT id, code FROM sales.T WHERE id > @min; RETURN; END;
GO
CREATE TRIGGER sales.trg ON sales.T AFTER DELETE AS PRINT 'x';
GO
`)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
	require.True(t, strings.HasPrefix(res.Plan.PreActions[0], "CREATE SCHEMA IF NOT EXISTS tsql;"), "%v", res.Plan.PreActions)

	p := seen["p"]
	require.NotNil(t, p)
	require.Equal(t, RoutineProcedure, p.Kind)
	require.Equal(t, "sales", p.SourceSchema)
	require.Equal(t, "sales", p.Schema)
	require.Len(t, p.Params, 2)
	require.Equal(t, RoutineParam{SourceName: "@Out", Name: "out", Type: "integer", Mode: "INOUT", Default: "5", Param: p.Params[1].Param}, p.Params[1])
	require.Equal(t, `IN "id" integer, INOUT "out" integer DEFAULT 5`, tsqlRoutine(res, "procedure", "p").Signature)
	require.Equal(t, "BEGIN\n  NULL;\nEND;", tsqlRoutineBodyOf(t, tsqlRoutine(res, "procedure", "p").DDL))

	f := seen["f"]
	require.Equal(t, RoutineScalarFunction, f.Kind)
	require.Equal(t, "numeric(10,2)", f.Returns)
	require.Contains(t, tsqlRoutine(res, "function", "f").DDL, `CREATE OR REPLACE FUNCTION "dbo"."f"("x" numeric(19,4)) RETURNS numeric(10,2)`)

	tvf := seen["tvf"]
	require.Equal(t, RoutineMultiTVF, tvf.Kind)
	require.Equal(t, "@r", tvf.TableVar)
	require.Equal(t, []RoutineColumn{{SourceName: "Id", Name: "id", Type: "integer"}, {SourceName: "Label", Name: "label", Type: "varchar(20)"}}, tvf.TableCols)
	require.Contains(t, tsqlRoutine(res, "function", "tvf").DDL, `RETURNS TABLE ("id" integer, "label" varchar(20))`)

	trg := seen["trg"]
	require.Equal(t, RoutineTrigger, trg.Kind)
	require.Equal(t, &TriggerInfo{Timing: "AFTER", Events: []string{"DELETE"}, ForEach: "STATEMENT",
		Schema: "sales", Table: "t", OldTable: "deleted", ReturnRow: "NULL"}, trg.Trigger)
	require.Len(t, tsqlWarnings(res, "routine.translated_with_remarks"), 1)
	require.Empty(t, tsqlWarnings(res, "routine.untranslated_construct"))
}

// tsqlRoutineBodyOf extracts the body between the dollar quotes of a
// CREATE FUNCTION / PROCEDURE rendered by the PG writer.
func tsqlRoutineBodyOf(t *testing.T, ddl string) string {
	t.Helper()
	start := strings.Index(ddl, "$body$\n")
	end := strings.LastIndex(ddl, "\n$body$")
	require.True(t, start >= 0 && end > start, ddl)
	return ddl[start+len("$body$\n") : end]
}

func TestTSQL_Routines(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.Orders (OrderID int NOT NULL PRIMARY KEY, CustomerID int NOT NULL, Total money NOT NULL);
GO
CREATE PROCEDURE dbo.GetOrders @cust int, @limit int = 10, @x int AS
BEGIN
    SET NOCOUNT ON;
    IF @limit > 0
        SELECT OrderID, Total FROM dbo.Orders WHERE CustomerID = @cust;
END;
GO
CREATE PROCEDURE dbo.CountOrders @n int OUTPUT AS SELECT @n = COUNT(*) FROM dbo.Orders;
GO
CREATE FUNCTION dbo.OrdersOf (@cust int) RETURNS TABLE AS RETURN (
    SELECT o.OrderID, o.Total AS Amount, CAST(o.CustomerID AS bigint) AS Cust FROM dbo.Orders AS o WHERE o.CustomerID = @cust
);
GO
CREATE FUNCTION dbo.Everything () RETURNS TABLE AS RETURN (SELECT * FROM dbo.Orders);
GO
CREATE FUNCTION dbo.Unresolved () RETURNS TABLE AS RETURN (SELECT COUNT(*) AS n FROM dbo.Orders);
GO
`, preserved())
	get := tsqlRoutine(res, "procedure", "getorders")
	require.Equal(t, `IN "cust" integer, IN "limit" integer DEFAULT 10, IN "x" integer DEFAULT NULL`, get.Signature)
	rs := tsqlWarnings(res, "routine.procedure_returns_resultset")
	require.Len(t, rs, 1, "only GetOrders returns rows: SELECT @n = … assigns")
	require.Equal(t, "procedure.dbo.getorders", rs[0].Object)
	require.NotNil(t, tsqlPrereq(res, "procedures that return result sets"))

	of := tsqlRoutine(res, "function", "ordersof")
	// @cust is renamed: the result has a column "cust" (tsqlRenameClashingParams).
	require.Contains(t, of.DDL, `CREATE OR REPLACE FUNCTION "dbo"."ordersof"("p_cust" integer) RETURNS TABLE ("orderid" integer, "amount" numeric(19,4), "cust" bigint)`)
	require.Contains(t, of.DDL, "LANGUAGE sql STABLE")
	all := tsqlRoutine(res, "function", "everything")
	require.Contains(t, all.DDL, `RETURNS TABLE ("orderid" integer, "customerid" integer, "total" numeric(19,4))`)
	un := tsqlRoutine(res, "function", "unresolved")
	require.Contains(t, un.DDL, "RETURNS SETOF record")
	ws := tsqlWarnings(res, "routine.tvf_columns_unresolved")
	require.Len(t, ws, 1)
	require.Equal(t, string(SeverityBlocking), ws[0].Severity)
}

func TestTSQL_TemporalAndSpecialTypes(t *testing.T) {
	src := `
CREATE TABLE dbo.Emp (
    id int NOT NULL PRIMARY KEY,
    rv rowversion NOT NULL,
    loc geography NULL,
    ValidFrom datetime2 GENERATED ALWAYS AS ROW START HIDDEN NOT NULL,
    ValidTo datetime2 GENERATED ALWAYS AS ROW END HIDDEN NOT NULL,
    PERIOD FOR SYSTEM_TIME (ValidFrom, ValidTo)
) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.EmpHistory));
GO
`
	res := tsqlTranslate(t, src, preserved())
	// The system versioning is emulated: no warning, a history table
	// copied from the source one, versioning triggers after the copy.
	require.Empty(t, tsqlWarnings(res, "table.temporal_not_emulated"))
	require.Nil(t, tsqlPrereq(res, "temporal tables"))
	require.Contains(t, res.DDLScript, `"validfrom" timestamp(6) NOT NULL,`)
	cur := tsqlTableOf(res, "dbo", "emp")
	require.Equal(t, &PGTSQLTemporal{
		RowStart: "validfrom", RowEnd: "validto", RowEndMax: "9999-12-31 23:59:59.999999",
		HistorySchema: "dbo", HistoryTable: "emphistory",
		HistorySourceSchema: "dbo", HistorySourceTable: "EmpHistory", CopyHistory: true,
	}, cur.TSQLTemporal)
	hist := tsqlTableOf(res, "dbo", "emphistory")
	require.Equal(t, "EmpHistory", hist.SourceName)
	require.Empty(t, hist.PK)
	require.Len(t, hist.Columns, 5)
	require.Contains(t, res.DDLScript, `CREATE TABLE "dbo"."emphistory" (`)
	post := res.DDLPostCopy
	require.Contains(t, post, "CREATE TRIGGER \"emp_period_stamp\" BEFORE INSERT OR UPDATE ON \"dbo\".\"emp\"\n  FOR EACH ROW")
	require.Contains(t, post, "CREATE TRIGGER \"emp_period_history\" AFTER UPDATE OR DELETE ON \"dbo\".\"emp\"\n  FOR EACH ROW")
	require.Contains(t, post, `NEW."validfrom" := timezone('UTC', transaction_timestamp());`)
	require.Contains(t, post, `NEW."validto" := CAST('9999-12-31 23:59:59.999999' AS timestamp);`)
	require.Contains(t, post, `INSERT INTO "dbo"."emphistory" ("id", "rv", "loc", "validfrom", "validto") VALUES (OLD."id", OLD."rv", OLD."loc", OLD."validfrom", NEW."validfrom")`)
	require.Contains(t, post, "SQL Server error 13535")
	var hidden bool
	for _, e := range res.Explanations {
		if e.Object == "table.dbo.emp" && strings.Contains(e.Reason, "HIDDEN") {
			hidden = true
		}
	}
	require.True(t, hidden, "HIDDEN period columns explained")
	// rowversion and spatial columns are outside the copier's default
	// list: the table gets an explicit one.
	tbl := tsqlTableOf(res, "dbo", "emp")
	require.Equal(t, []string{"id", "rv", "loc", "ValidFrom", "ValidTo"}, tbl.CopyColumns)
	require.Contains(t, res.DDLScript, `"loc" text,`)
	require.NotNil(t, tsqlPrereq(res, "Install PostGIS"))

	opt := preserved()
	opt.TargetExtensions = []string{"postgis"}
	res = tsqlTranslate(t, src, opt)
	require.Contains(t, res.DDLScript, `"loc" geography,`)
	require.Nil(t, tsqlPrereq(res, "Install PostGIS"))
}

func TestTSQL_ViewsAndNoops(t *testing.T) {
	withTSQLHooks(t)
	var ctxSeen *ExprCtx
	RegisterSelectRewriter(dialects.KindSQLServer, func(s *ast.SelectStmt, ctx *ExprCtx) (*ast.SelectStmt, []string) {
		ctxSeen = ctx
		return s, []string{"STUFF()"}
	})
	stmts, errs := tsqldialect.Parse(`
CREATE TABLE sales.T (id int NOT NULL);
GO
CREATE VIEW sales.[V Top] (Ident) WITH SCHEMABINDING AS SELECT id FROM sales.T;
GO
CREATE SYNONYM dbo.AllT FOR sales.T;
GO
CREATE XML SCHEMA COLLECTION dbo.sc AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"/>';
GO
`)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
	require.Len(t, res.Plan.Views, 1)
	v := res.Plan.Views[0]
	require.Equal(t, "sales", v.Schema)
	require.Equal(t, "v top", v.Name)
	require.Contains(t, v.DDL, `CREATE OR REPLACE VIEW "sales"."v top" ("ident") AS`)
	require.Equal(t, []string{"T"}, v.References)
	require.Equal(t, "view.sales.v top", ctxSeen.Object)
	require.Equal(t, "sales", ctxSeen.DefaultSchema)
	fw := tsqlWarnings(res, "view.functions")
	require.Len(t, fw, 1)
	require.Contains(t, fw[0].Message, "STUFF()")
	var syn, xsc, binding bool
	for _, e := range res.Explanations {
		switch {
		case e.Source == "CREATE SYNONYM":
			syn = true
		case e.Source == "CREATE XML SCHEMA COLLECTION":
			xsc = true
		case strings.Contains(e.Source, "SCHEMABINDING"):
			binding = true
		}
	}
	require.True(t, syn && xsc && binding)
}

// MySQL output does not move: the T-SQL path is a separate loop.
func TestTSQL_OtherKindsUnaffected(t *testing.T) {
	res := Translate([]ast.Stmt{&ast.CreateTable{Name: "T", Columns: []*ast.ColumnDef{{Name: "Id", Type: &ast.IntType{Name: "INT"}}}}},
		Options{TargetSchema: "mig", SourceKind: dialects.KindMySQL})
	require.Contains(t, res.DDLScript, `CREATE SCHEMA IF NOT EXISTS "mig";`+"\n"+`SET search_path TO "mig", "public";`)
	require.Contains(t, res.DDLScript, `CREATE TABLE "mig"."T" (`)
	require.Empty(t, res.Plan.Tables[0].SourceName)
}

// Every T-SQL corpus the repository knows (the e2e fixture, and the
// Microsoft sample scripts when they were fetched) translates without
// panicking, with every table and module accounted for.
func TestTSQL_Corpus(t *testing.T) {
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
			withTSQLHooks(t)
			stmts, _ := tsqldialect.Parse(string(src))
			var tables, modules int
			for _, s := range stmts {
				switch x := s.(type) {
				case *ast.CreateTable:
					if !x.Temporary {
						tables++
					}
					// SQL Server creates the history table of a
					// system-versioned table, which the plan holds.
					if x.SystemVersioned && x.HistoryTable != nil {
						tables++
					}
				case *ast.CreateView, *ast.CreateProcedure, *ast.CreateFunction:
					modules++
				}
			}
			res := Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
			require.Len(t, res.Plan.Tables, tables)
			fns := 0
			for _, r := range res.Plan.Routines {
				if r.Kind != "trigger" {
					fns++
				}
			}
			require.Equal(t, modules, fns+len(res.Plan.Views))
			require.Contains(t, res.DDLScript, `SET search_path TO "dbo"`)
			for _, tbl := range res.Plan.Tables {
				require.Equal(t, normalizeTSQLIdent(tbl.Name), tbl.Name)
				require.NotEmpty(t, tbl.SourceName)
			}
		})
	}
	if ran == 0 {
		t.Skip("no T-SQL corpus on disk")
	}
}

// A non-PERSISTED computed column whose expression is proven immutable
// and identical becomes a stored generated column too (SQL Server
// computes it on read, PG on write: the same value). CONCAT is rewritten
// to an immutable coalesce(…::text, '') || … chain (PG's concat() is
// only STABLE); a CONCAT over money or char(n) stays refused.
func TestTSQL_ComputedNonPersistedWithHooks(t *testing.T) {
	stmts, errs := tsqldialect.Parse(`
CREATE TABLE dbo.c (
    id int NOT NULL,
    code varchar(20) NOT NULL,
    note varchar(20) NULL,
    qty int NOT NULL,
    price money NOT NULL,
    pad char(4) NOT NULL,
    c_label AS (CONCAT(N'Q', qty, N' x ', code, NULL, note)),
    c_is_bulk AS (CASE WHEN qty >= 100 THEN 1 ELSE 0 END),
    c_money AS (CONCAT(N'p', price)),
    c_pad AS (CONCAT(pad, code))
);
GO
`)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "public", SourceKind: dialects.KindSQLServer, PreserveSourceSchemas: true})
	ddl := res.DDLScript
	require.Contains(t, ddl, `"c_label" text GENERATED ALWAYS AS (('Q' || coalesce(CAST("qty" AS text), '') || ' x ' || coalesce(CAST("code" AS text), '') || coalesce(CAST("note" AS text), ''))) STORED`)
	require.Contains(t, ddl, `"c_is_bulk" integer GENERATED ALWAYS AS (CASE WHEN "qty" >= 100 THEN 1 ELSE 0 END) STORED`)
	tbl := tsqlTableOf(res, "dbo", "c")
	require.Equal(t, []string{"c_money", "c_pad"}, tbl.CopiedGenerated)
	ws := tsqlWarnings(res, "table.computed_not_generated")
	require.Len(t, ws, 2)
	require.Contains(t, ws[0].Message, "non-PERSISTED computed column dbo.c.c_money")
	require.Contains(t, ws[0].Message, "CONCAT argument")
	var why string
	for _, e := range res.Explanations {
		if e.Object == "table.dbo.c.c_is_bulk" {
			why = e.Reason
		}
	}
	require.Contains(t, why, "non-PERSISTED computed column")
}

// A system-versioned table whose history table is not named (SQL Server
// then names it MSSQL_TemporalHistoryFor_<id>, which only the catalog
// knows) keeps the not-emulated warning.
func TestTSQL_TemporalWithoutHistoryName(t *testing.T) {
	res := tsqlTranslate(t, `
CREATE TABLE dbo.Emp (
    id int NOT NULL PRIMARY KEY,
    ValidFrom datetime2(2) GENERATED ALWAYS AS ROW START NOT NULL,
    ValidTo datetime2(2) GENERATED ALWAYS AS ROW END NOT NULL,
    PERIOD FOR SYSTEM_TIME (ValidFrom, ValidTo)
) WITH (SYSTEM_VERSIONING = ON);
GO
`, preserved())
	require.Len(t, tsqlWarnings(res, "table.temporal_not_emulated"), 1)
	require.Nil(t, tsqlTableOf(res, "dbo", "emp").TSQLTemporal)
	require.Equal(t, "9999-12-31 23:59:59.99", tsqlPeriodEndMax(2))
}
