
-- T-SQL UPDATE(col) in an AFTER UPDATE trigger: TRUE when the UPDATE
-- statement's SET list names the column (whatever the values). A
-- PostgreSQL trigger with transition tables cannot have a column list,
-- so the translation pairs it with one helper trigger per column tested,
-- `AFTER UPDATE OF col FOR EACH STATEMENT EXECUTE FUNCTION
-- tsql.mark_column_updated('col')`, which PG fires exactly when the
-- column is an UPDATE target, and which is named to fire before the
-- translated trigger (PG fires a relation's triggers of one kind in name
-- order). The mark is a transaction-local setting per relation and
-- column number; the translated trigger reads and clears it once, when
-- its body starts (tsql.take_column_updated), so the next UPDATE
-- statement starts unmarked.
CREATE OR REPLACE FUNCTION tsql.mark_column_updated()
RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    k integer := 0;
BEGIN
    WHILE k < TG_NARGS LOOP
        PERFORM set_config('tsql.updated_' || TG_RELID::text || '_' || a.attnum::text, 'on', true)
           FROM pg_catalog.pg_attribute a
          WHERE a.attrelid = TG_RELID AND a.attname = TG_ARGV[k] AND NOT a.attisdropped;
        k := k + 1;
    END LOOP;
    RETURN NULL;
END;
$$;

-- tsql.take_column_updated(rel, col) — whether the current UPDATE
-- statement on rel named col (marked by tsql.mark_column_updated), and
-- clears the mark.
CREATE OR REPLACE FUNCTION tsql.take_column_updated(rel oid, col name)
RETURNS boolean
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    k text;
    marked boolean;
BEGIN
    SELECT 'tsql.updated_' || rel::text || '_' || a.attnum::text INTO k
      FROM pg_catalog.pg_attribute a
     WHERE a.attrelid = rel AND a.attname = col AND NOT a.attisdropped;
    IF k IS NULL THEN
        RETURN false;
    END IF;
    marked := coalesce(current_setting(k, true), '') = 'on';
    IF marked THEN
        PERFORM set_config(k, '', true);
    END IF;
    RETURN marked;
END;
$$;
