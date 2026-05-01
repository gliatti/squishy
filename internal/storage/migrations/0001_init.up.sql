-- Initial schema (consolidated). The model is:
--   project (1) ──< instance (N) ──< migration (N, one per source schema)
--                      └── DSN super-user (Oracle/MySQL/MariaDB)
--   project (1) ──── connection target (1, PG super-user with CREATEDB)
--   migration (1) ──< run (N) ──< step (N) ──< step_batches (N) / job (N)

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE SCHEMA IF NOT EXISTS squishy;
SET search_path TO squishy;

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;

CREATE TYPE connection_kind AS ENUM ('mysql', 'mariadb', 'oracle', 'oracle19', 'postgres');
CREATE TYPE run_status      AS ENUM ('pending','running','succeeded','failed','cancelled');
CREATE TYPE step_status     AS ENUM ('pending','running','succeeded','failed','skipped','cancelled');
CREATE TYPE job_status      AS ENUM ('pending','running','succeeded','failed','dead');

-- ─────────────────────────────────────────────────────────────────────────────
-- projects
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE projects (
  id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name         TEXT        NOT NULL,
  slug         TEXT        NOT NULL,
  description  TEXT        NOT NULL DEFAULT '',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT projects_name_len    CHECK (length(name) BETWEEN 1 AND 200),
  CONSTRAINT projects_slug_uniq   UNIQUE (slug),
  CONSTRAINT projects_slug_format CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$')
);
CREATE TRIGGER trg_projects_updated_at BEFORE UPDATE ON projects
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- instances — one per source endpoint, holds super-user DSN + mapping strategy
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE instances (
  id                UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id        UUID            NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name              TEXT            NOT NULL,
  kind              connection_kind NOT NULL,
  -- Source endpoint (the Oracle / MySQL / MariaDB super-user DSN).
  host              TEXT            NOT NULL,
  port              INT             NOT NULL CHECK (port BETWEEN 1 AND 65535),
  database          TEXT            NOT NULL,
  username          TEXT            NOT NULL,
  password          TEXT            NOT NULL,
  ssl_mode          TEXT            NOT NULL DEFAULT 'disable',
  params            JSONB           NOT NULL DEFAULT '{}'::jsonb,
  -- target_strategy decides how each source schema is materialized in PG:
  --   dedicated_db     : one PG database per source schema (auto CREATE DATABASE);
  --                      target_database is the admin DB used to issue CREATE DATABASE.
  --   dedicated_schema : one PG schema per source schema, all in target_database.
  target_strategy   TEXT            NOT NULL CHECK (target_strategy IN ('dedicated_db','dedicated_schema')),
  -- Target endpoint — one PG super-user DSN per instance. With dedicated_db
  -- the user must hold CREATEDB.
  target_host       TEXT            NOT NULL,
  target_port       INT             NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
  target_database   TEXT            NOT NULL,
  target_username   TEXT            NOT NULL,
  target_password   TEXT            NOT NULL,
  target_ssl_mode   TEXT            NOT NULL DEFAULT 'disable',
  target_params     JSONB           NOT NULL DEFAULT '{}'::jsonb,
  -- When true, squishy auto-creates target_database via the PG default DB
  -- ("postgres") if it doesn't already exist, both on instance creation and
  -- before each run. Useful for dev/CI where the user doesn't pre-provision.
  target_create_db  BOOLEAN         NOT NULL DEFAULT false,
  last_tested_at    TIMESTAMPTZ,
  last_test_ok      BOOLEAN,
  last_test_msg     TEXT,
  created_at        TIMESTAMPTZ     NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ     NOT NULL DEFAULT now(),
  CONSTRAINT instances_name_len  CHECK (length(name) BETWEEN 1 AND 200),
  CONSTRAINT instances_name_uniq UNIQUE (project_id, name),
  CONSTRAINT instances_kind_chk  CHECK (kind IN ('mysql','mariadb','oracle','oracle19'))
);
CREATE INDEX idx_instances_project ON instances(project_id);
CREATE TRIGGER trg_instances_updated_at BEFORE UPDATE ON instances
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- migrations — one per (instance, source_schema_name). No version numbering;
-- re-planning upserts the row.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE migrations (
  id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  instance_id         UUID        NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  source_schema_name  TEXT        NOT NULL,
  target_db_name      TEXT        NOT NULL,
  target_schema_name  TEXT        NOT NULL,
  status              TEXT        NOT NULL DEFAULT 'draft'
    CHECK (status IN ('draft','planned')),
  source_schema       JSONB       NOT NULL DEFAULT '{}'::jsonb,
  target_plan         JSONB       NOT NULL DEFAULT '{}'::jsonb,
  ddl_script          TEXT        NOT NULL DEFAULT '',
  ddl_post_script     TEXT        NOT NULL DEFAULT '',
  data_plan           JSONB       NOT NULL DEFAULT '{}'::jsonb,
  type_mappings       JSONB       NOT NULL DEFAULT '[]'::jsonb,
  explanations        JSONB       NOT NULL DEFAULT '[]'::jsonb,
  warnings            JSONB       NOT NULL DEFAULT '[]'::jsonb,
  prerequisites       JSONB       NOT NULL DEFAULT '[]'::jsonb,
  acked_prereqs       JSONB       NOT NULL DEFAULT '[]'::jsonb,
  options             JSONB       NOT NULL DEFAULT '{}'::jsonb,
  source_snapshot_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT migrations_instance_schema_uniq UNIQUE (instance_id, source_schema_name),
  CONSTRAINT migrations_target_db_chk        CHECK (target_db_name <> ''),
  CONSTRAINT migrations_target_schema_chk    CHECK (target_schema_name <> '')
);
CREATE INDEX idx_migrations_instance ON migrations(instance_id);
CREATE TRIGGER trg_migrations_updated_at BEFORE UPDATE ON migrations
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- runs / steps / batches / jobs / events
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE runs (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  migration_id  UUID        NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
  status        run_status  NOT NULL DEFAULT 'pending',
  triggered_by  TEXT        NOT NULL DEFAULT 'api',
  parent_run_id UUID        REFERENCES runs(id) ON DELETE SET NULL,
  dispatch_mode TEXT        NOT NULL DEFAULT 'auto'
    CHECK (dispatch_mode IN ('auto','manual')),
  options       JSONB       NOT NULL DEFAULT '{}'::jsonb,
  rows_total    BIGINT      NOT NULL DEFAULT 0,
  rows_done     BIGINT      NOT NULL DEFAULT 0,
  started_at    TIMESTAMPTZ,
  finished_at   TIMESTAMPTZ,
  error         TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT runs_times_chk CHECK (finished_at IS NULL OR finished_at >= started_at)
);
CREATE INDEX idx_runs_migration   ON runs(migration_id, created_at DESC);
CREATE INDEX idx_runs_status_open ON runs(status) WHERE status IN ('pending','running');
-- At most one active run per migration (handler-level guard + DB invariant).
CREATE UNIQUE INDEX uq_runs_one_active_per_migration
  ON runs (migration_id) WHERE status IN ('pending', 'running');
CREATE TRIGGER trg_runs_updated_at BEFORE UPDATE ON runs
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE steps (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id      UUID        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  seq         INT         NOT NULL,
  kind        TEXT        NOT NULL,
  target      TEXT,
  payload     JSONB       NOT NULL DEFAULT '{}'::jsonb,
  depends_on  UUID[]      NOT NULL DEFAULT '{}'::uuid[],
  priority    SMALLINT    NOT NULL DEFAULT 100,
  level       INT         NOT NULL DEFAULT 0,
  unlocked    BOOLEAN     NOT NULL DEFAULT TRUE,
  status      step_status NOT NULL DEFAULT 'pending',
  attempts    INT         NOT NULL DEFAULT 0,
  rows_total  BIGINT      NOT NULL DEFAULT 0,
  rows_done   BIGINT      NOT NULL DEFAULT 0,
  started_at  TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  error       TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT steps_kind_chk CHECK (kind IN
    ('inspect','create_target_db','create_ddl','copy_table','create_index',
     'create_fk','create_routine','validate','custom')),
  CONSTRAINT steps_seq_uniq UNIQUE (run_id, seq)
);
CREATE INDEX idx_steps_run_status      ON steps(run_id, status);
CREATE INDEX idx_steps_run_kind        ON steps(run_id, kind);
CREATE INDEX idx_steps_run_level       ON steps(run_id, level);
CREATE INDEX idx_steps_pending_ready   ON steps(run_id) WHERE status = 'pending';
CREATE INDEX idx_steps_depends_on_gin  ON steps USING GIN (depends_on);
CREATE TRIGGER trg_steps_updated_at BEFORE UPDATE ON steps
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE step_batches (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  step_id       UUID        NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
  seq           INT         NOT NULL,
  pk_columns    TEXT[]      NOT NULL DEFAULT '{}'::text[],
  range_low     JSONB,
  range_high    JSONB,
  range_kind    TEXT        NOT NULL DEFAULT 'pk',
  row_count_est BIGINT,
  row_count     BIGINT,
  status        step_status NOT NULL DEFAULT 'pending',
  attempts      INT         NOT NULL DEFAULT 0,
  started_at    TIMESTAMPTZ,
  finished_at   TIMESTAMPTZ,
  error         TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT step_batches_seq_uniq       UNIQUE (step_id, seq),
  CONSTRAINT step_batches_range_kind_chk CHECK (range_kind IN ('pk','offset'))
);
CREATE INDEX idx_batches_step_status ON step_batches(step_id, status);
CREATE INDEX idx_batches_status_open ON step_batches(status) WHERE status IN ('pending','running');
CREATE TRIGGER trg_batches_updated_at BEFORE UPDATE ON step_batches
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE step_events (
  id         BIGSERIAL   PRIMARY KEY,
  run_id     UUID        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  step_id    UUID        REFERENCES steps(id) ON DELETE CASCADE,
  batch_id   UUID        REFERENCES step_batches(id) ON DELETE CASCADE,
  level      TEXT        NOT NULL CHECK (level IN ('debug','info','warn','error')),
  kind       TEXT        NOT NULL,
  message    TEXT        NOT NULL DEFAULT '',
  data       JSONB       NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_events_run_created  ON step_events(run_id, created_at);
CREATE INDEX idx_events_step_created ON step_events(step_id, created_at);
CREATE INDEX idx_events_kind         ON step_events(run_id, kind, created_at);

CREATE TABLE jobs (
  id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id       UUID        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  step_id      UUID        NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
  batch_id     UUID                 REFERENCES step_batches(id) ON DELETE CASCADE,
  kind         TEXT        NOT NULL,
  payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
  priority     SMALLINT    NOT NULL DEFAULT 100,
  status       job_status  NOT NULL DEFAULT 'pending',
  attempts     INT         NOT NULL DEFAULT 0,
  max_attempts INT         NOT NULL DEFAULT 3,
  available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  locked_at    TIMESTAMPTZ,
  locked_by    TEXT,
  last_error   TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT jobs_attempts_chk CHECK (attempts >= 0 AND attempts <= max_attempts + 1),
  CONSTRAINT jobs_lock_chk CHECK (
    (status = 'running' AND locked_at IS NOT NULL AND locked_by IS NOT NULL)
    OR status <> 'running'
  )
);
CREATE INDEX idx_jobs_claim
  ON jobs (priority, available_at, id)
  WHERE status = 'pending';
CREATE INDEX idx_jobs_run_status ON jobs(run_id, status);
CREATE INDEX idx_jobs_step       ON jobs(step_id);
CREATE INDEX idx_jobs_batch      ON jobs(batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX idx_jobs_orphaned   ON jobs(locked_at) WHERE status = 'running';
CREATE TRIGGER trg_jobs_updated_at BEFORE UPDATE ON jobs
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE VIEW v_run_progress AS
SELECT
  r.id                                           AS run_id,
  r.status                                       AS run_status,
  count(s.*)                                     AS steps_total,
  count(s.*) FILTER (WHERE s.status='succeeded') AS steps_done,
  count(s.*) FILTER (WHERE s.status='failed')    AS steps_failed,
  count(s.*) FILTER (WHERE s.status='running')   AS steps_running,
  COALESCE(sum(s.rows_total),0)                  AS rows_total,
  COALESCE(sum(s.rows_done),0)                   AS rows_done
FROM runs r
LEFT JOIN steps s ON s.run_id = r.id
GROUP BY r.id, r.status;
