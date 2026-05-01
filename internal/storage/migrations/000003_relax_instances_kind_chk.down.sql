ALTER TABLE instances DROP CONSTRAINT instances_kind_chk;
ALTER TABLE instances ADD CONSTRAINT instances_kind_chk
  CHECK (kind IN ('mysql','mariadb','oracle','oracle19'));
