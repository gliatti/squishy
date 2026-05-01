// Package dataxfer implements the source → PostgreSQL data-transfer workhorse:
// partitioning a source table into ~10 000-row batches, then streaming SELECT
// results into pgx.CopyFrom. The partitioner and copier are source-dialect
// agnostic: quoting, placeholders and catalog queries are routed through a
// SourceDialect (see source_dialect.go).
package dataxfer

import (
	"context"
	"database/sql"
	"fmt"
)

// PartitionPlan describes how a source table is split into batches for the
// copy workers.
type PartitionPlan struct {
	Schema    string
	Table     string
	PK        []string // 1+ columns of the natural PK
	RangeKind string   // "pk" (numeric monotone) or "offset" (fallback)
	TotalRows int64
	BatchSize int
	Ranges    []Range // len(Ranges) == number of batches
}

type Range struct {
	Seq      int
	Low      map[string]any // pk→value, inclusive
	High     map[string]any // pk→value, inclusive
	RowCount int64
}

// BuildPartitionPlan inspects a source table and decides how to split it.
// If the PK is a single numeric column we scan by PK range; otherwise we
// fall back to OFFSET/LIMIT (slower but works on any table).
//
// The dialect argument selects the flavour of quoting, placeholders and
// catalog queries. Pass MySQLSource() or OracleSource().
func BuildPartitionPlan(ctx context.Context, db *sql.DB, d SourceDialect, schema, table string, batchSize int) (*PartitionPlan, error) {
	if batchSize <= 0 {
		batchSize = 10000
	}
	pp := &PartitionPlan{Schema: schema, Table: table, BatchSize: batchSize}

	// row count
	if err := db.QueryRowContext(ctx, d.CountQuery(schema, table)).Scan(&pp.TotalRows); err != nil {
		return nil, fmt.Errorf("count rows: %w", err)
	}

	pk, err := sourcePKColumns(ctx, db, d, schema, table)
	if err != nil {
		return nil, fmt.Errorf("pk cols: %w", err)
	}
	pp.PK = pk

	if pp.TotalRows == 0 {
		pp.RangeKind = "pk"
		return pp, nil
	}

	// numeric single-column PK → range partition
	if len(pp.PK) == 1 {
		pkCol := pp.PK[0]
		dt, err := sourceColumnDataType(ctx, db, d, schema, table, pkCol)
		if err == nil && d.IsNumericDataType(dt) {
			pp.RangeKind = "pk"
			if err := pp.computeNumericRanges(ctx, db, d, pkCol); err != nil {
				return nil, err
			}
			return pp, nil
		}
	}

	// fallback: OFFSET/LIMIT
	pp.RangeKind = "offset"
	n := int((pp.TotalRows + int64(batchSize) - 1) / int64(batchSize))
	for i := 0; i < n; i++ {
		off := int64(i) * int64(batchSize)
		lim := int64(batchSize)
		if off+lim > pp.TotalRows {
			lim = pp.TotalRows - off
		}
		pp.Ranges = append(pp.Ranges, Range{
			Seq:      i,
			Low:      map[string]any{"offset": off},
			High:     map[string]any{"limit": lim},
			RowCount: lim,
		})
	}
	return pp, nil
}

func (pp *PartitionPlan) computeNumericRanges(ctx context.Context, db *sql.DB, d SourceDialect, pkCol string) error {
	var minV, maxV int64
	err := db.QueryRowContext(ctx, d.MinMaxQuery(pp.Schema, pp.Table, pkCol)).Scan(&minV, &maxV)
	if err != nil {
		return fmt.Errorf("min/max: %w", err)
	}
	low := minV
	seq := 0
	bs := int64(pp.BatchSize)
	for low <= maxV {
		high := low + bs - 1
		if high > maxV {
			high = maxV
		}
		pp.Ranges = append(pp.Ranges, Range{
			Seq:  seq,
			Low:  map[string]any{pkCol: low},
			High: map[string]any{pkCol: high},
		})
		seq++
		low = high + 1
	}
	return nil
}
