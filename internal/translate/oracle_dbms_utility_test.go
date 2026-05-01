package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// orafce ships dbms_utility.format_call_stack() in schema dbms_utility.
// Oracle code uses [SYS.]DBMS_UTILITY.FORMAT_CALL_STACK with no parens or
// with `()`. Without the rewrite, PG parses the dotted form as a 3-part
// column reference and raises "missing FROM-clause entry for table
// dbms_utility". The rewrite must always emit the canonical
// `dbms_utility.format_call_stack()` form.
func TestRewriteOracleDbmsUtilityFormatCallStack(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare with sys prefix and no parens",
			in:   "RAISE NOTICE '%', sys.DBMS_UTILITY.format_call_stack;",
			want: "RAISE NOTICE '%', dbms_utility.format_call_stack();",
		},
		{
			name: "bare without sys prefix, no parens",
			in:   "x := DBMS_UTILITY.FORMAT_CALL_STACK;",
			want: "x := dbms_utility.format_call_stack();",
		},
		{
			name: "with explicit empty parens",
			in:   "x := DBMS_UTILITY.FORMAT_CALL_STACK();",
			want: "x := dbms_utility.format_call_stack();",
		},
		{
			name: "sys prefix with mixed case",
			in:   "log := Sys.Dbms_Utility.Format_Call_Stack;",
			want: "log := dbms_utility.format_call_stack();",
		},
		{
			name: "bare FORMAT_CALL_STACK without DBMS_UTILITY qualifier left alone",
			in:   "x := FORMAT_CALL_STACK;",
			want: "x := FORMAT_CALL_STACK;",
		},
		{
			name: "non-CALL_STACK identifiers untouched",
			in:   "x := my_format_call_stack();",
			want: "x := my_format_call_stack();",
		},
		{
			name: "FORMAT_ERROR_BACKTRACE → '' (no PG analog)",
			in:   "RAISE NOTICE '%', DBMS_UTILITY.format_error_backtrace;",
			want: "RAISE NOTICE '%', '';",
		},
		{
			name: "FORMAT_ERROR_BACKTRACE with parens stripped",
			in:   "x := DBMS_UTILITY.FORMAT_ERROR_BACKTRACE();",
			want: "x := '';",
		},
		{
			name: "FORMAT_ERROR_STACK → SQLERRM (PG built-in)",
			in:   "x := DBMS_UTILITY.FORMAT_ERROR_STACK;",
			want: "x := SQLERRM;",
		},
		{
			name: "all three in one expression",
			in:   "x := DBMS_UTILITY.FORMAT_CALL_STACK || DBMS_UTILITY.FORMAT_ERROR_STACK || DBMS_UTILITY.FORMAT_ERROR_BACKTRACE;",
			want: "x := dbms_utility.format_call_stack() || SQLERRM || '';",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleDbmsUtilityFormatCallStack(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// End-to-end through rewriteOracleExpr: confirm the rewrite stacks with
// the surrounding Oracle-idiom passes (e.g. the SYSDATE substitution).
func TestRewriteOracleExprIncludesFormatCallStack(t *testing.T) {
	in := "RAISE NOTICE '%', sys.DBMS_UTILITY.format_call_stack || ' at ' || SYSDATE;"
	got := rewriteOracleExpr(in)
	require.Contains(t, got, "dbms_utility.format_call_stack()")
	require.NotContains(t, strings.ToUpper(got), "SYS.DBMS_UTILITY")
	require.Contains(t, got, "CURRENT_TIMESTAMP::timestamp(0)")
}
