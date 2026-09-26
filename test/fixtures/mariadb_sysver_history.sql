-- mariadb-sysver-history.sql — MariaDB WITH SYSTEM VERSIONING with REAL history.
--
-- Database: mariadb_sysver (MariaDB 10.5+, written against 11.8).
--
-- Unlike mariadb-features-xl.sql, this script UPDATEs and DELETEs versioned
-- rows so that `SELECT ... FOR SYSTEM_TIME ALL` holds several thousand closed
-- versions. It exercises squishy's system-versioning emulation
-- (<t>_history table + stamp/history triggers + system_time_history copy):
--
--   a) sv_accounts        explicit ROW START/ROW END + PERIOD FOR SYSTEM_TIME,
--                         several UPDATE rounds on the same keys (many
--                         versions per key) and DELETEs (keys that only
--                         survive in history);
--   b) sv_notes           implicit versioning (hidden ROW_START / ROW_END),
--                         with one WITHOUT SYSTEM VERSIONING column;
--   c) sv_products        explicit columns + a WITHOUT SYSTEM VERSIONING
--                         column updated alone (no history) and in mixed
--                         updates (history);
--   d) sv_orders          VIRTUAL and STORED generated columns, incl. text
--                         built by CONCAT / CONCAT_WS / CAST AS CHAR from
--                         TINYINT(1), CHAR(n), DECIMAL and hex operands,
--                         string columns holding a non-text value, a hex
--                         literal value and LENGTH (bytes) of UTF-8 text;
--   e) sv_events          PARTITION BY SYSTEM_TIME (history partitions);
--   f) sv_customers       ALTER TABLE ... ADD SYSTEM VERSIONING on a table
--                         that already held data, then UPDATE/DELETE;
--   g) sv_account_notes   plain table with a FK to the versioned sv_accounts;
--   h) updates performed with session time_zone '+05:30' / '-07:00'
--                         (the copy must pin the source session to UTC);
--   i) sv_ledger_trx      transaction-precise versioning (BIGINT UNSIGNED
--                         ROW START/END transaction ids) — not emulable in
--                         PostgreSQL, squishy must warn explicitly.
--
-- Load:  docker compose exec -T mariadb-sample mariadb -uroot -proot < dumps/mariadb/mariadb-sysver-history.sql
-- autocommit stays ON (CLAUDE.md "Bulk-loading MySQL/MariaDB dumps"); the
-- volume is small (a few thousand rows), no durability tuning is needed.
-- `DO SLEEP(...)` between rounds guarantees distinct ROW START / ROW END
-- timestamps between versions.

DROP DATABASE IF EXISTS mariadb_sysver;
CREATE DATABASE mariadb_sysver CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE mariadb_sysver;

SET time_zone = '+00:00';

-- ---------------------------------------------------------------------
-- a) explicit ROW START / ROW END, multiple versions per key + deletes
-- ---------------------------------------------------------------------
CREATE TABLE sv_accounts (
  id         INT NOT NULL PRIMARY KEY,
  owner      VARCHAR(64) NOT NULL,
  balance    DECIMAL(12,2) NOT NULL DEFAULT 0,
  status     VARCHAR(16) NOT NULL DEFAULT 'open',
  valid_from TIMESTAMP(6) GENERATED ALWAYS AS ROW START,
  valid_to   TIMESTAMP(6) GENERATED ALWAYS AS ROW END,
  PERIOD FOR SYSTEM_TIME (valid_from, valid_to),
  KEY idx_sv_accounts_owner (owner)
) ENGINE=InnoDB WITH SYSTEM VERSIONING;

INSERT INTO sv_accounts (id, owner, balance)
SELECT seq, CONCAT('owner-', LPAD(seq, 5, '0')), seq * 10
FROM seq_1_to_1500;

DO SLEEP(0.05);
UPDATE sv_accounts SET balance = balance + 1.25;                     -- round 1: 1500 versions
DO SLEEP(0.05);
UPDATE sv_accounts SET status = 'active' WHERE id % 2 = 0;           -- round 2: 750
DO SLEEP(0.05);
UPDATE sv_accounts SET balance = balance * 2, owner = UPPER(owner)
 WHERE id <= 600;                                                    -- round 3: 600
DO SLEEP(0.05);
UPDATE sv_accounts SET balance = balance - 3 WHERE id <= 100;        -- round 4: 100
DO SLEEP(0.05);
UPDATE sv_accounts SET balance = balance - 3 WHERE id <= 100;        -- round 5: 100 (same keys again)
DO SLEEP(0.05);
UPDATE sv_accounts SET status = status WHERE id BETWEEN 101 AND 150; -- no-op values: still versioned
DO SLEEP(0.05);
DELETE FROM sv_accounts WHERE id > 1400;                             -- 100 keys only in history
DO SLEEP(0.05);
-- a deleted key re-inserted: history and current share the key
INSERT INTO sv_accounts (id, owner, balance, status) VALUES (1401, 'reborn-1401', 1, 'reopened');

-- ---------------------------------------------------------------------
-- b) implicit system versioning (hidden ROW_START / ROW_END)
-- ---------------------------------------------------------------------
CREATE TABLE sv_notes (
  id      INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  title   VARCHAR(100) NOT NULL,
  body    TEXT,
  tag     VARCHAR(20),
  read_count   INT NOT NULL DEFAULT 0 WITHOUT SYSTEM VERSIONING
) ENGINE=InnoDB WITH SYSTEM VERSIONING;

INSERT INTO sv_notes (title, body, tag)
SELECT CONCAT('note ', seq), CONCAT('body of note ', seq, ' — ünïcödé ✓'), IF(seq % 3 = 0, 'red', 'blue')
FROM seq_1_to_400;

DO SLEEP(0.05);
UPDATE sv_notes SET body = CONCAT(body, ' (edited)');                -- 400 versions
DO SLEEP(0.05);
UPDATE sv_notes SET tag = NULL WHERE tag = 'red';                    -- 133 versions
DO SLEEP(0.05);
UPDATE sv_notes SET read_count = read_count + 7;                               -- unversioned only: no history
DO SLEEP(0.05);
UPDATE sv_notes SET read_count = read_count + 1, title = CONCAT(title, '*') WHERE id <= 50; -- mixed: 50 versions
DO SLEEP(0.05);
DELETE FROM sv_notes WHERE id > 380;                                 -- 20 keys only in history

-- ---------------------------------------------------------------------
-- c) WITHOUT SYSTEM VERSIONING column on an explicit-column table
-- ---------------------------------------------------------------------
CREATE TABLE sv_products (
  id         INT NOT NULL PRIMARY KEY,
  sku        VARCHAR(32) NOT NULL,
  price      DECIMAL(10,2) NOT NULL,
  view_count INT NOT NULL DEFAULT 0 WITHOUT SYSTEM VERSIONING,
  row_start  TIMESTAMP(6) GENERATED ALWAYS AS ROW START INVISIBLE,
  row_end    TIMESTAMP(6) GENERATED ALWAYS AS ROW END INVISIBLE,
  PERIOD FOR SYSTEM_TIME (row_start, row_end),
  UNIQUE KEY uq_sv_products_sku (sku)
) ENGINE=InnoDB WITH SYSTEM VERSIONING;

INSERT INTO sv_products (id, sku, price)
SELECT seq, CONCAT('SKU-', seq), 9.99 + seq FROM seq_1_to_300;

DO SLEEP(0.05);
UPDATE sv_products SET view_count = view_count + 10;                 -- unversioned only: no history
DO SLEEP(0.05);
UPDATE sv_products SET view_count = view_count + 1;                  -- unversioned only: no history
DO SLEEP(0.05);
UPDATE sv_products SET price = price * 1.10 WHERE id <= 200;         -- 200 versions
DO SLEEP(0.05);
UPDATE sv_products SET price = price - 1, view_count = 0 WHERE id <= 40; -- mixed: 40 versions
DO SLEEP(0.05);
DELETE FROM sv_products WHERE id > 290;                              -- 10 keys only in history

-- ---------------------------------------------------------------------
-- d) VIRTUAL + STORED generated columns on a versioned table, including
--    text built from typed operands (CONCAT / CONCAT_WS / CAST AS CHAR):
--    TINYINT(1) prints 1 / 0, CHAR(n) drops its pad, DECIMAL keeps its
--    scale, hex literals are strings, CONCAT_WS skips NULLs. The PG
--    generation expressions must reproduce MariaDB's text exactly, or the
--    current rows (computed by PG) and the copied history rows (computed
--    by MariaDB) disagree.
-- ---------------------------------------------------------------------
CREATE TABLE sv_orders (
  id          INT NOT NULL PRIMARY KEY,
  qty         INT NOT NULL,
  unit_price  DECIMAL(10,2) NOT NULL,
  paid        TINYINT(1) NOT NULL DEFAULT 0,
  code        CHAR(4) NULL,
  note        VARCHAR(20) NULL,
  total_v     DECIMAL(12,2) AS (qty * unit_price) VIRTUAL,
  total_s     DECIMAL(12,2) AS (qty * unit_price) STORED,
  label       VARCHAR(40) AS (CONCAT('order#', id)) VIRTUAL,
  summary     VARCHAR(80) AS (CONCAT('paid=', paid, ';t=', qty * unit_price, ';', X'23', note, ';ok=', qty > 3)) STORED,
  -- CHAR(n) operands are VIRTUAL only: MariaDB refuses them in a STORED
  -- column (their text depends on sql_mode PAD_CHAR_TO_FULL_LENGTH).
  tags        VARCHAR(80) AS (CONCAT_WS('|', note, paid, id, code)) VIRTUAL,
  paid_c      VARCHAR(4) AS (CAST(paid AS CHAR)) VIRTUAL,
  -- A string column whose whole expression is not text: MariaDB stores
  -- the value's text (1 / 0, DECIMAL scale), PG's implicit assignment
  -- cast would store 'true' / 'false'.
  paid_v      VARCHAR(5) AS (paid) VIRTUAL,
  total_t     VARCHAR(16) AS (qty * unit_price) STORED,
  -- A hex literal as the whole value of a string column is 'A', not 41.
  mark        VARCHAR(4) AS (X'41') VIRTUAL,
  -- LENGTH counts bytes (multibyte notes below): PG octet_length, not length.
  note_len    INT AS (LENGTH(note)) VIRTUAL,
  sys_start   TIMESTAMP(6) GENERATED ALWAYS AS ROW START,
  sys_end     TIMESTAMP(6) GENERATED ALWAYS AS ROW END,
  PERIOD FOR SYSTEM_TIME (sys_start, sys_end)
) ENGINE=InnoDB WITH SYSTEM VERSIONING;

INSERT INTO sv_orders (id, qty, unit_price, paid, code, note)
SELECT seq, 1 + seq % 7, 2.50 + (seq % 11), seq % 2,
       IF(seq % 4 = 0, NULL, CONCAT('c', seq % 9)),
       IF(seq % 3 = 0, NULL, CONCAT(IF(seq % 10 = 1, 'né', 'n'), seq))
FROM seq_1_to_500;

DO SLEEP(0.05);
UPDATE sv_orders SET qty = qty + 1;                                  -- 500 versions
DO SLEEP(0.05);
UPDATE sv_orders SET unit_price = unit_price + 0.25 WHERE id % 5 = 0; -- 100 versions
DO SLEEP(0.05);
UPDATE sv_orders SET paid = 1 - paid, note = NULL WHERE id % 3 = 1;   -- 167 versions
DO SLEEP(0.05);
DELETE FROM sv_orders WHERE id % 50 = 0;                             -- 10 keys only in history

-- ---------------------------------------------------------------------
-- e) PARTITION BY SYSTEM_TIME (history partitions)
-- ---------------------------------------------------------------------
CREATE TABLE sv_events (
  id      INT NOT NULL,
  kind    VARCHAR(20) NOT NULL,
  payload VARCHAR(200),
  PRIMARY KEY (id)
) ENGINE=InnoDB WITH SYSTEM VERSIONING
  PARTITION BY SYSTEM_TIME LIMIT 400 (
    PARTITION p_hist0 HISTORY,
    PARTITION p_hist1 HISTORY,
    PARTITION p_hist2 HISTORY,
    PARTITION p_cur CURRENT
  );

INSERT INTO sv_events (id, kind, payload)
SELECT seq, ELT(1 + seq % 3, 'click', 'view', 'buy'), CONCAT('{"n":', seq, '}') FROM seq_1_to_600;

DO SLEEP(0.05);
UPDATE sv_events SET payload = CONCAT(payload, '!');                 -- 600 versions
DO SLEEP(0.05);
UPDATE sv_events SET kind = 'archived' WHERE id <= 300;              -- 300 versions
DO SLEEP(0.05);
DELETE FROM sv_events WHERE id > 550;                                -- 50 keys only in history

-- ---------------------------------------------------------------------
-- f) ADD SYSTEM VERSIONING on a table that already had data
-- ---------------------------------------------------------------------
CREATE TABLE sv_customers (
  id     INT NOT NULL PRIMARY KEY,
  name   VARCHAR(80) NOT NULL,
  email  VARCHAR(120),
  tier   CHAR(1) NOT NULL DEFAULT 'B'
) ENGINE=InnoDB;

INSERT INTO sv_customers (id, name, email)
SELECT seq, CONCAT('Customer ', seq), CONCAT('c', seq, '@example.test') FROM seq_1_to_350;

DO SLEEP(0.05);
ALTER TABLE sv_customers ADD SYSTEM VERSIONING;
DO SLEEP(0.05);
UPDATE sv_customers SET tier = 'A' WHERE id % 4 = 0;                 -- 87 versions
DO SLEEP(0.05);
UPDATE sv_customers SET email = NULL WHERE id % 10 = 0;              -- 35 versions
DO SLEEP(0.05);
DELETE FROM sv_customers WHERE id > 340;                             -- 10 keys only in history

-- ---------------------------------------------------------------------
-- g) FK from a plain table to a versioned table
-- ---------------------------------------------------------------------
CREATE TABLE sv_account_notes (
  id         INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  account_id INT NOT NULL,
  note       VARCHAR(200) NOT NULL,
  CONSTRAINT fk_sv_account_notes_account FOREIGN KEY (account_id)
    REFERENCES sv_accounts (id) ON DELETE CASCADE
) ENGINE=InnoDB;

INSERT INTO sv_account_notes (account_id, note)
SELECT 1 + (seq % 1400), CONCAT('memo ', seq) FROM seq_1_to_700;

DO SLEEP(0.05);
UPDATE sv_accounts SET status = 'watched' WHERE id IN (SELECT account_id FROM sv_account_notes WHERE id <= 20);

-- ---------------------------------------------------------------------
-- h) updates in sessions with a non-UTC time_zone
-- ---------------------------------------------------------------------
SET time_zone = '+05:30';
DO SLEEP(0.05);
UPDATE sv_accounts SET balance = balance + 100 WHERE id BETWEEN 200 AND 299;  -- 100 versions
DO SLEEP(0.05);
UPDATE sv_notes SET body = CONCAT(body, ' [IST]') WHERE id BETWEEN 100 AND 149; -- 50 versions
SET time_zone = '-07:00';
DO SLEEP(0.05);
UPDATE sv_products SET price = price + 0.01 WHERE id BETWEEN 250 AND 279;     -- 30 versions
DO SLEEP(0.05);
DELETE FROM sv_customers WHERE id BETWEEN 331 AND 335;                        -- 5 keys only in history
SET time_zone = '+00:00';

-- ---------------------------------------------------------------------
-- i) transaction-precise versioning (not emulable: expect a warning)
-- ---------------------------------------------------------------------
CREATE TABLE sv_ledger_trx (
  id        INT NOT NULL PRIMARY KEY,
  amount    DECIMAL(12,2) NOT NULL,
  trx_start BIGINT UNSIGNED GENERATED ALWAYS AS ROW START,
  trx_end   BIGINT UNSIGNED GENERATED ALWAYS AS ROW END,
  PERIOD FOR SYSTEM_TIME (trx_start, trx_end)
) ENGINE=InnoDB WITH SYSTEM VERSIONING;

INSERT INTO sv_ledger_trx (id, amount) SELECT seq, seq FROM seq_1_to_100;
DO SLEEP(0.05);
UPDATE sv_ledger_trx SET amount = amount + 1;
DO SLEEP(0.05);
DELETE FROM sv_ledger_trx WHERE id > 90;
