
-- tsql.str_to_timestamp_mdy(s) / tsql.str_to_timestamp_dmy(s) — the
-- datetime CONVERT(datetime, s, style) reads from a string. A string
-- style of CONVERT does not fix one exact layout: SQL Server parses the
-- usual separated and unseparated forms, and the style only decides the
-- order of the day and the month — month first for 101 / 110 / 111,
-- day first for 103 / 104 / 105 ('02/03/2004' is February 3 with 101,
-- March 2 with 103), while 'yyyymmdd' and year-first forms read the
-- year first. PostgreSQL's own datetime input parser does the same under
-- DateStyle MDY / DMY, so each helper is that parser with the DateStyle
-- fixed by its SET clause (restored when it returns): no layout is
-- scanned here. One difference is kept: with a day-first style SQL
-- Server reads a dashed year-first date as year-day-month
-- ('2004-02-03' → March 2), PostgreSQL as ISO (February 3).
CREATE OR REPLACE FUNCTION tsql.str_to_timestamp_mdy(s text)
RETURNS timestamp
LANGUAGE sql STABLE STRICT PARALLEL SAFE
SET datestyle = 'ISO, MDY' AS $$
    SELECT s::timestamp
$$;

CREATE OR REPLACE FUNCTION tsql.str_to_timestamp_dmy(s text)
RETURNS timestamp
LANGUAGE sql STABLE STRICT PARALLEL SAFE
SET datestyle = 'ISO, DMY' AS $$
    SELECT s::timestamp
$$;
