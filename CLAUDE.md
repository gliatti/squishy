# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

squishy is a web UI + Go API for piloting **MySQL/MariaDB/Oracle/IBM DB2 → PostgreSQL** migrations. It parses source DDL with hand-rolled per-dialect parsers (no ANTLR runtime — `.g4` grammars live in `reference/` as spec), translates to Postgres, and runs the data copy via a Postgres-hosted job queue with a goroutine worker pool.

## Common commands

Development is fully dockerized — no local Go or Node toolchain is required.

```bash
make up              # postgres + mysql-sample + api + web (http://localhost:5173)
make down            # tear down + wipe volumes
make logs            # tail API logs
make restart         # down + up
make test            # unit tests (dockerized: go test ./... -race -count=1)
make e2e             # full scripts/run_e2e.sh: unit + migrations + API + integration
make psql            # psql on the app DB
make mysql           # mysql client on the sample source
make reset-dest      # DROP SCHEMA mig CASCADE (wipe migrated target, keep app schema)
make migrate-up      # apply SQL migrations in internal/storage/migrations
make migrate-new name=add_foo   # scaffold a new up/down migration pair
make oracle-up       # start the Oracle 23ai sample source (profile: oracle)
make db2-up          # start the IBM DB2 11.5 LUW sample source (profile: db2 — boot ~4-5 min, EULA accepted)
make db2-cli         # DB2 CLP shell on the sample (su to db2inst1, connect to SAMPLE)
make web-build       # vite build → web/dist
```

### DB2 driver and clidriver

The DB2 path requires the IBM clidriver library at runtime: the only viable
Go driver is `github.com/ibmdb/go_ibm_db` which is CGO + dynamically linked
to `libdb2.so`. clidriver only ships glibc binaries, so DB2 support lives
in a dedicated Dockerfile stage `dev-db2` (Debian-based, ~80 MB extra
image size). The `api` service in `docker-compose.yml` targets that stage
and runs the binary with `-tags db2` (which fires the conditional import
in `internal/connection/db2_driver_cgo.go`). Unit tests and the alpine-based
`unit-tests` service stay on the `dev` target — the parser/translator code
exercises DB2 sources without ever opening a real connection, which is the
expected pattern for in-repo tests.

The DB2 e2e scenario (`test/integration/db2_e2e_test.go`) is opt-in behind
`SQUISHY_E2E_DB2=1`; default `make e2e` runs only the MySQL flow to keep CI
under 5 min (DB2 sample container boot is ~4 min on its own).

Single-test runs go through the dockerized `unit-tests` service:

```bash
docker compose --profile test run --rm unit-tests go test ./internal/translate/... -run TestEnum -v
```

E2E integration tests live in `test/integration/` behind the `e2e` build tag and expect the full `docker compose --profile e2e` stack (see `scripts/run_e2e.sh` for the boot order — migrations must complete before the `e2e` container runs).

## Architecture

`cmd/squishy/main.go` wires everything together. On boot it:

1. Opens the app `pgxpool` and runs embedded migrations via `internal/storage`.
2. Starts an `events.Bus` (in-process pub/sub, also drives SSE) and `project.Repo`.
3. Starts a `worker.Pool` consuming jobs from `internal/queue` (Postgres-backed, `SELECT … FOR UPDATE SKIP LOCKED`).
4. Starts two polling goroutines:
   - **`dispatchLoop`** — promotes `squishy.steps` rows whose `depends_on[]` all succeeded into queued `squishy.jobs` (this is how the step DAG advances).
   - **`runStateLoop`** — flips `squishy.runs.status` to `succeeded`/`failed` once every step terminates.
5. Serves HTTP via `internal/httpapi` (chi router, `/api/v1/...`, SSE at `/runs/{id}/events`).

### Per-run connection cache (main.go `connCache`)

Source/target DB pools are **not** opened at startup. Each job handler calls `connCache.forRun(runID)`, which maps run → project → `squishy.connections` rows, then lazily opens + caches pools keyed by project UUID. A fresh `worker.Deps` snapshot is built per job dispatch so jobs from different projects don't race on a shared mutable `Deps`. When adding a new source kind, extend the `switch srcKind` block (currently `mysql`/`mariadb`/`oracle`) and wire a matching `dataxfer.*Source()` dialect.

### Job kinds

The worker registers handlers for a fixed set of kinds, all declared in `main.go`:
`inspect`, `create_ddl`, `copy_table`, `copy_batch`, `create_index`, `create_fk`, `create_routine`, `validate`. Actual handler bodies live in `internal/worker` (reached via `Deps.Handlers()`). Data copy is partition-by-PK into 10 000-row (`SQUISHY_BATCH_SIZE`) idempotent `COPY FROM STDIN` batches in `internal/dataxfer`; individual batches can retry independently.

### Dialects (`internal/dialects/`)

Each source dialect is its own subpackage with a vendored `.g4` under `reference/` (MIT from grammars-v4 where available), a hand-rolled `lexer.go` / `parser.go` / `token.go`, and a `dialect.go` whose `init()` registers with the central registry. **No ANTLR runtime.** The grammar is spec, not generated code. See `internal/dialects/README.md` for the full procedure when adding a dialect — it must also be added to the HTTP connection-kind enum and the Vue wizard dropdown.

Translation flow: source parser → `internal/sqlparse/ast` (dialect-agnostic AST) → `internal/translate` (emits a Postgres `SchemaPlan` + DDL + human explanations) → `internal/planner` (orders into a step DAG) → queued jobs.

### Frontend (`web/`)

Vue 3 + Vite + Pinia, five-step wizard (project → source → target → DDL xform → data xform → summary → launch). Dev server runs in the `web` compose service; `VITE_API_BASE` points at the `api` service. Production bundle via `make web-build`.

## Configuration

All via env vars (prefix `SQUISHY_`, see `.env.example` and `internal/config`):
`SQUISHY_PG_DSN`, `SQUISHY_HTTP_ADDR`, `SQUISHY_WORKERS`, `SQUISHY_WORKER_ID`, `SQUISHY_BATCH_SIZE`, `SQUISHY_LOG_LEVEL`.

The app schema is `squishy.*`; the target (migrated) schema is `mig` by convention — `make reset-dest` wipes only the latter.

## Recompiling after code changes

The `api` and `squishy-mcp` dev containers run `go run` directly — there is no file-watcher. After editing Go code, restart the container to recompile:

    docker compose restart api
    docker compose restart squishy-mcp

The Go build cache is mounted as a named volume (`gocache`, `mcp-gocache`), so restarts are fast after the first compile.

## No regex on SQL — AST only

**Forbidden** : tout traitement de SQL (Oracle, MySQL, MariaDB, DB2, PG)
par regex, scan textuel, `strings.Index/Contains`-driven rewrites,
substitution de placeholders dans des strings, ou tout équivalent
opérant sur le texte brut de SQL.

Toute transformation SQL passe **exclusivement** par l'AST :

- Le texte source entre via le parser hand-roll (`internal/dialects/<kind>/`)
  qui produit des nœuds `internal/sqlparse/ast/` typés.
- Les passes de translation (`internal/translate/`) consomment et produisent
  des nœuds AST. Elles n'inspectent jamais le texte SQL avec des regex
  ou des fonctions de string-matching.
- Si un nœud arrive en `*ast.RawExpr` (texte brut conservé par le parser),
  il faut **soit étendre le parser** pour produire des nœuds typés,
  **soit appeler le parser d'expression** sur ce texte pour obtenir un
  AST manipulable. Pas de découpage textuel manuel.
- Les fonctions de réécriture qui produisent du SQL pour PG passent par
  les builders de `internal/dialects/postgres` ou par `pgast.Write`.

Exceptions strictes :

- Le **lexer** lui-même (déjà écrit, non régénéré) reste le seul endroit
  où l'on lit du texte caractère par caractère.
- Les vrais comments / dollar-quote markers handled by the lexer.
- Identifier-name lower/uppercase folding (string ops on un seul ident,
  pas sur du SQL) — tolérable mais à éviter quand un AST node existe.

Tout PR qui contient `regexp.MustCompile`, `regexp.Compile`,
`strings.Index*`/`strings.Contains*`/`strings.Replace*` appliqué à du
contenu SQL (vs un identifiant scalaire) est à rejeter — il faut
remonter au parser pour exposer le nœud AST manquant.

## Bash style

**NEVER use `until` or `while` polling loops in Bash invocations.** They don't work reliably in this harness (they time out silently or get blocked) and they're noisy and hard to reason about. This is a hard rule — no exceptions.

Use one of these instead:
- A single blocking command: `docker compose up -d --wait <svc>`, `docker compose wait <svc>`, or `curl --retry N --retry-delay S --retry-connrefused <url>`.
- Compose healthchecks: rely on `depends_on: { condition: service_healthy }` or `--wait` rather than pinging in a loop.
- `run_in_background: true` for genuinely async work, then wait for the completion notification.

Never chain multiple `sleep`s as a workaround, and never wrap a readiness check in `until ...; do sleep N; done`.

## Bulk-loading MySQL/MariaDB dumps

Do **not** prepend `SET autocommit=0` to a `mysqldump` output without a final `COMMIT` — classic mysqldumps don't include a trailing `COMMIT`, so the whole load becomes one giant transaction. Consequences: the undo log grows to the size of the data, other sessions (including squishy's `SELECT COUNT(*)`) stall behind MVCC, and killing/restarting the session triggers a rollback that takes **as long as the load itself**. Even `docker compose restart` won't skip the rollback (InnoDB recovers it on boot).

Fast path instead: leave `autocommit=ON` (per-INSERT commits) and tune durability **globally** once:

```sql
SET GLOBAL innodb_flush_log_at_trx_commit=0;
SET GLOBAL sync_binlog=0;
```

That yields ~50k rows/s on commodity hardware without the giant-transaction hazards. If rollback is already in progress and unbearable, and the container has **no mount** on `/var/lib/mysql` (e.g. `mysql-sample` in `docker-compose.yml`), `docker compose rm -f` + `docker compose up -d` is faster than waiting — the uncommitted data dies with the container.
