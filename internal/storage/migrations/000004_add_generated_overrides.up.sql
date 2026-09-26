-- Per-column PostgreSQL generation expression overrides of a migration
-- (translate.GeneratedOverride: [{"table","column","expression"}]).
-- Set by the user, kept across re-plans like acked_prereqs.
ALTER TABLE squishy.migrations ADD COLUMN generated_overrides JSONB NOT NULL DEFAULT '[]'::jsonb;
