
-- T-SQL error functions read from a routine called by a CATCH block.
-- In SQL Server ERROR_MESSAGE() / ERROR_NUMBER() / … return the error
-- that sent execution to the innermost CATCH block in scope — also
-- inside a procedure that block calls (dbo.uspLogError's purpose) — and
-- NULL anywhere else. A PostgreSQL routine only sees SQLERRM / SQLSTATE
-- inside the EXCEPTION handler that caught the error, so a translated
-- CATCH block that calls a procedure publishes its error in the
-- transaction-local setting tsql.error_context (tsql.error_enter, which
-- returns the context it replaces) and restores the previous one when
-- it ends (tsql.error_leave); the error functions of the called routine
-- read it back (tsql.error_field). The setting is transaction-local
-- (set_config(…, true)) and transactional: a rolled-back subtransaction
-- restores it.

CREATE OR REPLACE FUNCTION tsql.error_enter(state text, message text)
RETURNS text
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    prev text := coalesce(current_setting('tsql.error_context', true), '');
BEGIN
    PERFORM set_config('tsql.error_context', json_build_object('state', state, 'message', message)::text, true);
    RETURN prev;
END;
$$;

CREATE OR REPLACE FUNCTION tsql.error_leave(prev text)
RETURNS void
LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    PERFORM set_config('tsql.error_context', coalesce(prev, ''), true);
END;
$$;

-- tsql.error_field(name) — a field ('state', 'message') of the error of
-- the CATCH block in scope, NULL outside one.
CREATE OR REPLACE FUNCTION tsql.error_field(name text)
RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('tsql.error_context', true), '')::json ->> name
$$;

-- tsql.error_number_of(sqlstate) — the SQL Server error number of the
-- PostgreSQL error class T-SQL code tests most (constraint violations,
-- conversions, division by zero, deadlocks); 50000 (a user error, as
-- RAISERROR / THROW raise by default) for the errors the translated code
-- raises itself and any other one.
CREATE OR REPLACE FUNCTION tsql.error_number_of(state text)
RETURNS integer
LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT CASE state
        WHEN '23505' THEN 2627   -- unique / primary key violation
        WHEN '23503' THEN 547    -- foreign key violation
        WHEN '23514' THEN 547    -- check violation
        WHEN '23502' THEN 515    -- NULL into a NOT NULL column
        WHEN '22012' THEN 8134   -- division by zero
        WHEN '22003' THEN 8115   -- arithmetic overflow
        WHEN '22001' THEN 8152   -- string data would be truncated
        WHEN '22P02' THEN 245    -- conversion failed
        WHEN '22007' THEN 241    -- date / time conversion failed
        WHEN '22008' THEN 242    -- date / time out of range
        WHEN '40P01' THEN 1205   -- deadlock victim
        WHEN '57014' THEN 3617   -- query cancelled
        ELSE 50000
    END
$$;
