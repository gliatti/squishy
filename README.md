# squishy

> Web UI for piloting **MySQL / MariaDB → PostgreSQL** migrations, with a
> guided 5-step wizard, a Postgres-backed job queue, real-time monitoring,
> and per-batch error recovery.

Part of the [Dalibo](https://dalibo.com) PostgreSQL tool family — inspired by
(but independent of) [`data2pg`](../data2pg), [`pg_migrate`](../pg_migrate)
and [`transqlate`](../transqlate).

## Highlights

- **Vue 3 wizard**, five steps: project → source → target → DDL transformation
  → data transformation → summary → launch.
- **Go API** (chi + pgx), with **PostgreSQL-hosted job queue** (`SELECT … FOR
  UPDATE SKIP LOCKED`) and **in-process worker pool**.
- **Hand-rolled SQL parser per dialect**, following canonical grammar sources
  (the MySQL dialect vendors [grammars-v4 Positive-Technologies][gv4], MIT).
  **No ANTLR runtime.** See [`internal/dialects/README.md`](internal/dialects/README.md).
- **Exhaustive translator**: numeric/string/temporal/spatial types, ENUM/SET,
  AUTO_INCREMENT → IDENTITY, ON UPDATE CURRENT_TIMESTAMP → trigger, generated
  columns, views, triggers, procedures, functions, events → pg_cron snippets.
- **Per-table × 10 000-row batches**, idempotent COPY FROM STDIN, retry on
  individual batches, orphan-lock recovery.
- **Entirely dockerized dev + test**: `make up`, `make test`, `make e2e` —
  no local Go / Node toolchain needed.

[gv4]: https://github.com/antlr/grammars-v4

## Quick start

```bash
make up              # postgres + mysql-sample + api + web
open http://localhost:5173
```

The `mysql-sample` container auto-loads an exhaustive fixture (`test/fixtures/
mysql_sample.sql` + `mysql_seed.sql`) covering every MySQL type/feature the
translator handles.

To end-to-end-test:

```bash
make e2e             # unit tests + full migration round-trip + row-count checks
```

## Architecture

```
  ┌─────────────┐   HTTP + SSE   ┌──────────────────────────────────────┐
  │   Vue 3     │◀──────────────▶│  Go API (chi)                        │
  │   wizard    │                │  ├─ httpapi/  handlers + SSE bus     │
  └─────────────┘                │  ├─ dialects/<id>/  parser + AST     │
                                 │  ├─ translate/      AST → PG plan    │
                                 │  ├─ planner/        DAG of steps     │
                                 │  ├─ queue/          FOR UPDATE SKIP…  │
                                 │  ├─ worker/         goroutine pool    │
                                 │  ├─ dataxfer/       partition + COPY  │
                                 │  └─ events/         in-proc pub/sub   │
  ┌─────────────┐                │                                      │
  │ MySQL 8     │◀───read────────┤  (source, per-project connection)    │
  └─────────────┘                │                                      │
  ┌─────────────┐                │                                      │
  │ Postgres 17 │◀──app DB───────┤  projects / runs / steps / jobs      │
  │             │◀──target───────┤  (writes migrated schema + data)     │
  └─────────────┘                └──────────────────────────────────────┘
```

## Layout

```
cmd/squishy/          main entry point (HTTP + worker pool + dispatcher)
internal/
├── config/           env parsing
├── storage/          pgx pool + embedded migrations runner
├── storage/migrations/  golang-migrate .up/.down SQL files
├── project/          domain: projects, connections, migrations
├── connection/       source/target pool factories + test probe
├── inspect/          MySQL introspection (SHOW CREATE TABLE & friends)
├── dialects/         SQL dialect registry (MySQL v1; ~20 planned)
│   └── mysql/
│       ├── reference/  .g4 vendored from grammars-v4 (MIT)
│       ├── lexer.go  parser.go  parser_ddl.go  token.go
│       └── dialect.go
├── sqlparse/ast/     shared AST types across dialects
├── translate/        MySQL AST → PG SchemaPlan + DDL + explanations
├── planner/          migration → ordered DAG of steps
├── queue/            Postgres-backed job queue (claim / fail / retry / requeue)
├── worker/           pool + dispatcher + per-kind handlers
├── dataxfer/         partition by PK, SELECT + COPY FROM STDIN, idempotency
├── events/           in-process pub/sub + SSE adapter
├── httpapi/          router + handlers + SSE streaming
└── version/
web/                  Vue 3 + Vite + Pinia
test/
├── fixtures/         mysql_sample.sql + mysql_seed.sql
└── integration/      e2e_test.go (tag: e2e)
```

## API

All routes are JSON, prefix `/api/v1`. See `internal/httpapi/`.

| Method | Path                                               | Role |
|--------|----------------------------------------------------|------|
| POST   | `/projects`                                        | create |
| GET    | `/projects`                                        | list |
| GET/DELETE | `/projects/{id}`                               | read / delete |
| PUT    | `/projects/{id}/connections/{source\|target}`      | upsert DSN |
| POST   | `/projects/{id}/connections/{role}/test`           | probe |
| POST   | `/projects/{id}/inspect`                           | MySQL introspection |
| POST   | `/projects/{id}/plan`                              | build DDL + explanations |
| POST   | `/migrations/{id}/runs`                            | start a run |
| GET    | `/runs/{id}`                                       | progress snapshot |
| GET    | `/runs/{id}/steps`                                 | steps detail |
| GET    | `/runs/{id}/events`                                | **SSE stream** |
| POST   | `/runs/{id}/retry`                                 | requeue failed jobs |
| POST   | `/runs/{id}/cancel`                                | kill pending/running |

SSE event kinds: `run.status`, `step.status`, `batch.progress`, `log`.

## Adding a dialect

See [`internal/dialects/README.md`](internal/dialects/README.md). TL;DR:
vendor the upstream `.g4`, write `lexer.go` + `parser.go` to match, register
via an `init()`.

## License

Code under `LICENSE` (PostgreSQL License, © Dalibo). Vendored grammar files
retain their upstream licenses; see each `reference/README.md`.
