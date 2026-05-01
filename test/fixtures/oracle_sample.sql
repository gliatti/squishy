-- ============================================================================
-- squishy — fixture Oracle exhaustive (23ai)
-- Chaque objet ici déclenche au moins une règle du traducteur.
-- Exécuté par gvenzl/oracle-free:23-slim-faststart dans le conteneur
-- oracle-sample sous l'utilisateur APP_USER (PDB FREEPDB1, schéma SQUISHY).
-- ============================================================================

-- gvenzl 23-slim-faststart exécute les scripts init comme SYSDBA dans
-- CDB$ROOT ; il faut se reconnecter explicitement en APP_USER dans FREEPDB1
-- pour que les CREATE TABLE/TRIGGER landent dans le schéma SQUISHY.
CONNECT squishy/squishy@localhost:1521/FREEPDB1

ALTER SESSION SET NLS_DATE_FORMAT = 'YYYY-MM-DD HH24:MI:SS';
ALTER SESSION SET NLS_TIMESTAMP_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF';
ALTER SESSION SET NLS_TIMESTAMP_TZ_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF TZR';

-- ---------------------------------------------------------------------------
-- t_numeric — NUMBER précision/scale, types numériques binaires, BOOLEAN 23c
-- ---------------------------------------------------------------------------
CREATE TABLE t_numeric (
  id                 NUMBER(19)       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  c_num_default      NUMBER,                        -- NUMBER sans précision
  c_num_int          NUMBER(10,0),                  -- entier  p <= 9  → integer
  c_num_big          NUMBER(19,0),                  -- entier  p >= 10 → bigint
  c_num_huge         NUMBER(38,0),                  -- entier  p > 18  → numeric(38,0)
  c_num_dec          NUMBER(12,4),                  -- décimal → numeric(12,4)
  c_integer          INTEGER,                       -- alias de NUMBER(38)
  c_int              INT,
  c_smallint         SMALLINT,
  c_float            FLOAT(126),                    -- = NUMBER, precision binaire
  c_binary_float     BINARY_FLOAT,                  -- IEEE single  → real
  c_binary_double    BINARY_DOUBLE,                 -- IEEE double  → double precision
  c_bool             BOOLEAN                        -- natif 23c    → boolean
);

-- ---------------------------------------------------------------------------
-- t_string — VARCHAR2 BYTE/CHAR, CHAR, NCHAR, NVARCHAR2, CLOB/NCLOB, RAW
-- Note: Oracle limite à UNE seule colonne LONG[RAW] par table — on garde
--       c_long_raw ici et on exerce LONG (texte) dans une table dédiée.
-- ---------------------------------------------------------------------------
CREATE TABLE t_string (
  id             NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  c_varchar2     VARCHAR2(255),
  c_varchar2_ch  VARCHAR2(50 CHAR),
  c_varchar2_by  VARCHAR2(50 BYTE),
  c_char         CHAR(10),
  c_nchar        NCHAR(10),
  c_nvarchar2    NVARCHAR2(255),
  c_clob         CLOB,
  c_nclob        NCLOB,
  c_raw          RAW(128),
  c_long_raw     LONG RAW,                           -- → bytea (deprecated)
  c_blob         BLOB,
  c_rowid        ROWID,                              -- → text + warning
  c_urowid       UROWID(100)                         -- → text + warning
);

CREATE TABLE t_string_long (
  id      NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  c_long  LONG                                       -- → text (deprecated)
);

-- ---------------------------------------------------------------------------
-- t_temporal — DATE Oracle (avec heure !), TIMESTAMP[TZ][LOCAL], INTERVAL
-- ---------------------------------------------------------------------------
CREATE TABLE t_temporal (
  id               NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  c_date           DATE,                              -- → timestamp(0)
  c_ts             TIMESTAMP,                         -- → timestamp(6)
  c_ts3            TIMESTAMP(3),                      -- → timestamp(3)
  c_ts_tz          TIMESTAMP WITH TIME ZONE,          -- → timestamptz
  c_ts_ltz         TIMESTAMP WITH LOCAL TIME ZONE,    -- → timestamptz + note
  c_ival_ym        INTERVAL YEAR(3) TO MONTH,         -- → interval
  c_ival_ds        INTERVAL DAY(2) TO SECOND(6)       -- → interval
);

-- ---------------------------------------------------------------------------
-- t_json_xml — JSON natif 23c, XMLTYPE, VECTOR (23ai)
-- ---------------------------------------------------------------------------
CREATE TABLE t_json_xml (
  id           NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  c_json       JSON,                                 -- natif 23c → jsonb
  c_json_chk   CLOB CONSTRAINT ck_json_chk CHECK (c_json_chk IS JSON),
  c_xml        XMLTYPE,                              -- → xml
  c_vec        VECTOR(384, FLOAT32)                  -- 23ai → vector(384) (pgvector)
);

-- ---------------------------------------------------------------------------
-- t_bfile — BFILE (warning: pas d'équivalent PG)
-- ---------------------------------------------------------------------------
CREATE TABLE t_bfile (
  id         NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  c_bfile    BFILE                                   -- → text + warning
);

-- ---------------------------------------------------------------------------
-- t_defaults — DEFAULT littéraux, expressions, fonctions, DEFAULT ON NULL
-- ---------------------------------------------------------------------------
CREATE TABLE t_defaults (
  id          NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  c_int_def   NUMBER(10)     DEFAULT 42         NOT NULL,
  c_str_def   VARCHAR2(20)   DEFAULT 'hello',
  c_expr_def  NUMBER         DEFAULT 10 + 5,
  c_now_def   TIMESTAMP      DEFAULT CURRENT_TIMESTAMP,
  c_sysdate   DATE           DEFAULT SYSDATE,
  c_systs     TIMESTAMP      DEFAULT SYSTIMESTAMP,
  c_on_null   VARCHAR2(10)   DEFAULT ON NULL 'fallback',
  c_null      VARCHAR2(50)   NULL
);

-- ---------------------------------------------------------------------------
-- t_generated — colonnes virtuelles calculées
-- ---------------------------------------------------------------------------
CREATE TABLE t_generated (
  c_base  NUMBER(10) NOT NULL,
  c_virt  NUMBER     GENERATED ALWAYS AS (c_base * 2) VIRTUAL,
  c_stor  NUMBER     AS (c_base + 1),                -- VIRTUAL par défaut côté Oracle
  PRIMARY KEY (c_base)
);

-- ---------------------------------------------------------------------------
-- t_identity — IDENTITY BY DEFAULT + UNIQUE inline
-- ---------------------------------------------------------------------------
CREATE TABLE t_identity (
  id     NUMBER       GENERATED BY DEFAULT AS IDENTITY START WITH 1000,
  label  VARCHAR2(100) NOT NULL UNIQUE,
  CONSTRAINT pk_t_identity PRIMARY KEY (id)
);

-- ---------------------------------------------------------------------------
-- Séquences explicites (Oracle-style, avant 12c)
-- ---------------------------------------------------------------------------
CREATE SEQUENCE seq_legacy
  START WITH 1
  INCREMENT BY 1
  MAXVALUE 9999999999
  NOCYCLE
  CACHE 20;

CREATE SEQUENCE seq_steps
  START WITH 100
  INCREMENT BY 10
  MINVALUE 100
  MAXVALUE 1000
  CYCLE
  NOCACHE;

-- ---------------------------------------------------------------------------
-- customers — table métier 1/3 (FK parent)
-- ---------------------------------------------------------------------------
CREATE TABLE customers (
  id          NUMBER         GENERATED ALWAYS AS IDENTITY,
  email       VARCHAR2(255)  NOT NULL,
  full_name   VARCHAR2(200)  NOT NULL,
  created_at  TIMESTAMP      DEFAULT SYSTIMESTAMP NOT NULL,
  CONSTRAINT pk_customers     PRIMARY KEY (id),
  CONSTRAINT uq_customers_email UNIQUE (email)
);

COMMENT ON TABLE customers IS 'Clients';

-- ---------------------------------------------------------------------------
-- orders — FK nommée, check constraint, ENUM émulé via CHECK
-- ---------------------------------------------------------------------------
CREATE TABLE orders (
  id            NUMBER         GENERATED ALWAYS AS IDENTITY,
  customer_id   NUMBER         NOT NULL,
  status        VARCHAR2(20)   DEFAULT 'pending' NOT NULL,
  total         NUMBER(12,2)   NOT NULL,
  metadata      JSON,                                          -- natif 23c
  created_at    TIMESTAMP      DEFAULT SYSTIMESTAMP NOT NULL,
  CONSTRAINT pk_orders         PRIMARY KEY (id),
  CONSTRAINT ck_orders_status  CHECK (status IN ('pending','paid','shipped','cancelled')),
  CONSTRAINT fk_orders_cust    FOREIGN KEY (customer_id)
                               REFERENCES customers(id) ON DELETE CASCADE
);

CREATE INDEX idx_orders_created ON orders (created_at);

-- ---------------------------------------------------------------------------
-- order_items — volumineuse (25 000+ lignes via seed), FK cascade
-- ---------------------------------------------------------------------------
CREATE TABLE order_items (
  id          NUMBER         GENERATED ALWAYS AS IDENTITY,
  order_id    NUMBER         NOT NULL,
  line_no     NUMBER(10)     NOT NULL,
  sku         VARCHAR2(64)   NOT NULL,
  quantity    NUMBER(10)     NOT NULL,
  unit_price  NUMBER(10,2)   NOT NULL,
  is_gift     NUMBER(1)      DEFAULT 0 NOT NULL,
  CONSTRAINT pk_order_items    PRIMARY KEY (id),
  CONSTRAINT uq_order_items    UNIQUE (order_id, line_no),
  CONSTRAINT fk_items_order    FOREIGN KEY (order_id)
                               REFERENCES orders(id) ON DELETE CASCADE,
  CONSTRAINT ck_items_gift     CHECK (is_gift IN (0,1))
);

-- ---------------------------------------------------------------------------
-- t_check — contraintes CHECK multi-colonnes
-- ---------------------------------------------------------------------------
CREATE TABLE t_check (
  id    NUMBER         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  age   NUMBER(3)      CONSTRAINT ck_age CHECK (age BETWEEN 0 AND 150),
  code  VARCHAR2(10)   CONSTRAINT ck_code CHECK (LENGTH(code) = 10)
);

-- ---------------------------------------------------------------------------
-- t_Case_sensitive — identifiants quotés (préservation de la casse)
-- ---------------------------------------------------------------------------
CREATE TABLE "t_Case_sensitive" (
  "id"        NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  "CamelCol"  VARCHAR2(50),
  "reserved"  NUMBER(10)
);

-- ---------------------------------------------------------------------------
-- Tables auxiliaires pour les triggers
-- ---------------------------------------------------------------------------
CREATE TABLE orders_audit (
  id        NUMBER         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id  NUMBER         NOT NULL,
  action    CHAR(3)        NOT NULL,
  at        TIMESTAMP      NOT NULL
);

CREATE TABLE items_deleted (
  id       NUMBER      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  item_id  NUMBER      NOT NULL,
  at       TIMESTAMP   NOT NULL
);

-- ---------------------------------------------------------------------------
-- Synonymes
-- ---------------------------------------------------------------------------
CREATE SYNONYM syn_orders FOR orders;
-- PUBLIC SYNONYM requiert des droits CREATE PUBLIC SYNONYM, retiré pour le
-- container gvenzl standard (APP_USER n'a pas ce privilège par défaut).
-- CREATE PUBLIC SYNONYM public_orders FOR orders;

-- ===========================================================================
-- Views — incluant une vue agrégée et une vue avec CHECK OPTION
-- ===========================================================================

CREATE OR REPLACE VIEW v_customer_orders AS
SELECT c.email, COUNT(o.id) AS n_orders
  FROM customers c
  LEFT JOIN orders o ON o.customer_id = c.id
 GROUP BY c.email;

CREATE OR REPLACE VIEW v_order_totals AS
SELECT o.id,
       JSON_VALUE(o.metadata, '$.vendor') AS vendor,
       o.total
  FROM orders o;

CREATE OR REPLACE VIEW v_complex AS
SELECT c.id                  AS customer_id,
       c.email,
       COUNT(o.id)            AS n_orders,
       LISTAGG(o.status, ',') WITHIN GROUP (ORDER BY o.id) AS statuses
  FROM customers c
  LEFT JOIN orders o ON o.customer_id = c.id
 WHERE c.id IN (SELECT customer_id FROM orders WHERE total > 0)
 GROUP BY c.id, c.email;

-- Updatable view avec WITH CHECK OPTION
CREATE OR REPLACE VIEW v_with_check AS
SELECT id, customer_id, total, status
  FROM orders
 WHERE total > 0
WITH CHECK OPTION;

-- ---------------------------------------------------------------------------
-- Materialized view (refresh complet sur demande)
-- ---------------------------------------------------------------------------
CREATE MATERIALIZED VIEW mv_customer_totals
BUILD IMMEDIATE
REFRESH COMPLETE ON DEMAND
AS
SELECT c.id AS customer_id, c.email,
       COUNT(o.id) AS n_orders,
       NVL(SUM(o.total), 0) AS revenue
  FROM customers c
  LEFT JOIN orders o ON o.customer_id = c.id
 GROUP BY c.id, c.email;

-- ===========================================================================
-- Triggers (PL/SQL)
-- ===========================================================================
CREATE OR REPLACE TRIGGER trg_orders_audit_ins
AFTER INSERT ON orders
FOR EACH ROW
BEGIN
  INSERT INTO orders_audit(order_id, action, at)
  VALUES (:NEW.id, 'INS', SYSTIMESTAMP);
END;
/

CREATE OR REPLACE TRIGGER trg_orders_tot_upd
BEFORE UPDATE ON orders
FOR EACH ROW
BEGIN
  :NEW.total := :NEW.total * 1.0;
END;
/

CREATE OR REPLACE TRIGGER trg_items_del
AFTER DELETE ON order_items
FOR EACH ROW
BEGIN
  INSERT INTO items_deleted(item_id, at) VALUES (:OLD.id, SYSTIMESTAMP);
END;
/

-- Trigger composé (Oracle feature) : démontre la syntaxe compound
CREATE OR REPLACE TRIGGER trg_compound
FOR INSERT OR UPDATE ON orders
COMPOUND TRIGGER
  BEFORE EACH ROW IS
  BEGIN
    IF :NEW.total < 0 THEN
      :NEW.total := 0;
    END IF;
  END BEFORE EACH ROW;
END trg_compound;
/

-- Trigger FOLLOWS — ordre d'exécution
CREATE OR REPLACE TRIGGER trg_multi
AFTER INSERT ON orders
FOR EACH ROW
FOLLOWS trg_orders_audit_ins
BEGIN
  INSERT INTO orders_audit(order_id, action, at)
  VALUES (:NEW.id, 'IN2', SYSTIMESTAMP);
END;
/

-- ===========================================================================
-- Procédures & fonctions standalone
-- ===========================================================================
CREATE OR REPLACE PROCEDURE p_recalc_total(p_order_id IN NUMBER)
IS
BEGIN
  UPDATE orders
     SET total = (SELECT NVL(SUM(unit_price * quantity), 0)
                    FROM order_items WHERE order_id = p_order_id)
   WHERE id = p_order_id;
END;
/

CREATE OR REPLACE PROCEDURE p_inout(a IN NUMBER, b IN OUT NUMBER, c OUT NUMBER)
IS
BEGIN
  c := a + b;
  b := b + 1;
END;
/

CREATE OR REPLACE PROCEDURE p_cursor
IS
  v_id  orders.id%TYPE;
  CURSOR cur_orders IS SELECT id FROM orders;
BEGIN
  OPEN cur_orders;
  LOOP
    FETCH cur_orders INTO v_id;
    EXIT WHEN cur_orders%NOTFOUND;
    NULL; -- traitement ici
  END LOOP;
  CLOSE cur_orders;
END;
/

CREATE OR REPLACE PROCEDURE p_cursor_for
IS
BEGIN
  FOR rec IN (SELECT id, total FROM orders WHERE total > 0) LOOP
    DBMS_OUTPUT.PUT_LINE(rec.id || ' = ' || rec.total);
  END LOOP;
END;
/

CREATE OR REPLACE PROCEDURE p_exception_demo(p_id IN NUMBER)
IS
  v_count NUMBER;
BEGIN
  SELECT COUNT(*) INTO v_count FROM orders WHERE id = p_id;
  IF v_count = 0 THEN
    RAISE_APPLICATION_ERROR(-20001, 'order not found: ' || p_id);
  END IF;
EXCEPTION
  WHEN NO_DATA_FOUND THEN
    NULL;
  WHEN OTHERS THEN
    RAISE;
END;
/

CREATE OR REPLACE FUNCTION f_order_count(p_email VARCHAR2) RETURN NUMBER
IS
  n NUMBER;
BEGIN
  SELECT COUNT(*) INTO n
    FROM orders o
    JOIN customers c ON c.id = o.customer_id
   WHERE c.email = p_email;
  RETURN n;
END;
/

CREATE OR REPLACE FUNCTION f_safe_div(a NUMBER, b NUMBER) RETURN NUMBER
DETERMINISTIC
IS
BEGIN
  IF b = 0 THEN
    RETURN NULL;
  END IF;
  RETURN a / b;
END;
/

-- Function avec BULK COLLECT + FORALL (patron exhaustif)
CREATE OR REPLACE PROCEDURE p_bulk_demo
IS
  TYPE t_id_arr IS TABLE OF orders.id%TYPE;
  v_ids t_id_arr;
BEGIN
  SELECT id BULK COLLECT INTO v_ids FROM orders WHERE total > 100;
  FORALL i IN 1 .. v_ids.COUNT
    UPDATE orders SET status = 'paid' WHERE id = v_ids(i);
END;
/

-- ===========================================================================
-- Package : namespace dédié, traduit → schéma PG homonyme
-- ===========================================================================
CREATE OR REPLACE PACKAGE pkg_math AS
  FUNCTION add_n(a NUMBER, b NUMBER) RETURN NUMBER;
  FUNCTION safe_div(a NUMBER, b NUMBER) RETURN NUMBER;
  PROCEDURE log_it(msg VARCHAR2);
  g_counter NUMBER := 0;                             -- variable globale → table _state
END pkg_math;
/

CREATE OR REPLACE PACKAGE BODY pkg_math AS

  FUNCTION add_n(a NUMBER, b NUMBER) RETURN NUMBER IS
  BEGIN
    g_counter := g_counter + 1;
    RETURN a + b;
  END add_n;

  FUNCTION safe_div(a NUMBER, b NUMBER) RETURN NUMBER IS
  BEGIN
    IF b = 0 THEN RETURN NULL; END IF;
    RETURN a / b;
  END safe_div;

  PROCEDURE log_it(msg VARCHAR2) IS
  BEGIN
    DBMS_OUTPUT.PUT_LINE('[pkg_math] ' || msg);
  END log_it;

END pkg_math;
/

-- ===========================================================================
-- Package avec curseur + exception + pragma AUTONOMOUS_TRANSACTION
-- ===========================================================================
CREATE OR REPLACE PACKAGE pkg_audit AS
  PROCEDURE log_order(p_order_id NUMBER, p_action VARCHAR2);
  log_error EXCEPTION;
  PRAGMA EXCEPTION_INIT(log_error, -20050);
END pkg_audit;
/

CREATE OR REPLACE PACKAGE BODY pkg_audit AS

  PROCEDURE log_order(p_order_id NUMBER, p_action VARCHAR2) IS
    PRAGMA AUTONOMOUS_TRANSACTION;
  BEGIN
    INSERT INTO orders_audit(order_id, action, at)
    VALUES (p_order_id, SUBSTR(p_action, 1, 3), SYSTIMESTAMP);
    COMMIT;
  EXCEPTION
    WHEN OTHERS THEN
      ROLLBACK;
      RAISE log_error;
  END log_order;

END pkg_audit;
/

-- ===========================================================================
-- Type objet (CREATE TYPE) + methods
-- ===========================================================================
CREATE OR REPLACE TYPE addr_t AS OBJECT (
  street   VARCHAR2(100),
  city     VARCHAR2(50),
  zip      VARCHAR2(10),
  MEMBER FUNCTION full_address RETURN VARCHAR2
);
/

CREATE OR REPLACE TYPE BODY addr_t AS
  MEMBER FUNCTION full_address RETURN VARCHAR2 IS
  BEGIN
    RETURN street || ', ' || zip || ' ' || city;
  END full_address;
END;
/

-- Collection type (VARRAY)
CREATE OR REPLACE TYPE tags_t AS VARRAY(10) OF VARCHAR2(30);
/

-- Nested table type
CREATE OR REPLACE TYPE id_list_t AS TABLE OF NUMBER;
/

-- ===========================================================================
-- Bloc anonyme de smoke test
-- ===========================================================================
BEGIN
  DBMS_OUTPUT.PUT_LINE('squishy oracle fixture loaded');
END;
/
