package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Oracle's ROWNUM pseudo-column has no PG equivalent — the common
// "first N rows" idiom maps cleanly to PG's LIMIT clause when ROWNUM is
// the sole filter. Combined predicates and uses in SELECT list / ORDER
// BY require ROW_NUMBER() and are intentionally left untouched.
func TestRewriteOracleRownumLimit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "WHERE ROWNUM = 1",
			in:   "SELECT * FROM t WHERE ROWNUM = 1;",
			want: "SELECT * FROM t LIMIT 1;",
		},
		{
			name: "WHERE ROWNUM <= 10",
			in:   "SELECT * FROM t WHERE ROWNUM <= 10;",
			want: "SELECT * FROM t LIMIT 10;",
		},
		{
			name: "WHERE ROWNUM < 5 → LIMIT 4",
			in:   "SELECT * FROM t WHERE ROWNUM < 5;",
			want: "SELECT * FROM t LIMIT 4;",
		},
		{
			name: "WHERE ROWNUM = 1 INTO ...",
			in:   "SELECT a FROM (sub) WHERE ROWNUM = 1 INTO v;",
			want: "SELECT a FROM (sub) LIMIT 1 INTO v;",
		},
		{
			name: "combined with AND — left alone",
			in:   "SELECT * FROM t WHERE ROWNUM = 1 AND x = 2;",
			want: "SELECT * FROM t WHERE ROWNUM = 1 AND x = 2;",
		},
		{
			name: "combined with OR — left alone",
			in:   "SELECT * FROM t WHERE ROWNUM = 1 OR x = 2;",
			want: "SELECT * FROM t WHERE ROWNUM = 1 OR x = 2;",
		},
		{
			name: "ROWNUM in SELECT list — left alone",
			in:   "SELECT ROWNUM, name FROM t;",
			want: "SELECT ROWNUM, name FROM t;",
		},
		{
			name: "ROWNUM as column suffix — not matched",
			in:   "SELECT XROWNUM FROM t WHERE XROWNUM = 1;",
			want: "SELECT XROWNUM FROM t WHERE XROWNUM = 1;",
		},
		{
			name: "no ROWNUM at all — passthrough",
			in:   "SELECT * FROM t WHERE x = 1;",
			want: "SELECT * FROM t WHERE x = 1;",
		},
		{
			name: "lowercase rownum",
			in:   "SELECT * FROM t where rownum = 1;",
			want: "SELECT * FROM t LIMIT 1;",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleRownumLimit(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}
