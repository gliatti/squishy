#!/usr/bin/env bash
# squishy — end-to-end gate.
# Run from the repo root. Requires only `docker` and `docker compose`.
#
# The run happens in its own compose project (docker-compose.e2e.yml,
# project "squishy-e2e") without any host port published, so it neither
# wipes the dev stack of `make up` nor collides with ports other local
# stacks already hold. Readiness is gated on compose healthchecks
# (`up --wait`, `depends_on: service_healthy`), no polling loop.
set -euo pipefail

cd "$(dirname "$0")/.."

files=(-f docker-compose.yml)
if [[ -f docker-compose.override.yml ]]; then
  files+=(-f docker-compose.override.yml)
fi
files+=(-f docker-compose.e2e.yml)

dc() { docker compose "${files[@]}" "$@"; }

echo "[e2e] clean slate (compose project squishy-e2e only)"
dc --profile test --profile e2e down -v --remove-orphans || true

# Rebuild the stack's images so the run exercises the current tree (the
# postgres image bakes postgres-init/ in; layer caching keeps this cheap).
echo "[e2e] build images"
dc --profile test --profile e2e build

echo "[e2e] unit tests"
dc --profile test run --rm unit-tests

echo "[e2e] start infra + wait for PG and MySQL healthchecks"
dc up -d --wait postgres mysql-sample

# Optional DB2 source — boot is ~4-5 min, image is ~3 GB, EULA must be
# acceptable in CI. Set SQUISHY_E2E_DB2=1 to include the DB2 e2e scenario.
if [[ "${SQUISHY_E2E_DB2:-0}" == "1" ]]; then
  echo "[e2e] start db2-sample + wait for its healthcheck (~4 min boot)"
  dc --profile db2 up -d --wait db2-sample
fi

echo "[e2e] apply migrations explicitly"
dc --profile e2e run --rm migrate

echo "[e2e] start API + wait for /readyz healthcheck"
dc up -d --wait api

echo "[e2e] run integration tests"
dc --profile e2e run --rm -e SQUISHY_E2E_DB2="${SQUISHY_E2E_DB2:-0}" e2e

echo "[e2e] done — leaving containers up for inspection. Tear down with: make e2e-down"
