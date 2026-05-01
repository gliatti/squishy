// Package inspect extracts a source schema description by introspection.
// Output format: a slice of CREATE TABLE / CREATE VIEW / CREATE TRIGGER /
// CREATE PROCEDURE / CREATE FUNCTION / CREATE EVENT strings, ready to feed
// squishy's MySQL parser.
package inspect

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

// inspectMySQLConcurrency caps in-flight SHOW CREATE calls during
// introspection. Matches connection.OpenMySQL's MaxOpenConns.
const inspectMySQLConcurrency = 16

// SourceSchema is the JSON payload stored in migrations.source_schema and
// returned to the wizard.
type SourceSchema struct {
	Database   string           `json:"database"`
	Version    string           `json:"version"`
	Tables     []ObjectSnapshot `json:"tables"`
	Views      []ObjectSnapshot `json:"views"`
	Triggers   []ObjectSnapshot `json:"triggers"`
	Procedures []ObjectSnapshot `json:"procedures"`
	Functions  []ObjectSnapshot `json:"functions"`
	Events     []ObjectSnapshot `json:"events"`
}

// ObjectSnapshot is a named SQL blob. DDL holds the verbatim CREATE statement.
type ObjectSnapshot struct {
	Name     string `json:"name"`
	Database string `json:"database"`
	Rows     int64  `json:"rows,omitempty"`
	DDL      string `json:"ddl"`
}

// MarshalJSON normalizes output for deterministic tests.
func (s *SourceSchema) MarshalJSON() ([]byte, error) {
	type alias SourceSchema
	return json.Marshal((*alias)(s))
}

// InspectMySQL walks the information_schema of the connected MySQL database
// and collects CREATE statements for every migratable object.
//
// The returned InspectTimings records per-section duration and counts so the
// caller can surface a breakdown alongside the plan response.
func InspectMySQL(ctx context.Context, db *sql.DB, database string, log zerolog.Logger) (*SourceSchema, *InspectTimings, error) {
	timings := NewInspectTimings()
	totalStart := time.Now()
	defer func() { timings.Total = time.Since(totalStart) }()

	s := &SourceSchema{Database: database}
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&s.Version); err != nil {
		return nil, timings, fmt.Errorf("version: %w", err)
	}

	logSection := func(name string) {
		log.Info().
			Str("dialect", "mysql").
			Str("section", name).
			Int("count", timings.ObjectCount[name]).
			Dur("duration", timings.BySection[name]).
			Msg("inspect section done")
	}

	// --- tables + estimated rows ---
	// MariaDB reports system-versioned tables with TABLE_TYPE='SYSTEM VERSIONED'
	// rather than 'BASE TABLE'. Accept both so those tables are not silently dropped.
	if err := timings.Section("tables", func(setCount func(int)) error {
		rows, err := db.QueryContext(ctx, `
			SELECT TABLE_NAME, IFNULL(TABLE_ROWS,0)
			  FROM information_schema.TABLES
			 WHERE TABLE_SCHEMA=? AND TABLE_TYPE IN ('BASE TABLE','SYSTEM VERSIONED')
			 ORDER BY TABLE_NAME`, database)
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
			tables = append(tables, ObjectSnapshot{Name: name, Database: database, Rows: n})
		}
		rows.Close()
		if err := mysqlConcurrentShowCreate(ctx, db, database, "TABLE", tables, false); err != nil {
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
		out, err := listAndShow(ctx, db, database, "VIEW", `
			SELECT TABLE_NAME FROM information_schema.VIEWS WHERE TABLE_SCHEMA=? ORDER BY TABLE_NAME`)
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
		out, err := listAndShow(ctx, db, database, "TRIGGER", `
			SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=? ORDER BY TRIGGER_NAME`)
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

	// --- procedures / functions ---
	if err := timings.Section("procedures", func(setCount func(int)) error {
		out, err := listAndShow(ctx, db, database, "PROCEDURE", `
			SELECT ROUTINE_NAME FROM information_schema.ROUTINES
			 WHERE ROUTINE_SCHEMA=? AND ROUTINE_TYPE='PROCEDURE' ORDER BY ROUTINE_NAME`)
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
		out, err := listAndShow(ctx, db, database, "FUNCTION", `
			SELECT ROUTINE_NAME FROM information_schema.ROUTINES
			 WHERE ROUTINE_SCHEMA=? AND ROUTINE_TYPE='FUNCTION' ORDER BY ROUTINE_NAME`)
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

	// --- events ---
	if err := timings.Section("events", func(setCount func(int)) error {
		out, err := listAndShow(ctx, db, database, "EVENT", `
			SELECT EVENT_NAME FROM information_schema.EVENTS WHERE EVENT_SCHEMA=? ORDER BY EVENT_NAME`)
		if err != nil {
			return err
		}
		s.Events = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("events")

	return s, timings, nil
}

func listAndShow(ctx context.Context, db *sql.DB, database, objType, listQuery string) ([]ObjectSnapshot, error) {
	rows, err := db.QueryContext(ctx, listQuery, database)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", objType, err)
	}
	defer rows.Close()
	var items []ObjectSnapshot
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		items = append(items, ObjectSnapshot{Name: n, Database: database})
	}
	if err := mysqlConcurrentShowCreate(ctx, db, database, objType, items, true); err != nil {
		return nil, err
	}
	return items, nil
}

// mysqlConcurrentShowCreate fans out SHOW CREATE calls across a worker pool,
// populating items[i].DDL in place. Order is preserved.
//
// If recordPrivErrors is true, per-object failures (typically privilege
// issues) become a placeholder DDL comment rather than aborting — preserves
// the original listAndShow behavior. Otherwise the first error aborts.
func mysqlConcurrentShowCreate(ctx context.Context, db *sql.DB, database, objType string, items []ObjectSnapshot, recordPrivErrors bool) error {
	if len(items) == 0 {
		return nil
	}
	jobs := make(chan int)
	g, gctx := errgroup.WithContext(ctx)
	for w := 0; w < inspectMySQLConcurrency; w++ {
		g.Go(func() error {
			for idx := range jobs {
				ddl, err := showCreate(gctx, db, objType, database, items[idx].Name)
				if err != nil {
					if recordPrivErrors {
						items[idx].DDL = "-- could not SHOW CREATE " + objType + ": " + err.Error()
						continue
					}
					return fmt.Errorf("SHOW CREATE %s %s: %w", objType, items[idx].Name, err)
				}
				items[idx].DDL = ddl
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

// showCreate returns the DDL body of a SHOW CREATE <KIND> query. The column
// layout varies by object kind; we pick the DDL column by name.
//
// Column names per MySQL variant:
//   TABLE/VIEW/EVENT/PROCEDURE/FUNCTION: "Create <Kind>"
//   TRIGGER:                             "SQL Original Statement"
// A "longest string" heuristic is unsafe because SHOW CREATE PROCEDURE / FUNCTION
// / TRIGGER also returns a sql_mode column, which can be longer than a short
// routine body (e.g. "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,...").
func showCreate(ctx context.Context, db *sql.DB, kind, database, name string) (string, error) {
	q := fmt.Sprintf("SHOW CREATE %s `%s`.`%s`", kind, database, name)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		return "", fmt.Errorf("no row")
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}

	toString := func(v any) string {
		switch x := v.(type) {
		case []byte:
			return string(x)
		case string:
			return x
		}
		return ""
	}

	// Try to pick the DDL column by name first — robust across MySQL variants.
	for i, c := range cols {
		if c == "SQL Original Statement" ||
			c == "Create Table" || c == "Create View" ||
			c == "Create Procedure" || c == "Create Function" ||
			c == "Create Event" || c == "Create Trigger" {
			return toString(vals[i]), nil
		}
	}
	// Fallback: pick the longest non-sql_mode string. Some MySQL forks
	// localize the column header — this keeps working in that case while
	// still ignoring the sql_mode column which is well-known.
	best := ""
	for i, c := range cols {
		if c == "sql_mode" || c == "character_set_client" ||
			c == "collation_connection" || c == "Database Collation" ||
			c == "time_zone" {
			continue
		}
		s := toString(vals[i])
		if len(s) > len(best) {
			best = s
		}
	}
	return best, nil
}
