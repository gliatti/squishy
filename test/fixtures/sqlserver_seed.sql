-- ============================================================================
-- squishy — seed SQL Server (base squishy_sample)
-- Déterministe (aucun RAND/NEWID dans les valeurs comparées) : les comptages
-- e2e sont stables d'un chargement à l'autre.
-- Volumes : 203 t_numeric, 203 t_string, 200 t_binary, 202 t_temporal,
-- 200 t_special, 300 t_computed, 500 Customers, 100 Products,
-- 2000 + 3 Orders, ~5000 Order Details, 300 Employees (+ historique).
-- ============================================================================

USE squishy_sample;
GO
SET NOCOUNT ON;
GO

-- ---------------------------------------------------------------------------
-- t_numeric : bornes, NULLs, puis 200 lignes générées
-- ---------------------------------------------------------------------------
INSERT INTO dbo.t_numeric (c_tinyint, c_smallint, c_int, c_bigint, c_bit, c_decimal, c_decimal_def,
                           c_numeric, c_money, c_smallmoney, c_float, c_float24, c_float53, c_real)
VALUES
 (0,   -32768, -2147483648, -9223372036854775808, 0, -99999999.9999, -999999999999999999,
  -99999999999999999999.9999999999, -922337203685477.5808, -214748.3648,
  -1.79E+308, -3.40E+38, -2.23E-308, -1.18E-38),
 (255,  32767,  2147483647,  9223372036854775807, 1,  99999999.9999,  999999999999999999,
   99999999999999999999.9999999999,  922337203685477.5807,  214748.3647,
   1.79E+308,  3.40E+38,  2.23E-308,  1.18E-38),
 (NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
GO

DECLARE @i int = 1;
WHILE @i <= 200
BEGIN
    INSERT INTO dbo.t_numeric (c_tinyint, c_smallint, c_int, c_bigint, c_bit, c_decimal, c_decimal_def,
                               c_numeric, c_money, c_smallmoney, c_float, c_float24, c_float53, c_real)
    VALUES (@i % 256, @i * 100 - 10000, @i * 1000003, CAST(@i AS bigint) * 10000000019,
            @i % 2, @i * 1.2345, @i * 7,
            CAST(@i AS numeric(30,10)) / 7, @i * 12.3456, @i * 1.5,
            @i * 3.14159265358979, @i * 0.5, 1.0 / @i, @i * 0.25);
    SET @i += 1;
END
GO

-- ---------------------------------------------------------------------------
-- t_string : quotes, unicode, chaînes vides, > 8000 caractères, NULLs
-- ---------------------------------------------------------------------------
INSERT INTO dbo.t_string (c_char, c_varchar, c_varchar_max, c_nchar, c_nvarchar, c_nvarchar_max,
                          c_text, c_ntext, c_varchar_cs, c_sparse)
VALUES
 ('abcdefghij', 'Héllo, ''world''! "quote"', REPLICATE(CAST('x' AS varchar(max)), 10000),
  N'ünïcødé', N'Zoé 🦑 日本語', REPLICATE(CAST(N'é' AS nvarchar(max)), 5000),
  'legacy text', N'legacy ntext ü', 'CaseSensitive', N'sparse value'),
 ('', '', '', N'', N'', N'', '', N'', '', NULL),
 (NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
GO

DECLARE @i int = 1;
WHILE @i <= 200
BEGIN
    INSERT INTO dbo.t_string (c_char, c_varchar, c_varchar_max, c_nchar, c_nvarchar, c_nvarchar_max,
                              c_text, c_ntext, c_varchar_cs, c_sparse)
    VALUES (RIGHT(CONCAT('0000000000', @i), 10),
            CONCAT('row ', @i, ' with trailing spaces   '),
            REPLICATE(CAST(CONCAT('chunk', @i, ';') AS varchar(max)), @i),
            CONCAT(N'n', @i),
            CONCAT(N'Ligne n°', @i, N' — été'),
            REPLICATE(CAST(N'ü' AS nvarchar(max)), @i * 3),
            CONCAT('text ', @i),
            CONCAT(N'ntext ', @i),
            CASE WHEN @i % 2 = 0 THEN 'UPPER' ELSE 'lower' END,
            CASE WHEN @i % 10 = 0 THEN CONCAT(N'sparse ', @i) ELSE NULL END);
    SET @i += 1;
END
GO

-- ---------------------------------------------------------------------------
-- t_binary : 200 lignes (rowversion attribué par le moteur)
-- ---------------------------------------------------------------------------
DECLARE @i int = 1;
WHILE @i <= 200
BEGIN
    INSERT INTO dbo.t_binary (c_binary, c_varbinary, c_varbinary_max, c_image)
    VALUES (CAST(HASHBYTES('MD5', CONCAT('k', @i)) AS binary(16)),
            HASHBYTES('SHA2_256', CONCAT('v', @i)),
            CAST(REPLICATE(CAST('ab' AS varchar(max)), @i * 10) AS varbinary(max)),
            CASE WHEN @i % 5 = 0 THEN NULL ELSE CAST(CONCAT('img', @i) AS varbinary(max)) END);
    SET @i += 1;
END
GO

-- ---------------------------------------------------------------------------
-- t_temporal : bornes + 200 lignes ; c_created* prennent leur DEFAULT
-- ---------------------------------------------------------------------------
INSERT INTO dbo.t_temporal (c_date, c_time, c_time3, c_datetime, c_datetime2, c_datetime2_0,
                            c_smalldatetime, c_datetimeoffset)
VALUES ('0001-01-01', '00:00:00.0000000', '00:00:00.000', '1753-01-01T00:00:00.000',
        '0001-01-01T00:00:00.0000000', '0001-01-01T00:00:00', '1900-01-01T00:00:00',
        '0001-01-01T00:00:00.0000000+00:00'),
       ('9999-12-31', '23:59:59.9999999', '23:59:59.999', '9999-12-31T23:59:59.997',
        '9999-12-31T23:59:59.9999999', '9999-12-31T23:59:59', '2079-06-06T23:59:00',
        '9999-12-31T23:59:59.9999999+00:00');
GO

DECLARE @i int = 1, @base datetime2(7) = '2024-01-01T08:30:00.1234567';
WHILE @i <= 200
BEGIN
    INSERT INTO dbo.t_temporal (c_date, c_time, c_time3, c_datetime, c_datetime2, c_datetime2_0,
                                c_smalldatetime, c_datetimeoffset)
    VALUES (CAST(DATEADD(day, @i, @base) AS date),
            CAST(DATEADD(second, @i * 37, @base) AS time(7)),
            CAST(DATEADD(millisecond, @i * 7, @base) AS time(3)),
            CAST(DATEADD(hour, @i, @base) AS datetime),
            DATEADD(microsecond, @i, @base),
            CAST(DATEADD(minute, @i * 13, @base) AS datetime2(0)),
            CAST(DATEADD(day, -@i, @base) AS smalldatetime),
            TODATETIMEOFFSET(DATEADD(hour, @i, @base), (@i % 27 - 13) * 60));
    SET @i += 1;
END
GO

-- ---------------------------------------------------------------------------
-- t_special : xml, hierarchyid, sql_variant, geography, geometry
-- (c_guid prend NEWID() : seule colonne non déterministe, jamais comparée)
-- ---------------------------------------------------------------------------
DECLARE @i int = 1;
WHILE @i <= 200
BEGIN
    INSERT INTO dbo.t_special (c_xml, c_hierarchy, c_variant, c_geography, c_geometry)
    VALUES (CAST(CONCAT(N'<item id="', @i, N'"><name>item ', @i, N'</name></item>') AS xml),
            hierarchyid::Parse(CONCAT('/', @i % 5 + 1, '/', @i, '/')),
            CASE @i % 3
                WHEN 0 THEN CAST(@i AS sql_variant)
                WHEN 1 THEN CAST(CONCAT(N'variant ', @i) AS sql_variant)
                ELSE CAST(CAST('2024-06-15' AS date) AS sql_variant)
            END,
            geography::Point(-80 + (@i % 160), -170 + ((@i * 3) % 340), 4326),
            geometry::STGeomFromText(CONCAT('POINT(', @i, ' ', @i * 2, ')'), 0));
    SET @i += 1;
END
GO

-- ---------------------------------------------------------------------------
-- t_computed : 300 lignes (colonnes calculées évaluées par le moteur)
-- ---------------------------------------------------------------------------
DECLARE @i int = 1;
WHILE @i <= 300
BEGIN
    INSERT INTO dbo.t_computed (code, qty, unit_price)
    VALUES (CONCAT('code-', @i), (@i * 7) % 150 + 1, @i * 0.75);
    SET @i += 1;
END
GO

-- ---------------------------------------------------------------------------
-- Customers : 500 lignes, emails NULL 1 sur 10 (index filtré)
-- ---------------------------------------------------------------------------
DECLARE @i int = 1;
WHILE @i <= 500
BEGIN
    INSERT INTO dbo.Customers (customer_name, email, zip_code, area_code, country_code, status, is_vip)
    VALUES (CASE WHEN @i % 7 = 0 THEN CONCAT(N'Zoé Ñúñez ', @i) ELSE CONCAT(N'Customer ', RIGHT(CONCAT('000', @i), 4)) END,
            CASE WHEN @i % 10 = 0 THEN NULL ELSE CONCAT(N'customer', @i, N'@example.com') END,
            RIGHT(CONCAT('00000', (@i * 37) % 100000), 5),
            RIGHT(CONCAT('000', (@i * 7) % 1000), 3),
            CASE @i % 4 WHEN 0 THEN 'FRA' WHEN 1 THEN 'DEU' WHEN 2 THEN 'USA' ELSE 'JPN' END,
            CASE WHEN @i % 25 = 0 THEN 'inactive' WHEN @i % 101 = 0 THEN 'BANNED' ELSE 'active' END,
            CASE WHEN @i % 50 = 0 THEN 1 ELSE 0 END);
    SET @i += 1;
END
GO

-- ---------------------------------------------------------------------------
-- Products : 95 lignes directes + 5 via la vue (trigger INSTEAD OF INSERT)
-- ---------------------------------------------------------------------------
DECLARE @i int = 1;
WHILE @i <= 95
BEGIN
    INSERT INTO dbo.Products (sku, product_name, list_price, discontinued)
    VALUES (CONCAT(CHAR(65 + @i % 26), CHAR(65 + (@i / 26) % 26), '-', RIGHT(CONCAT('0000', @i), 4)),
            CONCAT(N'Product ', @i),
            5 + (@i * 13) % 200 + 0.99,
            CASE WHEN @i % 30 = 0 THEN 1 ELSE 0 END);
    SET @i += 1;
END
GO

INSERT INTO dbo.v_products_active (sku, product_name, list_price)
VALUES ('VW-0096', N'View product 96', 19.99),
       ('VW-0097', N'View product 97', 29.99),
       ('VW-0098', N'View product 98', 39.99),
       ('VW-0099', N'View product 99', 49.99),
       ('VW-0100', N'View product 100', 59.99);
GO

-- MERGE : une mise à jour (prix différent) + une ligne inchangée.
EXEC dbo.usp_merge_product @sku = 'VW-0096', @name = N'View product 96 (v2)', @price = 21.50;
EXEC dbo.usp_merge_product @sku = 'VW-0097', @name = N'View product 97', @price = 29.99;
GO

-- ---------------------------------------------------------------------------
-- Orders : 2000 lignes ensemblistes (trigger AFTER INSERT : 1 audit / ligne)
-- ---------------------------------------------------------------------------
WITH n AS (
    SELECT TOP (2000) ROW_NUMBER() OVER (ORDER BY a.object_id, b.object_id) AS i
    FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b
)
INSERT INTO sales.Orders (customer_id, order_date, status, total)
SELECT (i * 13) % 500 + 1,
       DATEADD(minute, i * 97, CAST('2023-01-01T00:00:00' AS datetime2(0))),
       CASE i % 5 WHEN 0 THEN 'shipped' WHEN 1 THEN 'paid' WHEN 2 THEN 'new' WHEN 3 THEN 'shipped' ELSE 'cancelled' END,
       0
FROM n
ORDER BY i;
GO

-- 1 à 4 lignes par commande (~5000 lignes).
WITH l AS (
    SELECT v.line_no FROM (VALUES (1), (2), (3), (4)) AS v (line_no)
)
INSERT INTO sales.[Order Details] (order_id, line_no, product_id, quantity, unit_price, discount)
SELECT o.order_id,
       l.line_no,
       p.product_id,
       (o.order_id + l.line_no) % 10 + 1,
       p.list_price,
       CASE WHEN l.line_no = 4 THEN 0.1 ELSE 0 END
FROM sales.Orders AS o
INNER JOIN l ON l.line_no <= 1 + o.order_id % 4
INNER JOIN dbo.Products AS p ON p.product_id = (o.order_id * 7 + l.line_no * 11) % 100 + 1;
GO

-- Totaux ensemblistes (UPDATE ... FROM ; trigger AFTER UPDATE sans UPDATE(status)).
UPDATE o
SET o.total = x.total
FROM sales.Orders AS o
INNER JOIN (
    SELECT order_id, SUM(quantity * unit_price) AS total
    FROM sales.[Order Details]
    GROUP BY order_id
) AS x ON x.order_id = o.order_id;
GO

-- Changements de statut : trigger AFTER UPDATE avec UPDATE(status).
UPDATE sales.Orders SET status = 'shipped' WHERE order_id % 50 = 2 AND status = 'new';
GO

-- Procédures : création de commandes, lignes par paramètre table, curseur.
DECLARE @order_id int, @updated int, @count int;
EXEC sales.usp_create_order @customer_id = 1, @product_id = 1, @quantity = 3, @order_id = @order_id OUTPUT;
EXEC sales.usp_create_order @customer_id = 2, @product_id = 2, @quantity = 1, @order_id = @order_id OUTPUT;
EXEC sales.usp_create_order @customer_id = 3, @product_id = 4, @quantity = 2, @order_id = @order_id OUTPUT;

DECLARE @lines dbo.OrderLineList;
INSERT INTO @lines (product_id, quantity) VALUES (5, 2), (6, 1), (7, 4);
EXEC sales.usp_add_lines @order_id = @order_id, @lines = @lines;

EXEC sales.usp_recompute_totals @max_orders = 50, @updated = @updated OUTPUT;
EXEC dbo.usp_count_customers @country = 'FRA', @count = @count OUTPUT;
GO

-- INSTEAD OF DELETE : l'erreur est attrapée, la ligne reste.
BEGIN TRY
    DELETE FROM dbo.Customers WHERE customer_id = 1;
END TRY
BEGIN CATCH
    PRINT CONCAT(N'expected error ', ERROR_NUMBER(), N': ', ERROR_MESSAGE());
END CATCH;
GO

-- ---------------------------------------------------------------------------
-- Employees (temporelle) : 300 lignes puis mises à jour -> historique
-- ---------------------------------------------------------------------------
DECLARE @i int = 1;
WHILE @i <= 300
BEGIN
    INSERT INTO dbo.Employees (employee_id, full_name, salary, manager_id)
    VALUES (@i, CONCAT(N'Employee ', @i), 30000 + (@i * 137) % 50000,
            CASE WHEN @i <= 10 THEN NULL ELSE (@i % 10) + 1 END);
    SET @i += 1;
END
GO

UPDATE dbo.Employees SET salary = salary * 1.05 WHERE employee_id % 3 = 0;
GO
UPDATE dbo.Employees SET full_name = CONCAT(full_name, N' (senior)') WHERE employee_id % 30 = 0;
GO

PRINT 'squishy_sample seeded';
GO
