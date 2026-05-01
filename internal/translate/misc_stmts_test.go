package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateTruncate(t *testing.T) {
	stmts, errs := mysql.Parse("TRUNCATE TABLE orders;")
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	pre := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, pre, `TRUNCATE TABLE "mig"."orders";`)
}

func TestTranslateRenameTableExpandsToOnePerPair(t *testing.T) {
	stmts, errs := mysql.Parse("RENAME TABLE a TO b, c TO d;")
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	pre := strings.Join(res.Plan.PreActions, "\n")
	require.Contains(t, pre, `ALTER TABLE "mig"."a" RENAME TO "b";`)
	require.Contains(t, pre, `ALTER TABLE "mig"."c" RENAME TO "d";`)
}
