package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_dyn_trigger.go — visitor for Oracle's "build a CREATE TRIGGER
// at runtime via string concat then EXECUTE IMMEDIATE" idiom, common
// in maintenance-style DRSE-class procedures.
//
// Source shape (typical):
//
//   L_VC_STMT := 'CREATE OR REPLACE trigger ' || pfx || 'MVT_' || tab
//             || chr(10) || 'BEFORE INSERT ON ' || pfx || 'MVT_' || tab2
//             || chr(10) || 'FOR EACH ROW' || chr(10)
//             || 'DECLARE ' || chr(10) || 'vl_x varchar2(3);'
//             || chr(10) || 'BEGIN ' || ver || chr(10);
//   IF cond THEN L_VC_STMT := L_VC_STMT || '...'; END IF;
//   L_VC_STMT := L_VC_STMT || 'END;';
//   EXECUTE IMMEDIATE L_VC_STMT;
//
// PG cannot parse a CREATE TRIGGER with an inline DECLARE / BEGIN / END
// body — it requires a separate CREATE FUNCTION RETURNS trigger that the
// trigger references via EXECUTE FUNCTION.
//
// This visitor walks an *ast.Block, finds the contiguous chain of
// AssignStmts that build a single string variable starting with
// `CREATE OR REPLACE TRIGGER` (or `CREATE TRIGGER`) and ending with the
// matching `EXECUTE IMMEDIATE <var>` statement, splits the constructed
// string at the structural boundaries (DECLARE / BEGIN / END;) — using
// the AST's typed Literals, never substring-matching live SQL — and
// substitutes the entire run with two assemble-and-EXECUTE sequences:
// one for the function and one for the trigger.

// (flattenConcat lives in oracle_visitors_dynddl.go — re-used here.)

// assembleConcat builds a left-associated chain of `||` BinaryExprs
// from a flat slice. Empty / single-element slices return the operand
// directly (or a synthetic empty string Literal for the empty case).
func assembleConcat(parts []ast.Expr) ast.Expr {
	if len(parts) == 0 {
		return &ast.Literal{Kind: "string", Text: "''"}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out = &ast.BinaryExpr{Op: "||", Lhs: out, Rhs: parts[i]}
	}
	return out
}

// stripStringLit returns the raw content of a string Literal — the
// parser stores the unescaped body in Literal.Text (the lexer
// already collapsed `''` → `'` and dropped the surrounding quotes).
// Returns ("", false) for non-string literals.
func stripStringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.Literal)
	if !ok || lit.Kind != "string" {
		return "", false
	}
	return lit.Text, true
}

// quoteStringLit builds a Literal node that stores `raw` verbatim in
// Text. The Postgres writer (sqlString) is responsible for wrapping
// it in quotes and doubling embedded `'` at emit time, so we MUST
// pass the unquoted body here — wrapping ourselves would result in
// double-quoting (`'''text'''`) once the writer adds its own pair.
func quoteStringLit(raw string) *ast.Literal {
	return &ast.Literal{Kind: "string", Text: raw}
}

// hasCreateTriggerHead reports whether the first string Literal of the
// flattened concat chain (after trimming leading whitespace and the
// optional `CREATE OR REPLACE` prefix) starts the `TRIGGER` keyword.
// Returns the residual content of that Literal after the `CREATE [OR
// REPLACE] TRIGGER ` prefix has been consumed (typed AST consumption,
// not regex on SQL — we only inspect a single Literal's content text).
func hasCreateTriggerHead(parts []ast.Expr) (rest string, ok bool) {
	if len(parts) == 0 {
		return "", false
	}
	raw, ok := stripStringLit(parts[0])
	if !ok {
		return "", false
	}
	upper := strings.ToUpper(strings.TrimLeft(raw, " \t\r\n"))
	const orRepl = "CREATE OR REPLACE TRIGGER "
	const plain = "CREATE TRIGGER "
	switch {
	case strings.HasPrefix(upper, orRepl):
		// Preserve the original casing of the residual text — only the
		// length-of-prefix bytes were matched.
		idx := strings.Index(strings.ToUpper(raw), "TRIGGER ")
		return raw[idx+len("TRIGGER "):], true
	case strings.HasPrefix(upper, plain):
		idx := strings.Index(strings.ToUpper(raw), "TRIGGER ")
		return raw[idx+len("TRIGGER "):], true
	}
	return "", false
}

// dynDDLLeadingKeywords is the set of leading SQL keywords that mark
// the start of a runtime-built DDL/DML payload eligible for the
// generic dyn-DDL visitor. Match is case-insensitive on the trimmed
// content of a single Literal — never crosses node boundaries.
var dynDDLLeadingKeywords = []string{
	"ALTER ",
	"DROP ",
	"COMMENT ",
	"GRANT ",
	"REVOKE ",
	"TRUNCATE ",
	"CREATE INDEX ",
	"CREATE UNIQUE INDEX ",
	"CREATE BITMAP INDEX ",
	"CREATE OR REPLACE VIEW ",
	"CREATE VIEW ",
	"CREATE OR REPLACE FORCE VIEW ",
	"CREATE FORCE VIEW ",
	"CREATE TABLE ",
	"CREATE GLOBAL TEMPORARY TABLE ",
	"CREATE OR REPLACE SYNONYM ",
	"CREATE SYNONYM ",
	"CREATE PUBLIC SYNONYM ",
	"CREATE SEQUENCE ",
	"CREATE OR REPLACE PROCEDURE ",
	"CREATE PROCEDURE ",
	"CREATE OR REPLACE FUNCTION ",
	"CREATE FUNCTION ",
	"INSERT INTO ",
	"UPDATE ",
	"DELETE FROM ",
	"MERGE INTO ",
	"BEGIN ", // anonymous PL/SQL block
}

// hasDynamicDDLHead reports whether the first string Literal of the
// concat chain begins with any of the dynDDLLeadingKeywords (case-
// insensitive, after trimming whitespace). Used by the generic
// visitor to decide whether to engage. CREATE TRIGGER is intentionally
// NOT in the keyword set — it has its own specialised rewriter.
func hasDynamicDDLHead(parts []ast.Expr) bool {
	if len(parts) == 0 {
		return false
	}
	raw, ok := stripStringLit(parts[0])
	if !ok {
		return false
	}
	upper := strings.ToUpper(strings.TrimLeft(raw, " \t\r\n"))
	// Skip CREATE TRIGGER — handled by the specialised visitor.
	if strings.HasPrefix(upper, "CREATE OR REPLACE TRIGGER ") || strings.HasPrefix(upper, "CREATE TRIGGER ") {
		return false
	}
	for _, kw := range dynDDLLeadingKeywords {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}
	return false
}

// splitConcatAt slices the flattened concat into [before-marker,
// after-marker]. The marker is a case-insensitive substring search
// inside string Literals only; non-Literal operands are passed through
// untouched into whichever side they fall on. The Literal that
// contains the marker is split into two Literal nodes — one trimmed to
// the prefix, one to the suffix — so no substring-matching of live SQL
// happens beyond a single Literal's static content.
//
// Returns (before, after, true) when the marker is found, and (nil,
// nil, false) when it isn't. When the marker spans the boundary
// between two Literals (e.g. `'BE' || 'GIN'`) it is NOT detected — the
// caller is responsible for emitting the marker as a single Literal in
// the source AST. In practice the procedures we target always do.
func splitConcatAt(parts []ast.Expr, marker string) (before, after []ast.Expr, ok bool) {
	upMarker := strings.ToUpper(marker)
	for i, p := range parts {
		raw, isStr := stripStringLit(p)
		if !isStr {
			continue
		}
		upRaw := strings.ToUpper(raw)
		idx := strings.Index(upRaw, upMarker)
		if idx < 0 {
			continue
		}
		before = append([]ast.Expr{}, parts[:i]...)
		if idx > 0 {
			before = append(before, quoteStringLit(raw[:idx]))
		}
		afterLit := raw[idx+len(marker):]
		if afterLit != "" {
			after = append(after, quoteStringLit(afterLit))
		}
		after = append(after, parts[i+1:]...)
		return before, after, true
	}
	return nil, nil, false
}

// reparseAndTranslateBodyParts mutates each string Literal in the
// flattened concat slice to map Oracle scalar types → PG, drop
// `FROM DUAL`, etc. AST-only: every operand is inspected via the
// existing typed nodes — `*ast.Literal` for the Oracle SQL fragment
// text, non-Literals (Idents, FuncCalls like chr(10)) pass through
// unchanged because they hold runtime values, not literal SQL text.
//
// Word-level rewrites use replaceWholeWordFold so identifiers that
// happen to embed a type-name suffix (`varchar2_helpers`) are left
// alone. The FROM DUAL drop is targeted at a single token sequence
// (` FROM DUAL`) inside one Literal — never across nodes.
//
// Each mutation operates on a SINGLE Literal text — that's the
// tolerable string-op-on-an-isolated-scalar pattern CLAUDE.md
// allows for identifier folding. Crucially, the visitor only
// reaches Literals it positively identified as part of a dyn-DDL
// build chain, so user-facing message Literals stay untouched.
func reparseAndTranslateBodyParts(parts []ast.Expr) []ast.Expr {
	for i, p := range parts {
		lit, ok := p.(*ast.Literal)
		if !ok || lit.Kind != "string" {
			continue
		}
		mapped := mapOracleSQLFragmentLiteral(lit.Text)
		if mapped != lit.Text {
			parts[i] = &ast.Literal{Kind: "string", Text: mapped, P: lit.P}
		}
	}
	return parts
}

// mapOracleSQLFragmentLiteral applies the Oracle → PG token
// substitutions to a single string Literal's text. Combines:
//
//   - mapOracleTypesInLiteral (varchar2 → VARCHAR, …)
//   - dropFromDual (`SELECT … INTO … FROM DUAL` → `SELECT … INTO …`)
//   - mapTriggerPseudoVarsInLiteral (Oracle's INSERTING / UPDATING /
//     DELETING trigger pseudo-vars → PG's TG_OP = '<EVENT>' guard)
//   - mapBareReturnInLiteral (Oracle's `RETURN ;` early-exit → PG's
//     `RETURN NULL ;` for trigger functions)
//
// Returns the input unchanged when no rule matches.
func mapOracleSQLFragmentLiteral(s string) string {
	out := mapOracleTypesInLiteral(s)
	out = dropFromDualInLiteral(out)
	out = mapTriggerPseudoVarsInLiteral(out)
	out = mapBareReturnInLiteral(out)
	return out
}

// mapBareReturnInLiteral rewrites Oracle's bare `RETURN ;` (early-exit
// from a trigger body) into PG's `RETURN NULL ;`. PG's plpgsql trigger
// functions reject `RETURN ;` (it expects an expression); Oracle accepts
// it as "exit the trigger immediately". We map to NULL because AFTER
// triggers ignore the return value and BEFORE triggers treat NULL as
// "skip the row" — which is the intent of an early return in the
// no-op trigger generated when FLG_TRG_MVT='0'.
//
// Word-boundary aware on the keyword (so `RETURNING` stays untouched);
// the trailing `;` may have any whitespace between `RETURN` and `;`.
func mapBareReturnInLiteral(s string) string {
	upper := strings.ToUpper(s)
	out := []byte(s)
	written := []byte{}
	cursor := 0
	for {
		idx := strings.Index(upper[cursor:], "RETURN")
		if idx < 0 {
			written = append(written, out[cursor:]...)
			break
		}
		idx += cursor
		// word-boundary check on the LEFT
		if idx > 0 {
			prev := out[idx-1]
			if isIdentRune(rune(prev)) {
				written = append(written, out[cursor:idx+len("RETURN")]...)
				cursor = idx + len("RETURN")
				continue
			}
		}
		// scan past whitespace after RETURN to find the next non-blank char
		end := idx + len("RETURN")
		j := end
		for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\r' || out[j] == '\n') {
			j++
		}
		if j >= len(out) || out[j] != ';' {
			// not a bare RETURN ;, leave alone
			written = append(written, out[cursor:end]...)
			cursor = end
			continue
		}
		// match: replace the RETURN<ws>; with `RETURN NULL ;`
		written = append(written, out[cursor:idx]...)
		written = append(written, []byte("RETURN NULL ;")...)
		cursor = j + 1
	}
	return string(written)
}

// (isIdentRune lives in plsql_xlate.go — re-used here.)

// mapTriggerPseudoVarsInLiteral replaces Oracle's INSERTING /
// UPDATING / DELETING trigger pseudo-vars with the PG-equivalent
// TG_OP guard. Word-boundary aware so an identifier `inserting_xxx`
// stays untouched.
//
// Example: `IF INSERTING THEN …` → `IF (TG_OP = 'INSERT') THEN …`.
func mapTriggerPseudoVarsInLiteral(s string) string {
	rules := []struct{ from, to string }{
		{"INSERTING", "(TG_OP = 'INSERT')"},
		{"UPDATING", "(TG_OP = 'UPDATE')"},
		{"DELETING", "(TG_OP = 'DELETE')"},
	}
	out := s
	for _, r := range rules {
		out = replaceWholeWordFold(out, r.from, r.to)
	}
	return out
}

// dropFromDualInLiteral removes any case-insensitive ` FROM DUAL`
// occurrence inside a single Literal's content. PG's SELECT INTO
// doesn't need a FROM clause for constant projections — orafce ships
// a `dual` view but most DRSE-class code uses FROM DUAL purely as
// Oracle syntactic noise.
func dropFromDualInLiteral(s string) string {
	upper := strings.ToUpper(s)
	const marker = " FROM DUAL"
	for {
		idx := strings.Index(upper, marker)
		if idx < 0 {
			return s
		}
		s = s[:idx] + s[idx+len(marker):]
		upper = strings.ToUpper(s)
	}
}

// (splitOnSentinels was removed: the sentinel-based approach is
// gone. We mutate Literals directly in the flattened concat slice
// — no string assembly, no parser round-trip.)

// dialectsKindOracle is the Oracle Kind constant, resolved via the
// dialects package import.
const dialectsKindOracle = dialects.KindOracle

// chr10Literal recognises the common Oracle idiom `chr(10)` (line
// terminator) — purely syntactic check at AST level so the visitor
// can either drop or keep newlines without textual heuristics.
func chr10Literal(e ast.Expr) bool {
	fc, ok := e.(*ast.FuncCall)
	if !ok || !strings.EqualFold(fc.Name, "chr") || len(fc.Args) != 1 {
		return false
	}
	lit, ok := fc.Args[0].(*ast.Literal)
	if !ok || lit.Kind != "number" {
		return false
	}
	return lit.Text == "10"
}

// isAssignTo reports whether s is `<target> := <expr>;` with target
// matching name (case-insensitive). The target field is stored
// verbatim (often UPPERCASE for unquoted Oracle names) so we
// fold-compare.
func isAssignTo(s ast.PLStmt, name string) (*ast.AssignStmt, bool) {
	a, ok := s.(*ast.AssignStmt)
	if !ok {
		return nil, false
	}
	if !strings.EqualFold(a.Target, name) {
		return nil, false
	}
	return a, true
}

// concatStartsWithSelfRef reports whether the first operand of the
// flattened concat is an Ident referencing `name` (the variable being
// appended to). Pattern detection for `var := var || …` continuation
// assignments.
func concatStartsWithSelfRef(parts []ast.Expr, name string) bool {
	if len(parts) == 0 {
		return false
	}
	id, ok := parts[0].(*ast.Ident)
	if !ok || len(id.Parts) != 1 {
		return false
	}
	return strings.EqualFold(id.Parts[0], name)
}

// dynTriggerNameFromHeader extracts a deterministic-yet-unique name
// suffix for the synthesised trigger function. We can't know the
// runtime trigger name (it's built from variables), so we derive a
// name from the header expression's structural fingerprint: a stable
// hash of the Literal segments. The function is generated in the same
// schema as the calling routine; collisions across triggers in the
// same routine are avoided by including a per-call counter.
//
// Returns ("squishy_dyn_trg_<hash>_<counter>", counter+1).
func dynTriggerNameFromHeader(parts []ast.Expr, counter int) (string, int) {
	var h strings.Builder
	for _, p := range parts {
		if lit, ok := p.(*ast.Literal); ok && lit.Kind == "string" {
			h.WriteString(lit.Text)
			h.WriteByte('|')
		}
	}
	// Cheap FNV-1a 32-bit hash.
	var hash uint32 = 2166136261
	for _, b := range []byte(h.String()) {
		hash ^= uint32(b)
		hash *= 16777619
	}
	return fmt.Sprintf("squishy_dyn_trg_%08x_%d", hash, counter), counter + 1
}

// MakeOracleDynamicTriggerBuildVisitor returns a rewriter that
// detects and rewrites the runtime-built-trigger idiom in any node
// that carries a `[]ast.PLStmt` slice — Block, CursorForStmt,
// NumericForStmt, LoopStmt, IfStmt branches and Else, CaseStmt
// branches and Else, and WhileStmt. The factory carries a per-
// translator counter used to mint unique synthetic function names
// across multiple trigger-build sites in the same routine.
func MakeOracleDynamicTriggerBuildVisitor(targetSchema string, counter *int) ast.Rewriter {
	rewriteList := func(stmts []ast.PLStmt) ([]ast.PLStmt, bool) {
		i := 0
		changed := false
		for i < len(stmts) {
			match, ok := matchDynamicTriggerRange(stmts, i)
			if !ok {
				i++
				continue
			}
			fnName, _ := dynTriggerNameFromHeader(match.accum, *counter)
			*counter++
			stmts = rewriteDynTriggerRange(stmts, match, targetSchema, fnName)
			i = match.execIdx + 3
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

// dynTriggerMatch records what the matcher found inside a Block.Stmts.
// `firstIdx` is the AssignStmt that initiates `var := 'CREATE [OR
// REPLACE] TRIGGER ' || …`. `execIdx` is the matching `EXECUTE
// IMMEDIATE var`. `contribIdx` enumerates every Stmt index between
// the two that touches `var` (including the initiator, the EXECUTE is
// not in the list). `accum` is the flattened concat of all
// contributing rhs slots in source order, used for boundary detection.
// `headerRest` carries the content of the first Literal AFTER the
// `CREATE [OR REPLACE] TRIGGER ` prefix has been consumed (so callers
// can re-prefix without scanning the keyword twice).
type dynTriggerMatch struct {
	varName    string
	firstIdx   int
	execIdx    int
	contribIdx []int   // indices in stmts that contribute to var
	accum      []ast.Expr
	headerRest string
}

// matchDynamicTriggerRange scans stmts starting at i for the dyn-
// trigger build pattern. The matcher tolerates intervening stmts that
// don't touch the target variable (e.g. `L_TRACE_CODE := …`,
// `EXECUTE 'ALTER SESSION …'`, free-standing IFs that don't append
// to var). Only stmts that DO touch var are tracked as contributors;
// the run terminates at the first `EXECUTE var` that doesn't match a
// preceding contributor.
func matchDynamicTriggerRange(stmts []ast.PLStmt, i int) (m dynTriggerMatch, ok bool) {
	// First slot must be the trigger initiator.
	a, isAssign := stmts[i].(*ast.AssignStmt)
	if !isAssign {
		return m, false
	}
	parts := flattenConcat(a.Expr)
	rest, isTriggerHead := hasCreateTriggerHead(parts)
	if !isTriggerHead {
		return m, false
	}
	m.varName = a.Target
	m.firstIdx = i
	m.headerRest = rest
	m.contribIdx = append(m.contribIdx, i)
	m.accum = append(m.accum, parts...)
	for j := i + 1; j < len(stmts); j++ {
		switch s := stmts[j].(type) {
		case *ast.AssignStmt:
			if !strings.EqualFold(s.Target, m.varName) {
				continue // unrelated assignment — pass through
			}
			cParts := flattenConcat(s.Expr)
			if !concatStartsWithSelfRef(cParts, m.varName) {
				// var := <something not starting with var> — abandon
				// the match: the variable is being overwritten, not
				// appended to.
				return m, false
			}
			m.contribIdx = append(m.contribIdx, j)
			m.accum = append(m.accum, cParts[1:]...)
		case *ast.IfStmt:
			// IfStmt is a contributor when every branch only appends
			// to var. Most DRSE-style routines mix var-appends with
			// other targets (L_BO_CLAUSE := …) inside the same body,
			// so we relax: collect every AssignStmt that DOES append
			// to var (selfRef-shaped) into m.accum, even when the
			// branch holds other unrelated stmts. The runtime PG
			// branching survives untouched (we don't touch the
			// IfStmt structure here); we only mine the appends so
			// boundary detection (DECLARE/BEGIN/END) sees the full
			// picture.
			if ifOnlyAppendsVar(s, m.varName) {
				accumIfBranches(s, m.varName, &m.accum)
				m.contribIdx = append(m.contribIdx, j)
				continue
			}
			if ifContainsSink(s, m.varName) {
				m.execIdx = j
				return m, true
			}
			// Mine partial contributions: scan every branch for
			// AssignStmts that append to var, append their flattened
			// rhs (sans the leading selfRef) to m.accum. This is a
			// READ-only pass — the IfStmt remains pass-through in
			// the rewriter, but boundary detection now sees the
			// `'BEGIN '` / `'END;'` Literals that live inside the
			// branches.
			minePartialBranchAppends(s, m.varName, &m.accum)
			// pass-through stays — the IfStmt isn't a contributor.
		case *ast.ExecuteImmediateStmt:
			id, isId := s.SQL.(*ast.Ident)
			if !isId || len(id.Parts) != 1 || !strings.EqualFold(id.Parts[0], m.varName) {
				continue // EXECUTE on a different var — pass-through
			}
			m.execIdx = j
			return m, true
		case *ast.CallStmt:
			// Oracle's `DBMS_SQL.PARSE(cur, stmt)` is the long-form
			// equivalent of EXECUTE IMMEDIATE — Oracle code uses
			// it for trigger payloads larger than 32KB. Treat any
			// CALL to dbms_sql.parse whose 2nd arg is the build
			// variable as a sink for the dyn-build pattern.
			if !isDbmsSqlParseOf(s, m.varName) {
				continue
			}
			m.execIdx = j
			return m, true
		}
	}
	return m, false
}

// patchIfBranchesSink walks every branch + Else of `s` and rewrites
// each direct sink stmt (EXECUTE IMMEDIATE var OR CALL dbms_sql.
// parse(_, var)) into a fresh ExecuteImmediate(var) on the
// lowercase-folded ident. The IfStmt structure is preserved so the
// runtime branching survives — only the sink contents inside each
// branch are mutated.
func patchIfBranchesSink(s *ast.IfStmt, varName string) {
	folded := normalizeOracleIdent(varName)
	patch := func(body []ast.PLStmt) []ast.PLStmt {
		out := make([]ast.PLStmt, 0, len(body))
		for _, x := range body {
			if isSinkOf(x, varName) {
				out = append(out, &ast.ExecuteImmediateStmt{
					SQL: &ast.Ident{Parts: []string{folded}},
				})
				continue
			}
			out = append(out, x)
		}
		return out
	}
	for i := range s.Branches {
		s.Branches[i].Body = patch(s.Branches[i].Body)
	}
	s.Else = patch(s.Else)
}

// ifContainsSink reports whether any branch (or Else) of `s` holds
// a sink stmt for varName — either EXECUTE IMMEDIATE var or CALL
// dbms_sql.parse(_, var). The walk is non-recursive: we only look at
// direct children of each branch, not at nested IfStmts. The DRSE
// pattern fits this: `IF length > max THEN parse(_, array) ELSE
// parse(_, scalar) END IF` has the sinks at branch top level.
func ifContainsSink(s *ast.IfStmt, varName string) bool {
	scan := func(body []ast.PLStmt) bool {
		for _, x := range body {
			if isSinkOf(x, varName) {
				return true
			}
		}
		return false
	}
	for _, br := range s.Branches {
		if scan(br.Body) {
			return true
		}
	}
	return scan(s.Else)
}

// isSinkOf returns true for either EXECUTE IMMEDIATE var or CALL
// dbms_sql.parse(_, var). Single source of truth for the
// "what counts as a sink" question used by the matcher and the
// rewriter alike.
func isSinkOf(s ast.PLStmt, varName string) bool {
	switch x := s.(type) {
	case *ast.ExecuteImmediateStmt:
		id, ok := x.SQL.(*ast.Ident)
		return ok && len(id.Parts) == 1 && strings.EqualFold(id.Parts[0], varName)
	case *ast.CallStmt:
		return isDbmsSqlParseOf(x, varName)
	}
	return false
}

// dbmsSqlParseCursorOf inspects the matched sink stmt and returns the
// cursor variable name when the sink is a `CALL dbms_sql.parse(cur,
// var)` (or an IfStmt whose branches each carry such a call). Returns
// "" for direct ExecuteImmediate sinks (no cursor involved). Used by
// the rewriter to inject a follow-up no-op parse so the surrounding
// `dbms_sql.execute`/`close_cursor` calls find a parseable cursor.
func dbmsSqlParseCursorOf(s ast.PLStmt, varName string) string {
	scan := func(c *ast.CallStmt) string {
		if !strings.EqualFold(c.Schema, "dbms_sql") || !strings.EqualFold(c.Name, "parse") {
			return ""
		}
		if len(c.Args) < 1 {
			return ""
		}
		switch a := c.Args[0].(type) {
		case *ast.Ident:
			if len(a.Parts) == 1 {
				return a.Parts[0]
			}
		case *ast.RawExpr:
			t := strings.TrimSpace(a.Text)
			t = strings.TrimRight(t, ",")
			t = strings.TrimSpace(t)
			if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
				t = t[1 : len(t)-1]
			}
			return t
		}
		return ""
	}
	switch x := s.(type) {
	case *ast.CallStmt:
		return scan(x)
	case *ast.IfStmt:
		for _, br := range x.Branches {
			for _, bs := range br.Body {
				if c, ok := bs.(*ast.CallStmt); ok {
					if name := scan(c); name != "" {
						return name
					}
				}
			}
		}
		for _, bs := range x.Else {
			if c, ok := bs.(*ast.CallStmt); ok {
				if name := scan(c); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// isDbmsSqlParseOf reports whether `s` is `CALL dbms_sql.parse(cur,
// varName, …)` (case-insensitive on every part). Used by
// matchDynamicTriggerRange to recognise the DBMS_SQL alternative to
// EXECUTE IMMEDIATE. Accepts both shapes the Oracle parser might
// produce for the statement arg:
//   - `*ast.Ident{Parts: [varName]}` — the typed form for a simple
//     bare ident reference.
//   - `*ast.RawExpr{Text: "varName"}` — the fall-back form when the
//     parser keeps the args as raw text (common for procedure calls
//     with multi-arg signatures).
func isDbmsSqlParseOf(s *ast.CallStmt, varName string) bool {
	if !strings.EqualFold(s.Schema, "dbms_sql") || !strings.EqualFold(s.Name, "parse") {
		return false
	}
	if len(s.Args) < 2 {
		return false
	}
	switch a := s.Args[1].(type) {
	case *ast.Ident:
		return len(a.Parts) == 1 && strings.EqualFold(a.Parts[0], varName)
	case *ast.RawExpr:
		// Trim surrounding whitespace and any trailing comma the
		// parser left in the raw text.
		t := strings.TrimSpace(a.Text)
		t = strings.TrimRight(t, ",")
		t = strings.TrimSpace(t)
		// Strip optional surrounding double quotes if the parser
		// preserved them.
		if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
			t = t[1 : len(t)-1]
		}
		return strings.EqualFold(t, varName)
	}
	return false
}

// ifOnlyAppendsVar reports whether every stmt in every branch+else of
// the IfStmt is a `var := var || …` append.
func ifOnlyAppendsVar(s *ast.IfStmt, varName string) bool {
	all := func(body []ast.PLStmt) bool {
		for _, x := range body {
			a, ok := x.(*ast.AssignStmt)
			if !ok || !strings.EqualFold(a.Target, varName) {
				return false
			}
			if !concatStartsWithSelfRef(flattenConcat(a.Expr), varName) {
				return false
			}
		}
		return true
	}
	for _, br := range s.Branches {
		if !all(br.Body) {
			return false
		}
	}
	return all(s.Else)
}

// stripVarAppendsFromIfBranches walks every branch of `s` and
// removes every AssignStmt whose target is `varName` and whose rhs
// is a self-append (`var := var || …`). Recurses into nested
// IfStmt / CaseStmt / loop bodies so deeply nested `var := var || …`
// assignments are removed too. Non-AssignStmts stay; assignments
// to other targets (L_BO_CLAUSE := …) stay.
//
// The mirror of minePartialBranchAppends: that function COLLECTED
// the nested appends into m.accum so the matcher could find body
// boundary keywords; this function REMOVES them at rewrite time so
// the runtime-built variable doesn't get clobbered by the legacy
// concat path after the visitor's initiator AssignStmt set it to
// the PG-shaped CREATE FUNCTION DDL.
func stripVarAppendsFromIfBranches(s *ast.IfStmt, varName string) {
	var stripList func(body []ast.PLStmt) []ast.PLStmt
	stripList = func(body []ast.PLStmt) []ast.PLStmt {
		out := make([]ast.PLStmt, 0, len(body))
		for _, x := range body {
			switch t := x.(type) {
			case *ast.AssignStmt:
				if strings.EqualFold(t.Target, varName) &&
					concatStartsWithSelfRef(flattenConcat(t.Expr), varName) {
					continue // drop
				}
				out = append(out, x)
			case *ast.IfStmt:
				for i := range t.Branches {
					t.Branches[i].Body = stripList(t.Branches[i].Body)
				}
				t.Else = stripList(t.Else)
				out = append(out, t)
			case *ast.CaseStmt:
				for i := range t.When {
					t.When[i].Body = stripList(t.When[i].Body)
				}
				t.Else = stripList(t.Else)
				out = append(out, t)
			case *ast.LoopStmt:
				t.Body = stripList(t.Body)
				out = append(out, t)
			case *ast.WhileStmt:
				t.Body = stripList(t.Body)
				out = append(out, t)
			case *ast.NumericForStmt:
				t.Body = stripList(t.Body)
				out = append(out, t)
			case *ast.CursorForStmt:
				t.Body = stripList(t.Body)
				out = append(out, t)
			default:
				out = append(out, x)
			}
		}
		return out
	}
	for i := range s.Branches {
		s.Branches[i].Body = stripList(s.Branches[i].Body)
	}
	s.Else = stripList(s.Else)
}

// minePartialBranchAppends walks every branch of `s` and appends
// the flattened rhs (sans the leading selfRef Ident) of every
// AssignStmt that targets varName with a self-append shape. Other
// stmts (assignments to OTHER targets, ExecuteImmediate, IfStmts,
// etc.) are skipped — we ONLY mine var-append fragments to feed
// the boundary detector. Recurses into nested IfStmts so deeply
// nested `IF … THEN var := var || 'BEGIN '` patterns surface too.
func minePartialBranchAppends(s *ast.IfStmt, varName string, accum *[]ast.Expr) {
	var walk func(body []ast.PLStmt)
	walk = func(body []ast.PLStmt) {
		for _, x := range body {
			switch t := x.(type) {
			case *ast.AssignStmt:
				if !strings.EqualFold(t.Target, varName) {
					continue
				}
				parts := flattenConcat(t.Expr)
				if !concatStartsWithSelfRef(parts, varName) {
					continue
				}
				*accum = append(*accum, parts[1:]...)
			case *ast.IfStmt:
				for _, br := range t.Branches {
					walk(br.Body)
				}
				walk(t.Else)
			case *ast.CaseStmt:
				for _, w := range t.When {
					walk(w.Body)
				}
				walk(t.Else)
			case *ast.LoopStmt:
				walk(t.Body)
			case *ast.WhileStmt:
				walk(t.Body)
			case *ast.NumericForStmt:
				walk(t.Body)
			case *ast.CursorForStmt:
				walk(t.Body)
			}
		}
	}
	for _, br := range s.Branches {
		walk(br.Body)
	}
	walk(s.Else)
}

// accumIfBranches walks an IfStmt that has been validated by
// ifOnlyAppendsVar and appends every branch's contribution to *accum.
// Branches contribute in source order — runtime PG semantics still
// pick exactly one branch, but for boundary detection we treat them
// as a sequential merge so DECLARE/BEGIN/END found inside any branch
// are visible to the splitter.
func accumIfBranches(s *ast.IfStmt, varName string, accum *[]ast.Expr) {
	walk := func(body []ast.PLStmt) {
		for _, x := range body {
			a := x.(*ast.AssignStmt)
			parts := flattenConcat(a.Expr)
			*accum = append(*accum, parts[1:]...)
		}
	}
	for _, br := range s.Branches {
		walk(br.Body)
	}
	walk(s.Else)
}

// rewriteDynTriggerRange transforms the AssignStmts that contribute
// to `var` inside [match.firstIdx, match.execIdx]:
//
//   - The initiator's `'CREATE OR REPLACE trigger '` Literal is
//     replaced with `'CREATE OR REPLACE FUNCTION <fn>() RETURNS
//     trigger LANGUAGE plpgsql AS $f$ '`.
//   - Any contributor whose flattened rhs spans the `'DECLARE'` or
//     `'BEGIN'` markers retains them — they stay valid inside the
//     PL/pgSQL function body PG accepts.
//   - The contributor whose flattened rhs ends with `'END;'` has it
//     replaced by `' RETURN NEW; END $f$;'`.
//
// Then the EXECUTE at execIdx is left in place (it now executes the
// CREATE FUNCTION DDL) and two new stmts are appended right after it:
//
//   - AssignStmt: `var := 'CREATE TRIGGER ' || <header parts before
//     DECLARE/BEGIN> || ' EXECUTE FUNCTION <fn>();'`
//   - ExecuteImmediate(var)
//
// Non-contributor stmts in [firstIdx, execIdx] are left untouched at
// their original positions so any side-effecting work between
// builds-up steps continues to run.
func rewriteDynTriggerRange(stmts []ast.PLStmt, m dynTriggerMatch, targetSchema, fnName string) []ast.PLStmt {
	// Branch-preserving design (was: merge-all-branches):
	//
	// The Oracle source builds CREATE TRIGGER incrementally via
	//   <var> := 'CREATE OR REPLACE TRIGGER ' || …;   -- header init
	//   <var> := <var> || …;                          -- header continuation
	//   <var> := <var> || 'BEGIN ' || …;              -- BOUNDARY: body start
	//   IF cond THEN <var> := <var> || …; END IF;     -- conditional body chunks
	//   <var> := <var> || 'END;';                     -- body trailer
	//   EXECUTE IMMEDIATE <var>;                      -- sink (or DBMS_SQL.PARSE)
	//
	// The earlier merge-all-branches strategy fed every contributor's rhs
	// into a single accumulator and emitted ONE CREATE FUNCTION DDL whose
	// body was the static concatenation of every branch — which produced
	// invalid PG when mutually-exclusive branches both contributed (e.g.
	// the FLG_TRG_MVT='0' branch's `return ;` early-exit landing alongside
	// the FLG_TRG_MVT='1' INSERT logic).
	//
	// New strategy: keep the runtime IFs intact, split the variable into
	// two halves at the BEGIN/DECLARE boundary:
	//   <var>      → trigger HEADER (table name + AFTER … + ON … + FOR EACH ROW)
	//   <var>_body → trigger BODY (everything from BEGIN/DECLARE to END;)
	// Pre-boundary contributors (and IfStmts) target <var>; post-boundary
	// ones target <var>_body. The sink fires two EXECUTEs, one for the
	// CREATE FUNCTION (using <var>_body) and one for the CREATE TRIGGER
	// (using <var>). The body variable is declared in a synthetic nested
	// Block that wraps the entire dyn-trigger range.

	a0 := stmts[m.firstIdx].(*ast.AssignStmt)
	parts0 := flattenConcat(a0.Expr)
	if len(parts0) == 0 {
		return stmts
	}
	// Drop the leading `'CREATE OR REPLACE TRIGGER '` literal; the
	// remainder of parts0 is the start of the trigger header (table name
	// builder, etc.). headerRest may carry residual text after the
	// keyword (typically empty).
	headerInitParts := parts0[1:]
	if m.headerRest != "" {
		headerInitParts = append([]ast.Expr{quoteStringLit(m.headerRest)}, headerInitParts...)
	}

	// Locate the boundary: the first top-level CONTRIBUTOR whose rhs
	// includes a `'BEGIN '` or `'DECLARE'` literal. Falls back to the
	// initiator if it already carries the marker (rare — DRSE-style
	// code emits BEGIN in a separate self-append).
	boundaryStmtIdx, boundaryKw := findBoundaryContributor(stmts, m)
	if boundaryStmtIdx < 0 {
		// No boundary found — refuse to rewrite (we'd produce malformed
		// CREATE FUNCTION with no body).
		return stmts
	}

	// Body variable name: lowercase + `_body` suffix so the DECLARE
	// emitted by writeDeclLine (unquoted name) and the runtime
	// references (writeIdent always quotes, so they need to match the
	// post-fold form) agree on the same identifier.
	bodyVar := strings.ToLower(m.varName) + "_body"
	bodyVarFolded := bodyVar

	// Build the body of the synthetic wrapper Block. We walk the original
	// stmts in [firstIdx, execIdx] and retarget contributors based on
	// their position relative to the boundary.
	wrapperBody := make([]ast.PLStmt, 0, m.execIdx-m.firstIdx+4)
	for k := m.firstIdx; k <= m.execIdx; k++ {
		s := stmts[k]
		switch k {
		case m.firstIdx:
			// Initiator: <var> := <header init parts> (CREATE TRIGGER
			// prefix dropped, just the table-name builder remains).
			translated := reparseAndTranslateBodyParts(headerInitParts)
			if k == boundaryStmtIdx {
				// Boundary lives inside the initiator's literals
				// (one-shot build with both header and body in a
				// single rhs). Split at the boundary keyword: pre
				// goes into <var>, post initialises <var>_body.
				hdrParts, bodyParts := splitContributorAtBoundary(translated, boundaryKw)
				bodyParts = stripTrailingEndMarker(bodyParts)
				if len(hdrParts) > 0 {
					wrapperBody = append(wrapperBody, &ast.AssignStmt{Target: m.varName, Expr: assembleConcat(hdrParts), P: a0.P})
				}
				if len(bodyParts) > 0 {
					wrapperBody = append(wrapperBody, &ast.AssignStmt{Target: bodyVar, Expr: assembleConcat(bodyParts), P: a0.P})
				}
				continue
			}
			rhs := assembleConcat(translated)
			if rhs == nil {
				rhs = quoteStringLit("")
			}
			wrapperBody = append(wrapperBody, &ast.AssignStmt{Target: m.varName, Expr: rhs, P: a0.P})

		case m.execIdx:
			// Sink: emit two EXECUTEs (CREATE FUNCTION + CREATE
			// TRIGGER). When the original sink was a
			// `dbms_sql.parse(cur, var)` call, the surrounding code
			// typically follows up with `dbms_sql.execute(cur)` and
			// `dbms_sql.close_cursor(cur)` — those calls would error
			// (orafce raises "cannot to prepare plan") if we left the
			// cursor opened-but-not-parsed. Detect the cursor variable
			// and inject a no-op `dbms_sql.parse(cur, 'SELECT 1')` so
			// the post-range execute/close still find a parseable
			// statement on the cursor. Direct EXECUTE IMMEDIATE sinks
			// don't involve cursors so the no-op parse is skipped.
			fnHead := fmt.Sprintf("CREATE OR REPLACE FUNCTION %q.%q() RETURNS trigger LANGUAGE plpgsql AS $f$ ", targetSchema, fnName)
			fnTail := " RETURN NEW; END $f$;"
			fnExec := assembleConcat([]ast.Expr{
				quoteStringLit(fnHead),
				&ast.Ident{Parts: []string{bodyVarFolded}},
				quoteStringLit(fnTail),
			})
			trgExec := assembleConcat([]ast.Expr{
				quoteStringLit("CREATE TRIGGER "),
				&ast.Ident{Parts: []string{normalizeOracleIdent(m.varName)}},
				quoteStringLit(fmt.Sprintf(" EXECUTE FUNCTION %q.%q();", targetSchema, fnName)),
			})
			wrapperBody = append(wrapperBody, &ast.ExecuteImmediateStmt{SQL: fnExec})
			wrapperBody = append(wrapperBody, &ast.ExecuteImmediateStmt{SQL: trgExec})
			if cursorIdent := dbmsSqlParseCursorOf(s, m.varName); cursorIdent != "" {
				// Inject `CALL dbms_sql.parse(<cursor>, 'SELECT 1')`
				// so the surrounding cursor-execute/close finds a
				// parsed (no-op) statement.
				wrapperBody = append(wrapperBody, &ast.CallStmt{
					Schema: "dbms_sql",
					Name:   "parse",
					Args: []ast.Expr{
						&ast.Ident{Parts: []string{normalizeOracleIdent(cursorIdent)}},
						quoteStringLit("SELECT 1"),
					},
				})
			}

		default:
			if isContrib(k, m.contribIdx) {
				// IfStmt contributors (the ones ifOnlyAppendsVar
				// promoted to contribIdx — every branch is a pure
				// `<var> := <var> || …` append) need their nested
				// appends retargeted just like a regular
				// non-contributor IfStmt would. The position-vs-
				// boundary logic still applies.
				if ifs, ok := s.(*ast.IfStmt); ok {
					toVar := m.varName
					if k > boundaryStmtIdx {
						toVar = bodyVar
					}
					retargetVarAppendsInIfBranches(ifs, m.varName, toVar)
					wrapperBody = append(wrapperBody, ifs)
					continue
				}
				a, ok := s.(*ast.AssignStmt)
				if !ok {
					wrapperBody = append(wrapperBody, s)
					continue
				}
				parts := flattenConcat(a.Expr)
				parts = reparseAndTranslateBodyParts(parts)
				switch {
				case k == boundaryStmtIdx:
					// Boundary contributor: split at the BEGIN/DECLARE
					// literal. Pre-marker parts continue to target <var>;
					// post-marker parts initialise <var>_body.
					hdrParts, bodyParts := splitContributorAtBoundary(parts, boundaryKw)
					if hasMeaningfulHeaderTail(hdrParts, m.varName) {
						wrapperBody = append(wrapperBody, &ast.AssignStmt{Target: m.varName, Expr: assembleConcat(hdrParts), P: a.P})
					}
					if len(bodyParts) > 0 {
						wrapperBody = append(wrapperBody, &ast.AssignStmt{Target: bodyVar, Expr: assembleConcat(bodyParts), P: a.P})
					}
				case k < boundaryStmtIdx:
					// Pre-boundary contributor: keep `<var> := <var> || …`
					wrapperBody = append(wrapperBody, &ast.AssignStmt{Target: m.varName, Expr: assembleConcat(parts), P: a.P})
				default:
					// Post-boundary contributor: retarget to <var>_body
					if len(parts) > 0 {
						if id, ok := parts[0].(*ast.Ident); ok && len(id.Parts) == 1 && strings.EqualFold(id.Parts[0], m.varName) {
							parts[0] = &ast.Ident{Parts: []string{bodyVarFolded}}
						}
					}
					// Strip trailing `END;` from the LAST body
					// contributor — the wrapper appends ` END $f$;` at
					// emit time so we mustn't ship a duplicate END.
					parts = stripTrailingEndMarker(parts)
					if hasMeaningfulHeaderTail(parts, bodyVar) {
						wrapperBody = append(wrapperBody, &ast.AssignStmt{Target: bodyVar, Expr: assembleConcat(parts), P: a.P})
					}
				}
				continue
			}
			// Non-contributor stmt in range. If it's an IfStmt, retarget
			// nested var-appends based on the IfStmt's position relative
			// to the boundary.
			if ifs, ok := s.(*ast.IfStmt); ok {
				toVar := m.varName
				if k > boundaryStmtIdx {
					toVar = bodyVar
				}
				retargetVarAppendsInIfBranches(ifs, m.varName, toVar)
				wrapperBody = append(wrapperBody, ifs)
				continue
			}
			// Other non-contributor (TRACE_CODE updates, EXECUTE 'ALTER
			// SESSION …', cursor open/close, …): keep in place.
			wrapperBody = append(wrapperBody, s)
		}
	}

	// Emit the wrapper: nested DECLARE … BEGIN … END; with bodyVar
	// declared as TEXT.
	wrapper := &ast.Block{
		Decls: []ast.PLDecl{
			&ast.DeclareVar{
				Name: bodyVar,
				Type: &ast.UserDefinedType{Name: "text"},
			},
		},
		Stmts: wrapperBody,
		P:     a0.P,
	}

	out := make([]ast.PLStmt, 0, len(stmts)-(m.execIdx-m.firstIdx)+1)
	out = append(out, stmts[:m.firstIdx]...)
	out = append(out, wrapper)
	out = append(out, stmts[m.execIdx+1:]...)
	return out
}

// findBoundaryContributor scans m.contribIdx and returns the index of
// the FIRST top-level contributor whose flattened rhs contains a
// `'BEGIN '` or `'DECLARE'` literal — the structural boundary between
// the trigger header and the trigger body. Returns (-1, "") when no
// contributor carries the marker.
func findBoundaryContributor(stmts []ast.PLStmt, m dynTriggerMatch) (int, string) {
	for _, idx := range m.contribIdx {
		if idx == m.firstIdx {
			// initiator usually doesn't carry BEGIN — skip
			a, ok := stmts[idx].(*ast.AssignStmt)
			if !ok {
				continue
			}
			parts := flattenConcat(a.Expr)
			if kw := scanBoundaryKeyword(parts); kw != "" {
				return idx, kw
			}
			continue
		}
		a, ok := stmts[idx].(*ast.AssignStmt)
		if !ok {
			continue
		}
		parts := flattenConcat(a.Expr)
		if kw := scanBoundaryKeyword(parts); kw != "" {
			return idx, kw
		}
	}
	return -1, ""
}

// scanBoundaryKeyword inspects a flattened concat slice for a literal
// containing `'DECLARE'` or `'BEGIN'`. Returns the keyword found or ""
// when neither is present. DECLARE wins over BEGIN when both appear in
// the same slice (it precedes BEGIN in Oracle PL/SQL trigger syntax).
func scanBoundaryKeyword(parts []ast.Expr) string {
	for _, p := range parts {
		raw, ok := stripStringLit(p)
		if !ok {
			continue
		}
		up := strings.ToUpper(raw)
		if strings.Contains(up, "DECLARE") {
			return "DECLARE"
		}
		if strings.Contains(up, "BEGIN") {
			return "BEGIN"
		}
	}
	return ""
}

// splitContributorAtBoundary splits a flattened contributor rhs at the
// boundary keyword literal. Returns (headerParts, bodyParts) where
// headerParts is everything BEFORE the keyword and bodyParts is the
// keyword literal followed by everything after. The boundary keyword is
// preserved (re-prepended to bodyParts) so the body init starts with
// `'BEGIN '` or `'DECLARE'`.
func splitContributorAtBoundary(parts []ast.Expr, kw string) (header, body []ast.Expr) {
	before, after, ok := splitConcatAt(parts, kw)
	if !ok {
		return parts, nil
	}
	body = append(body, quoteStringLit(kw+" "))
	body = append(body, after...)
	return before, body
}

// hasMeaningfulHeaderTail reports whether `parts` carries any operand
// beyond a bare self-ref to `varName` — used to skip emitting a no-op
// `<var> := <var>;` assignment when the boundary split leaves only the
// self-ref behind (e.g. `<var> || l || 'BEGIN '` split at BEGIN leaves
// header=[<var>, l]; body=['BEGIN ', l_v_version] — header is kept; if
// the contributor were `<var> || 'BEGIN '` only, header=[<var>] alone
// is a no-op and we drop it).
func hasMeaningfulHeaderTail(parts []ast.Expr, varName string) bool {
	if len(parts) == 0 {
		return false
	}
	if len(parts) == 1 {
		// Single operand: meaningful only if it isn't a bare self-ref.
		if id, ok := parts[0].(*ast.Ident); ok && len(id.Parts) == 1 && strings.EqualFold(id.Parts[0], varName) {
			return false
		}
		return true
	}
	return true
}

// retargetVarAppendsInIfBranches walks every branch of `s` and
// rewrites every AssignStmt of shape `<fromVar> := <fromVar> || …` to
// target `<toVar>` instead. The first operand (the self-ref Ident) is
// also retargeted. Other stmts (assignments to OTHER targets,
// ExecuteImmediate, …) pass through unchanged. Recurses into nested
// IfStmt / CaseStmt / loop bodies so deeply nested var-appends surface.
//
// When fromVar == toVar this is a literal-translation pass: every
// self-append's rhs is fed through reparseAndTranslateBodyParts so
// Oracle SQL fragments inside Literals (varchar2, FROM DUAL,
// INSERTING, RETURN ;, …) get mapped to PG equivalents. The trailing
// `END;` literal in the LAST self-append is also stripped (so the
// wrapper's ` END $f$;` doesn't collide with a body-internal END).
func retargetVarAppendsInIfBranches(s *ast.IfStmt, fromVar, toVar string) {
	folded := normalizeOracleIdent(toVar)
	var walk func(body []ast.PLStmt) []ast.PLStmt
	walk = func(body []ast.PLStmt) []ast.PLStmt {
		out := make([]ast.PLStmt, 0, len(body))
		for _, x := range body {
			switch t := x.(type) {
			case *ast.AssignStmt:
				if !strings.EqualFold(t.Target, fromVar) {
					out = append(out, x)
					continue
				}
				parts := flattenConcat(t.Expr)
				if !concatStartsWithSelfRef(parts, fromVar) {
					out = append(out, x)
					continue
				}
				parts[0] = &ast.Ident{Parts: []string{folded}}
				parts = reparseAndTranslateBodyParts(parts)
				parts = stripTrailingEndMarker(parts)
				if !hasMeaningfulHeaderTail(parts, toVar) {
					continue
				}
				out = append(out, &ast.AssignStmt{Target: toVar, Expr: assembleConcat(parts), P: t.P})
			case *ast.IfStmt:
				for i := range t.Branches {
					t.Branches[i].Body = walk(t.Branches[i].Body)
				}
				t.Else = walk(t.Else)
				out = append(out, t)
			case *ast.CaseStmt:
				for i := range t.When {
					t.When[i].Body = walk(t.When[i].Body)
				}
				t.Else = walk(t.Else)
				out = append(out, t)
			case *ast.LoopStmt:
				t.Body = walk(t.Body)
				out = append(out, t)
			case *ast.WhileStmt:
				t.Body = walk(t.Body)
				out = append(out, t)
			case *ast.NumericForStmt:
				t.Body = walk(t.Body)
				out = append(out, t)
			case *ast.CursorForStmt:
				t.Body = walk(t.Body)
				out = append(out, t)
			default:
				out = append(out, x)
			}
		}
		return out
	}
	for i := range s.Branches {
		s.Branches[i].Body = walk(s.Branches[i].Body)
	}
	s.Else = walk(s.Else)
}

// triggerHeaderParts returns the slice of accum that runs from the
// start (just after the synthesised `headerRest` slot) up to (but not
// including) the first Literal containing `'DECLARE'` or `'BEGIN'`.
// The caller has already replaced the first Literal with `headerRest`
// in m.accum, so this slices on the marker keyword.
func triggerHeaderParts(accum []ast.Expr) []ast.Expr {
	before, _, ok := splitConcatAt(accum, "DECLARE")
	if !ok {
		before, _, ok = splitConcatAt(accum, "BEGIN")
		if !ok {
			return nil
		}
	}
	return trimTrailingWhitespaceOps(before)
}

// rewriteEndMarker locates the contributor that ends the trigger
// body with `'END;'` and replaces that suffix with
// `' RETURN NEW; END $f$;'`. Returns (newStmt, true) when the marker
// was found and patched; (nil, false) when this contributor doesn't
// hold the END; marker.
func rewriteEndMarker(s ast.PLStmt, varName string) (ast.PLStmt, bool) {
	a, ok := s.(*ast.AssignStmt)
	if !ok || !strings.EqualFold(a.Target, varName) {
		return nil, false
	}
	parts := flattenConcat(a.Expr)
	patched, ok := rewriteEndMarkerInParts(parts)
	if !ok {
		return nil, false
	}
	return &ast.AssignStmt{Target: a.Target, Expr: assembleConcat(patched), P: a.P}, true
}

// stripTrailingEndMarker scans the parts slice from the tail looking
// for a string Literal that ends with `END;` (case-insensitive,
// optional surrounding whitespace). Trims that marker out — the
// caller will append a fresh `RETURN NEW; END $f$;` trailer.
//
// If the END; spans multiple Literals (e.g. `'EN' || 'D;'`) we leave
// it alone — the structure on real DRSE-class code always emits the
// marker as a single Literal.
func stripTrailingEndMarker(parts []ast.Expr) []ast.Expr {
	for i := len(parts) - 1; i >= 0; i-- {
		lit, ok := parts[i].(*ast.Literal)
		if !ok || lit.Kind != "string" {
			continue
		}
		raw := lit.Text
		upper := strings.ToUpper(raw)
		// Look for the last `END;` occurrence (with or without
		// trailing whitespace).
		idx := strings.LastIndex(upper, "END;")
		if idx < 0 {
			continue
		}
		// Trim the END; and anything after it (typically nothing
		// or just whitespace).
		newText := strings.TrimRight(raw[:idx], " \t\r\n")
		out := append([]ast.Expr{}, parts[:i]...)
		if newText != "" {
			out = append(out, &ast.Literal{Kind: "string", Text: newText, P: lit.P})
		}
		out = append(out, parts[i+1:]...)
		return out
	}
	return parts
}

// rewriteEndMarkerInParts is the parts-level helper used by both the
// stmt-level rewriteEndMarker and the initiator-reconstruction path.
// Returns the new parts slice plus a flag indicating whether the
// `'END;'` marker was found and patched. The original slice is left
// untouched.
func rewriteEndMarkerInParts(parts []ast.Expr) ([]ast.Expr, bool) {
	for i := len(parts) - 1; i >= 0; i-- {
		raw, isStr := stripStringLit(parts[i])
		if !isStr {
			continue
		}
		idx := strings.Index(strings.ToUpper(raw), "END;")
		if idx < 0 {
			continue
		}
		newLit := raw[:idx] + " RETURN NEW; END $f$;"
		out := append([]ast.Expr{}, parts[:i]...)
		out = append(out, quoteStringLit(newLit))
		out = append(out, parts[i+1:]...)
		return out, true
	}
	return parts, false
}

func isContrib(idx int, list []int) bool {
	for _, x := range list {
		if x == idx {
			return true
		}
	}
	return false
}

// (buildPGTriggerSequence has been replaced by rewriteDynTriggerRange,
// which keeps non-contributor stmts in place and only rewrites the
// AssignStmts that touch the trigger-builder variable.)

// trimTrailingWhitespaceOps drops trailing chr(10) FuncCalls and
// pure-whitespace string Literals from a flattened concat slice. AST-
// level — no scan of meaningful SQL content.
func trimTrailingWhitespaceOps(parts []ast.Expr) []ast.Expr {
	for len(parts) > 0 {
		last := parts[len(parts)-1]
		if chr10Literal(last) {
			parts = parts[:len(parts)-1]
			continue
		}
		if lit, ok := last.(*ast.Literal); ok && lit.Kind == "string" {
			raw, _ := stripStringLit(last)
			if strings.TrimSpace(raw) == "" {
				parts = parts[:len(parts)-1]
				continue
			}
		}
		break
	}
	return parts
}

// _ keeps the oracle import alive for the upcoming body-Literal
// re-parse pass.
var _ = oracle.Parse
