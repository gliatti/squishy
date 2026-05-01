package translate

import (
	"fmt"
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects"
	mysqldialect "gitlab.com/dalibo/squishy/internal/dialects/mysql"
	oracledialect "gitlab.com/dalibo/squishy/internal/dialects/oracle"
	pgast "gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// TranslateRoutineBody parses a source-dialect routine body into the shared
// PL/SQL AST, translates each statement into the PL/pgSQL AST, and renders
// the PG body text. Falls back to the token rewriter only when the parser
// produces no usable AST (e.g. an empty body).
//
// Returns (pgBodyText, untranslated, notes). `untranslated` lists constructs
// the translator could not handle (each becomes a blocking manual-review
// prerequisite). `notes` lists best-effort translations that succeeded but
// differ from Oracle semantics in a documented way (each becomes an
// info-level note, no prerequisite). The caller wraps the body in a
// CreateFunction/Procedure/Trigger shell via dialects/postgres.
func TranslateRoutineBody(body string, kind dialects.Kind) (string, []string, []string) {
	text, untrans, notes, _ := TranslateRoutineBodyExt(body, kind)
	return text, untrans, notes
}

// TranslateRoutineBodyExt is the variant that also reports whether the
// rewrites used the adminpack extension (pg_file_write). Callers that
// build the migration's prerequisites checklist use that flag to surface
// a blocking `CREATE EXTENSION adminpack;` requirement.
func TranslateRoutineBodyExt(body string, kind dialects.Kind) (string, []string, []string, bool) {
	return TranslateRoutineBodyExtV(body, kind, "", "")
}

// TranslateRoutineBodyExtV is the variant that lets the caller pass
// the trigger's REFERENCING aliases so the AST row-alias visitor can
// rename NR/OR1 → NEW/OLD on the parsed Idents BEFORE pgast
// translation. Empty newAlias / oldAlias are treated as no-ops.
//
// Non-trigger callers (procedure/function bodies) use the V-less
// wrapper above — there's no row-alias context to thread.
func TranslateRoutineBodyExtV(body string, kind dialects.Kind, newAlias, oldAlias string) (string, []string, []string, bool) {
	return TranslateRoutineBodyExtVS(body, kind, newAlias, oldAlias, "")
}

// TranslateRoutineBodyExtVS is the variant that also propagates the
// migration's target schema so the dynamic-trigger visitor knows where
// to mint synthetic CREATE FUNCTION targets at runtime PG. Empty
// targetSchema disables the dyn-trigger pass.
func TranslateRoutineBodyExtVS(body string, kind dialects.Kind, newAlias, oldAlias, targetSchema string) (string, []string, []string, bool) {
	if strings.TrimSpace(body) == "" {
		return "", nil, nil, false
	}
	stmts, errs := parseRoutineBodyByKind(body, kind)
	if len(stmts) == 0 {
		return rewriteMySQLBody(body), []string{"PL/SQL parser returned no statements: " + errs}, nil, false
	}
	// Phase 3.6/3.8 orchestrator: run the AST rewriters BEFORE pgast
	// translation so substituted nodes (decode → oracle.decode,
	// SequenceRef → nextval/currval, MOD → mod, …) flow through the
	// regular pgast emitter. Trigger callers thread the REFERENCING
	// aliases through MakeTriggerAliasComposite so NR/OR1 → NEW/OLD
	// happens at the AST level and the writer's pseudocol guard
	// emits the unquoted PG plpgsql form. The legacy text passes
	// (further down, rewriteOraclePLpgSQLWithFlags) still run as a
	// safety net during the transition window — Phase 5 removes them.
	if dialects.IsOracle(kind) {
		var extras []ast.Rewriter
		if newAlias != "" || oldAlias != "" {
			extras = append(extras, MakeTriggerAliasComposite(newAlias, oldAlias))
		}
		if targetSchema != "" {
			counter := 0
			// CREATE TRIGGER → CREATE FUNCTION + CREATE TRIGGER
			// decomposition runs first so the generic dyn-DDL
			// visitor doesn't try to re-translate a CREATE TRIGGER
			// payload (which Translate would reject because PG
			// can't host its DECLARE/BEGIN body inline).
			extras = append(extras, MakeOracleDynamicTriggerBuildVisitor(targetSchema, &counter))
			// Generic dyn-DDL: ALTER, DROP, COMMENT, INSERT,
			// UPDATE, CREATE INDEX/VIEW/SEQUENCE, …
			extras = append(extras, MakeOracleDynamicDDLBuildVisitor(targetSchema))
		}
		var extra ast.Rewriter
		switch len(extras) {
		case 0:
		case 1:
			extra = extras[0]
		default:
			extra = ast.Compose(extras...)
		}
		stmts = applyOracleASTRewritesWith(stmts, extra)
	}
	tx := &plTranslator{kind: kind}
	pgStmts := tx.stmts(stmts)
	var block *pgast.PLBlock
	if b, ok := pgStmts[0].(*pgast.PLBlock); ok && len(pgStmts) == 1 {
		block = b
	} else {
		block = &pgast.PLBlock{Body: pgStmts}
	}
	if tx.usedBulkExceptions {
		// Phase 6.5: full FORALL SAVE EXCEPTIONS translation requires a
		// JSONB-typed accumulator visible to both the FORALL body and
		// any post-loop SQL%BULK_EXCEPTIONS access. The variable is
		// declared lowercase-quoted so PG's case-folding rules match
		// every reference in the body — the SQL%BULK_EXCEPTIONS
		// pre-translation also emits the quoted form so the Oracle
		// parser doesn't uppercase it (parser_expr's lexer uppercases
		// unquoted idents).
		decl := pgast.PLBlockDecl{Text: `"_bulk_exceptions" jsonb[] := '{}';`}
		block.Decls = append([]pgast.PLBlockDecl{decl}, block.Decls...)
	}
	text := pgast.WritePLpgSQL(block)
	usedAdminpack := false
	if dialects.IsOracle(kind) {
		text, usedAdminpack = rewriteOraclePLpgSQLWithFlags(text)
		// Translate Oracle collection methods on variables we tracked
		// as TABLE OF / VARRAY in the DECLARE pre-pass. Done after
		// rewriteOraclePLpgSQLWithFlags so we don't fight with its
		// UTL_FILE / DBMS_OUTPUT lowercasing.
		text = rewriteOracleCollections(text, tx.collectionVars)
	}
	return text, tx.untranslated, tx.notes, usedAdminpack
}

// prefixOracleDeclare wraps an Oracle routine body captured after IS/AS into
// a canonical DECLARE/BEGIN/END form when it has declarations before BEGIN
// but no DECLARE keyword. No-op for bodies that already start with DECLARE
// or BEGIN, and for pure PL/SQL block trigger bodies that begin directly
// with BEGIN.
func prefixOracleDeclare(body string) string {
	trimmed := strings.TrimLeft(body, " \t\r\n")
	if trimmed == "" {
		return body
	}
	up := strings.ToUpper(trimmed)
	// Already canonical — no wrapping needed.
	if strings.HasPrefix(up, "DECLARE") || strings.HasPrefix(up, "BEGIN") || strings.HasPrefix(up, "<<") {
		return body
	}
	// If there's no BEGIN inside the body at all, don't wrap (would only
	// confuse the parser further — let it report a real error).
	if !containsWord(up, "BEGIN") {
		return body
	}
	return "DECLARE\n" + body
}

// containsWord is a whitespace-delimited search that ignores occurrences
// inside larger identifiers (e.g. "REBEGIN" does not count as "BEGIN").
func containsWord(s, word string) bool {
	for i := 0; i <= len(s)-len(word); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		before := i == 0 || !isIdentRune(rune(s[i-1]))
		afterIdx := i + len(word)
		after := afterIdx == len(s) || !isIdentRune(rune(s[afterIdx]))
		if before && after {
			return true
		}
	}
	return false
}

func isIdentRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

// stripBindRefColons removes the leading `:` from Oracle trigger bind
// references `:NEW.x` / `:OLD.x` so the PL/SQL expression parser sees them
// as plain qualified identifiers (`NEW.x` / `OLD.x`). Quoted-identifier
// occurrences inside string literals are skipped — single-quoted text is
// preserved untouched, and a `:'literal'` (host-variable cast) form is also
// left alone to avoid double-processing.
func stripBindRefColons(body string) string {
	if !strings.Contains(body, ":") {
		return body
	}
	var b strings.Builder
	b.Grow(len(body))
	inStr := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inStr {
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					b.WriteByte(body[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		// `--` line comment: copy the rest of the line verbatim and
		// skip out of any string/colon scanning so an `'` inside a
		// French / English contraction (`l'identifiant`, `it's`) doesn't
		// flip inStr=true and leak the "string" mode across the rest of
		// the body, blocking every subsequent `:new.col` strip.
		if c == '-' && i+1 < len(body) && body[i+1] == '-' {
			j := i
			for j < len(body) && body[j] != '\n' {
				j++
			}
			b.WriteString(body[i:j])
			i = j - 1
			continue
		}
		// `/* ... */` block comment.
		if c == '/' && i+1 < len(body) && body[i+1] == '*' {
			end := strings.Index(body[i+2:], "*/")
			if end < 0 {
				b.WriteString(body[i:])
				return b.String()
			}
			j := i + 2 + end + 2
			b.WriteString(body[i:j])
			i = j - 1
			continue
		}
		if c == '\'' {
			b.WriteByte(c)
			inStr = true
			continue
		}
		if c == ':' && i+3 < len(body) {
			next := strings.ToUpper(body[i+1 : min(i+4, len(body))])
			matchLen := 0
			switch {
			case strings.HasPrefix(next, "NEW") && i+4 < len(body) && body[i+4] == '.':
				matchLen = 4
			case strings.HasPrefix(next, "OLD") && i+4 < len(body) && body[i+4] == '.':
				matchLen = 4
			}
			if matchLen > 0 {
				// Word boundary on the left so we don't strip colons inside
				// random expressions (`x:NEW` would never appear, but be safe).
				if i == 0 || !isIdentRune(rune(body[i-1])) {
					// Keep the NEW/OLD identifier and the dot, drop the ':'.
					b.WriteString(body[i+1 : i+matchLen+1])
					i += matchLen
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// rewriteOraclePLpgSQL does post-translation token-level rewrites on the
// emitted PL/pgSQL body to fix Oracle constructs the structured translator
// leaves intact. Currently:
//   - ":NEW.col" / ":OLD.col" → "NEW.col" / "OLD.col"   (Oracle bind-style
//     trigger record references; PG uses plain NEW/OLD records with no
//     leading colon, and ':' at statement level is a parse error).
//   - Trailing SYSDATE / SYSTIMESTAMP / NVL() that slipped through the
//     structural walker into RawSQL bodies are normalized via
//     rewriteOracleExpr.
func rewriteOraclePLpgSQL(body string) string {
	body = strings.ReplaceAll(body, ":NEW.", "NEW.")
	body = strings.ReplaceAll(body, ":OLD.", "OLD.")
	body = strings.ReplaceAll(body, ":new.", "NEW.")
	body = strings.ReplaceAll(body, ":old.", "OLD.")
	// stripBindRefColons (pre-parse pass) replaces `:NEW.x` with `NEW.x`,
	// which the parser then emits as `"NEW"."x"` via quoteIdent. PG's
	// trigger pseudorecords reject the quoted form (`"NEW"` is treated as
	// a case-sensitive identifier and PG knows only the unquoted record
	// names), so unquote them in the rendered text.
	body = strings.ReplaceAll(body, `"NEW".`, "NEW.")
	body = strings.ReplaceAll(body, `"OLD".`, "OLD.")
	body = strings.ReplaceAll(body, `"new".`, "NEW.")
	body = strings.ReplaceAll(body, `"old".`, "OLD.")
	// Oracle's RAISE_APPLICATION_ERROR(code, msg) → PG's RAISE EXCEPTION
	// USING ERRCODE/MESSAGE. The Oracle code is a negative integer
	// (`-20001`); strip the sign and prefix with 'P' to fit PG's 5-char
	// SQLSTATE shape (P0001..P9999 user-defined range).
	body = rewriteRaiseApplicationError(body)
	body, _ = rewriteUTLFile(body)
	return rewriteOracleExpr(body)
}

// rewriteOraclePLpgSQLWithFlags is the variant that exposes the
// adminpack-usage flag to callers that build prerequisites. The textual
// rewrites match rewriteOraclePLpgSQL exactly.
func rewriteOraclePLpgSQLWithFlags(body string) (string, bool) {
	body = strings.ReplaceAll(body, ":NEW.", "NEW.")
	body = strings.ReplaceAll(body, ":OLD.", "OLD.")
	body = strings.ReplaceAll(body, ":new.", "NEW.")
	body = strings.ReplaceAll(body, ":old.", "OLD.")
	body = strings.ReplaceAll(body, `"NEW".`, "NEW.")
	body = strings.ReplaceAll(body, `"OLD".`, "OLD.")
	body = strings.ReplaceAll(body, `"new".`, "NEW.")
	body = strings.ReplaceAll(body, `"old".`, "OLD.")
	body = rewriteRaiseApplicationError(body)
	body = rewriteConnectByLevelSplit(body)
	body, used := rewriteUTLFile(body)
	return rewriteOracleExpr(body), used
}

// rewriteConnectByLevelSplit detects the canonical Oracle "split string
// into rows" idiom:
//
//	SELECT REGEXP_SUBSTR(<arg>, '[^<sep>]+', 1, LEVEL) [BULK COLLECT] INTO <var>
//	FROM   DUAL
//	CONNECT BY REGEXP_SUBSTR(<arg>, '[^<sep>]+', 1, LEVEL) IS NOT NULL;
//
// and rewrites it to PG's idiomatic
//
//	<var> := string_to_array(<arg>, '<sep>');
//
// PG has no CONNECT BY (use WITH RECURSIVE instead), but the split-by-
// separator idiom collapses cleanly to string_to_array which returns
// the same array semantics. Detection is deliberately narrow — only the
// REGEXP_SUBSTR + LEVEL pattern is rewritten; arbitrary CONNECT BY
// hierarchical queries land on a TODO comment so the user can review.
//
// INSERT_FONCTION_WEB_RSC in DRSE used this idiom to split a
// comma-separated list parameter; PG raised `syntax error at or near
// "BY"` because CONNECT isn't a PG keyword.
func rewriteConnectByLevelSplit(body string) string {
	if !containsKeywordCI(body, "CONNECT") || !containsKeywordCI(body, "REGEXP_SUBSTR") {
		return body
	}
	var out strings.Builder
	out.Grow(len(body))
	i := 0
	for i < len(body) {
		// Find next SELECT at word boundary.
		nextSel := findKeywordCI(body, i, "SELECT")
		if nextSel < 0 {
			out.WriteString(body[i:])
			return out.String()
		}
		// Emit the bytes before this SELECT.
		out.WriteString(body[i:nextSel])
		// Find statement terminator (;) for this SELECT.
		end := findStmtTerminator(body, nextSel)
		if end >= len(body) {
			out.WriteString(body[nextSel:])
			return out.String()
		}
		stmt := body[nextSel:end]
		stmtUpper := strings.ToUpper(stmt)
		if !strings.Contains(stmtUpper, "CONNECT") || !strings.Contains(stmtUpper, "REGEXP_SUBSTR") || !strings.Contains(stmtUpper, "LEVEL") {
			out.WriteString(stmt)
			i = end
			continue
		}
		// Try to extract the input argument and separator from the FIRST
		// REGEXP_SUBSTR call: `REGEXP_SUBSTR(<arg>, '[^<sep>]+', 1, LEVEL)`.
		arg, sep, ok := extractRegexpSubstrSplitArgs(stmt)
		if !ok {
			// Pattern didn't match closely enough — leave a TODO and the
			// original statement so the user can review.
			out.WriteString("/* TODO: Oracle CONNECT BY hierarchical query — PG has no equivalent; rewrite as WITH RECURSIVE or move to application code. */\n")
			out.WriteString(stmt)
			i = end
			continue
		}
		// Try to detect the INTO target.
		target := extractBulkCollectIntoTarget(stmt)
		if target == "" {
			// No BULK COLLECT INTO — fall back to comment + leave.
			out.WriteString("/* TODO: Oracle CONNECT BY split idiom; consider replacing with string_to_array(" + arg + ", '" + sep + "'). */\n")
			out.WriteString(stmt)
			i = end
			continue
		}
		out.WriteString(target + " := string_to_array(" + arg + ", '" + sep + "')")
		i = end
	}
	return out.String()
}

// extractRegexpSubstrSplitArgs parses `REGEXP_SUBSTR(<arg>, '[^X]+', 1,
// LEVEL)` from the first match in s. Returns (arg, sep, true) where sep
// is the captured separator character (between `[^` and `]+'`). Returns
// false if the pattern doesn't match cleanly.
func extractRegexpSubstrSplitArgs(s string) (string, string, bool) {
	idx := findKeywordCI(s, 0, "REGEXP_SUBSTR")
	if idx < 0 {
		return "", "", false
	}
	openParen := skipWS(s, idx+len("REGEXP_SUBSTR"))
	if openParen >= len(s) || s[openParen] != '(' {
		return "", "", false
	}
	closeParen, ok := findMatchingParen(s, openParen)
	if !ok {
		return "", "", false
	}
	inner := s[openParen+1 : closeParen]
	args := splitTopLevelArgs(inner)
	if len(args) < 4 {
		return "", "", false
	}
	arg := strings.TrimSpace(args[0])
	pat := strings.TrimSpace(args[1])
	if !strings.HasPrefix(pat, "'[^") || !strings.HasSuffix(pat, "]+'") {
		return "", "", false
	}
	sep := pat[3 : len(pat)-3] // strip leading `'[^` and trailing `]+'`
	if sep == "" {
		return "", "", false
	}
	// Escape PG single-quotes inside the separator (rare but safe).
	sep = strings.ReplaceAll(sep, "'", "''")
	return arg, sep, true
}

// extractBulkCollectIntoTarget locates `BULK COLLECT INTO <target>` (or
// plain `INTO <target>`) in s and returns the target identifier text.
// Returns "" when no INTO clause is present or the target can't be
// extracted cleanly.
func extractBulkCollectIntoTarget(s string) string {
	idx := findKeywordCI(s, 0, "INTO")
	if idx < 0 {
		return ""
	}
	// Walk past INTO and collect the target identifier (single token,
	// possibly dotted).
	j := skipWS(s, idx+len("INTO"))
	if j >= len(s) {
		return ""
	}
	start := j
	for j < len(s) && (isIdentByte(s[j]) || s[j] == '.') {
		j++
	}
	if j == start {
		return ""
	}
	return s[start:j]
}

// translateSqlBulkExceptionsRefs rewrites every Oracle SQL%BULK_EXCEPTIONS
// reference in body to the PG _bulk_exceptions accumulator the FORALL
// SAVE EXCEPTIONS emitter populates (see plsql_xlate.ForallStmt).
// Patterns recognised (case-insensitive on identifiers and keywords):
//
//	SQL%BULK_EXCEPTIONS.COUNT
//	  → cardinality(_bulk_exceptions)
//	SQL%BULK_EXCEPTIONS(<idx-expr>).ERROR_INDEX
//	  → (_bulk_exceptions[<idx-expr>]->>'error_index')::int
//	SQL%BULK_EXCEPTIONS(<idx-expr>).ERROR_CODE
//	  → (_bulk_exceptions[<idx-expr>]->>'error_code')
//	SQL%BULK_EXCEPTIONS(<idx-expr>).ERROR_MESSAGE
//	  → (_bulk_exceptions[<idx-expr>]->>'error_message')
//
// Single-quoted string literals are skipped so a `'SQL%BULK_EXCEPTIONS'`
// literal in the source isn't accidentally rewritten. The function
// scans byte-by-byte and replaces each matched pattern in place,
// preserving surrounding text verbatim.
//
// This translation is applied AFTER the FORALL emitter has wrapped the
// loop body in BEGIN/EXCEPTION/END that pushes `jsonb_build_object(
// 'error_index', i, 'error_code', SQLSTATE, 'error_message', SQLERRM)`
// onto _bulk_exceptions. The accumulator declaration itself is added
// to the routine's outermost DECLARE in TranslateRoutineBodyExtV when
// tx.usedBulkExceptions is set.
func translateSqlBulkExceptionsRefs(body string) string {
	if body == "" {
		return body
	}
	const marker = "BULK_EXCEPTIONS"
	if !strings.Contains(strings.ToUpper(body), marker) {
		return body
	}
	var b strings.Builder
	b.Grow(len(body))
	i := 0
	for i < len(body) {
		// Honor single-quoted strings + `--` comments so a literal
		// containing the marker isn't mis-rewritten.
		if body[i] == '\'' {
			b.WriteByte(body[i])
			i++
			for i < len(body) {
				b.WriteByte(body[i])
				if body[i] == '\'' {
					i++
					if i < len(body) && body[i] == '\'' {
						b.WriteByte(body[i])
						i++
						continue
					}
					break
				}
				i++
			}
			continue
		}
		if i+1 < len(body) && body[i] == '-' && body[i+1] == '-' {
			for i < len(body) && body[i] != '\n' {
				b.WriteByte(body[i])
				i++
			}
			continue
		}
		// Match SQL%BULK_EXCEPTIONS at a word-boundary.
		if (i == 0 || !isIdentByte(body[i-1])) &&
			i+4 <= len(body) && strings.EqualFold(body[i:i+4], "SQL%") &&
			i+4+len(marker) <= len(body) &&
			strings.EqualFold(body[i+4:i+4+len(marker)], marker) {
			tail := i + 4 + len(marker)
			// SQL%BULK_EXCEPTIONS.COUNT
			if tail+len(".COUNT") <= len(body) &&
				strings.EqualFold(body[tail:tail+len(".COUNT")], ".COUNT") &&
				(tail+len(".COUNT") == len(body) || !isIdentByte(body[tail+len(".COUNT")])) {
				b.WriteString(`cardinality("_bulk_exceptions")`)
				i = tail + len(".COUNT")
				continue
			}
			// SQL%BULK_EXCEPTIONS(<idx>).<field>
			if tail < len(body) && body[tail] == '(' {
				closer, ok := findMatchingParen(body, tail)
				if ok {
					idx := strings.TrimSpace(body[tail+1 : closer])
					field := ""
					afterParen := closer + 1
					if afterParen < len(body) && body[afterParen] == '.' {
						k := afterParen + 1
						for k < len(body) && isIdentByte(body[k]) {
							k++
						}
						field = strings.ToUpper(body[afterParen+1 : k])
						afterParen = k
					}
					switch field {
					case "ERROR_INDEX":
						fmt.Fprintf(&b, `("_bulk_exceptions"[%s]->>'error_index')::int`, idx)
					case "ERROR_CODE":
						fmt.Fprintf(&b, `("_bulk_exceptions"[%s]->>'error_code')`, idx)
					case "ERROR_MESSAGE":
						fmt.Fprintf(&b, `("_bulk_exceptions"[%s]->>'error_message')`, idx)
					default:
						// Unknown field — emit the whole element as JSONB
						// so the user can see what was captured.
						fmt.Fprintf(&b, `"_bulk_exceptions"[%s]`, idx)
					}
					i = afterParen
					continue
				}
			}
		}
		b.WriteByte(body[i])
		i++
	}
	return b.String()
}

// stripSqlBulkExceptionsLoops is kept (Deprecated) for any caller that
// still references it; new code should call translateSqlBulkExceptionsRefs.
//
// Deprecated: replaced by translateSqlBulkExceptionsRefs which preserves
// the post-FORALL diagnostic loops by translating their references to
// the PG _bulk_exceptions JSONB accumulator instead of dropping them.
func stripSqlBulkExceptionsLoops(body string) string {
	if body == "" {
		return body
	}
	upper := strings.ToUpper(body)
	if !strings.Contains(upper, "BULK_EXCEPTIONS") {
		return body
	}
	var b strings.Builder
	b.Grow(len(body))
	i := 0
	for i < len(body) {
		// Try to detect a `FOR …` loop opener at i (word-boundary). The
		// detection MUST be conservative: only a numeric-FOR whose
		// header references BULK_EXCEPTIONS qualifies for the strip
		// (otherwise we'd nuke unrelated FOR loops in the same routine
		// and a regular `CURSOR c FOR SELECT … LOOP` cursor decl).
		if (i == 0 || !isIdentByte(body[i-1])) &&
			i+3 <= len(body) && strings.EqualFold(body[i:i+3], "FOR") &&
			(i+3 == len(body) || !isIdentByte(body[i+3])) {
			loopOpenIdx := findKeywordCI(body, i+3, "LOOP")
			if loopOpenIdx > 0 {
				header := body[i:loopOpenIdx]
				if strings.Contains(strings.ToUpper(header), "BULK_EXCEPTIONS") {
					endIdx := findMatchingEndLoop(body, loopOpenIdx+len("LOOP"))
					if endIdx > 0 {
						b.WriteString("-- (post-FORALL diagnostic loop dropped — squishy converted each iteration into a RAISE NOTICE; SQL%BULK_EXCEPTIONS has no PG counterpart)\n  NULL;")
						i = endIdx
						continue
					}
				}
			}
		}
		b.WriteByte(body[i])
		i++
	}
	return b.String()
}

// findMatchingEndLoop returns the byte offset just past `END LOOP;` (or
// `END LOOP`) that closes a LOOP body started before `start`. Nested
// LOOP keywords are tracked as +1, END LOOP as -1, balanced. Returns
// -1 when no closing END LOOP is found.
func findMatchingEndLoop(s string, start int) int {
	depth := 1
	i := start
	for i < len(s) && depth > 0 {
		// Skip line comments and strings to avoid false-positive matches.
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if s[i] == '\'' {
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// Match `LOOP` at word boundary (excluding the keyword that
		// follows END / FOR — those are separate concerns).
		if (i == 0 || !isIdentByte(s[i-1])) &&
			i+4 <= len(s) && strings.EqualFold(s[i:i+4], "LOOP") &&
			(i+4 == len(s) || !isIdentByte(s[i+4])) {
			// Was this preceded by END (whitespace-only between)?
			j := i - 1
			for j > 0 && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j--
			}
			if j >= 2 && strings.EqualFold(s[j-2:j+1], "END") &&
				(j-2 == 0 || !isIdentByte(s[j-3])) {
				depth--
				i += 4
				if depth == 0 {
					// consume optional trailing `;`
					for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
						i++
					}
					if i < len(s) && s[i] == ';' {
						i++
					}
					return i
				}
				continue
			}
			// Otherwise this LOOP opens a new nested loop.
			depth++
			i += 4
			continue
		}
		i++
	}
	return -1
}

// normalizeQuotedSchemaCall converts every `[CALL ]"<schema>"."<func>"
// (args)` occurrence into `<schema>.<func>(args)` (unquoted, no leading
// CALL keyword). The PG writer emits the quoted-CALL form for AST-driven
// dispatch nodes targeting Oracle system packages (DBMS_OUTPUT,
// DBMS_APPLICATION_INFO, …), but the downstream text-pass rewrites
// (rewriteCallToNotice / rewriteCallToNoop) match on the unquoted token
// shape only. Without this normalisation, AST-emitted dbms_output calls
// stay as `CALL "DBMS_OUTPUT"."PUT_LINE"(args)` and PG raises `syntax
// error at or near "DBMS_OUTPUT"` (CALL accepts a procedure name, not a
// nested-quoted identifier with parens).
func normalizeQuotedSchemaCall(s, schema string) string {
	if s == "" {
		return s
	}
	// Build the literal we look for: `"<SCHEMA>"."` (case-insensitive
	// on the name; quotes are literal).
	prefix := `"` + strings.ToUpper(schema) + `".`
	prefixLow := `"` + strings.ToLower(schema) + `".`
	if !strings.Contains(s, prefix) && !strings.Contains(s, prefixLow) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// Match either uppercase or lowercase quoted schema.
		var pfx string
		switch {
		case i+len(prefix) <= len(s) && strings.EqualFold(s[i:i+len(prefix)], prefix):
			pfx = prefix
		default:
			b.WriteByte(s[i])
			i++
			continue
		}
		// Walk over the trailing `"<func>"` segment.
		j := i + len(pfx) // points just past the dot
		if j >= len(s) || s[j] != '"' {
			b.WriteByte(s[i])
			i++
			continue
		}
		fnStart := j + 1
		fnEnd := fnStart
		for fnEnd < len(s) && s[fnEnd] != '"' {
			fnEnd++
		}
		if fnEnd >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		// Strip a preceding `CALL` token if present (case-insensitive).
		// We've already emitted bytes up to `i`, so we operate on the
		// builder. Walk back from the end over whitespace, then check
		// for CALL — boundary-aware so an ident ending in CALL (e.g.
		// PROC_RECALL) survives.
		cur := b.String()
		trimAt := len(cur)
		for trimAt > 0 && (cur[trimAt-1] == ' ' || cur[trimAt-1] == '\t') {
			trimAt--
		}
		if trimAt >= 4 && strings.EqualFold(cur[trimAt-4:trimAt], "CALL") {
			boundaryOK := trimAt == 4 || !isIdentByte(cur[trimAt-5])
			if boundaryOK {
				b.Reset()
				b.WriteString(cur[:trimAt-4])
			}
		}
		// Re-emit `<schema>.<fn>` lowercased + the rest.
		b.WriteString(strings.ToLower(strings.Trim(s[i:i+len(pfx)-1], `"`)))
		b.WriteString(".")
		b.WriteString(s[fnStart:fnEnd])
		i = fnEnd + 1
	}
	return b.String()
}

// rewriteUTLFile lowercases every Oracle `UTL_FILE.*` invocation so the
// migrated routines bind to the `utl_file` schema shipped by orafce
// (https://github.com/orafce/orafce). orafce mirrors Oracle's package
// signatures 1:1 (FOPEN / PUT_LINE / PUT / PUTF / NEW_LINE / FCLOSE / ...)
// — once the extension is installed on the target, the calls compile and
// run unchanged.
//
// Why bother lowercasing if PG folds unquoted identifiers anyway?
// DBMS_METADATA emits the original Oracle identifiers in full UPPERCASE,
// quoted by the writer (`"UTL_FILE"."PUT_LINE"`). Quoted identifiers are
// case-sensitive in PG, so `"UTL_FILE"` would never match orafce's
// lowercase schema. Stripping the quotes by re-emitting the calls in
// lowercase keeps them resolvable through search_path.
//
// Returns (rewritten body, used) so the caller can flag the orafce
// dependency as a blocking prerequisite.
func rewriteUTLFile(s string) (string, bool) {
	used := false
	out := s
	// DBMS_OUTPUT / DBMS_APPLICATION_INFO are diagnostic-only on the
	// Oracle side: PUT_LINE writes to the SQL*Plus stdout, SET_MODULE /
	// SET_ACTION / SET_CLIENT_INFO update v$session metadata. PG has a
	// native equivalent — RAISE NOTICE — so we rewrite them to that
	// instead of pulling orafce in just for log lines. The user keeps
	// the runtime visibility (NOTICE messages stream to the client and
	// to client_min_messages-filtered logs) without the extra extension.
	out = rewriteDbmsOutput(out)
	out = rewriteDbmsApplicationInfo(out)
	// UTL_FILE (and the other DBMS_* packages that DO need a real
	// implementation — LOB / RANDOM / UTILITY) are left in place but
	// lowercased so PG name resolution finds the orafce-provided schemas.
	// Run normalizeQuotedSchemaCall first to collapse the quoted-AST
	// `CALL "UTL_FILE"."PUT_LINE"(args)` form into `utl_file.put_line
	// (args)` so the lowercase + PERFORM passes below catch it.
	for _, pkg := range []string{"UTL_FILE", "DBMS_LOB", "DBMS_RANDOM", "DBMS_UTILITY"} {
		out = normalizeQuotedSchemaCall(out, pkg)
		out = lowercaseSchemaQualifiedCalls(out, pkg, &used)
	}
	// orafce ships these as functions, not procedures. AST-driven calls
	// already render as `PERFORM <call>` thanks to the PG writer's
	// schema-aware dispatch, but raw-text passes (EXCEPTION handler
	// bodies captured as text by the parser, MariaDB-style raw SQL, …)
	// emit a bare `<schema>.<fn>(args);` which PG plpgsql refuses with
	// `syntax error at or near "<schema>"`. Prepend PERFORM to those
	// statement-position calls so the body compiles.
	for _, schema := range []string{"utl_file", "dbms_lob", "dbms_random", "dbms_utility"} {
		out = prependPerformOnStatementCalls(out, schema)
	}
	return out, used
}

// prependPerformOnStatementCalls scans s for `<schema>.<fn>(…)` invocations
// whose left context places them at statement boundary (start-of-string,
// `;`, `THEN`, `BEGIN`, `LOOP`, `ELSE`, label `:`, etc.) and inserts a
// leading `PERFORM ` so PG plpgsql accepts the call as a statement.
// String literals are skipped to avoid rewriting incidental matches inside
// a quoted message.
func prependPerformOnStatementCalls(s, schema string) string {
	if !containsKeywordCI(s, schema) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s) + 16)
	i := 0
	inStr := false
	for i < len(s) {
		c := s[i]
		if inStr {
			out.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					out.WriteByte(s[i+1])
					i += 2
					continue
				}
				inStr = false
			}
			i++
			continue
		}
		// SQL comments must NOT toggle string state. An apostrophe sitting
		// in `-- d'une dépose` (French) would otherwise latch inStr=true
		// for the rest of the body and silently disable every PERFORM
		// prefix downstream — same hazard as Fix 17 for findKeywordCI but
		// in this walker's local string tracker. Copy the comment span
		// through verbatim.
		if next := skipSQLComment(s, i); next != i {
			out.WriteString(s[i:next])
			i = next
			continue
		}
		if c == '\'' {
			out.WriteByte(c)
			inStr = true
			i++
			continue
		}
		// Try to match `<schema>` (case-insensitive) at a word boundary,
		// followed by `.<ident>(`.
		if (c == '"' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') &&
			(i == 0 || !isIdentByte(s[i-1])) {
			pkg, p, ok := readSchemaToken(s, i)
			if ok && strings.EqualFold(pkg, schema) && p < len(s) && s[p] == '.' {
				_, q, ok2 := readSchemaToken(s, p+1)
				if ok2 {
					r := q
					for r < len(s) && (s[r] == ' ' || s[r] == '\t') {
						r++
					}
					if r < len(s) && s[r] == '(' &&
						isStatementBoundaryTail(out.String()) {
						out.WriteString("PERFORM ")
					}
				}
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// isStatementBoundaryTail looks back over the already-emitted text and
// reports whether the last non-whitespace token signals a statement-
// boundary position (where a bare `<schema>.<fn>(...)` call needs a
// PERFORM prefix to be a valid PL/pgSQL statement).
//
// Boundaries:
//   - start of buffer (empty / whitespace-only tail)
//   - `;`  / `/`  (statement terminators)
//   - keywords that open a statement list: THEN, ELSE, BEGIN, LOOP,
//     DECLARE, EXCEPTION
//
// Already-prefixed contexts (PERFORM/CALL/RETURN/RAISE/EXECUTE/`:=`/
// `INTO`) are explicitly NOT boundaries — the call is already in an
// expression position and must not be re-prefixed.
func isStatementBoundaryTail(emitted string) bool {
	tail := strings.TrimRight(emitted, " \t\r\n")
	if tail == "" {
		return true
	}
	if last := tail[len(tail)-1]; last == ';' || last == '/' {
		return true
	}
	upper := strings.ToUpper(tail)
	for _, kw := range []string{"PERFORM", "CALL", "RETURN", "RAISE", "EXECUTE", ":=", "INTO"} {
		if hasTrailingWord(upper, kw) {
			return false
		}
	}
	for _, kw := range []string{"THEN", "ELSE", "BEGIN", "LOOP", "DECLARE", "EXCEPTION"} {
		if hasTrailingWord(upper, kw) {
			return true
		}
	}
	return false
}

// hasTrailingWord reports whether upper ends with the keyword `kw` at a
// word boundary (no identifier char immediately preceding kw inside the
// trailing run).
func hasTrailingWord(upper, kw string) bool {
	if !strings.HasSuffix(upper, kw) {
		return false
	}
	leftBoundary := len(upper) == len(kw)
	if !leftBoundary {
		c := upper[len(upper)-len(kw)-1]
		leftBoundary = !isIdentByte(c)
	}
	return leftBoundary
}

// rewriteDbmsOutput maps the Oracle DBMS_OUTPUT package to PG's RAISE NOTICE.
//   - DBMS_OUTPUT.PUT_LINE(msg) → RAISE NOTICE '%', msg
//   - DBMS_OUTPUT.PUT(msg)      → RAISE NOTICE '%', msg
//   - DBMS_OUTPUT.NEW_LINE      → NULL  (NOTICE already adds a delimiter)
//   - DBMS_OUTPUT.ENABLE / DISABLE / GET_LINE / GET_LINES → NULL (no-op)
func rewriteDbmsOutput(s string) string {
	if !containsKeywordCI(s, "DBMS_OUTPUT") {
		return s
	}
	// Normalise the quoted-AST form `[CALL ]"DBMS_OUTPUT"."<fn>"(args)`
	// emitted by the PG writer for AST-driven CALL nodes back to the
	// unquoted `DBMS_OUTPUT.<fn>(args)` shape so the keyword-based
	// findKeywordCI matcher below catches both call sites (AST-driven
	// CALLs with quoted idents, and raw-text EXCEPTION-handler bodies
	// with bare lowercase calls).
	s = normalizeQuotedSchemaCall(s, "DBMS_OUTPUT")
	s = rewriteCallToNotice(s, "DBMS_OUTPUT.PUT_LINE")
	s = rewriteCallToNotice(s, "DBMS_OUTPUT.PUT")
	s = rewriteCallToNoop(s, "DBMS_OUTPUT.NEW_LINE")
	s = rewriteCallToNoop(s, "DBMS_OUTPUT.ENABLE")
	s = rewriteCallToNoop(s, "DBMS_OUTPUT.DISABLE")
	s = rewriteCallToNoop(s, "DBMS_OUTPUT.GET_LINE")
	s = rewriteCallToNoop(s, "DBMS_OUTPUT.GET_LINES")
	return s
}

// rewriteDbmsApplicationInfo maps Oracle session-metadata setters to PG's
// RAISE NOTICE so the call sites compile and the values still surface in
// the log stream (PG has no per-session module/action metadata API in
// core; pg_stat_activity exposes only application_name + the running
// query). The Oracle semantics — pure observability — translate cleanly
// to NOTICE.
func rewriteDbmsApplicationInfo(s string) string {
	if !containsKeywordCI(s, "DBMS_APPLICATION_INFO") {
		return s
	}
	s = normalizeQuotedSchemaCall(s, "DBMS_APPLICATION_INFO")
	s = rewriteCallToNotice(s, "DBMS_APPLICATION_INFO.SET_MODULE")
	s = rewriteCallToNotice(s, "DBMS_APPLICATION_INFO.SET_ACTION")
	s = rewriteCallToNotice(s, "DBMS_APPLICATION_INFO.SET_CLIENT_INFO")
	s = rewriteCallToNotice(s, "DBMS_APPLICATION_INFO.SET_SESSION_LONGOPS")
	s = rewriteCallToNoop(s, "DBMS_APPLICATION_INFO.READ_MODULE")
	s = rewriteCallToNoop(s, "DBMS_APPLICATION_INFO.READ_CLIENT_INFO")
	return s
}

// rewriteCallToNotice replaces every `<target>(arg1, arg2, ...)` invocation
// with `RAISE NOTICE '%', concat_ws(' ', arg1, arg2, ...)` so the call
// renders as a PG diagnostic line carrying every original argument.
// Single-arg calls collapse to `RAISE NOTICE '%', arg`.
func rewriteCallToNotice(s, target string) string {
	if !containsKeywordCI(s, target) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for {
		k := findKeywordCI(s, i, target)
		if k < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		j := skipWS(s, k+len(target))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(target)])
			i = k + len(target)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(target)])
			i = k + len(target)
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		out.WriteString(s[i:k])
		switch len(args) {
		case 0:
			out.WriteString("RAISE NOTICE '%', '(no args)'")
		case 1:
			out.WriteString("RAISE NOTICE '%', ")
			out.WriteString(strings.TrimSpace(args[0]))
		default:
			out.WriteString("RAISE NOTICE '%', concat_ws(' ', ")
			parts := make([]string, 0, len(args))
			for _, a := range args {
				parts = append(parts, strings.TrimSpace(a))
			}
			out.WriteString(strings.Join(parts, ", "))
			out.WriteString(")")
		}
		i = end + 1
	}
}

// rewriteCallToNoop replaces `<target>(...)` with the literal `NULL` so
// the call site stays syntactically valid while doing nothing — for
// reads (e.g. DBMS_OUTPUT.GET_LINE) and metadata operations that have no
// PG equivalent and are usually safe to drop.
func rewriteCallToNoop(s, target string) string {
	if !containsKeywordCI(s, target) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for {
		k := findKeywordCI(s, i, target)
		if k < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		j := skipWS(s, k+len(target))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i:k])
			out.WriteString("NULL")
			i = k + len(target)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(target)])
			i = k + len(target)
			continue
		}
		out.WriteString(s[i:k])
		out.WriteString("NULL")
		i = end + 1
	}
}

// lowercaseSchemaQualifiedCalls scans s for `<SCHEMA>.<func>(...)` calls
// (with optional double-quotes around either part) and rewrites them to
// the lowercase, unquoted form. `mark` is set to true on the first hit so
// the prerequisite builder can surface the schema as a blocking dependency.
// String literals are skipped so an Oracle identifier appearing inside a
// quoted error message isn't accidentally rewritten.
func lowercaseSchemaQualifiedCalls(s, schema string, mark *bool) string {
	if !containsKeywordCI(s, schema) && !containsKeywordCI(s, `"`+schema+`"`) {
		return s
	}
	lcSchema := strings.ToLower(schema)
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	inStr := false
	for i < len(s) {
		c := s[i]
		if inStr {
			out.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					out.WriteByte(s[i+1])
					i += 2
					continue
				}
				inStr = false
			}
			i++
			continue
		}
		if c == '\'' {
			out.WriteByte(c)
			inStr = true
			i++
			continue
		}
		// Try to match (optionally-quoted) <schema>(.|.)(optionally-quoted)<func>
		// at a word boundary.
		if c == '"' || isIdentStart(c) {
			if i > 0 && c != '"' && isIdentChar(s[i-1]) {
				out.WriteByte(c)
				i++
				continue
			}
			schemaTok, p, ok := readSchemaToken(s, i)
			if ok && strings.EqualFold(schemaTok, schema) {
				if p < len(s) && s[p] == '.' {
					funcTok, q, ok2 := readSchemaToken(s, p+1)
					if ok2 {
						// Must look like a function call (followed by `(`).
						r := q
						for r < len(s) && (s[r] == ' ' || s[r] == '\t') {
							r++
						}
						if r < len(s) && s[r] == '(' {
							out.WriteString(lcSchema)
							out.WriteByte('.')
							out.WriteString(strings.ToLower(funcTok))
							i = q
							if mark != nil {
								*mark = true
							}
							continue
						}
					}
				}
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// readSchemaToken reads a quoted ("X") or bare (X) identifier token at p
// and returns (lit, end, ok). Identifier characters follow the SQL idiom:
// letter / digit / underscore / `#` / `$` (Oracle allows the latter two).
func readSchemaToken(s string, p int) (string, int, bool) {
	if p >= len(s) {
		return "", p, false
	}
	if s[p] == '"' {
		end := strings.IndexByte(s[p+1:], '"')
		if end < 0 {
			return "", p, false
		}
		return s[p+1 : p+1+end], p + 1 + end + 1, true
	}
	if !isIdentStart(s[p]) {
		return "", p, false
	}
	q := p
	for q < len(s) && (isIdentChar(s[q]) || s[q] == '#' || s[q] == '$') {
		q++
	}
	return s[p:q], q, true
}


// rewriteRaiseApplicationError translates `RAISE_APPLICATION_ERROR(code, msg)`
// to PL/pgSQL's `RAISE EXCEPTION '%', msg USING ERRCODE='Pxxxx'` form.
// Oracle's user-defined error range is -20000..-20999; we map to PG's P0001
// .. P0999 (truncating; 4-digit room stays clear of P0000 reserved-class).
// Best-effort textual rewrite — the Oracle parser doesn't model this as a
// distinct AST node so the call is parsed as a generic CallStmt and emitted
// verbatim by the structural writer.
func rewriteRaiseApplicationError(s string) string {
	const fname = "RAISE_APPLICATION_ERROR"
	var out strings.Builder
	i := 0
	for {
		k := findKeywordCI(s, i, fname)
		if k < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		j := skipWS(s, k+len(fname))
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		end, ok := findMatchingParen(s, j)
		if !ok {
			out.WriteString(s[i : k+len(fname)])
			i = k + len(fname)
			continue
		}
		args := splitTopLevelArgs(s[j+1 : end])
		if len(args) < 2 {
			// Malformed; leave it in place and let PG report a clearer
			// error than a half-rewrite would.
			out.WriteString(s[i : end+1])
			i = end + 1
			continue
		}
		code := strings.TrimSpace(args[0])
		msg := strings.TrimSpace(args[1])
		// Map Oracle's negative numeric code to a PG SQLSTATE in the
		// user-defined P0xxx class. `-20001` → `P0001`.
		errcode := "P0001"
		if len(code) > 1 && code[0] == '-' {
			digits := strings.TrimLeft(code[1:], "0")
			if len(digits) > 4 {
				digits = digits[len(digits)-4:]
			}
			if digits != "" {
				errcode = "P" + strings.Repeat("0", 4-len(digits)) + digits
			}
		}
		out.WriteString(s[i:k])
		out.WriteString("RAISE EXCEPTION '%', ")
		out.WriteString(msg)
		out.WriteString(" USING ERRCODE='")
		out.WriteString(errcode)
		out.WriteString("'")
		i = end + 1
	}
}

// parseRoutineBodyByKind routes the raw body text to the right dialect
// parser. Unknown kinds fall back to the MySQL parser (historical default).
func parseRoutineBodyByKind(body string, kind dialects.Kind) ([]ast.PLStmt, string) {
	if dialects.IsOracle(kind) {
		// Oracle routine bodies sit after `IS`/`AS` and mix variable
		// declarations and the executable block WITHOUT an explicit DECLARE
		// keyword, e.g.
		//     n NUMBER;
		//     CURSOR cur IS SELECT id FROM t;
		//     BEGIN … END;
		// The Oracle PL/SQL parser only recognizes the canonical
		// DECLARE/BEGIN/label form, so when the captured body has tokens
		// before the first BEGIN but no leading DECLARE, synthesize one.
		body = prefixOracleDeclare(body)
		// `:NEW.col` / `:OLD.col` are Oracle's bind-variable trigger
		// pseudorecords. Our PL/SQL expression parser doesn't handle the
		// leading `:` mid-expression (it gets fragmented into separate
		// tokens, breaking INSERT … VALUES (… || :OLD.x || …) bodies into
		// stray statements). Strip the colon BEFORE parsing — the
		// remaining `OLD.col` / `NEW.col` is then a plain qualified
		// identifier, which the parser handles correctly. Restoring the
		// PG-style `OLD`/`NEW` semantics in the post-pass would be too
		// late for any expression that triggered a parse error.
		body = stripBindRefColons(body)
		// SQL%BULK_EXCEPTIONS isn't recognised by parser_expr's cursor-
		// attribute branch (FOUND/NOTFOUND/ISOPEN/ROWCOUNT only) and the
		// `%`-suffix would fragment the surrounding expression into
		// separate stmts. Pre-translate every reference here so the
		// parser sees plain PG-shaped expressions: cardinality(...) for
		// .COUNT and JSONB extracts for the per-element fields.
		body = translateSqlBulkExceptionsRefs(body)
		stmts, errs := oracledialect.ParseRoutineBody(body)
		if len(errs) == 0 {
			return stmts, ""
		}
		return stmts, errs.Error()
	}
	stmts, errs := mysqldialect.ParseRoutineBody(body)
	if len(errs) == 0 {
		return stmts, ""
	}
	return stmts, errs.Error()
}

type plTranslator struct {
	kind         dialects.Kind
	untranslated []string // truly unhandled constructs — surface as blocking review prereqs
	notes        []string // best-effort translations with documented PG-divergent semantics — info-level only
	// usedBulkExceptions is set by the FORALL SAVE EXCEPTIONS emitter
	// (oracle_visitors_pragma's neighbour: plsql_xlate's ForallStmt
	// case). When true, TranslateRoutineBodyExtV prepends a
	// `_bulk_exceptions jsonb[] := '{}'` decl to the routine's
	// outermost block so the per-iteration error accumulator is
	// visible to both the FORALL body and any post-loop diagnostic
	// access (see translateSqlBulkExceptionsRefs).
	usedBulkExceptions bool
	// userExceptions tracks the lowercased names of user-defined Oracle
	// exceptions declared anywhere in the routine body (e.g. `appli_error
	// EXCEPTION;`). PG plpgsql has no name→SQLSTATE binding — referencing
	// them in `WHEN <name> THEN` is a hard parse error. We collect them as
	// blocks are translated, then in extractExceptionHandlers any WHEN
	// clause naming one is remapped to OTHERS with a TODO marker so the
	// user can audit. This sacrifices Oracle's per-name dispatch (which
	// PRAGMA EXCEPTION_INIT was already noted as unsupportable in PG) for
	// a routine that compiles.
	userExceptions map[string]bool
	// oracleTypes maps the lowercased name of an Oracle local TYPE/SUBTYPE
	// declaration to the PG type it should be replaced with at every
	// variable-decl and body site:
	//   TYPE arr IS TABLE OF NUMBER INDEX BY BINARY_INTEGER;  → "numeric[]"
	//   TYPE rec IS RECORD (a NUMBER, b VARCHAR2(20));        → "RECORD"
	//   TYPE c IS REF CURSOR;                                 → "refcursor"
	//   SUBTYPE big_int IS NUMBER(20);                        → "numeric(20)"
	// Variables typed as a registered name get the underlying type at
	// emission time. Arrays additionally enable a body text-pass that
	// translates Oracle collection methods (.COUNT/.FIRST/.NEXT/(i)
	// indexing) to their PG equivalents.
	oracleTypes map[string]oracleTypeInfo
	// collectionVars holds the lowercased names of variables declared
	// with a TABLE OF / VARRAY type. The post-translation body rewrite
	// uses this set to disambiguate `name(i)` (array index → `name[i]`)
	// from `name(arg)` (function call, kept verbatim) since both share
	// surface syntax in Oracle.
	collectionVars map[string]string // varName → element PG type
	// cursorNames holds the lowercased names of cursors declared in the
	// current routine. Used by declareVarText to rewrite a variable's
	// declared type from `<cursor>%ROWTYPE` (Oracle, PG-incompatible)
	// to `RECORD` (PG-compatible — anonymous record materialised on
	// first FETCH). PG plpgsql's `%ROWTYPE` is restricted to tables and
	// views; cursors aren't valid base names for it, so the literal
	// substitution would emit `<cursor> does not exist` at apply time.
	cursorNames map[string]bool
	// arrayUsedVars holds lowercased variable names that appear as
	// array-subscript targets (`var[idx] := …`) in the routine body.
	// declareVarText promotes their type to `<base>[]` when the
	// original DECLARE didn't carry an array hint — typical of Oracle
	// `TYPE TblText IS TABLE OF VARCHAR2(255);` declarations the
	// translator couldn't resolve in full.
	arrayUsedVars map[string]bool
}

// oracleTypeInfo describes a translated Oracle TYPE/SUBTYPE so the
// translator can substitute it at every reference site.
type oracleTypeInfo struct {
	// pg is the rendered PG type the variable should be declared as.
	// e.g. "numeric[]", "RECORD", "refcursor", "varchar(20)".
	pg string
	// kind classifies the source so body rewrites can target it:
	//   "table_of"  — TABLE OF / VARRAY (PG array)
	//   "record"    — RECORD (anonymous PG record)
	//   "ref_cursor"— REF CURSOR
	//   "subtype"   — SUBTYPE alias
	kind string
	// elem is the array-element PG type (only set for kind=="table_of").
	// Used by `arr.DELETE` rewrites that need an explicit cast.
	elem string
	// fields is set for kind=="record" — maps lowercased field name to
	// the raw type-text for the field. Used by the body rewriter to
	// detect record fields that are themselves collection-typed
	// (PRC_TLR-style `record.field(i)` element access).
	fields map[string]string
}

// parseRecordFieldList parses the body of `RECORD (f1 t1, f2 t2, …)`
// (the input is the substring AFTER the RECORD keyword and may include
// whitespace before the opening `(`). Returns nil when the body isn't a
// well-formed parenthesised field list.
//
// Each value is the raw type text trimmed of whitespace; the caller
// resolves it against the local oracleTypes map to decide whether the
// field is a collection.
func parseRecordFieldList(body string) map[string]string {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "(") || !strings.HasSuffix(body, ")") {
		return nil
	}
	inner := strings.TrimSpace(body[1 : len(body)-1])
	if inner == "" {
		return nil
	}
	// Split at top-level commas (depth-aware so nested parens like
	// `numeric(38,0)` don't split mid-type).
	parts := []string{}
	depth := 0
	last := 0
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, inner[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, inner[last:])
	out := map[string]string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// First whitespace-delimited token is the field name.
		i := 0
		for i < len(p) && !(p[i] == ' ' || p[i] == '\t') {
			i++
		}
		if i == 0 || i >= len(p) {
			continue
		}
		name := strings.ToLower(p[:i])
		typ := strings.TrimSpace(p[i:])
		if name != "" && typ != "" {
			out[name] = typ
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// warn records a construct the translator could not translate (or could only
// translate with potentially-incorrect semantics). Each entry becomes a
// blocking "review the routine body" prerequisite for the user.
// collectionCtorView returns a snapshot of the local Oracle TABLE OF /
// VARRAY type registry shaped for MakeCollectionConstructorVisitor —
// only kind=="table_of" entries with the PG element type. Callers use
// it to rewrite the `<type_name>(args…)` constructor idiom anywhere it
// appears in the routine (decl Default and body assignments alike).
func (t *plTranslator) collectionCtorView() map[string]struct{ Elem string } {
	if len(t.oracleTypes) == 0 {
		return nil
	}
	out := make(map[string]struct{ Elem string }, len(t.oracleTypes))
	for k, v := range t.oracleTypes {
		if v.kind == "table_of" {
			out[k] = struct{ Elem string }{Elem: v.elem}
		}
	}
	return out
}

func (t *plTranslator) warn(msg string) { t.untranslated = append(t.untranslated, msg) }

// note records a best-effort translation that succeeded but differs from
// Oracle semantics in a documented way (e.g. PRAGMA EXCEPTION_INIT dropped,
// FORALL SAVE EXCEPTIONS wrapped in BEGIN/EXCEPTION/END). The user may want
// to be aware but no manual intervention is required.
func (t *plTranslator) note(msg string) { t.notes = append(t.notes, msg) }

func (t *plTranslator) stmts(in []ast.PLStmt) []pgast.PLStmt {
	out := make([]pgast.PLStmt, 0, len(in))
	for _, s := range in {
		if r := t.stmt(s); r != nil {
			out = append(out, r)
		}
	}
	return out
}

func (t *plTranslator) stmt(n ast.PLStmt) pgast.PLStmt {
	switch s := n.(type) {
	case *ast.Block:
		return t.block(s)
	case *ast.AssignStmt:
		return &pgast.PLAssign{Target: t.target(s.Target), Expr: t.expr(s.Expr)}
	case *ast.IfStmt:
		out := &pgast.PLIf{}
		for _, br := range s.Branches {
			out.Branches = append(out.Branches, pgast.PLIfBranch{
				Cond: t.expr(br.Cond),
				Body: t.stmts(br.Body),
			})
		}
		out.Else = t.stmts(s.Else)
		return out
	case *ast.CaseStmt:
		out := &pgast.PLCase{}
		if s.Expr != nil {
			out.Expr = t.expr(s.Expr)
		}
		for _, w := range s.When {
			out.When = append(out.When, pgast.PLCaseWhen{
				Match: t.expr(w.Match),
				Body:  t.stmts(w.Body),
			})
		}
		out.Else = t.stmts(s.Else)
		return out
	case *ast.WhileStmt:
		return &pgast.PLWhile{Label: s.Label, Cond: t.expr(s.Cond), Body: t.stmts(s.Body)}
	case *ast.LoopStmt:
		return &pgast.PLLoop{Label: s.Label, Body: t.stmts(s.Body)}
	case *ast.RepeatStmt:
		// REPEAT body UNTIL cond  →  LOOP body; EXIT WHEN cond; END LOOP
		inner := t.stmts(s.Body)
		inner = append(inner, &pgast.PLExitWhen{Cond: t.expr(s.Cond)})
		return &pgast.PLLoop{Label: s.Label, Body: inner}
	case *ast.LeaveStmt:
		if s.WhenCond != nil {
			return &pgast.PLExitWhen{Label: s.Label, Cond: t.expr(s.WhenCond)}
		}
		return &pgast.PLExit{Label: s.Label}
	case *ast.IterateStmt:
		if s.WhenCond != nil {
			return &pgast.PLContinueWhen{Label: s.Label, Cond: t.expr(s.WhenCond)}
		}
		return &pgast.PLContinue{Label: s.Label}
	case *ast.ReturnStmt:
		if s.Expr == nil {
			return &pgast.PLReturn{}
		}
		return &pgast.PLReturn{Expr: t.expr(s.Expr)}
	case *ast.CallStmt:
		// Oracle collection methods at statement level — `coll.DELETE;` /
		// `coll.EXTEND;` / `coll.TRIM[(n)];` — surface as a CallStmt with
		// Schema = collection name and Name = method. Rewrite via the AST
		// so we don't lean on text-level regexes (the rest of the
		// expression-level uses live inside RawExpr.Text and are out of
		// scope until the codebase grows a real expression AST).
		if dialects.IsOracle(t.kind) && s.Schema != "" && len(s.Args) <= 1 {
			switch strings.ToUpper(s.Name) {
			case "DELETE":
				if len(s.Args) == 0 {
					return &pgast.PLRawSQL{Text: s.Schema + " := ARRAY[]::" + s.Schema + "%TYPE"}
				}
				// coll.DELETE(i) — element removal. PG arrays don't have
				// gap semantics, so we emit a TODO comment.
				t.warn(fmt.Sprintf("collection method %s.DELETE(i) — PG arrays have no gap semantics; rewrite as %s := %s[1:i-1] || %s[i+1:]",
					s.Schema, s.Schema, s.Schema, s.Schema))
				return &pgast.PLRawSQL{Text: "-- TODO: " + s.Schema + ".DELETE(" + t.expr(s.Args[0]) + ") — PG arrays have no gap semantics"}
			case "EXTEND":
				if len(s.Args) == 0 {
					return &pgast.PLRawSQL{Text: s.Schema + " := array_append(" + s.Schema + ", NULL)"}
				}
				return &pgast.PLRawSQL{Text: s.Schema + " := array_cat(" + s.Schema + ", array_fill(NULL::anyelement, ARRAY[" + t.expr(s.Args[0]) + "]))"}
			case "TRIM":
				if len(s.Args) == 0 {
					return &pgast.PLRawSQL{Text: s.Schema + " := " + s.Schema + "[1:array_length(" + s.Schema + ", 1) - 1]"}
				}
				return &pgast.PLRawSQL{Text: s.Schema + " := " + s.Schema + "[1:array_length(" + s.Schema + ", 1) - " + t.expr(s.Args[0]) + "]"}
			}
		}
		args := make([]string, 0, len(s.Args))
		for _, a := range s.Args {
			args = append(args, t.expr(a))
		}
		return &pgast.PLCall{Schema: s.Schema, Name: s.Name, Args: args}
	case *ast.OpenStmt:
		op := &pgast.PLCursorOp{Kind: "OPEN", Cursor: s.Cursor, Args: s.Args}
		if s.ForQuery != "" {
			// Static body: rewrite Oracle expression idioms (NVL → COALESCE,
			// etc.) so the SELECT runs on PG. Dynamic body: PG's EXECUTE
			// runs the string as-is at runtime; we still apply the rewriter
			// so common literal-string patterns (e.g. NVL embedded in a
			// concat-built dyn-SQL) come out PG-shaped.
			body := s.ForQuery
			if dialects.IsOracle(t.kind) {
				body = rewriteOracleExpr(body)
			} else {
				body = rewriteMySQLBody(body)
			}
			op.ForQuery = body
			op.IsDynamic = s.IsDynamic
			op.UsingArgs = append(op.UsingArgs, s.UsingArgs...)
		}
		return op
	case *ast.FetchStmt:
		return &pgast.PLCursorOp{Kind: "FETCH", Cursor: s.Cursor, Into: s.Into}
	case *ast.CloseStmt:
		return &pgast.PLCursorOp{Kind: "CLOSE", Cursor: s.Cursor}
	case *ast.SignalStmt:
		msg := s.Message
		if msg == "" {
			msg = "signalled from migrated routine"
		}
		return &pgast.PLRaise{Level: "EXCEPTION", Msg: msg, ErrCode: s.SQLState}
	case *ast.SelectInto:
		// PG: SELECT INTO var FROM ... is the same syntax. Apply body rewrite
		// to the rest so backticks/functions/JSON path are normalized.
		vars := strings.Join(s.Vars, ", ")
		return &pgast.PLRawSQL{Text: "SELECT " + rewriteMySQLBody(strings.TrimPrefix(strings.TrimSpace(s.RawQuery), "SELECT")) + " INTO " + vars}
	case *ast.RawSQL:
		// Catch-all for statements the parser left as raw text — COMMIT /
		// ROLLBACK / SAVEPOINT, MariaDB Oracle-compat package bodies, etc.
		// Oracle MERGE used to be rewritten textually here (rewriteOracleMerge);
		// the typed *ast.MergeStmt path now owns that translation, so this
		// branch only normalises lexical differences (backticks, function
		// renames) via rewriteMySQLBody.
		return &pgast.PLRawSQL{Text: rewriteMySQLBody(s.Text)}
	case *ast.SelectStmt:
		// Typed SELECT — emit PG SQL via the structured DML writer, then
		// pass through rewriteMySQLBody so MySQL-only function names that
		// snuck into the AST (via FuncCall.Name) are normalised. For Oracle
		// the post-pass rewriteOraclePLpgSQL covers the analogous fixes.
		return &pgast.PLRawSQL{Text: rewriteMySQLBody(emitSelectStmt(s))}
	case *ast.InsertStmt:
		return &pgast.PLRawSQL{Text: rewriteMySQLBody(emitInsertStmt(s))}
	case *ast.UpdateStmt:
		return &pgast.PLRawSQL{Text: rewriteMySQLBody(emitUpdateStmt(s))}
	case *ast.DeleteStmt:
		return &pgast.PLRawSQL{Text: rewriteMySQLBody(emitDeleteStmt(s))}
	case *ast.MergeStmt:
		// Typed MERGE — render directly to PG MERGE. Oracle-only trailers
		// already absorbed by parseMergeStatement (LOG ERRORS, inline
		// DELETE WHERE on a MATCHED branch) are surfaced as remediation
		// warnings via flags on the AST.
		text := rewriteMySQLBody(emitMergeStmt(s))
		if s.HasLogErrors {
			t.warn("MERGE … LOG ERRORS dropped — PG has no log-errors trailer; INSERT/UPDATE failures abort the MERGE")
		}
		for _, m := range s.WhenMatched {
			if m.HasInlineDelete {
				t.warn("MERGE … WHEN MATCHED THEN UPDATE … DELETE WHERE — manual split into two PG WHEN branches required")
				break
			}
		}
		return &pgast.PLRawSQL{Text: text}
	case *ast.NullStmt:
		// Oracle's `NULL;` is a no-op. PL/pgSQL supports `NULL;` verbatim, so
		// emit it as-is rather than drop it (keeps empty branches valid — e.g.
		// a LOOP body that only contains NULL; or an EXCEPTION handler body).
		return &pgast.PLRawSQL{Text: "NULL"}
	case *ast.CursorForStmt:
		// Oracle `FOR rec IN (SELECT …) LOOP … END LOOP;` maps 1:1 onto PG's
		// FOR-IN-query loop. The Record variable is implicitly declared as
		// ROW type by PL/pgSQL, so we don't need a DECLARE. Cursor-by-name
		// form (`FOR rec IN cur_name[(args)]`) also works on PG when cur_name
		// refers to a refcursor declared in the same block.
		query := s.SelectBody
		if query == "" && s.CursorName != "" {
			// Reconstruct `cursor_name(args)` for the PG FOR clause.
			args := make([]string, 0, len(s.CursorArgs))
			for _, a := range s.CursorArgs {
				args = append(args, t.expr(a))
			}
			if len(args) > 0 {
				query = s.CursorName + "(" + strings.Join(args, ", ") + ")"
			} else {
				query = s.CursorName
			}
		} else {
			query = rewriteMySQLBody(query)
			// Preserve parens around the inline SELECT so post-pass rewriters
			// (notably rewriteOracleOuterJoin) keep a clean boundary at the
			// `)` of the FOR-IN-(SELECT). Without the parens the (+) rewriter
			// walks past the SELECT into the LOOP body looking for a
			// terminator and ends up treating the body's `WHERE` (e.g. an
			// UPDATE WHERE) as the SELECT's clause-end. PRC_RCO_CTRL landed
			// in production with the SELECT WHERE displaced AFTER the UPDATE
			// (two `WHERE` clauses on the same UPDATE) — PG raised
			// `syntax error at or near "WHERE"`. PG plpgsql accepts both
			// forms `FOR rec IN SELECT …` and `FOR rec IN (SELECT …)`, so
			// always wrapping is safe.
			trimmed := strings.TrimSpace(query)
			upper := strings.ToUpper(trimmed)
			if (strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH")) &&
				!(strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")")) {
				query = "(" + trimmed + ")"
			}
		}
		return &pgast.PLForQuery{
			Label: s.Label,
			Vars:  []string{s.Record},
			Query: query,
			Body:  t.stmts(s.Body),
		}
	case *ast.RaiseStmt:
		// Oracle: `RAISE;` re-raises the current exception (only valid
		// inside an EXCEPTION handler); `RAISE exc_name;` raises a named
		// exception (built-in like NO_DATA_FOUND or user-declared via
		// EXCEPTION + optional PRAGMA EXCEPTION_INIT). PG has no named-
		// exception registry; the AST visitor VisitOracleExceptionInit
		// (oracle_visitors_pragma.go) walks each Block, harvests the
		// PRAGMA EXCEPTION_INIT bindings, and stamps the resolved
		// SQLSTATE on every RaiseStmt that names a known exception.
		//
		//   RAISE;                        → RAISE      (re-raise)
		//   RAISE exc_name; (PRAGMA ok)   → RAISE SQLSTATE '<code>'
		//   RAISE exc_name; (no PRAGMA)   → RAISE EXCEPTION '<exc_name>' USING ERRCODE='P0001'
		if s.Name == "" {
			return &pgast.PLRawSQL{Text: "RAISE"}
		}
		// PRAGMA EXCEPTION_INIT binding resolved by the visitor — emit
		// the SQLSTATE form PG plpgsql expects.
		if s.SQLState != "" {
			return &pgast.PLRaise{
				Level:   "EXCEPTION",
				Msg:     s.Name,
				ErrCode: s.SQLState,
			}
		}
		// Oracle exception identifier → PG message + sentinel SQLSTATE.
		// We emit the original Oracle name in the message so a downstream
		// EXCEPTION handler keying on text can still find it.
		return &pgast.PLRaise{
			Level: "EXCEPTION",
			Msg:   s.Name,
			// P0001 is PG's "raise_exception" generic class — semantically
			// closest to a user-raised named exception with no PRAGMA
			// EXCEPTION_INIT mapping.
			ErrCode: "P0001",
		}
	case *ast.ExecuteImmediateStmt:
		// Oracle EXECUTE IMMEDIATE → PL/pgSQL EXECUTE (same syntax shape:
		// optional INTO and USING clauses). The dynamic-SQL expression is
		// passed through rewriteOracleExpr/rewriteMySQLBody so common
		// Oracle idioms (NVL, SYSDATE, …) inside string concatenations
		// come out PG-shaped at runtime.
		var b strings.Builder
		b.WriteString("EXECUTE ")
		sqlExpr := t.expr(s.SQL)
		b.WriteString(sqlExpr)
		if len(s.Into) > 0 {
			b.WriteString(" INTO ")
			b.WriteString(strings.Join(s.Into, ", "))
		}
		if len(s.Using) > 0 {
			b.WriteString(" USING ")
			parts := make([]string, 0, len(s.Using))
			for _, a := range s.Using {
				parts = append(parts, t.expr(a))
			}
			b.WriteString(strings.Join(parts, ", "))
		}
		return &pgast.PLRawSQL{Text: b.String()}
	case *ast.ForallStmt:
		// Oracle FORALL i IN lo..hi <dml>; is a bulk-DML shortcut: semantically
		// equivalent to FOR i IN lo..hi LOOP <dml>; END LOOP. PostgreSQL has no
		// bulk-DML primitive, but a plain integer FOR-loop produces the same
		// effect (at the cost of per-row round-trips). Collection indexing
		// (`v(i)` Oracle) is rewritten to PG array indexing (`v[i]`) and the
		// `.COUNT` attribute to array_length(..., 1) below in the post-pass.
		body := s.RawBody
		// FORALL allows SAVE EXCEPTIONS — Oracle would still execute every
		// iteration, collecting per-iteration errors into SQL%BULK_EXCEPTIONS
		// at the end. PG's plain FOR loop aborts on the first error. To
		// preserve the "best-effort" semantics, wrap each iteration in
		// BEGIN/EXCEPTION/END so individual failures don't abort the whole
		// loop; collected errors are surfaced via a RAISE NOTICE per failure
		// (closest PG analogue, since SQL%BULK_EXCEPTIONS has no PG counterpart
		// and cataloguing them in a per-call array would need a wrapper helper
		// the user has to declare).
		if s.SaveExcept {
			// Full Oracle SAVE EXCEPTIONS semantics: each iteration's
			// failure is captured into a JSONB-typed accumulator
			// (`_bulk_exceptions`) instead of aborting the loop. After
			// the loop, references to `SQL%BULK_EXCEPTIONS.COUNT` /
			// `SQL%BULK_EXCEPTIONS(i).ERROR_INDEX/ERROR_CODE/ERROR_MESSAGE`
			// translate to cardinality(_bulk_exceptions) and JSONB
			// field extracts (via translateSqlBulkExceptionsRefs in the
			// post-pass).
			//
			// usedBulkExceptions tells TranslateRoutineBodyExtV to
			// inject the `_bulk_exceptions jsonb[] := '{}'` declaration
			// in the routine's outermost DECLARE so the accumulator is
			// visible to both the FORALL body and the post-loop
			// diagnostic accessors.
			t.usedBulkExceptions = true
			text := fmt.Sprintf(
				"FOR %[1]s IN %[2]s..%[3]s LOOP\n"+
					"  BEGIN\n"+
					"    %[4]s;\n"+
					"  EXCEPTION WHEN OTHERS THEN\n"+
					"    \"_bulk_exceptions\" := array_append(\"_bulk_exceptions\", jsonb_build_object("+
					"'error_index', %[1]s, 'error_code', SQLSTATE, 'error_message', SQLERRM));\n"+
					"  END;\n"+
					"END LOOP",
				s.Var, t.expr(s.Low), t.expr(s.High),
				rewriteMySQLBody(body))
			return &pgast.PLRawSQL{Text: text}
		}
		return &pgast.PLRawSQL{Text: fmt.Sprintf("FOR %s IN %s..%s LOOP %s; END LOOP",
			s.Var, t.expr(s.Low), t.expr(s.High), rewriteMySQLBody(body))}
	case *ast.NumericForStmt:
		// Oracle: `FOR i IN [REVERSE] lo..hi LOOP <body> END LOOP;`
		// PG:     `FOR i IN [REVERSE] lo..hi LOOP <body> END LOOP;`
		// — the surface syntax is identical, so we can re-emit verbatim
		// (with the body translated stmt-by-stmt and the bound expressions
		// passed through Oracle expr rewrites).
		return &pgast.PLForRange{
			Label:   s.Label,
			Var:     s.Var,
			Reverse: s.Reverse,
			Low:     t.expr(s.Low),
			High:    t.expr(s.High),
			Body:    t.stmts(s.Body),
		}
	}
	t.warn(fmt.Sprintf("unhandled PL/SQL node: %T", n))
	return nil
}

// block translates a MySQL BEGIN…END block. Handles the cursor + NOT FOUND
// handler idiom: detects `DECLARE c CURSOR …; DECLARE CONTINUE HANDLER FOR
// NOT FOUND SET done = 1;` then a loop containing OPEN, FETCH, IF done
// THEN LEAVE, CLOSE — and rewrites it as a FOR row IN <select> LOOP.
func (t *plTranslator) block(blk *ast.Block) pgast.PLStmt {
	pgBlock := &pgast.PLBlock{Label: blk.Label}

	// Detect the cursor idiom pre-translation.
	cursor, handlerVar, handlerMatched := detectCursorIdiom(blk)

	// Pre-pass: scan body stmts for variables used as array
	// subscript targets (`var[idx] := …`). The Oracle parser emits
	// `Target: "var[idx]"` for these, but the matching DeclareVar
	// usually carries no array hint (Oracle TYPE … TABLE OF …
	// declarations the translator doesn't fully resolve fall back
	// to TEXT). Mark them so the DECLARE loop below can promote
	// the type to TEXT[].
	if t.arrayUsedVars == nil {
		t.arrayUsedVars = map[string]bool{}
	}
	collectArraySubscriptTargets(blk.Stmts, t.arrayUsedVars)

	// Pre-pass: harvest Oracle local TYPE/SUBTYPE declarations into the
	// translator's type registry so subsequent variable declarations can
	// substitute the PG-native equivalent (an array, RECORD, refcursor,
	// or aliased scalar) instead of falling back to TEXT. Done before
	// the decl loop below so the substitution sees every type — including
	// types declared after the variables that use them, since Oracle
	// declarations are scope-visible regardless of textual order within
	// the same DECLARE section.
	for _, d := range blk.Decls {
		if pr, ok := d.(*ast.PragmaStmt); ok {
			if k := strings.ToUpper(pr.Kind); k == "TYPE" || k == "SUBTYPE" {
				if t.oracleTypes == nil {
					t.oracleTypes = map[string]oracleTypeInfo{}
				}
				if name, info, ok := parseOracleTypeDecl(pr.Text, k, t.kind); ok {
					t.oracleTypes[strings.ToLower(name)] = info
				}
			}
		}
	}

	// Decls → PG DECLAREs. Skip cursor + handler if the idiom was matched
	// (they get absorbed by the FOR-loop rewrite).
	for _, d := range blk.Decls {
		switch x := d.(type) {
		case *ast.DeclareVar:
			if handlerMatched && x.Name == handlerVar {
				continue // `done` flag no longer needed
			}
			// User-defined Oracle exception declaration `<name> EXCEPTION;`
			// — PG has no equivalent of a named exception type. Track
			// the name so the EXCEPTION-block extractor can remap any
			// `WHEN <name> THEN` to OTHERS, and skip emitting a PG decl
			// (otherwise we'd produce `name EXCEPTION;` which PG can't
			// parse).
			if isExceptionDecl(x) {
				if t.userExceptions == nil {
					t.userExceptions = map[string]bool{}
				}
				t.userExceptions[strings.ToLower(x.Name)] = true
				continue
			}
			// If the variable is typed by a registered Oracle local
			// TABLE OF / VARRAY type, record it so the body rewrite
			// pass knows to translate `var(i)` → `var[i]` and the
			// collection methods (.COUNT/.FIRST/.LAST/.NEXT/.EXTEND/
			// .DELETE) into PG equivalents.
			if udt, ok := x.Type.(*ast.UserDefinedType); ok {
				if info, hit := t.oracleTypes[strings.ToLower(udt.Name)]; hit && info.kind == "table_of" {
					if t.collectionVars == nil {
						t.collectionVars = map[string]string{}
					}
					t.collectionVars[strings.ToLower(x.Name)] = info.elem
				}
				// Record-typed variable: register every field that is
				// itself a collection so the rewriter turns
				// `<rec_var>.<coll_field>(i)` into `<rec_var>.<coll_field>[i]`.
				// PRC_TLR has `t_TRN ot_TRN` where ot_TRN is RECORD with
				// fields like NUM_TRN ovt_NUM_TRN (TABLE OF). Without this,
				// every `t_TRN.NUM_TRN(i)` access in the body stayed as a
				// function call and PG raised `syntax error at or near "."`.
				if info, hit := t.oracleTypes[strings.ToLower(udt.Name)]; hit && info.kind == "record" {
					for fName, fType := range info.fields {
						// Resolve the field type against local oracleTypes
						// to see if it's a TABLE OF.
						fTypeKey := strings.ToLower(strings.TrimSpace(fType))
						if fInfo, fHit := t.oracleTypes[fTypeKey]; fHit && fInfo.kind == "table_of" {
							if t.collectionVars == nil {
								t.collectionVars = map[string]string{}
							}
							t.collectionVars[strings.ToLower(x.Name)+"."+fName] = fInfo.elem
						}
					}
				}
			}
			// Phase 6.6: Oracle's "type-name as constructor" idiom
			//   l_v type_chaine := type_chaine();
			// has no PG counterpart — running the unmodified call
			// raises "function type_chaine() does not exist". When
			// the variable's Default is a FuncCall whose Name matches
			// a registered Oracle TABLE OF / VARRAY type, rewrite
			// it to the equivalent ARRAY[…]::<elem>[] form so the
			// declaration emits a valid empty (or pre-populated) PG
			// array literal.
			if x.Default != nil {
				if ctorView := t.collectionCtorView(); len(ctorView) > 0 {
					if r, ok := ast.Rewrite(x.Default, MakeCollectionConstructorVisitor(ctorView)).(ast.Expr); ok {
						x.Default = r
					}
				}
			}
			pgBlock.Decls = append(pgBlock.Decls, pgast.PLBlockDecl{Text: declareVarText(x, t.kind, t.oracleTypes, t.cursorNames, t.arrayUsedVars)})
		case *ast.DeclareCursor:
			if handlerMatched && x.Name == cursor.Name {
				continue // folded into FOR loop
			}
			// Track the cursor name so any later variable typed as
			// `<this_cursor>%ROWTYPE` can be rewritten to `RECORD`.
			if t.cursorNames == nil {
				t.cursorNames = map[string]bool{}
			}
			t.cursorNames[strings.ToLower(x.Name)] = true
			// PG cursor decl: `name CURSOR [(params)] FOR <select>;`.
			// Without re-emitting the param list, an Oracle
			// `OPEN c (a, b)` (or `FOR rec IN c(a,b) LOOP`) would
			// fail at runtime with "cursor X has no arguments".
			cur := fmt.Sprintf(`%s CURSOR FOR %s;`, x.Name, rewriteMySQLBody(x.SelectBody))
			if strings.TrimSpace(x.Params) != "" {
				cur = fmt.Sprintf(`%s CURSOR (%s) FOR %s;`,
					x.Name, mapCursorParams(x.Params, t.kind), rewriteMySQLBody(x.SelectBody))
			}
			pgBlock.Decls = append(pgBlock.Decls, pgast.PLBlockDecl{Text: cur})
		case *ast.DeclareHandler:
			if handlerMatched && strings.EqualFold(x.Condition, "NOT FOUND") {
				continue
			}
			// Keep as a top-level comment so the user sees it; SQLEXCEPTION
			// would need an explicit EXCEPTION block wrapper.
			pgBlock.Body = append(pgBlock.Body, &pgast.PLRawSQL{
				Text: fmt.Sprintf("-- TODO: DECLARE %s HANDLER FOR %s was not automatically translated",
					x.Kind, x.Condition),
			})
			t.warn(fmt.Sprintf("DECLARE %s HANDLER FOR %s (wrap containing block in BEGIN … EXCEPTION WHEN … THEN … END)",
				x.Kind, x.Condition))
		case *ast.PragmaStmt:
			kind := strings.ToUpper(x.Kind)
			// TYPE / SUBTYPE were already harvested into t.oracleTypes by
			// the pre-pass above. Drop them silently — the substitution
			// at every variable + body site is the actual translation.
			// Emitting a "PRAGMA dropped" warning per local TYPE would
			// be misleading: the type is preserved, just inlined into PG
			// arrays / RECORDs / refcursors / scalar aliases.
			if kind == "TYPE" || kind == "SUBTYPE" {
				continue
			}
			// EXCEPTION_INIT is now translated by VisitOracleExceptionInit
			// (oracle_visitors_pragma.go): the visitor stamps every
			// matching RaiseStmt with the resolved SQLSTATE before the
			// translator runs. The pragma itself can be dropped silently
			// — the rewrite is complete by the time we reach this branch.
			if kind == "EXCEPTION_INIT" {
				pgBlock.Body = append(pgBlock.Body, &pgast.PLRawSQL{
					Text: "-- Oracle PRAGMA EXCEPTION_INIT translated: matching RAISE statements emit RAISE SQLSTATE '<code>' (see PG SQLSTATE 45XXX user-defined class).",
				})
				continue
			}
			// Other Oracle PRAGMAs have no PG equivalent. Drop them from
			// the body so PG can parse the routine, but emit a TODO
			// comment + note so the user has a remediation path. Most
			// common cases:
			//   - AUTONOMOUS_TRANSACTION → use the autonomous-transaction
			//     extension or dblink to spawn a separate session
			//   - SERIALLY_REUSABLE / RESTRICT_REFERENCES / INLINE / UDF
			//     → compiler hints, just dropped
			pgBlock.Body = append(pgBlock.Body, &pgast.PLRawSQL{
				Text: fmt.Sprintf("-- TODO: Oracle PRAGMA %s dropped — no PG equivalent. %s",
					kind, pragmaRemediation(kind)),
			})
			t.note(fmt.Sprintf("PRAGMA %s dropped (no PG equivalent — %s)",
				kind, pragmaRemediation(kind)))
		}
	}

	// Phase 6.6 (body pass): the Oracle "type-name as constructor"
	// idiom can also appear in the body of the block — e.g.
	// `l_tab := type_chaine();` after the variable was declared
	// without a default. Apply the same rewrite to every body stmt
	// so PG sees `ARRAY[]::elem[]` instead of `type_chaine()`.
	if ctorView := t.collectionCtorView(); len(ctorView) > 0 {
		ctor := MakeCollectionConstructorVisitor(ctorView)
		for i, s := range blk.Stmts {
			if rs, ok := ast.Rewrite(s, ctor).(ast.PLStmt); ok {
				blk.Stmts[i] = rs
			}
		}
	}

	// Body statements.
	if handlerMatched {
		// Find the LOOP containing OPEN/FETCH/LEAVE/CLOSE and replace with FOR.
		translated := t.translateCursorBody(blk, cursor)
		pgBlock.Body = append(pgBlock.Body, translated...)
	} else {
		pgBlock.Body = append(pgBlock.Body, t.stmts(blk.Stmts)...)
	}
	// The Oracle PL/SQL parser encodes the EXCEPTION section of a block as
	// a synthetic RawSQL statement prefixed with the marker `/*EXCEPTION*/`
	// (see parser_plsql.go:parsePLBlock). Extract it from the body list and
	// lift it into PLBlock.Exception so the writer emits a proper PG
	// `EXCEPTION WHEN … THEN …` tail, not a stray WHEN in the body.
	pgBlock.Body, pgBlock.Exception = extractExceptionHandlers(pgBlock.Body, t.userExceptions)
	return pgBlock
}

// rewriteOracleCollections translates Oracle collection-method invocations
// on variables that were declared as TABLE OF / VARRAY in the DECLARE
// section to their PG-array equivalents. Three patterns:
//
//  1. `<arr>.METHOD` (no args)        — METHOD ∈ {COUNT, FIRST, LAST}
//  2. `<arr>.METHOD(<expr>)` (with arg)— METHOD ∈ {NEXT, PRIOR, EXTEND}
//  3. `<arr>(<idx>)` (call form)       — array element read/write
//
// The collectionVars map is the gate: only variables we know are
// arrays get the (idx)→[idx] rewrite, otherwise a regular function call
// like `pkg_uti.f_rep_log` would be mangled. Method dispatch (.COUNT,
// .NEXT, …) is name-blind because Oracle reserves those identifiers as
// collection methods exclusively in the .METHOD position.
func rewriteOracleCollections(body string, collectionVars map[string]string) string {
	if body == "" {
		return body
	}
	out := body
	// Method-style rewrites first; they apply regardless of which
	// variable is the receiver. The order matters: `.NEXT(` /
	// `.PRIOR(` (with parens) are matched before bare `.NEXT` /
	// `.PRIOR` so we don't half-eat an arg.
	out = rewriteCollectionMethodNoArg(out, "COUNT", "coalesce(array_length(%s, 1), 0)")
	out = rewriteCollectionMethodNoArg(out, "LAST", "coalesce(array_length(%s, 1), 0)")
	out = rewriteCollectionMethodNoArg(out, "FIRST", "1")
	out = rewriteCollectionMethodWithArg(out, "NEXT", "(%s + 1)")
	out = rewriteCollectionMethodWithArg(out, "PRIOR", "(%s - 1)")
	// EXTEND is a procedure call (no return value). Drop with a NULL
	// stmt so the surrounding `;` stays valid — PG arrays auto-grow on
	// assignment, so the pre-allocation is unnecessary.
	out = rewriteCollectionMethodNoArg(out, "EXTEND", "NULL /* EXTEND no-op (PG arrays auto-grow) */")
	out = rewriteCollectionMethodWithArg(out, "EXTEND", "NULL /* EXTEND(%[2]s) no-op (PG arrays auto-grow) */")
	// DELETE on a whole collection resets to empty. We don't know the
	// element type at the call site (would need plumbing through this
	// pass) so emit `array[]` which PG infers from the LHS.
	out = rewriteCollectionMethodNoArg(out, "DELETE", "array[]")
	// (idx) → [idx] for known array vars.
	if len(collectionVars) > 0 {
		out = rewriteCollectionElementAccess(out, collectionVars)
	}
	return out
}

// rewriteCollectionMethodNoArg rewrites every `<lhs>.<METHOD>` (no
// trailing `(`) where lhs is an Oracle identifier (`name` or
// `name.subname`). The format string is fed `%s` = lhs.
func rewriteCollectionMethodNoArg(body, method, format string) string {
	upMethod := strings.ToUpper(method)
	var b strings.Builder
	upper := strings.ToUpper(body)
	i := 0
	for i < len(body) {
		idx := strings.Index(upper[i:], "."+upMethod)
		if idx < 0 {
			b.WriteString(body[i:])
			break
		}
		idx += i
		// `<lhs>.METHOD` — lhs must be a sequence of identifier bytes
		// (and dots) immediately preceding `.`. Find its start.
		lhsEnd := idx
		lhsStart := lhsEnd
		for lhsStart > 0 {
			c := body[lhsStart-1]
			if isIdentByte(c) || c == '.' || c == '"' {
				lhsStart--
				continue
			}
			break
		}
		// METHOD must end at a non-ident byte (so `.COUNTRY` isn't
		// mistaken for `.COUNT`).
		afterIdx := idx + 1 + len(upMethod)
		if afterIdx < len(body) && isIdentByte(body[afterIdx]) {
			b.WriteString(body[i : afterIdx+1])
			i = afterIdx + 1
			continue
		}
		// Reject if followed by `(` — that's the with-arg form, handled
		// by rewriteCollectionMethodWithArg.
		if afterIdx < len(body) && body[afterIdx] == '(' {
			b.WriteString(body[i : afterIdx+1])
			i = afterIdx + 1
			continue
		}
		// Reject if lhs is empty.
		lhs := body[lhsStart:lhsEnd]
		if strings.TrimSpace(lhs) == "" {
			b.WriteString(body[i : afterIdx])
			i = afterIdx
			continue
		}
		b.WriteString(body[i:lhsStart])
		b.WriteString(fmt.Sprintf(format, lhs))
		i = afterIdx
	}
	return b.String()
}

// rewriteCollectionMethodWithArg rewrites `<lhs>.<METHOD>(<arg>)` —
// format string is fed (`%[1]s` = lhs, `%[2]s` = arg).
func rewriteCollectionMethodWithArg(body, method, format string) string {
	upMethod := strings.ToUpper(method)
	var b strings.Builder
	upper := strings.ToUpper(body)
	i := 0
	for i < len(body) {
		idx := strings.Index(upper[i:], "."+upMethod+"(")
		if idx < 0 {
			b.WriteString(body[i:])
			break
		}
		idx += i
		// METHOD bytes mustn't be a prefix of a longer ident. We also
		// require the token before `.` to belong to an identifier.
		lhsEnd := idx
		lhsStart := lhsEnd
		for lhsStart > 0 {
			c := body[lhsStart-1]
			if isIdentByte(c) || c == '.' || c == '"' {
				lhsStart--
				continue
			}
			break
		}
		lhs := body[lhsStart:lhsEnd]
		if strings.TrimSpace(lhs) == "" {
			b.WriteString(body[i : idx+1])
			i = idx + 1
			continue
		}
		// Find the matching close-paren.
		open := idx + 1 + len(upMethod) // points at the `(`
		closeIdx, okPar := findMatchingParen(body, open)
		if !okPar {
			b.WriteString(body[i : open+1])
			i = open + 1
			continue
		}
		arg := body[open+1 : closeIdx]
		b.WriteString(body[i:lhsStart])
		// Two-argument format strings need explicit indices to keep
		// the lhs/arg ordering predictable.
		b.WriteString(fmt.Sprintf(format, lhs, arg))
		i = closeIdx + 1
	}
	return b.String()
}

// rewriteCollectionElementAccess converts `<arr>(<expr>)` → `<arr>[<expr>]`
// for every variable the translator tagged as a collection. Restricting
// the pass to known-array names avoids mangling regular function calls
// (`pkg_uti.f_log(x)`) into invalid array access.
func rewriteCollectionElementAccess(body string, collectionVars map[string]string) string {
	if len(collectionVars) == 0 {
		return body
	}
	var b strings.Builder
	i := 0
	for i < len(body) {
		// Read the next identifier (possibly dotted) at i; if it
		// matches a known collection var AND the next non-space byte
		// is `(`, rewrite the parens to brackets.
		if !isIdentByte(body[i]) {
			b.WriteByte(body[i])
			i++
			continue
		}
		j := i
		for j < len(body) && (isIdentByte(body[j]) || body[j] == '.') {
			j++
		}
		ident := body[i:j]
		// Skip past whitespace to look for `(`.
		k := j
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if k < len(body) && body[k] == '(' {
			// Try to match the FULL dotted path first (record-field
			// collection access like `t_TRN.NUM_TRN`), then fall back
			// to the bare ident (top-level collection var).
			lower := strings.ToLower(ident)
			matched := false
			if _, hit := collectionVars[lower]; hit {
				matched = true
			} else if dot := strings.LastIndexByte(ident, '.'); dot >= 0 {
				bare := ident[dot+1:]
				if _, hit := collectionVars[strings.ToLower(bare)]; hit {
					matched = true
				}
			}
			if matched {
				closeIdx, okPar := findMatchingParen(body, k)
				if okPar {
					b.WriteString(body[i:k])
					b.WriteByte('[')
					b.WriteString(body[k+1 : closeIdx])
					b.WriteByte(']')
					i = closeIdx + 1
					continue
				}
			}
		}
		b.WriteString(body[i:j])
		i = j
	}
	return b.String()
}

// parseOracleTypeDecl decodes the textual body the parser stuck inside a
// PragmaStmt with Kind="TYPE" or "SUBTYPE" — the form is:
//   TYPE  : "<name> IS TABLE OF <elem> [INDEX BY <key>] [NOT NULL]"
//         | "<name> IS VARRAY(N) OF <elem>" / "VARYING ARRAY(N) OF ..."
//         | "<name> IS RECORD (f1 t1, f2 t2, ...)"
//         | "<name> IS REF CURSOR [RETURN <type>]"
//   SUBTYPE: "<name> IS <basetype> [(precision[,scale])] [RANGE …] [NOT NULL]"
//
// The leading name is stripped at parse time (parser_plsql.go captures
// `name + " " + body`) so what we receive is "<name> <BODY>". Returns the
// extracted name + a pg-side descriptor; ok=false if the form isn't
// recognised — in which case the caller leaves the variable type
// resolution to MapType (which falls back to TEXT, the same as before).
func parseOracleTypeDecl(text, pragmaKind string, kind dialects.Kind) (name string, info oracleTypeInfo, ok bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", oracleTypeInfo{}, false
	}
	// Pull the first token off as the type name.
	var nameTok string
	if i := strings.IndexAny(t, " \t\r\n("); i > 0 {
		nameTok = t[:i]
		t = strings.TrimSpace(t[i:])
	} else {
		return "", oracleTypeInfo{}, false
	}
	upper := strings.ToUpper(t)
	// SUBTYPE alpha IS NUMBER(10);
	if strings.EqualFold(pragmaKind, "SUBTYPE") {
		if !strings.HasPrefix(upper, "IS") {
			return "", oracleTypeInfo{}, false
		}
		base := strings.TrimSpace(t[2:])
		// Drop trailing NOT NULL / RANGE clauses for the alias type
		// (PG won't accept them in a column-less DECLARE).
		base = stripSubtypeTail(base)
		pg := mapOracleScalar(base, kind)
		return nameTok, oracleTypeInfo{pg: pg, kind: "subtype"}, true
	}
	// TYPE name IS …
	if !strings.HasPrefix(upper, "IS ") && upper != "IS" {
		return "", oracleTypeInfo{}, false
	}
	t = strings.TrimSpace(t[2:])
	upper = strings.ToUpper(t)

	switch {
	case strings.HasPrefix(upper, "TABLE OF "):
		rest := strings.TrimSpace(t[len("TABLE OF "):])
		elem := stripIndexByTail(rest)
		pgElem := arrayElemFallback(mapOracleScalar(elem, kind))
		return nameTok, oracleTypeInfo{pg: pgElem + "[]", kind: "table_of", elem: pgElem}, true
	case strings.HasPrefix(upper, "VARRAY(") ||
		strings.HasPrefix(upper, "VARYING ARRAY(") ||
		strings.HasPrefix(upper, "VARRAY "):
		// Find the OF clause and take what follows.
		ofIdx := indexFoldKeyword(t, "OF")
		if ofIdx < 0 {
			return "", oracleTypeInfo{}, false
		}
		elem := strings.TrimSpace(t[ofIdx+len("OF"):])
		pgElem := arrayElemFallback(mapOracleScalar(elem, kind))
		return nameTok, oracleTypeInfo{pg: pgElem + "[]", kind: "table_of", elem: pgElem}, true
	case strings.HasPrefix(upper, "RECORD"):
		// PG has no per-routine RECORD type; using the `RECORD`
		// keyword turns the variable into an anonymous polymorphic
		// record that materialises on its first SELECT … INTO. Field
		// reads after that work as expected.
		//
		// Capture the record's field list so the rewriter knows which
		// fields are collection-typed (Oracle: `TYPE r IS RECORD (a
		// some_table_of_type, b some_other_table_of_type)` — accessing
		// `var.a(i)` is element access, not function call). PG needs
		// `var.a[i]`. Fields are stored as ident → raw type text; the
		// caller resolves type-by-name later.
		fields := parseRecordFieldList(t[len("RECORD"):])
		return nameTok, oracleTypeInfo{pg: "RECORD", kind: "record", fields: fields}, true
	case strings.HasPrefix(upper, "REF CURSOR"):
		return nameTok, oracleTypeInfo{pg: "refcursor", kind: "ref_cursor"}, true
	}
	return "", oracleTypeInfo{}, false
}

// stripIndexByTail trims `INDEX BY <key>` (and `NOT NULL`) suffixes from
// the rest of a TABLE OF body, leaving just the element-type fragment.
func stripIndexByTail(s string) string {
	if idx := indexFoldKeyword(s, "INDEX"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	if idx := indexFoldKeyword(s, "NOT"); idx >= 0 {
		// drop trailing NOT NULL
		if strings.HasPrefix(strings.ToUpper(s[idx:]), "NOT NULL") {
			s = strings.TrimSpace(s[:idx])
		}
	}
	return strings.TrimSpace(s)
}

// stripSubtypeTail strips the `NOT NULL` / `RANGE …` tails that may
// follow a SUBTYPE base type. PG won't accept those in a DECLARE.
func stripSubtypeTail(s string) string {
	for _, kw := range []string{"NOT NULL", "RANGE"} {
		if idx := indexFoldKeyword(s, kw); idx >= 0 {
			s = strings.TrimSpace(s[:idx])
		}
	}
	return strings.TrimSpace(s)
}

// arrayElemFallback rewrites a per-column %TYPE / %ROWTYPE element into
// a concrete PG type. PG plpgsql allows `tab.col%TYPE` for scalar vars
// but does NOT allow it as an array element (`tab.col%TYPE[]` is a
// hard syntax error). We can't introspect the source column from the
// DDL stage, so we fall back to TEXT — which accepts any string-coerced
// value and is the pragmatic compromise for the migration's first run.
// A reviewer can manually tighten the type after schema apply.
func arrayElemFallback(elem string) string {
	up := strings.ToUpper(elem)
	if strings.HasSuffix(up, "%TYPE") || strings.HasSuffix(up, "%ROWTYPE") {
		return "text"
	}
	return elem
}

// mapOracleScalar coerces an Oracle scalar type fragment (e.g.
// "NUMBER(10,2)", "VARCHAR2(20)", "trn.num_trn%TYPE") to its closest PG
// counterpart. Composite / unrecognised types fall through unchanged so
// PG's own resolution (or %TYPE) handles them. Used by parseOracleTypeDecl
// when synthesising the substitution string for TABLE OF / VARRAY / SUBTYPE.
func mapOracleScalar(s string, kind dialects.Kind) string {
	_ = kind
	t := strings.TrimSpace(s)
	if t == "" {
		return "text"
	}
	upper := strings.ToUpper(t)
	// %TYPE / %ROWTYPE — keep, lowercased table.column ref to match PG conv.
	if strings.HasSuffix(upper, "%TYPE") {
		return strings.ToLower(t[:len(t)-len("%TYPE")]) + "%TYPE"
	}
	if strings.HasSuffix(upper, "%ROWTYPE") {
		return strings.ToLower(t[:len(t)-len("%ROWTYPE")]) + "%ROWTYPE"
	}
	// Common scalar mappings — case-insensitive prefix match keeps
	// `(N[,S])` modifiers intact.
	for _, r := range []struct{ from, to string }{
		{"VARCHAR2", "VARCHAR"},
		{"NVARCHAR2", "VARCHAR"},
		{"NCHAR", "CHAR"},
		{"NUMBER", "NUMERIC"},
		{"PLS_INTEGER", "INTEGER"},
		{"BINARY_INTEGER", "INTEGER"},
		{"BINARY_FLOAT", "REAL"},
		{"BINARY_DOUBLE", "DOUBLE PRECISION"},
		{"DATE", "TIMESTAMP(0)"},
		{"CLOB", "TEXT"},
		{"NCLOB", "TEXT"},
		{"BLOB", "BYTEA"},
		{"RAW", "BYTEA"},
	} {
		if strings.HasPrefix(upper, r.from) {
			rest := t[len(r.from):]
			// boundary check — VARCHAR2_x shouldn't match VARCHAR2.
			if rest == "" || !isIdentByte(rest[0]) {
				return r.to + rest
			}
		}
	}
	return t
}

// mapCursorParams takes the raw text of an Oracle cursor parameter list
// (e.g. `p_lot VARCHAR2, p_tra NUMBER DEFAULT 0`) and returns a best-effort
// PG-compatible rewrite (`p_lot VARCHAR, p_tra NUMERIC DEFAULT 0`). We do
// a coarse word-level rewrite of the common Oracle scalar type spellings
// rather than re-tokenising — PG cursor decls are forgiving about types
// inside the param list (PG promotes them to the IN-actuals at OPEN time)
// so unmapped Oracle-only types usually still compile, with a runtime
// type-mismatch surfaced if the actuals don't coerce.
func mapCursorParams(params string, kind dialects.Kind) string {
	_ = kind
	// First, strip Oracle parameter-mode keywords. Cursor params are
	// always read-only in Oracle (the `IN` is optional and the only
	// mode allowed) but the keyword still appears verbatim in code
	// generated by DBMS_METADATA / older DDL exports:
	//   `CURSOR c (p_id IN NUMBER) IS SELECT …`
	// PG cursor params don't take a mode hint, and a leading `IN`
	// triggers `syntax error at or near "IN"`. We drop it (along with
	// OUT / IN OUT — those would be semantic errors in Oracle cursor
	// params anyway, but mirror the parser's tolerance). Use word-
	// boundary replacement so an identifier ending in `_in` (e.g.
	// `p_log_in NUMBER`) is left alone.
	out := replaceWholeWordFold(params, "IN OUT", "")
	out = replaceWholeWordFold(out, "IN", "")
	out = replaceWholeWordFold(out, "OUT", "")
	// Word-boundary replacements; case-insensitive so `Number` and
	// `NUMBER` and `number` all hit. Anchored on identifier boundaries
	// so `NUMBER_T` (a user type) is left alone.
	rules := []struct{ from, to string }{
		{"VARCHAR2", "VARCHAR"},
		{"NVARCHAR2", "VARCHAR"},
		{"NUMBER", "NUMERIC"},
		{"PLS_INTEGER", "INTEGER"},
		{"BINARY_INTEGER", "INTEGER"},
		{"DATE", "TIMESTAMP(0)"},
		{"CLOB", "TEXT"},
		{"NCLOB", "TEXT"},
		{"BLOB", "BYTEA"},
		{"RAW", "BYTEA"},
		{"BOOLEAN", "BOOLEAN"},
	}
	for _, r := range rules {
		out = replaceWholeWordFold(out, r.from, r.to)
	}
	// Collapse runs of whitespace introduced by the IN/OUT removal so
	// the final param list looks tidy: `p_id   NUMERIC` → `p_id NUMERIC`.
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return strings.TrimSpace(out)
}

// replaceWholeWordFold replaces case-insensitive whole-word occurrences
// of from with to, where "whole word" means the match is not adjacent to
// an ident byte (alphanumeric or underscore) on either side. Reuses the
// existing isIdentByte helper from body_rewrite.go.
func replaceWholeWordFold(s, from, to string) string {
	if from == "" {
		return s
	}
	upper := strings.ToUpper(s)
	upFrom := strings.ToUpper(from)
	var b strings.Builder
	i := 0
	for i < len(upper) {
		j := strings.Index(upper[i:], upFrom)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		j += i
		// boundary check
		before := byte(' ')
		if j > 0 {
			before = s[j-1]
		}
		after := byte(' ')
		if j+len(from) < len(s) {
			after = s[j+len(from)]
		}
		if !isIdentByte(before) && !isIdentByte(after) {
			b.WriteString(s[i:j])
			b.WriteString(to)
			i = j + len(from)
		} else {
			b.WriteString(s[i : j+1])
			i = j + 1
		}
	}
	return b.String()
}

// isExceptionDecl returns true when v is a `<name> EXCEPTION;` declaration —
// the Oracle parser stores those as DeclareVar with a UserDefinedType named
// "EXCEPTION" (see parser_plsql.go:parsePLDecl).
func isExceptionDecl(v *ast.DeclareVar) bool {
	if v == nil || v.Type == nil {
		return false
	}
	udt, ok := v.Type.(*ast.UserDefinedType)
	if !ok {
		return false
	}
	return strings.EqualFold(udt.Name, "EXCEPTION")
}

// pragmaRemediation returns a short hint for the most common Oracle
// PRAGMAs surfaced as TODO comments by the block-decl translator.
func pragmaRemediation(kind string) string {
	switch strings.ToUpper(kind) {
	case "AUTONOMOUS_TRANSACTION":
		return "use the pg_background or autonomous_transaction extension, or open a separate session via dblink to commit independently"
	case "EXCEPTION_INIT":
		return "PG has no name→SQLSTATE binding; reference the SQLSTATE directly (RAISE SQLSTATE 'XXXXX')"
	case "SERIALLY_REUSABLE":
		return "PG has no per-call state isolation hint; functions are stateless by default"
	case "RESTRICT_REFERENCES":
		return "PG infers PURITY (RNDS/RNPS/WNDS/WNPS) from the function's body and VOLATILE/STABLE/IMMUTABLE markers"
	case "INLINE":
		return "use a SQL function (LANGUAGE sql) to enable inlining; PL/pgSQL functions are not inlined"
	case "UDF":
		return "no PG counterpart; UDF was an Oracle 12.2+ optimisation hint for compiled-PL/SQL paths"
	}
	return "review the routine body — the PRAGMA was Oracle-specific compiler metadata"
}

// extractExceptionHandlers walks a PG statement list, pulls out any PLRawSQL
// node carrying the `/*EXCEPTION*/` marker emitted by the Oracle PL/SQL
// parser, and returns (remainingBody, renderedHandlers) where
// renderedHandlers is PG-ready text for the PLBlock.Exception slot.
// The Oracle conditions are mapped 1:1 onto their PG equivalents:
//   - NO_DATA_FOUND, TOO_MANY_ROWS, OTHERS, ZERO_DIVIDE, DUP_VAL_ON_INDEX
//     all exist under the same name in PG.
//   - Unknown user-defined conditions fall back to OTHERS with a warning
//     comment so the translation remains syntactically valid.
func extractExceptionHandlers(body []pgast.PLStmt, userExc map[string]bool) ([]pgast.PLStmt, string) {
	out := make([]pgast.PLStmt, 0, len(body))
	var exc string
	for _, s := range body {
		raw, ok := s.(*pgast.PLRawSQL)
		if !ok || !strings.Contains(raw.Text, "/*EXCEPTION*/") {
			out = append(out, s)
			continue
		}
		// Drop the marker, keep the rest. The parser already emits
		// `WHEN names THEN body` entries separated by newlines.
		text := strings.Replace(raw.Text, "/*EXCEPTION*/", "", 1)
		exc = strings.TrimSpace(text)
		// Map Oracle scalar type names that may appear bare in nested
		// DECLARE blocks captured as raw text inside an EXCEPTION handler
		// (e.g. `DECLARE err_code number; BEGIN err_code := SQLCODE;
		// EXCEPTION ...`). PG has no `number` (or `varchar2`/`pls_integer`)
		// type, so rewrite the common scalar names to PG equivalents
		// here — the structured DECLARE walker handles them everywhere
		// else, but raw EXCEPTION bodies bypass that pass.
		exc = mapOracleRawTypeNames(exc)
		// Remap any `WHEN <user_named_exception>` clauses to OTHERS —
		// PG has no name→SQLSTATE binding so the original would fail to
		// parse. Done before semicolon normalization so the line layout
		// the normalizer expects is preserved.
		exc = remapUserNamedWhenClauses(exc, userExc)
		// Bare `RAISE <name>;` re-raises a named Oracle exception. PG
		// has no name binding, so we rewrite to `RAISE EXCEPTION
		// '<name>' USING ERRCODE='P0001'` — preserving the message
		// text the user can match against in containing handlers.
		exc = rewriteBareRaiseInHandlers(exc)
		// Normalize trailing semicolons. The parser captures each WHEN
		// handler verbatim and Oracle sometimes omits the trailing `;` of
		// the handler body — appending one keeps PG happy. We can only
		// safely append a `;` to the LAST non-empty line of each WHEN
		// block; touching every line shreds multi-line `RAISE_APPLICATION_
		// ERROR(\n  -20001,\n  'msg');` calls into syntax-error confetti.
		exc = normalizeExceptionHandlerSemicolons(exc)
	}
	return out, exc
}

// mapOracleRawTypeNames rewrites bare Oracle scalar type names in raw SQL
// fragments to their PG equivalents. Used on EXCEPTION-handler bodies
// (captured verbatim by the parser) so a nested DECLARE like
// `err_code number;` inside a WHEN OTHERS handler doesn't trip PG with
// `type "number" does not exist`. Only the most common scalar names are
// mapped — anything else stays untouched and the user can review.
func mapOracleRawTypeNames(s string) string {
	if s == "" {
		return s
	}
	pairs := []struct{ from, to string }{
		{"number", "numeric"},
		{"NUMBER", "numeric"},
		{"varchar2", "varchar"},
		{"VARCHAR2", "varchar"},
		{"pls_integer", "integer"},
		{"PLS_INTEGER", "integer"},
		{"binary_integer", "integer"},
		{"BINARY_INTEGER", "integer"},
		{"boolean", "boolean"}, // identity, but normalises case
		{"BOOLEAN", "boolean"},
	}
	out := s
	for _, p := range pairs {
		out = replaceWholeWordCI(out, p.from, p.to)
	}
	return out
}

// orafceExceptionSQLSTATEs maps Oracle package-qualified exception names
// (lowercased) to the SQLSTATE codes that the orafce extension raises for
// the corresponding error. PG plpgsql can't reference these by name (no
// name→SQLSTATE binding) but `WHEN sqlstate 'XXXXX' THEN` works directly,
// so we translate `WHEN UTL_FILE.INVALID_PATH` to `WHEN sqlstate 'UTFIP'`
// — preserving Oracle's per-error dispatch instead of collapsing to
// OTHERS. Codes harvested from orafce/utl_file.c (MAKE_SQLSTATE macros).
var orafceExceptionSQLSTATEs = map[string]string{
	// utl_file (orafce/utl_file.c)
	"utl_file.invalid_path":       "UTFIP",
	"utl_file.invalid_mode":       "UTFIM",
	"utl_file.invalid_operation":  "UTFIO",
	"utl_file.invalid_filehandle": "UTFIH",
	"utl_file.write_error":        "UTFIW",
	"utl_file.read_error":         "UTFIR",
	"utl_file.invalid_filename":   "UTFIN",
	"utl_file.access_denied":      "UTFIA",
	"utl_file.invalid_offset":     "UTFIS",
	"utl_file.delete_failed":      "UTFID",
	"utl_file.rename_failed":      "UTFIE",
	// dbms_lob (best-effort — orafce uses generic codes here, but the
	// names appear often enough in Oracle code that we keep a mapping
	// hook even though we currently fall back to OTHERS for them).
}

// pgKnownExceptionConditions is the set of condition names PG plpgsql
// recognises directly in `WHEN <name> THEN` clauses (drawn from PG's
// errcodes-appendix and the SQL/PSM Oracle-compat aliases — NO_DATA_FOUND
// and TOO_MANY_ROWS in particular). Anything else surfacing in an Oracle
// `WHEN` clause is either a user-declared exception, a package-qualified
// name (e.g. `UTL_FILE.INVALID_PATH`) or an Oracle-only built-in with no
// PG counterpart — all of which we remap to OTHERS so the routine still
// compiles. This is a closed allow-list; treating it conservatively means
// we sometimes downgrade a known PG name we forgot to list, but never
// emit DDL that PG can't parse.
var pgKnownExceptionConditions = map[string]bool{
	"others": true, "sqlstate": true,
	// PG/Oracle shared
	"no_data_found": true, "too_many_rows": true,
	// PG-native (PG accepts these directly via plpgsql)
	"division_by_zero": true, "unique_violation": true,
	"foreign_key_violation": true, "check_violation": true,
	"not_null_violation": true, "raise_exception": true,
	"invalid_cursor_state": true, "syntax_error": true,
	"string_data_right_truncation": true,
	"numeric_value_out_of_range":   true,
	"invalid_text_representation":  true,
	"lock_not_available":           true, "deadlock_detected": true,
	"serialization_failure": true, "query_canceled": true,
	"data_exception": true, "case_not_found": true,
	"undefined_table": true, "undefined_column": true,
	"undefined_function": true, "undefined_object": true,
	"duplicate_table": true, "duplicate_column": true,
	"ambiguous_column":   true,
	"insufficient_privilege": true,
	"invalid_password":       true,
	"feature_not_supported":  true,
	"connection_exception":   true,
	"connection_failure":     true,
	"transaction_rollback":   true,
	"plpgsql_error":          true, "assert_failure": true,
	"no_data": true, "warning": true,
	"successful_completion": true,
}

// isCommentOnlyLine returns true when the trimmed line is entirely a SQL
// comment — either a `--` line comment, a single-line `/* … */` block,
// or the closing `… */` of a multi-line block. These lines can't be the
// terminator of an executable statement, so the handler-semicolon
// normalizer must skip them; appending `;` to a `*/` line creates an
// empty-statement syntax error in PG plpgsql.
func isCommentOnlyLine(s string) bool {
	if strings.HasPrefix(s, "--") {
		return true
	}
	// Single-line `/* … */` block.
	if strings.HasPrefix(s, "/*") && strings.HasSuffix(s, "*/") {
		return true
	}
	// Closing fragment of a multi-line block (no opener on this line).
	if strings.HasSuffix(s, "*/") && !strings.Contains(s, "/*") {
		return true
	}
	return false
}

// rewriteBareRaiseInHandlers rewrites `RAISE <user_exc>;` (with no
// EXCEPTION/NOTICE/DEBUG/WARNING/INFO/LOG keyword between RAISE and the
// name) into PG's `RAISE EXCEPTION '<name>' USING ERRCODE='P0001';`
// form. Oracle uses bare RAISE both to re-raise the current exception
// (`RAISE;` — handled here as a passthrough since PG accepts that
// syntax) and to raise a named user exception (`RAISE my_exc;` — the
// failure mode this rewrite targets). PG plpgsql has no name→SQLSTATE
// binding so the named form would fail to compile; we substitute a
// generic SQLSTATE while preserving the name as the message text so a
// reviewer can spot the original exception class. Handler bodies are
// captured as raw text by the Oracle parser (the EXCEPTION block isn't
// tokenised statement-by-statement) so this is a text-level pass.
func rewriteBareRaiseInHandlers(exc string) string {
	if !strings.Contains(strings.ToUpper(exc), "RAISE ") {
		return exc
	}
	lines := strings.Split(exc, "\n")
	for i, ln := range lines {
		// Find a `RAISE <ident>` token at any depth in the line. We
		// don't need to track string-literal escaping since handler
		// bodies are line-oriented in Oracle and `RAISE` doesn't
		// appear inside strings in production code.
		j := indexFoldKeyword(ln, "RAISE")
		if j < 0 {
			continue
		}
		// Skip whitespace after RAISE.
		k := j + len("RAISE")
		for k < len(ln) && (ln[k] == ' ' || ln[k] == '\t') {
			k++
		}
		if k >= len(ln) || !isIdentByte(ln[k]) {
			continue // bare `RAISE;` — re-raise, leave alone
		}
		nameStart := k
		for k < len(ln) && (isIdentByte(ln[k]) || ln[k] == '.') {
			k++
		}
		name := ln[nameStart:k]
		// Skip if name is a PG RAISE level keyword (NOTICE / DEBUG /
		// INFO / LOG / WARNING / EXCEPTION) — those are valid PG
		// `RAISE <level>` forms and must not be rewritten.
		switch strings.ToUpper(name) {
		case "NOTICE", "DEBUG", "INFO", "LOG", "WARNING", "EXCEPTION", "SQLSTATE":
			continue
		}
		// Build the rewrite: keep everything before RAISE, replace
		// `RAISE <name>` with `RAISE EXCEPTION '<name>' USING
		// ERRCODE='P0001'`, keep the rest of the line.
		var b strings.Builder
		b.WriteString(ln[:j])
		b.WriteString(fmt.Sprintf("RAISE EXCEPTION '%s' USING ERRCODE='P0001'", name))
		b.WriteString(ln[k:])
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

// remapUserNamedWhenClauses rewrites `WHEN <name> [OR <name>...] THEN`
// clauses so that any name PG plpgsql can't recognise becomes `OTHERS`. PG
// has no name→SQLSTATE binding (PRAGMA EXCEPTION_INIT had no equivalent
// either) so a user-declared Oracle exception, a package-qualified name
// like `UTL_FILE.INVALID_PATH`, or an Oracle-only built-in with no PG
// counterpart is a hard parse error. We treat pgKnownExceptionConditions
// as the allow-list and remap anything else to OTHERS — userExc (the set
// of exceptions declared in the surrounding block) is consulted to make
// the audit comment more precise when present. A `-- TODO: was WHEN
// <orig>` trailer on the THEN line lets a reviewer narrow the handler
// later by inspecting SQLSTATE / SQLERRM inside the OTHERS body.
func remapUserNamedWhenClauses(exc string, userExc map[string]bool) string {
	lines := strings.Split(exc, "\n")
	for i, ln := range lines {
		trim := strings.TrimSpace(ln)
		up := strings.ToUpper(trim)
		if !strings.HasPrefix(up, "WHEN ") {
			continue
		}
		// Find the `THEN` keyword (case-insensitive). Everything between
		// the `WHEN ` prefix and ` THEN` is the condition list. We use
		// the original-case line so identifiers keep their casing in the
		// audit comment.
		thenIdx := indexFoldKeyword(ln, "THEN")
		if thenIdx < 0 {
			continue
		}
		// Skip past the leading whitespace + `WHEN ` token.
		whenIdx := indexFoldKeyword(ln, "WHEN")
		if whenIdx < 0 || whenIdx >= thenIdx {
			continue
		}
		condStart := whenIdx + len("WHEN")
		cond := strings.TrimSpace(ln[condStart:thenIdx])
		// Split the condition on OR (case-insensitive). We don't have
		// to be precise about commas / parens because Oracle WHEN
		// clauses are simply `name [OR name ...]`.
		names := splitOnOr(cond)
		// Build the rewritten WHEN list element-by-element. Each name
		// resolves to one of:
		//   1. a PG-known condition       → keep verbatim
		//   2. an orafce-mapped SQLSTATE  → `sqlstate 'XXXXX'`
		//   3. anything else (user excs,  → OTHERS (and the whole
		//      unknown dotted, etc.)         clause collapses to
		//                                    OTHERS since it subsumes
		//                                    the rest).
		var rewritten []string
		hasOthersFallback := false
		hasOrafceMap := false
		for _, n := range names {
			raw := strings.TrimSpace(n)
			lc := strings.ToLower(raw)
			if code, ok := orafceExceptionSQLSTATEs[lc]; ok {
				rewritten = append(rewritten, fmt.Sprintf("sqlstate '%s'", code))
				hasOrafceMap = true
				continue
			}
			if strings.Contains(lc, ".") || userExc[lc] || !pgKnownExceptionConditions[lc] {
				hasOthersFallback = true
				continue
			}
			rewritten = append(rewritten, raw)
		}
		if !hasOthersFallback && !hasOrafceMap {
			// All names PG already accepts as-is.
			continue
		}
		indent := ln[:whenIdx]
		tail := ln[thenIdx+len("THEN"):]
		// If any unmappable name was present, OTHERS subsumes the
		// whole clause and the SQLSTATE-mapped siblings become
		// unreachable — log them in the TODO so the user can split if
		// needed. This is conservative but always parses.
		if hasOthersFallback {
			lines[i] = fmt.Sprintf("%sWHEN OTHERS THEN -- TODO: was WHEN %s%s",
				indent, strings.TrimSpace(cond), tail)
			continue
		}
		// Pure orafce-mapped clause — emit `WHEN sqlstate 'A' OR
		// sqlstate 'B' THEN` with a comment of the original names.
		lines[i] = fmt.Sprintf("%sWHEN %s THEN -- was WHEN %s%s",
			indent, strings.Join(rewritten, " OR "), strings.TrimSpace(cond), tail)
	}
	return strings.Join(lines, "\n")
}

// indexFoldKeyword returns the byte offset of the first whole-word match of
// kw in s, case-insensitively. A "whole word" means surrounded by non-ident
// characters (so `WHENEVER` doesn't match `WHEN`). Returns -1 on no match.
func indexFoldKeyword(s, kw string) int {
	upper := strings.ToUpper(s)
	kw = strings.ToUpper(kw)
	from := 0
	for from < len(upper) {
		i := strings.Index(upper[from:], kw)
		if i < 0 {
			return -1
		}
		i += from
		// boundary check
		before := byte(' ')
		if i > 0 {
			before = s[i-1]
		}
		after := byte(' ')
		if i+len(kw) < len(s) {
			after = s[i+len(kw)]
		}
		if !isIdentByte(before) && !isIdentByte(after) {
			return i
		}
		from = i + 1
	}
	return -1
}

// splitOnOr splits s on the keyword OR (case-insensitive, whole-word). We
// don't need to honor parens or strings since Oracle WHEN clauses are just
// `name [OR name ...]`.
func splitOnOr(s string) []string {
	out := []string{}
	rest := s
	for {
		i := indexFoldKeyword(rest, "OR")
		if i < 0 {
			out = append(out, strings.TrimSpace(rest))
			return out
		}
		out = append(out, strings.TrimSpace(rest[:i]))
		rest = rest[i+len("OR"):]
	}
}

// normalizeExceptionHandlerSemicolons indents each line of a `WHEN … THEN
// body` exception block by two spaces and ensures the final non-empty line
// of every handler body ends with a `;`. Lines internal to a multi-line
// statement (continuation lines) are left untouched so a multi-line
// `RAISE_APPLICATION_ERROR(\n  code,\n  msg)` call doesn't get a stray `;`
// inserted between its arguments.
func normalizeExceptionHandlerSemicolons(exc string) string {
	lines := strings.Split(exc, "\n")
	// Find indices of WHEN-introduced handler boundaries that belong to
	// THIS exception block — i.e. WHEN clauses at the outer-most BEGIN
	// depth. A nested `BEGIN … EXCEPTION WHEN … THEN … END` inside one
	// of our handler bodies has its own WHEN clauses; treating those as
	// our handlers would split the outer body incorrectly and append a
	// stray `;` after `EXCEPTION` (turning the nested block into a
	// syntax error: `... EXCEPTION; WHEN ...`). Track depth via the
	// surface keywords BEGIN/IF/LOOP/CASE (push) and END (pop).
	whenIdx := []int{}
	depth := 0
	for i, ln := range lines {
		// Crude but effective tokeniser: walk uppercase fragments
		// separated by non-ident bytes. Comment / string literals
		// inside a one-line statement could confuse this, but in a
		// captured handler body those are rare and the worst case is
		// missing a `;` — never adding a wrong one.
		up := strings.ToUpper(ln)
		isWhen := false
		j := 0
		for j < len(up) {
			c := up[j]
			if !isIdentByte(c) {
				j++
				continue
			}
			k := j
			for k < len(up) && isIdentByte(up[k]) {
				k++
			}
			tok := up[j:k]
			j = k
			switch tok {
			case "BEGIN", "IF", "LOOP", "CASE":
				depth++
			case "END":
				if depth > 0 {
					depth--
				}
				// Skip optional `IF`/`LOOP`/`CASE` close-form keyword.
				for j < len(up) && !isIdentByte(up[j]) {
					j++
				}
				kk := j
				for kk < len(up) && isIdentByte(up[kk]) {
					kk++
				}
				if kk > j {
					next := up[j:kk]
					if next == "IF" || next == "LOOP" || next == "CASE" {
						j = kk
					}
				}
			case "WHEN":
				if depth == 0 && !isWhen {
					isWhen = true
				}
			}
		}
		if isWhen && strings.HasPrefix(strings.TrimSpace(up), "WHEN ") {
			whenIdx = append(whenIdx, i)
		}
	}
	// For each handler, if its LAST non-empty NON-COMMENT line doesn't
	// end with `;`, append one. Then indent every line by two spaces.
	// We deliberately do NOT touch any other lines — they may be
	// continuations of a multi-line statement.
	//
	// Skipping comment-only lines is essential. Oracle handler bodies
	// often end with a banner block-comment (`/****…****/`); appending
	// a `;` after `*/` would create a literal empty statement (`*/;`)
	// that PG plpgsql rejects with `syntax error at or near ";"`.
	for w, start := range whenIdx {
		end := len(lines)
		if w+1 < len(whenIdx) {
			end = whenIdx[w+1]
		}
		// Find the last non-empty, non-comment line in [start, end).
		lastNonEmpty := -1
		for i := end - 1; i >= start; i-- {
			tr := strings.TrimSpace(lines[i])
			if tr == "" {
				continue
			}
			if isCommentOnlyLine(tr) {
				continue
			}
			lastNonEmpty = i
			break
		}
		if lastNonEmpty < 0 {
			continue
		}
		ln := strings.TrimRight(lines[lastNonEmpty], " \t")
		if !strings.HasSuffix(ln, ";") {
			ln += ";"
		}
		lines[lastNonEmpty] = ln
	}
	for i, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" {
			lines[i] = ""
			continue
		}
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}

// detectCursorIdiom returns the cursor declaration + the done-flag variable
// name if the block has the classical pattern:
//
//	DECLARE done INT DEFAULT 0;
//	DECLARE v_* ...;
//	DECLARE cur CURSOR FOR SELECT ...;
//	DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;
//	OPEN cur;
//	<label>: LOOP
//	  FETCH cur INTO ...;
//	  IF done = 1 THEN LEAVE <label>; END IF;
//	  ...
//	END LOOP;
//	CLOSE cur;
func detectCursorIdiom(blk *ast.Block) (cursor *ast.DeclareCursor, doneVar string, ok bool) {
	var cur *ast.DeclareCursor
	var handlerVar string
	for _, d := range blk.Decls {
		switch x := d.(type) {
		case *ast.DeclareCursor:
			cur = x
		case *ast.DeclareHandler:
			if strings.EqualFold(x.Condition, "NOT FOUND") {
				if as, isAssign := x.Action.(*ast.AssignStmt); isAssign {
					handlerVar = as.Target
				}
			}
		}
	}
	if cur == nil || handlerVar == "" {
		return nil, "", false
	}
	// Need at least an OPEN followed by a LOOP containing a FETCH.
	hasOpen := false
	hasLoop := false
	for _, s := range blk.Stmts {
		switch x := s.(type) {
		case *ast.OpenStmt:
			if x.Cursor == cur.Name {
				hasOpen = true
			}
		case *ast.LoopStmt:
			for _, ls := range x.Body {
				if f, isFetch := ls.(*ast.FetchStmt); isFetch && f.Cursor == cur.Name {
					hasLoop = true
					break
				}
			}
		}
	}
	if !hasOpen || !hasLoop {
		return nil, "", false
	}
	return cur, handlerVar, true
}

// translateCursorBody rewrites the body of a cursor-idiom block into a
// `FOR row IN <select> LOOP ... END LOOP;` form.
func (t *plTranslator) translateCursorBody(blk *ast.Block, cursor *ast.DeclareCursor) []pgast.PLStmt {
	var out []pgast.PLStmt
	for _, s := range blk.Stmts {
		switch x := s.(type) {
		case *ast.OpenStmt, *ast.CloseStmt:
			// absorbed by FOR loop
			continue
		case *ast.LoopStmt:
			// replace with FOR row IN <select> LOOP
			body := t.stmts(filterCursorLoopBody(x.Body, cursor.Name))
			out = append(out, &pgast.PLForQuery{
				Label: x.Label,
				Vars:  []string{"_row"},
				Query: rewriteMySQLBody(cursor.SelectBody),
				Body:  body,
			})
		default:
			_ = x
			if translated := t.stmt(s); translated != nil {
				out = append(out, translated)
			}
		}
	}
	return out
}

// filterCursorLoopBody drops the FETCH, the "IF done THEN LEAVE" guard,
// and any reference to the done flag — those are no longer needed because
// the FOR loop natively iterates until rows are exhausted.
func filterCursorLoopBody(body []ast.PLStmt, cursorName string) []ast.PLStmt {
	var out []ast.PLStmt
	for _, s := range body {
		switch x := s.(type) {
		case *ast.FetchStmt:
			if x.Cursor == cursorName {
				continue
			}
		case *ast.IfStmt:
			// Drop `IF done = 1 THEN LEAVE …;` — heuristic: one branch, one
			// LEAVE statement, no else.
			if len(x.Branches) == 1 && len(x.Branches[0].Body) == 1 && len(x.Else) == 0 {
				if _, isLeave := x.Branches[0].Body[0].(*ast.LeaveStmt); isLeave {
					continue
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// target rewrites an assignment target: NEW.col / OLD.col are kept, @var is
// flagged (untranslated) and emitted as-is so the user can fix it, plain
// identifiers pass through.
func (t *plTranslator) target(tgt string) string {
	if strings.HasPrefix(tgt, "@") {
		t.warn("session variable " + tgt + " (promote to DECLARE …)")
		return tgt
	}
	return tgt
}

// expr renders an expression node as PG text. Delegates to the DML body
// rewriter so function names and identifiers are translated consistently.
func (t *plTranslator) expr(e ast.Expr) string {
	return rewriteMySQLBody(rawExpr(e))
}

// declareVarText produces the `name type [DEFAULT expr];` form for PG's
// DECLARE section. If the variable's type name matches a registered
// Oracle-local TYPE/SUBTYPE, the substitution is honoured — that's how a
// `v arr;` declaration after `TYPE arr IS TABLE OF NUMBER INDEX BY
// BINARY_INTEGER` becomes `v numeric[];` instead of falling back to TEXT.
// collectArraySubscriptTargets walks every PLStmt in `stmts`
// (recursively, descending into IfStmt branches, loops, nested
// blocks) and harvests the base name of every AssignStmt whose
// Target carries a `[…]` subscript suffix. The Oracle parser emits
// `Target: "var[idx]"` for `var(idx) := …` source. We feed the
// collected names to declareVarText so the matching DeclareVar's
// type is bumped to `<type>[]`.
func collectArraySubscriptTargets(stmts []ast.PLStmt, out map[string]bool) {
	for _, s := range stmts {
		switch x := s.(type) {
		case *ast.AssignStmt:
			if i := strings.IndexByte(x.Target, '['); i > 0 {
				name := strings.ToLower(strings.TrimSpace(x.Target[:i]))
				out[name] = true
			}
		case *ast.IfStmt:
			for _, br := range x.Branches {
				collectArraySubscriptTargets(br.Body, out)
			}
			collectArraySubscriptTargets(x.Else, out)
		case *ast.CaseStmt:
			for _, w := range x.When {
				collectArraySubscriptTargets(w.Body, out)
			}
			collectArraySubscriptTargets(x.Else, out)
		case *ast.LoopStmt:
			collectArraySubscriptTargets(x.Body, out)
		case *ast.WhileStmt:
			collectArraySubscriptTargets(x.Body, out)
		case *ast.NumericForStmt:
			collectArraySubscriptTargets(x.Body, out)
		case *ast.CursorForStmt:
			collectArraySubscriptTargets(x.Body, out)
		case *ast.Block:
			collectArraySubscriptTargets(x.Stmts, out)
		}
	}
}

func declareVarText(v *ast.DeclareVar, kind dialects.Kind, oracleTypes map[string]oracleTypeInfo, cursorNames map[string]bool, arrayUsedVars map[string]bool) string {
	typ := ""
	if v.Type != nil {
		// Local Oracle TYPE/SUBTYPE substitution.
		if udt, ok := v.Type.(*ast.UserDefinedType); ok {
			if info, hit := oracleTypes[strings.ToLower(udt.Name)]; hit {
				typ = info.pg
			}
			// Cursor%ROWTYPE: PG plpgsql's %ROWTYPE is restricted
			// to tables and views, so a cursor name there raises
			// `relation "<cursor>" does not exist` at apply time.
			// Substitute RECORD — PG materialises the anonymous
			// record's structure on first FETCH, matching the
			// Oracle semantics for typical usage.
			if typ == "" && len(cursorNames) > 0 && strings.EqualFold(udt.Anchored, "ROWTYPE") {
				name := udt.Name
				if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
					name = name[dot+1:]
				}
				if cursorNames[strings.ToLower(name)] {
					typ = "RECORD"
				}
			}
		}
		if typ == "" {
			typ = MapType(kind, v.Type, v.Name, Caps{}).PG
		}
	} else {
		typ = "text"
	}
	// Array-usage promotion: if the variable name appears anywhere in
	// the routine body as `var[idx] := …`, promote its declared type
	// to `<type>[]` so PG accepts the subscript at runtime. Skip when
	// the type already ends with `[]` (already array) or when the
	// type carries a length modifier the parser bracket scrubber would
	// confuse with array brackets.
	if arrayUsedVars != nil && arrayUsedVars[strings.ToLower(v.Name)] && !strings.HasSuffix(typ, "[]") {
		typ = typ + "[]"
	}
	out := fmt.Sprintf("%s %s", v.Name, typ)
	if v.Default != nil {
		out += " DEFAULT " + rewriteMySQLBody(rawExpr(v.Default))
	}
	out += ";"
	return out
}
