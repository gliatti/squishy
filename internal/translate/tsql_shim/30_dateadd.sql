
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
