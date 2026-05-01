package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Oracle accepts `#` in identifier names (MVT#ABT, IDX#MVT#xxx — common
// trace/journal naming). PG only allows `#` inside double-quoted
// identifiers; unquoted, it raises `syntax error at or near "#"`.
// Procedures that build DDL via concat (`'MVT#' || var`) emit the
// unquoted form and fail. The rewrite replaces `#` with `_` inside
// string literals only when surrounded by identifier-class bytes —
// free-text `#` (after a space, in a comment) is preserved.
func TestRewriteOracleHashInIdentLiterals(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "trailing # in identifier prefix",
			in:   "v := 'MVT#' || tbl;",
			want: "v := 'MVT_' || tbl;",
		},
		{
			name: "two # in IDX#MVT# prefix",
			in:   "v := 'IDX#MVT#' || x;",
			want: "v := 'IDX_MVT_' || x;",
		},
		{
			name: "leading # before identifier",
			in:   "v := '#abc';",
			want: "v := '_abc';",
		},
		{
			name: "between two identifier chars",
			in:   "v := 'A#B';",
			want: "v := 'A_B';",
		},
		{
			name: "free-text # is preserved",
			in:   "v := 'priority #1 task';",
			want: "v := 'priority #1 task';",
		},
		{
			name: "lone # in string left alone",
			in:   "v := '#';",
			want: "v := '#';",
		},
		{
			name: "# in unquoted code left alone (no string context)",
			in:   "x := y;",
			want: "x := y;",
		},
		{
			name: "escaped quotes inside literal",
			in:   "v := 'it''s MVT#table';",
			want: "v := 'it''s MVT_table';",
		},
		{
			name: "single-line SQL comment with # left alone",
			in:   "x := 1; -- bug #42\nmore;",
			want: "x := 1; -- bug #42\nmore;",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleHashInIdentLiterals(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// normalizeOracleIdent must apply the same `#→_` rule so the table/
// column names emitted by the data-migration path agree with what the
// procedure body produces at runtime.
func TestNormalizeOracleIdentReplacesHash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MVT#ABT", "mvt_abt"},
		{"IDX#MVT#FOO", "idx_mvt_foo"},
		{"trc#mixed", "trc_mixed"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, normalizeOracleIdent(tc.in))
		})
	}
}

// normalizeRawIdents (used on raw CHECK / DEFAULT expression text) must
// strip `#` from identifiers — both bare-word and quoted forms.
func TestNormalizeRawIdentsReplacesHash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"IDX#MVT#X = 1", "idx_mvt_x = 1"},
		{`"MVT#ABT"."COL#1"`, "mvt_abt.col_1"},
		{`"Mixed#Case"`, `"Mixed#Case"`}, // mixed-case quoted preserved
		{"'see #1 issue'", "'see #1 issue'"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, normalizeRawIdents(tc.in))
		})
	}
}
