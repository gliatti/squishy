package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A MySQL `FOLLOWS other` trigger whose name sorts before `other` is
// renamed so PG's alphabetical firing order matches. The rename lives on
// the CREATE TRIGGER node: an identifier in the body that merely shares
// the trigger's name (here the audit table a_log) is left alone, and the
// backing function keeps its <original>_fn name.
func TestTriggerOrderRename_InAST(t *testing.T) {
	res := translateMySQL(t, "CREATE TABLE t (id INT PRIMARY KEY);\n"+
		"CREATE TABLE a_log (id INT);\n"+
		"CREATE TRIGGER b_first AFTER INSERT ON t FOR EACH ROW SET @x = 1;\n"+
		"CREATE TRIGGER a_log AFTER INSERT ON t FOR EACH ROW FOLLOWS b_first INSERT INTO a_log (id) VALUES (NEW.id);")

	var ddl, name string
	for _, r := range res.Plan.Routines {
		if r.Kind == "trigger" && strings.Contains(r.DDL, `"a_log_fn"`) {
			ddl, name = r.DDL, r.Name
		}
	}
	require.NotEmpty(t, ddl, "trigger a_log not emitted: %+v", res.Plan.Routines)
	t.Logf("DDL:\n%s", ddl)
	require.Equal(t, "zz_a_log", name)
	require.Contains(t, ddl, `CREATE TRIGGER "zz_a_log"`)
	require.Contains(t, ddl, `INSERT INTO "a_log" ("id") VALUES (NEW."id");`)
	require.NotContains(t, ddl, `INSERT INTO "zz_a_log"`)
	require.NotContains(t, ddl, `CREATE TRIGGER "a_log"`)
}
