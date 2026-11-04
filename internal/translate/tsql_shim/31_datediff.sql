
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
