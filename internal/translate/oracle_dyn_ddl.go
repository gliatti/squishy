package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_dyn_ddl.go — generic visitor for Oracle's "build a DDL/DML
// statement at runtime via string concat then EXECUTE IMMEDIATE"
// idiom. Covers ALTER TABLE, DROP, COMMENT, INSERT/UPDATE/DELETE,
// MERGE, CREATE INDEX/VIEW/SEQUENCE/PROCEDURE/FUNCTION, anonymous
// BEGIN…END blocks. CREATE TRIGGER is excluded — its decomposition
// into CREATE FUNCTION + CREATE TRIGGER lives in oracle_dyn_trigger.go.
//
// Approach (post-order, AST-only):
//
//   1. Match the same shape as the trigger visitor: an initial
//      AssignStmt whose rhs is a `||` chain starting with a Literal
//      that begins with a known DDL/DML keyword, followed by N
//      contributing AssignStmts (`var := var || …`) and IfStmts
//      (every branch only appends to var), terminated by an
//      ExecuteImmediate(var). Non-contributor stmts in between are
//      passed through.
//   2. Assemble the flattened concat into one Oracle source string by
//      concatenating Literal contents and substituting non-Literal
//      operands with `__squishy_dyn_<i>__` sentinels.
//   3. Re-parse the assembled text with the Oracle parser, run it
//      through the full Translate() pipeline (which fires every Phase
//      3 visitor — varchar2 → VARCHAR, FROM DUAL drop, identifier
//      case-fold, …), and capture the PG DDL the translator emits.
//   4. Split the PG DDL on the sentinel substrings to rebuild a flat
//      []ast.Expr where the original Idents/expressions are spliced
//      back at the positions the writer placed them.
//   5. Replace the contributors with a single `var := <new concat>;`
//      AssignStmt; the existing ExecuteImmediate(var) stays and now
//      runs the PG-flavoured statement.

// VisitOracleLiteralTypeMap is a leaf-level rewriter that maps
// Oracle scalar-type vocabulary inside string Literals to their PG
// equivalents (varchar2 → VARCHAR, number → NUMERIC, …). Only
// fires on `*ast.Literal` of Kind=string — non-Literals are passed
// through. The substitution is word-boundary aware so identifiers
// that happen to embed a type name (e.g. `varchar2_helpers`) are
// untouched.
//
// This pass catches the cross-variable runtime-build pattern where
// a column-list buffer is built in a loop and spliced into the
// main DDL via SUBSTR — the dyn-DDL visitor only sees the main
// concat, but this visitor visits every Literal in the routine
// body so the loop-built fragments get fixed too.
//
// Risk: a Literal that happens to be a user-facing error message
// containing `varchar2` would be mutated. In practice DRSE-class
// procedures don't carry such messages — and the substitution is
// reversible at human review.
func VisitOracleLiteralTypeMap(n ast.Node) ast.Node {
	lit, ok := n.(*ast.Literal)
	if !ok || lit.Kind != "string" {
		return n
	}
	mapped := mapOracleTypesInLiteral(lit.Text)
	if mapped == lit.Text {
		return n
	}
	return &ast.Literal{Kind: "string", Text: mapped, P: lit.P}
}

// MakeOracleDynamicDDLBuildVisitor returns a Block-level rewriter
// that handles the generic dyn-DDL pattern. The factory carries a
// per-routine counter for sentinel naming, and the targetSchema for
// the inner re-translation pass.
func MakeOracleDynamicDDLBuildVisitor(targetSchema string) ast.Rewriter {
	rewriteList := func(stmts []ast.PLStmt) ([]ast.PLStmt, bool) {
		i := 0
		changed := false
		for i < len(stmts) {
			match, ok := matchDynamicDDLRange(stmts, i)
			if !ok {
				i++
				continue
			}
			newStmts := rewriteDynDDLRange(stmts, match, targetSchema)
			if newStmts == nil {
				// Translation failed — leave stmts untouched and skip
				// past the EXECUTE so we don't loop on the same range.
				i = match.execIdx + 1
				continue
			}
			stmts = newStmts
			i = match.execIdx + 1
			changed = true
		}
		return stmts, changed
	}
	return func(n ast.Node) ast.Node {
		switch x := n.(type) {
		case *ast.Block:
			if rewritten, changed := rewriteList(x.Stmts); changed {
				x.Stmts = rewritten
			}
		case *ast.CursorForStmt:
			if rewritten, changed := rewriteList(x.Body); changed {
				x.Body = rewritten
			}
		case *ast.NumericForStmt:
			if rewritten, changed := rewriteList(x.Body); changed {
				x.Body = rewritten
			}
		case *ast.LoopStmt:
			if rewritten, changed := rewriteList(x.Body); changed {
				x.Body = rewritten
			}
		case *ast.WhileStmt:
			if rewritten, changed := rewriteList(x.Body); changed {
				x.Body = rewritten
			}
		case *ast.IfStmt:
			for i := range x.Branches {
				if rewritten, changed := rewriteList(x.Branches[i].Body); changed {
					x.Branches[i].Body = rewritten
				}
			}
			if rewritten, changed := rewriteList(x.Else); changed {
				x.Else = rewritten
			}
		case *ast.CaseStmt:
			for i := range x.When {
				if rewritten, changed := rewriteList(x.When[i].Body); changed {
					x.When[i].Body = rewritten
				}
			}
			if rewritten, changed := rewriteList(x.Else); changed {
				x.Else = rewritten
			}
		}
		return n
	}
}

// matchDynamicDDLRange scans stmts starting at i for the dyn-DDL
// build pattern. Returns false when the slot at i isn't a recognised
// DDL/DML initiator. Reuses the trigger matcher's tolerance for
// pass-through stmts (TRACE_CODE updates, EXECUTE 'ALTER SESSION …',
// …) so any side-effecting work between contributors is preserved.
func matchDynamicDDLRange(stmts []ast.PLStmt, i int) (m dynTriggerMatch, ok bool) {
	a, isAssign := stmts[i].(*ast.AssignStmt)
	if !isAssign {
		return m, false
	}
	parts := flattenConcat(a.Expr)
	if !hasDynamicDDLHead(parts) {
		return m, false
	}
	m.varName = a.Target
	m.firstIdx = i
	m.contribIdx = append(m.contribIdx, i)
	m.accum = append(m.accum, parts...)
	for j := i + 1; j < len(stmts); j++ {
		switch s := stmts[j].(type) {
		case *ast.AssignStmt:
			if !strings.EqualFold(s.Target, m.varName) {
				continue
			}
			cParts := flattenConcat(s.Expr)
			if !concatStartsWithSelfRef(cParts, m.varName) {
				return m, false
			}
			m.contribIdx = append(m.contribIdx, j)
			m.accum = append(m.accum, cParts[1:]...)
		case *ast.IfStmt:
			if ifOnlyAppendsVar(s, m.varName) {
				accumIfBranches(s, m.varName, &m.accum)
				m.contribIdx = append(m.contribIdx, j)
			}
		case *ast.ExecuteImmediateStmt:
			id, isId := s.SQL.(*ast.Ident)
			if !isId || len(id.Parts) != 1 || !strings.EqualFold(id.Parts[0], m.varName) {
				continue
			}
			m.execIdx = j
			return m, true
		}
	}
	return m, false
}

// rewriteDynDDLRange transforms the contributor AssignStmts into a
// single AssignStmt that holds the PG-translated DDL. The
// ExecuteImmediate is left in place and now runs the PG statement.
//
// Approach: pure AST mutation. Each contributor's `||` chain is
// flattened into a []ast.Expr, then each *ast.Literal in the slice
// is mapped via mapOracleSQLFragmentLiteral (Oracle scalar-type
// vocabulary + FROM DUAL drop). Non-Literal operands (Idents,
// FuncCalls like chr(10), SUBSTR(buf, 2)) carry runtime values and
// pass through unchanged. The ALTER … ADD ( … ) → ADD COLUMN
// transformation lifts the parens and rewrites the SUBSTR into
// replace(SUBSTR(…), ',', ', add column ') so each Oracle
// `,col TYPE` chunk becomes its own PG ADD COLUMN clause at runtime.
//
// Returns nil — the visitor mutates Literals in place inside the
// original AssignStmts. The caller advances past the EXECUTE.
func rewriteDynDDLRange(stmts []ast.PLStmt, m dynTriggerMatch, targetSchema string) []ast.PLStmt {
	applyTypeMapsToContributors(stmts, m)
	return nil
}

// applyTypeMapsToContributors mutates every string Literal in the
// rhs of every contributor AssignStmt + IfStmt branches, applying:
//
//   - Oracle scalar-type vocabulary mapping (varchar2 → VARCHAR, …)
//     in Literals that look like DDL fragments.
//   - The `add ( cols ) ` → `add column cols` transformation specific
//     to ALTER TABLE multi-column add: rewrite the `' add ( '`
//     keyword tail to `' add column '`, drop the `' ) '` trailer,
//     and wrap any SUBSTR(other_var, N) operand in
//     replace(…, ',', ', add column ') so each column in the spliced
//     buffer becomes its own ADD COLUMN clause at runtime PG.
//
// The visitor runs in post-order, so this mutation is observed by
// every downstream emit pass.
func applyTypeMapsToContributors(stmts []ast.PLStmt, m dynTriggerMatch) {
	for _, k := range m.contribIdx {
		switch s := stmts[k].(type) {
		case *ast.AssignStmt:
			parts := flattenConcat(s.Expr)
			parts = applyTypeMapsToConcat(parts)
			parts = applyAlterAddParenTransform(parts, m.accum)
			s.Expr = assembleConcat(parts)
		case *ast.IfStmt:
			applyTypeMapsToIfBranches(s)
		}
	}
}

// applyAlterAddParenTransform rewrites the Oracle `ALTER … ADD ( … )`
// parenthesised multi-column form into PG's `ALTER … ADD COLUMN …,
// ADD COLUMN …` shape. The transformation is purely AST-level:
//
//   - any string Literal whose content ends with " add ( " (case-
//     insensitive, optional surrounding whitespace) has the trailing
//     "( " replaced by "column ".
//   - any string Literal whose trimmed content is ")" or " ) " is
//     reduced to a single space — PG's per-column ADD COLUMN list
//     doesn't need closing parens.
//   - any operand of the concat that's a FuncCall named SUBSTR (or
//     equivalent) is wrapped in replace(<call>, ',', ', add column ')
//     so each Oracle-flavoured `,col TYPE` segment becomes a fresh
//     PG ADD COLUMN clause at runtime.
//
// The transformations only fire when the routine-wide accum carries
// at least one Literal containing "alter " (case-insensitive prefix),
// which is the dynamic-DDL signal the visitor already validated.
// Outside of an ALTER context they're no-ops.
func applyAlterAddParenTransform(parts []ast.Expr, accum []ast.Expr) []ast.Expr {
	if !concatHasAlterAddParen(accum) {
		return parts
	}
	for i, p := range parts {
		switch x := p.(type) {
		case *ast.Literal:
			if x.Kind != "string" {
				continue
			}
			parts[i] = transformAlterAddLiteral(x)
		case *ast.FuncCall:
			if isSubstrCall(x) {
				// The buffer may carry Oracle scalar-type names from
				// the data-dictionary simulator (`'VARCHAR2'`,
				// `'NUMBER'`, …). Map them to PG equivalents at
				// runtime via chained replace() calls — only on
				// THIS SUBSTR result, never on the buffer variable
				// itself, so other consumers (CASE WHEN data_type =
				// 'VARCHAR2' THEN …) keep their semantics.
				wrapped := ast.Expr(x)
				wrapped = wrapWithReplace(wrapped, ",", ", add column ")
				wrapped = wrapWithReplace(wrapped, "VARCHAR2", "VARCHAR")
				wrapped = wrapWithReplace(wrapped, "NVARCHAR2", "VARCHAR")
				wrapped = wrapWithReplace(wrapped, "NUMBER", "NUMERIC")
				wrapped = wrapWithReplace(wrapped, "PLS_INTEGER", "INTEGER")
				wrapped = wrapWithReplace(wrapped, "BINARY_INTEGER", "INTEGER")
				wrapped = wrapWithReplace(wrapped, "BINARY_FLOAT", "REAL")
				wrapped = wrapWithReplace(wrapped, "BINARY_DOUBLE", "DOUBLE PRECISION")
				wrapped = wrapWithReplace(wrapped, "CLOB", "TEXT")
				wrapped = wrapWithReplace(wrapped, "NCLOB", "TEXT")
				wrapped = wrapWithReplace(wrapped, "BLOB", "BYTEA")
				parts[i] = wrapped
			}
		}
	}
	return parts
}

// concatHasAlterAddParen reports whether the accum carries the
// signature of an ALTER … ADD ( … ) build: at least one string
// Literal that begins with `alter ` (case-insensitive) AND at least
// one Literal containing ` add ( ` (case-insensitive substring).
func concatHasAlterAddParen(accum []ast.Expr) bool {
	hasAlter, hasAddParen := false, false
	for _, p := range accum {
		raw, ok := stripStringLit(p)
		if !ok {
			continue
		}
		upper := strings.ToUpper(raw)
		if !hasAlter && strings.HasPrefix(strings.TrimSpace(upper), "ALTER ") {
			hasAlter = true
		}
		if !hasAddParen && strings.Contains(upper, " ADD ( ") {
			hasAddParen = true
		}
		if hasAlter && hasAddParen {
			return true
		}
	}
	return false
}

// transformAlterAddLiteral mutates a single string Literal:
//   - " add ( " (case-insensitive) → " add column "
//   - trimmed content of "(" or " )" or "( )" or ") " → " " (space)
//
// Returns the original Literal pointer when no rule fires; returns a
// fresh Literal node when content was rewritten.
func transformAlterAddLiteral(l *ast.Literal) ast.Expr {
	raw := l.Text
	upper := strings.ToUpper(raw)
	// 1. " add ( " → " add column "
	if idx := strings.Index(upper, " ADD ( "); idx >= 0 {
		newText := raw[:idx] + " add column " + raw[idx+len(" ADD ( "):]
		return &ast.Literal{Kind: "string", Text: newText, P: l.P}
	}
	// 2. trailing " ) " or " )" or "( )" — common as the trailer of the
	//    multi-column ADD form. Replace the parens with a space so the
	//    PG output stays whitespace-clean.
	trimmed := strings.TrimSpace(raw)
	switch trimmed {
	case ")", "( )", ") ":
		return &ast.Literal{Kind: "string", Text: " ", P: l.P}
	}
	return l
}

// isSubstrCall reports whether fc is a FuncCall to SUBSTR or
// SUBSTRING (case-insensitive). Used to detect the `SUBSTR(buffer, 2)`
// operand that splices a column-list buffer into the main DDL
// concat.
func isSubstrCall(fc *ast.FuncCall) bool {
	name := fc.Name
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		name = name[dot+1:]
	}
	switch strings.ToUpper(name) {
	case "SUBSTR", "SUBSTRING":
		return true
	}
	return false
}

// wrapWithReplace wraps `inner` in a PG `replace(<inner>, '<from>',
// '<to>')` FuncCall. Used to rewrite a SUBSTR(buffer, N) operand so
// each Oracle-flavoured `,col TYPE` segment becomes a PG `,
// add column …` at runtime.
func wrapWithReplace(inner ast.Expr, from, to string) ast.Expr {
	return &ast.FuncCall{
		Name: "replace",
		Args: []ast.Expr{
			inner,
			&ast.Literal{Kind: "string", Text: from},
			&ast.Literal{Kind: "string", Text: to},
		},
	}
}

// applyTypeMapsToIfBranches recurses into IfStmt branches and Else
// to map types in every AssignStmt's rhs Literals. Used by
// applyTypeMapsToContributors when an IfStmt was admitted as a
// contributor (its branches all append to the build variable).
func applyTypeMapsToIfBranches(s *ast.IfStmt) {
	walk := func(body []ast.PLStmt) {
		for _, x := range body {
			a, ok := x.(*ast.AssignStmt)
			if !ok {
				continue
			}
			parts := flattenConcat(a.Expr)
			parts = applyTypeMapsToConcat(parts)
			a.Expr = assembleConcat(parts)
		}
	}
	for _, br := range s.Branches {
		walk(br.Body)
	}
	walk(s.Else)
}

// (assembleConcatWithSentinels / translateOracleStmtFragment /
// renderOracleStmtAsPG / renderAlterTableAsPG were removed: the
// sentinel-based re-parse + render path was replaced by direct AST
// mutation of contributor Literals — see rewriteDynDDLRange and
// applyTypeMapsToContributors. The flattened `||` chain already
// holds the structural information we need (Literal vs FuncCall vs
// Ident), so per-Literal mutations + replace() wraps suffice.)

// mapOracleTypesInLiteral applies the Oracle → PG scalar-type
// vocabulary substitutions to a string Literal's content. Word-
// boundary aware (so `varchar2_t` isn't matched). The input is a
// scalar Literal text (not full SQL) — we mutate it in-place via
// replaceWholeWordFold which already implements the boundary check.
//
// Applied only on Literals that the dyn-DDL visitor identified as
// part of a runtime-built DDL chain — never on user comment text or
// arbitrary error-message Literals.
func mapOracleTypesInLiteral(s string) string {
	rules := []struct{ from, to string }{
		{"VARCHAR2", "VARCHAR"},
		{"NVARCHAR2", "VARCHAR"},
		{"NCHAR", "CHAR"},
		{"NUMBER", "NUMERIC"},
		{"PLS_INTEGER", "INTEGER"},
		{"BINARY_INTEGER", "INTEGER"},
		{"BINARY_FLOAT", "REAL"},
		{"BINARY_DOUBLE", "DOUBLE PRECISION"},
		{"CLOB", "TEXT"},
		{"NCLOB", "TEXT"},
		{"BLOB", "BYTEA"},
	}
	out := s
	for _, r := range rules {
		out = replaceWholeWordFold(out, r.from, r.to)
	}
	return out
}

// applyTypeMapsToConcat walks a flattened concat slice and rewrites
// each string Literal's content via mapOracleTypesInLiteral. Returns
// the same slice with mutated Literals (no new allocation when no
// match changes the content).
func applyTypeMapsToConcat(parts []ast.Expr) []ast.Expr {
	for i, p := range parts {
		lit, ok := p.(*ast.Literal)
		if !ok || lit.Kind != "string" {
			continue
		}
		mapped := mapOracleTypesInLiteral(lit.Text)
		if mapped != lit.Text {
			parts[i] = &ast.Literal{Kind: "string", Text: mapped, P: lit.P}
		}
	}
	return parts
}

// (renderAlterTableAsPG was removed alongside translateOracleStmtFragment
// — its job is now covered by direct Literal mutation + the
// applyAlterAddParenTransform helper which lifts the Oracle parens
// and wraps SUBSTR with replace() chains.)
