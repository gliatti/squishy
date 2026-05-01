-- ============================================================================
-- squishy — fixture MySQL exhaustive
-- Chaque objet ici déclenche au moins une règle du traducteur (voir plan).
-- ============================================================================

SET NAMES utf8mb4;
SET character_set_client = utf8mb4;
SET collation_connection = utf8mb4_unicode_ci;

-- Allow the sample source user to see routine bodies through SHOW CREATE.
-- Without SHOW_ROUTINE (introduced in MySQL 8.0.20) the "Create Procedure"
-- / "Create Function" column returns NULL for any routine the user did not
-- create, silently breaking DDL introspection.
GRANT SHOW_ROUTINE ON *.* TO 'sakila'@'%';
FLUSH PRIVILEGES;

-- ---------------------------------------------------------------------------
-- t_numeric — couvre tous les types numériques, unsigned, zerofill, BIT
-- ---------------------------------------------------------------------------
CREATE TABLE t_numeric (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  c_tinyint         TINYINT,
  c_tinyint_u       TINYINT UNSIGNED,
  c_tinyint_bool    TINYINT(1),
  c_smallint        SMALLINT,
  c_smallint_u      SMALLINT UNSIGNED,
  c_mediumint       MEDIUMINT,
  c_mediumint_u     MEDIUMINT UNSIGNED,
  c_int             INT,
  c_int_u           INT UNSIGNED,
  c_bigint          BIGINT,
  c_bigint_u        BIGINT UNSIGNED,
  c_decimal         DECIMAL(12,4),
  c_decimal_default DECIMAL,
  c_numeric         NUMERIC(30,10),
  c_float           FLOAT,
  c_float_prec      FLOAT(7,4),
  c_double          DOUBLE,
  c_real            REAL,
  c_zerofill        INT(5) ZEROFILL,
  c_bit1            BIT(1),
  c_bit8            BIT(8),
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- t_string — char/varchar/text/blob/enum/set/json, collations variées
-- ---------------------------------------------------------------------------
CREATE TABLE t_string (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  c_char        CHAR(10),
  c_varchar     VARCHAR(255),
  c_varchar_bin VARCHAR(50) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin,
  c_tinytext    TINYTEXT,
  c_text        TEXT,
  c_mediumtext  MEDIUMTEXT,
  c_longtext    LONGTEXT,
  c_tinyblob    TINYBLOB,
  c_blob        BLOB,
  c_mediumblob  MEDIUMBLOB,
  c_longblob    LONGBLOB,
  c_binary      BINARY(16),
  c_varbinary   VARBINARY(64),
  c_enum        ENUM('s','m','l','xl'),
  c_set         SET('a','b','c','d'),
  c_json        JSON,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- t_temporal — dates/heures avec fractional seconds et ON UPDATE
-- ---------------------------------------------------------------------------
CREATE TABLE t_temporal (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  c_date            DATE,
  c_time            TIME,
  c_time_frac       TIME(6),
  c_datetime        DATETIME,
  c_datetime_frac   DATETIME(3),
  c_timestamp       TIMESTAMP NULL DEFAULT NULL,
  c_timestamp_def   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  c_year            YEAR,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- t_spatial — types géométriques (warning, non mappé en v1)
-- ---------------------------------------------------------------------------
CREATE TABLE t_spatial (
  id       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  c_geom   GEOMETRY,
  c_point  POINT,
  PRIMARY KEY (id)
) ENGINE=InnoDB;

-- ---------------------------------------------------------------------------
-- t_defaults — DEFAULT littéraux, expressions, fonctions, NULL handling
-- ---------------------------------------------------------------------------
CREATE TABLE t_defaults (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  c_int_def     INT NOT NULL DEFAULT 42,
  c_str_def     VARCHAR(10) DEFAULT 'hello',
  c_expr_def    INT DEFAULT (10 + 5),
  c_now_def     DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3),
  c_uuid_def    VARCHAR(36) DEFAULT (UUID()),
  c_nullable    VARCHAR(50) NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB;

-- ---------------------------------------------------------------------------
-- t_generated — colonnes VIRTUAL et STORED, PK composite incluant la colonne générée
-- ---------------------------------------------------------------------------
CREATE TABLE t_generated (
  c_base  INT NOT NULL,
  c_virt  INT AS (c_base * 2) VIRTUAL,
  c_stor  INT AS (c_base + 1) STORED,
  PRIMARY KEY (c_base, c_stor)
) ENGINE=InnoDB;

-- ---------------------------------------------------------------------------
-- t_identity — AUTO_INCREMENT + UNIQUE inline
-- ---------------------------------------------------------------------------
CREATE TABLE t_identity (
  id     BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  label  VARCHAR(100) NOT NULL UNIQUE,
  PRIMARY KEY (id)
) ENGINE=InnoDB AUTO_INCREMENT=1000;

-- ---------------------------------------------------------------------------
-- customers — table métier 1/3, FK parent
-- ---------------------------------------------------------------------------
CREATE TABLE customers (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  email       VARCHAR(255) NOT NULL,
  full_name   VARCHAR(200) NOT NULL,
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uq_customers_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='Clients';

-- ---------------------------------------------------------------------------
-- orders — FK nommée vers customers, index secondaire, ENUM + JSON
-- ---------------------------------------------------------------------------
CREATE TABLE orders (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  customer_id  BIGINT UNSIGNED NOT NULL,
  status       ENUM('pending','paid','shipped','cancelled') NOT NULL DEFAULT 'pending',
  total        DECIMAL(12,2) NOT NULL,
  metadata     JSON DEFAULT NULL,
  created_at   DATETIME NOT NULL,
  PRIMARY KEY (id),
  KEY idx_orders_created (created_at),
  CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id)
    REFERENCES customers(id) ON DELETE CASCADE ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------------------
-- order_items — volumineuse (≥25 000 lignes via seed), FK cascadée
-- ---------------------------------------------------------------------------
CREATE TABLE order_items (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_id    BIGINT UNSIGNED NOT NULL,
  line_no     INT UNSIGNED NOT NULL,
  sku         VARCHAR(64) NOT NULL,
  quantity    INT UNSIGNED NOT NULL,
  unit_price  DECIMAL(10,2) NOT NULL,
  is_gift     TINYINT(1) NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  UNIQUE KEY uq_item (order_id, line_no),
  FOREIGN KEY (order_id) REFERENCES orders(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 ROW_FORMAT=DYNAMIC;

-- ---------------------------------------------------------------------------
-- t_check — contraintes CHECK (MySQL 8+)
-- ---------------------------------------------------------------------------
CREATE TABLE t_check (
  id    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  age   INT CHECK (age >= 0 AND age < 150),
  code  VARCHAR(10) CHECK (CHAR_LENGTH(code) = 10),
  PRIMARY KEY (id)
) ENGINE=InnoDB;

-- ---------------------------------------------------------------------------
-- t_Case_sensitive — identifiants en camelCase et mots réservés PG
-- ---------------------------------------------------------------------------
CREATE TABLE `t_Case_sensitive` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `CamelCol`   VARCHAR(50),
  `reserved`   INT,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB;

-- ---------------------------------------------------------------------------
-- Tables auxiliaires pour les triggers
-- ---------------------------------------------------------------------------
CREATE TABLE orders_audit (
  id        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_id  BIGINT UNSIGNED NOT NULL,
  action    CHAR(3) NOT NULL,
  at        DATETIME NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB;

CREATE TABLE items_deleted (
  id       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  item_id  BIGINT UNSIGNED NOT NULL,
  at       DATETIME NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB;

-- ===========================================================================
-- Views
-- ===========================================================================
CREATE OR REPLACE VIEW v_customer_orders AS
SELECT c.email, COUNT(o.id) AS n_orders
FROM customers c
LEFT JOIN orders o ON o.customer_id = c.id
GROUP BY c.email;

CREATE OR REPLACE SQL SECURITY INVOKER VIEW v_order_totals AS
SELECT o.id,
       JSON_EXTRACT(o.metadata, '$.vendor') AS vendor,
       o.total
FROM orders o;

-- Aggregate view: exercises GROUP_CONCAT + subquery (no CHECK OPTION, non-updatable)
CREATE OR REPLACE VIEW v_complex AS
SELECT c.id           AS customer_id,
       c.email,
       COUNT(o.id)    AS n_orders,
       GROUP_CONCAT(o.status SEPARATOR ',') AS statuses
FROM customers c
LEFT JOIN orders o ON o.customer_id = c.id
WHERE c.id IN (SELECT customer_id FROM orders WHERE total > 0)
GROUP BY c.id, c.email;

-- Updatable view: exercises WITH CHECK OPTION parsing
CREATE OR REPLACE VIEW v_with_check AS
SELECT id, customer_id, total, status
  FROM orders
 WHERE total > 0
WITH CASCADED CHECK OPTION;

-- ===========================================================================
-- Triggers
-- ===========================================================================
DELIMITER //

CREATE DEFINER=`root`@`%` TRIGGER trg_orders_audit_ins
AFTER INSERT ON orders
FOR EACH ROW
BEGIN
  INSERT INTO orders_audit(order_id, action, at) VALUES (NEW.id, 'INS', NOW());
END //

CREATE TRIGGER trg_orders_tot_upd
BEFORE UPDATE ON orders
FOR EACH ROW
BEGIN
  SET NEW.total = NEW.total * 1.0;
END //

CREATE TRIGGER trg_items_del
AFTER DELETE ON order_items
FOR EACH ROW
BEGIN
  INSERT INTO items_deleted(item_id, at) VALUES (OLD.id, NOW());
END //

CREATE TRIGGER trg_multi
AFTER INSERT ON orders
FOR EACH ROW
FOLLOWS trg_orders_audit_ins
BEGIN
  INSERT INTO orders_audit(order_id, action, at) VALUES (NEW.id, 'INS', NOW());
END //

-- ===========================================================================
-- Procedures
-- ===========================================================================
CREATE PROCEDURE p_recalc_total(IN p_order_id BIGINT UNSIGNED)
BEGIN
  UPDATE orders
     SET total = (SELECT IFNULL(SUM(unit_price * quantity), 0)
                    FROM order_items WHERE order_id = p_order_id)
   WHERE id = p_order_id;
END //

CREATE PROCEDURE p_inout(IN a INT, INOUT b INT, OUT c INT)
BEGIN
  SET c = a + b;
  SET b = b + 1;
END //

CREATE PROCEDURE p_cursor()
BEGIN
  DECLARE done INT DEFAULT 0;
  DECLARE v_id BIGINT;
  DECLARE cur1 CURSOR FOR SELECT id FROM orders;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;
  OPEN cur1;
  read_loop: LOOP
    FETCH cur1 INTO v_id;
    IF done = 1 THEN LEAVE read_loop; END IF;
  END LOOP;
  CLOSE cur1;
END //

CREATE PROCEDURE p_sec() SQL SECURITY DEFINER DETERMINISTIC CONTAINS SQL
BEGIN
  SELECT 1;
END //

-- ===========================================================================
-- Functions
-- ===========================================================================
CREATE FUNCTION f_order_count(p_email VARCHAR(255))
RETURNS BIGINT
DETERMINISTIC READS SQL DATA
BEGIN
  DECLARE n BIGINT;
  SELECT COUNT(*) INTO n
    FROM orders o
    JOIN customers c ON c.id = o.customer_id
   WHERE c.email = p_email;
  RETURN n;
END //

CREATE FUNCTION f_safe_div(a DECIMAL(12,4), b DECIMAL(12,4))
RETURNS DECIMAL(12,4)
DETERMINISTIC NO SQL
BEGIN
  IF b = 0 THEN RETURN NULL; END IF;
  RETURN a / b;
END //

CREATE FUNCTION f_json_vendor(j JSON)
RETURNS VARCHAR(200)
DETERMINISTIC
BEGIN
  RETURN JSON_UNQUOTE(JSON_EXTRACT(j, '$.vendor'));
END //

-- ===========================================================================
-- Events
-- ===========================================================================
CREATE EVENT ev_purge_audit
ON SCHEDULE EVERY 1 DAY STARTS '2025-01-01 03:00:00'
DO
  DELETE FROM orders_audit WHERE at < NOW() - INTERVAL 30 DAY //

CREATE EVENT ev_once
ON SCHEDULE AT CURRENT_TIMESTAMP + INTERVAL 1 HOUR
DO
  CALL p_recalc_total(42) //

DELIMITER ;
