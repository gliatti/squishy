# Oracle PL/SQL grammar reference

This directory vendors the canonical Oracle PL/SQL grammar used as the
specification for squishy's hand-rolled recursive-descent parser.

## Files

| File | Purpose |
|---|---|
| `PlSqlLexer.g4` | ANTLR4 lexer rules for Oracle SQL + PL/SQL |
| `PlSqlParser.g4` | ANTLR4 parser rules for Oracle SQL + PL/SQL |

Both files are **verbatim copies** from
[`antlr/grammars-v4`](https://github.com/antlr/grammars-v4), path
[`sql/plsql/`](https://github.com/antlr/grammars-v4/tree/master/sql/plsql).

## License

Apache License 2.0 — see the header of each `.g4` file for full attribution:

- Copyright (c) 2009–2011 Alexandre Porcelli
- Copyright (c) 2015–2019 Ivan Kochurkin (Positive Technologies)
- Copyright (c) 2017–2018 Mark Adams

Apache 2.0 text: <https://www.apache.org/licenses/LICENSE-2.0>

## Role in squishy

These `.g4` files are the **specification, not generation input**. We do NOT
run the ANTLR code generator and we do NOT import an ANTLR Go runtime. The
hand-rolled parser under this directory's parent package (`internal/dialects/
oracle/`) follows the rules of these grammars; when the Go parser disagrees
with the grammar, the grammar wins and the Go code is updated to match.

Coverage we implement (subset):

- DDL: `CREATE TABLE`, `CREATE INDEX`, `CREATE VIEW`, `CREATE MATERIALIZED
  VIEW`, `CREATE SEQUENCE`, `CREATE SYNONYM`, `CREATE TYPE`, `CREATE PACKAGE`,
  `CREATE PACKAGE BODY`, `CREATE PROCEDURE`, `CREATE FUNCTION`, `CREATE
  TRIGGER`, `ALTER TABLE` (add/modify/drop column + constraints), `DROP`.
- PL/SQL: blocks (`DECLARE/BEGIN/EXCEPTION/END`), control flow (`IF`, `CASE`,
  `LOOP`, `WHILE`, `FOR` numeric/cursor, `FORALL`), cursors (explicit and
  for-loop), exceptions, `EXECUTE IMMEDIATE`, `RAISE`, `RETURN`, assignments,
  `BULK COLLECT INTO`, pragmas.
- Types: `NUMBER`, `VARCHAR2`, `NVARCHAR2`, `CHAR`, `NCHAR`, `CLOB`, `NCLOB`,
  `BLOB`, `RAW`, `LONG [RAW]`, `DATE`, `TIMESTAMP[(n)] [WITH [LOCAL] TIME ZONE]`,
  `INTERVAL YEAR TO MONTH`, `INTERVAL DAY TO SECOND`, `ROWID`, `UROWID`,
  `BINARY_FLOAT`, `BINARY_DOUBLE`, `XMLTYPE`, `BFILE`, `VECTOR` (23ai),
  native `JSON` and `BOOLEAN` (23c).

Coverage we intentionally do NOT parse structurally (captured as raw text
and/or passed through to PostgreSQL after lexical rewriting):

- DML statement internals (`SELECT`, `INSERT`, `UPDATE`, `DELETE`, `MERGE`) —
  treated as raw passthrough by the body rewriter.
- Storage / partitioning clauses — preserved in `TableOptions.Oracle*` fields.

## Upgrade procedure

1. Fetch fresh copies from upstream:
   ```
   curl -sSL -o PlSqlLexer.g4  https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/plsql/PlSqlLexer.g4
   curl -sSL -o PlSqlParser.g4 https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/plsql/PlSqlParser.g4
   ```
2. Diff against the previous version. For any new rule touching our subset
   (see Coverage above), update:
   - `internal/dialects/oracle/token.go` — add new keywords.
   - `internal/dialects/oracle/lexer.go` — add new lexical constructs.
   - `internal/dialects/oracle/parser_ddl.go` or `parser_plsql.go` — add new
     statement forms.
   - `internal/sqlparse/ast/` — add new AST node types if semantics demand.
3. Add a unit test per new construct under
   `internal/dialects/oracle/parser_test.go`.
4. Re-run `make test`.

## Known limitations

- `BFILE`, `UROWID`, and `ROWID` are translated to `text` with a warning
  emitted in the migration plan — no direct PG equivalent.
- `PRAGMA AUTONOMOUS_TRANSACTION` has no native PG equivalent. Suggested
  workarounds (`dblink`, `pg_background`) are emitted as plan notes.
- Object types (`CREATE TYPE ... AS OBJECT`) with member methods are parsed
  but translated to PG composite types only. Methods become standalone
  functions prefixed with the type name.
- Nested tables and `VARRAY` are translated to PG arrays of the element type,
  losing the bounded/ordered distinction.
