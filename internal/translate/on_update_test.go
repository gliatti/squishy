package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// `ON UPDATE CURRENT_TIMESTAMP` must materialise as a PG BEFORE UPDATE
// trigger that touches the column on every UPDATE. The trigger function is
// created in pre-actions; the trigger itself in post-actions, so the table
// already exists by the time the trigger is bound.
func TestOnUpdateCurrentTimestampEmitsTrigger(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		stamped TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	post := strings.Join(res.Plan.PostActions, "\n")
	require.Contains(t, post, `CREATE OR REPLACE FUNCTION "mig".set_t_stamped()`,
		"trigger function must be emitted")
	require.Contains(t, post, `NEW."stamped" := now();`,
		"trigger body must update the timestamp column")
	require.Contains(t, post, `CREATE TRIGGER trg_t_stamped BEFORE UPDATE ON "mig"."t"`,
		"BEFORE UPDATE row trigger must be wired")
	require.Contains(t, post, `EXECUTE FUNCTION "mig".set_t_stamped()`,
		"trigger must call its emit function")
}

// Multiple ON UPDATE columns each emit their own dedicated trigger so they
// don't trample each other's columns.
func TestOnUpdateCurrentTimestampMultipleColumns(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		a TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		b TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	post := strings.Join(res.Plan.PostActions, "\n")
	require.Contains(t, post, `set_t_a()`)
	require.Contains(t, post, `set_t_b()`)
	require.Contains(t, post, `trg_t_a`)
	require.Contains(t, post, `trg_t_b`)
}

// A column with a DEFAULT but no ON UPDATE must NOT emit a trigger.
func TestOnUpdateAbsentDoesNotEmitTrigger(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		stamped TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	post := strings.Join(res.Plan.PostActions, "\n")
	require.NotContains(t, post, "set_t_stamped")
	require.NotContains(t, post, "trg_t_stamped")
}
