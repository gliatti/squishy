# MySQL / MariaDB — grammar reference

The `.g4` files in this directory are **vendored from the [grammars-v4][gv4]
project** (Positive-Technologies MySQL subgrammar), preserved verbatim with
their original MIT license and attribution.

| File             | Source                                                                                      |
|------------------|---------------------------------------------------------------------------------------------|
| `MySqlLexer.g4`  | `grammars-v4/sql/mysql/Positive-Technologies/MySqlLexer.g4`  (MIT, © Positive Technologies) |
| `MySqlParser.g4` | `grammars-v4/sql/mysql/Positive-Technologies/MySqlParser.g4` (MIT, © Positive Technologies) |

[gv4]: https://github.com/antlr/grammars-v4

## Role in the codebase

These files are the **canonical specification** of what squishy's MySQL parser
understands. The hand-rolled Go parser (`parser.go`, `lexer.go`) implements the
subset of rules required for migration (`ddlStatement`, `createView`,
`createTrigger`, `createProcedure`, `createFunction`, `createEvent`, plus the
expression fragments needed for `DEFAULT` / `CHECK` / generated columns).

**The `.g4` is the source of truth.** When the Go parser diverges, we update
the Go code to match the grammar — never the other way around.

We do **not** run ANTLR at build time. No `antlr4` binary, no runtime JAR, no
generated `*.go` files. The `.g4` files are documentation-as-code.

## Upgrading

To refresh from upstream:

```bash
curl -sL -o MySqlLexer.g4  https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/mysql/Positive-Technologies/MySqlLexer.g4
curl -sL -o MySqlParser.g4 https://raw.githubusercontent.com/antlr/grammars-v4/master/sql/mysql/Positive-Technologies/MySqlParser.g4
```

Then diff, and if the parser needs to catch up, open an issue tagged
`grammar/mysql`.

## License

The `.g4` files are MIT-licensed by their original authors. The hand-rolled Go
parser is covered by the root `LICENSE` file (PostgreSQL License, © Dalibo).
The two are independent: the grammar is reference documentation we follow, not
a derivative work that would subject our parser to the MIT terms.
