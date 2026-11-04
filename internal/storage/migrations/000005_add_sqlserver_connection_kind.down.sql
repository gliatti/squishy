-- Restores the 000003 check. Fails while a 'sqlserver' instance still
-- exists: delete those instances first.
ALTER TABLE instances DROP CONSTRAINT instances_kind_chk;
ALTER TABLE instances ADD CONSTRAINT instances_kind_chk
  CHECK (kind IN ('mysql','mariadb','oracle','oracle19','db2','db2zos'));

-- PG enums cannot have values removed: 'sqlserver' stays in
-- connection_kind (harmless once the check above rejects it).
