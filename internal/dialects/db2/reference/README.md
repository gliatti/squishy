# IBM DB2 SQL grammar reference

This directory vendors the canonical IBM Db2 SQL grammar used as the
specification for squishy's hand-rolled recursive-descent parser.

## Files

| File | Purpose |
|---|---|
| `Db2Lexer.g4`  | ANTLR4 lexer rules for Db2 SQL + SQL PL |
| `Db2Parser.g4` | ANTLR4 parser rules for Db2 SQL + SQL PL |

Both files are **verbatim copies** from
[`antlr/grammars-v4`](https://github.com/antlr/grammars-v4), path
[`sql/db2/`](https://github.com/antlr/grammars-v4/tree/master/sql/db2),
fetched from `master` HEAD at the time of vendoring (no pinned commit hash:
upstream is treated as moving spec; diff on each upgrade).

## License

MIT — see `LICENSE-MIT`. Copyright (c) 2023, Michał Lorek.

## Role in squishy

These `.g4` files are the **specification, not generation input**. We do NOT
run the ANTLR code generator and we do NOT import an ANTLR Go runtime. The
hand-rolled parser under this directory's parent package
(`internal/dialects/db2/`) follows the rules of these grammars; when the Go
parser disagrees with the grammar, the grammar wins and the Go code is
updated to match.

## Coverage

### Parsed structurally

- DDL : `CREATE TABLE`, `CREATE INDEX [UNIQUE|CLUSTER] [INCLUDE]`,
  `CREATE VIEW [WITH [LOCAL|CASCADED] CHECK OPTION]`, `CREATE SEQUENCE`,
  `CREATE [DISTINCT] TYPE`, `CREATE [OR REPLACE] PROCEDURE`,
  `CREATE [OR REPLACE] FUNCTION`, `CREATE [OR REPLACE] TRIGGER`,
  `CREATE ALIAS`, `ALTER TABLE` (ADD/ALTER/DROP COLUMN, ADD/DROP CONSTRAINT,
  ADD/DROP PARTITION RANGE, RENAME COLUMN), `DROP …`,
  `COMMENT ON …`, `LABEL ON …`.
- SQL PL : `BEGIN [NOT] ATOMIC … END`, `DECLARE` (variables, conditions,
  cursors, handlers `CONTINUE/EXIT/UNDO`), `IF/CASE/WHILE/REPEAT/FOR/LOOP`,
  `ITERATE/LEAVE`, `OPEN/FETCH/CLOSE`, `SIGNAL`/`RESIGNAL`,
  `GET DIAGNOSTICS`, dynamic SQL `EXECUTE IMMEDIATE`, `PREPARE`/`EXECUTE`.
- Identités : `GENERATED [ALWAYS|BY DEFAULT] AS IDENTITY`,
  `GENERATED ALWAYS AS (expr)`.
- Partitionnement : `PARTITION BY RANGE` (LUW), partitions multi-fichiers.
- Types : `SMALLINT`, `INTEGER`, `BIGINT`, `DECIMAL(p,s)`, `NUMERIC(p,s)`,
  `DECFLOAT(16|34)`, `REAL`, `DOUBLE`, `FLOAT(p)`, `CHAR(n)`, `VARCHAR(n)`,
  `CHAR/VARCHAR FOR BIT DATA`, `BINARY(n)`, `VARBINARY(n)`, `GRAPHIC(n)`,
  `VARGRAPHIC(n)`, `DBCLOB(n)`, `CLOB(n)`, `BLOB(n)`, `XML`, `BOOLEAN`
  (LUW 11+), `DATE`, `TIME`, `TIMESTAMP[(n)]`,
  `TIMESTAMP[(n)] WITH TIME ZONE` (LUW only), `ROWID`, `LONG VARCHAR`,
  `LONG VARGRAPHIC`, distinct types, array types.

### Raw passthrough

- DML internals (`SELECT`, `INSERT`, `UPDATE`, `DELETE`, `MERGE`) — preserved
  in raw text by the lexer and lexically rewritten by
  `internal/translate/db2_body_xlate.go`.
- Storage / `TABLESPACE` / `INDEX IN` / `COMPRESSION YES` clauses — preserved
  in `TableOptions.DB2*` and emitted as warnings (PG has no equivalent).

## Variants

| Kind id   | Engine                | Catalog       | Notes |
|-----------|-----------------------|---------------|-------|
| `db2`     | DB2 LUW 11.5          | `SYSCAT.*`    | primary target |
| `db2zos`  | DB2 for z/OS          | `SYSIBM.*`    | strict subset of LUW DDL/SQL PL; type mapper differs (no BOOLEAN, no TIMESTAMPTZ) |

## Upgrade procedure

1. Fetch fresh copies from upstream:
   ```
   curl -sSL -o Db2Lexer.g4  https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/db2/Db2Lexer.g4
   curl -sSL -o Db2Parser.g4 https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/db2/Db2Parser.g4
   ```
2. Diff against the previous version. For any new rule touching our subset
   (see Coverage above), update:
   - `internal/dialects/db2/token.go` — add new keywords.
   - `internal/dialects/db2/lexer.go` — add new lexical constructs.
   - `internal/dialects/db2/parser_ddl.go` / `parser_sqlpl.go` —
     add new statement forms.
   - `internal/sqlparse/ast/` — add new AST node types if semantics demand.
3. Add a unit test per new construct under
   `internal/dialects/db2/parser_*_test.go`.
4. Re-run `make test`.

## Known limitations

- `ROWID` is translated to `text` with a diagnostic warning (no PG
  equivalent; the migration plan suggests replacing with a UUID column).
- `DECFLOAT(16|34)` → `NUMERIC` (PG has no IEEE 754-2008 decimal floating
  point; the warning advises about loss of decimal-rounding semantics).
- `GRAPHIC` / `VARGRAPHIC` / `DBCLOB` (UCS-2 encoding) → `TEXT` — PG is
  UTF-8 native and preserves the data round-trip.
- z/OS-specific DDL clauses (`STOGROUP`, `BUFFERPOOL`, `EDITPROC`,
  `VALIDPROC`, `FIELDPROC`) are parsed but stripped at PG emission with an
  info-level warning.
- `BEGIN … END` SQL PL routines are translated to `DO $$ … $$` /
  PL/pgSQL `LANGUAGE plpgsql` blocks. SQL PL handlers (`CONTINUE`/`EXIT`/
  `UNDO`) are mapped to `EXCEPTION WHEN …` blocks — `UNDO` semantics
  (full sub-block rollback) are simulated with a savepoint pattern.
- `GENERATE_UNIQUE()` requires the `pgcrypto` extension on the target
  (emitted as a `Prerequisite`).
