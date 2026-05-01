#!/usr/bin/env bash
# squishy — end-to-end gate.
# Run from the repo root. Requires only `docker` and `docker compose`.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "[e2e] clean slate"
docker compose down -v --remove-orphans || true

echo "[e2e] unit tests"
docker compose --profile test run --rm unit-tests

echo "[e2e] start infra"
docker compose up -d postgres mysql-sample

# Optional DB2 source — boot is ~4-5 min, image is ~3 GB, EULA must be
# acceptable in CI. Set SQUISHY_E2E_DB2=1 to include the DB2 e2e scenario.
if [[ "${SQUISHY_E2E_DB2:-0}" == "1" ]]; then
  echo "[e2e] start db2-sample (profile db2 — ~4 min boot)"
  docker compose --profile db2 up -d db2-sample
fi

echo "[e2e] wait for PG + MySQL healthchecks"
until docker compose ps postgres     | grep -q '(healthy)'; do sleep 1; done
until docker compose ps mysql-sample | grep -q '(healthy)'; do sleep 1; done

if [[ "${SQUISHY_E2E_DB2:-0}" == "1" ]]; then
  echo "[e2e] wait for DB2 healthcheck"
  until docker compose ps db2-sample | grep -q '(healthy)'; do sleep 5; done
fi

echo "[e2e] apply migrations explicitly"
docker compose --profile e2e run --rm migrate

echo "[e2e] start API + web"
docker compose up -d api web

echo "[e2e] wait for API /readyz"
until docker compose exec -T api wget -q -O - http://localhost:8080/readyz >/dev/null 2>&1; do sleep 1; done

echo "[e2e] run integration tests"
docker compose --profile e2e run --rm e2e

echo "[e2e] done — leaving containers up for inspection. Tear down with: docker compose down -v"
