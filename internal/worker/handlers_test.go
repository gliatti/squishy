package worker

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	oracle "gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

var errRelEmp = errors.New(`ERROR: relation "emp" does not exist (SQLSTATE 42P01)`)

// Probe: documents how the Oracle lexer splits an anchored %TYPE
// reference. replaceAnchoredTypeRefs relies on this exact shape:
// IDENT . IDENT PUNCT(%) KEYWORD|IDENT(TYPE).
func TestRetryWithTypeFallback_TokenShape(t *testing.T) {
	toks := significantTokens(`v emp.sal%TYPE;`)
	type kl struct {
		kind oracle.TokenKind
		lit  string
	}
	var got []kl
	for _, tk := range toks {
		got = append(got, kl{tk.Kind, tk.Lit})
	}
	require.Len(t, got, 8)
	require.Equal(t, kl{oracle.TOK_IDENT, "V"}, got[0])
	require.Equal(t, kl{oracle.TOK_IDENT, "EMP"}, got[1])
	require.Equal(t, kl{oracle.TOK_PUNCT, "."}, got[2])
	require.Equal(t, kl{oracle.TOK_IDENT, "SAL"}, got[3])
	require.Equal(t, kl{oracle.TOK_PUNCT, "%"}, got[4])
	require.Equal(t, "TYPE", got[5].lit)
	require.Contains(t, []oracle.TokenKind{oracle.TOK_IDENT, oracle.TOK_KEYWORD}, got[5].kind)
	require.Equal(t, kl{oracle.TOK_PUNCT, ";"}, got[6])
	require.Equal(t, oracle.TOK_EOF, got[7].kind)
}

func TestRetryWithTypeFallback(t *testing.T) {
	cases := []struct {
		name    string
		ddl     string
		err     error
		want    string
		changed bool
	}{
		{"simple", `v emp.sal%TYPE;`, errRelEmp, `v text;`, true},
		{"case-insensitive relation", `v EMP.sal%TYPE;`, errRelEmp, `v text;`, true},
		{"lowercase type keyword", `v emp.sal%type;`, errRelEmp, `v text;`, true},
		{"schema-qualified", `v mig.emp.sal%TYPE;`, errRelEmp, `v text;`, true},
		{"quoted identifiers", `v "mig"."emp"."sal"%TYPE;`, errRelEmp, `v text;`, true},
		{"several occurrences", "a emp.x%TYPE;\nb emp.y%TYPE;\nc other.z%TYPE;", errRelEmp,
			"a text;\nb text;\nc other.z%TYPE;", true},
		{"other relation untouched", `v other.sal%TYPE;`, errRelEmp, `v other.sal%TYPE;`, false},
		{"suffix relation untouched", `v xemp.sal%TYPE;`, errRelEmp, `v xemp.sal%TYPE;`, false},
		{"inside string literal untouched", `v := 'emp.sal%TYPE';`, errRelEmp, `v := 'emp.sal%TYPE';`, false},
		{"inside comment untouched", "-- emp.sal%TYPE\nv int;", errRelEmp, "-- emp.sal%TYPE\nv int;", false},
		{"rowtype untouched", `v emp%ROWTYPE;`, errRelEmp, `v emp%ROWTYPE;`, false},
		{"nil error", `v emp.sal%TYPE;`, nil, `v emp.sal%TYPE;`, false},
		{"no relation marker", `v emp.sal%TYPE;`, errors.New(`ERROR: syntax error at or near "x"`), `v emp.sal%TYPE;`, false},
		{"non-ASCII before match (rune offsets)", "-- é\nv emp.sal%TYPE; -- ü\nw emp.id%TYPE;", errRelEmp,
			"-- é\nv text; -- ü\nw text;", true},
		{"non-ASCII string before match", `x := 'déjà'; v emp.sal%TYPE;`, errRelEmp,
			`x := 'déjà'; v text;`, true},
		{"qualified relation in error", `v mig.emp.sal%TYPE; w emp.id%TYPE;`,
			errors.New(`ERROR: relation "mig.emp" does not exist (SQLSTATE 42P01)`),
			`v text; w emp.id%TYPE;`, true},
		{"dollar-quoted routine body",
			"CREATE FUNCTION f() RETURNS int AS $body$\nDECLARE\n  v emp.sal%TYPE;\nBEGIN\n  <<lbl>>\n  v := 1::int;\n  RETURN v;\nEND;\n$body$ LANGUAGE plpgsql;",
			errRelEmp,
			"CREATE FUNCTION f() RETURNS int AS $body$\nDECLARE\n  v text;\nBEGIN\n  <<lbl>>\n  v := 1::int;\n  RETURN v;\nEND;\n$body$ LANGUAGE plpgsql;",
			true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := retryWithTypeFallback(tc.ddl, tc.err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.changed, changed)
		})
	}
}
