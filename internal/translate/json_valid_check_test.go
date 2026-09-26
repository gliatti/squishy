package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// The MariaDB LONGTEXT + CHECK(JSON_VALID(col)) idiom is recognised on the
// typed CHECK expression (no text matching) and promoted to JSONB.
func TestMySQLJSONValidCheckPromotesToJSONB(t *testing.T) {
	cases := []struct {
		name  string
		check string
		want  bool
	}{
		{"backticked lowercase", "json_valid(`doc`)", true},
		{"uppercase bare", "JSON_VALID(doc)", true},
		{"extra parens", "((JSON_VALID((`doc`))))", true},
		{"other column", "json_valid(`other`)", false},
		{"other function", "json_type(`doc`) = 'OBJECT'", false},
		{"compound", "json_valid(`doc`) AND doc <> ''", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateMySQL(t, "CREATE TABLE t (id INT PRIMARY KEY, other LONGTEXT, doc LONGTEXT CHECK ("+tc.check+"));")
			require.Len(t, res.Plan.Tables, 1)
			var doc PGColumn
			for _, c := range res.Plan.Tables[0].Columns {
				if c.Name == "doc" {
					doc = c
				}
			}
			require.Equal(t, "doc", doc.Name)
			if tc.want {
				require.Equal(t, "JSONB", doc.Type)
				require.Empty(t, doc.Check)
			} else {
				require.NotEqual(t, "JSONB", doc.Type)
			}
		})
	}
}

func TestIsJSONValidCheckAST(t *testing.T) {
	call := &ast.FuncCall{Name: "json_valid", Args: []ast.Expr{&ast.Ident{Parts: []string{"Doc"}, Backtick: true}}}
	require.True(t, isJSONValidCheck(call, "doc"))
	require.True(t, isJSONValidCheck(&ast.ParenExpr{Inner: call}, "DOC"))
	require.False(t, isJSONValidCheck(nil, "doc"))
	require.False(t, isJSONValidCheck(&ast.FuncCall{Name: "JSON_VALID", Args: []ast.Expr{&ast.Ident{Parts: []string{"t", "doc"}}}}, "doc"))
	require.False(t, isJSONValidCheck(&ast.FuncCall{Name: "JSON_VALID"}, "doc"))
}
