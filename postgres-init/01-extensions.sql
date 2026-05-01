-- Installed by Dockerfile.postgres on first cluster bootstrap.
-- Safe to run repeatedly (IF NOT EXISTS), but only fires once because
-- /docker-entrypoint-initdb.d/ is gated by the data volume being empty.

CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS pg_cron;
CREATE EXTENSION IF NOT EXISTS vector;

-- pg_cron needs a host database; it was configured via cron.database_name in
-- the compose command line. Grant the app role permission to schedule jobs.
GRANT USAGE ON SCHEMA cron TO squishy;
