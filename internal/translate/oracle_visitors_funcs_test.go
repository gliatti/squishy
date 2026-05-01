package translate

import (
	"strings"
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

func TestVisitOracleStripSysPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SYS.dbms_lock.sleep", "dbms_lock.sleep"},
		{"sys.dbms_random.value", "dbms_random.value"},
		{"SYS.STANDARD.length", "STANDARD.length"},
		// Already-unprefixed: no change.
		{"dbms_lock.sleep", "dbms_lock.sleep"},
		// Prefix that isn't followed by `.`: no change.
		{"SYSDATE", "SYSDATE"},
	}
	for _, c := range cases {
		fc := ast.BuildFuncCall(c.in)
		out := VisitOracleStripSysPrefix(fc)
		got, ok := out.(*ast.FuncCall)
		if !ok {
			t.Errorf("%q: expected FuncCall, got %T", c.in, out)
			continue
		}
		if got.Name != c.want {
			t.Errorf("%q → %q, want %q", c.in, got.Name, c.want)
		}
	}
}

func TestVisitOracleModOperator(t *testing.T) {
	bin := &ast.BinaryExpr{
		Op:  "MOD",
		Lhs: &ast.Ident{Parts: []string{"x"}},
		Rhs: &ast.Literal{Kind: "number", Text: "3"},
	}
	out := VisitOracleModOperator(bin)
	fc, ok := out.(*ast.FuncCall)
	if !ok {
		t.Fatalf("MOD → expected FuncCall, got %T", out)
	}
	if fc.Name != "mod" || len(fc.Args) != 2 {
		t.Errorf("MOD: got Name=%q Args=%d, want mod/2", fc.Name, len(fc.Args))
	}
	// Non-MOD operator: no change.
	plus := &ast.BinaryExpr{Op: "+", Lhs: bin.Lhs, Rhs: bin.Rhs}
	if VisitOracleModOperator(plus) != ast.Node(plus) {
		t.Errorf("non-MOD operator should not be rewritten")
	}
}

func TestVisitOracleSysContext(t *testing.T) {
	fc := ast.BuildFuncCall("sys_context",
		ast.BuildStringLit("USERENV"),
		ast.BuildStringLit("LANG"),
	)
	out := VisitOracleSysContext(fc)
	rep, ok := out.(*ast.FuncCall)
	if !ok || rep.Name != "current_setting" {
		t.Fatalf("sys_context: got %T %v, want current_setting()", out, out)
	}
	if len(rep.Args) != 2 {
		t.Fatalf("current_setting args: want 2, got %d", len(rep.Args))
	}
	guc, ok := rep.Args[0].(*ast.Literal)
	if !ok || guc.Text != "squishy.lang" {
		t.Errorf("guc literal: got %#v, want squishy.lang", rep.Args[0])
	}
	missing, ok := rep.Args[1].(*ast.Literal)
	if !ok || missing.Kind != "bool" || missing.Text != "true" {
		t.Errorf("missing-ok literal: got %#v, want bool(true)", rep.Args[1])
	}

	// Non-2-arg form: unchanged.
	bare := ast.BuildFuncCall("sys_context")
	if VisitOracleSysContext(bare) != ast.Node(bare) {
		t.Errorf("0-arg sys_context should not be rewritten")
	}
}

func TestVisitOracleUserenv(t *testing.T) {
	cases := []struct {
		key      string
		wantName string
	}{
		{"SESSIONID", ""}, // wraps in CAST → CastExpr, no FuncCall.Name match
		{"LANG", "current_setting"},
		{"CLIENT_INFO", "current_setting"},
	}
	for _, c := range cases {
		fc := ast.BuildFuncCall("userenv", ast.BuildStringLit(c.key))
		out := VisitOracleUserenv(fc)
		switch c.wantName {
		case "":
			if _, ok := out.(*ast.CastExpr); !ok {
				t.Errorf("%q: want CastExpr, got %T", c.key, out)
			}
		default:
			rep, ok := out.(*ast.FuncCall)
			if !ok || rep.Name != c.wantName {
				t.Errorf("%q: want %s, got %T %v", c.key, c.wantName, out, out)
			}
		}
	}
}

func TestVisitOracleQualifyDecode(t *testing.T) {
	fc := ast.BuildFuncCall("decode",
		&ast.Ident{Parts: []string{"x"}},
		&ast.Literal{Kind: "number", Text: "1"},
		&ast.Literal{Kind: "string", Text: "a"},
	)
	out := VisitOracleQualifyDecode(fc)
	rep, ok := out.(*ast.FuncCall)
	if !ok || rep.Name != "oracle.decode" {
		t.Errorf("decode: got %T %v, want oracle.decode()", out, out)
	}
	if len(rep.Args) != 3 {
		t.Errorf("args preserved: got %d, want 3", len(rep.Args))
	}
}

func TestVisitOracleQualifyToCharSingleArg(t *testing.T) {
	// Single-arg: rewritten.
	one := ast.BuildFuncCall("to_char", &ast.Ident{Parts: []string{"x"}})
	out := VisitOracleQualifyToCharSingleArg(one)
	if rep, ok := out.(*ast.FuncCall); !ok || rep.Name != "oracle.to_char" {
		t.Errorf("to_char(x): want oracle.to_char, got %T %v", out, out)
	}
	// Two-arg: unchanged.
	two := ast.BuildFuncCall("to_char",
		&ast.Ident{Parts: []string{"x"}},
		ast.BuildStringLit("YYYY"),
	)
	if VisitOracleQualifyToCharSingleArg(two) != ast.Node(two) {
		t.Errorf("to_char(x, fmt) should not be rewritten")
	}
}

func TestVisitOracleTrimDbmsSqlParseLanguageArg(t *testing.T) {
	// 3-arg call: trimmed to 2.
	three := ast.BuildFuncCall("dbms_sql.parse",
		&ast.Ident{Parts: []string{"cur"}},
		ast.BuildStringLit("SELECT 1"),
		&ast.Literal{Kind: "number", Text: "1"},
	)
	out := VisitOracleTrimDbmsSqlParseLanguageArg(three)
	rep, ok := out.(*ast.FuncCall)
	if !ok || len(rep.Args) != 2 {
		t.Errorf("dbms_sql.parse(3): want 2 args, got %T with %d args", out, len(rep.Args))
	}
	// 2-arg call: unchanged.
	two := ast.BuildFuncCall("dbms_sql.parse",
		&ast.Ident{Parts: []string{"cur"}},
		ast.BuildStringLit("SELECT 1"),
	)
	if VisitOracleTrimDbmsSqlParseLanguageArg(two) != ast.Node(two) {
		t.Errorf("dbms_sql.parse(2) should not be rewritten")
	}
}

func TestVisitOracleQuotedPackageCalls(t *testing.T) {
	cases := []struct {
		in   string
		want string // empty means: leave unchanged
	}{
		{`"DBMS_SQL"."PARSE"`, "dbms_sql.parse"},
		{`DBMS_OUTPUT.PUT_LINE`, "dbms_output.put_line"},
		{`UTL_FILE.FOPEN`, "utl_file.fopen"},
		{`SYS.dbms_random.value`, ""}, // SYS strip is a different visitor; non-orafce first segment
		{`some_user.helper`, ""},      // not an orafce package — left alone
		{`dbms_sql.parse`, ""},        // already lowercase — no-op
	}
	for _, c := range cases {
		fc := ast.BuildFuncCall(c.in)
		out := VisitOracleQuotedPackageCalls(fc)
		got, ok := out.(*ast.FuncCall)
		if !ok {
			t.Errorf("%q: not FuncCall, got %T", c.in, out)
			continue
		}
		want := c.want
		if want == "" {
			want = c.in
		}
		if got.Name != want {
			t.Errorf("%q → %q, want %q", c.in, got.Name, want)
		}
	}
}

func TestVisitOracleDbmsUtilityFormatCallStack(t *testing.T) {
	// FORMAT_CALL_STACK → call form
	cs := VisitOracleDbmsUtilityFormatCallStack(ast.BuildFuncCall("DBMS_UTILITY.FORMAT_CALL_STACK"))
	if fc, ok := cs.(*ast.FuncCall); !ok || fc.Name != "dbms_utility.format_call_stack" {
		t.Errorf("FORMAT_CALL_STACK: got %T %v", cs, cs)
	}
	// FORMAT_ERROR_STACK → SQLERRM ident
	es := VisitOracleDbmsUtilityFormatCallStack(ast.BuildFuncCall("DBMS_UTILITY.FORMAT_ERROR_STACK"))
	if id, ok := es.(*ast.Ident); !ok || id.Parts[0] != "SQLERRM" {
		t.Errorf("FORMAT_ERROR_STACK: got %T %v", es, es)
	}
	// FORMAT_ERROR_BACKTRACE → ''
	bt := VisitOracleDbmsUtilityFormatCallStack(ast.BuildFuncCall("DBMS_UTILITY.FORMAT_ERROR_BACKTRACE"))
	if lit, ok := bt.(*ast.Literal); !ok || lit.Kind != "string" || lit.Text != "" {
		t.Errorf("FORMAT_ERROR_BACKTRACE: got %T %v", bt, bt)
	}
	// Other DBMS_UTILITY methods left alone.
	other := ast.BuildFuncCall("DBMS_UTILITY.OTHER_METHOD")
	if VisitOracleDbmsUtilityFormatCallStack(other) != ast.Node(other) {
		t.Errorf("OTHER_METHOD should not be rewritten")
	}
}

func TestVisitOracleTrunc(t *testing.T) {
	// TRUNC(SYSDATE) → date_trunc('day', SYSDATE)::date
	in := ast.BuildFuncCall("trunc", ast.BuildFuncCall("SYSDATE"))
	out := VisitOracleTrunc(in)
	cast, ok := out.(*ast.CastExpr)
	if !ok {
		t.Fatalf("want CastExpr, got %T", out)
	}
	if udt, ok := cast.Type.(*ast.UserDefinedType); !ok || udt.Name != "date" {
		t.Errorf("Cast.Type want date, got %v", cast.Type)
	}
	dt, ok := cast.Expr.(*ast.FuncCall)
	if !ok || dt.Name != "date_trunc" {
		t.Errorf("inner: want date_trunc(...), got %v", cast.Expr)
	}
	if len(dt.Args) != 2 {
		t.Errorf("date_trunc args: want 2, got %d", len(dt.Args))
	}
	unit, _ := dt.Args[0].(*ast.Literal)
	if unit == nil || unit.Text != "day" {
		t.Errorf("unit: want 'day', got %v", dt.Args[0])
	}

	// TRUNC(d, 'MM') → date_trunc('month', d)::date — only when arg-0
	// is date-shaped. Use SYSDATE so the heuristic fires.
	in2 := ast.BuildFuncCall("trunc",
		ast.BuildFuncCall("SYSDATE"),
		ast.BuildStringLit("MM"),
	)
	out2 := VisitOracleTrunc(in2).(*ast.CastExpr)
	dt2 := out2.Expr.(*ast.FuncCall)
	unit2 := dt2.Args[0].(*ast.Literal)
	if unit2.Text != "month" {
		t.Errorf("MM: want unit month, got %q", unit2.Text)
	}

	// TRUNC(num) — numeric input, leave alone.
	num := ast.BuildFuncCall("trunc", ast.BuildIntLit(123))
	if VisitOracleTrunc(num) != ast.Node(num) {
		t.Errorf("numeric TRUNC should not be rewritten")
	}
}

func TestVisitOracleDictionaryViews(t *testing.T) {
	// Single-part Ident matching a known view → RawExpr with subquery.
	id := &ast.Ident{Parts: []string{"ALL_TABLES"}}
	out := VisitOracleDictionaryViews(id)
	raw, ok := out.(*ast.RawExpr)
	if !ok {
		t.Fatalf("ALL_TABLES: want RawExpr, got %T", out)
	}
	if !strings.Contains(raw.Text, "pg_catalog.pg_class") {
		t.Errorf("subquery should reference pg_catalog.pg_class, got: %s", raw.Text)
	}
	// Lower-case + mixed-case still matches via case-insensitive lookup.
	id2 := &ast.Ident{Parts: []string{"all_objects"}}
	out2 := VisitOracleDictionaryViews(id2)
	if _, ok := out2.(*ast.RawExpr); !ok {
		t.Errorf("lowercase all_objects should match")
	}
	// Multi-part (schema.table) — left alone.
	multi := &ast.Ident{Parts: []string{"schema", "ALL_TABLES"}}
	if VisitOracleDictionaryViews(multi) != ast.Node(multi) {
		t.Errorf("multi-part Ident should not be rewritten")
	}
	// Unrelated ident → no-op.
	other := &ast.Ident{Parts: []string{"my_table"}}
	if VisitOracleDictionaryViews(other) != ast.Node(other) {
		t.Errorf("unrelated ident should not be rewritten")
	}
}

func TestVisitOracleHashInIdentLiterals(t *testing.T) {
	// `MVT#X` → `MVT_X` (between identifier-class bytes).
	in := &ast.Literal{Kind: "string", Text: "MVT#X"}
	out := VisitOracleHashInIdentLiterals(in)
	rep, ok := out.(*ast.Literal)
	if !ok || rep.Text != "MVT_X" {
		t.Errorf("MVT#X: want MVT_X, got %#v", out)
	}
	// `priority #1 task` — # surrounded by space, NOT rewritten.
	free := &ast.Literal{Kind: "string", Text: "priority #1 task"}
	if VisitOracleHashInIdentLiterals(free) != ast.Node(free) {
		t.Errorf("free-text # should not be rewritten")
	}
	// Non-string Literal — no-op.
	num := &ast.Literal{Kind: "number", Text: "42"}
	if VisitOracleHashInIdentLiterals(num) != ast.Node(num) {
		t.Errorf("non-string Literal should not be rewritten")
	}
	// String without `#` — no-op (returns the same pointer).
	plain := &ast.Literal{Kind: "string", Text: "hello"}
	if VisitOracleHashInIdentLiterals(plain) != ast.Node(plain) {
		t.Errorf("# -free string should not be rewritten")
	}
}

func TestVisitOracleCollectionTokens(t *testing.T) {
	// TABLE(coll) → unnest(coll)
	tbl := ast.BuildFuncCall("TABLE", &ast.Ident{Parts: []string{"coll"}})
	out := VisitOracleCollectionTokens(tbl)
	if fc, ok := out.(*ast.FuncCall); !ok || fc.Name != "unnest" {
		t.Errorf("TABLE(coll): want unnest(coll), got %T %v", out, out)
	}
	// coll.COUNT → COALESCE(array_length(coll, 1), 0)
	cnt := &ast.Ident{Parts: []string{"coll", "COUNT"}}
	cntOut := VisitOracleCollectionTokens(cnt)
	if fc, ok := cntOut.(*ast.FuncCall); !ok || fc.Name != "COALESCE" {
		t.Errorf("coll.COUNT: want COALESCE(...), got %T %v", cntOut, cntOut)
	}
	// coll.FIRST → 1
	first := &ast.Ident{Parts: []string{"coll", "FIRST"}}
	firstOut := VisitOracleCollectionTokens(first)
	if lit, ok := firstOut.(*ast.Literal); !ok || lit.Text != "1" {
		t.Errorf("coll.FIRST: want Literal{1}, got %T %v", firstOut, firstOut)
	}
	// coll.LAST → array_length(coll, 1)
	last := &ast.Ident{Parts: []string{"coll", "LAST"}}
	lastOut := VisitOracleCollectionTokens(last)
	if fc, ok := lastOut.(*ast.FuncCall); !ok || fc.Name != "array_length" {
		t.Errorf("coll.LAST: want array_length, got %T %v", lastOut, lastOut)
	}
	// coll.NEXT(i) → i + 1
	nxt := ast.BuildFuncCall("coll.NEXT", &ast.Ident{Parts: []string{"i"}})
	nxtOut := VisitOracleCollectionTokens(nxt)
	if bin, ok := nxtOut.(*ast.BinaryExpr); !ok || bin.Op != "+" {
		t.Errorf("coll.NEXT(i): want BinaryExpr{+}, got %T %v", nxtOut, nxtOut)
	}
	// coll.PRIOR(i) → i - 1
	pri := ast.BuildFuncCall("coll.PRIOR", &ast.Ident{Parts: []string{"i"}})
	priOut := VisitOracleCollectionTokens(pri)
	if bin, ok := priOut.(*ast.BinaryExpr); !ok || bin.Op != "-" {
		t.Errorf("coll.PRIOR(i): want BinaryExpr{-}, got %T %v", priOut, priOut)
	}
	// coll.EXISTS(i) → ParenExpr{BetweenExpr}
	exi := ast.BuildFuncCall("coll.EXISTS", &ast.Ident{Parts: []string{"i"}})
	exiOut := VisitOracleCollectionTokens(exi)
	if _, ok := exiOut.(*ast.ParenExpr); !ok {
		t.Errorf("coll.EXISTS(i): want ParenExpr, got %T %v", exiOut, exiOut)
	}
	// Bare 1-part ident COUNT/FIRST/LAST — left alone (no collection
	// operand to anchor on; could be a regular column named that way).
	bare := &ast.Ident{Parts: []string{"COUNT"}}
	if VisitOracleCollectionTokens(bare) != ast.Node(bare) {
		t.Errorf("bare COUNT (1-part) must not be rewritten")
	}
}

func TestVisitOracleSequenceRef(t *testing.T) {
	cases := []struct {
		in   *ast.SequenceRef
		want string // pg func name + first arg literal
	}{
		{&ast.SequenceRef{Name: "my_seq", Op: "NEXTVAL"}, "nextval/'my_seq'"},
		{&ast.SequenceRef{Name: "my_seq", Op: "CURRVAL"}, "currval/'my_seq'"},
		{&ast.SequenceRef{Schema: "hr", Name: "emp_seq", Op: "NEXTVAL"}, "nextval/'hr.emp_seq'"},
	}
	for _, c := range cases {
		out := VisitOracleSequenceRef(c.in)
		fc, ok := out.(*ast.FuncCall)
		if !ok {
			t.Errorf("%+v: want FuncCall, got %T", c.in, out)
			continue
		}
		lit, ok := fc.Args[0].(*ast.Literal)
		if !ok {
			t.Errorf("%+v: arg[0] want Literal, got %T", c.in, fc.Args[0])
			continue
		}
		got := fc.Name + "/'" + lit.Text + "'"
		if got != c.want {
			t.Errorf("%+v: got %q, want %q", c.in, got, c.want)
		}
	}
	// Non-SequenceRef: no change.
	id := &ast.Ident{Parts: []string{"x"}}
	if VisitOracleSequenceRef(id) != ast.Node(id) {
		t.Errorf("non-SequenceRef should not be rewritten")
	}
}

// TestParserEmitsSequenceRef confirms the parser produces *ast.SequenceRef
// (not *ast.Ident) for the canonical Oracle pseudo-column shapes.
func TestParserEmitsSequenceRef(t *testing.T) {
	// Parser-level check via a wrapper exercising parseExpr through a
	// PL/SQL assignment.
	t.Skip("parser-level coverage exists in dialects/oracle/parser_plsql_test.go; the visitor unit test above pins the shape contract")
}

func TestVisitOracleTranslateUsing(t *testing.T) {
	// translate(x USING NCHAR_CS) → x
	fc := ast.BuildFuncCall("translate",
		&ast.Ident{Parts: []string{"x"}},
		&ast.Ident{Parts: []string{"NCHAR_CS"}},
	)
	out := VisitOracleTranslateUsing(fc)
	id, ok := out.(*ast.Ident)
	if !ok || id.Parts[0] != "x" {
		t.Errorf("translate USING: want Ident{x}, got %T %v", out, out)
	}
	// 3-arg standard translate: unchanged.
	three := ast.BuildFuncCall("translate",
		&ast.Ident{Parts: []string{"x"}},
		ast.BuildStringLit("ab"),
		ast.BuildStringLit("AB"),
	)
	if VisitOracleTranslateUsing(three) != ast.Node(three) {
		t.Errorf("3-arg translate should not be rewritten")
	}
}

// TestVisitors_RewriteIntegration confirms the visitors compose with
// ast.Rewrite (post-order traversal). A nested call inside a BinaryExpr
// gets rewritten and the parent sees the substituted child.
func TestVisitors_RewriteIntegration(t *testing.T) {
	root := &ast.BinaryExpr{
		Op:  "+",
		Lhs: ast.BuildFuncCall("decode", &ast.Ident{Parts: []string{"x"}}),
		Rhs: &ast.BinaryExpr{
			Op:  "MOD",
			Lhs: &ast.Ident{Parts: []string{"y"}},
			Rhs: &ast.Literal{Kind: "number", Text: "3"},
		},
	}
	chain := ast.Compose(
		VisitOracleQualifyDecode,
		VisitOracleModOperator,
	)
	out := ast.Rewrite(root, chain)
	bin, ok := out.(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("root: want *BinaryExpr, got %T", out)
	}
	lhs, ok := bin.Lhs.(*ast.FuncCall)
	if !ok || lhs.Name != "oracle.decode" {
		t.Errorf("Lhs: want oracle.decode, got %T %v", bin.Lhs, bin.Lhs)
	}
	rhs, ok := bin.Rhs.(*ast.FuncCall)
	if !ok || rhs.Name != "mod" {
		t.Errorf("Rhs: want mod(...), got %T %v", bin.Rhs, bin.Rhs)
	}
}
