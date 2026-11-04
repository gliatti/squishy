//go:build sqlserver_live

package inspect

// Live check of the SQL Server plumbing against the compose sample
// (`make sqlserver-up`). Opt-in, never part of `make test`:
//
//	docker compose --profile test run --rm unit-tests \
//	  go test -tags sqlserver_live ./internal/inspect/ -run TestSQLServerLive -v
//
// SQUISHY_SQLSERVER_HOST / _PORT / _PASSWORD override the sample defaults.

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/connection"
	"gitlab.com/dalibo/squishy/internal/dataxfer"
	"gitlab.com/dalibo/squishy/internal/discover"
)

func liveEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestSQLServerLive(t *testing.T) {
	port, _ := strconv.Atoi(liveEnv("SQUISHY_SQLSERVER_PORT", "1433"))
	p := connection.Params{
		Kind: "sqlserver", Host: liveEnv("SQUISHY_SQLSERVER_HOST", "sqlserver-sample"), Port: port,
		Database: "master", Username: "sa", Password: liveEnv("SQUISHY_SQLSERVER_PASSWORD", "Squishy_2026x"),
		SSLMode: "disable",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res := connection.Test(ctx, p)
	require.True(t, res.OK, res.Message)
	t.Logf("version: %s", res.Version)

	db, err := connection.OpenSQLServer(ctx, p)
	require.NoError(t, err)
	defer db.Close()

	dbs, err := discover.Schemas(ctx, db, "sqlserver")
	require.NoError(t, err)
	t.Logf("databases: %v", dbs)
	require.NotContains(t, dbs, "master")

	log := zerolog.New(zerolog.NewTestWriter(t))
	for _, name := range dbs {
		s, tm, err := InspectSQLServer(ctx, db, name, log)
		require.NoError(t, err, name)
		t.Logf("%s: version=%s tables=%d views=%d triggers=%d procs=%d funcs=%d events=%d (%v)",
			name, s.Version, len(s.Tables), len(s.Views), len(s.Triggers), len(s.Procedures),
			len(s.Functions), len(s.Events), tm.Total)
		for i, tb := range s.Tables {
			require.NotEmpty(t, tb.Schema)
			require.Equal(t, name, tb.Database)
			if i < 2 || os.Getenv("SQUISHY_SQLSERVER_DUMP") != "" {
				t.Logf("--- %s.%s (%d rows)\n%s", tb.Schema, tb.Name, tb.Rows, tb.DDL)
			}
		}
		for _, e := range s.Events {
			if os.Getenv("SQUISHY_SQLSERVER_DUMP") != "" {
				t.Logf("--- event %s.%s\n%s", e.Schema, e.Name, e.DDL)
			}
		}

		// The pool must still be on master: the inspector discards the
		// connections it switched to another database.
		var cur string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&cur))
		require.Equal(t, "master", cur)
	}

	// Copy-side catalog queries on a database-scoped pool.
	for _, name := range dbs {
		pp := p
		pp.Database = name
		sdb, err := connection.OpenSQLServer(ctx, pp)
		require.NoError(t, err)
		s, _, err := InspectSQLServer(ctx, sdb, name, log)
		require.NoError(t, err)
		d := dataxfer.SQLServerSource()
		for _, tb := range s.Tables {
			plan, err := dataxfer.BuildPartitionPlan(ctx, sdb, d, tb.Schema, tb.Name, 1000)
			require.NoError(t, err, tb.Schema+"."+tb.Name)
			if len(plan.Ranges) > 0 {
				t.Logf("%s.%s.%s: pk=%v kind=%s rows=%d batches=%d", name, tb.Schema, tb.Name,
					plan.PK, plan.RangeKind, plan.TotalRows, len(plan.Ranges))
			}
		}
		sdb.Close()
	}
}
