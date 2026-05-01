-- ============================================================================
-- squishy — seed Oracle
-- Volume : 1 000 customers, 5 000 orders, 25 000 order_items (3+ batches de 10k)
-- ============================================================================

CONNECT squishy/squishy@localhost:1521/FREEPDB1

ALTER SESSION SET NLS_DATE_FORMAT = 'YYYY-MM-DD HH24:MI:SS';
ALTER SESSION SET NLS_TIMESTAMP_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF';

-- ---------------------------------------------------------------------------
-- Petites tables : bornes et valeurs de diagnostic
-- ---------------------------------------------------------------------------
-- Bornes compatibles avec les types déclarés :
--   c_num_int NUMBER(10,0)  → |v| < 10^10
--   c_num_big NUMBER(19,0)  → |v| < 10^19
--   c_num_huge NUMBER(38,0) → 37 nines max (10^37-1) pour rester côté sûr
INSERT INTO t_numeric (c_num_default, c_num_int, c_num_big, c_num_huge, c_num_dec,
                       c_integer, c_int, c_smallint, c_float,
                       c_binary_float, c_binary_double, c_bool) VALUES
  (12345.6789, -2147483648, -9223372036854775807,
   -9999999999999999999999999999999999999, -9999.9999,
   1, 1, 1, 3.14, 1.5e38f, 1.7e308d, TRUE);

INSERT INTO t_numeric (c_num_default, c_num_int, c_num_big, c_num_huge, c_num_dec,
                       c_integer, c_int, c_smallint, c_float,
                       c_binary_float, c_binary_double, c_bool) VALUES
  (-12345.6789,  2147483647,  9223372036854775807,
    9999999999999999999999999999999999999,  9999.9999,
   -1, -1, -1, -3.14, -1.5e38f, -1.7e308d, FALSE);

INSERT INTO t_numeric (c_num_default) VALUES (0);
INSERT INTO t_numeric (c_num_default) VALUES (NULL);

-- ---------------------------------------------------------------------------
INSERT INTO t_string (c_varchar2, c_varchar2_ch, c_varchar2_by, c_char, c_nchar,
                      c_nvarchar2, c_clob, c_nclob, c_raw, c_blob) VALUES
  ('Héllo, ''world''! "quote"', 'éàü', 'bytes-sens', 'abcdefghij', 'ñ', N'🦑 unicode',
   'long CLOB text here',
   N'long NCLOB text here',
   HEXTORAW('DEADBEEF'),
   HEXTORAW('00112233445566778899AABBCCDDEEFF'));

INSERT INTO t_string (c_varchar2) VALUES ('');
INSERT INTO t_string (c_varchar2) VALUES (NULL);

INSERT INTO t_string_long (c_long) VALUES ('LONG column kept isolated to respect the one-LONG-per-table Oracle rule.');

-- ---------------------------------------------------------------------------
INSERT INTO t_temporal (c_date, c_ts, c_ts3, c_ts_tz, c_ts_ltz, c_ival_ym, c_ival_ds) VALUES
  (DATE '0001-01-01',
   TIMESTAMP '0001-01-01 00:00:00.000000',
   TIMESTAMP '0001-01-01 00:00:00.000',
   TIMESTAMP '2024-06-15 12:34:56.789 +02:00',
   TIMESTAMP '2024-06-15 12:34:56.789',
   INTERVAL '2-3' YEAR TO MONTH,
   INTERVAL '10 11:12:13.456' DAY TO SECOND);

INSERT INTO t_temporal (c_date, c_ts, c_ts3, c_ts_tz, c_ts_ltz) VALUES
  (DATE '9999-12-31',
   TIMESTAMP '9999-12-31 23:59:59.999999',
   TIMESTAMP '9999-12-31 23:59:59.999',
   TIMESTAMP '9999-12-31 23:59:59.999 +14:00',
   TIMESTAMP '9999-12-31 23:59:59.999');

INSERT INTO t_temporal (c_date) VALUES (NULL);

-- ---------------------------------------------------------------------------
INSERT INTO t_json_xml (c_json, c_json_chk, c_xml) VALUES
  ('{"k":"v","n":42,"a":[1,2,3],"null":null}',
   '{"check":"ok"}',
   XMLTYPE('<root><item id="1">alpha</item></root>'));

INSERT INTO t_json_xml (c_json) VALUES ('[]');
INSERT INTO t_json_xml (c_json) VALUES (NULL);

-- ---------------------------------------------------------------------------
INSERT INTO t_defaults (c_int_def, c_str_def, c_expr_def, c_now_def, c_sysdate, c_systs, c_null) VALUES
  (100, 'override', 99, TIMESTAMP '2024-01-01 00:00:00', DATE '2024-01-01', SYSTIMESTAMP, 'explicit');
INSERT INTO t_defaults DEFAULT VALUES;  -- déclenche tous les DEFAULT

-- ---------------------------------------------------------------------------
INSERT INTO t_generated (c_base) VALUES (5);
INSERT INTO t_generated (c_base) VALUES (-3);

-- ---------------------------------------------------------------------------
INSERT INTO t_identity (label) VALUES ('alpha');
INSERT INTO t_identity (label) VALUES ('beta');
INSERT INTO t_identity (label) VALUES ('gamma');

-- ---------------------------------------------------------------------------
INSERT INTO t_check (age, code) VALUES (30, 'ABCDEFGHIJ');
INSERT INTO t_check (age, code) VALUES (0,  '0123456789');

-- ---------------------------------------------------------------------------
INSERT INTO "t_Case_sensitive" ("CamelCol", "reserved") VALUES ('kept', 1);
INSERT INTO "t_Case_sensitive" ("CamelCol", "reserved") VALUES (NULL,   2);

-- ---------------------------------------------------------------------------
-- customers : 1 000 lignes
-- ---------------------------------------------------------------------------
BEGIN
  FOR i IN 1 .. 1000 LOOP
    INSERT INTO customers(email, full_name)
    VALUES ('user' || i || '@example.com', 'User ' || i);
  END LOOP;
  COMMIT;
END;
/

-- ---------------------------------------------------------------------------
-- orders : 5 000 lignes (5 par customer en moyenne)
-- ---------------------------------------------------------------------------
DECLARE
  v_cust_id NUMBER;
  v_status  VARCHAR2(20);
  v_total   NUMBER(12,2);
BEGIN
  FOR i IN 1 .. 5000 LOOP
    v_cust_id := MOD(i - 1, 1000) + 1;
    v_status  := CASE MOD(i, 4)
                   WHEN 0 THEN 'pending'
                   WHEN 1 THEN 'paid'
                   WHEN 2 THEN 'shipped'
                   ELSE        'cancelled'
                 END;
    v_total   := 10.00 + MOD(i, 2000);
    INSERT INTO orders(customer_id, status, total, metadata, created_at)
    VALUES (v_cust_id, v_status, v_total,
            '{"vendor":"acme","line":' || i || '}',
            SYSTIMESTAMP - NUMTODSINTERVAL(MOD(i, 400), 'DAY'));
  END LOOP;
  COMMIT;
END;
/

-- ---------------------------------------------------------------------------
-- order_items : 25 000 lignes (5 par order)
-- ---------------------------------------------------------------------------
DECLARE
  v_order_id NUMBER;
  v_line_no  NUMBER;
BEGIN
  -- 5 000 orders × 5 lignes → 25 000 order_items
  -- line_no = 1 pour i 1..5000, 2 pour 5001..10000, ..., 5 pour 20001..25000
  FOR i IN 1 .. 25000 LOOP
    v_order_id := MOD(i - 1, 5000) + 1;
    v_line_no  := FLOOR((i - 1) / 5000) + 1;
    INSERT INTO order_items(order_id, line_no, sku, quantity, unit_price, is_gift)
    VALUES (v_order_id,
            v_line_no,
            'SKU-' || LPAD(i, 6, '0'),
            MOD(i, 10) + 1,
            ROUND(DBMS_RANDOM.VALUE(1, 100), 2),
            CASE MOD(i, 20) WHEN 0 THEN 1 ELSE 0 END);
    IF MOD(i, 5000) = 0 THEN
      COMMIT;
    END IF;
  END LOOP;
  COMMIT;
END;
/

-- ---------------------------------------------------------------------------
-- Petites tables d'audit pour que les triggers ne laissent pas le schéma vide
-- ---------------------------------------------------------------------------
INSERT INTO orders_audit(order_id, action, at) VALUES (1, 'INS', SYSTIMESTAMP);
INSERT INTO items_deleted(item_id, at)         VALUES (1, SYSTIMESTAMP);
COMMIT;

-- ---------------------------------------------------------------------------
-- Statistiques pour que DBA_TABLES.NUM_ROWS soit fiable quand l'introspection
-- de squishy l'utilise pour dimensionner les batches.
-- ---------------------------------------------------------------------------
BEGIN
  DBMS_STATS.GATHER_SCHEMA_STATS(ownname => USER, estimate_percent => 10);
END;
/
