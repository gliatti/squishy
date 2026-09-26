package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Review finding: readString turned '\_' / '\%' into '_' / '%'. MySQL /
// MariaDB keep the backslash (outside a LIKE pattern '\_' is the two
// characters \_, inside one a literal _), so a hand-written
// `s = 'a\_b'` or `s LIKE 'a\_b'` changed meaning once emitted for PG
// ('a_b' matches 'axb'). \b and \Z are backspace and Control+Z.
func TestLexerStringEscapes(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{`'a\_b'`, `a\_b`},
		{`'100\%'`, `100\%`},
		{`'a\nb'`, "a\nb"},
		{`'a\bb'`, "a\bb"},
		{`'a\Zb'`, "a\x1ab"},
		{`'a\\_b'`, `a\_b`},
		{`'a\qb'`, `aqb`},
		{`'it''s'`, `it's`},
	} {
		tok := NewLexer(tc.src).Next()
		require.Equal(t, TOK_STRING, tok.Kind, tc.src)
		require.Equal(t, tc.want, tok.Lit, tc.src)
		require.Equal(t, tc.src, tok.Raw, tc.src)
	}
}
