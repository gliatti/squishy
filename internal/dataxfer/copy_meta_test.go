package dataxfer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// recDB is a database/sql connector that records every query and its
// arguments and answers them from a table keyed by the exact query text
// (an unknown query gets an empty result). It stands in for the source
// database, so a test can prove which catalog round trips a copy makes.
type recDB struct {
	mu      sync.Mutex
	queries []string
	args    [][]any
	answers map[string]recAnswer
}

type recAnswer struct {
	cols []string
	rows [][]driver.Value
}

func newRecDB(answers map[string]recAnswer) (*recDB, *sql.DB) {
	r := &recDB{answers: answers}
	return r, sql.OpenDB(r)
}

func (r *recDB) Connect(context.Context) (driver.Conn, error) { return recConn{r}, nil }
func (r *recDB) Driver() driver.Driver                        { return recDriver{r} }

func (r *recDB) logged() ([]string, [][]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...), append([][]any(nil), r.args...)
}

type recDriver struct{ r *recDB }

func (d recDriver) Open(string) (driver.Conn, error) { return recConn{d.r}, nil }

type recConn struct{ r *recDB }

func (recConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("recDB: no prepare") }
func (recConn) Close() error                        { return nil }
func (recConn) Begin() (driver.Tx, error)           { return nil, errors.New("recDB: no tx") }

func (c recConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	c.r.mu.Lock()
	c.r.queries = append(c.r.queries, q)
	c.r.args = append(c.r.args, vals)
	a := c.r.answers[q]
	c.r.mu.Unlock()
	cols := a.cols
	if cols == nil {
		cols = []string{"c"}
	}
	return &recRows{cols: cols, rows: a.rows}, nil
}

type recRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *recRows) Columns() []string { return r.cols }
func (r *recRows) Close() error      { return nil }
func (r *recRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// A copy_batch whose payload carries the order key and the source column
// types resolved at partition time makes no source catalog round trip:
// the only source query is the batch SELECT itself, built exactly as the
// catalog lookups would have built it (offset bound first).
func TestCopyBatchResolvedMetaSkipsSourceCatalog(t *testing.T) {
	ctx := context.Background()
	cols := []string{"Id", "Node", "Name"}
	targetTypes := []string{"int4", "text", "text"}
	want := sqlserverDialect{}.withTypes(cols, map[string]string{"Node": "hierarchyid"}, targetTypes).
		SelectOffsetOrderedQuery("dbo", "Doc", cols, []string{"Code"})

	rec, db := newRecDB(nil)
	defer db.Close()
	o := CopyOpts{
		SourceDB: db, SourceDialect: SQLServerSource(),
		SrcSchema: "dbo", SrcTable: "Doc", Columns: cols,
		RangeKind: "offset",
		Low:       map[string]any{"offset": int64(20000)},
		High:      map[string]any{"limit": int64(10000)},

		MetaResolved: true,
		OrderKey:     []string{"Code"},
		SourceTypes:  map[string]string{"Node": "hierarchyid"},
	}
	require.NoError(t, o.prepareSource(ctx, targetTypes))
	rows, err := o.selectSource(ctx)
	require.NoError(t, err)
	require.NoError(t, rows.Close())

	queries, args := rec.logged()
	require.Equal(t, []string{want}, queries, "only the batch SELECT reaches the source")
	require.Equal(t, []any{int64(20000), int64(10000)}, args[0])

	// Resolved with no key (a heap the partitioner made a single batch):
	// still no lookup, plain OFFSET query.
	rec, db2 := newRecDB(nil)
	defer db2.Close()
	o = CopyOpts{
		SourceDB: db2, SourceDialect: SQLServerSource(),
		SrcSchema: "dbo", SrcTable: "Heap", Columns: []string{"a"},
		RangeKind: "offset",
		Low:       map[string]any{"offset": int64(0)},
		High:      map[string]any{"limit": int64(5)},

		MetaResolved: true,
	}
	require.NoError(t, o.prepareSource(ctx, []string{"text"}))
	rows, err = o.selectSource(ctx)
	require.NoError(t, err)
	require.NoError(t, rows.Close())
	queries, _ = rec.logged()
	require.Equal(t, []string{SQLServerSource().SelectOffsetQuery("dbo", "Heap", []string{"a"})}, queries)
}

// Without the resolved metadata (a copy_batch enqueued before it was
// carried) the copier falls back to the catalog lookups and builds the
// same batch SELECT.
func TestCopyBatchUnresolvedMetaFallsBackToCatalog(t *testing.T) {
	ctx := context.Background()
	d := SQLServerSource()
	cols := []string{"Id", "Node", "Name"}
	targetTypes := []string{"int4", "text", "text"}
	want := sqlserverDialect{}.withTypes(cols, map[string]string{"Node": "hierarchyid"}, targetTypes).
		SelectOffsetOrderedQuery("dbo", "Doc", cols, []string{"Code"})

	rec, db := newRecDB(map[string]recAnswer{
		sqlserverColumnTypesQuery: {cols: []string{"n", "t"}, rows: [][]driver.Value{
			{"Id", "int"}, {"Node", "hierarchyid"}, {"Name", "nvarchar"},
		}},
		d.(uniqueKeyDialect).UniqueKeyColumnsQuery(): {rows: [][]driver.Value{{"Code"}}},
	})
	defer db.Close()
	o := CopyOpts{
		SourceDB: db, SourceDialect: d,
		SrcSchema: "dbo", SrcTable: "Doc", Columns: cols,
		RangeKind: "offset",
		Low:       map[string]any{"offset": int64(0)},
		High:      map[string]any{"limit": int64(10000)},
	}
	require.NoError(t, o.prepareSource(ctx, targetTypes))
	rows, err := o.selectSource(ctx)
	require.NoError(t, err)
	require.NoError(t, rows.Close())

	queries, _ := rec.logged()
	require.Equal(t, []string{
		sqlserverColumnTypesQuery,
		d.PKColumnsQuery(),
		d.(uniqueKeyDialect).UniqueKeyColumnsQuery(),
		want,
	}, queries)
}

// BuildPartitionPlan resolves the per-table metadata the batches carry:
// the keyed OFFSET order key (a unique index key when there is no
// primary key) and only the column types converted server-side.
func TestBuildPartitionPlanResolvesCopyMeta(t *testing.T) {
	ctx := context.Background()
	d := SQLServerSource()
	_, db := newRecDB(map[string]recAnswer{
		d.CountQuery("dbo", "Doc"): {rows: [][]driver.Value{{int64(25000)}}},
		sqlserverColumnTypesQuery: {cols: []string{"n", "t"}, rows: [][]driver.Value{
			{"Id", "int"}, {"Node", "hierarchyid"}, {"V", "sql_variant"}, {"Name", "nvarchar"},
		}},
		d.(uniqueKeyDialect).UniqueKeyColumnsQuery(): {rows: [][]driver.Value{{"Code"}}},
	})
	defer db.Close()
	plan, err := BuildPartitionPlan(ctx, db, d, "dbo", "Doc", 10000)
	require.NoError(t, err)
	require.Equal(t, "offset", plan.RangeKind)
	require.Len(t, plan.Ranges, 3)
	require.Equal(t, []string{"Code"}, plan.OrderKey)
	require.Equal(t, map[string]string{"Node": "hierarchyid", "V": "sql_variant"}, plan.SourceTypes)

	// MySQL resolves nothing extra: no conversions, no keyed paging.
	m := MySQLSource()
	rec, mdb := newRecDB(map[string]recAnswer{
		m.CountQuery("s", "t"): {rows: [][]driver.Value{{int64(3)}}},
	})
	defer mdb.Close()
	plan, err = BuildPartitionPlan(ctx, mdb, m, "s", "t", 10000)
	require.NoError(t, err)
	require.Nil(t, plan.SourceTypes)
	require.Nil(t, plan.OrderKey)
	queries, _ := rec.logged()
	require.Equal(t, []string{m.CountQuery("s", "t"), m.PKColumnsQuery()}, queries)
}

// Each dialect states the bind order of its OFFSET batch SELECT: ISO
// OFFSET … FETCH NEXT (Oracle, SQL Server, also once converted by
// ForColumns) binds the offset first, MySQL's LIMIT ? OFFSET ? the limit.
func TestOffsetArgsFirst(t *testing.T) {
	cases := []struct {
		name string
		d    SourceDialect
		want bool
	}{
		{"mysql", MySQLSource(), false},
		{"mysql history", MySQLSystemTimeHistorySource("re"), false},
		{"oracle", OracleSource(), true},
		{"sqlserver", SQLServerSource(), true},
		{"db2", DB2Source(), true},
		{"db2zos", DB2zOSSource(), true},
		{"sqlserver converted", sqlserverDialect{}.WithColumnTypes([]string{"n"}, map[string]string{"n": "hierarchyid"}, nil), true},
	}
	for _, c := range cases {
		require.Equal(t, c.want, offsetArgsFirst(c.d), c.name)
	}
}
