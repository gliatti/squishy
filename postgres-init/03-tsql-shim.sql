-- tsql shim: runtime helpers for SQL Server (T-SQL) code translated by
-- squishy. The AST rewriters (internal/translate/tsql_visitors.go) turn
-- the T-SQL built-ins PostgreSQL has no counterpart for into calls to
-- the functions below; each migration's DDL creates the ones it calls
-- (Plan.PreActions), and the dev cluster gets all of them at bootstrap.
--
-- Source of truth: internal/translate/tsql_shim/*.sql, concatenated in
-- file-name order into postgres-init/03-tsql-shim.sql (a unit test keeps
-- both identical). Edit the pieces, then regenerate with:
--   cat internal/translate/tsql_shim/*.sql > postgres-init/03-tsql-shim.sql
--
-- CLAUDE.md "No regex anywhere": every helper is plain SQL or a
-- PL/pgSQL character-by-character loop, without any regular-expression
-- function or operator.

CREATE SCHEMA IF NOT EXISTS tsql;
GRANT USAGE ON SCHEMA tsql TO PUBLIC;

-- tsql.datepart_canon(part) — the canonical name of a T-SQL date part
-- keyword or abbreviation (yy / yyyy → year, dw / w → weekday, …), NULL
-- for an unknown one. The translator already passes canonical names; the
-- aliases keep hand-written calls working.
CREATE OR REPLACE FUNCTION tsql.datepart_canon(part text)
RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT CASE lower(btrim(part))
        WHEN 'year' THEN 'year' WHEN 'yy' THEN 'year' WHEN 'yyyy' THEN 'year'
        WHEN 'quarter' THEN 'quarter' WHEN 'qq' THEN 'quarter' WHEN 'q' THEN 'quarter'
        WHEN 'month' THEN 'month' WHEN 'mm' THEN 'month' WHEN 'm' THEN 'month'
        WHEN 'dayofyear' THEN 'dayofyear' WHEN 'dy' THEN 'dayofyear' WHEN 'y' THEN 'dayofyear'
        WHEN 'day' THEN 'day' WHEN 'dd' THEN 'day' WHEN 'd' THEN 'day'
        WHEN 'week' THEN 'week' WHEN 'wk' THEN 'week' WHEN 'ww' THEN 'week'
        WHEN 'weekday' THEN 'weekday' WHEN 'dw' THEN 'weekday' WHEN 'w' THEN 'weekday'
        WHEN 'hour' THEN 'hour' WHEN 'hh' THEN 'hour'
        WHEN 'minute' THEN 'minute' WHEN 'mi' THEN 'minute' WHEN 'n' THEN 'minute'
        WHEN 'second' THEN 'second' WHEN 'ss' THEN 'second' WHEN 's' THEN 'second'
        WHEN 'millisecond' THEN 'millisecond' WHEN 'ms' THEN 'millisecond'
        WHEN 'microsecond' THEN 'microsecond' WHEN 'mcs' THEN 'microsecond'
        WHEN 'nanosecond' THEN 'nanosecond' WHEN 'ns' THEN 'nanosecond'
        WHEN 'iso_week' THEN 'iso_week' WHEN 'isowk' THEN 'iso_week' WHEN 'isoww' THEN 'iso_week'
        WHEN 'tzoffset' THEN 'tzoffset' WHEN 'tz' THEN 'tzoffset'
    END;
$$;

-- tsql.like(subject, pattern [, escape_char]) — T-SQL LIKE:
--   %        any run of characters (possibly empty)
--   _        exactly one character
--   [abc]    one character of the set; [a-f] a range; [^a-f] any other
--   escape   the character after escape_char is literal
-- Case-insensitive (SQL Server's default collation): subject and pattern
-- are compared through lower(). Trailing blanks of the subject are
-- ignored, as SQL Server ignores them. A '[' without a closing ']' is a
-- literal character; a ']' right after '[' or '[^' is a member of the
-- set. Matching is the classic wildcard scan with one backtrack point
-- (the last %), character by character.
CREATE OR REPLACE FUNCTION tsql.like(subject text, pattern text, escape_char text)
RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    s text;
    p text;
    esc text;
    slen integer;
    plen integer;
    si integer := 1;
    pp integer := 1;
    star_p integer := 0;
    star_s integer := 0;
    c text;
    sc text;
    lo text;
    hi text;
    j integer;
    k integer;
    close_at integer;
    neg boolean;
    hit boolean;
    matched boolean;
    is_star boolean;
    adv integer;
BEGIN
    IF subject IS NULL OR pattern IS NULL THEN
        RETURN NULL;
    END IF;
    s := rtrim(lower(subject), ' ');
    p := lower(pattern);
    esc := lower(escape_char);
    IF esc = '' THEN
        esc := NULL;
    END IF;
    slen := length(s);
    plen := length(p);
    WHILE si <= slen LOOP
        matched := false;
        is_star := false;
        adv := 1;
        IF pp <= plen THEN
            c := substr(p, pp, 1);
            sc := substr(s, si, 1);
            IF esc IS NOT NULL AND c = esc AND pp < plen THEN
                adv := 2;
                matched := sc = substr(p, pp + 1, 1);
            ELSIF c = '%' THEN
                is_star := true;
            ELSIF c = '_' THEN
                matched := true;
            ELSIF c = '[' THEN
                j := pp + 1;
                neg := false;
                IF j <= plen AND substr(p, j, 1) = '^' THEN
                    neg := true;
                    j := j + 1;
                END IF;
                close_at := 0;
                k := j + 1;
                WHILE k <= plen LOOP
                    IF substr(p, k, 1) = ']' THEN
                        close_at := k;
                        EXIT;
                    END IF;
                    k := k + 1;
                END LOOP;
                IF close_at = 0 THEN
                    matched := sc = '[';
                ELSE
                    adv := close_at - pp + 1;
                    hit := false;
                    k := j;
                    WHILE k < close_at LOOP
                        lo := substr(p, k, 1);
                        IF k + 2 < close_at AND substr(p, k + 1, 1) = '-' THEN
                            hi := substr(p, k + 2, 1);
                            IF sc COLLATE "C" >= lo COLLATE "C" AND sc COLLATE "C" <= hi COLLATE "C" THEN
                                hit := true;
                            END IF;
                            k := k + 3;
                        ELSE
                            IF sc = lo THEN
                                hit := true;
                            END IF;
                            k := k + 1;
                        END IF;
                    END LOOP;
                    matched := hit <> neg;
                END IF;
            ELSE
                matched := sc = c;
            END IF;
        END IF;
        IF is_star THEN
            star_p := pp;
            star_s := si;
            pp := pp + 1;
        ELSIF matched THEN
            si := si + 1;
            pp := pp + adv;
        ELSIF star_p > 0 THEN
            star_s := star_s + 1;
            si := star_s;
            pp := star_p + 1;
        ELSE
            RETURN false;
        END IF;
    END LOOP;
    WHILE pp <= plen AND substr(p, pp, 1) = '%' LOOP
        pp := pp + 1;
    END LOOP;
    RETURN pp > plen;
END;
$$;

CREATE OR REPLACE FUNCTION tsql.like(subject text, pattern text)
RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT tsql.like(subject, pattern, NULL::text);
$$;

-- tsql.dateadd(part, n, d) — T-SQL DATEADD. Months and years clamp to
-- the end of the month like SQL Server (Jan 31 + 1 month = Feb 28/29);
-- quarter is 3 months, week 7 days, dayofyear / weekday add days;
-- nanoseconds are truncated to microseconds (PG's resolution). The date
-- overload refuses time parts, as SQL Server does.
CREATE OR REPLACE FUNCTION tsql.dateadd_interval(part text, n bigint)
RETURNS interval
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
BEGIN
    IF n IS NULL THEN
        RETURN NULL;
    END IF;
    CASE tsql.datepart_canon(part)
        WHEN 'year' THEN RETURN n * interval '1 year';
        WHEN 'quarter' THEN RETURN (n * 3) * interval '1 month';
        WHEN 'month' THEN RETURN n * interval '1 month';
        WHEN 'dayofyear', 'day', 'weekday' THEN RETURN n * interval '1 day';
        WHEN 'week' THEN RETURN (n * 7) * interval '1 day';
        WHEN 'hour' THEN RETURN n * interval '1 hour';
        WHEN 'minute' THEN RETURN n * interval '1 minute';
        WHEN 'second' THEN RETURN n * interval '1 second';
        WHEN 'millisecond' THEN RETURN n * interval '1 millisecond';
        WHEN 'microsecond' THEN RETURN n * interval '1 microsecond';
        WHEN 'nanosecond' THEN RETURN (n / 1000) * interval '1 microsecond';
        ELSE
            RAISE EXCEPTION 'tsql.dateadd: unsupported date part "%"', part USING ERRCODE = '22023';
    END CASE;
END;
$$;

CREATE OR REPLACE FUNCTION tsql.dateadd(part text, n bigint, d timestamp)
RETURNS timestamp
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT d + tsql.dateadd_interval(part, n);
$$;

CREATE OR REPLACE FUNCTION tsql.dateadd(part text, n bigint, d timestamptz)
RETURNS timestamptz
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT d + tsql.dateadd_interval(part, n);
$$;

CREATE OR REPLACE FUNCTION tsql.dateadd(part text, n bigint, d date)
RETURNS date
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
BEGIN
    IF tsql.datepart_canon(part) IN ('hour', 'minute', 'second', 'millisecond', 'microsecond', 'nanosecond') THEN
        RAISE EXCEPTION 'tsql.dateadd: the date part "%" is not supported for data type date', part USING ERRCODE = '22023';
    END IF;
    RETURN (d + tsql.dateadd_interval(part, n))::date;
END;
$$;

-- tsql.datediff(part, a, b) — T-SQL DATEDIFF / DATEDIFF_BIG: the number
-- of part boundaries crossed from a to b, not the elapsed time rounded
-- (datediff(year, '2024-12-31', '2025-01-01') = 1). year / quarter /
-- month compare calendar fields, day counts midnights, week counts
-- Sundays (SQL Server ignores DATEFIRST here), hour / minute / second /
-- millisecond / microsecond subtract the values truncated to that unit.
-- timestamptz values are compared in the session time zone (the
-- translator maps GETDATE() to now()).
CREATE OR REPLACE FUNCTION tsql.datediff(part text, a timestamp, b timestamp)
RETURNS bigint
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
BEGIN
    IF a IS NULL OR b IS NULL THEN
        RETURN NULL;
    END IF;
    CASE tsql.datepart_canon(part)
        WHEN 'year' THEN
            RETURN extract(year FROM b)::bigint - extract(year FROM a)::bigint;
        WHEN 'quarter' THEN
            RETURN (extract(year FROM b)::bigint * 4 + extract(quarter FROM b)::bigint)
                 - (extract(year FROM a)::bigint * 4 + extract(quarter FROM a)::bigint);
        WHEN 'month' THEN
            RETURN (extract(year FROM b)::bigint * 12 + extract(month FROM b)::bigint)
                 - (extract(year FROM a)::bigint * 12 + extract(month FROM a)::bigint);
        WHEN 'dayofyear', 'day', 'weekday' THEN
            RETURN (b::date - a::date)::bigint;
        WHEN 'week' THEN
            RETURN (((b::date - extract(dow FROM b)::integer) - (a::date - extract(dow FROM a)::integer)) / 7)::bigint;
        WHEN 'hour' THEN
            RETURN round(extract(epoch FROM date_trunc('hour', b)) - extract(epoch FROM date_trunc('hour', a)))::bigint / 3600;
        WHEN 'minute' THEN
            RETURN round(extract(epoch FROM date_trunc('minute', b)) - extract(epoch FROM date_trunc('minute', a)))::bigint / 60;
        WHEN 'second' THEN
            RETURN round(extract(epoch FROM date_trunc('second', b)) - extract(epoch FROM date_trunc('second', a)))::bigint;
        WHEN 'millisecond' THEN
            RETURN round((extract(epoch FROM date_trunc('milliseconds', b)) - extract(epoch FROM date_trunc('milliseconds', a))) * 1000)::bigint;
        WHEN 'microsecond' THEN
            RETURN round((extract(epoch FROM b) - extract(epoch FROM a)) * 1000000)::bigint;
        WHEN 'nanosecond' THEN
            RETURN round((extract(epoch FROM b) - extract(epoch FROM a)) * 1000000)::bigint * 1000;
        ELSE
            RAISE EXCEPTION 'tsql.datediff: unsupported date part "%"', part USING ERRCODE = '22023';
    END CASE;
END;
$$;

CREATE OR REPLACE FUNCTION tsql.datediff(part text, a timestamptz, b timestamptz)
RETURNS bigint
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT tsql.datediff(part, a::timestamp, b::timestamp);
$$;

CREATE OR REPLACE FUNCTION tsql.datediff(part text, a date, b date)
RETURNS bigint
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT tsql.datediff(part, a::timestamp, b::timestamp);
$$;

-- tsql.datepart(part, d) — T-SQL DATEPART under SET DATEFIRST 7 (the
-- us_english default): weekday is 1 on Sunday, week counts from the week
-- holding January 1st (week = 1 + (dayofyear - 1 + weekday(Jan 1) - 1) / 7),
-- iso_week is the ISO 8601 week. tzoffset is the offset in minutes of a
-- timestamptz in the session time zone (0 for the other types).
CREATE OR REPLACE FUNCTION tsql.datepart(part text, d timestamp)
RETURNS integer
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    jan1_wd integer;
BEGIN
    IF d IS NULL THEN
        RETURN NULL;
    END IF;
    CASE tsql.datepart_canon(part)
        WHEN 'year' THEN RETURN extract(year FROM d)::integer;
        WHEN 'quarter' THEN RETURN extract(quarter FROM d)::integer;
        WHEN 'month' THEN RETURN extract(month FROM d)::integer;
        WHEN 'dayofyear' THEN RETURN extract(doy FROM d)::integer;
        WHEN 'day' THEN RETURN extract(day FROM d)::integer;
        WHEN 'weekday' THEN RETURN extract(dow FROM d)::integer + 1;
        WHEN 'week' THEN
            jan1_wd := extract(dow FROM date_trunc('year', d))::integer + 1;
            RETURN 1 + (extract(doy FROM d)::integer - 1 + jan1_wd - 1) / 7;
        WHEN 'iso_week' THEN RETURN extract(week FROM d)::integer;
        WHEN 'hour' THEN RETURN extract(hour FROM d)::integer;
        WHEN 'minute' THEN RETURN extract(minute FROM d)::integer;
        WHEN 'second' THEN RETURN floor(extract(second FROM d))::integer;
        WHEN 'millisecond' THEN RETURN floor(extract(milliseconds FROM d))::integer % 1000;
        WHEN 'microsecond' THEN RETURN floor(extract(microseconds FROM d))::integer % 1000000;
        WHEN 'nanosecond' THEN RETURN (floor(extract(microseconds FROM d))::integer % 1000000) * 1000;
        WHEN 'tzoffset' THEN RETURN 0;
        ELSE
            RAISE EXCEPTION 'tsql.datepart: unsupported date part "%"', part USING ERRCODE = '22023';
    END CASE;
END;
$$;

CREATE OR REPLACE FUNCTION tsql.datepart(part text, d timestamptz)
RETURNS integer
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT CASE tsql.datepart_canon(part)
        WHEN 'tzoffset' THEN (extract(timezone FROM d) / 60)::integer
        ELSE tsql.datepart(part, d::timestamp)
    END;
$$;

CREATE OR REPLACE FUNCTION tsql.datepart(part text, d date)
RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT tsql.datepart(part, d::timestamp);
$$;

-- tsql.datename(part, d) — T-SQL DATENAME (us_english): the English
-- month and weekday names, the DATEPART number as text for the other
-- parts. The names come from constant arrays, so the result does not
-- depend on lc_time.
CREATE OR REPLACE FUNCTION tsql.datename(part text, d timestamp)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE tsql.datepart_canon(part)
        WHEN 'month' THEN (ARRAY['January', 'February', 'March', 'April', 'May', 'June', 'July',
                                 'August', 'September', 'October', 'November', 'December'])[extract(month FROM d)::integer]
        WHEN 'weekday' THEN (ARRAY['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday',
                                   'Saturday'])[extract(dow FROM d)::integer + 1]
        ELSE tsql.datepart(part, d)::text
    END;
$$;

CREATE OR REPLACE FUNCTION tsql.datename(part text, d timestamptz)
RETURNS text
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT CASE tsql.datepart_canon(part)
        WHEN 'tzoffset' THEN tsql.datepart(part, d)::text
        ELSE tsql.datename(part, d::timestamp)
    END;
$$;

CREATE OR REPLACE FUNCTION tsql.datename(part text, d date)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT tsql.datename(part, d::timestamp);
$$;

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

-- tsql.convert_style(d, style) — the text CONVERT(varchar, d, style)
-- produces for the date styles the translator emulates:
--   101 mm/dd/yyyy   103 dd/mm/yyyy   104 dd.mm.yyyy   105 dd-mm-yyyy
--   110 mm-dd-yyyy   111 yyyy/mm/dd   112 yyyymmdd
--   120 yyyy-mm-dd hh:mi:ss          121 yyyy-mm-dd hh:mi:ss.mmm
--   126 yyyy-mm-ddThh:mi:ss[.mmm]    (milliseconds omitted when 0)
-- A timestamptz is formatted in the session time zone.
CREATE OR REPLACE FUNCTION tsql.convert_style(d timestamp, style integer)
RETURNS text
LANGUAGE plpgsql STABLE PARALLEL SAFE AS $$
BEGIN
    IF d IS NULL OR style IS NULL THEN
        RETURN NULL;
    END IF;
    CASE style
        WHEN 101 THEN RETURN to_char(d, 'MM/DD/YYYY');
        WHEN 103 THEN RETURN to_char(d, 'DD/MM/YYYY');
        WHEN 104 THEN RETURN to_char(d, 'DD.MM.YYYY');
        WHEN 105 THEN RETURN to_char(d, 'DD-MM-YYYY');
        WHEN 110 THEN RETURN to_char(d, 'MM-DD-YYYY');
        WHEN 111 THEN RETURN to_char(d, 'YYYY/MM/DD');
        WHEN 112 THEN RETURN to_char(d, 'YYYYMMDD');
        WHEN 120 THEN RETURN to_char(d, 'YYYY-MM-DD HH24:MI:SS');
        WHEN 121 THEN RETURN to_char(d, 'YYYY-MM-DD HH24:MI:SS.MS');
        WHEN 126 THEN
            IF floor(extract(milliseconds FROM d))::integer % 1000 = 0 THEN
                RETURN to_char(d, 'YYYY-MM-DD"T"HH24:MI:SS');
            END IF;
            RETURN to_char(d, 'YYYY-MM-DD"T"HH24:MI:SS.MS');
        ELSE
            RAISE EXCEPTION 'tsql.convert_style: unsupported style %', style USING ERRCODE = '22023';
    END CASE;
END;
$$;

CREATE OR REPLACE FUNCTION tsql.convert_style(d timestamptz, style integer)
RETURNS text
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT tsql.convert_style(d::timestamp, style);
$$;
