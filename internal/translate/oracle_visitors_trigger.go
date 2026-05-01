package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_visitors_trigger.go — AST-level rewriters for Oracle trigger
// constructs that the legacy text pipeline handled via post-rendering
// substitutions.
//
// The flagship visitor here is the row-alias rewriter: Oracle triggers
// declared with `REFERENCING NEW AS NR OLD AS OR1` use NR/OR1 in place
// of the NEW/OLD pseudorecord names. PG plpgsql doesn't support
// REFERENCING aliases — the NEW/OLD names are fixed — so each NR/OR1
// reference must be renamed before the body reaches the writer.
//
// Doing this at the AST level (Ident.Parts substitution) instead of
// scanning the rendered text is what unblocks the IfStmt typed Cond
// migration deferred from Phase 1.1: when the parser produces typed
// Idents inside the IF condition, the visitor renames `NR` → `NEW`
// in place, and the writer emits the unquoted-trigger-pseudocol path
// (see expr_writer.go isPLpgSQLPseudoCol) — `NEW.col` instead of the
// quoted `"NEW"."col"` that breaks PG semantics.

// MakeRowAliasVisitor returns a Rewriter that substitutes Ident.Parts
// matching `from` (case-insensitive, single-part match only) with
// `to`. The match is exact on the first part of the dotted ident path
// — a multi-part Ident like NR.QTY has Parts[0] == "NR" and gets
// rewritten to ["NEW", "QTY"]. Whole-name match keeps schema-
// qualified references safe (a column named NR inside a different
// schema/table won't be touched).
//
// `from` empty, or a no-op rename (strings.EqualFold(from, to)),
// returns an identity rewriter so callers can wire the visitor
// unconditionally.
func MakeRowAliasVisitor(from, to string) ast.Rewriter {
	if from == "" || strings.EqualFold(from, to) {
		return identityRewriter
	}
	return func(n ast.Node) ast.Node {
		id, ok := n.(*ast.Ident)
		if !ok {
			return n
		}
		if len(id.Parts) == 0 {
			return n
		}
		if !strings.EqualFold(id.Parts[0], from) {
			return n
		}
		// Substitute Parts[0] in a fresh slice — never mutate the
		// caller's Ident in place because the same node may be
		// referenced from multiple parents.
		newParts := make([]string, len(id.Parts))
		newParts[0] = to
		copy(newParts[1:], id.Parts[1:])
		return &ast.Ident{Parts: newParts, Backtick: id.Backtick, P: id.P}
	}
}

// identityRewriter — convenience no-op returned by MakeRowAliasVisitor
// when from is empty or matches to. Sharing one instance avoids
// allocating an empty closure per trigger.
var identityRewriter ast.Rewriter = func(n ast.Node) ast.Node { return n }

// MakeTriggerAliasComposite returns a single Rewriter that runs the
// NEW-alias rename followed by the OLD-alias rename. Either side may
// be empty; the composite only substitutes the non-empty halves.
//
// Naming this `composite` (vs. exposing two separate rewriters) makes
// the orchestrator wiring cleaner: the trigger translator passes one
// callable to ast.Rewrite regardless of which aliases the source
// trigger actually used.
func MakeTriggerAliasComposite(newAlias, oldAlias string) ast.Rewriter {
	rNew := MakeRowAliasVisitor(newAlias, "NEW")
	rOld := MakeRowAliasVisitor(oldAlias, "OLD")
	return ast.Compose(rNew, rOld)
}
