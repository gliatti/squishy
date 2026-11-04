
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
