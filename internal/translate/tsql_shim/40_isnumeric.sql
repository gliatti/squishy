
-- tsql.isnumeric(s) — T-SQL ISNUMERIC: 1 when s converts to one of the
-- numeric types (int, decimal, float, money), else 0; NULL gives 0. Read
-- character by character:
--   [blanks] [currency] [sign] [currency] digits/commas [. digits]
--   [e|d [sign] digits] [blanks]
-- SQL Server's quirks are kept: thousands commas are accepted, and a lone
-- sign, currency symbol, '.' or ',' counts as numeric (they convert to
-- money 0).
CREATE OR REPLACE FUNCTION tsql.isnumeric(s text)
RETURNS integer
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    i integer := 1;
    n integer;
    c text;
    digits integer := 0;
    seen_dot boolean := false;
    seen_cur boolean := false;
BEGIN
    IF s IS NULL THEN
        RETURN 0;
    END IF;
    n := length(s);
    WHILE i <= n AND substr(s, i, 1) IN (' ', E'\t', E'\n', E'\r') LOOP
        i := i + 1;
    END LOOP;
    IF i > n THEN
        RETURN 0;
    END IF;
    IF substr(s, i, 1) IN ('$', '£', '€', '¥', '¢', '¤') THEN
        seen_cur := true;
        i := i + 1;
    END IF;
    IF i <= n AND substr(s, i, 1) IN ('+', '-') THEN
        i := i + 1;
    END IF;
    IF NOT seen_cur AND i <= n AND substr(s, i, 1) IN ('$', '£', '€', '¥', '¢', '¤') THEN
        i := i + 1;
    END IF;
    WHILE i <= n LOOP
        c := substr(s, i, 1);
        IF ascii(c) BETWEEN 48 AND 57 THEN
            digits := digits + 1;
        ELSIF c = ',' AND NOT seen_dot THEN
            NULL;
        ELSIF c = '.' AND NOT seen_dot THEN
            seen_dot := true;
        ELSE
            EXIT;
        END IF;
        i := i + 1;
    END LOOP;
    IF i <= n AND lower(substr(s, i, 1)) IN ('e', 'd') THEN
        IF digits = 0 THEN
            RETURN 0;
        END IF;
        i := i + 1;
        IF i <= n AND substr(s, i, 1) IN ('+', '-') THEN
            i := i + 1;
        END IF;
        WHILE i <= n AND ascii(substr(s, i, 1)) BETWEEN 48 AND 57 LOOP
            i := i + 1;
        END LOOP;
    END IF;
    WHILE i <= n AND substr(s, i, 1) IN (' ', E'\t', E'\n', E'\r') LOOP
        i := i + 1;
    END LOOP;
    IF i <= n THEN
        RETURN 0;
    END IF;
    RETURN 1;
END;
$$;
