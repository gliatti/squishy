-- ============================================================================
-- squishy — seed MySQL
-- Volume : 1k customers, 5k orders, 25k order_items (3+ batches de 10k).
-- ============================================================================

USE sakila;

-- ---------------------------------------------------------------------------
-- Petites tables : valeurs aux bornes
-- ---------------------------------------------------------------------------
INSERT INTO t_numeric (c_tinyint, c_tinyint_u, c_tinyint_bool, c_smallint, c_smallint_u,
                       c_mediumint, c_mediumint_u, c_int, c_int_u, c_bigint, c_bigint_u,
                       c_decimal, c_decimal_default, c_numeric, c_float, c_float_prec,
                       c_double, c_real, c_zerofill, c_bit1, c_bit8) VALUES
 (-128, 0,   0,   -32768, 0,      -8388608, 0,        -2147483648, 0,          -9223372036854775808, 0,                     -1234.5678, 9999999999, 1234567890.1234567890, 3.14, 1.2345, 1.7976931348623157e308, 1.23, 42, b'0', b'10101010'),
 ( 127, 255, 1,    32767, 65535,   8388607, 16777215,  2147483647, 4294967295,  9223372036854775807, 18446744073709551615,  1234.5678, -9999999999, -1234567890.1234567890, -3.14, -1.2345, -1.7976931348623157e308, -1.23, 1, b'1', b'01010101'),
 (  0, 0,   0,        0, 0,             0, 0,                  0, 0,                             0, 0,                         0,          0,                  0,    0,      0,                      0,     0, 0, b'0', b'00000000'),
 ( NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);

INSERT INTO t_string (c_char, c_varchar, c_varchar_bin, c_tinytext, c_text, c_mediumtext, c_longtext,
                      c_tinyblob, c_blob, c_mediumblob, c_longblob, c_binary, c_varbinary,
                      c_enum, c_set, c_json) VALUES
 ('abcdefghij', 'Héllo, \'world\'! "quote"', 'bin-sens', 'tt', 't', 'mt', 'lt',
  X'00010203', X'04050607', X'08090A0B', X'0C0D0E0F',
  X'00112233445566778899AABBCCDDEEFF', X'DEADBEEF',
  'm', 'a,c', JSON_OBJECT('k','v','n',42,'a',JSON_ARRAY(1,2,3),'null',NULL)),
 ('',           '',                       '',         '',   '',  '',   '',    '',     '',     '',     '',
  X'00000000000000000000000000000000', X'',
  's', '', JSON_ARRAY()),
 ('🦑éàü',           ' trailing   ',  'éàü',      NULL, NULL, NULL, NULL, NULL,   NULL,   NULL,   NULL,
  NULL, NULL,
  'xl', 'a,b,c,d', JSON_OBJECT('emoji','🦑','esc',CONCAT('a', CHAR(0), 'b'))),
 (NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);

INSERT INTO t_temporal (c_date, c_time, c_time_frac, c_datetime, c_datetime_frac,
                        c_timestamp, c_year) VALUES
 ('1000-01-01', '-838:59:59', '00:00:00.000000', '1000-01-01 00:00:00', '1000-01-01 00:00:00.000', NULL, 1901),
 ('9999-12-31', '838:59:59',  '23:59:59.999999', '9999-12-31 23:59:59', '9999-12-31 23:59:59.999', '2024-06-01 12:00:00', 2155),
 ('2024-06-15', '12:34:56',   '12:34:56.123456', '2024-06-15 12:34:56', '2024-06-15 12:34:56.789', NULL, 2024),
 (NULL, NULL, NULL, NULL, NULL, NULL, NULL);

INSERT INTO t_spatial (c_geom, c_point) VALUES
 (ST_GeomFromText('POINT(1 1)'), ST_GeomFromText('POINT(2 3)')),
 (NULL, NULL);

INSERT INTO t_defaults (c_int_def, c_str_def, c_expr_def, c_now_def, c_uuid_def, c_nullable) VALUES
 (DEFAULT, DEFAULT, DEFAULT, DEFAULT, DEFAULT, NULL),
 (99, 'override', 0, '2024-01-01 00:00:00.000', '00000000-0000-0000-0000-000000000000', 'set');

INSERT INTO t_generated (c_base) VALUES (1),(2),(3),(10),(-5);

INSERT INTO t_identity (label) VALUES ('first'),('second'),('third');

INSERT INTO t_check (age, code) VALUES (0, '0123456789'), (42, 'ABCDEFGHIJ'), (NULL, NULL);

INSERT INTO `t_Case_sensitive` (`CamelCol`, `reserved`) VALUES ('mixed Case', 1),(NULL, NULL);

-- ---------------------------------------------------------------------------
-- customers : 1 000 lignes avec un NULL tactique
-- ---------------------------------------------------------------------------
DELIMITER //
CREATE PROCEDURE _seed_customers()
BEGIN
  DECLARE i INT DEFAULT 1;
  WHILE i <= 1000 DO
    INSERT INTO customers (email, full_name, created_at)
    VALUES (CONCAT('user', i, '@example.com'),
            CONCAT('Name ', i, CASE WHEN i % 7 = 0 THEN ' — héros 🦑' ELSE '' END),
            DATE_SUB(NOW(), INTERVAL i DAY));
    SET i = i + 1;
  END WHILE;
END //
DELIMITER ;
CALL _seed_customers();
DROP PROCEDURE _seed_customers;

-- ---------------------------------------------------------------------------
-- orders : 5 000 lignes, 4 statuts, metadata JSON varié
-- ---------------------------------------------------------------------------
DELIMITER //
CREATE PROCEDURE _seed_orders()
BEGIN
  DECLARE i INT DEFAULT 1;
  DECLARE s VARCHAR(16);
  WHILE i <= 5000 DO
    SET s = ELT((i % 4) + 1, 'pending','paid','shipped','cancelled');
    INSERT INTO orders (customer_id, status, total, metadata, created_at)
    VALUES ((i % 1000) + 1,
            s,
            ROUND(RAND(i) * 1000, 2),
            JSON_OBJECT('vendor', CONCAT('v', i % 20),
                        'items',  i % 10,
                        'flags',  JSON_ARRAY(i % 2, (i+1) % 2),
                        'note',   CASE WHEN i % 13 = 0 THEN NULL ELSE CONCAT('o', i) END),
            DATE_SUB(NOW(), INTERVAL (i * 13 MOD 365) DAY));
    SET i = i + 1;
  END WHILE;
END //
DELIMITER ;
CALL _seed_orders();
DROP PROCEDURE _seed_orders;

-- ---------------------------------------------------------------------------
-- order_items : 25 000 lignes (≥ 3 batches de 10k)
-- ---------------------------------------------------------------------------
DELIMITER //
CREATE PROCEDURE _seed_items()
BEGIN
  DECLARE i INT DEFAULT 1;
  WHILE i <= 25000 DO
    -- order_id cycles 1..5000 so every order has ~5 items; line_no is
    -- ((i-1) DIV 5000) * ... wait we just need it unique per order_id.
    -- With i going 1..25000 and order_id = ((i-1) % 5000) + 1, the "wrap
    -- number" ((i-1) DIV 5000) is 0..4 so line_no = wrap+1 ∈ 1..5 per order
    -- gives (order_id, line_no) unique.
    INSERT INTO order_items (order_id, line_no, sku, quantity, unit_price, is_gift)
    VALUES (((i-1) MOD 5000) + 1,
            ((i-1) DIV 5000) + 1,
            CONCAT('SKU-', LPAD(i, 6, '0')),
            (i % 9) + 1,
            ROUND(RAND(i) * 100 + 1, 2),
            i % 17 = 0);
    SET i = i + 1;
  END WHILE;
END //
DELIMITER ;
CALL _seed_items();
DROP PROCEDURE _seed_items;
