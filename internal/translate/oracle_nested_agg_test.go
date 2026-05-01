package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Oracle accepts MIN(COUNT(...)) etc. directly when GROUP BY is present.
// PG rejects nested aggregates and demands a derived table. The translator
// must lift the inner aggregate into a sub-SELECT.
func TestRewriteOracleNestedAggregates(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustHave []string
		mustMiss []string
	}{
		{
			name: "MIN(COUNT) with GROUP BY in scalar subquery",
			in: `(SELECT MIN(COUNT(C1.COLUMN_NAME)) ` +
				`FROM idx I1 ` +
				`WHERE I1.UNIQUENESS = 'UNIQUE' ` +
				`GROUP BY I1.INDEX_NAME)`,
			mustHave: []string{
				"SELECT min(_nag)",
				"SELECT count(C1.COLUMN_NAME) AS _nag",
				"GROUP BY I1.INDEX_NAME",
				") _nag_t",
			},
			mustMiss: []string{"MIN(COUNT(", "min(count("},
		},
		{
			name: "MAX(SUM) with GROUP BY",
			in:   "(SELECT MAX(SUM(c.amount)) FROM orders o GROUP BY o.id)",
			mustHave: []string{
				"SELECT max(_nag)",
				"SELECT sum(c.amount) AS _nag",
				"FROM orders o",
				"GROUP BY o.id",
			},
			mustMiss: []string{"MAX(SUM("},
		},
		{
			name: "no GROUP BY — still rewrites (scalar over single agg)",
			// Even without GROUP BY this still nests — Oracle allows it
			// for scalar contexts. PG rejects, so wrap anyway.
			in: "(SELECT MIN(COUNT(c)) FROM t)",
			mustHave: []string{
				"SELECT min(_nag)",
				"SELECT count(c) AS _nag",
			},
			mustMiss: []string{"MIN(COUNT("},
		},
		{
			name: "non-aggregate outer call — left alone",
			in:   "SELECT my_func(COUNT(c)) FROM t GROUP BY x;",
			mustHave: []string{
				"my_func(COUNT(c))",
			},
			mustMiss: []string{"_nag"},
		},
		{
			name: "non-aggregate inner call — left alone",
			in:   "SELECT MAX(some_func(c)) FROM t GROUP BY x;",
			mustHave: []string{
				"MAX(some_func(c))",
			},
			mustMiss: []string{"_nag"},
		},
		{
			name: "no SELECT at all — passthrough",
			in:   "x := MIN(COUNT(y));",
			mustHave: []string{
				"MIN(COUNT(y))",
			},
			mustMiss: []string{"_nag"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleNestedAggregates(tc.in)
			for _, want := range tc.mustHave {
				require.True(t, strings.Contains(got, want),
					"output missing %q\noutput:\n%s", want, got)
			}
			for _, miss := range tc.mustMiss {
				require.False(t, strings.Contains(got, miss),
					"output unexpectedly contains %q\noutput:\n%s", miss, got)
			}
		})
	}
}
