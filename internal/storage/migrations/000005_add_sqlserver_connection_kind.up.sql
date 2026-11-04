-- Microsoft SQL Server source kind.
ALTER TYPE connection_kind ADD VALUE IF NOT EXISTS 'sqlserver';

-- The migration runner applies each file in one transaction, and PG
-- refuses to use an enum value added in the same transaction ("unsafe use
-- of new value"). Comparing kind as text keeps the check free of an enum
-- literal, so it can be created right after the ADD VALUE above.
ALTER TABLE instances DROP CONSTRAINT instances_kind_chk;
ALTER TABLE instances ADD CONSTRAINT instances_kind_chk
  CHECK (kind::text IN ('mysql','mariadb','oracle','oracle19','db2','db2zos','sqlserver'));
