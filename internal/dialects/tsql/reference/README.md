# Microsoft SQL Server (T-SQL) — grammar reference

The `.g4` files in this directory are **vendored from the [grammars-v4][gv4]
project** (`sql/tsql`), preserved verbatim with their original MIT license and
attribution. The upstream directory has no separate license file: the MIT
notice lives in the header of each grammar and is copied in `LICENSE.txt`.

| File             | Source                                                       |
|------------------|--------------------------------------------------------------|
| `TSqlLexer.g4`   | `grammars-v4/sql/tsql/TSqlLexer.g4`  (MIT, © Mark Adams, Ivan Kochurkin / Positive Technologies, Scott Ure, Rui Zhang, Marcus Henriksson) |
| `TSqlParser.g4`  | `grammars-v4/sql/tsql/TSqlParser.g4` (MIT, same authors)    |
| `LICENSE.txt`    | the MIT notice of the grammar headers                       |

[gv4]: https://github.com/antlr/grammars-v4

## Role in the codebase

These files are the **canonical specification** of what squishy's T-SQL parser
understands. The hand-rolled Go parser (`../lexer.go`, `../parser*.go`)
implements the subset of rules required for migration: the DDL of tables,
indexes, views, procedures, functions, triggers, types, sequences and schemas
(`ddl_clause` and below), `sp_addextendedproperty` comments, the DML used in
views and bodies (`select_statement_standalone`, `insert_statement`,
`update_statement`, `delete_statement`, `merge_statement`), the expression
grammar (`expression`, `search_condition`, `function_call`, `over_clause`)
and the procedural statements (`cfl_statement`, `declare_statement`,
`execute_statement`, `cursor_statement`, `transaction_statement`). The sqlcmd
layer (`GO`, `:setvar`, `$(var)`) is handled by the lexer.

**The `.g4` is the source of truth.** When the Go parser diverges, we update
the Go code to match the grammar — never the other way around.

We do **not** run ANTLR at build time. No `antlr4` binary, no runtime JAR, no
generated `*.go` files. The `.g4` files are documentation-as-code.

## Upgrading

To refresh from upstream:

```bash
curl -sSfL -o TSqlLexer.g4  https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/tsql/TSqlLexer.g4
curl -sSfL -o TSqlParser.g4 https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/tsql/TSqlParser.g4
```

Then diff, refresh `LICENSE.txt` if the header changed, and if the parser
needs to catch up, open an issue tagged `grammar/tsql`.

## License

The `.g4` files are MIT-licensed by their original authors. The hand-rolled Go
parser is covered by the root `LICENSE` file (PostgreSQL License, © Dalibo).
The two are independent: the grammar is reference documentation we follow, not
a derivative work that would subject our parser to the MIT terms.
