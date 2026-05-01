# TODO

Open work items for the squishy translator. Add new entries at the top.

## AST-only purity

### `mapBareReturnInLiteral` (and its sister mappers) operates on Literal text

`internal/translate/oracle_dyn_trigger.go` ships a family of literal-text
mutators that translate Oracle SQL fragments to PG inside the runtime-built
trigger body:

- `mapOracleTypesInLiteral` — `varchar2` → `VARCHAR`, etc.
- `dropFromDualInLiteral` — strip ` FROM DUAL`
- `mapTriggerPseudoVarsInLiteral` — `INSERTING` → `(TG_OP = 'INSERT')`
- `mapBareReturnInLiteral` — `RETURN ;` → `RETURN NULL ;` (added 2026-05-01)

Each one mutates the `Text` field of a SINGLE `*ast.Literal` node — never
crosses node boundaries, never operates on a SQL composite. CLAUDE.md
allows this as a "string-op-on-an-isolated-scalar" pattern (the same one
used for identifier folding), and the existing mappers have used it
since the visitor was first written.

**Open question (raised by @robin 2026-05-01):** is this strict enough?
A purer alternative would be:

1. For each contributing Literal that holds a body fragment, route through
   `oracle.Parse` to get a sub-AST.
2. Apply the per-construct visitors (`VisitOracleHashInIdentLiterals`,
   `VisitOracleDbmsLobSubstr`, type-mapping at `*ast.UserDefinedType`, etc.)
   on that sub-AST.
3. Emit the translated fragment back via `pgast.Write` and re-wrap as a
   Literal.

Cost: per-fragment parser invocation + AST round-trip on every
trigger-builder Literal. Benefit: zero string-ops on SQL fragments,
strict CLAUDE.md compliance. Decision pending.

If we move forward, drop `mapBareReturnInLiteral`,
`mapTriggerPseudoVarsInLiteral`, `dropFromDualInLiteral`,
`mapOracleTypesInLiteral`, and the call site in
`reparseAndTranslateBodyParts` — replace with the parse-AST-emit pipeline.

## Visitor wrap doesn't yet drop orphan dbms_sql cleanup calls

After `rewriteDynTriggerRange` wraps the dyn-trigger range in a synthetic
`DECLARE … BEGIN … END;` block, the surrounding code's
`rows := dbms_sql.execute(cursor)` and `dbms_sql.close_cursor(cursor)`
calls (which followed the original `dbms_sql.parse` sink) are left in
place OUTSIDE the wrapper. They'd raise "cannot to prepare plan" because
the cursor was opened-but-not-parsed once we replaced the original parse
call with our two EXECUTEs.

Workaround in place: the visitor injects a no-op
`CALL dbms_sql.parse(<cursor>, 'SELECT 1')` inside the wrapper so the
cursor stays parseable for the post-wrapper execute/close. Functional
but inelegant.

Cleaner fix: scan AHEAD of `m.execIdx` for the cursor's `execute` /
`close_cursor` calls (and BACKWARD for the `cursor := dbms_sql.open_cursor`
init) and drop the entire trio when present. Requires tracking which
cursor variable backs the original sink — already exposed by
`dbmsSqlParseCursorOf`.
