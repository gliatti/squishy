package translate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateCompressedColumnEmitsToastNote(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, body TEXT COMPRESSED);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.Len(t, res.Plan.Tables, 1)

	var found bool
	for _, e := range res.Explanations {
		if e.Source == "COMPRESSED column" && e.Object == "t.body" {
			found = true
			require.Equal(t, "info", e.Level)
		}
	}
	require.True(t, found, "expected COMPRESSED column explanation")
}
