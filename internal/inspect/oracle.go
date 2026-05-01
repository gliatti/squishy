package inspect

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

// inspectOracleConcurrency caps the number of in-flight DBMS_METADATA.GET_DDL
// calls during introspection. Capped at 8 deliberately: above that, we observe
// flaky ORA-06502 / LPX-00002 errors from DBMS_METADATA on large schemas — a
// known Oracle bug where concurrent metadata exports race on shared XDK
// structures. 8 stays well clear of the bug while still giving a real speedup
// over serial fetch.
const inspectOracleConcurrency = 8

// oracleSessionPragmas configure DBMS_METADATA to emit portable DDL.
// Applied per-connection by every worker in the parallel fetcher because
// these are session-scoped and only stick to the conn that ran them.
// Failures are non-fatal — the parser accepts the verbose form too.
var oracleSessionPragmas = []string{
	`BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'STORAGE',false); END;`,
	`BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'TABLESPACE',false); END;`,
	`BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'SEGMENT_ATTRIBUTES',false); END;`,
	`BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'CONSTRAINTS_AS_ALTER',false); END;`,
	`BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'REF_CONSTRAINTS',true); END;`,
	`BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'SQLTERMINATOR',true); END;`,
}

// InspectOracle walks the data dictionary of the connected Oracle PDB and
// collects CREATE statements for every migratable object in the user's
// current schema (USER_*) — mirrors the MySQL introspection shape so the
// rest of squishy (parser, translator, planner) sees a uniform SourceSchema.
//
// The schema argument, when non-empty, is used as the OWNER filter against
// ALL_* views; it's uppercased because Oracle stores unquoted identifiers
// folded to upper. When empty, we fall back to USER (current schema).
//
// The returned InspectTimings records the wall-clock duration and object
// count of each section, intended to drive performance diagnostics on large
// schemas where DBMS_METADATA.GET_DDL is the dominant cost.
func InspectOracle(ctx context.Context, db *sql.DB, schema string, log zerolog.Logger) (*SourceSchema, *InspectTimings, error) {
	timings := NewInspectTimings()
	totalStart := time.Now()
	defer func() { timings.Total = time.Since(totalStart) }()

	owner := strings.ToUpper(strings.TrimSpace(schema))
	if owner == "" {
		if err := db.QueryRowContext(ctx, "SELECT USER FROM dual").Scan(&owner); err != nil {
			return nil, timings, fmt.Errorf("resolve current user: %w", err)
		}
	}

	s := &SourceSchema{Database: owner}
	if err := db.QueryRowContext(ctx,
		"SELECT banner_full FROM v$version WHERE rownum=1").Scan(&s.Version); err != nil {
		return nil, timings, fmt.Errorf("version: %w", err)
	}

	logSection := func(name string) {
		log.Info().
			Str("dialect", "oracle").
			Str("section", name).
			Int("count", timings.ObjectCount[name]).
			Dur("duration", timings.BySection[name]).
			Msg("inspect section done")
	}

	// --- tables (+ estimated row counts from the optimizer stats) ---
	// Exclude nested-table storage tables (NESTED='YES') and secondary IOT
	// segments — DBMS_METADATA refuses to dump them as standalone tables; the
	// parent table's DDL already declares the column type that owns them.
	if err := timings.Section("tables", func(setCount func(int)) error {
		rows, err := db.QueryContext(ctx, `
			SELECT table_name, NVL(num_rows, 0)
			  FROM all_tables
			 WHERE owner = :1
			   AND temporary = 'N'
			   AND nested = 'NO'
			   AND secondary = 'N'
			 ORDER BY table_name`, owner)
		if err != nil {
			return fmt.Errorf("list tables: %w", err)
		}
		var tables []ObjectSnapshot
		for rows.Next() {
			var name string
			var n int64
			if err := rows.Scan(&name, &n); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, ObjectSnapshot{Name: name, Database: owner, Rows: n})
		}
		rows.Close()
		if err := oracleConcurrentGetDDL(ctx, db, owner, "TABLE", tables, false); err != nil {
			return err
		}
		s.Tables = tables
		setCount(len(tables))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("tables")

	// --- views ---
	if err := timings.Section("views", func(setCount func(int)) error {
		out, err := oracleListAndGetDDL(ctx, db, owner, "VIEW", `
			SELECT view_name FROM all_views
			 WHERE owner = :1 ORDER BY view_name`)
		if err != nil {
			return err
		}
		s.Views = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("views")

	// --- triggers ---
	if err := timings.Section("triggers", func(setCount func(int)) error {
		out, err := oracleListAndGetDDL(ctx, db, owner, "TRIGGER", `
			SELECT trigger_name FROM all_triggers
			 WHERE owner = :1 ORDER BY trigger_name`)
		if err != nil {
			return err
		}
		s.Triggers = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("triggers")

	// --- procedures + functions (split via OBJECT_TYPE in all_objects) ---
	if err := timings.Section("procedures", func(setCount func(int)) error {
		out, err := oracleListAndGetDDL(ctx, db, owner, "PROCEDURE", `
			SELECT object_name FROM all_objects
			 WHERE owner = :1 AND object_type = 'PROCEDURE'
			 ORDER BY object_name`)
		if err != nil {
			return err
		}
		s.Procedures = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("procedures")

	if err := timings.Section("functions", func(setCount func(int)) error {
		out, err := oracleListAndGetDDL(ctx, db, owner, "FUNCTION", `
			SELECT object_name FROM all_objects
			 WHERE owner = :1 AND object_type = 'FUNCTION'
			 ORDER BY object_name`)
		if err != nil {
			return err
		}
		s.Functions = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("functions")

	// --- Oracle-specific objects stashed into "Events" to avoid breaking the
	//     wire shape; the planner treats them as extra DDL snippets:
	//        - sequences
	//        - packages (spec + body)
	//        - types (spec + body)
	//        - materialized views
	var extras []ObjectSnapshot
	for _, sec := range []struct {
		name, kind, query string
	}{
		{"sequences", "SEQUENCE", `
			SELECT sequence_name FROM all_sequences
			 WHERE sequence_owner = :1 ORDER BY sequence_name`},
		{"packages", "PACKAGE", `
			SELECT object_name FROM all_objects
			 WHERE owner = :1 AND object_type = 'PACKAGE'
			 ORDER BY object_name`},
		{"package_bodies", "PACKAGE_BODY", `
			SELECT object_name FROM all_objects
			 WHERE owner = :1 AND object_type = 'PACKAGE BODY'
			 ORDER BY object_name`},
		{"types", "TYPE", `
			SELECT type_name FROM all_types
			 WHERE owner = :1 ORDER BY type_name`},
		{"materialized_views", "MATERIALIZED_VIEW", `
			SELECT mview_name FROM all_mviews
			 WHERE owner = :1 ORDER BY mview_name`},
	} {
		sec := sec
		if err := timings.Section(sec.name, func(setCount func(int)) error {
			out, err := oracleListAndGetDDL(ctx, db, owner, sec.kind, sec.query)
			if err != nil {
				return err
			}
			extras = append(extras, out...)
			setCount(len(out))
			return nil
		}); err != nil {
			return nil, timings, err
		}
		logSection(sec.name)
	}
	s.Events = extras

	return s, timings, nil
}

// oracleListAndGetDDL lists names from listQuery, then fans out the per-object
// DBMS_METADATA.GET_DDL calls across a worker pool. Per-object failures are
// dropped silently (some objects may be unreadable due to privileges or
// metadata bugs); the caller still receives the survivors.
func oracleListAndGetDDL(ctx context.Context, db *sql.DB, owner, objType, listQuery string) ([]ObjectSnapshot, error) {
	rows, err := db.QueryContext(ctx, listQuery, owner)
	if err != nil {
		// Some data-dictionary views (e.g. all_mviews) may be denied in stripped
		// environments; treat as empty rather than aborting.
		return nil, nil
	}
	var items []ObjectSnapshot
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, ObjectSnapshot{Name: n, Database: owner})
	}
	rows.Close()
	if err := oracleConcurrentGetDDL(ctx, db, owner, objType, items, true); err != nil {
		return nil, err
	}
	out := make([]ObjectSnapshot, 0, len(items))
	for _, s := range items {
		if s.DDL != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// oracleConcurrentGetDDL fetches DBMS_METADATA.GET_DDL for every items[i].Name
// concurrently, populating items[i].DDL in place. Order is preserved.
//
// Each worker pins a *sql.Conn and applies the session-scoped DBMS_METADATA
// pragmas on it before running queries — this is required because pragmas
// only stick to the connection that ran them, and a parallel fetcher
// otherwise spreads queries across uninitialized connections.
//
// If skipOnError is true, items whose fetch failed keep DDL="" and the
// function returns nil; the caller is expected to filter. Otherwise the
// first error aborts the whole walk.
func oracleConcurrentGetDDL(ctx context.Context, db *sql.DB, owner, objType string, items []ObjectSnapshot, skipOnError bool) error {
	if len(items) == 0 {
		return nil
	}
	jobs := make(chan int)
	g, gctx := errgroup.WithContext(ctx)
	for w := 0; w < inspectOracleConcurrency; w++ {
		g.Go(func() error {
			conn, err := db.Conn(gctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			for _, p := range oracleSessionPragmas {
				_, _ = conn.ExecContext(gctx, p)
			}
			for idx := range jobs {
				var ddl string
				err := conn.QueryRowContext(gctx,
					`SELECT DBMS_METADATA.GET_DDL(:1, :2, :3) FROM dual`,
					objType, items[idx].Name, owner).Scan(&ddl)
				if err != nil {
					if skipOnError {
						continue
					}
					return fmt.Errorf("get_ddl %s %s: %w", objType, items[idx].Name, err)
				}
				items[idx].DDL = strings.TrimSpace(ddl)
			}
			return nil
		})
	}
	for i := range items {
		select {
		case jobs <- i:
		case <-gctx.Done():
			close(jobs)
			return g.Wait()
		}
	}
	close(jobs)
	return g.Wait()
}
