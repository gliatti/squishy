package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// CreateTrigger.Events / UpdateOf render `UPDATE OF col, …` attached to
// the UPDATE event (the legacy Event string is upper-cased whole, which
// would mangle quoted column names).
func TestWriteCreateTrigger_EventsWithUpdateOf(t *testing.T) {
	out := Write([]Stmt{&CreateTrigger{
		Name: "t_trg", Timing: "AFTER", Events: []string{"UPDATE", "DELETE"},
		UpdateOf: []string{"id", "Mixed"},
		Schema:   "mig", Table: "t", FnName: "t_fn",
	}})
	require.Contains(t, out, `CREATE TRIGGER "t_trg" AFTER UPDATE OF "id", "Mixed" OR DELETE ON "mig"."t"`)

	out = Write([]Stmt{&CreateTrigger{
		Name: "t_trg", Timing: "BEFORE", Events: []string{"INSERT", "UPDATE"},
		Schema: "mig", Table: "t", FnName: "t_fn",
	}})
	require.Contains(t, out, `CREATE TRIGGER "t_trg" BEFORE INSERT OR UPDATE ON "mig"."t"`)
}
