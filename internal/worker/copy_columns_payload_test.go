package worker

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dataxfer"
)

// The explicit column list of a copy_table step (set by the planner for
// an emulated MariaDB system-versioned table) reaches every copy_batch
// job, which hands it to the copier; without one the batch keeps the
// copier's catalog default.
func TestCopyBatchPayloadCarriesColumns(t *testing.T) {
	var step copyTablePayload
	require.NoError(t, json.Unmarshal([]byte(`{"schema":"probe","table":"t_gen","target_schema":"probe",
		"target_table":"t_gen_history","source_variant":"system_time_history","row_end_col":"re",
		"columns":["id","a","g","v","rs","re"]}`), &step))
	require.NoError(t, step.validate())

	raw, err := json.Marshal(newCopyBatchPayload(step, "t_gen_history", "pk",
		dataxfer.Range{Low: map[string]any{"id": 1}, High: map[string]any{"id": 3}}))
	require.NoError(t, err)
	var batch copyBatchPayload
	require.NoError(t, json.Unmarshal(raw, &batch))
	require.Equal(t, []string{"id", "a", "g", "v", "rs", "re"}, batch.Columns)
	require.Equal(t, "t_gen_history", batch.DstTable)
	require.Equal(t, "t_gen", batch.SrcTable)
	require.Equal(t, "system_time_history", batch.SourceVariant)
	require.Equal(t, "re", batch.RowEndCol)

	// No list: the batch payload has none either (catalog default).
	step = copyTablePayload{Schema: "probe", Table: "t_trx", TargetSchema: "probe"}
	require.NoError(t, step.validate())
	raw, err = json.Marshal(newCopyBatchPayload(step, "t_trx", "pk", dataxfer.Range{}))
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"columns"`)
	batch = copyBatchPayload{}
	require.NoError(t, json.Unmarshal(raw, &batch))
	require.Empty(t, batch.Columns)
}

// The history copy without an explicit column list would fall back to
// the catalog default, which skips generated columns the history table
// stores: refused rather than copied as NULL.
func TestCopyTablePayloadHistoryNeedsColumns(t *testing.T) {
	p := copyTablePayload{Schema: "probe", Table: "t_gen", SourceVariant: copySourceVariantSystemTimeHistory, RowEndCol: "re"}
	require.Error(t, p.validate())
	p.Columns = []string{"id", "re"}
	require.NoError(t, p.validate())
}

// The per-table source metadata resolved at partition time (order key of
// the keyed OFFSET paging, server-side column conversions) rides in every
// copy_batch payload and reaches the copier, which then skips the source
// catalog; a payload enqueued before these fields existed leaves
// MetaResolved false so the copier looks them up as before.
func TestCopyBatchPayloadCarriesPlanMeta(t *testing.T) {
	step := copyTablePayload{Schema: "Production", Table: "Document", TargetSchema: "production"}
	plan := &dataxfer.PartitionPlan{
		RangeKind:   "offset",
		OrderKey:    []string{"DocumentNode"},
		SourceTypes: map[string]string{"DocumentNode": "hierarchyid"},
	}
	r := dataxfer.Range{Low: map[string]any{"offset": int64(10000)}, High: map[string]any{"limit": int64(10000)}}
	raw, err := json.Marshal(newCopyBatchPayload(step, "document", plan.RangeKind, r).withPlanMeta(plan))
	require.NoError(t, err)
	var batch copyBatchPayload
	require.NoError(t, json.Unmarshal(raw, &batch))
	o := batch.copyOpts()
	require.True(t, o.MetaResolved)
	require.Equal(t, []string{"DocumentNode"}, o.OrderKey)
	require.Equal(t, map[string]string{"DocumentNode": "hierarchyid"}, o.SourceTypes)
	require.Equal(t, "Production", o.SrcSchema)
	require.Equal(t, "Document", o.SrcTable)
	require.Equal(t, "production", o.DstSchema)
	require.Equal(t, "document", o.DstTable)
	require.Equal(t, "offset", o.RangeKind)

	// A heap without key: resolved, and resolved to "no key" (one batch).
	raw, err = json.Marshal(newCopyBatchPayload(step, "document", "offset", r).
		withPlanMeta(&dataxfer.PartitionPlan{RangeKind: "offset"}))
	require.NoError(t, err)
	batch = copyBatchPayload{}
	require.NoError(t, json.Unmarshal(raw, &batch))
	o = batch.copyOpts()
	require.True(t, o.MetaResolved)
	require.Empty(t, o.OrderKey)
	require.Empty(t, o.SourceTypes)

	// A payload from before the change: the copier falls back to lookups.
	batch = copyBatchPayload{}
	require.NoError(t, json.Unmarshal([]byte(`{"src_schema":"dbo","src_table":"T","dst_schema":"dbo","dst_table":"t",
		"range_kind":"offset","low":{"offset":0},"high":{"limit":10}}`), &batch))
	o = batch.copyOpts()
	require.False(t, o.MetaResolved)
	require.Nil(t, o.OrderKey)
	require.Nil(t, o.SourceTypes)
}
