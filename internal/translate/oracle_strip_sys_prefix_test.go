package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// PG has no `sys` schema; Oracle qualifies system packages and views with
// `SYS.` for emphasis. Stripping the prefix lets PG resolve the identifier
// against the actual search_path (orafce schemas, application views, …).
func TestRewriteOracleStripSysPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "package call",
			in:   "RAISE NOTICE '%', sys.DBMS_UTILITY.format_call_stack;",
			want: "RAISE NOTICE '%', DBMS_UTILITY.format_call_stack;",
		},
		{
			name: "data dictionary view",
			in:   "SELECT count(*) FROM sys.ALL_OBJECTS WHERE owner = 'X';",
			want: "SELECT count(*) FROM ALL_OBJECTS WHERE owner = 'X';",
		},
		{
			name: "lowercase + whitespace around dot",
			in:   "x := sys . dbms_output.put_line('hi');",
			want: "x := dbms_output.put_line('hi');",
		},
		{
			name: "multiple sys-prefixed forms in same expr",
			in:   "RAISE NOTICE '%', sys.DBMS_UTILITY.x || sys.STANDARD.y;",
			want: "RAISE NOTICE '%', DBMS_UTILITY.x || STANDARD.y;",
		},
		{
			name: "bare SYS token without dotted suffix is left alone",
			in:   "v_role := 'SYS';",
			want: "v_role := 'SYS';",
		},
		{
			name: "string literal containing sys. is preserved",
			in:   "msg := 'see sys.dba_users';",
			want: "msg := 'see sys.dba_users';",
		},
		{
			name: "ident containing SYS as a substring not stripped",
			in:   "x := sys_context('USERENV','LANG');",
			want: "x := sys_context('USERENV','LANG');",
		},
		{
			name: "trailing SYS with no dot left alone",
			in:   "v := SYS;",
			want: "v := SYS;",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleStripSysPrefix(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// End-to-end through rewriteOracleExpr: the SYS prefix must be gone before
// the dbms_utility / sysdate passes run, and the canonical orafce form must
// emerge.
func TestRewriteOracleExprStripsSysPrefixBeforePackageRewrites(t *testing.T) {
	in := "RAISE NOTICE '%', sys.DBMS_UTILITY.format_call_stack;"
	got := rewriteOracleExpr(in)
	require.NotContains(t, strings.ToLower(got), "sys.dbms_utility")
	require.Contains(t, got, "dbms_utility.format_call_stack()")
}
