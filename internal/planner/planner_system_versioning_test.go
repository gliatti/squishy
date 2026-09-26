package planner

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// An emulated MariaDB system-versioned table gets a second copy step that
// reads its closed versions into the history table; create_fk (which
// installs the versioning triggers) waits for it.
func TestPlan_SystemVersionedHistoryCopy(t *testing.T) {
	src := &inspect.SourceSchema{Database: "mariadb_features", Tables: []inspect.ObjectSnapshot{
		{Name: "employees", Database: "mariadb_features", Rows: 10},
	}}
	pg := &translate.Result{Plan: translate.SchemaPlan{Tables: []translate.PGTable{
		{Schema: "public", Name: "employees", SystemVersioning: &translate.PGSystemVersioning{
			RowStart: "valid_from", RowEnd: "valid_to", HistoryTable: "employees_history",
		}},
		{Schema: "public", Name: "employees_history"},
	}}}

	p := Build(src, pg, BuildOptions{TargetSchema: "public"})
	var hist, fk *Step
	var copies int
	for i := range p.Steps {
		s := &p.Steps[i]
		switch {
		case s.Kind == "copy_table":
			copies++
			if s.Target == "employees_history" {
				hist = s
			}
		case s.Kind == "create_fk":
			fk = s
		}
	}
	require.Equal(t, 2, copies)
	require.NotNil(t, hist)
	require.Equal(t, "mariadb_features", hist.Payload["schema"])
	require.Equal(t, "employees", hist.Payload["table"])
	require.Equal(t, "employees_history", hist.Payload["target_table"])
	require.Equal(t, "system_time_history", hist.Payload["source_variant"])
	require.Equal(t, "valid_to", hist.Payload["row_end_col"])
	require.Equal(t, "public", hist.Payload["target_schema"])

	// create_fk depends on the history table's create_index step, which
	// depends on the history copy.
	require.NotNil(t, fk)
	var histIdx *Step
	for i := range p.Steps {
		if p.Steps[i].Kind == "create_index" && p.Steps[i].Target == "employees_history" {
			histIdx = &p.Steps[i]
		}
	}
	require.NotNil(t, histIdx)
	require.Contains(t, histIdx.DependsOn, hist.ID)
	require.Contains(t, fk.DependsOn, histIdx.ID)

	// SkipData drops the history copy with the other copies.
	p = Build(src, pg, BuildOptions{TargetSchema: "public", SkipData: true})
	for _, s := range p.Steps {
		require.NotEqual(t, "copy_table", s.Kind)
	}
}

// The validate step lists every emulated history table with its source
// table and ROW END column, so the worker can count the source history
// rows and compare them with the history copy.
func TestPlan_ValidateCoversHistoryTables(t *testing.T) {
	src := &inspect.SourceSchema{Database: "mariadb_features", Tables: []inspect.ObjectSnapshot{
		{Name: "employees", Database: "mariadb_features", Rows: 10},
		{Name: "plain", Database: "mariadb_features", Rows: 1},
	}}
	pg := &translate.Result{Plan: translate.SchemaPlan{Tables: []translate.PGTable{
		{Schema: "public", Name: "employees", SystemVersioning: &translate.PGSystemVersioning{
			RowStart: "valid_from", RowEnd: "valid_to", HistoryTable: "employees_history",
		}},
		{Schema: "public", Name: "plain"},
		{Schema: "public", Name: "employees_history"},
	}}}
	for _, skip := range []bool{false, true} {
		p := Build(src, pg, BuildOptions{TargetSchema: "public", SkipData: skip})
		var validate *Step
		for i := range p.Steps {
			if p.Steps[i].Kind == "validate" {
				validate = &p.Steps[i]
			}
		}
		require.NotNil(t, validate)
		require.Equal(t, []string{"employees", "plain"}, validate.Payload["tables"])
		require.Equal(t, []ValidateHistoryTable{{
			SourceTable: "employees", TargetTable: "employees_history", RowEndCol: "valid_to",
		}}, validate.Payload["history_tables"])
	}

	// No system-versioned table: an empty list, never nil.
	p := Build(src, &translate.Result{}, BuildOptions{TargetSchema: "public"})
	for _, s := range p.Steps {
		if s.Kind == "validate" {
			require.Equal(t, []ValidateHistoryTable{}, s.Payload["history_tables"])
		}
	}
}
