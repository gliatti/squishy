package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// oracle_visitors_funcs.go — Group A AST rewriters for Oracle FuncCall
// and operator-shaped expressions.
//
// Each rewriter is a self-contained ast.Rewriter: takes a Node, returns
// either the same Node (no change) or a substituted Node. Composed via
// ast.Compose by the Phase 3.6 orchestrator (RewriteOracleAST). Until
// the orchestrator wires them in, every rewriter is exercised
// individually by its unit test.
//
// The rewriters target patterns the legacy text pipeline matched via
// strings.Index/Contains/EqualFold scans on rendered SQL. Once a
// matching visitor lands, the corresponding text pass becomes a no-op
// because the AST has already substituted the canonical PG form
// before WriteExpr renders.

// VisitOracleStripSysPrefix strips the leading `SYS.` qualifier from
// FuncCall.Name. PG search_path resolves the unprefixed name in the
// public schema or whichever extension owns the function (orafce maps
// most Oracle SYS.* helpers).
//
// Pattern: FuncCall{Name: "SYS.PKG.FUNC"} → FuncCall{Name: "PKG.FUNC"}.
// Strict prefix match — case-insensitive on the leading "SYS." only;
// downstream parts round-trip verbatim.
//
// Replaces the legacy text pass `rewriteOracleStripSysPrefix` (oracle_
// body_xlate.go L1833) which scanned for word-boundary "SYS." in the
// rendered routine body.
func VisitOracleStripSysPrefix(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok {
		return n
	}
	const prefix = "SYS."
	if len(fc.Name) <= len(prefix) {
		return n
	}
	if !strings.EqualFold(fc.Name[:len(prefix)], prefix) {
		return n
	}
	return &ast.FuncCall{
		Name: fc.Name[len(prefix):],
		Args: fc.Args,
		P:    fc.P,
	}
}

// VisitOracleModOperator rewrites the Oracle infix `MOD` operator to a
// PG `mod(lhs, rhs)` function call. Both surface forms are accepted by
// the lexer (`x MOD y` and `mod(x, y)`); the parser today emits
// BinaryExpr{Op:"MOD"} for the infix case, which PG rejects.
//
// Replaces the legacy text pass `rewriteOracleModOperator`.
func VisitOracleModOperator(n ast.Node) ast.Node {
	bin, ok := ast.IsBinaryOp(n, "MOD")
	if !ok {
		return n
	}
	return ast.BuildFuncCall("mod", bin.Lhs, bin.Rhs)
}

// VisitOracleSysContext rewrites Oracle `sys_context('USERENV', '<key>')`
// to `current_setting('squishy.<key>', true)`. The `, true` arg makes
// PG return NULL for an unset GUC instead of raising — Oracle's
// sys_context is similarly forgiving.
//
// Only the two-arg shape is matched; calls with a third argument
// (length cap, rarely used) fall through to the legacy text pass for
// now.
//
// Replaces the legacy text pass `rewriteSysContext`.
func VisitOracleSysContext(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "sys_context")
	if !ok {
		return n
	}
	if len(fc.Args) != 2 {
		return n
	}
	keyLit, ok := ast.IsLiteralKind(fc.Args[1], "string")
	if !ok {
		return n
	}
	guc := "squishy." + strings.ToLower(keyLit.Text)
	return ast.BuildFuncCall("current_setting",
		ast.BuildStringLit(guc),
		&ast.Literal{Kind: "bool", Text: "true"},
	)
}

// VisitOracleUserenv rewrites the legacy Oracle single-arg userenv()
// pseudo-function. Oracle deprecates userenv() in favour of sys_context;
// the equivalents PG provides differ by key:
//
//	userenv('SESSIONID') → pg_backend_pid()::numeric
//	userenv('LANG')      → current_setting('squishy.lang', true)
//	userenv('CLIENT_INFO') / 'TERMINAL' / 'OSDBA' / others
//	                     → current_setting('squishy.<key>', true)
//
// Anything else falls back to the GUC pattern so the call type-checks
// even if the key doesn't have a meaningful PG counterpart.
func VisitOracleUserenv(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "userenv")
	if !ok {
		return n
	}
	if len(fc.Args) != 1 {
		return n
	}
	keyLit, ok := ast.IsLiteralKind(fc.Args[0], "string")
	if !ok {
		return n
	}
	switch strings.ToUpper(keyLit.Text) {
	case "SESSIONID":
		return &ast.CastExpr{
			Expr: ast.BuildFuncCall("pg_backend_pid"),
			Type: &ast.UserDefinedType{Name: "numeric"},
		}
	}
	guc := "squishy." + strings.ToLower(keyLit.Text)
	return ast.BuildFuncCall("current_setting",
		ast.BuildStringLit(guc),
		&ast.Literal{Kind: "bool", Text: "true"},
	)
}

// VisitOracleQualifyDecode renames bare `decode(...)` calls to
// `oracle.decode(...)`. orafce ships a polymorphic decode at this
// schema; the qualified call resolves regardless of search_path
// ordering. Calls already prefixed with a schema (oracle.decode,
// pkg.decode) are left alone.
//
// A future visitor (Phase 3.5 nested case) may further rewrite
// decode → CASE for shapes orafce can't handle (>7 args). For now the
// qualification is enough to fix the common base case.
//
// Replaces the legacy text pass `qualifyOracleDecode`.
func VisitOracleQualifyDecode(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "decode")
	if !ok {
		return n
	}
	return &ast.FuncCall{
		Name: "oracle.decode",
		Args: fc.Args,
		P:    fc.P,
	}
}

// VisitOracleQualifyToCharSingleArg renames the unqualified single-arg
// `to_char(x)` to `oracle.to_char(x)`. PG's built-in to_char requires
// at least two arguments (value, format); orafce supplies the single-
// argument overload that defaults the format per type.
//
// Two- and three-argument calls are left alone — those map cleanly to
// PG's built-in to_char.
//
// Replaces the legacy text pass `qualifyOracleToCharSingleArg`.
func VisitOracleQualifyToCharSingleArg(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "to_char")
	if !ok {
		return n
	}
	if len(fc.Args) != 1 {
		return n
	}
	return &ast.FuncCall{
		Name: "oracle.to_char",
		Args: fc.Args,
		P:    fc.P,
	}
}

// VisitOracleTrimDbmsSqlParseLanguageArg drops the trailing `language`
// argument from `dbms_sql.parse(cur, stmt, lang)`. orafce's dbms_sql.
// parse only accepts the two-arg shape (cur, stmt); the third arg
// indicates the language flag (1 = native, 2 = V7 compat) which has no
// PG equivalent.
//
// Replaces the legacy text pass `trimOracleDbmsSqlParseLanguageArg`.
func VisitOracleTrimDbmsSqlParseLanguageArg(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "dbms_sql.parse")
	if !ok {
		return n
	}
	if len(fc.Args) != 3 {
		return n
	}
	return &ast.FuncCall{
		Name: fc.Name,
		Args: fc.Args[:2],
		P:    fc.P,
	}
}

// MakeCollectionConstructorVisitor returns an ast.Rewriter that
// rewrites Oracle's "type-name as constructor" idiom into PG's typed
// ARRAY literal:
//
//	type_chaine := type_chaine();             → ARRAY[]::VARCHAR(4000)[]
//	type_chaine := type_chaine('a', 'b');     → ARRAY['a', 'b']::VARCHAR(4000)[]
//
// Oracle lets a TABLE OF / VARRAY's own type-name double as a
// constructor function (`l_v type_chaine := type_chaine();`). PG has
// no such overload — running the unmodified call raises "function
// type_chaine() does not exist" at apply time. The translator already
// harvests TYPE/SUBTYPE declarations into oracleTypes (with kind
// "table_of" + the element PG type), so the visitor matches a
// FuncCall whose Name is a registered collection type and emits the
// equivalent ARRAY[…]::elem[] form.
//
// Match shape:
//   - FuncCall.Name (case-insensitive, after stripping any schema
//     prefix) is a key in oracleTypes with kind == "table_of".
//   - Args are 0..n typed Expr values; rendered via the AST writer.
//
// Visitor closure factory because the oracleTypes map is per-routine
// state held by plTranslator. Callers wire it in after the standard
// RewriteOracleAST chain (or as part of declareVarText's expression
// processing).
func MakeCollectionConstructorVisitor(oracleTypes map[string]struct{ Elem string }) ast.Rewriter {
	if len(oracleTypes) == 0 {
		return func(n ast.Node) ast.Node { return n }
	}
	return func(n ast.Node) ast.Node {
		fc, ok := n.(*ast.FuncCall)
		if !ok {
			return n
		}
		name := fc.Name
		// Strip an optional schema prefix — Oracle source might write
		// `pkg.type_chaine()` to disambiguate.
		if dot := strings.LastIndexByte(name, '.'); dot > 0 {
			name = name[dot+1:]
		}
		info, hit := oracleTypes[strings.ToLower(name)]
		if !hit {
			return n
		}
		// Build "ARRAY[args]::<elem>[]" — RawExpr because the writer
		// handles the verbatim text of the cast cleanly.
		var argText string
		if len(fc.Args) > 0 {
			parts := make([]string, len(fc.Args))
			for i, a := range fc.Args {
				parts[i] = rawExpr(a)
			}
			argText = strings.Join(parts, ", ")
		}
		return &ast.RawExpr{
			Text: "ARRAY[" + argText + "]::" + info.Elem + "[]",
			P:    fc.P,
		}
	}
}

// orafcePackages is the set of Oracle system-package qualifiers that
// land in lowercase un-quoted form on PG (orafce ships these as schemas
// or extension functions). When a FuncCall.Name has dotted segments
// whose first segment matches one of these (case-insensitive),
// VisitOracleQuotedPackageCalls normalises every segment to lowercase
// — Oracle dump tools (DBMS_METADATA, SQL Developer) emit
// `"DBMS_SQL"."PARSE"(...)` with the quoted-uppercase form, which PG
// then matches case-sensitively and fails to find the lowercase orafce
// overload.
var orafcePackages = map[string]bool{
	"DBMS_SQL":     true,
	"DBMS_OUTPUT":  true,
	"DBMS_UTILITY": true,
	"DBMS_RANDOM":  true,
	"DBMS_LOCK":    true,
	"DBMS_PIPE":    true,
	"DBMS_ALERT":   true,
	"DBMS_ASSERT":  true,
	"UTL_FILE":     true,
}

// VisitOracleQuotedPackageCalls normalises function-call names whose
// first segment matches a known Oracle system package. The rewrite
// folds the entire dotted name to lowercase so PG resolves the
// orafce-shipped overload regardless of the source's quoting style:
//
//	"DBMS_SQL"."PARSE"(c, stmt)        → dbms_sql.parse(c, stmt)
//	DBMS_OUTPUT.PUT_LINE('msg')        → dbms_output.put_line('msg')
//	UTL_FILE.FOPEN(rep, 'log.txt','w') → utl_file.fopen(rep, 'log.txt', 'w')
//
// The parser's parsePrimary stores the dotted name verbatim (the
// uppercase from Oracle's case-folding lexer plus any quoted segments
// passed through). The visitor lowercases every segment of the dotted
// path and emits the result as a single lowercase Name — orafce's
// schema (`dbms_sql`) is search_path-resolved, the per-method
// resolution is handled by PG's standard function lookup.
//
// Replaces the legacy text pass `rewriteOracleQuotedPackageCalls`.
func VisitOracleQuotedPackageCalls(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok {
		return n
	}
	dot := strings.IndexByte(fc.Name, '.')
	if dot <= 0 {
		return n
	}
	// Strip surrounding double-quotes (DBMS_METADATA emits the quoted
	// form `"DBMS_SQL"."PARSE"`) before the package-set lookup.
	first := strings.ToUpper(strings.Trim(fc.Name[:dot], `"`))
	if !orafcePackages[first] {
		return n
	}
	// Strip every `"` from every segment, fold to lowercase.
	out := strings.ToLower(strings.ReplaceAll(fc.Name, `"`, ""))
	if out == fc.Name {
		return n
	}
	return &ast.FuncCall{Name: out, Args: fc.Args, P: fc.P}
}

// VisitOracleDbmsUtilityFormatCallStack maps the three Oracle PL/SQL
// diagnostics calls onto their PG equivalents:
//
//	DBMS_UTILITY.FORMAT_CALL_STACK    → dbms_utility.format_call_stack()  (orafce)
//	DBMS_UTILITY.FORMAT_ERROR_STACK   → SQLERRM                           (PG built-in)
//	DBMS_UTILITY.FORMAT_ERROR_BACKTRACE → ''                              (no PG analog)
//
// Oracle accepts the call without parens (`DBMS_UTILITY.FORMAT_CALL_STACK`);
// PG rejects bare references for FUNCTION calls. The legacy text pass
// scanned for the suffix and confirmed the DBMS_UTILITY prefix; the
// visitor uses the typed FuncCall.Name directly (parser produces
// `DBMS_UTILITY.FORMAT_CALL_STACK` in the canonical form even when
// the source omits parens — see parser_expr's FuncCall detection).
//
// SYS-prefixed forms (e.g. `SYS.DBMS_UTILITY.FORMAT_CALL_STACK`) are
// covered because VisitOracleStripSysPrefix runs earlier in the
// orchestration and normalises the Name to drop the `SYS.` segment.
//
// Replaces the legacy text pass `rewriteOracleDbmsUtilityFormatCallStack`.
func VisitOracleDbmsUtilityFormatCallStack(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok {
		return n
	}
	switch strings.ToUpper(fc.Name) {
	case "DBMS_UTILITY.FORMAT_CALL_STACK":
		return &ast.FuncCall{Name: "dbms_utility.format_call_stack", P: fc.P}
	case "DBMS_UTILITY.FORMAT_ERROR_STACK":
		return &ast.Ident{Parts: []string{"SQLERRM"}, P: fc.P}
	case "DBMS_UTILITY.FORMAT_ERROR_BACKTRACE":
		return &ast.Literal{Kind: "string", Text: "", P: fc.P}
	}
	return n
}

// truncDateTriggers is the set of identifier-leading expressions whose
// presence on TRUNC's first argument signals a date input rather than a
// numeric one. Conservative match: only known Oracle date sources count
// as triggering the rewrite — arbitrary numeric expressions are left
// alone so PG's native trunc(numeric, scale) handles them.
var truncDateTriggers = map[string]bool{
	"SYSDATE":           true,
	"SYSTIMESTAMP":      true,
	"CURRENT_DATE":      true,
	"CURRENT_TIMESTAMP": true,
	"LOCALTIMESTAMP":    true,
	"TO_DATE":           true,
	"TO_TIMESTAMP":      true,
	"ADD_MONTHS":        true,
	"TRUNC":             true, // nested TRUNC — still date-shaped.
}

// VisitOracleTrunc rewrites the date-flavoured Oracle TRUNC overload
// to PG's date_trunc:
//
//	TRUNC(SYSDATE)            → date_trunc('day', SYSDATE)::date
//	TRUNC(SYSDATE, 'MM')      → date_trunc('month', SYSDATE)::date
//	TRUNC(d, 'YYYY')          → date_trunc('year', d)::date
//
// Heuristic: only matches when the first argument is a date-shaped
// expression (FuncCall named one of the date-source set, or an Ident
// path matching one). Numeric TRUNC stays unchanged — PG's stock
// trunc(numeric[, scale]) handles those overloads natively.
//
// Replaces the legacy text pass `rewriteOracleTrunc`.
func VisitOracleTrunc(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "trunc")
	if !ok {
		return n
	}
	if len(fc.Args) < 1 || len(fc.Args) > 2 {
		return n
	}
	if !looksLikeDateExprAST(fc.Args[0]) {
		return n
	}
	unit := "day"
	if len(fc.Args) == 2 {
		if lit, ok := ast.IsLiteralKind(fc.Args[1], "string"); ok {
			unit = oracleFmtMaskToPGUnit(strings.ToUpper(lit.Text))
		}
	}
	inner := ast.BuildFuncCall("date_trunc",
		ast.BuildStringLit(unit),
		fc.Args[0],
	)
	return &ast.CastExpr{
		Expr: inner,
		Type: &ast.UserDefinedType{Name: "date"},
	}
}

// looksLikeDateExprAST reports whether expr's leading identifier or
// function-name belongs to truncDateTriggers — i.e. the expression
// produces a date/timestamp value Oracle's TRUNC was operating on.
func looksLikeDateExprAST(expr ast.Expr) bool {
	for {
		switch x := expr.(type) {
		case *ast.ParenExpr:
			expr = x.Inner
		case *ast.FuncCall:
			return truncDateTriggers[strings.ToUpper(x.Name)]
		case *ast.Ident:
			if len(x.Parts) == 0 {
				return false
			}
			return truncDateTriggers[strings.ToUpper(x.Parts[0])]
		default:
			return false
		}
	}
}

// VisitOracleDictionaryViews rewrites references to Oracle data-
// dictionary view identifiers (ALL_OBJECTS, ALL_TABLES, ALL_TAB_COLUMNS,
// …) into PG sub-SELECT expressions that reproduce the observable
// shape Oracle code typically queries. The legacy text pass exposed
// the same set of subqueries via dictViewSubquery; the visitor
// delegates to that table for the actual SQL templates so a single
// source-of-truth rewrites apply both paths during the transition.
//
// Match pattern: a single-part *ast.Ident whose lower-cased name
// matches a key in dictViewSubquery. The visitor wraps the subquery
// text in a *ast.RawExpr — the Postgres writer (and the translator's
// rawExpr) emits RawExpr.Text verbatim, so the substitution flows
// through unchanged. Multi-part references (`SCHEMA.ALL_TABLES`) are
// left alone — those are intentional cross-schema references in the
// source dump and we don't second-guess the user.
//
// Replaces the legacy text pass `rewriteOracleDictionaryViews`.
func VisitOracleDictionaryViews(n ast.Node) ast.Node {
	id, ok := n.(*ast.Ident)
	if !ok || len(id.Parts) != 1 {
		return n
	}
	name := strings.ToUpper(id.Parts[0])
	if sub, ok := dictViewSubquery[name]; ok {
		return &ast.RawExpr{Text: sub, P: id.P}
	}
	return n
}

// VisitOracleHashInIdentLiterals rewrites embedded `#` characters
// inside string-typed Literals when the `#` sits between identifier-
// class bytes. Oracle code routinely builds SQL by concatenation
// (`'MVT#' || var || '#X'`) using `#` as a separator — Oracle accepts
// `#` in unquoted identifiers, PG rejects it outside double-quoted
// names. The visitor flips every such `#` to `_` so the runtime
// EXECUTE produces a PG-valid identifier.
//
// String-scalar operation: this visitor matches *ast.Literal nodes
// (Kind="string") only — the underlying Text is a single scalar
// payload, not a SQL fragment. Per CLAUDE.md the strings.* operations
// here are acceptable because the input is one isolated literal value.
//
// Replaces the legacy text pass `rewriteOracleHashInIdentLiterals`.
func VisitOracleHashInIdentLiterals(n ast.Node) ast.Node {
	lit, ok := n.(*ast.Literal)
	if !ok || lit.Kind != "string" {
		return n
	}
	if !strings.ContainsRune(lit.Text, '#') {
		return n
	}
	src := lit.Text
	// Special case: a Literal whose entire content is just `#` is
	// almost always a concat separator (`'PFX' || '#' || sfx`).
	// The runtime concatenation produces `PFX#sfx` — a PG-invalid
	// unquoted identifier. Replace with `_` so the resulting name
	// is PG-valid.
	if src == "#" {
		return &ast.Literal{Kind: "string", Text: "_", P: lit.P}
	}
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '#' && hashIsBetweenIdentBytes(src, i) {
			b.WriteByte('_')
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()
	if out == src {
		return n
	}
	return &ast.Literal{Kind: "string", Text: out, P: lit.P}
}

// VisitOracleCollectionTokens rewrites a small set of Oracle
// collection-method patterns expressed as FuncCall / Ident shapes:
//
//	TABLE(coll)              → unnest(coll)
//	coll.COUNT               → COALESCE(array_length(coll, 1), 0)
//	coll.FIRST               → 1
//	coll.LAST                → array_length(coll, 1)
//	coll.NEXT(i)             → i + 1
//	coll.PRIOR(i)            → i - 1
//	coll.EXISTS(i)           → (i BETWEEN 1 AND array_length(coll, 1))
//
// Most of these come into the AST as either a FuncCall named "TABLE"
// (for TABLE(...)) or as an Ident path with a method-shaped trailing
// segment ("COUNT" / "FIRST" / "LAST" / etc.). Method-with-args forms
// (NEXT(i), PRIOR(i), EXISTS(i)) parse as FuncCall with a 2-part Name
// where the first segment is the collection variable.
//
// Heuristic: when an Ident path's last segment is one of the no-arg
// collection-method names AND the path has at least 2 parts, rewrite.
// The collection variable might also be a single-part ident in some
// patterns; we only fire when there's a leading collection operand
// (the multi-part case) so a column literally named COUNT in a
// regular query isn't accidentally rewritten.
//
// Replaces the legacy text pass `rewriteOracleCollectionTokens`
// (which lexed the body and walked tokens; the visitor relies on the
// parser's already-typed AST instead).
func VisitOracleCollectionTokens(n ast.Node) ast.Node {
	// TABLE(coll) → unnest(coll).
	if fc, ok := ast.IsFuncCallNamed(n, "TABLE"); ok && len(fc.Args) == 1 {
		return &ast.FuncCall{Name: "unnest", Args: fc.Args, P: fc.P}
	}
	// Method-with-args: coll.NEXT(i) parses as FuncCall("coll.NEXT").
	if fc, ok := n.(*ast.FuncCall); ok {
		dot := strings.LastIndexByte(fc.Name, '.')
		if dot > 0 {
			// Apply the same case-fold rule as VisitOracleIdentCaseFold
			// — Oracle's lexer uppercases unquoted FuncCall names, so
			// the Ident we build from this prefix would otherwise be
			// emitted as `"L_TAB_COMMENTAIRE"` (case-sensitive) while
			// the variable's DECLARE was lowered. The fold here is a
			// scalar string-op on a single ident, which CLAUDE.md
			// explicitly tolerates.
			coll := normalizeOracleIdent(fc.Name[:dot])
			method := strings.ToUpper(fc.Name[dot+1:])
			switch method {
			case "NEXT":
				if len(fc.Args) == 1 {
					return ast.BuildBinary("+", fc.Args[0], ast.BuildIntLit(1))
				}
			case "PRIOR":
				if len(fc.Args) == 1 {
					return ast.BuildBinary("-", fc.Args[0], ast.BuildIntLit(1))
				}
			case "EXISTS":
				if len(fc.Args) == 1 {
					// (i BETWEEN 1 AND array_length(coll, 1))
					return &ast.ParenExpr{
						Inner: &ast.BetweenExpr{
							Expr: fc.Args[0],
							Low:  ast.BuildIntLit(1),
							High: ast.BuildFuncCall("array_length",
								&ast.Ident{Parts: []string{coll}},
								ast.BuildIntLit(1),
							),
						},
					}
				}
			}
		}
	}
	// Method-no-args: coll.COUNT / coll.FIRST / coll.LAST as a 2-part
	// Ident path (the parser stores the method as the second segment).
	if id, ok := n.(*ast.Ident); ok && len(id.Parts) >= 2 {
		method := strings.ToUpper(id.Parts[len(id.Parts)-1])
		coll := strings.Join(id.Parts[:len(id.Parts)-1], ".")
		switch method {
		case "COUNT":
			// COALESCE(array_length(coll, 1), 0) — array_length on an
			// empty/NULL array returns NULL; Oracle's COUNT returns 0.
			return ast.BuildFuncCall("COALESCE",
				ast.BuildFuncCall("array_length",
					&ast.Ident{Parts: []string{coll}},
					ast.BuildIntLit(1),
				),
				ast.BuildIntLit(0),
			)
		case "FIRST":
			return ast.BuildIntLit(1)
		case "LAST":
			return ast.BuildFuncCall("array_length",
				&ast.Ident{Parts: []string{coll}},
				ast.BuildIntLit(1),
			)
		}
	}
	return n
}

// VisitOracleSequenceRef rewrites *ast.SequenceRef (Oracle pseudo-
// column) to a PG nextval/currval function call:
//
//	seq.NEXTVAL                → nextval('seq')
//	schema.seq.CURRVAL         → currval('schema.seq')
//
// The parser produces *SequenceRef when it sees an ident path whose
// last part is NEXTVAL or CURRVAL. PG resolves the embedded sequence
// name through search_path; schema-qualified Oracle refs round-trip
// with the schema preserved in the string literal.
//
// Replaces the legacy text pass `rewriteNextvalCurrval`.
func VisitOracleSequenceRef(n ast.Node) ast.Node {
	ref, ok := n.(*ast.SequenceRef)
	if !ok {
		return n
	}
	name := ref.Name
	if ref.Schema != "" {
		name = ref.Schema + "." + ref.Name
	}
	fn := "nextval"
	if strings.EqualFold(ref.Op, "CURRVAL") {
		fn = "currval"
	}
	return ast.BuildFuncCall(fn, ast.BuildStringLit(name))
}

// VisitOracleTranslateUsing collapses the no-op Oracle
// `TRANSLATE(expr USING <charset>)` form. Oracle's USING-flavoured
// translate switches the result charset to NCHAR_CS or CHAR_CS — both
// roundtrip to the same PG `text` value, so the cast is identity.
// Detection is shape-based: a 2-arg translate whose second argument is
// an Ident whose lone part matches NCHAR_CS or CHAR_CS.
//
// The standard 3-arg `translate(str, from, to)` is left alone — that
// maps cleanly to PG's identical built-in.
//
// Replaces the legacy text pass `rewriteTranslateUsing`.
func VisitOracleTranslateUsing(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "translate")
	if !ok {
		return n
	}
	if len(fc.Args) != 2 {
		return n
	}
	id, ok := fc.Args[1].(*ast.Ident)
	if !ok || len(id.Parts) != 1 {
		return n
	}
	switch strings.ToUpper(id.Parts[0]) {
	case "NCHAR_CS", "CHAR_CS":
		return fc.Args[0]
	}
	return n
}

// VisitOracleDbmsLobSubstr maps `dbms_lob.substr(lob, len, off)` to
// the PG-native `substr(lob, off::int, len::int)` — note the arg
// order swap (PG: start, then length) and the integer casts (PG's
// substr(text, integer, integer) doesn't accept numeric directly,
// and DRSE-style routines declare offset/length variables as
// NUMBER → NUMERIC).
//
// Other dbms_lob functions (read, write, instr, …) are NOT mapped
// here — they have no direct PG counterpart and would need orafce
// or a manual port.
func VisitOracleDbmsLobSubstr(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok || !strings.EqualFold(fc.Name, "dbms_lob.substr") {
		return n
	}
	if len(fc.Args) < 2 || len(fc.Args) > 3 {
		return n
	}
	castInt := func(e ast.Expr) ast.Expr {
		// Wrap as `(<expr>)::int` via RawExpr.
		return &ast.RawExpr{Text: "(" + rawExpr(e) + ")::int"}
	}
	newArgs := []ast.Expr{fc.Args[0]}
	if len(fc.Args) == 3 {
		newArgs = append(newArgs, castInt(fc.Args[2]), castInt(fc.Args[1]))
	} else {
		newArgs = append(newArgs, &ast.Literal{Kind: "number", Text: "1"}, castInt(fc.Args[1]))
	}
	return &ast.FuncCall{Name: "substr", Args: newArgs, P: fc.P}
}

// oracleNoArgsPackageFuncs is the set of no-args package functions
// that Oracle code commonly calls without parens (`x := dbms_sql.
// open_cursor`). PG treats the unparen'd form as a relation
// reference and raises `missing FROM-clause entry`. Each entry is
// matched lowercase on the dotted Ident path.
var oracleNoArgsPackageFuncs = map[string]bool{
	"dbms_sql.open_cursor":     true,
	"dbms_sql.last_row_count":  true,
	"sys.dbms_sql.open_cursor": true,
}

// VisitOracleNoArgsPackageCall promotes a multi-part Ident whose
// dotted form (lowercased) is a key in oracleNoArgsPackageFuncs into
// a FuncCall with empty Args. The writer renders FuncCall with the
// `()` PG needs, so `x := dbms_sql.open_cursor` becomes
// `x := dbms_sql.open_cursor()`.
func VisitOracleNoArgsPackageCall(n ast.Node) ast.Node {
	id, ok := n.(*ast.Ident)
	if !ok || len(id.Parts) < 2 {
		return n
	}
	dotted := strings.ToLower(strings.Join(id.Parts, "."))
	if !oracleNoArgsPackageFuncs[dotted] {
		return n
	}
	return &ast.FuncCall{
		Name: strings.Join(id.Parts, "."),
		Args: nil,
		P:    id.P,
	}
}

// VisitOracleIdentCaseFold lowercases every part of an unquoted Oracle
// Ident so PG-side references match the lowercase form used by
// pgProcSignature when emitting routine parameters.
//
// Background: Oracle's lexer uppercases unquoted identifiers, so the
// parser surfaces e.g. `P_BO_VERSION` (no quotes in source) as an
// Ident{Parts:["P_BO_VERSION"], Backtick:false}. When the postgres
// writer (writeIdent) double-quotes that part, the body emits
// `"P_BO_VERSION"` — case-sensitive — while pgProcSignature
// normalised the matching parameter to `p_bo_version`. PG then raises
// `column "P_BO_VERSION" does not exist` at apply time.
//
// Folding rule: only Backtick==false Idents are touched (the user did
// NOT quote the source ident, so case is non-significant in Oracle).
// Each part is normalised via normalizeOracleIdent (all-caps →
// lowercase, mixed/lowercase preserved). Quoted Idents (Backtick==true)
// keep their exact case so a deliberately-quoted reference like
// `"MyTab"` round-trips intact.
//
// The visitor mutates Parts in place to keep the writer's emission
// path stable — a fresh Ident node would lose Position metadata and
// the Backtick flag, both still useful for downstream visitors and
// error reporting.
func VisitOracleIdentCaseFold(n ast.Node) ast.Node {
	id, ok := n.(*ast.Ident)
	if !ok || id.Backtick {
		return n
	}
	for i, p := range id.Parts {
		id.Parts[i] = normalizeOracleIdent(p)
	}
	return id
}
