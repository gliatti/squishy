
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
