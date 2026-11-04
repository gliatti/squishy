-- ============================================================================
-- squishy — fixture SQL Server (T-SQL) exhaustive
-- Chaque objet ici déclenche au moins une règle du traducteur
-- (voir PLAN-sqlserver-dialect.md : carte des types, visiteurs, routines).
-- Chargée par le one-shot `sqlserver-init` (docker-compose.yml) via
--   sqlcmd -C -b -f 65001 -i sqlserver_sample.sql -i sqlserver_seed.sql
-- et parsée telle quelle par les tests du parser internal/dialects/tsql.
-- Les séparateurs de batch GO sont volontaires : c'est la forme des vrais
-- scripts (Northwind, AdventureWorks) que le parser doit accepter.
-- ============================================================================

SET NOCOUNT ON;
-- Index filtrés, colonnes calculées PERSISTED indexées et types xml exigent
-- ces deux options (sqlcmd met QUOTED_IDENTIFIER à OFF sans -I).
SET ANSI_NULLS ON;
SET QUOTED_IDENTIFIER ON;
GO

USE master;
GO

-- Re-runnable : relancer sqlserver-init recharge une base propre. SINGLE_USER
-- WITH ROLLBACK IMMEDIATE coupe les sessions ouvertes (api, sqlcmd) qui
-- feraient échouer le DROP.
IF DB_ID(N'squishy_sample') IS NOT NULL
BEGIN
    ALTER DATABASE squishy_sample SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
    DROP DATABASE squishy_sample;
END
GO

CREATE DATABASE squishy_sample;
GO

USE squishy_sample;
GO

-- ---------------------------------------------------------------------------
-- Schéma secondaire : les FK et les vues traversent dbo <-> sales
-- (préservation des schémas T-SQL, plan §2.3).
-- ---------------------------------------------------------------------------
CREATE SCHEMA sales AUTHORIZATION dbo;
GO

-- ---------------------------------------------------------------------------
-- Types alias (-> CREATE DOMAIN) et type table (-> composite + warning)
-- ---------------------------------------------------------------------------
CREATE TYPE dbo.Name FROM nvarchar(50) NOT NULL;
GO
CREATE TYPE dbo.Flag FROM bit NOT NULL;
GO
CREATE TYPE dbo.OrderLineList AS TABLE (
    product_id int      NOT NULL,
    quantity   smallint NOT NULL
);
GO

-- ---------------------------------------------------------------------------
-- Séquence (+ DEFAULT NEXT VALUE FOR sur sales.Orders.invoice_no)
-- ---------------------------------------------------------------------------
CREATE SEQUENCE dbo.invoice_seq AS bigint
    START WITH 100000
    INCREMENT BY 1
    MINVALUE 1
    NO CYCLE
    CACHE 50;
GO

-- ---------------------------------------------------------------------------
-- t_numeric — entiers, bit, décimaux, money, flottants ; IDENTITY(1000,5)
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.t_numeric (
    id            int IDENTITY(1000,5) NOT NULL,
    c_tinyint     tinyint,
    c_smallint    smallint,
    c_int         int,
    c_bigint      bigint,
    c_bit         bit,
    c_decimal     decimal(12,4),
    c_decimal_def decimal,
    c_numeric     numeric(30,10),
    c_money       money,
    c_smallmoney  smallmoney,
    c_float       float,
    c_float24     float(24),
    c_float53     float(53),
    c_real        real,
    CONSTRAINT PK_t_numeric PRIMARY KEY CLUSTERED (id)
) ON [PRIMARY];
GO

-- ---------------------------------------------------------------------------
-- t_string — char/varchar/nchar/nvarchar (n | max), text/ntext, COLLATE, SPARSE
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.t_string (
    id                int IDENTITY(1,1) NOT NULL CONSTRAINT PK_t_string PRIMARY KEY,
    c_char            char(10),
    c_varchar         varchar(255),
    c_varchar_max     varchar(max),
    c_nchar           nchar(10),
    c_nvarchar        nvarchar(100),
    c_nvarchar_max    nvarchar(max),
    c_text            text,
    c_ntext           ntext,
    c_varchar_cs      varchar(50) COLLATE Latin1_General_CS_AS,
    c_sparse          nvarchar(40) SPARSE NULL
) TEXTIMAGE_ON [PRIMARY];
GO

-- ---------------------------------------------------------------------------
-- t_binary — binary/varbinary(n | max), image, rowversion (non insérable,
-- exclu de la copie)
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.t_binary (
    id               int IDENTITY(1,1) NOT NULL CONSTRAINT PK_t_binary PRIMARY KEY,
    c_binary         binary(16),
    c_varbinary      varbinary(64),
    c_varbinary_max  varbinary(max),
    c_image          image,
    c_rowversion     rowversion NOT NULL
);
GO

-- ---------------------------------------------------------------------------
-- t_temporal — date/time(n)/datetime/datetime2(n)/smalldatetime/datetimeoffset,
-- DEFAULT nommés sur SYSDATETIME() / GETUTCDATE() / GETDATE()
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.t_temporal (
    id                 int IDENTITY(1,1) NOT NULL CONSTRAINT PK_t_temporal PRIMARY KEY,
    c_date             date,
    c_time             time(7),
    c_time3            time(3),
    c_datetime         datetime,
    c_datetime2        datetime2(7),
    c_datetime2_0      datetime2(0),
    c_smalldatetime    smalldatetime,
    c_datetimeoffset   datetimeoffset(7),
    c_created          datetime2(3) NOT NULL CONSTRAINT DF_t_temporal_created DEFAULT SYSDATETIME(),
    c_created_utc      datetime     NOT NULL CONSTRAINT DF_t_temporal_created_utc DEFAULT GETUTCDATE(),
    c_touched          datetime              CONSTRAINT DF_t_temporal_touched DEFAULT (GETDATE())
);
GO

-- ---------------------------------------------------------------------------
-- t_special — uniqueidentifier ROWGUIDCOL DEFAULT NEWID(), xml, hierarchyid,
-- sql_variant, geography, geometry
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.t_special (
    id           int IDENTITY(1,1) NOT NULL CONSTRAINT PK_t_special PRIMARY KEY,
    c_guid       uniqueidentifier ROWGUIDCOL NOT NULL
                 CONSTRAINT DF_t_special_guid DEFAULT NEWID()
                 CONSTRAINT UQ_t_special_guid UNIQUE,
    c_xml        xml,
    c_hierarchy  hierarchyid,
    c_variant    sql_variant,
    c_geography  geography,
    c_geometry   geometry
);
GO

-- ---------------------------------------------------------------------------
-- t_computed — colonnes calculées PERSISTED et non persistées
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.t_computed (
    id          int IDENTITY(1,1) NOT NULL CONSTRAINT PK_t_computed PRIMARY KEY,
    code        varchar(20) NOT NULL,
    qty         int NOT NULL CONSTRAINT DF_t_computed_qty DEFAULT ((1)),
    unit_price  money NOT NULL,
    c_total     AS (qty * unit_price) PERSISTED,
    c_code_up   AS UPPER(code) PERSISTED,
    c_label     AS (CONCAT(N'Q', qty, N' x ', code)),
    c_is_bulk   AS (CASE WHEN qty >= 100 THEN 1 ELSE 0 END)
);
GO

-- ---------------------------------------------------------------------------
-- audit_log — alimentée par les triggers et dbo.usp_log
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.audit_log (
    audit_id    bigint IDENTITY(1,1) NOT NULL CONSTRAINT PK_audit_log PRIMARY KEY,
    event_name  varchar(50) NOT NULL,
    table_name  sysname     NOT NULL,
    row_key     int         NULL,
    detail      nvarchar(4000) NULL,
    changed_at  datetime2(3) NOT NULL CONSTRAINT DF_audit_log_changed_at DEFAULT SYSDATETIME(),
    changed_by  sysname      NOT NULL CONSTRAINT DF_audit_log_changed_by DEFAULT SUSER_SNAME()
);
GO

-- ---------------------------------------------------------------------------
-- Customers — type alias, CHECK nommés (LIKE à classes [...], UPPER(...) IN),
-- DEFAULT nommés, index filtré + index INCLUDE
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.Customers (
    customer_id    int IDENTITY(1,1) NOT NULL,
    customer_name  dbo.Name,
    email          nvarchar(255) NULL,
    zip_code       char(5) NULL
                   CONSTRAINT CK_Customers_zip CHECK (zip_code LIKE '[0-9][0-9][0-9][0-9][0-9]'),
    area_code      char(3) NULL,
    country_code   char(3) NOT NULL CONSTRAINT DF_Customers_country DEFAULT 'FRA',
    status         varchar(10) NOT NULL CONSTRAINT DF_Customers_status DEFAULT ('active'),
    is_vip         dbo.Flag CONSTRAINT DF_Customers_is_vip DEFAULT ((0)),
    created_at     datetime2(3) NOT NULL CONSTRAINT DF_Customers_created_at DEFAULT SYSDATETIME(),
    CONSTRAINT PK_Customers PRIMARY KEY CLUSTERED (customer_id ASC),
    CONSTRAINT CK_Customers_area CHECK (area_code LIKE '[0-9][0-9][0-9]'),
    CONSTRAINT CK_Customers_status CHECK (UPPER(status) IN ('ACTIVE', 'INACTIVE', 'BANNED')),
    CONSTRAINT CK_Customers_country CHECK (LEN(country_code) = 3)
);
GO

-- Index filtré (email unique quand renseigné : une contrainte UNIQUE
-- n'autoriserait qu'un seul NULL en T-SQL) et index couvrant INCLUDE.
CREATE UNIQUE NONCLUSTERED INDEX UX_Customers_email
    ON dbo.Customers (email)
    WHERE email IS NOT NULL;
GO
CREATE NONCLUSTERED INDEX IX_Customers_country
    ON dbo.Customers (country_code ASC, status DESC)
    INCLUDE (customer_name, email)
    WITH (FILLFACTOR = 90);
GO

-- ---------------------------------------------------------------------------
-- Products — CHECK LIKE '[A-Z][A-Z]-[0-9]...', UNIQUE nommée
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.Products (
    product_id    int IDENTITY(1,1) NOT NULL CONSTRAINT PK_Products PRIMARY KEY,
    sku           varchar(20) NOT NULL CONSTRAINT UQ_Products_sku UNIQUE,
    product_name  dbo.Name,
    list_price    money NOT NULL CONSTRAINT DF_Products_list_price DEFAULT (0.00),
    discontinued  bit NOT NULL CONSTRAINT DF_Products_discontinued DEFAULT 0,
    CONSTRAINT CK_Products_sku CHECK (sku LIKE '[A-Z][A-Z]-[0-9][0-9][0-9][0-9]'),
    CONSTRAINT CK_Products_price CHECK (list_price >= 0)
);
GO

-- ---------------------------------------------------------------------------
-- sales.Orders — FK inter-schémas, DEFAULT NEXT VALUE FOR, ROWGUIDCOL
-- ---------------------------------------------------------------------------
CREATE TABLE sales.Orders (
    order_id     int IDENTITY(1,1) NOT NULL CONSTRAINT PK_Orders PRIMARY KEY,
    invoice_no   bigint NOT NULL CONSTRAINT DF_Orders_invoice_no DEFAULT (NEXT VALUE FOR dbo.invoice_seq),
    customer_id  int NOT NULL,
    order_date   datetime2(0) NOT NULL CONSTRAINT DF_Orders_order_date DEFAULT SYSDATETIME(),
    status       varchar(20) NOT NULL CONSTRAINT DF_Orders_status DEFAULT 'new',
    total        money NOT NULL CONSTRAINT DF_Orders_total DEFAULT 0,
    row_guid     uniqueidentifier ROWGUIDCOL NOT NULL CONSTRAINT DF_Orders_row_guid DEFAULT NEWID(),
    CONSTRAINT UQ_Orders_invoice_no UNIQUE (invoice_no),
    CONSTRAINT FK_Orders_Customers FOREIGN KEY (customer_id)
        REFERENCES dbo.Customers (customer_id),
    CONSTRAINT CK_Orders_total CHECK (total >= 0),
    CONSTRAINT CK_Orders_status CHECK (status IN ('new', 'paid', 'shipped', 'cancelled'))
);
GO
CREATE NONCLUSTERED INDEX IX_Orders_customer_date
    ON sales.Orders (customer_id, order_date DESC)
    INCLUDE (total, status);
GO

-- ---------------------------------------------------------------------------
-- sales.[Order Details] — identifiant avec espace, PK composite, FK CASCADE,
-- colonne calculée non persistée
-- ---------------------------------------------------------------------------
CREATE TABLE sales.[Order Details] (
    [order_id]    int      NOT NULL,
    [line_no]     int      NOT NULL,
    [product_id]  int      NOT NULL,
    [quantity]    smallint NOT NULL CONSTRAINT [DF_Order_Details_quantity] DEFAULT ((1)),
    [unit_price]  money    NOT NULL,
    [discount]    real     NOT NULL CONSTRAINT [DF_Order_Details_discount] DEFAULT ((0)),
    [line_total]  AS (CONVERT(money, [quantity] * [unit_price] * (1.0 - [discount]))),
    CONSTRAINT [PK_Order_Details] PRIMARY KEY CLUSTERED ([order_id], [line_no]),
    CONSTRAINT [FK_Order_Details_Orders] FOREIGN KEY ([order_id])
        REFERENCES [sales].[Orders] ([order_id]) ON DELETE CASCADE,
    CONSTRAINT [FK_Order_Details_Products] FOREIGN KEY ([product_id])
        REFERENCES [dbo].[Products] ([product_id]),
    CONSTRAINT [CK_Order_Details_quantity] CHECK ([quantity] > (0)),
    CONSTRAINT [CK_Order_Details_discount] CHECK ([discount] >= (0) AND [discount] <= (1))
);
GO
CREATE NONCLUSTERED INDEX [IX_Order_Details_product]
    ON [sales].[Order Details] ([product_id]);
GO

-- ---------------------------------------------------------------------------
-- Employees — table temporelle system-versioned + table d'historique
-- (prérequis bloquant côté squishy en v1, plan §2.7)
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.Employees (
    employee_id  int NOT NULL CONSTRAINT PK_Employees PRIMARY KEY,
    full_name    nvarchar(100) NOT NULL,
    salary       decimal(10,2) NOT NULL,
    manager_id   int NULL CONSTRAINT FK_Employees_manager REFERENCES dbo.Employees (employee_id),
    valid_from   datetime2(7) GENERATED ALWAYS AS ROW START NOT NULL,
    valid_to     datetime2(7) GENERATED ALWAYS AS ROW END NOT NULL,
    PERIOD FOR SYSTEM_TIME (valid_from, valid_to)
)
WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.EmployeesHistory));
GO

-- ---------------------------------------------------------------------------
-- Extended properties (MS_Description -> COMMENT ON)
-- ---------------------------------------------------------------------------
EXEC sys.sp_addextendedproperty
    @name = N'MS_Description', @value = N'Customers of the shop (one row per account).',
    @level0type = N'SCHEMA', @level0name = N'dbo',
    @level1type = N'TABLE',  @level1name = N'Customers';
GO
EXEC sys.sp_addextendedproperty
    @name = N'MS_Description', @value = N'Five-digit postal code, digits only.',
    @level0type = N'SCHEMA', @level0name = N'dbo',
    @level1type = N'TABLE',  @level1name = N'Customers',
    @level2type = N'COLUMN', @level2name = N'zip_code';
GO
EXECUTE [sys].[sp_addextendedproperty] N'MS_Description', N'One line per ordered product; line_total is computed.', N'SCHEMA', [sales], N'TABLE', [Order Details], NULL, NULL;
GO

-- ---------------------------------------------------------------------------
-- Synonyme (-> NoopStmt expliqué)
-- ---------------------------------------------------------------------------
CREATE SYNONYM dbo.AllOrders FOR sales.Orders;
GO

-- ---------------------------------------------------------------------------
-- Fonctions : scalaire, TVF inline, TVF multi-instructions
-- ---------------------------------------------------------------------------
CREATE FUNCTION dbo.fn_full_price (
    @price    money,
    @qty      int,
    @discount real = 0
)
RETURNS money
WITH SCHEMABINDING
AS
BEGIN
    DECLARE @r money;
    SET @r = @price * @qty * (1 - @discount);
    IF @r < 0
        SET @r = 0;
    RETURN @r;
END;
GO

CREATE FUNCTION sales.fn_orders_of (@customer_id int)
RETURNS TABLE
AS
RETURN (
    SELECT o.order_id, o.order_date, o.status, o.total
    FROM sales.Orders AS o
    WHERE o.customer_id = @customer_id
);
GO

CREATE FUNCTION sales.fn_customer_summary (@min_total money)
RETURNS @result TABLE (
    customer_id  int PRIMARY KEY,
    order_count  int NOT NULL,
    spent        money NOT NULL
)
AS
BEGIN
    INSERT INTO @result (customer_id, order_count, spent)
    SELECT o.customer_id, COUNT(*), ISNULL(SUM(o.total), 0)
    FROM sales.Orders AS o WITH (NOLOCK)
    GROUP BY o.customer_id;

    DELETE FROM @result WHERE spent < @min_total;
    RETURN;
END;
GO

-- ---------------------------------------------------------------------------
-- Vues : WITH SCHEMABINDING ; CTE + TOP + CROSS/OUTER APPLY ; vue avec trigger
-- ---------------------------------------------------------------------------
CREATE VIEW dbo.v_customer_names
WITH SCHEMABINDING
AS
SELECT customer_id, customer_name, country_code
FROM dbo.Customers
WHERE status = 'active';
GO

CREATE VIEW sales.v_top_customers
AS
WITH totals AS (
    SELECT o.customer_id, SUM(o.total) AS spent, COUNT(*) AS order_count
    FROM sales.Orders AS o
    WHERE o.status <> 'cancelled'
    GROUP BY o.customer_id
)
SELECT TOP (10)
       c.customer_id,
       c.customer_name,
       t.spent,
       t.order_count,
       last_o.order_date AS last_order_date,
       ISNULL(s.spent, 0) AS spent_over_100
FROM dbo.Customers AS c
JOIN totals AS t ON t.customer_id = c.customer_id
CROSS APPLY (
    SELECT TOP (1) o2.order_date
    FROM sales.Orders AS o2
    WHERE o2.customer_id = c.customer_id
    ORDER BY o2.order_date DESC
) AS last_o
OUTER APPLY (
    SELECT SUM(f.total) AS spent
    FROM sales.fn_orders_of(c.customer_id) AS f
    WHERE f.total > 100
) AS s
ORDER BY t.spent DESC;
GO

CREATE VIEW dbo.v_products_active
AS
SELECT product_id, sku, product_name, list_price
FROM dbo.Products
WHERE discontinued = 0;
GO

-- ---------------------------------------------------------------------------
-- Procédures
-- ---------------------------------------------------------------------------
CREATE PROCEDURE dbo.usp_log
    @event  varchar(50),
    @table  sysname,
    @key    int = NULL,
    @detail nvarchar(4000) = NULL
AS
BEGIN
    SET NOCOUNT ON;
    INSERT INTO dbo.audit_log (event_name, table_name, row_key, detail)
    VALUES (@event, @table, @key, @detail);
END;
GO

-- OUTPUT param, DECLARE/SET/SELECT @v =, IF/ELSE, TRY/CATCH + THROW,
-- RAISERROR, transaction, SCOPE_IDENTITY(), @@ROWCOUNT, EXEC, PRINT.
CREATE PROCEDURE sales.usp_create_order
    @customer_id int,
    @product_id  int,
    @quantity    smallint,
    @order_id    int OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    SET XACT_ABORT ON;

    DECLARE @price money, @rows int = 0;

    SELECT @price = p.list_price
    FROM dbo.Products AS p
    WHERE p.product_id = @product_id AND p.discontinued = 0;

    IF @price IS NULL
    BEGIN
        RAISERROR(N'Unknown or discontinued product %d', 16, 1, @product_id);
        RETURN -1;
    END
    ELSE IF @quantity <= 0
    BEGIN
        RAISERROR(N'Quantity must be positive', 16, 1);
        RETURN -2;
    END

    BEGIN TRY
        BEGIN TRANSACTION;

        INSERT INTO sales.Orders (customer_id, status, total)
        VALUES (@customer_id, 'new', dbo.fn_full_price(@price, @quantity, 0));
        SET @order_id = SCOPE_IDENTITY();

        INSERT INTO sales.[Order Details] (order_id, line_no, product_id, quantity, unit_price)
        VALUES (@order_id, 1, @product_id, @quantity, @price);
        SET @rows = @@ROWCOUNT;

        EXEC dbo.usp_log 'order_created', N'sales.Orders', @order_id;

        COMMIT TRANSACTION;
    END TRY
    BEGIN CATCH
        IF @@TRANCOUNT > 0
            ROLLBACK TRANSACTION;
        THROW;
    END CATCH;

    PRINT CONCAT(N'order ', @order_id, N' created, lines: ', @rows);
    RETURN 0;
END;
GO

-- Paramètre table READONLY (type table dbo.OrderLineList).
CREATE PROCEDURE sales.usp_add_lines
    @order_id int,
    @lines    dbo.OrderLineList READONLY
AS
BEGIN
    SET NOCOUNT ON;
    DECLARE @next_line int;
    SELECT @next_line = ISNULL(MAX(line_no), 0) FROM sales.[Order Details] WHERE order_id = @order_id;

    INSERT INTO sales.[Order Details] (order_id, line_no, product_id, quantity, unit_price)
    SELECT @order_id,
           @next_line + ROW_NUMBER() OVER (ORDER BY l.product_id),
           l.product_id,
           l.quantity,
           p.list_price
    FROM @lines AS l
    INNER JOIN dbo.Products AS p ON p.product_id = l.product_id;

    UPDATE o
    SET o.total = x.total
    FROM sales.Orders AS o
    INNER JOIN (
        SELECT d.order_id, SUM(d.quantity * d.unit_price) AS total
        FROM sales.[Order Details] AS d
        WHERE d.order_id = @order_id
        GROUP BY d.order_id
    ) AS x ON x.order_id = o.order_id;
END;
GO

-- Curseur FETCH NEXT ... @@FETCH_STATUS, WHILE + BREAK/CONTINUE.
CREATE PROCEDURE sales.usp_recompute_totals
    @max_orders int = 100,
    @updated    int OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    DECLARE @id int, @sum money;
    SET @updated = 0;

    DECLARE order_cur CURSOR LOCAL FAST_FORWARD FOR
        SELECT order_id FROM sales.Orders ORDER BY order_id;

    OPEN order_cur;
    FETCH NEXT FROM order_cur INTO @id;
    WHILE @@FETCH_STATUS = 0
    BEGIN
        SELECT @sum = SUM(quantity * unit_price)
        FROM sales.[Order Details]
        WHERE order_id = @id;

        IF @sum IS NULL
        BEGIN
            FETCH NEXT FROM order_cur INTO @id;
            CONTINUE;
        END

        UPDATE sales.Orders SET total = @sum WHERE order_id = @id AND total <> @sum;
        SET @updated = @updated + @@ROWCOUNT;

        IF @updated >= @max_orders
            BREAK;

        FETCH NEXT FROM order_cur INTO @id;
    END

    CLOSE order_cur;
    DEALLOCATE order_cur;
END;
GO

-- MERGE avec OUTPUT $action.
CREATE PROCEDURE dbo.usp_merge_product
    @sku   varchar(20),
    @name  nvarchar(50),
    @price money
AS
BEGIN
    SET NOCOUNT ON;
    MERGE dbo.Products AS tgt
    USING (SELECT @sku AS sku, @name AS product_name, @price AS list_price) AS src
        ON tgt.sku = src.sku
    WHEN MATCHED AND tgt.list_price <> src.list_price THEN
        UPDATE SET tgt.list_price = src.list_price, tgt.product_name = src.product_name
    WHEN NOT MATCHED BY TARGET THEN
        INSERT (sku, product_name, list_price) VALUES (src.sku, src.product_name, src.list_price)
    OUTPUT $action AS merge_action, inserted.product_id;
END;
GO

-- SQL dynamique : sp_executesql avec paramètre OUTPUT, EXEC(@sql).
CREATE PROCEDURE dbo.usp_count_customers
    @country char(3),
    @count   int OUTPUT
AS
BEGIN
    SET NOCOUNT ON;
    DECLARE @sql nvarchar(400) =
        N'SELECT @c = COUNT(*) FROM dbo.Customers WHERE country_code = @cc';
    EXEC sp_executesql @sql, N'@cc char(3), @c int OUTPUT', @cc = @country, @c = @count OUTPUT;

    DECLARE @msg nvarchar(200);
    SET @msg = N'PRINT ''' + @country + N': ' + CAST(@count AS nvarchar(10)) + N' customers''';
    EXEC (@msg);
END;
GO

-- ---------------------------------------------------------------------------
-- Triggers
-- ---------------------------------------------------------------------------
-- AFTER INSERT, UPDATE multi-événements : inserted/deleted, UPDATE(col),
-- @@ROWCOUNT en première instruction.
CREATE TRIGGER sales.trg_orders_audit
ON sales.Orders
AFTER INSERT, UPDATE
AS
BEGIN
    IF @@ROWCOUNT = 0
        RETURN;
    SET NOCOUNT ON;

    IF NOT EXISTS (SELECT 1 FROM deleted)
    BEGIN
        INSERT INTO dbo.audit_log (event_name, table_name, row_key, detail)
        SELECT 'order_inserted', N'sales.Orders', i.order_id, CONCAT(N'customer ', i.customer_id)
        FROM inserted AS i;
        RETURN;
    END

    IF UPDATE(status)
    BEGIN
        INSERT INTO dbo.audit_log (event_name, table_name, row_key, detail)
        SELECT 'status_changed', N'sales.Orders', i.order_id, d.status + N' -> ' + i.status
        FROM inserted AS i
        INNER JOIN deleted AS d ON d.order_id = i.order_id
        WHERE i.status <> d.status;
    END
END;
GO

-- INSTEAD OF DELETE qui lève une erreur (suppression interdite).
CREATE TRIGGER dbo.trg_customers_no_delete
ON dbo.Customers
INSTEAD OF DELETE
AS
BEGIN
    SET NOCOUNT ON;
    IF EXISTS (SELECT 1 FROM deleted)
        THROW 50001, N'Customers cannot be deleted; set status = ''banned'' instead.', 1;
END;
GO

-- Trigger sur une vue (INSTEAD OF INSERT).
CREATE TRIGGER dbo.trg_v_products_active_insert
ON dbo.v_products_active
INSTEAD OF INSERT
AS
BEGIN
    SET NOCOUNT ON;
    INSERT INTO dbo.Products (sku, product_name, list_price, discontinued)
    SELECT i.sku, i.product_name, ISNULL(i.list_price, 0), 0
    FROM inserted AS i;
    PRINT 'products inserted through dbo.v_products_active';
END;
GO
