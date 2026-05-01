#!/usr/bin/env bash
# Fetches public sample dumps used to exercise the squishy parsers and
# data-copy pipeline. Idempotent: re-running skips files already present
# with non-zero size.
#
# Targets (9 dumps):
#   MySQL:
#     - employees.sql        datacharmer/test_db        ~170 MB (routines + 4 M rows)
#     - enwiki-page.sql.gz           dumps.wikimedia.org   ~2.4 GB gz
#     - enwiki-categorylinks.sql.gz  dumps.wikimedia.org   ~2.4 GB gz
#   MariaDB:
#     - employees.sql                   copy of MySQL one
#     - enwiki-categorylinks.sql.gz     hard-link of MySQL one
#     - mariadb-features-xl.sql         generated, ~10 M rows + SEQUENCE/temporal/JSON CHECK
#   Oracle (official samples from oracle-samples/db-sample-schemas, UPL-1.0):
#     - dumps/oracle/hr/     human_resources (code-rich: procs/funcs/triggers)
#     - dumps/oracle/oe/     order_entry     (types, XML, procs, huge set of scripts)
#     - dumps/oracle/sh/     sales_history   (partitions, MVs, ~1 M rows via CSV loader)
#
# Usage:
#   bash scripts/fetch_dumps.sh [--skip-wiki]

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DUMPS="$ROOT/dumps"
SKIP_WIKI=0
for arg in "$@"; do
  [[ "$arg" == "--skip-wiki" ]] && SKIP_WIKI=1
done

mkdir -p "$DUMPS/mysql" "$DUMPS/mariadb" "$DUMPS/oracle" "$DUMPS/db2"

# -- helpers ----------------------------------------------------------------

have() { [[ -s "$1" ]]; }

fetch() {
  local url="$1" dest="$2"
  if have "$dest"; then
    echo "skip  $(basename "$dest") (already present, $(du -h "$dest" | cut -f1))"
    return
  fi
  echo "fetch $url"
  curl -fL --retry 3 --retry-delay 2 --progress-bar -o "$dest.tmp" "$url"
  mv "$dest.tmp" "$dest"
  echo "      -> $(du -h "$dest" | cut -f1)"
}

# -- MySQL ------------------------------------------------------------------

echo "== MySQL =="

# 1. employees_db (datacharmer) — 170 MB, routines + 4 M rows
if ! have "$DUMPS/mysql/employees.sql"; then
  tmp="$(mktemp -d)"
  git clone --depth=1 https://github.com/datacharmer/test_db.git "$tmp/test_db"
  {
    cat "$tmp/test_db/employees.sql"
    for f in "$tmp/test_db/"load_*.dump; do
      echo "-- >>> $(basename "$f")"
      cat "$f"
    done
  } > "$DUMPS/mysql/employees.sql"
  rm -rf "$tmp"
  echo "wrote $DUMPS/mysql/employees.sql ($(du -h "$DUMPS/mysql/employees.sql" | cut -f1))"
fi

# 2-3. Wikipedia dumps — plain MySQL-compatible SQL, kept gzipped on disk.
#      Load with `zcat dumps/mysql/<file>.sql.gz | mysql ...`.
#      NB: we do NOT use MySQL airportdb anymore — it ships in MySQL Shell
#      `util.dumpInstance` format (TSV + zstd) which is not a plain SQL dump
#      and cannot be fed directly to squishy's mysql-dialect parser via file.
if [[ $SKIP_WIKI -eq 0 ]]; then
  fetch "https://dumps.wikimedia.org/enwiki/latest/enwiki-latest-page.sql.gz" \
        "$DUMPS/mysql/enwiki-page.sql.gz"
  fetch "https://dumps.wikimedia.org/enwiki/latest/enwiki-latest-categorylinks.sql.gz" \
        "$DUMPS/mysql/enwiki-categorylinks.sql.gz"
else
  echo "skip  enwiki-*.sql.gz (--skip-wiki)"
fi

# -- MariaDB ----------------------------------------------------------------

echo "== MariaDB =="

# 1-2. Reuse MySQL dumps — exercises the mariadb dialect parser on the shared
# MySQL-compatible subset. Hard-linked to save disk.
for name in employees.sql enwiki-categorylinks.sql.gz; do
  src="$DUMPS/mysql/$name"
  dst="$DUMPS/mariadb/$name"
  if have "$src" && ! have "$dst"; then
    ln "$src" "$dst" 2>/dev/null || cp "$src" "$dst"
    echo "linked $dst -> $src"
  fi
done

# 3. MariaDB-features at scale. Generates ~10 M rows (~500 MB) via
#    INSERT ... SELECT from a CTE, plus SEQUENCE, system-versioned table,
#    application-time PERIOD, JSON CHECK, INVISIBLE columns.
if ! have "$DUMPS/mariadb/mariadb-features-xl.sql"; then
  cat > "$DUMPS/mariadb/mariadb-features-xl.sql" <<'SQL'
-- MariaDB-specific features at scale. Loadable on mariadb:11+.
-- Target size after load: ~500 MB, ~10 M rows.

CREATE DATABASE IF NOT EXISTS mariadb_features
  DEFAULT CHARACTER SET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci;
USE mariadb_features;

CREATE SEQUENCE IF NOT EXISTS order_seq
  START WITH 1 INCREMENT BY 1 MINVALUE 1 NOCACHE;

CREATE TABLE IF NOT EXISTS employees (
  id         INT PRIMARY KEY,
  name       VARCHAR(120) NOT NULL,
  salary     DECIMAL(10,2) NOT NULL,
  hired_at   DATE NOT NULL,
  valid_from TIMESTAMP(6) GENERATED ALWAYS AS ROW START,
  valid_to   TIMESTAMP(6) GENERATED ALWAYS AS ROW END,
  PERIOD FOR SYSTEM_TIME (valid_from, valid_to)
) WITH SYSTEM VERSIONING;

CREATE TABLE IF NOT EXISTS contracts (
  id          BIGINT PRIMARY KEY,
  employee_id INT NOT NULL,
  terms       JSON NOT NULL CHECK (JSON_VALID(terms)),
  start_date  DATE NOT NULL,
  end_date    DATE NOT NULL,
  PERIOD FOR contract_period (start_date, end_date),
  KEY idx_contracts_emp (employee_id)
);

CREATE TABLE IF NOT EXISTS audit_log (
  id       BIGINT AUTO_INCREMENT PRIMARY KEY,
  event    VARCHAR(200) NOT NULL,
  payload  TEXT,
  inserted TIMESTAMP DEFAULT CURRENT_TIMESTAMP INVISIBLE
);

-- Populate 100k employees via seq_ table (MariaDB virtual sequences).
INSERT INTO employees (id, name, salary, hired_at)
SELECT seq,
       CONCAT('Employee #', seq),
       ROUND(30000 + RAND(seq) * 70000, 2),
       DATE_SUB(CURRENT_DATE, INTERVAL FLOOR(RAND(seq) * 4000) DAY)
FROM seq_1_to_100000;

-- Populate 10 M contracts — one batch per 1 M to avoid gigantic single INSERT.
-- Each iteration is a separate statement so the parser can consume them.
INSERT INTO contracts (id, employee_id, terms, start_date, end_date)
SELECT seq,
       1 + MOD(seq, 100000),
       JSON_OBJECT('role', ELT(1 + MOD(seq,3),'lead','eng','ops'),
                   'remote', IF(MOD(seq,2)=0,TRUE,FALSE),
                   'bonus',  ROUND(RAND(seq)*10000,2)),
       DATE_SUB(CURRENT_DATE, INTERVAL FLOOR(RAND(seq)*2000) DAY),
       DATE_ADD(CURRENT_DATE, INTERVAL FLOOR(RAND(seq)*2000) DAY)
FROM seq_1_to_1000000;

INSERT INTO contracts (id, employee_id, terms, start_date, end_date)
SELECT 1000000+seq,
       1 + MOD(seq, 100000),
       JSON_OBJECT('role','eng','remote',TRUE),
       CURRENT_DATE, CURRENT_DATE
FROM seq_1_to_1000000;

INSERT INTO contracts (id, employee_id, terms, start_date, end_date)
SELECT 2000000+seq,
       1 + MOD(seq, 100000),
       JSON_OBJECT('role','ops','remote',FALSE),
       CURRENT_DATE, CURRENT_DATE
FROM seq_1_to_1000000;

INSERT INTO contracts (id, employee_id, terms, start_date, end_date)
SELECT 3000000+seq,
       1 + MOD(seq, 100000),
       JSON_OBJECT('role','lead','remote',TRUE),
       CURRENT_DATE, CURRENT_DATE
FROM seq_1_to_1000000;

INSERT INTO contracts (id, employee_id, terms, start_date, end_date)
SELECT 4000000+seq,
       1 + MOD(seq, 100000),
       JSON_OBJECT('role','eng','remote',FALSE),
       CURRENT_DATE, CURRENT_DATE
FROM seq_1_to_1000000;

INSERT INTO audit_log (event, payload)
SELECT CONCAT('evt-', MOD(seq, 7)),
       CONCAT('payload for seq=', seq)
FROM seq_1_to_100000;
SQL
  echo "wrote $DUMPS/mariadb/mariadb-features-xl.sql (schema+generator)"
fi

# -- Oracle (official samples, kept as directories so _install.sql works) ----

echo "== Oracle =="

# Shallow-clone once, copy hr/oe/sh subtrees intact into dumps/oracle/.
if [[ ! -d "$DUMPS/oracle/hr" || ! -d "$DUMPS/oracle/oe" || ! -d "$DUMPS/oracle/sh" ]]; then
  tmp="$(mktemp -d)"
  git clone --depth=1 https://github.com/oracle-samples/db-sample-schemas.git \
    "$tmp/repo"
  [[ -d "$DUMPS/oracle/hr" ]] || cp -r "$tmp/repo/human_resources" "$DUMPS/oracle/hr"
  [[ -d "$DUMPS/oracle/oe" ]] || cp -r "$tmp/repo/order_entry"     "$DUMPS/oracle/oe"
  [[ -d "$DUMPS/oracle/sh" ]] || cp -r "$tmp/repo/sales_history"   "$DUMPS/oracle/sh"
  rm -rf "$tmp"
  echo "wrote $DUMPS/oracle/{hr,oe,sh}"
fi

# -- IBM DB2 LUW 11.5 --------------------------------------------------------

echo "== DB2 (LUW 11.5) =="

# IBM/db2-samples is ~400 MB total but only db_library/ and db2_graph/ are
# directly useful as squishy fixtures. Clone shallow then copy the two
# subtrees and discard the rest.
if [[ ! -d "$DUMPS/db2/db-library" || ! -d "$DUMPS/db2/graph" ]]; then
  tmp="$(mktemp -d)"
  git clone --depth=1 https://github.com/IBM/db2-samples.git "$tmp/repo"
  [[ -d "$DUMPS/db2/db-library" ]] || cp -r "$tmp/repo/db_library" "$DUMPS/db2/db-library"
  [[ -d "$DUMPS/db2/graph"      ]] || cp -r "$tmp/repo/db2_graph"  "$DUMPS/db2/graph"
  rm -rf "$tmp"
  echo "wrote $DUMPS/db2/{db-library,graph}"
fi

# `db2-features-xl.sql` is committed at repo level (analogous to
# mariadb-features-xl.sql) — fetch_dumps.sh leaves it alone if present.
if ! have "$DUMPS/db2/db2-features-xl.sql"; then
  echo "warn: dumps/db2/db2-features-xl.sql missing — restore from git or regenerate manually"
fi

echo
echo "Done. Sizes:"
du -sh "$DUMPS"/mysql/* "$DUMPS"/mariadb/* "$DUMPS"/oracle/* "$DUMPS"/db2/* 2>/dev/null || true
