// MySQL/MariaDB translation invariant (AST only): source text goes
// through the mysql dialect parser exactly once, the resulting typed
// nodes are rewritten with ast.Rewrite(node, RewriteMySQLAST), and the
// PostgreSQL text is produced by the dialects/postgres writer. When the
// parser rejects a body, the failure is surfaced as a warning and the
// raw source text is copied verbatim; it is never re-lexed or patched
// with string operations. ast_only_guard_test.go enforces this (no
// regexp import, no MySQL lexer call in this package, no legacy text
// rewriter).
package translate

import (
	"strconv"
	"strings"
	"unicode"

	"gitlab.com/dalibo/squishy/internal/dialects"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// mysql_visitors.go — AST rewriters for the MySQL / MariaDB → PG path.
//
// Each Visit* function is a self-contained ast.Rewriter: it takes a
// Node and returns either the same Node (no change) or a freshly built
// replacement. Rewriters never mutate the node they receive — a node
// may be reachable from several parents while a Compose chain runs —
// they copy, then modify the copy. RewriteMySQLAST composes them in the
// order the idioms depend on each other; ast.Rewrite runs the chain in
// post-order, so every child has already been rewritten when a parent
// is visited (JSON_UNQUOTE sees the jsonb_extract_path its argument
// became, DATE_ADD sees the PG-shaped interval its argument became).
//
// Several passes replace a function call (which renders as a primary)
// with an operator expression (DATE_ADD → `+`, JSON_UNQUOTE → `#>>`),
// and `<=>` becomes IS NOT DISTINCT FROM, whose PG precedence is below
// `=`. The PG writer does not parenthesise BinaryExpr operands, so
// VisitMySQLOperandPrecedence runs last in the chain and wraps any
// operand whose PG precedence would otherwise regroup the rendered
// expression. A visitor used outside RewriteMySQLAST must be followed
// by that pass.
//
// These passes replace the removed legacy text pipeline (re-lexing
// rendered SQL with the MySQL lexer, then scanning the text for
// GROUP_CONCAT, JSON_EXTRACT paths, bare JOINs and INTERVAL
// arithmetic). Output is rendered by the dialects/postgres
// writer; no SQL text is inspected here. Name comparisons are
// case-insensitive and only ever touch a single identifier
// (FuncCall.Name, IntervalLit.Unit).

// RewriteMySQLAST is the MySQL → PG rewrite pipeline, applied with
// ast.Rewrite to any expression, DML statement, view body or routine
// statement parsed by internal/dialects/mysql.
var RewriteMySQLAST = ast.Compose(
	VisitMySQLFuncRenames,
	VisitMySQLTimeFormat,
	VisitMySQLGroupConcat,
	VisitMySQLJSONExtract,
	VisitMySQLNullSafeEq,
	VisitMySQLBareJoin,
	VisitMySQLInterval,
	VisitMySQLDateAddSub,
	VisitMySQLCastType,
	VisitMySQLOperandPrecedence,
)

// applyMySQLASTRewrites runs RewriteMySQLAST over every statement of a
// parsed MySQL routine body. Mirrors applyOracleASTRewritesWith: a
// statement the pipeline would replace with a non-PLStmt (never the
// case today) is kept as is.
func applyMySQLASTRewrites(stmts []ast.PLStmt) []ast.PLStmt {
	if len(stmts) == 0 {
		return stmts
	}
	out := make([]ast.PLStmt, len(stmts))
	for i, s := range stmts {
		if rs, ok := ast.Rewrite(s, RewriteMySQLAST).(ast.PLStmt); ok {
			out[i] = rs
		} else {
			out[i] = s
		}
	}
	return out
}

// rewriteMySQLSelect runs RewriteMySQLAST over a SELECT (view body,
// cursor query, SELECT … INTO). nil in, nil out.
func rewriteMySQLSelect(sel *ast.SelectStmt) *ast.SelectStmt {
	if sel == nil {
		return nil
	}
	if rs, ok := ast.Rewrite(sel, RewriteMySQLAST).(*ast.SelectStmt); ok {
		return rs
	}
	return sel
}

// rewriteMySQLExpr runs RewriteMySQLAST over a single expression
// (DEFAULT clause, RETURN value, event schedule, …). nil in, nil out.
func rewriteMySQLExpr(e ast.Expr) ast.Expr {
	if e == nil {
		return nil
	}
	if re, ok := ast.Rewrite(e, RewriteMySQLAST).(ast.Expr); ok {
		return re
	}
	return e
}

// VisitMySQLNullSafeEq rewrites MySQL's null-safe equality operator:
//
//	a <=> b   →   a IS NOT DISTINCT FROM b
//
// Both forms are TRUE when both sides are NULL and never return NULL.
func VisitMySQLNullSafeEq(n ast.Node) ast.Node {
	bin, ok := n.(*ast.BinaryExpr)
	if !ok || bin.Op != "<=>" {
		return n
	}
	cp := *bin
	cp.Op = "IS NOT DISTINCT FROM"
	return &cp
}

// VisitMySQLFuncRenames maps MySQL built-ins whose PG counterpart only
// differs by name (or by a dropped argument):
//
//	IFNULL(a, b)                        → coalesce(a, b)
//	CHAR_LENGTH(s) / CHARACTER_LENGTH(s) → length(s)
//	NOW / CURRENT_TIMESTAMP / LOCALTIMESTAMP / SYSDATE ([fsp]) → now()
//	UUID()                              → gen_random_uuid()
//	CONCAT(…)                           → concat(…)
//	TRUNCATE(x, d)                      → trunc(x, d)
//	CURDATE()                           → CURRENT_DATE
//	CURTIME([fsp])                      → CURRENT_TIME[(fsp)]
//
// The optional fractional-seconds precision argument of NOW(fsp) and
// friends is dropped: now() takes no argument and already carries
// microsecond precision (MySQL's maximum), so the value is never less
// precise than the source asked for. SYSDATE() evaluates at execution
// time in MySQL whereas now() is the transaction start; for routine
// bodies that is the historical squishy mapping and it is kept.
// CURRENT_DATE / CURRENT_TIME are left untouched; the PG writer renders
// the zero-argument forms bare. Every other call is left untouched.
func VisitMySQLFuncRenames(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok {
		return n
	}
	switch strings.ToUpper(fc.Name) {
	case "IFNULL":
		return renameMySQLFuncCall(fc, "coalesce")
	case "CHAR_LENGTH", "CHARACTER_LENGTH":
		return renameMySQLFuncCall(fc, "length")
	case "NOW", "CURRENT_TIMESTAMP", "LOCALTIMESTAMP", "SYSDATE":
		out := renameMySQLFuncCall(fc, "now")
		out.Args = nil
		return out
	case "UUID":
		if len(fc.Args) != 0 {
			return n
		}
		return renameMySQLFuncCall(fc, "gen_random_uuid")
	case "CONCAT":
		if fc.Name == "concat" {
			return n
		}
		return renameMySQLFuncCall(fc, "concat")
	case "TRUNCATE":
		if len(fc.Args) != 2 {
			return n
		}
		return renameMySQLFuncCall(fc, "trunc")
	case "CURDATE":
		if len(fc.Args) != 0 {
			return n
		}
		return renameMySQLFuncCall(fc, "CURRENT_DATE")
	case "CURTIME":
		if len(fc.Args) > 1 {
			return n
		}
		return renameMySQLFuncCall(fc, "CURRENT_TIME")
	}
	return n
}

// renameMySQLFuncCall returns a copy of fc carrying the new name. The Args
// slice is copied too: ast.Rewrite reassigns argument slots in place,
// and the copy must not alias the original's backing array.
func renameMySQLFuncCall(fc *ast.FuncCall, name string) *ast.FuncCall {
	cp := *fc
	cp.Name = name
	if fc.Args != nil {
		cp.Args = append([]ast.Expr(nil), fc.Args...)
	}
	return &cp
}

// VisitMySQLTimeFormat rewrites the MySQL time/day formatting built-ins
// that have no PG function of the same name:
//
//	TIME_FORMAT(t, '%H:%i')  → to_char(CAST(CAST(t AS time) AS interval), 'HH24:MI')
//	DAYNAME(d)               → to_char(CAST(d AS date), 'FMDay')
//	MONTHNAME(d)             → to_char(CAST(d AS date), 'FMMonth')
//
// TIME_FORMAT formats a TIME value. PG has no to_char(time); the value
// is cast to time first (MySQL TIME columns map to PG TIME, and MySQL's
// CURRENT_TIME / CURTIME() are rendered as PG CURRENT_TIME, a
// `time with time zone` that has no direct cast to interval) and then to
// interval, whose to_char accepts every hour/minute/second pattern
// TIME_FORMAT allows. Only a string-literal format made of the
// specifiers TIME_FORMAT documents (see mysqlTimeFormatToPG) is
// rewritten; any other call is left untouched and reported by
// mysqlResidualIdioms.
//
// DAYNAME / MONTHNAME return the English name (MySQL's default
// lc_time_names); PG's `Day` / `Month` patterns without the TM prefix
// are English too, and FM drops the blank padding to the longest name.
// The argument is cast to date so a string literal argument resolves.
func VisitMySQLTimeFormat(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok {
		return n
	}
	switch strings.ToUpper(fc.Name) {
	case "TIME_FORMAT":
		if len(fc.Args) != 2 {
			return n
		}
		lit, ok := ast.IsLiteralKind(fc.Args[1], "string")
		if !ok {
			return n
		}
		pgFmt, ok := mysqlTimeFormatToPG(lit.Text)
		if !ok {
			return n
		}
		asTime := &ast.CastExpr{Expr: fc.Args[0], Type: &ast.PGType{Name: "time"}}
		asInterval := &ast.CastExpr{Expr: asTime, Type: &ast.PGType{Name: "interval"}}
		return &ast.FuncCall{
			Name: "to_char",
			Args: []ast.Expr{asInterval, &ast.Literal{Kind: "string", Text: pgFmt, P: lit.P}},
			P:    fc.P,
		}
	case "DAYNAME", "MONTHNAME":
		if len(fc.Args) != 1 {
			return n
		}
		pattern := "FMDay"
		if strings.EqualFold(fc.Name, "MONTHNAME") {
			pattern = "FMMonth"
		}
		asDate := &ast.CastExpr{Expr: fc.Args[0], Type: &ast.PGType{Name: "date"}}
		return &ast.FuncCall{
			Name: "to_char",
			Args: []ast.Expr{asDate, ast.BuildStringLit(pattern)},
			P:    fc.P,
		}
	}
	return n
}

// mysqlTimeFormatSpecs maps the TIME_FORMAT specifiers (the character
// after '%') to their PG to_char pattern. TIME_FORMAT only documents the
// hour / minute / second / microsecond specifiers; the date specifiers
// yield NULL or 0 in MySQL and have no counterpart on a PG interval, so
// they are absent and make mysqlTimeFormatToPG refuse the format.
var mysqlTimeFormatSpecs = map[rune]string{
	'H': "HH24",
	'k': "FMHH24",
	'h': "HH12",
	'I': "HH12",
	'l': "FMHH12",
	'i': "MI",
	'S': "SS",
	's': "SS",
	'f': "US",
	'p': "AM",
	'r': "HH12:MI:SS AM",
	'T': "HH24:MI:SS",
}

// mysqlTimeFormatToPG converts the content of a TIME_FORMAT format
// string literal (a tiny formatting language, not SQL) into a PG to_char
// pattern, scanning it rune by rune:
//
//	format := (spec | text)*
//	spec   := '%' <one of mysqlTimeFormatSpecs> | '%%'
//	text   := any run of runes other than '%'
//
// Literal text is copied verbatim when it holds no letter (':' '-' ' '
// and digits are not PG patterns); a run holding letters, '"' or '\' is
// wrapped in double quotes with '"' and '\' backslash-escaped, so PG
// never reads it as a pattern. `%%` is a literal percent sign. A
// trailing lone '%' or an unsupported specifier returns ok=false.
func mysqlTimeFormatToPG(format string) (string, bool) {
	var out, text []rune
	flush := func() {
		if len(text) == 0 {
			return
		}
		quote := false
		for _, r := range text {
			if unicode.IsLetter(r) || r == '"' || r == '\\' {
				quote = true
				break
			}
		}
		if !quote {
			out = append(out, text...)
		} else {
			out = append(out, '"')
			for _, r := range text {
				if r == '"' || r == '\\' {
					out = append(out, '\\')
				}
				out = append(out, r)
			}
			out = append(out, '"')
		}
		text = text[:0]
	}
	rs := []rune(format)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '%' {
			text = append(text, rs[i])
			continue
		}
		if i+1 >= len(rs) {
			return "", false
		}
		i++
		if rs[i] == '%' {
			text = append(text, '%')
			continue
		}
		pg, ok := mysqlTimeFormatSpecs[rs[i]]
		if !ok {
			return "", false
		}
		flush()
		out = append(out, []rune(pg)...)
	}
	flush()
	return string(out), true
}

// VisitMySQLGroupConcat rewrites MySQL's GROUP_CONCAT aggregate into
// PG's string_agg:
//
//	GROUP_CONCAT([DISTINCT] e [ORDER BY …] [SEPARATOR s])
//	  → string_agg([DISTINCT] CAST(e AS text), s [ORDER BY …])
//	GROUP_CONCAT(a, b, …)
//	  → string_agg(CAST(concat(a, b, …) AS text), ',')
//
// The default separator is ',' (MySQL's default). The CAST is required
// because string_agg only accepts text/bytea while GROUP_CONCAT
// stringifies any type.
//
// Caveat carried over from the source semantics: PG rejects
// `DISTINCT … ORDER BY k` when k is not one of the aggregate's
// arguments ("in an aggregate with DISTINCT, ORDER BY expressions must
// appear in argument list"). Because the argument becomes
// CAST(e AS text), GROUP_CONCAT(DISTINCT e ORDER BY e) renders a form
// PG refuses at apply time — a loud failure, not a silent reorder —
// and mysqlResidualIdioms flags every DISTINCT + ORDER BY string_agg so
// the user sees it before apply.
func VisitMySQLGroupConcat(n ast.Node) ast.Node {
	fc, ok := ast.IsFuncCallNamed(n, "GROUP_CONCAT")
	if !ok || len(fc.Args) == 0 {
		return n
	}
	var e ast.Expr
	if len(fc.Args) == 1 {
		e = fc.Args[0]
	} else {
		e = ast.BuildFuncCall("concat", append([]ast.Expr(nil), fc.Args...)...)
	}
	sep := fc.AggSeparator
	if sep == nil {
		sep = ast.BuildStringLit(",")
	}
	return &ast.FuncCall{
		Name:     "string_agg",
		Distinct: fc.Distinct,
		Args: []ast.Expr{
			&ast.CastExpr{Expr: e, Type: &ast.PGType{Name: "text"}},
			sep,
		},
		AggOrderBy: append([]ast.OrderItem(nil), fc.AggOrderBy...),
		P:          fc.P,
	}
}

// VisitMySQLJSONExtract rewrites the MySQL JSON path accessors into the
// jsonb_* family (MySQL JSON columns are mapped to JSONB, see
// types_map_mysql.go):
//
//	JSON_EXTRACT(x, '$.a.b[0]')   → jsonb_extract_path(x, 'a', 'b', '0')
//	JSON_UNQUOTE(jsonb_extract_path(x, …))
//	                              → jsonb_extract_path_text(x, …)
//	JSON_UNQUOTE(other)           → other #>> '{}'
//
// Only a two-argument JSON_EXTRACT whose path is a string literal made
// of `.name` and `[N]` legs is rewritten; wildcards, `**`, quoted keys,
// `last` and ranges have no jsonb_extract_path counterpart, so the call
// is left untouched and reported by mysqlResidualIdioms. Post-order
// traversal guarantees a JSON_UNQUOTE sees its argument already
// rewritten.
func VisitMySQLJSONExtract(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok {
		return n
	}
	switch strings.ToUpper(fc.Name) {
	case "JSON_EXTRACT":
		if len(fc.Args) != 2 {
			return n
		}
		lit, ok := ast.IsLiteralKind(fc.Args[1], "string")
		if !ok {
			return n
		}
		segs, ok := parseMySQLJSONPath(lit.Text)
		if !ok || len(segs) == 0 {
			return n
		}
		args := make([]ast.Expr, 0, len(segs)+1)
		args = append(args, fc.Args[0])
		for _, s := range segs {
			args = append(args, ast.BuildStringLit(s))
		}
		return &ast.FuncCall{Name: "jsonb_extract_path", Args: args, P: fc.P}
	case "JSON_UNQUOTE":
		if len(fc.Args) != 1 {
			return n
		}
		arg := fc.Args[0]
		if inner, ok := ast.IsFuncCallNamed(arg, "jsonb_extract_path"); ok {
			return renameMySQLFuncCall(inner, "jsonb_extract_path_text")
		}
		return ast.BuildBinary("#>>", parenIfCompound(arg), ast.BuildStringLit("{}"))
	}
	return n
}

// parseMySQLJSONPath decodes a MySQL JSON path such as `$.a.b[0]` into
// its legs ("a", "b", "0"). The input is the content of a JSON-path
// string literal — a tiny path language, not SQL — so it is scanned
// rune by rune:
//
//	path := '$' leg*
//	leg  := '.' name | '[' digits ']'
//	name := (letter | digit | '_')+
//
// Array indices are emitted as their decimal text: jsonb_extract_path
// accepts numeric strings as array subscripts. Anything outside that
// grammar (`*`, `**`, quoted keys, `last`, `to` ranges, empty legs,
// whitespace) returns ok=false and the caller leaves the call alone.
func parseMySQLJSONPath(path string) ([]string, bool) {
	rs := []rune(path)
	if len(rs) == 0 || rs[0] != '$' {
		return nil, false
	}
	segs := []string{}
	i := 1
	for i < len(rs) {
		switch rs[i] {
		case '.':
			i++
			start := i
			for i < len(rs) && (unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i]) || rs[i] == '_') {
				i++
			}
			if i == start {
				return nil, false
			}
			segs = append(segs, string(rs[start:i]))
		case '[':
			i++
			start := i
			for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
				i++
			}
			if i == start || i >= len(rs) || rs[i] != ']' {
				return nil, false
			}
			segs = append(segs, string(rs[start:i]))
			i++
		default:
			return nil, false
		}
	}
	return segs, true
}

// VisitMySQLBareJoin rewrites an INNER JOIN without ON / USING into a
// CROSS JOIN. MySQL accepts `a JOIN b` (and mysqldump emits view bodies
// as `from ((a join b) join c) where …`) while PG requires a join
// condition on INNER JOIN. The semantics are identical: the predicates
// live in WHERE.
func VisitMySQLBareJoin(n ast.Node) ast.Node {
	j, ok := n.(*ast.FromJoin)
	if !ok || j.Kind != ast.InnerJoin || j.On != nil || len(j.Using) > 0 || j.Natural {
		return n
	}
	cp := *j
	cp.Kind = ast.CrossJoin
	return &cp
}

// mysqlSimpleIntervalUnits maps the single-field MySQL interval units
// (upper-case, singular) to the PG interval-string unit word.
var mysqlSimpleIntervalUnits = map[string]string{
	"MICROSECOND": "microsecond",
	"SECOND":      "second",
	"MINUTE":      "minute",
	"HOUR":        "hour",
	"DAY":         "day",
	"WEEK":        "week",
	"MONTH":       "month",
	"QUARTER":     "quarter",
	"YEAR":        "year",
}

// mysqlIntervalUnit normalises a MySQL interval unit: upper-cased, a
// trailing plural `S` stripped (`HOURS` → `HOUR`). Returns the PG unit
// word and ok=false for compound units (YEAR_MONTH, DAY_HOUR, …) and
// anything unknown.
func mysqlIntervalUnit(unit string) (string, bool) {
	u := strings.ToUpper(unit)
	if pg, ok := mysqlSimpleIntervalUnits[u]; ok {
		return pg, true
	}
	if len(u) > 1 && u[len(u)-1] == 'S' {
		if pg, ok := mysqlSimpleIntervalUnits[u[:len(u)-1]]; ok {
			return pg, true
		}
	}
	return "", false
}

// VisitMySQLInterval rewrites MySQL's `INTERVAL <quantity> <unit>` into
// PG interval values:
//
//	INTERVAL 1 HOUR        → INTERVAL '1 hour'
//	INTERVAL 2 QUARTER     → INTERVAL '6 months'   (PG has no quarter)
//	INTERVAL n DAY         → "n" * INTERVAL '1 day'
//	INTERVAL (x + 1) DAY   → (x + 1) * INTERVAL '1 day'
//
// A trailing plural `S` on the unit is tolerated. Compound units
// (YEAR_MONTH, DAY_HOUR, …) are left untouched — their quantity is a
// formatted string whose split PG cannot express in one literal — and
// are reported by mysqlResidualIdioms.
//
// The MySQL parser keeps only literal quantities as text in Value
// (numbers, strings, negated numbers); identifiers and every other
// quantity arrive typed in Expr. A numeric Value becomes the interval
// string; a non-numeric literal Value (e.g. the string '1:30' or 'abc'
// with a single-field unit) is left untouched and reported by
// mysqlResidualIdioms instead of being guessed at.
func VisitMySQLInterval(n ast.Node) ast.Node {
	iv, ok := n.(*ast.IntervalLit)
	if !ok || iv.Unit == "" {
		return n
	}
	unit, ok := mysqlIntervalUnit(iv.Unit)
	if !ok {
		return n
	}
	months := unit == "quarter"
	oneUnit := "1 " + unit
	if months {
		oneUnit = "3 months"
	}
	switch {
	case iv.Expr != nil && iv.Value == "":
		return ast.BuildBinary("*", parenIfCompound(iv.Expr), &ast.IntervalLit{Value: oneUnit, P: iv.P})
	case isMySQLIntervalNumber(iv.Value):
		if months {
			q, err := strconv.ParseInt(iv.Value, 10, 64)
			if err != nil {
				// Decimal quarter count: keep the arithmetic in PG.
				return ast.BuildBinary("*", &ast.Literal{Kind: "number", Text: iv.Value},
					&ast.IntervalLit{Value: oneUnit, P: iv.P})
			}
			return &ast.IntervalLit{Value: strconv.FormatInt(q*3, 10) + " months", P: iv.P}
		}
		return &ast.IntervalLit{Value: iv.Value + " " + unit, P: iv.P}
	}
	return n
}

// isMySQLIntervalNumber reports whether v is a plain signed integer or
// decimal number (`1`, `-2`, `1.5`, `.5`). v is the quantity of a single
// interval literal, scanned rune by rune.
func isMySQLIntervalNumber(v string) bool {
	rs := []rune(v)
	i := 0
	if i < len(rs) && (rs[i] == '-' || rs[i] == '+') {
		i++
	}
	digits, dot := 0, false
	for ; i < len(rs); i++ {
		switch {
		case rs[i] >= '0' && rs[i] <= '9':
			digits++
		case rs[i] == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits > 0
}

// parenIfCompound wraps an operand in parentheses unless it already
// renders as a single primary (identifier, literal, call, CAST, …), so
// placing it next to a higher-precedence operator keeps its grouping.
func parenIfCompound(e ast.Expr) ast.Expr {
	switch e.(type) {
	case *ast.Ident, *ast.Literal, *ast.FuncCall, *ast.ParenExpr, *ast.CastExpr,
		*ast.CaseExpr, *ast.SubqueryExpr, *ast.WindowedAgg, *ast.IntervalLit:
		return e
	}
	return ast.BuildParen(e)
}

// VisitMySQLDateAddSub rewrites MySQL's date arithmetic functions into
// PG operators once their interval argument is PG-shaped:
//
//	DATE_ADD(d, INTERVAL 1 DAY) / ADDDATE(…)  → d + INTERVAL '1 day'
//	DATE_SUB(d, INTERVAL n MONTH) / SUBDATE(…) → d - "n" * INTERVAL '1 month'
//
// Post-order traversal means VisitMySQLInterval has already rewritten
// the second argument; the call is only rewritten when that argument is
// an interval value (IntervalLit without a MySQL unit) or the
// `n * INTERVAL '…'` product. ADDDATE(d, n) (a day count) and compound
// units are left untouched and reported by mysqlResidualIdioms.
//
// The call rendered as a primary; the operator it becomes does not.
// VisitMySQLOperandPrecedence (last in RewriteMySQLAST) parenthesises
// the result when its parent would otherwise regroup it, e.g.
// `x - DATE_SUB(d, INTERVAL 1 DAY)` → `"x" - ("d" - INTERVAL '1 day')`.
func VisitMySQLDateAddSub(n ast.Node) ast.Node {
	fc, ok := n.(*ast.FuncCall)
	if !ok || len(fc.Args) != 2 {
		return n
	}
	op := ""
	switch strings.ToUpper(fc.Name) {
	case "DATE_ADD", "ADDDATE":
		op = "+"
	case "DATE_SUB", "SUBDATE":
		op = "-"
	default:
		return n
	}
	if !isPGIntervalValue(fc.Args[1]) {
		return n
	}
	lhs := fc.Args[0]
	if bin, ok := lhs.(*ast.BinaryExpr); ok && !isAdditiveOrMultiplicative(bin.Op) {
		lhs = ast.BuildParen(lhs)
	}
	return &ast.BinaryExpr{Op: op, Lhs: lhs, Rhs: fc.Args[1], P: fc.P}
}

// isPGIntervalValue reports whether e is an interval already rewritten
// into PG form by VisitMySQLInterval.
func isPGIntervalValue(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.IntervalLit:
		return x.Unit == "" && x.Expr == nil && x.Value != ""
	case *ast.BinaryExpr:
		if x.Op != "*" {
			return false
		}
		iv, ok := x.Rhs.(*ast.IntervalLit)
		return ok && iv.Unit == "" && iv.Expr == nil && iv.Value != ""
	}
	return false
}

// isAdditiveOrMultiplicative reports whether op binds at least as
// tightly as the `+` / `-` the date rewrite introduces, so the left
// operand needs no parentheses.
func isAdditiveOrMultiplicative(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%":
		return true
	}
	return false
}

// VisitMySQLCastType maps the target type of a CAST to its PG form:
//
//	CAST(x AS CHAR)      → CAST(x AS text)
//	CAST(x AS CHAR(10))  → CAST(x AS varchar(10))
//	CAST(x AS UNSIGNED)  → CAST(x AS NUMERIC(20,0))   (MySQL type mapper)
//	CAST(x AS DATETIME)  → CAST(x AS TIMESTAMP)       (MySQL type mapper)
//
// CHAR / NCHAR are special-cased. As a CAST target, MySQL's CHAR is an
// unbounded variable-length string and CHAR(N) a variable-length string
// truncated to N characters. The column-type mapper would turn a
// length-less CHAR into char(1) — PG silently truncates an explicit cast
// to char(1), so CONCAT('id-', CAST(123 AS CHAR)) would yield 'id-1' —
// and PG char(N) blank-pads, so neither mapping is used here. Every
// other type goes through the MySQL column-type mapper. The rewritten
// target is an *ast.PGType; a CastExpr whose type is already one (built
// by another visitor, e.g. VisitMySQLGroupConcat) is left alone.
func VisitMySQLCastType(n ast.Node) ast.Node {
	c, ok := n.(*ast.CastExpr)
	if !ok || c.Type == nil {
		return n
	}
	var name string
	switch t := c.Type.(type) {
	case *ast.PGType, *ast.UserDefinedType:
		return n
	case *ast.CharType:
		name = mysqlCastCharType(t.HasLength, t.Length)
	case *ast.NCharType:
		name = mysqlCastCharType(t.HasLength, t.Length)
	default:
		name = MapType(dialects.KindMySQL, c.Type, "", Caps{}).PG
	}
	cp := *c
	cp.Type = &ast.PGType{Name: name, P: c.Type.Pos()}
	return &cp
}

// mysqlCastCharType returns the PG type for a MySQL CAST(… AS CHAR[(N)])
// target: text without a (positive) length, varchar(N) with one.
func mysqlCastCharType(hasLength bool, length int) string {
	if !hasLength || length <= 0 {
		return "text"
	}
	return "varchar(" + strconv.Itoa(length) + ")"
}

// PG operator precedence levels, loosest to tightest binding (PostgreSQL
// documentation, "Operator Precedence"; the %left / %nonassoc lines of
// gram.y).
const (
	pgPrecOr      = 1  // OR
	pgPrecAnd     = 2  // AND
	pgPrecNot     = 3  // NOT (prefix)
	pgPrecIs      = 4  // IS [NOT] NULL / DISTINCT FROM / TRUE …
	pgPrecCmp     = 5  // = <> < <= > >=
	pgPrecLike    = 6  // [NOT] LIKE / ILIKE / SIMILAR / BETWEEN / IN
	pgPrecOther   = 7  // any other operator: || #>> -> @> & | …
	pgPrecAdd     = 8  // binary + -
	pgPrecMul     = 9  // * / %
	pgPrecExp     = 10 // ^
	pgPrecUnary   = 11 // prefix + - (and other prefix operators)
	pgPrecPrimary = 100
)

// pgBinaryOpPrec returns the PG precedence of a BinaryExpr operator as
// the PG writer renders it (DIV is written `/`; MySQL `||` / `&&` are
// written verbatim and therefore parse as generic PG operators). The
// operator is a single token (or a fixed keyword sequence built by a
// visitor), not SQL text.
func pgBinaryOpPrec(op string) int {
	switch strings.ToUpper(op) {
	case "OR":
		return pgPrecOr
	case "AND":
		return pgPrecAnd
	case "IS", "IS NOT", "IS DISTINCT FROM", "IS NOT DISTINCT FROM":
		return pgPrecIs
	case "=", "<>", "!=", "<", "<=", ">", ">=", "<=>":
		return pgPrecCmp
	case "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE", "IN", "NOT IN", "SIMILAR TO", "NOT SIMILAR TO":
		return pgPrecLike
	case "+", "-":
		return pgPrecAdd
	case "*", "/", "%", "DIV", "MOD":
		return pgPrecMul
	case "^":
		return pgPrecExp
	}
	return pgPrecOther
}

// pgExprPrec returns the precedence e renders at in PG output. Nodes the
// writer renders self-delimited (identifiers, literals, calls, CAST,
// CASE, parenthesised and subquery forms, …) are primaries.
func pgExprPrec(e ast.Expr) int {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		return pgBinaryOpPrec(x.Op)
	case *ast.UnaryExpr:
		if strings.EqualFold(x.Op, "NOT") {
			return pgPrecNot
		}
		return pgPrecUnary
	case *ast.BetweenExpr, *ast.InExpr:
		return pgPrecLike
	}
	return pgPrecPrimary
}

// pgPrecNonAssoc reports whether PG declares the level %nonassoc
// (`a = b = c` is a syntax error), so an equal-level left operand needs
// parentheses too.
func pgPrecNonAssoc(prec int) bool {
	return prec == pgPrecIs || prec == pgPrecCmp || prec == pgPrecLike
}

// parenIf wraps e in parentheses when cond holds and reports whether it
// did.
func parenIf(e ast.Expr, cond bool) (ast.Expr, bool) {
	if !cond || e == nil {
		return e, false
	}
	return ast.BuildParen(e), true
}

// VisitMySQLOperandPrecedence parenthesises operator operands whose PG
// precedence would regroup the rendered expression. The tree is the
// source of truth for grouping (the MySQL parser keeps explicit
// parentheses as ParenExpr and builds the rest from MySQL precedence),
// but the PG writer renders BinaryExpr / UnaryExpr operands bare, and
// the passes above introduce operators where calls used to be:
//
//	x - DATE_SUB(d, INTERVAL 1 DAY)  → "x" - ("d" - INTERVAL '1 day')
//	JSON_UNQUOTE(m) + 1              → ("m" #>> '{}') + 1
//	a <=> b = c                      → ("a" IS NOT DISTINCT FROM "b") = "c"
//
// Rules, on PG levels (see pgBinaryOpPrec): a left operand is wrapped
// when it binds looser than its parent, or equally on a %nonassoc
// level; a right operand is wrapped when it binds looser or equally
// (every PG binary level is left-associative or non-associative).
// Prefix-operator, BETWEEN and IN operands follow the same idea. For a
// tree whose MySQL grouping already matches PG precedence the pass is a
// no-op. It must run after every other MySQL pass on the same node,
// i.e. last in RewriteMySQLAST.
func VisitMySQLOperandPrecedence(n ast.Node) ast.Node {
	switch x := n.(type) {
	case *ast.BinaryExpr:
		p := pgBinaryOpPrec(x.Op)
		lp, rp := pgExprPrec(x.Lhs), pgExprPrec(x.Rhs)
		lhs, lw := parenIf(x.Lhs, lp < p || (lp == p && pgPrecNonAssoc(p)))
		rhs, rw := parenIf(x.Rhs, rp <= p)
		if !lw && !rw {
			return n
		}
		cp := *x
		cp.Lhs, cp.Rhs = lhs, rhs
		return &cp
	case *ast.UnaryExpr:
		var wrap bool
		if strings.EqualFold(x.Op, "NOT") {
			wrap = pgExprPrec(x.Rhs) < pgPrecNot
		} else {
			// `- -x` would render `--x`, a PG comment: wrap equal
			// levels too.
			wrap = pgExprPrec(x.Rhs) <= pgPrecUnary
		}
		rhs, w := parenIf(x.Rhs, wrap)
		if !w {
			return n
		}
		cp := *x
		cp.Rhs = rhs
		return &cp
	case *ast.BetweenExpr:
		e, ew := parenIf(x.Expr, pgExprPrec(x.Expr) <= pgPrecLike)
		lo, lw := parenIf(x.Low, pgExprPrec(x.Low) <= pgPrecLike)
		hi, hw := parenIf(x.High, pgExprPrec(x.High) <= pgPrecLike)
		if !ew && !lw && !hw {
			return n
		}
		cp := *x
		cp.Expr, cp.Low, cp.High = e, lo, hi
		return &cp
	case *ast.InExpr:
		e, w := parenIf(x.Expr, pgExprPrec(x.Expr) <= pgPrecLike)
		if !w {
			return n
		}
		cp := *x
		cp.Expr = e
		return &cp
	}
	return n
}

// mysqlResidualIdioms lists, in tree order and without duplicates, the
// MySQL constructs RewriteMySQLAST could not translate and that would
// otherwise reach PG unchanged. The caller surfaces them as warnings /
// prerequisites so they never pass silently.
func mysqlResidualIdioms(n ast.Node) []string {
	if n == nil {
		return nil
	}
	v := &mysqlResidualVisitor{seen: map[string]bool{}}
	ast.Walk(v, n)
	return v.out
}

type mysqlResidualVisitor struct {
	out  []string
	seen map[string]bool
}

func (v *mysqlResidualVisitor) add(label string) {
	if v.seen[label] {
		return
	}
	v.seen[label] = true
	v.out = append(v.out, label)
}

func (v *mysqlResidualVisitor) Visit(n ast.Node) ast.Visitor {
	switch x := n.(type) {
	case *ast.FuncCall:
		name := strings.ToUpper(x.Name)
		switch name {
		case "GROUP_CONCAT":
			v.add("GROUP_CONCAT could not be rewritten to string_agg — manual review")
		case "STRING_AGG":
			// Built by VisitMySQLGroupConcat: the aggregated argument is
			// CAST(e AS text), so DISTINCT combined with ORDER BY never
			// satisfies PG's "ORDER BY expressions must appear in
			// argument list" rule and fails at apply time.
			if x.Distinct && len(x.AggOrderBy) > 0 {
				v.add("GROUP_CONCAT(DISTINCT … ORDER BY …): PG string_agg rejects ORDER BY keys that are not the DISTINCT argument — manual review")
			}
		case "JSON_EXTRACT", "JSON_UNQUOTE":
			v.add(name + ": JSON path uses wildcards/quoted keys — manual review")
		case "DATE_FORMAT", "STR_TO_DATE":
			v.add(name + ": format codes differ from PG to_char/to_date — manual review")
		case "TIME_FORMAT":
			// Left by VisitMySQLTimeFormat: non-literal format or a
			// specifier outside TIME_FORMAT's hour/minute/second set.
			v.add("TIME_FORMAT: format is not a literal of hour/minute/second specifiers — manual review")
		case "DATE_ADD", "ADDDATE", "DATE_SUB", "SUBDATE":
			v.add(name + ": second argument is not an interval PG can express (day count or compound unit) — manual review")
		case "CONVERT":
			v.add("CONVERT: MySQL CONVERT(expr, type) / CONVERT(expr USING charset) has no direct PG form — manual review")
		}
	case *ast.IntervalLit:
		if x.Unit == "" {
			break
		}
		unit := strings.ToUpper(x.Unit)
		if _, simple := mysqlIntervalUnit(x.Unit); simple {
			v.add("INTERVAL … " + unit + ": non-numeric interval quantity has no direct PG form — manual review")
		} else {
			v.add("INTERVAL … " + unit + ": compound/unsupported interval unit has no direct PG form — manual review")
		}
	}
	return v
}

// collectTableRefs returns the relation names a SELECT reads: every
// FROM table reachable through joins, derived tables and subqueries in
// any expression slot (WHERE / IN / EXISTS / scalar subqueries), minus
// the names of CTEs defined inside the statement. Names are
// de-duplicated case-insensitively and returned in source order, bare
// (without schema qualifier). Used to derive view dependencies.
func collectTableRefs(sel *ast.SelectStmt) []string {
	if sel == nil {
		return nil
	}
	v := &tableRefVisitor{ctes: map[string]bool{}}
	ast.Walk(v, sel)
	var out []string
	seen := map[string]bool{}
	for _, name := range v.tables {
		key := strings.ToLower(name)
		if v.ctes[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

type tableRefVisitor struct {
	tables []string
	ctes   map[string]bool
}

func (v *tableRefVisitor) Visit(n ast.Node) ast.Visitor {
	switch x := n.(type) {
	case *ast.SelectStmt:
		if x.With != nil {
			for _, c := range x.With.CTEs {
				v.ctes[strings.ToLower(c.Name)] = true
			}
		}
	case *ast.FromTable:
		if x.Name != "" {
			v.tables = append(v.tables, x.Name)
		}
	}
	return v
}

// -- Local variable references ---------------------------------------

// MySQL (and DB2, routed through the same PL parser) resolve local
// variable names case-insensitively, and the DECLARE section of the
// translated routine emits each local unquoted (`vCount INTEGER;`), so PG
// folds the declared name to lowercase (`vcount`). The PG writer, on the
// other hand, double-quotes every identifier with its source case
// (`"vCount"`), which would no longer resolve to the declared variable.
// Assignment targets and INTO / FETCH lists are emitted bare and fold the
// same way as the declaration, so only expression references need
// aligning: foldMySQLLocalVarRefs rewrites every single-part Ident that
// names a declared local to the PG-folded spelling of its declaration.
//
// Routine parameters are NOT folded: the signature renders them with
// quoteIdent (case kept), so the writer's case-preserving reference
// already matches when the body spells them like the declaration.
// Column references keep their source spelling too — the MySQL DDL path
// also emits columns quoted with their source case, so a body written
// with the same spelling as the table definition resolves.
//
// Locals are collected over the whole routine (every nested block): a
// column spelled like a local elsewhere in the routine is folded as
// well, which is harmless for lowercase columns and already ambiguous in
// PL/pgSQL otherwise.

// foldMySQLLocalVarRefs applies the local-variable fold to a parsed
// MySQL / DB2 routine body. No declared locals → stmts unchanged.
func foldMySQLLocalVarRefs(stmts []ast.PLStmt) []ast.PLStmt {
	locals := collectMySQLLocalVars(stmts)
	if len(locals) == 0 {
		return stmts
	}
	fold := MakeMySQLLocalVarFoldVisitor(locals)
	out := make([]ast.PLStmt, len(stmts))
	for i, s := range stmts {
		if rs, ok := ast.Rewrite(s, fold).(ast.PLStmt); ok {
			out[i] = rs
		} else {
			out[i] = s
		}
	}
	return out
}

// collectMySQLLocalVars maps the case-insensitive key of every local
// declared in the body (DECLARE v …, including each name of a
// multi-name `DECLARE a, b INT`) to the name PG gives the unquoted
// declaration.
func collectMySQLLocalVars(stmts []ast.PLStmt) map[string]string {
	c := &mysqlLocalVarCollector{names: map[string]string{}}
	for _, s := range stmts {
		ast.Walk(c, s)
	}
	return c.names
}

type mysqlLocalVarCollector struct {
	names map[string]string
}

func (c *mysqlLocalVarCollector) Visit(n ast.Node) ast.Visitor {
	if d, ok := n.(*ast.DeclareVar); ok {
		// The MySQL parser joins a multi-name declaration with ','
		// (identifier list, not SQL text).
		start := 0
		for i := 0; i <= len(d.Name); i++ {
			if i < len(d.Name) && d.Name[i] != ',' {
				continue
			}
			if name := d.Name[start:i]; name != "" {
				c.names[strings.ToLower(name)] = pgFoldIdent(name)
			}
			start = i + 1
		}
	}
	return c
}

// MakeMySQLLocalVarFoldVisitor returns a rewriter that renames every
// single-part Ident whose case-insensitive name is a key of locals to the
// mapped (PG-folded) spelling. Qualified names (NEW.col, t.col) are left
// alone.
func MakeMySQLLocalVarFoldVisitor(locals map[string]string) ast.Rewriter {
	return func(n ast.Node) ast.Node {
		id, ok := n.(*ast.Ident)
		if !ok || len(id.Parts) != 1 {
			return n
		}
		folded, hit := locals[strings.ToLower(id.Parts[0])]
		if !hit || folded == id.Parts[0] {
			return n
		}
		cp := *id
		cp.Parts = []string{folded}
		return &cp
	}
}

// pgFoldIdent folds one unquoted identifier the way PostgreSQL does
// (downcase_identifier): ASCII A-Z only, other bytes untouched.
func pgFoldIdent(name string) string {
	b := []byte(name)
	for i, ch := range b {
		if ch >= 'A' && ch <= 'Z' {
			b[i] = ch + ('a' - 'A')
		}
	}
	return string(b)
}
