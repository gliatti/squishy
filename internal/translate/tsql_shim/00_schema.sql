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
