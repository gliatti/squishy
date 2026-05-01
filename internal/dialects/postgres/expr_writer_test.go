package postgres

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// TestWriteExpr_Literals — string escapes ('' for embedded quotes),
// NULL, numeric verbatim.
func TestWriteExpr_Literals(t *testing.T) {
	cases := []struct {
		in   ast.Expr
		want string
	}{
		{&ast.Literal{Kind: "string", Text: "hello"}, "'hello'"},
		{&ast.Literal{Kind: "string", Text: "it's me"}, "'it''s me'"},
		{&ast.Literal{Kind: "string", Text: ""}, "''"},
		{&ast.Literal{Kind: "null"}, "NULL"},
		{&ast.Literal{Kind: "number", Text: "42"}, "42"},
		{&ast.Literal{Kind: "number", Text: "-3.14"}, "-3.14"},
		{&ast.Literal{Kind: "bool", Text: "TRUE"}, "TRUE"},
	}
	for _, c := range cases {
		if got := WriteExpr(c.in); got != c.want {
			t.Errorf("WriteExpr(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteExpr_Idents — multi-part idents are dotted-quoted; the
// `*` part round-trips bare; trigger pseudocols (NEW, OLD, TG_*) stay
// unquoted because PG plpgsql keywords change meaning when quoted.
func TestWriteExpr_Idents(t *testing.T) {
	cases := []struct {
		in   ast.Expr
		want string
	}{
		{&ast.Ident{Parts: []string{"col"}}, `"col"`},
		{&ast.Ident{Parts: []string{"schema", "tbl", "col"}}, `"schema"."tbl"."col"`},
		{&ast.Ident{Parts: []string{"t", "*"}}, `"t".*`},
		{&ast.Ident{Parts: []string{"NEW", "qty"}}, `NEW."qty"`},
		{&ast.Ident{Parts: []string{"OLD", "id"}}, `OLD."id"`},
		{&ast.Ident{Parts: []string{":NEW", "col"}}, `:NEW."col"`},
		{&ast.Ident{Parts: []string{"TG_OP"}}, `TG_OP`},
		// Reserved-word-looking identifier still gets quoted normally.
		{&ast.Ident{Parts: []string{"order"}}, `"order"`},
		// Embedded double-quote escapes by doubling.
		{&ast.Ident{Parts: []string{`weird"name`}}, `"weird""name"`},
	}
	for _, c := range cases {
		if got := WriteExpr(c.in); got != c.want {
			t.Errorf("WriteExpr(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteExpr_BinaryOps — operator round-trip, plus DIV → / fold.
func TestWriteExpr_BinaryOps(t *testing.T) {
	cases := []struct {
		op   string
		want string
	}{
		{"+", `"a" + "b"`},
		{"=", `"a" = "b"`},
		{"||", `"a" || "b"`},
		{"AND", `"a" AND "b"`},
		{"DIV", `"a" / "b"`}, // Oracle DIV folds to PG /
	}
	for _, c := range cases {
		bin := &ast.BinaryExpr{
			Op:  c.op,
			Lhs: &ast.Ident{Parts: []string{"a"}},
			Rhs: &ast.Ident{Parts: []string{"b"}},
		}
		if got := WriteExpr(bin); got != c.want {
			t.Errorf("op=%q: got %q, want %q", c.op, got, c.want)
		}
	}
}

// TestWriteExpr_FuncCall — niladic Oracle pseudocols emit without
// parens; everything else gets the standard func(arg, arg) form.
func TestWriteExpr_FuncCall(t *testing.T) {
	cases := []struct {
		in   ast.Expr
		want string
	}{
		{&ast.FuncCall{Name: "SYSDATE"}, "SYSDATE"},
		{&ast.FuncCall{Name: "ROWNUM"}, "ROWNUM"},
		{&ast.FuncCall{Name: "now"}, "now()"},
		{&ast.FuncCall{
			Name: "decode",
			Args: []ast.Expr{
				&ast.Ident{Parts: []string{"x"}},
				&ast.Literal{Kind: "number", Text: "1"},
				&ast.Literal{Kind: "string", Text: "a"},
			},
		}, `decode("x", 1, 'a')`},
	}
	for _, c := range cases {
		if got := WriteExpr(c.in); got != c.want {
			t.Errorf("WriteExpr(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteExpr_CaseExpr — both simple and searched forms.
func TestWriteExpr_CaseExpr(t *testing.T) {
	simple := &ast.CaseExpr{
		Operand: &ast.Ident{Parts: []string{"x"}},
		Whens: []ast.CaseExprWhen{
			{Match: &ast.Literal{Kind: "number", Text: "1"}, Then: &ast.Literal{Kind: "string", Text: "a"}},
		},
		Else: &ast.Literal{Kind: "string", Text: "z"},
	}
	want := `CASE "x" WHEN 1 THEN 'a' ELSE 'z' END`
	if got := WriteExpr(simple); got != want {
		t.Errorf("simple CASE: got %q, want %q", got, want)
	}

	searched := &ast.CaseExpr{
		Whens: []ast.CaseExprWhen{
			{
				Match: &ast.BinaryExpr{
					Op:  ">",
					Lhs: &ast.Ident{Parts: []string{"x"}},
					Rhs: &ast.Literal{Kind: "number", Text: "0"},
				},
				Then: &ast.Literal{Kind: "string", Text: "pos"},
			},
		},
	}
	wantSearched := `CASE WHEN "x" > 0 THEN 'pos' END`
	if got := WriteExpr(searched); got != wantSearched {
		t.Errorf("searched CASE: got %q, want %q", got, wantSearched)
	}
}

// TestWriteExpr_NilSafe — nil input returns "".
func TestWriteExpr_NilSafe(t *testing.T) {
	if got := WriteExpr(nil); got != "" {
		t.Errorf("WriteExpr(nil) = %q, want empty", got)
	}
}

// TestWriteExpr_RawExprFallback — RawExpr emits its Text verbatim
// during the transition window.
func TestWriteExpr_RawExprFallback(t *testing.T) {
	got := WriteExpr(&ast.RawExpr{Text: "weird oracle thing"})
	if got != "weird oracle thing" {
		t.Errorf("RawExpr fallback: got %q", got)
	}
}

// TestWriteExpr_CursorAttr — emits the source `cur%attr` form when no
// visitor has rewritten the node yet. Phase 3 visitors are expected to
// substitute these before the writer sees them; this test pins the
// faithful-fallback contract.
func TestWriteExpr_CursorAttr(t *testing.T) {
	got := WriteExpr(&ast.CursorAttr{Cursor: "c1", Attr: "NOTFOUND"})
	if got != "c1%NOTFOUND" {
		t.Errorf("CursorAttr fallback: got %q", got)
	}
}
