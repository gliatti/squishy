package planner

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// tsqlStepsByKind indexes the steps of p by kind, then target.
func tsqlStepsByKind(p *Plan) map[string]map[string]*Step {
	out := map[string]map[string]*Step{}
	for i := range p.Steps {
		s := &p.Steps[i]
		if out[s.Kind] == nil {
			out[s.Kind] = map[string]*Step{}
		}
		out[s.Kind][s.Target] = s
	}
	return out
}

// A case-sensitive SQL Server database holding dbo.Orders and
// dbo.orders: the translator creates only the first (the second is the
// blocking name.fold_collision). The dropped snapshot must not be mapped
// case-insensitively onto the survivor — two COPYs into one PG table —
// so it gets no copy_table, create_index nor validate check.
func TestPlan_TSQLFoldCollisionNotCopiedTwice(t *testing.T) {
	src := &inspect.SourceSchema{Database: "cs_db", Tables: []inspect.ObjectSnapshot{
		{Name: "Orders", Schema: "dbo", Database: "cs_db", Rows: 4},
		{Name: "orders", Schema: "dbo", Database: "cs_db", Rows: 9},
	}}
	pg := &translate.Result{Plan: translate.SchemaPlan{
		Tables: []translate.PGTable{
			{Schema: "dbo", Name: "orders", SourceSchema: "dbo", SourceName: "Orders"},
		},
	}}
	p := Build(src, pg, BuildOptions{TargetSchema: "public", PreserveSourceSchemas: true})
	steps := tsqlStepsByKind(p)

	require.Len(t, steps["copy_table"], 1)
	c := steps["copy_table"]["dbo.Orders"]
	require.NotNil(t, c)
	require.Equal(t, "Orders", c.Payload["table"])
	require.Equal(t, "dbo", c.Payload["target_schema"])
	require.Nil(t, steps["copy_table"]["dbo.orders"])

	require.Len(t, steps["create_index"], 1)
	require.NotNil(t, steps["create_index"]["dbo.Orders"])

	var validate *Step
	for _, s := range steps["validate"] {
		validate = s
	}
	require.NotNil(t, validate)
	require.Equal(t, []ValidateTableCheck{
		{SourceSchema: "dbo", SourceTable: "Orders", TargetSchema: "dbo", TargetTable: "orders"},
	}, validate.Payload["table_checks"])

	// The match is exact both ways: the dropped table alone in the plan
	// is not matched by the survivor's snapshot either.
	pg.Plan.Tables[0].SourceName = "orders"
	p = Build(src, pg, BuildOptions{TargetSchema: "public", PreserveSourceSchemas: true})
	steps = tsqlStepsByKind(p)
	require.Len(t, steps["copy_table"], 1)
	require.NotNil(t, steps["copy_table"]["dbo.orders"])
}

// Snapshots without a schema (MySQL/MariaDB, Oracle, DB2) keep their
// exact-name match, and a table the translator does not know is still
// copied (historical behaviour).
func TestPlan_TSQLFixesLeaveOtherDialects(t *testing.T) {
	src := &inspect.SourceSchema{Database: "HR", Tables: []inspect.ObjectSnapshot{
		{Name: "EMPLOYEES", Database: "HR", Rows: 3},
		{Name: "unknown", Database: "HR", Rows: 1},
	}}
	pg := &translate.Result{Plan: translate.SchemaPlan{
		Tables: []translate.PGTable{{Schema: "mig", Name: "EMPLOYEES", CopyColumns: []string{"ID"}}},
	}}
	p := Build(src, pg, BuildOptions{TargetSchema: "mig"})
	steps := tsqlStepsByKind(p)
	require.Len(t, steps["copy_table"], 2)
	require.Equal(t, []string{"ID"}, steps["copy_table"]["EMPLOYEES"].Payload["columns"])
	require.Nil(t, steps["copy_table"]["unknown"].Payload["columns"])
}

// The history copy of an emulated SQL Server system-versioned table
// carries the history table's explicit source column list: the
// copier's catalog default would leave the rowversion / geography
// columns out while the PG history table declares them.
func TestPlan_TSQLTemporalHistoryCopyColumns(t *testing.T) {
	src := &inspect.SourceSchema{Database: "db", Tables: []inspect.ObjectSnapshot{
		{Name: "Emp", Schema: "dbo", Database: "db", Rows: 2},
	}}
	cols := []string{"id", "rv", "loc", "ValidFrom", "ValidTo"}
	pg := &translate.Result{Plan: translate.SchemaPlan{
		Tables: []translate.PGTable{
			{Schema: "dbo", Name: "emp", SourceSchema: "dbo", SourceName: "Emp", CopyColumns: cols,
				TSQLTemporal: &translate.PGTSQLTemporal{
					RowStart: "validfrom", RowEnd: "validto",
					HistorySchema: "dbo", HistoryTable: "emphistory",
					HistorySourceSchema: "dbo", HistorySourceTable: "EmpHistory", CopyHistory: true,
				}},
			{Schema: "dbo", Name: "emphistory", SourceSchema: "dbo", SourceName: "EmpHistory", CopyColumns: cols},
		},
	}}
	p := Build(src, pg, BuildOptions{TargetSchema: "public", PreserveSourceSchemas: true})
	steps := tsqlStepsByKind(p)
	h := steps["copy_table"]["dbo.EmpHistory"]
	require.NotNil(t, h)
	require.Equal(t, "EmpHistory", h.Payload["table"])
	require.Equal(t, "emphistory", h.Payload["target_table"])
	require.Equal(t, cols, h.Payload["columns"])
	require.Equal(t, cols, steps["copy_table"]["dbo.Emp"].Payload["columns"])

	// Without a copy list on the history table the payload stays as it
	// was (no "columns").
	pg.Plan.Tables[1].CopyColumns = nil
	p = Build(src, pg, BuildOptions{TargetSchema: "public", PreserveSourceSchemas: true})
	require.Nil(t, tsqlStepsByKind(p)["copy_table"]["dbo.EmpHistory"].Payload["columns"])
}
