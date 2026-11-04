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

// SQL Server: the validate step counted nothing (tables is empty for a
// multi-schema database and table_checks was not read). Each table is
// now counted in its own source and target schema, the history table of
// an emulated system-versioned table included.
func TestValidateChecksSQLServerTableChecks(t *testing.T) {
	src := &inspect.SourceSchema{Database: "db", Tables: []inspect.ObjectSnapshot{
		{Name: "Employees", Schema: "dbo", Database: "db", Rows: 4},
		{Name: "Order Details", Schema: "sales", Database: "db", Rows: 7},
	}}
	pg := &translate.Result{Plan: translate.SchemaPlan{Tables: []translate.PGTable{
		{Schema: "dbo", Name: "employees", SourceSchema: "dbo", SourceName: "Employees", TSQLTemporal: &translate.PGTSQLTemporal{
			RowStart: "valid_from", RowEnd: "valid_to",
			HistorySchema: "dbo", HistoryTable: "employeeshistory",
			HistorySourceSchema: "dbo", HistorySourceTable: "EmployeesHistory", CopyHistory: true,
		}},
		{Schema: "sales", Name: "order details", SourceSchema: "sales", SourceName: "Order Details"},
		{Schema: "dbo", Name: "employeeshistory", SourceSchema: "dbo", SourceName: "EmployeesHistory"},
	}}}
	plan := planner.Build(src, pg, planner.BuildOptions{TargetSchema: "public", PreserveSourceSchemas: true})
	var raw []byte
	copies := map[string]map[string]any{}
	for _, s := range plan.Steps {
		switch s.Kind {
		case "validate":
			var err error
			raw, err = json.Marshal(s.Payload)
			require.NoError(t, err)
		case "copy_table":
			copies[s.Target] = s.Payload
		}
	}
	require.NotNil(t, raw, "validate step expected")
	// The history table is copied from the source history table.
	require.Equal(t, "EmployeesHistory", copies["dbo.EmployeesHistory"]["table"])
	require.Equal(t, "employeeshistory", copies["dbo.EmployeesHistory"]["target_table"])

	var p validatePayload
	require.NoError(t, json.Unmarshal(raw, &p))
	d := &Deps{SourceDialect: dataxfer.SQLServerSource()}
	checks, err := d.validateChecks(p)
	require.NoError(t, err)
	require.Len(t, checks, 3)
	require.Equal(t, "SELECT COUNT_BIG(*) FROM [sales].[Order Details]", checks[1].src.CountQuery(checks[1].SourceSchema, checks[1].SourceTable))
	require.Equal(t, "sales", checks[1].TargetSchema)
	require.Equal(t, "order details", checks[1].TargetTable)
	require.Equal(t, "EmployeesHistory", checks[2].SourceTable)
	require.Equal(t, "employeeshistory", checks[2].TargetTable)

	_, err = d.validateChecks(func() validatePayload {
		var q validatePayload
		require.NoError(t, json.Unmarshal([]byte(`{"tables":[],"table_checks":[{"source_table":"a","target_table":"a"}]}`), &q))
		return q
	}())
	require.Error(t, err, "a table check without schemas")
}
