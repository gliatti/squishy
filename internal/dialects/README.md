# `internal/dialects/` — SQL dialects registry

squishy supports MySQL 8 / MariaDB 11, Oracle 19c / 23ai, and **IBM DB2 11.5
(LUW)** in source today. DB2 for z/OS shares the parser at the `db2zos` Kind
(catalog views diverge — see `internal/dialects/db2/` and
`internal/dataxfer/source_dialect.go`). Additional dialects are planned (T-SQL /
SQL Server 2016–2022, PostgreSQL 11–17, Sybase ASE, Snowflake, Presto, Hive,
SQLite…). Each appears here as its own subpackage.

## Layout per dialect

```
dialects/<id>/
├── reference/                # vendored grammar source of truth
│   ├── *.g4                  # from grammars-v4 or another canonical project
│   └── README.md             # attribution + upgrade instructions
├── dialect.go                # Dialect interface impl + init() registration
├── lexer.go
├── parser.go
├── token.go
└── errors.go
```

## Sourcing grammars

We systematically reuse existing, publicly maintained grammars rather than
handwriting them. For most SQL dialects the canonical home is the ANTLR
[grammars-v4][gv4] repo:

| Dialect            | Upstream path in grammars-v4                   | License |
|--------------------|------------------------------------------------|---------|
| MySQL / MariaDB    | `sql/mysql/Positive-Technologies`              | MIT     |
| PostgreSQL         | `sql/postgresql`                               | MIT     |
| Oracle PL/SQL      | `sql/plsql`                                    | MIT     |
| T-SQL (SQL Server) | `sql/tsql`                                     | MIT     |
| DB2                | `sql/db2`                                      | MIT     |
| SQLite             | `sql/sqlite`                                   | MIT     |
| Snowflake          | `sql/snowflake`                                | Apache-2|
| Hive               | `sql/hive`                                     | MIT     |
| Presto / Trino     | `sql/presto`                                   | Apache-2|

[gv4]: https://github.com/antlr/grammars-v4

When no mature ANTLR grammar exists (or the license is incompatible), we
vendor the official BNF/EBNF from the vendor's reference manual or another
open-source parser project, clearly documented in the dialect's
`reference/README.md`.

## Zero runtime dependency on ANTLR

The `.g4` files live here as **specification**. We do not run the ANTLR code
generator and we do not import an ANTLR Go runtime. Each dialect ships a
hand-rolled recursive-descent parser following the rules defined in its
reference `.g4`. This gives us full control over error recovery, position
tracking, raw-body preservation, and AST shape — at the cost of implementing
each dialect subset explicitly.

## Adding a new dialect

1. Create `dialects/<id>/` and `dialects/<id>/reference/`.
2. Vendor the canonical `.g4` (or equivalent) into `reference/`, with the
   upstream license file alongside and a `README.md` documenting source +
   upgrade procedure.
3. Implement `token.go`, `lexer.go`, `parser.go` following the reference.
4. Implement `dialect.go` with a `Dialect` struct and an `init()` that calls
   `dialects.Register("<id>", …)`.
5. Add the id to the enum accepted by the HTTP `PUT /connections/source`
   endpoint and to the Vue.js wizard dropdown.
6. Add fixtures + unit tests under `dialects/<id>/`, plus an end-to-end
   scenario in `test/integration/` when a runtime source is available.
