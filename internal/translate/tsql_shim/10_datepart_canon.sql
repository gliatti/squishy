
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
