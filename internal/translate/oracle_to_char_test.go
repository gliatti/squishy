package translate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// orafce ships single-arg `oracle.to_char(<type>)` (numeric, timestamp,
// integer, etc.). PG-native `pg_catalog.to_char` has 2-arg overloads
// only. Bare `to_char(x)` therefore fails to resolve; the translator
// must schema-qualify single-arg calls. Two-arg calls match pg_catalog
// directly and stay unqualified.
func TestQualifyOracleToCharSingleArg(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single-arg numeric",
			in:   "v := TO_CHAR(DATA_PRECISION);",
			want: "v := oracle.to_char(DATA_PRECISION);",
		},
		{
			name: "single-arg lowercase",
			in:   "v := to_char(x);",
			want: "v := oracle.to_char(x);",
		},
		{
			name: "two-arg call left alone",
			in:   "v := to_char(SYSDATE, 'YYYYMMDD');",
			want: "v := to_char(SYSDATE, 'YYYYMMDD');",
		},
		{
			name: "already qualified — left alone",
			in:   "v := oracle.to_char(x);",
			want: "v := oracle.to_char(x);",
		},
		{
			name: "user schema qualified — left alone",
			in:   "v := mig.to_char(x);",
			want: "v := mig.to_char(x);",
		},
		{
			name: "to_char as identifier (no parens) — left alone",
			in:   "alias := some.col AS to_char;",
			want: "alias := some.col AS to_char;",
		},
		{
			name: "to_char in string literal preserved",
			in:   "msg := 'see to_char(x)';",
			want: "msg := 'see to_char(x)';",
		},
		{
			name: "nested expression in single arg",
			in:   "v := TO_CHAR(a + b * c);",
			want: "v := oracle.to_char(a + b * c);",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qualifyOracleToCharSingleArg(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}
