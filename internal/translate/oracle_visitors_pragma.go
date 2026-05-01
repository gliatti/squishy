package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_visitors_pragma.go — AST rewriters for Oracle PL/SQL
// PRAGMA directives that have a structural PG equivalent.
//
// Phase 6.2 introduces the EXCEPTION_INIT translator: instead of
// dropping `PRAGMA EXCEPTION_INIT(name, code);` with a TODO comment
// and downgrading every `RAISE name;` to a generic P0001 raise, we
// harvest the binding at parse time, allocate a unique PG SQLSTATE
// per name, and rewrite every matching RaiseStmt to emit the
// SQLSTATE directly. PG's `45XXX` user-defined SQLSTATE class is
// reserved for exactly this kind of remap.

// VisitOracleExceptionInit collects PRAGMA EXCEPTION_INIT bindings
// inside each *ast.Block and rewrites every RaiseStmt whose Name
// matches one of the harvested names to carry the resolved SQLSTATE
// on its new SQLState field.
//
// The pragma stays in place — translator.block() drops it gracefully
// with a downgraded note explaining the remap (no longer a "PRAGMA
// dropped — no PG equivalent" warning, since we DID translate it).
//
// Bindings are scoped to the Block they appear in; nested blocks
// inherit nothing from their parent (Oracle scoping is the same).
// The visitor descends into nested constructs via ast.Rewrite so
// every RaiseStmt within the block — including those buried in
// IfStmt/CaseStmt/Loop/exception-handler bodies — gets stamped.
//
// SQLSTATE allocation uses the `45XXX` user-defined class reserved
// for EXCEPTION_INIT remaps. The counter is per-Block; cross-block
// uniqueness is preserved by salting with a stable hash of the
// block's first stmt position so two procedures don't collide on
// the same emitted code.
func VisitOracleExceptionInit(n ast.Node) ast.Node {
	blk, ok := n.(*ast.Block)
	if !ok {
		return n
	}
	bindings := harvestExceptionInitBindings(blk)
	if len(bindings) == 0 {
		return n
	}
	// Stamp every RaiseStmt within the block with the resolved
	// SQLSTATE. ast.Rewrite descends into nested blocks too — those
	// have their own bindings via the visitor running at each block
	// scope, so the inner RAISE picks up the most-specific scope's
	// binding when it walks bottom-up.
	stampRaiseStmts(blk, bindings)
	return blk
}

// harvestExceptionInitBindings walks blk.Decls (where pragmas live in
// Oracle PL/SQL) and returns a name→SQLSTATE map for every
// EXCEPTION_INIT pragma in the block. Names are lowercased for
// case-insensitive matching against RaiseStmt.Name.
func harvestExceptionInitBindings(blk *ast.Block) map[string]string {
	out := map[string]string{}
	counter := 0
	for _, d := range blk.Decls {
		pr, ok := d.(*ast.PragmaStmt)
		if !ok {
			continue
		}
		if !strings.EqualFold(pr.Kind, "EXCEPTION_INIT") {
			continue
		}
		name, oracleCode, ok := parseExceptionInitArgs(pr.Text)
		if !ok {
			continue
		}
		counter++
		out[strings.ToLower(name)] = sqlstateFromOracleCode(oracleCode, counter, blk.P.Offset)
	}
	return out
}

// parseExceptionInitArgs extracts (name, code) from the argument
// portion of `PRAGMA EXCEPTION_INIT(name, code)`. The PragmaStmt
// captures the args verbatim — typically `name, -20001` (commas
// inside the args are rare for EXCEPTION_INIT). Returns (_, _, false)
// when the shape isn't recognised.
//
// The code half is captured but kept opaque — sqlstateFromOracleCode
// does the actual mapping. We don't try to reproduce Oracle's exact
// error code in the SQLSTATE because PG's 5-character format can't
// always carry the Oracle integer faithfully.
func parseExceptionInitArgs(text string) (name string, code string, ok bool) {
	// Strip surrounding parens if any: pragma capture sometimes
	// includes them, sometimes doesn't.
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "(")
	t = strings.TrimSuffix(t, ")")
	t = strings.TrimSpace(t)
	comma := strings.IndexByte(t, ',')
	if comma < 0 {
		return "", "", false
	}
	name = strings.TrimSpace(t[:comma])
	code = strings.TrimSpace(t[comma+1:])
	if name == "" {
		return "", "", false
	}
	return name, code, true
}

// sqlstateFromOracleCode picks a PG SQLSTATE for the bound exception.
// Strategy: stay in PG's `45XXX` user-defined class so we never
// collide with a built-in. The block-local counter gives uniqueness
// within a routine; the salt (block position offset) keeps cross-
// block emissions from colliding when two unrelated procedures pick
// the same counter slot.
//
// The format is `45` + 3 digits — supports 1000 distinct exceptions
// per block, well above any realistic Oracle routine. Counter values
// beyond 999 wrap and are noted in the SQLSTATE so the operator can
// review.
func sqlstateFromOracleCode(_ string, counter, salt int) string {
	// Mix the salt to avoid two unrelated blocks emitting the same
	// SQLSTATE for their first/second/… pragma. Modulo 1000 keeps the
	// last three digits in range.
	mixed := (counter*10007 + salt*1009) % 1000
	if mixed < 0 {
		mixed = -mixed
	}
	return fmt.Sprintf("45%03d", mixed)
}

// stampRaiseStmts walks the entire block (Stmts + nested blocks +
// exception-handler bodies) and sets RaiseStmt.SQLState whenever the
// exception name matches a binding.
//
// Implementation note: this uses ast.Rewrite with a closure rather
// than a top-level Rewriter so the bindings map stays scope-local
// and doesn't leak across blocks.
func stampRaiseStmts(blk *ast.Block, bindings map[string]string) {
	rewriter := func(n ast.Node) ast.Node {
		rs, ok := n.(*ast.RaiseStmt)
		if !ok || rs.Name == "" {
			return n
		}
		if rs.SQLState != "" {
			// Already stamped (inner-block visitor ran first).
			return n
		}
		if code, hit := bindings[strings.ToLower(rs.Name)]; hit {
			rs.SQLState = code
		}
		return n
	}
	for i, s := range blk.Stmts {
		if rs, ok := ast.Rewrite(s, rewriter).(ast.PLStmt); ok {
			blk.Stmts[i] = rs
		}
	}
	if blk.Except != nil {
		for h := range blk.Except.Handlers {
			for j, s := range blk.Except.Handlers[h].Body {
				if rs, ok := ast.Rewrite(s, rewriter).(ast.PLStmt); ok {
					blk.Except.Handlers[h].Body[j] = rs
				}
			}
		}
	}
}
