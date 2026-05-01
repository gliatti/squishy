package translate

import (
	"strings"
	"testing"
)

func TestOuterJoinRewrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // substrings that must appear in output
	}{
		{
			name: "simple single LEFT",
			in: `SELECT a.x, b.y
FROM A a, B b
WHERE a.id = b.id(+)
  AND a.x > 5`,
			want: []string{"LEFT JOIN", "ON", "WHERE"},
		},
		{
			name: "two outer joins on different tables",
			in: `SELECT a.x, b.y, c.z
FROM A a, B b, C c
WHERE a.id = b.id(+)
  AND a.id2 = c.id2(+)`,
			want: []string{"LEFT JOIN B b", "LEFT JOIN C c"},
		},
		{
			name: "outer on left side flips alias direction",
			in: `SELECT a.x, b.y
FROM A a, B b
WHERE a.id(+) = b.id`,
			want: []string{"LEFT JOIN", "ON"},
		},
		{
			name: "no plus markers untouched",
			in: `SELECT a.x FROM A a, B b WHERE a.id = b.id`,
			want: []string{},
		},
		{
			name: "multi pred same pair aggregated to one ON",
			in: `SELECT a.x, b.y
FROM A a, B b
WHERE a.id  = b.id(+)
  AND a.id2 = b.id2(+)`,
			want: []string{"LEFT JOIN B b ON"},
		},
		{
			name: "subquery in WHERE",
			in: `SELECT a.x FROM A a
WHERE EXISTS (SELECT 1 FROM B b, C c WHERE b.id = c.id(+))`,
			want: []string{"LEFT JOIN"},
		},
		{
			// Mix of two-alias join predicate (b.id = c.id(+)) AND
			// single-alias filter (c.kind(+) = 'K'). The filter MUST
			// land in the LEFT JOIN's ON clause — leaving it in WHERE
			// would defeat the outer join semantics by suppressing
			// NULL-extended rows. Source: PRC_MAJ_JRN's TJO trigger
			// generator in the DRSE schema migration.
			name: "single alias plus filter moves to ON",
			in: `SELECT b.x, c.y
FROM B b, C c
WHERE b.id = c.id(+)
  AND c.kind(+) = 'K'
  AND c.owner(+) = upper('SCOTT')`,
			want: []string{"LEFT JOIN C c ON", "c.kind", "'K'", "c.owner", "upper('SCOTT')"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := rewriteOracleOuterJoin(tc.in)
			t.Logf("\n--- in ---\n%s\n--- out ---\n%s", tc.in, out)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("expected output to contain %q\nout:\n%s", w, out)
				}
			}
			// Output must NOT still contain `(+)` (we either translated it or left the input untouched when no plus markers existed).
			if strings.Contains(tc.in, "(+)") && strings.Contains(out, "(+)") {
				t.Errorf("output still contains (+) markers:\n%s", out)
			}
		})
	}
}
