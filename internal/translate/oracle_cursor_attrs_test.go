package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Oracle cursor attributes have no inline PG plpgsql syntax. The
// translator must rewrite the postfix form to the matching PG idiom:
//   - %NOTFOUND / %FOUND  → the special FOUND / NOT FOUND booleans
//   - %ISOPEN             → pg_cursors lookup
//   - %ROWCOUNT           → placeholder + TODO marker
//
// %TYPE and %ROWTYPE are PG-valid type-anchoring keywords — they MUST
// pass through untouched.
func TestRewriteOracleCursorAttributes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "EXIT WHEN cursor%NOTFOUND",
			in:   "EXIT WHEN c_TAB_TRC%NOTFOUND;",
			want: "EXIT WHEN NOT FOUND;",
		},
		{
			name: "IF cursor%NOTFOUND THEN",
			in:   "IF c_TAB_TRC%NOTFOUND THEN NULL; END IF;",
			want: "IF NOT FOUND THEN NULL; END IF;",
		},
		{
			name: "%FOUND in expression",
			in:   "IF cur%FOUND AND x > 0 THEN ...",
			want: "IF FOUND AND x > 0 THEN ...",
		},
		{
			name: "%ISOPEN with parens",
			in:   "IF (l_rc_curs%ISOPEN) THEN CLOSE l_rc_curs; END IF;",
			want: "IF (EXISTS (SELECT 1 FROM pg_cursors WHERE name = l_rc_curs::text)) THEN CLOSE l_rc_curs; END IF;",
		},
		{
			name: "%ROWCOUNT gets TODO placeholder",
			in:   "n := cur%ROWCOUNT;",
			want: "n := 0 /* TODO: replace with `GET DIAGNOSTICS x = ROW_COUNT` after the FETCH/EXECUTE referencing cur */;",
		},
		{
			name: "%TYPE is PG-valid — passthrough",
			in:   "v_id orders.id%TYPE;",
			want: "v_id orders.id%TYPE;",
		},
		{
			name: "%ROWTYPE is PG-valid — passthrough",
			in:   "v_row orders%ROWTYPE;",
			want: "v_row orders%ROWTYPE;",
		},
		{
			name: "qualified cursor name",
			in:   "EXIT WHEN pkg.cur%NOTFOUND;",
			want: "EXIT WHEN NOT FOUND;",
		},
		{
			name: "no % at all — passthrough",
			in:   "x := y + 1;",
			want: "x := y + 1;",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleCursorAttributes(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// End-to-end: the rewrite kicks in inside rewriteOracleExpr so the body
// translator picks it up automatically.
func TestRewriteOracleExprIncludesCursorAttrs(t *testing.T) {
	in := "EXIT WHEN c_TAB_TRC%NOTFOUND;"
	got := rewriteOracleExpr(in)
	require.False(t, strings.Contains(strings.ToUpper(got), "%NOTFOUND"),
		"raw %%NOTFOUND must be rewritten:\n%s", got)
	require.Contains(t, got, "NOT FOUND")
}
