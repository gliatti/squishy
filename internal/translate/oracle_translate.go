package translate

import (
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_translate.go — orchestrator for the Phase 3 AST rewriters.
//
// RewriteOracleAST composes every Oracle-specific Rewriter currently
// implemented and runs them in a single Compose chain. The chain is
// applied via ast.Rewrite to each PLStmt in the parsed routine body
// before the existing pgast-based translation kicks in. This way a
// rewritten node (e.g. a SequenceRef → nextval('seq') FuncCall)
// reaches the pgast emitter as a typed expression and the legacy
// text passes that scan for the same Oracle pattern become no-ops.
//
// Visitor ordering matters when two passes could match the same
// node — outer-rewriter-first is the rule of thumb so child
// substitutions feed into the parent's pattern. The current set is
// chosen so each pass operates on a disjoint AST shape; adding a
// new visitor that overlaps with another requires updating the
// composition order.
//
// During the migration window the legacy text pipeline still runs
// after RewriteOracleAST — visitors that don't fully cover their
// pattern set let the text pass clean up the residue. Phase 5
// removes the legacy pipeline once every visitor has full coverage.

// RewriteOracleAST is the composed Rewriter applied to every parsed
// PL/SQL stmt before pgast translation. Callers should always invoke
// it via ast.Rewrite(node, RewriteOracleAST) so child substitutions
// propagate through the post-order walker.
//
// The orchestration order:
//
//  1. Function-call name normalisations: strip SYS., qualify decode/
//     to_char, trim dbms_sql.parse's language arg. These run first so
//     downstream visitors that match by Name see the canonical form.
//  2. Function-call argument substitutions: sys_context, userenv,
//     translate_using.
//  3. Operator rewrites: MOD infix → mod() call.
//  4. Oracle pseudo-column substitutions: SequenceRef → nextval/
//     currval. CursorAttr is consumed directly by translator.rawExpr
//     (no visitor needed since the substitution happens at write
//     time, not at AST level).
//  5. SelectStmt-level rewrites: rownum_limit, listagg.
var RewriteOracleAST = ast.Compose(append(
	rewriteOracleASTBase(),
	VisitOracleDynamicDDL,
)...)

// rewriteOracleASTBase is the rewriter chain WITHOUT the
// dynamic-DDL visitor. Used both as the spine of RewriteOracleAST and
// (cycle-free) by VisitOracleDynamicDDL's inner re-parse-and-translate
// path: when the visitor folds a static EXECUTE IMMEDIATE 'CREATE
// TABLE …' it re-parses the literal and runs this base chain on the
// inner stmts — recursing through the full chain would cause an init
// cycle since the dyn-DDL visitor sits inside RewriteOracleAST.
func rewriteOracleASTBase() []ast.Rewriter {
	return []ast.Rewriter{
		// First in the chain so every Ident reaching downstream
		// visitors is already lowercase-folded (Backtick==false). Some
		// visitors materialise sub-Idents from the source Ident's
		// parts (e.g. VisitOracleCollectionTokens splits
		// `arr.COUNT` into `array_length(arr, 1)` — `arr` carries
		// over the source casing). Folding upstream means the
		// sub-Ident inherits the lowercased text by construction.
		VisitOracleIdentCaseFold,
		VisitOracleStripSysPrefix,
		VisitOracleQuotedPackageCalls,
		VisitOracleDbmsUtilityFormatCallStack,
		VisitOracleTrunc,
		VisitOracleQualifyDecode,
		VisitOracleQualifyToCharSingleArg,
		VisitOracleTrimDbmsSqlParseLanguageArg,
		VisitOracleSysContext,
		VisitOracleUserenv,
		VisitOracleTranslateUsing,
		VisitOracleModOperator,
		VisitOracleSequenceRef,
		VisitOracleRownumLimit,
		VisitOracleListagg,
		VisitOracleNestedAggregates,
		VisitOracleOuterJoin,
		VisitOracleExceptionInit,
		VisitOracleDictionaryViews,
		VisitOracleHashInIdentLiterals,
		VisitOracleCollectionTokens,
		// Promote `dbms_sql.open_cursor` (Ident, no parens) to a
		// FuncCall so the writer emits `dbms_sql.open_cursor()` —
		// PG treats the unparen'd form as a relation ref.
		VisitOracleNoArgsPackageCall,
		// Map `dbms_lob.substr(lob, len, off)` → PG-native
		// `substr(lob, off, len)` (arg order swap).
		VisitOracleDbmsLobSubstr,
		// Also last in the chain — covers Idents materialised by
		// upstream visitors with literal uppercase strings (e.g. an
		// Ident built from a FuncCall.Name that the parser kept
		// upper-cased). Idempotent on already-lowered Idents so the
		// double application is safe.
		VisitOracleIdentCaseFold,
		// (VisitOracleLiteralTypeMap was added then removed: while
		// it correctly maps `varchar2(7)` → `VARCHAR(7)` in trigger
		// fragments, it also rewrites the *value* literals that DRSE-
		// class procedures use to simulate the Oracle data dictionary
		// in PG — `CASE WHEN typname = 'varchar' THEN 'VARCHAR2'`
		// would become `THEN 'VARCHAR'`, breaking the downstream
		// CASE WHEN data_type = 'VARCHAR2' check. Use the dyn-DDL
		// visitor's targeted re-translation pass instead, which
		// only touches Literals it positively identifies as part of
		// a runtime-built DDL statement.)
	}
}

// rewriteOracleASTBaseChain is the composed chain used by inner
// re-parse paths. Built once per call (Compose itself is cheap) so
// the dyn-DDL visitor doesn't reach into the package-level
// RewriteOracleAST var that itself contains the dyn-DDL visitor.
func rewriteOracleASTBaseChain() ast.Rewriter {
	return ast.Compose(rewriteOracleASTBase()...)
}

// applyOracleASTRewrites runs the orchestrator on every PLStmt in
// stmts and returns the (possibly substituted) slice. Used as the
// Phase 3.6 hook from TranslateRoutineBodyExt.
func applyOracleASTRewrites(stmts []ast.PLStmt) []ast.PLStmt {
	return applyOracleASTRewritesWith(stmts, nil)
}

// applyOracleASTRewritesWith is the caller-extensible variant: extra
// rewriters compose AFTER RewriteOracleAST so callers (e.g. the
// trigger translator) can layer context-specific substitutions on
// top of the standard pipeline. Pass extra=nil for the default path.
//
// The trigger row-alias rewriter (MakeTriggerAliasComposite) is the
// canonical caller — it walks Idents under the trigger body and
// renames REFERENCING aliases (NR → NEW, OR1 → OLD) at the AST level
// so the writer's isPLpgSQLPseudoCol guard emits `NEW.col` unquoted
// rather than `"NEW"."col"` (a case-sensitive PG ident reference).
func applyOracleASTRewritesWith(stmts []ast.PLStmt, extra ast.Rewriter) []ast.PLStmt {
	if len(stmts) == 0 {
		return stmts
	}
	rewriter := RewriteOracleAST
	if extra != nil {
		rewriter = ast.Compose(RewriteOracleAST, extra)
	}
	out := make([]ast.PLStmt, len(stmts))
	for i, s := range stmts {
		if rs, ok := ast.Rewrite(s, rewriter).(ast.PLStmt); ok {
			out[i] = rs
		} else {
			out[i] = s
		}
	}
	return out
}
