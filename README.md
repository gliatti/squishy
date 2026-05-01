<p align="center">
  <img src="docs/logo.svg" alt="squishy" width="320"/>
</p>

<p align="center">
  <em>Web UI + Go API for piloting <strong>MySQL / MariaDB / Oracle / IBM DB2 → PostgreSQL</strong> migrations,
  with a Postgres-backed job queue, real-time SSE monitoring, per-batch error recovery,
  and an MCP server for agentic clients.</em>
</p>

<p align="center">
  <a href="https://go.dev/"><img alt="Go 1.23" src="https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white"/></a>
  <a href="https://vuejs.org/"><img alt="Vue 3" src="https://img.shields.io/badge/Vue-3-4FC08D?logo=vue.js&logoColor=white"/></a>
  <a href="https://vitejs.dev/"><img alt="Vite" src="https://img.shields.io/badge/Vite-5-646CFF?logo=vite&logoColor=white"/></a>
  <a href="https://www.postgresql.org/"><img alt="PostgreSQL 17" src="https://img.shields.io/badge/PostgreSQL-17-4169E1?logo=postgresql&logoColor=white"/></a>
  <img alt="MySQL" src="https://img.shields.io/badge/MySQL-8.4-4479A1?logo=mysql&logoColor=white"/>
  <img alt="MariaDB" src="https://img.shields.io/badge/MariaDB-11-003545?logo=mariadb&logoColor=white"/>
  <img alt="Oracle" src="https://img.shields.io/badge/Oracle-19c%20%2F%2023ai-F80000?logo=oracle&logoColor=white"/>
  <img alt="IBM DB2" src="https://img.shields.io/badge/IBM%20DB2-11.5%20LUW-052FAD?logo=ibm&logoColor=white"/>
  <a href="https://www.docker.com/"><img alt="Docker" src="https://img.shields.io/badge/Docker-compose-2496ED?logo=docker&logoColor=white"/></a>
  <a href="LICENSE"><img alt="License: PostgreSQL" src="https://img.shields.io/badge/license-PostgreSQL-336791"/></a>
</p>

Part of the [Dalibo](https://dalibo.com) PostgreSQL tool family.

## Highlights

- **Vue 3 frontend** (Pinia + Vite) organized around the resource hierarchy
  *project → instance → migration → run*, with a live run monitor (DAG
  view, level lanes, per-batch progress, DDL / execution / warnings tabs).
- **Go API** (chi + pgx), **PostgreSQL-hosted job queue** (`SELECT … FOR
  UPDATE SKIP LOCKED`) with an in-process worker pool and a step DAG
  promoted by a dedicated dispatch loop.
- **Hand-rolled SQL parser per dialect**, no ANTLR runtime. Vendored `.g4`
  grammars live under `internal/dialects/<id>/reference/` as spec only
  (MIT for upstreams from [grammars-v4][gv4] where applicable). See
  [`internal/dialects/README.md`](internal/dialects/README.md).
- **AST-only translation pipeline** — a `make check-no-regex` guard rail
  fails the build if `regexp.MustCompile` reappears under
  `internal/translate` or `internal/dialects`.
- **Exhaustive translator**: numeric / string / temporal / spatial types,
  ENUM / SET, AUTO_INCREMENT → IDENTITY, ON UPDATE CURRENT_TIMESTAMP →
  trigger, generated columns, views, triggers, procedures, functions,
  MySQL events → pg_cron snippets, Oracle PL/SQL packages, DB2 routines.
- **Per-table × 10 000-row batches**, idempotent `COPY FROM STDIN`,
  individual batch retry, orphan-lock recovery.
- **MCP server** (`mcp-server/`) exposing the same workflow to agentic
  clients (Claude, etc.) over HTTP on `:8002`.
- **Entirely dockerized dev + test**: `make up`, `make test`, `make e2e` —
  no local Go / Node toolchain needed. DB2 is dynamically linked against
  IBM clidriver and lives in a dedicated `dev-db2` Debian stage.

[gv4]: https://github.com/antlr/grammars-v4

## Quick start

```bash
make up              # postgres + mysql-sample + api + web + squishy-mcp
open http://localhost:5173
```

The `mysql-sample` container auto-loads an exhaustive fixture
(`test/fixtures/mysql_sample.sql` + `mysql_seed.sql`) covering every
MySQL type/feature the translator handles. MariaDB, Oracle 23ai, Oracle
19c and DB2 LUW samples are opt-in via Compose profiles:

```bash
make oracle-up       # Oracle 23ai (profile: oracle)
make oracle19-up     # Oracle 19c   (profile: oracle19, requires container-registry.oracle.com login)
make db2-up          # DB2 11.5 LUW (profile: db2, ~4-5 min first boot, EULA accepted)
```

End-to-end gate (unit tests + Compose stack + migration scenarios):

```bash
make e2e             # MySQL flow by default
SQUISHY_E2E_DB2=1 make e2e   # also runs the DB2 scenario
```

## Architecture

```
  ┌─────────────┐   HTTP + SSE   ┌──────────────────────────────────────┐
  │   Vue 3     │◀──────────────▶│  Go API (chi)                        │
  │  frontend   │                │  ├─ httpapi/    handlers + SSE bus   │
  └─────────────┘                │  ├─ dialects/<id>/  parser + AST     │
                                 │  ├─ sqlparse/ast/  shared AST types  │
                                 │  ├─ discover/   source introspection │
  ┌─────────────┐                │  ├─ translate/  AST → PG SchemaPlan  │
  │ MCP client  │◀──MCP/HTTP────▶│  ├─ planner/    DAG of steps         │
  └─────────────┘                │  ├─ queue/      FOR UPDATE SKIP …    │
                                 │  ├─ worker/     goroutine pool       │
  ┌─────────────┐                │  ├─ dataxfer/   partition + COPY     │
  │ MySQL 8     │                │  ├─ events/     in-proc pub/sub      │
  │ MariaDB 11  │◀───read────────┤  └─ connection/ per-run pool cache   │
  │ Oracle 19c/23ai             │                                       │
  │ DB2 11.5 LUW│                │                                      │
  └─────────────┘                │                                      │
  ┌─────────────┐                │                                      │
  │ Postgres 17 │◀──app DB───────┤  squishy.{projects,instances,        │
  │             │                │           migrations,runs,steps,jobs}│
  │             │◀──target───────┤  (writes migrated schema + data)     │
  └─────────────┘                └──────────────────────────────────────┘
```

The app schema is `squishy.*`; the migrated target schema is `mig` by
convention — `make reset-dest` drops only the latter.

## Domain model

A **project** holds Postgres admin credentials. Each project owns one or
more **instances** (a source DB + super-user DSN). Each instance, once
discovered, exposes one or more **migrations** (schema-scoped translation
plans). A **run** is one execution of a migration; runs spawn **steps**
(arranged in a DAG and grouped into levels), and steps spawn **batches**
for table copies.

## Layout

```
cmd/squishy/          main entry point (HTTP + worker pool + dispatch + run-state loops)
mcp-server/           MCP-over-HTTP server wrapping the same API (port 8002)
internal/
├── config/           env parsing
├── storage/          pgx pool + embedded migrations runner
│   └── migrations/   golang-migrate .up/.down SQL files
├── project/          domain: projects / instances / migrations
├── connection/       source/target pool factories + DB2 cgo build tag
├── discover/         source introspection (MySQL / MariaDB / Oracle / DB2)
├── dialects/         SQL dialect registry — currently mysql, mariadb, oracle, db2, postgres
│   └── <id>/
│       ├── reference/  vendored .g4 (spec only, no ANTLR runtime)
│       ├── lexer.go  parser.go  parser_ddl.go  parser_dml.go  parser_expr.go  …
│       ├── token.go  errors.go
│       └── dialect.go  (registers via init())
├── sqlparse/ast/     shared AST types across dialects
├── translate/        AST → Postgres SchemaPlan + DDL + human explanations
├── planner/          migration → ordered DAG of steps (levels)
├── queue/            Postgres-backed job queue (claim / fail / retry / requeue)
├── worker/           pool + per-kind handlers
├── dataxfer/         partition by PK, SELECT + COPY FROM STDIN, idempotency
├── events/           in-process pub/sub (also drives SSE)
├── httpapi/          chi router + handlers + SSE streaming
└── version/
web/                  Vue 3 + Vite + Pinia
test/
├── fixtures/         sample dumps + seeds (mysql / oracle / oracle19 / db2)
└── integration/      e2e_test.go + db2_e2e_test.go (build tag: e2e)
```

## API

All routes are JSON, prefix `/api/v1`. See `internal/httpapi/router.go`.

| Method | Path                                                | Role |
|--------|-----------------------------------------------------|------|
| GET / POST | `/projects`                                     | list / create |
| GET / PUT / DELETE | `/projects/{projectID}`                 | read / update / delete |
| GET / POST | `/projects/{projectID}/instances`               | list / create |
| GET / PUT / DELETE | `/instances/{instanceID}`               | read / update / delete |
| POST   | `/instances/{instanceID}/test-connection`           | probe source DSN |
| POST   | `/instances/{instanceID}/test-target-connection`    | probe Postgres admin DSN |
| POST   | `/instances/{instanceID}/rediscover`                | re-introspect source schema |
| GET    | `/instances/{instanceID}/migrations`                | list migrations |
| GET    | `/migrations/{migrationID}`                         | read |
| POST   | `/migrations/{migrationID}/inspect`                 | source introspection |
| POST   | `/migrations/{migrationID}/plan`                    | build DDL + explanations |
| GET    | `/migrations/{migrationID}/prerequisites`           | list prereq checks |
| POST   | `/migrations/{migrationID}/prerequisites/ack`       | acknowledge prereqs |
| GET / POST | `/migrations/{migrationID}/runs`                | list / start a run |
| GET    | `/runs/{runID}`                                     | progress snapshot |
| GET    | `/runs/{runID}/steps`                               | steps detail |
| GET    | `/runs/{runID}/steps/{stepID}/batches`              | batches detail |
| POST   | `/runs/{runID}/steps/{stepID}/play`                 | enqueue a single step |
| POST   | `/runs/{runID}/steps/{stepID}/replay`               | replay a single step |
| POST   | `/runs/{runID}/levels/{level}/play`                 | enqueue an entire level |
| POST   | `/runs/{runID}/levels/{level}/replay`               | replay an entire level |
| POST   | `/runs/{runID}/retry`                               | requeue failed jobs |
| POST   | `/runs/{runID}/cancel`                              | kill pending/running |
| GET    | `/runs/{runID}/events`                              | **SSE stream** |

Top-level: `/healthz`, `/readyz`, `/version`.

SSE event kinds: `run.status`, `step.status`, `batch.progress`, `log`.

## Job kinds

The worker registers a fixed set of kinds (`cmd/squishy/main.go`):
`inspect`, `create_target_db`, `create_ddl`, `copy_table`, `copy_batch`,
`create_index`, `create_fk`, `create_routine`, `validate`. Handler bodies
live in `internal/worker`. Data copy partitions by PK into 10 000-row
batches (`SQUISHY_BATCH_SIZE`) with idempotent `COPY FROM STDIN`; batches
retry independently of the parent step.

## No regex on SQL — AST only

Any SQL transformation goes through the AST: source text → hand-rolled
parser → typed `internal/sqlparse/ast` nodes → translator passes →
typed Postgres writer (`internal/dialects/postgres`). `make test` runs a
`check-no-regex` guard that fails on any new `regexp.MustCompile` or
`"regexp"` import inside `internal/translate` or `internal/dialects`.
The lexer is the only place that walks SQL text byte-by-byte.

## MCP server

`mcp-server/` exposes the same workflow over MCP-over-HTTP on port 8002,
wrapping project / instance / migration / run lifecycle plus per-step
play/replay actions.

```bash
make mcp-up          # start (already part of `make up`)
make mcp-logs        # tail
make mcp-build       # build distroless prod image squishy-mcp:prod
```

## Configuration

All via env vars (prefix `SQUISHY_`, see `.env.example` and
`internal/config`): `SQUISHY_PG_DSN`, `SQUISHY_HTTP_ADDR`,
`SQUISHY_WORKERS`, `SQUISHY_WORKER_ID`, `SQUISHY_BATCH_SIZE`,
`SQUISHY_LOG_LEVEL`.

## Common make targets

```
make up              start postgres + mysql-sample + api + web + squishy-mcp
make down            tear down + wipe volumes
make restart         down + up
make logs            tail API logs
make psql            psql shell on the squishy app DB
make mysql           mysql client on the sample source
make reset-dest      DROP SCHEMA mig CASCADE (target only, app schema kept)
make migrate-up      apply embedded SQL migrations
make migrate-new name=add_foo   scaffold a new up/down migration pair
make oracle-up | oracle19-up | db2-up | mariadb-sample (via compose profiles)
make test            unit tests + check-no-regex
make e2e             scripts/run_e2e.sh (full Compose round-trip)
make web-build       vite build → web/dist
make mcp-up | mcp-logs | mcp-build
```

## Adding a dialect

See [`internal/dialects/README.md`](internal/dialects/README.md). TL;DR:
vendor the upstream `.g4` under `reference/`, write `lexer.go` +
`parser.go` to match, register via an `init()`, then wire the new kind
into the HTTP connection-kind enum, the per-run connection cache
(`cmd/squishy/main.go`), `internal/discover`, `internal/translate`, and
the frontend instance form.

## License

Code under `LICENSE` (PostgreSQL License, © Dalibo). Vendored grammar
files retain their upstream licenses; see each `reference/README.md`.
