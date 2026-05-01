package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Oracle DECODE(expr, k1, v1, k2, v2, ..., default) gets expanded to a
// PG CASE expression. orafce only ships overloads up to 3 search/result
// pairs (7 args); type-introspection code routinely passes 5+ pairs and
// triggers `function oracle.decode(...) does not exist`. Lifting to
// CASE is independent of orafce and matches Oracle's "two NULLs match"
// semantic via `IS NOT DISTINCT FROM`.
func TestQualifyOracleDecode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "1 pair + default",
			in:   `x := decode(substr(name,1,1), '"', replace(name,'"',''), upper(name));`,
			want: `x := (CASE WHEN substr(name,1,1) IS NOT DISTINCT FROM '"' THEN replace(name,'"','') ELSE upper(name) END);`,
		},
		{
			name: "multi-pair, mixed case",
			in:   "y := DECODE(t, 'A','x','B','y','z');",
			want: "y := (CASE WHEN t IS NOT DISTINCT FROM 'A' THEN 'x' WHEN t IS NOT DISTINCT FROM 'B' THEN 'y' ELSE 'z' END);",
		},
		{
			name: "oracle-qualified prefix is absorbed",
			in:   "z := oracle.decode(t, 'A','x');",
			want: "z := (CASE WHEN t IS NOT DISTINCT FROM 'A' THEN 'x' END);",
		},
		{
			name: "user schema qualified — left alone",
			in:   "z := mig.decode(t,'A','x');",
			want: "z := mig.decode(t,'A','x');",
		},
		{
			name: "decode as identifier (no parens) — left alone",
			in:   "alias := some.column AS decode;",
			want: "alias := some.column AS decode;",
		},
		{
			name: "decode in string literal preserved",
			in:   "msg := 'see decode(x,y)';",
			want: "msg := 'see decode(x,y)';",
		},
		{
			name: "nested decode calls expand both",
			in:   "v := decode(a, 1, decode(b, 2, 'x', 'y'), 'z');",
			want: "v := (CASE WHEN a IS NOT DISTINCT FROM 1 THEN (CASE WHEN b IS NOT DISTINCT FROM 2 THEN 'x' ELSE 'y' END) ELSE 'z' END);",
		},
		{
			name: "12-arg decode (5 pairs + default) — the failing PRC_MAJ_TRC shape",
			in:   "v := DECODE(DATA_TYPE, 'DATE','DATE','CLOB','CLOB','ROWID','ROWID','NUMBER','NUMBER','TS','TS', DATA_TYPE||'(');",
			want: "v := (CASE WHEN DATA_TYPE IS NOT DISTINCT FROM 'DATE' THEN 'DATE' WHEN DATA_TYPE IS NOT DISTINCT FROM 'CLOB' THEN 'CLOB' WHEN DATA_TYPE IS NOT DISTINCT FROM 'ROWID' THEN 'ROWID' WHEN DATA_TYPE IS NOT DISTINCT FROM 'NUMBER' THEN 'NUMBER' WHEN DATA_TYPE IS NOT DISTINCT FROM 'TS' THEN 'TS' ELSE DATA_TYPE||'(' END);",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qualifyOracleDecode(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}
