# PostgreSQL — grammar reference

The `.g4` files in this directory are **vendored from the [grammars-v4][gv4]
project** (`sql/postgresql/`), preserved verbatim with their original license
and attribution.

| File                  | Upstream path                                      |
|-----------------------|----------------------------------------------------|
| `PostgreSQLLexer.g4`  | `grammars-v4/sql/postgresql/PostgreSQLLexer.g4`    |
| `PostgreSQLParser.g4` | `grammars-v4/sql/postgresql/PostgreSQLParser.g4`   |

[gv4]: https://github.com/antlr/grammars-v4

## Role in the codebase

squishy emits its target DDL through a PostgreSQL-side **AST + writer**
(`ast.go` + `writer.go` in the parent directory), structurally mirroring the
MySQL-side parser. The `.g4` files serve as the canonical specification of
what valid PostgreSQL syntax looks like — they anchor the emitter so that
every node we print maps 1:1 to a production in the reference grammar.

The AST is intentionally narrow: it covers only the statements squishy
generates for a migration (CREATE SCHEMA, CREATE SEQUENCE, CREATE TABLE with
columns/constraints/checks, CREATE INDEX, ALTER TABLE ... ADD CONSTRAINT,
COMMENT ON, CREATE TRIGGER / FUNCTION stubs). Full SQL grammar coverage is
out of scope for v1 — if the user wants to audit a wider surface, the `.g4`
is the reference they (and any future contributor) target.

## Symmetric architecture

```
MySQL text  ──► dialects/mysql/parser  ──► shared AST (MySQL-shaped)
                                               │
                                               ▼
                                        translate/  ──── SchemaPlan
                                                            │
                                                            ▼
                                                 dialects/postgres/ast  ──► writer.go ──► PG DDL text
```

The MySQL grammar parses **into** the AST; the PostgreSQL grammar informs the
shape of the nodes we **emit**. Both dialects share the registry interface
(`dialects.Dialect`) and coexist without either depending on the other.

## Upgrading

```bash
curl -sL -o PostgreSQLLexer.g4  https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/postgresql/PostgreSQLLexer.g4
curl -sL -o PostgreSQLParser.g4 https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/postgresql/PostgreSQLParser.g4
```

Review the diff and adjust the emitter if new constructs need coverage.

## License

The `.g4` files carry their upstream license (BSD-style, see header
comments). Squishy's Go emitter code is covered by the root `LICENSE` file
(PostgreSQL License, © Dalibo). The two are independent: the grammar is
specification we follow, not a derivative work governing our code.
