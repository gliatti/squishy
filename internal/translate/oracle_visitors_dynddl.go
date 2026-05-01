package translate

import (
	"strings"

	oracledialect "gitlab.com/dalibo/squishy/internal/dialects/oracle"
	"gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_visitors_dynddl.go — AST-only rewriter for Oracle dynamic
// DDL (EXECUTE IMMEDIATE).
//
// Phase 1.8 made ExecuteImmediateStmt.SQL a typed concat tree
// (BinaryExpr{Op:"||"} of Literal + Ident operands). Phase 4 walks
// that tree at AST level:
//
//   - All-Literal concat (e.g. 'CREATE TABLE ' || 'foo' || ' (id NUMBER)')
//     folds to a single Literal so the runtime EXECUTE has no string-
//     concatenation work and the static SQL is visible in the
//     generated PL/pgSQL — easier to grep, easier to debug.
//
//   - Mixed Literal + non-Literal stays as-is. The existing EXECUTE
//     emitter handles the dynamic case correctly via the typed
//     concat tree (rawExpr renders each operand and the runtime
//     concatenation produces the final SQL). Phase 4's "best effort"
//     contract is intentional: when we can't fully resolve the SQL
//     statically, we don't try to half-resolve. The translator's
//     prerequisite layer will eventually flag specific
//     intractable shapes (CREATE TRIGGER built from variable
//     fragments) when Phase 6's e2e validation surfaces them.
//
// Visitor wired by RewriteOracleAST in oracle_translate.go.

// PrereqEmitter — callback the translator supplies so the dynamic-DDL
// visitor can surface a Prerequisite (typically blocking) when it
// finds a shape it cannot statically resolve. Optional: when nil the
// visitor still folds the all-Literal case but skips prereq emission
// (used by stand-alone visitor unit tests).
type PrereqEmitter func(Prerequisite)

// VisitOracleDynamicDDL is the fold-only Rewriter wired into
// RewriteOracleAST by default. The full re-parse-and-translate path
// — which uses the translator's Oracle parser + AST orchestrator +
// PG writers in concert — lives in MakeOracleDynamicDDLVisitor below
// because it needs a PrereqEmitter to surface unresolvable
// constructs as Prerequisites.
func VisitOracleDynamicDDL(n ast.Node) ast.Node {
	return makeDynDDLRewriter(nil)(n)
}

// MakeOracleDynamicDDLVisitor returns a Rewriter that:
//
//  1. Folds an all-Literal concat into a single Literal.
//  2. Re-parses the resulting Literal as Oracle DDL/DML, applies the
//     standard RewriteOracleAST orchestrator to the sub-AST, and
//     renders the result via the PG writers — replacing the Literal
//     with the translated SQL text.
//  3. For mixed concat trees (Literal + variable operands) emits a
//     blocking Prerequisite so the user reviews the routine: the SQL
//     statement is built at runtime from values squishy cannot see at
//     translation time, and PG cannot generally execute Oracle DDL
//     dynamically (CREATE TRIGGER body, CREATE TYPE attrs, …) the
//     same way Oracle does.
//
// emit may be nil — in which case the visitor degrades to step 1
// only (matching VisitOracleDynamicDDL's behaviour).
func MakeOracleDynamicDDLVisitor(emit PrereqEmitter) ast.Rewriter {
	return makeDynDDLRewriter(emit)
}

func makeDynDDLRewriter(emit PrereqEmitter) ast.Rewriter {
	return func(n ast.Node) ast.Node {
		ei, ok := n.(*ast.ExecuteImmediateStmt)
		if !ok {
			return n
		}
		parts := flattenConcat(ei.SQL)
		if len(parts) == 0 {
			return n
		}
		allLiteral := true
		for _, p := range parts {
			if _, ok := p.(*ast.Literal); !ok {
				allLiteral = false
				break
			}
		}
		if !allLiteral {
			// Mixed concat with variable operands. Emit a blocking
			// prereq so the user reviews the routine — squishy can't
			// resolve the SQL statically and PG's runtime EXECUTE may
			// not accept the Oracle-shaped output.
			if emit != nil {
				emit(makeDynDDLPrereq(ei))
			}
			return n
		}
		// All-Literal concat: fold to a single Literal so the runtime
		// EXECUTE has no concat work to do.
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.(*ast.Literal).Text)
		}
		folded := b.String()
		// Try to re-parse the folded SQL via the Oracle parser. On
		// success, run the rewrite orchestrator on each parsed stmt
		// and render via the PG writers. On failure (parser doesn't
		// recognise the shape, or the SQL is non-DML/non-DDL such as
		// `BEGIN <body> END;`), keep the folded Literal so the
		// runtime EXECUTE handles it directly.
		stmts, errs := oracledialect.Parse(folded)
		if len(errs) > 0 || len(stmts) == 0 {
			return &ast.ExecuteImmediateStmt{
				SQL:   &ast.Literal{Kind: "string", Text: folded, P: ei.P},
				Into:  ei.Into,
				Using: ei.Using,
				P:     ei.P,
			}
		}
		translated := translateInlineStmts(stmts)
		if translated == "" {
			// Re-parse worked but the translator didn't know how to
			// render the result. Fall back to the folded literal —
			// the runtime EXECUTE is still a strict improvement over
			// the original concat tree.
			return &ast.ExecuteImmediateStmt{
				SQL:   &ast.Literal{Kind: "string", Text: folded, P: ei.P},
				Into:  ei.Into,
				Using: ei.Using,
				P:     ei.P,
			}
		}
		return &ast.ExecuteImmediateStmt{
			SQL:   &ast.Literal{Kind: "string", Text: translated, P: ei.P},
			Into:  ei.Into,
			Using: ei.Using,
			P:     ei.P,
		}
	}
}

// translateInlineStmts walks each parsed Oracle stmt, applies the
// standard rewriter chain, and renders DML stmts via the PG writers.
// Anything that doesn't fit the supported set returns "" so the
// caller can fall back to the un-translated folded form.
//
// Supported: Select, Insert, Update, Delete, Merge — the writers in
// internal/dialects/postgres handle these natively. DDL stays
// out-of-scope at this level (the translator's main DDL pipeline
// owns CREATE TABLE / CREATE TRIGGER / etc., and shoehorning it
// here would duplicate that logic).
func translateInlineStmts(stmts []ast.Stmt) string {
	chain := rewriteOracleASTBaseChain()
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		rewritten := ast.Rewrite(s, chain)
		switch x := rewritten.(type) {
		case *ast.SelectStmt, *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt, *ast.MergeStmt:
			text := postgres.WriteStmt(x.(ast.Stmt))
			if text == "" {
				return ""
			}
			out = append(out, text)
		default:
			// DDL or PL/SQL block — out of scope for the inline path.
			return ""
		}
	}
	return strings.Join(out, "; ")
}

// makeDynDDLPrereq builds the canonical Prerequisite emitted when a
// mixed concat tree is encountered. The Object field stays empty (the
// visitor doesn't know which routine encloses the EXECUTE) — the
// caller can post-process to enrich.
func makeDynDDLPrereq(_ *ast.ExecuteImmediateStmt) Prerequisite {
	return Prerequisite{
		Severity: SeverityBlocking,
		Category: CatManualReview,
		Title:    "Dynamic DDL via runtime variable",
		Description: "An EXECUTE IMMEDIATE statement builds its SQL from " +
			"runtime variables (one or more concat operands are not string " +
			"literals). squishy cannot translate dynamic DDL whose body is " +
			"unknown at translation time, and PostgreSQL's runtime EXECUTE " +
			"may reject the Oracle-shaped output even when the source SQL " +
			"would be valid Oracle.",
		Remediation: "Inspect the routine, replace the EXECUTE IMMEDIATE " +
			"with explicit static CREATE/ALTER statements at deployment " +
			"time, or split the dynamic logic into a parameterised migration " +
			"step. If the dynamic SQL is purely DML (INSERT/UPDATE/DELETE), " +
			"prefer EXECUTE format('…', vars) so PG's quote_ident handling " +
			"keeps identifiers safe.",
	}
}

// flattenConcat linearises a `||` BinaryExpr tree into its leaf
// operands left-to-right. ParenExprs are unwrapped (Oracle treats
// `(a || b) || c` as `a || b || c`). Leaves that are themselves
// concat-shaped feed back into the flatten call so an arbitrary
// associativity round-trips correctly.
//
// Non-concat root: returns []Expr{root} so callers can uniformly
// iterate on the operand list.
func flattenConcat(e ast.Expr) []ast.Expr {
	if e == nil {
		return nil
	}
	var out []ast.Expr
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case *ast.BinaryExpr:
			if x.Op == "||" {
				walk(x.Lhs)
				walk(x.Rhs)
				return
			}
		case *ast.ParenExpr:
			walk(x.Inner)
			return
		}
		out = append(out, e)
	}
	walk(e)
	return out
}
