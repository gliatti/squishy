package postgres

import "testing"

// A routine body is dollar-quoted with a tag that does not occur in it, so
// a body kept verbatim from the source cannot close the quote and append
// its own statements.
func TestWriteRoutine_BodyDollarQuote(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"plain body", "BEGIN\n  RETURN 1;\nEND;",
			"CREATE OR REPLACE FUNCTION \"s\".\"f\"() RETURNS int\nLANGUAGE plpgsql AS $body$\nBEGIN\n  RETURN 1;\nEND;\n$body$;\n"},
		{"body contains $body$", "BEGIN\n  RETURN '$body$; DROP TABLE t; --';\nEND;",
			"CREATE OR REPLACE FUNCTION \"s\".\"f\"() RETURNS int\nLANGUAGE plpgsql AS $body_1$\nBEGIN\n  RETURN '$body$; DROP TABLE t; --';\nEND;\n$body_1$;\n"},
		{"body contains $body$ and $body_1$", "BEGIN RETURN '$body$' || '$body_1$'; END;",
			"CREATE OR REPLACE FUNCTION \"s\".\"f\"() RETURNS int\nLANGUAGE plpgsql AS $body_2$\nBEGIN RETURN '$body$' || '$body_1$'; END;\n$body_2$;\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Write([]Stmt{&CreateFunction{Schema: "s", Name: "f", Returns: "int", Body: c.body}})
			if got != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}

	got := Write([]Stmt{&CreateProcedure{Schema: "s", Name: "p", Body: "BEGIN PERFORM '$body$'; END;"}})
	want := "CREATE OR REPLACE PROCEDURE \"s\".\"p\"()\nLANGUAGE plpgsql AS $body_1$\nBEGIN PERFORM '$body$'; END;\n$body_1$;\n"
	if got != want {
		t.Fatalf("procedure got:\n%s\nwant:\n%s", got, want)
	}
}
