package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckExprFragmentAccepts(t *testing.T) {
	for _, expr := range []string{
		`CASE WHEN "paid" THEN '1' ELSE '0' END`,
		`upper("v")`,
		`("v" || 'x')`,
		`'it''s ; -- /* ) ('`, // specials inside a string
		`"we""ird ; ) -- id"`, // specials inside a quoted identifier
		`E'a\\b' || E'\n'`,    // backslash escapes other than \'
		`'a\b' || '\\'`,       // backslashes in plain strings
		`"a" - -1`,            // two minus signs, not a comment
		`"a" / 2 * 3`,         // division, not a comment
		`coalesce(("a"::text), '')`,
		`U&'d\0061t\+000061'`,
		"  \"a\"\n+ 1\t",
	} {
		require.NoError(t, CheckExprFragment(expr), expr)
	}
}

func TestCheckExprFragmentRejects(t *testing.T) {
	for _, tc := range []struct{ expr, why string }{
		{``, "empty"},
		{" \t\n ", "empty"},
		// Break out of GENERATED ALWAYS AS (…) STORED and run another
		// statement, then reopen a clause so the script stays valid.
		{`'x') STORED); DROP SCHEMA squishy CASCADE; CREATE TABLE z (a text GENERATED ALWAYS AS ('x'`, "unbalanced ')'"},
		{`1); DROP TABLE t; SELECT (1`, "unbalanced ')'"},
		{`'x' ; DROP TABLE t`, "';'"},
		{`"a"; COMMIT`, "';'"},
		{`upper("v"`, "unclosed '('"},
		{`(("a")`, "unclosed '('"},
		{`"a")`, "unbalanced ')'"},
		{`'abc`, "unterminated string"},
		{`'abc''`, "unterminated string"},
		{`'abc\`, "unterminated string"},
		{`"abc`, "unterminated quoted identifier"},
		{`"a" -- trailing comment eats ) STORED`, "comment"},
		{`"a" /* unterminated`, "comment"},
		{`"a" /* closed */`, "comment"},
		{`"a" +-- x`, "comment"},
		{`$$x$$`, "'$'"},
		{`$q$ ); DROP TABLE t; $q$`, "'$'"},
		{`E'\'); DROP TABLE t; --'`, `\'`},
		{`'\'`, `\'`},
		{"'a\x00b'", "NUL"},
		{"\"a\" \x00", "NUL"},
		{"'\xff'", "UTF-8"},
	} {
		err := CheckExprFragment(tc.expr)
		require.Error(t, err, tc.expr)
		require.Contains(t, err.Error(), tc.why, tc.expr)
	}
}
