.DEFAULT_GOAL := help

COMPOSE ?= docker compose
MIGRATE_IMG ?= migrate/migrate:v4.18.1

help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---- lifecycle ----

up: ## Start core stack (postgres, mysql-sample, api, web, squishy-mcp)
	$(COMPOSE) up -d postgres mysql-sample api web squishy-mcp

down: ## Stop and remove containers + volumes
	$(COMPOSE) down -v --remove-orphans

logs: ## Tail API logs
	$(COMPOSE) logs -f api

restart: down up ## Full recycle

# ---- database shells ----

psql: ## Open psql shell on squishy app DB
	$(COMPOSE) exec postgres psql -U squishy -d squishy

reset-dest: ## Drop the target migration schema ("mig") — leaves the squishy app schema intact
	$(COMPOSE) exec postgres psql -U squishy -d squishy -c 'DROP SCHEMA IF EXISTS mig CASCADE;'

mysql: ## Open mysql client shell on the sample source DB
	$(COMPOSE) exec mysql-sample mysql -u sakila -psakila sakila

oracle-up: ## Start the Oracle 23ai sample source (profile oracle)
	$(COMPOSE) --profile oracle up -d oracle-sample

oracle-logs: ## Tail oracle-sample init logs
	$(COMPOSE) logs -f oracle-sample

oracle-sql: ## Open SQL*Plus on the Oracle sample (APP_USER=squishy@FREEPDB1)
	$(COMPOSE) exec oracle-sample sqlplus squishy/squishy@localhost:1521/FREEPDB1

oracle-down: ## Stop the Oracle sample container + wipe its volume
	$(COMPOSE) --profile oracle down -v oracle-sample

oracle19-up: ## Start the Oracle 19c sample source (profile oracle19 — requires docker login container-registry.oracle.com)
	$(COMPOSE) --profile oracle19 up -d oracle19-sample

oracle19-logs: ## Tail oracle19-sample init logs (first boot ~10 min)
	$(COMPOSE) logs -f oracle19-sample

oracle19-sql: ## Open SQL*Plus on the Oracle 19c sample (squishy@ORCLPDB1)
	$(COMPOSE) exec oracle19-sample sqlplus squishy/squishy@localhost:1521/ORCLPDB1

oracle19-down: ## Stop the Oracle 19c sample container + wipe its volume
	$(COMPOSE) --profile oracle19 down -v oracle19-sample

db2-up: ## Start the DB2 11.5 LUW sample source (profile db2 — boot ~3-5 min, EULA accepted)
	$(COMPOSE) --profile db2 up -d db2-sample

db2-logs: ## Tail db2-sample init logs
	$(COMPOSE) logs -f db2-sample

db2-cli: ## Open the DB2 CLP shell on the sample (su to db2inst1 + connect to SAMPLE)
	$(COMPOSE) exec db2-sample su - db2inst1 -c "db2 connect to SAMPLE && bash"

db2-down: ## Stop the DB2 sample container + wipe its volume
	$(COMPOSE) --profile db2 down -v db2-sample

# ---- schema migrations ----

migrate-up: ## Apply all up migrations
	$(COMPOSE) --profile e2e run --rm migrate

migrate-down: ## Roll back one migration step
	docker run --rm --network host -v $(PWD)/internal/storage/migrations:/m $(MIGRATE_IMG) \
	  -path=/m -database=postgres://squishy:squishy@localhost:5432/squishy?sslmode=disable down 1

migrate-new: ## Create a new pair of migration files: make migrate-new name=add_foo
	@test -n "$(name)" || (echo "usage: make migrate-new name=add_foo" && exit 1)
	docker run --rm -v $(PWD)/internal/storage/migrations:/m $(MIGRATE_IMG) \
	  create -ext sql -dir /m -seq $(name)

# ---- tests ----

test: check-no-regex ## Run unit tests (dockerized)
	$(COMPOSE) --profile test run --rm unit-tests

# Guard rail introduced after the SQL-translation pipeline was de-regex'ed
# (PR2–PR6: byte-walks, AST extensions, structured DML parsers and a typed
# Postgres writer). New regexp.MustCompile calls inside internal/translate
# or internal/dialects regress the project back toward the brittle text
# rewriting we worked to remove — fail the test target rather than letting
# them slip in silently. Local helpers like httpapi/projects.go (slug
# regex, hors chaîne SQL) and planner/planner.go (dependency analysis on
# raw routine bodies, AST'ifié dans une PR ultérieure) sont volontairement
# en dehors de la portée surveillée.
check-no-regex: ## Fail when regexp.MustCompile reappears in the SQL translation pipeline
	@hits=$$(grep -rn 'regexp\.MustCompile\|"regexp"' --include='*.go' \
	  internal/translate internal/dialects 2>/dev/null || true); \
	if [ -n "$$hits" ]; then \
	  echo "regex regression in SQL translation pipeline:"; \
	  echo "$$hits"; \
	  echo ""; \
	  echo "These call sites should be reworked into byte-walk or AST visitors —"; \
	  echo "see ~/.claude/plans/dans-le-code-une-luminous-cat.md for the rationale."; \
	  exit 1; \
	fi

e2e: ## Run full end-to-end scenario (dockerized)
	./scripts/run_e2e.sh

# ---- mcp ----

mcp-up: ## Start the squishy MCP server (http on localhost:8002)
	$(COMPOSE) up -d squishy-mcp

mcp-down: ## Stop the squishy MCP server
	$(COMPOSE) stop squishy-mcp

mcp-logs: ## Tail squishy-mcp logs
	$(COMPOSE) logs -f squishy-mcp

mcp-build: ## Build the squishy-mcp production (distroless) image as squishy-mcp:prod
	docker build --target prod -t squishy-mcp:prod ./mcp-server

# ---- frontend ----

web-dev: ## Start Vite dev server (already part of `make up`)
	$(COMPOSE) up -d web

web-build: ## Build production bundle into ./web/dist
	$(COMPOSE) run --rm web npm run build
