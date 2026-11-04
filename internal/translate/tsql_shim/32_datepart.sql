
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
