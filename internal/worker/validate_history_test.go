package worker

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dataxfer"
	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/planner"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// Reviewer bug: the history copy of an emulated MariaDB system-versioned
// table had no row-count validation — validate only counted the source
// base tables, so a short or empty history copy went unnoticed. The
// validate payload the planner builds now carries the history tables,
// and each one is counted through the same `FOR SYSTEM_TIME ALL` history
// read as its copy.
func TestValidateChecksCountHistoryThroughSystemTimeSource(t *testing.T) {
	src := &inspect.SourceSchema{Database: "mdb", Tables: []inspect.ObjectSnapshot{
		{Name: "emp", Database: "mdb", Rows: 10},
		{Name: "dept", Database: "mdb", Rows: 3},
	}}
	pg := &translate.Result{Plan: translate.SchemaPlan{Tables: []translate.PGTable{
		{Schema: "mig", Name: "emp", SystemVersioning: &translate.PGSystemVersioning{
			RowStart: "rs", RowEnd: "re", HistoryTable: "emp_history2",
		}},
		{Schema: "mig", Name: "dept"},
		{Schema: "mig", Name: "emp_history2"},
	}}}
	plan := planner.Build(src, pg, planner.BuildOptions{TargetSchema: "mig"})
	var raw []byte
	for _, s := range plan.Steps {
		if s.Kind == "validate" {
			var err error
			raw, err = json.Marshal(s.Payload)
			require.NoError(t, err)
		}
	}
	require.NotNil(t, raw, "validate step expected")

	var p validatePayload
	require.NoError(t, json.Unmarshal(raw, &p))
	d := &Deps{SourceDialect: dataxfer.MySQLSource()}
	checks, err := d.validateChecks(p)
	require.NoError(t, err)
	require.Len(t, checks, 3)

	require.Equal(t, "emp", checks[0].TargetTable)
	require.Equal(t, "SELECT COUNT(*) FROM `mdb`.`emp`", checks[0].src.CountQuery(p.SourceSchema, checks[0].SourceTable))
	require.Equal(t, "dept", checks[1].TargetTable)

	h := checks[2]
	require.Equal(t, "emp", h.SourceTable)
	require.Equal(t, "emp_history2", h.TargetTable)
	require.Equal(t, dataxfer.MySQLSystemTimeHistorySource("re"), h.src)
	require.Equal(t, "SELECT COUNT(*) FROM `mdb`.`emp` FOR SYSTEM_TIME ALL WHERE `re` <= NOW(6)",
		h.src.CountQuery(p.SourceSchema, h.SourceTable))
}

// A payload without history tables (every non-MariaDB migration, and
// plans persisted before history validation existed) validates the base
// tables only; a malformed history entry, or one on a non-MariaDB
// source, is an error rather than a skipped check.
func TestValidateChecksHistoryEdgeCases(t *testing.T) {
	d := &Deps{SourceDialect: dataxfer.MySQLSource()}
	decode := func(raw string) validatePayload {
		var p validatePayload
		require.NoError(t, json.Unmarshal([]byte(raw), &p))
		return p
	}
	checks, err := d.validateChecks(decode(`{"tables":["a"],"source_schema":"s","target_schema":"mig"}`))
	require.NoError(t, err)
	require.Len(t, checks, 1)

	_, err = d.validateChecks(decode(`{"tables":[],"history_tables":[{"source_table":"a","target_table":"a_history"}]}`))
	require.Error(t, err, "missing row_end_col")

	_, err = d.validateChecks(decode(`{"tables":[],"history_tables":[{"source_table":"a","row_end_col":"re"}]}`))
	require.Error(t, err, "missing target_table")

	ora := &Deps{SourceDialect: dataxfer.OracleSource()}
	_, err = ora.validateChecks(decode(`{"tables":[],"history_tables":[{"source_table":"a","target_table":"a_history","row_end_col":"re"}]}`))
	require.Error(t, err)
}
