package tsql

import "testing"

type tokWant struct {
	kind TokenKind
	lit  string
}

func lexKinds(src string) []tokWant {
	var out []tokWant
	for _, t := range Tokenize(src) {
		if t.Kind == TOK_EOF {
			break
		}
		out = append(out, tokWant{t.Kind, t.Lit})
	}
	return out
}

func TestLexer(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []tokWant
	}{
		{"bracket identifiers", "[Order Details].[a]]b]",
			[]tokWant{{TOK_QUOTED_IDENT, "Order Details"}, {TOK_PUNCT, "."}, {TOK_QUOTED_IDENT, "a]b"}}},
		{"double-quoted identifiers", `"dbo"."Order ""X"""`,
			[]tokWant{{TOK_QUOTED_IDENT, "dbo"}, {TOK_PUNCT, "."}, {TOK_QUOTED_IDENT, `Order "X"`}}},
		{"strings and national strings", `'it''s' N'café' n'x'`,
			[]tokWant{{TOK_STRING, "it's"}, {TOK_STRING, "café"}, {TOK_STRING, "x"}}},
		{"no backslash escape", `'C:\Samples\'`,
			[]tokWant{{TOK_STRING, `C:\Samples\`}}},
		{"variables", "@x @@ROWCOUNT @@FETCH_STATUS @a#b",
			[]tokWant{{TOK_VARIABLE, "@x"}, {TOK_VARIABLE, "@@ROWCOUNT"}, {TOK_VARIABLE, "@@FETCH_STATUS"}, {TOK_VARIABLE, "@a#b"}}},
		{"temp tables and pseudo columns", "#t ##g $action",
			[]tokWant{{TOK_IDENT, "#t"}, {TOK_IDENT, "##g"}, {TOK_IDENT, "$action"}}},
		{"numbers", "42 3.14 .5 1. 1e10 2.5E-3",
			[]tokWant{{TOK_NUMBER, "42"}, {TOK_NUMBER, "3.14"}, {TOK_NUMBER, ".5"}, {TOK_NUMBER, "1."}, {TOK_NUMBER, "1e10"}, {TOK_NUMBER, "2.5E-3"}}},
		{"hex and money", "0x1F 0x $12.50 $8000",
			[]tokWant{{TOK_HEX, "1F"}, {TOK_HEX, ""}, {TOK_MONEY, "12.50"}, {TOK_MONEY, "8000"}}},
		{"keywords upper-cased, other words kept", "select Top nocount",
			[]tokWant{{TOK_KEYWORD, "SELECT"}, {TOK_KEYWORD, "TOP"}, {TOK_IDENT, "nocount"}}},
		{"operators", "+= -= <> != !< !> <= >= :: ||",
			[]tokWant{{TOK_PUNCT, "+="}, {TOK_PUNCT, "-="}, {TOK_PUNCT, "<>"}, {TOK_PUNCT, "!="}, {TOK_PUNCT, "!<"}, {TOK_PUNCT, "!>"}, {TOK_PUNCT, "<="}, {TOK_PUNCT, ">="}, {TOK_PUNCT, "::"}, {TOK_PUNCT, "||"}}},
		{"comments dropped, nested block comments", "a /* x /* y */ z */ b -- tail\nc",
			[]tokWant{{TOK_IDENT, "a"}, {TOK_IDENT, "b"}, {TOK_IDENT, "c"}}},
		{"GO on its own line", "SELECT 1\nGO\nSELECT 2\n  go 3 -- twice\n",
			[]tokWant{{TOK_KEYWORD, "SELECT"}, {TOK_NUMBER, "1"}, {TOK_GO, ""}, {TOK_KEYWORD, "SELECT"}, {TOK_NUMBER, "2"}, {TOK_GO, "3"}}},
		{"GO not alone is an identifier", "SELECT go FROM t\ngo x",
			[]tokWant{{TOK_KEYWORD, "SELECT"}, {TOK_IDENT, "go"}, {TOK_KEYWORD, "FROM"}, {TOK_IDENT, "t"}, {TOK_IDENT, "go"}, {TOK_IDENT, "x"}}},
		{"sqlcmd directive and variable", " :setvar Db \"x\"\nUSE $(Db)",
			[]tokWant{{TOK_SQLCMD, "SETVAR"}, {TOK_KEYWORD, "USE"}, {TOK_SQLCMD_VAR, "Db"}}},
		{"byte-order mark skipped", "\uFEFFSELECT",
			[]tokWant{{TOK_KEYWORD, "SELECT"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lexKinds(c.src)
			if len(got) != len(c.want) {
				t.Fatalf("tokens: want %v, got %v", c.want, got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("token %d: want %v, got %v", i, c.want[i], got[i])
				}
			}
		})
	}
}

func TestLexerNationalFlagAndPositions(t *testing.T) {
	toks := Tokenize("SELECT\n  N'é', x")
	if len(toks) != 5 {
		t.Fatalf("want 5 tokens, got %d (%v)", len(toks), toks)
	}
	s := toks[1]
	if !s.NString || s.Kind != TOK_STRING {
		t.Errorf("N'é' must be an NString STRING, got %+v", s)
	}
	if s.Pos.Line != 2 || s.Pos.Col != 3 || s.Pos.Offset != 9 {
		t.Errorf("position: got %+v", s.Pos)
	}
	if s.End != 13 {
		t.Errorf("end offset: want 13, got %d", s.End)
	}
	if !s.LineStart || toks[3].LineStart {
		t.Errorf("LineStart: want true for N'é', false for x")
	}
}

func TestLexerErrors(t *testing.T) {
	for _, src := range []string{"'open", "[open", `"open`, "/* open", "$"} {
		_, errs := Parse(src)
		if len(errs) == 0 {
			t.Errorf("%q: want a lexical error", src)
		}
	}
}
