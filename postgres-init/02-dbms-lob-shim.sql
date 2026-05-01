-- dbms_lob shim: orafce 4.x doesn't ship a dbms_lob schema. We provide a
-- thin compatibility layer that maps the dbms_lob.* functions our
-- translator emits to PG-native equivalents. Loaded after orafce.
--
-- The AST visitor `VisitOracleDbmsLobSubstr` already rewrites
-- `dbms_lob.substr(lob, len, off)` into the native `substr(lob, off, len)`
-- (with arg-order swap and ::int casts), so this schema is mostly a
-- runtime safety net for code paths the visitor misses (e.g. dbms_lob
-- references built dynamically inside string literals at runtime).

CREATE SCHEMA IF NOT EXISTS dbms_lob;
GRANT USAGE ON SCHEMA dbms_lob TO PUBLIC;

-- dbms_lob.substr(lob, amount, offset) — Oracle's signature is
-- (lob_loc, amount, offset DEFAULT 1). PG's substr is
-- (string, from, count). We swap the argument order in the wrapper
-- so callers from translated code keep the Oracle calling convention.
CREATE OR REPLACE FUNCTION dbms_lob.substr(lob text, amount numeric, offset_ numeric DEFAULT 1)
RETURNS text
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT substr(lob, offset_::int, amount::int);
$$;

CREATE OR REPLACE FUNCTION dbms_lob.substr(lob text, amount integer, offset_ integer DEFAULT 1)
RETURNS text
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT substr(lob, offset_, amount);
$$;

-- dbms_lob.getlength(lob) — returns the character length.
CREATE OR REPLACE FUNCTION dbms_lob.getlength(lob text)
RETURNS integer
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT length(lob);
$$;

-- dbms_lob.instr(lob, pattern[, start, occurrence]) — character offset
-- of pattern inside lob.
CREATE OR REPLACE FUNCTION dbms_lob.instr(lob text, pattern text, start_ integer DEFAULT 1, occurrence integer DEFAULT 1)
RETURNS integer
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE
    pos integer := start_;
    found integer := 0;
    i integer := 0;
BEGIN
    WHILE i < occurrence LOOP
        pos := position(pattern in substr(lob, pos));
        IF pos = 0 THEN
            RETURN 0;
        END IF;
        pos := pos + start_ - 1;
        found := pos;
        i := i + 1;
        pos := pos + 1;
        start_ := pos;
    END LOOP;
    RETURN found;
END;
$$;

GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA dbms_lob TO PUBLIC;

-- ---------------------------------------------------------------------------
-- dbms_sql wrappers: orafce ships dbms_sql but with a narrower signature
-- set than Oracle. The translator emits trigger-build code that splits
-- the payload into a text[] when LENGTH > 32K and calls
-- dbms_sql.parse(cursor, text[]). orafce only has parse(int, varchar2),
-- so we concat the array elements and forward.
-- ---------------------------------------------------------------------------

-- Array variant of parse: concat the chunks and EXECUTE the
-- statement directly. orafce's dbms_sql.parse(int, varchar2) caps
-- payloads at varchar2 size, but the routines that hit the array
-- form do so precisely BECAUSE the payload exceeds 32K — so the
-- forward path would fail. The matching dbms_sql.execute(cursor)
-- becomes a no-op once the payload is already executed.
CREATE OR REPLACE PROCEDURE dbms_sql.parse(c integer, stmt text[])
LANGUAGE plpgsql AS $$
DECLARE
    full_stmt text;
BEGIN
    SELECT string_agg(elem, '' ORDER BY ord) INTO full_stmt FROM unnest(stmt) WITH ORDINALITY t(elem, ord);
    EXECUTE full_stmt;
END;
$$;

-- Numeric-cursor overloads: DRSE-style code declares the cursor handle
-- as NUMBER (mapped to NUMERIC) but orafce expects integer. Cast and
-- forward.
CREATE OR REPLACE PROCEDURE dbms_sql.parse(c numeric, stmt text)
LANGUAGE plpgsql AS $$
BEGIN
    CALL dbms_sql.parse(c::int, stmt::oracle.varchar2);
END;
$$;

CREATE OR REPLACE PROCEDURE dbms_sql.parse(c numeric, stmt text[])
LANGUAGE plpgsql AS $$
DECLARE
    full_stmt text;
BEGIN
    SELECT string_agg(elem, '' ORDER BY ord) INTO full_stmt FROM unnest(stmt) WITH ORDINALITY t(elem, ord);
    EXECUTE full_stmt;
END;
$$;

CREATE OR REPLACE FUNCTION dbms_sql.execute(c numeric)
RETURNS bigint LANGUAGE sql AS $$
    SELECT dbms_sql.execute(c::int);
$$;

CREATE OR REPLACE PROCEDURE dbms_sql.close_cursor(c numeric)
LANGUAGE plpgsql AS $$
BEGIN
    CALL dbms_sql.close_cursor(c::int);
END;
$$;

GRANT EXECUTE ON ALL ROUTINES IN SCHEMA dbms_sql TO PUBLIC;

-- ---------------------------------------------------------------------------
-- sys_context shim: orafce ships dbms_session.set_context but no
-- top-level sys_context function. DRSE-style routines call
-- `SYS_CONTEXT('USERENV', '<param>')` from inside trigger bodies built
-- at runtime; we map the common USERENV parameters to PG-native
-- equivalents and return '' for unknown ones so the trigger body
-- compiles + executes without raising "function sys_context does not
-- exist".
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION public.sys_context(namespace text, parameter text)
RETURNS text LANGUAGE sql IMMUTABLE AS $f$
    SELECT CASE
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'OS_USER'           THEN session_user::text
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'CURRENT_USER'      THEN current_user::text
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'SESSION_USER'      THEN session_user::text
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'CURRENT_SCHEMA'    THEN current_schema::text
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'TERMINAL'          THEN coalesce(current_setting('squishy.terminal', true), '')
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'CLIENT_IDENTIFIER' THEN coalesce(current_setting('squishy.client_identifier', true), '')
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'SESSIONID'         THEN pg_backend_pid()::text
        WHEN upper(namespace) = 'USERENV' AND upper(parameter) = 'IP_ADDRESS'        THEN coalesce(inet_client_addr()::text, '')
        ELSE ''
    END;
$f$;
GRANT EXECUTE ON FUNCTION public.sys_context(text, text) TO PUBLIC;
