
-- tsql.isdate(s) — T-SQL ISDATE: 1 when s converts to a timestamp, else
-- 0; NULL gives 0. The conversion is attempted in an exception block
-- (so the function is neither immutable — it follows DateStyle — nor
-- parallel safe). PG's special inputs ('now', 'today', 'epoch',
-- 'infinity', …) hold no digit and are refused, as SQL Server refuses
-- them.
CREATE OR REPLACE FUNCTION tsql.isdate(s text)
RETURNS integer
LANGUAGE plpgsql STABLE AS $$
DECLARE
    i integer := 1;
    has_digit boolean := false;
BEGIN
    IF s IS NULL THEN
        RETURN 0;
    END IF;
    WHILE i <= length(s) AND NOT has_digit LOOP
        has_digit := ascii(substr(s, i, 1)) BETWEEN 48 AND 57;
        i := i + 1;
    END LOOP;
    IF NOT has_digit THEN
        RETURN 0;
    END IF;
    PERFORM s::timestamp;
    RETURN 1;
EXCEPTION WHEN others THEN
    RETURN 0;
END;
$$;
