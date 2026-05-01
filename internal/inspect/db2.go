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

// inspectDB2Concurrency caps in-flight db2BuildTableDDL calls during
// introspection. Matches connection.OpenDB2's MaxOpenConns; each table fetch
// runs two SYSCAT queries (columns + PK) sequentially on its own goroutine.
const inspectDB2Concurrency = 16

// InspectDB2 collects a SourceSchema for the given DB2 LUW schema.
//
// Strategy:
//   - Tables: assemble CREATE TABLE from SYSCAT.COLUMNS (no DBMS_METADATA on
//     DB2; db2look exists but is an external utility we cannot call here).
//   - Views, routines, triggers: use the verbatim TEXT column from SYSCAT,
//     which holds the original CREATE statement as the user typed it.
//   - Sequences + indexes + FKs: stashed in Events as extra DDL snippets.
//
// schema is the SYSCAT schema name (case-sensitive, upper-case for unquoted
// identifiers).
//
// The returned InspectTimings records per-section duration and counts so the
// caller can pinpoint which catalog walk dominates on a slow plan.
func InspectDB2(ctx context.Context, db *sql.DB, schema string, log zerolog.Logger) (*SourceSchema, *InspectTimings, error) {
	timings := NewInspectTimings()
	totalStart := time.Now()
	defer func() { timings.Total = time.Since(totalStart) }()

	owner := strings.ToUpper(strings.TrimSpace(schema))
	if owner == "" {
		if err := db.QueryRowContext(ctx, "VALUES CURRENT SCHEMA").Scan(&owner); err != nil {
			return nil, timings, fmt.Errorf("resolve current schema: %w", err)
		}
		owner = strings.TrimSpace(owner)
	}

	s := &SourceSchema{Database: owner}
	if err := db.QueryRowContext(ctx,
		"SELECT service_level FROM TABLE(sysproc.env_get_inst_info())").Scan(&s.Version); err != nil {
		// non-fatal — ENV_GET_INST_INFO is available on LUW but may need
		// privileges. Fall back to a generic banner.
		s.Version = "DB2 LUW"
	}

	logSection := func(name string) {
		log.Info().
			Str("dialect", "db2").
			Str("section", name).
			Int("count", timings.ObjectCount[name]).
			Dur("duration", timings.BySection[name]).
			Msg("inspect section done")
	}

	// --- tables ---
	if err := timings.Section("tables", func(setCount func(int)) error {
		tables, err := db2ListTables(ctx, db, owner)
		if err != nil {
			return fmt.Errorf("list tables: %w", err)
		}
		if err := db2ConcurrentBuildTableDDL(ctx, db, owner, tables); err != nil {
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
		out, err := db2ListWithText(ctx, db, owner, `
			SELECT RTRIM(VIEWNAME), TEXT FROM SYSCAT.VIEWS
			 WHERE VIEWSCHEMA = ? ORDER BY VIEWNAME`)
		if err != nil {
			return fmt.Errorf("list views: %w", err)
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
		out, err := db2ListWithText(ctx, db, owner, `
			SELECT RTRIM(TRIGNAME), TEXT FROM SYSCAT.TRIGGERS
			 WHERE TRIGSCHEMA = ? ORDER BY TRIGNAME`)
		if err != nil {
			return fmt.Errorf("list triggers: %w", err)
		}
		s.Triggers = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("triggers")

	// --- procedures + functions ---
	// SYSCAT.ROUTINES.ROUTINETYPE: 'P' procedure, 'F' function, 'M' method.
	// ORIGIN='Q' = SQL-bodied (the only kind the parser will understand);
	// 'E' = external (C/Java) — we keep them but flag with a placeholder.
	if err := timings.Section("procedures", func(setCount func(int)) error {
		out, err := db2ListWithText(ctx, db, owner, `
			SELECT RTRIM(ROUTINENAME), TEXT FROM SYSCAT.ROUTINES
			 WHERE ROUTINESCHEMA = ? AND ROUTINETYPE = 'P' AND ORIGIN = 'Q'
			 ORDER BY ROUTINENAME`)
		if err != nil {
			return fmt.Errorf("list procedures: %w", err)
		}
		s.Procedures = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("procedures")

	if err := timings.Section("functions", func(setCount func(int)) error {
		out, err := db2ListWithText(ctx, db, owner, `
			SELECT RTRIM(ROUTINENAME), TEXT FROM SYSCAT.ROUTINES
			 WHERE ROUTINESCHEMA = ? AND ROUTINETYPE = 'F' AND ORIGIN = 'Q'
			 ORDER BY ROUTINENAME`)
		if err != nil {
			return fmt.Errorf("list functions: %w", err)
		}
		s.Functions = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("functions")

	// --- extras (sequences) — stashed in Events ---
	if err := timings.Section("sequences", func(setCount func(int)) error {
		out, err := db2ListSequences(ctx, db, owner)
		if err != nil {
			return fmt.Errorf("list sequences: %w", err)
		}
		s.Events = out
		setCount(len(out))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("sequences")

	return s, timings, nil
}

// db2ConcurrentBuildTableDDL fans out db2BuildTableDDL calls across a worker
// pool, populating items[i].DDL in place. The first error aborts the walk.
func db2ConcurrentBuildTableDDL(ctx context.Context, db *sql.DB, owner string, items []ObjectSnapshot) error {
	if len(items) == 0 {
		return nil
	}
	jobs := make(chan int)
	g, gctx := errgroup.WithContext(ctx)
	for w := 0; w < inspectDB2Concurrency; w++ {
		g.Go(func() error {
			for idx := range jobs {
				ddl, err := db2BuildTableDDL(gctx, db, owner, items[idx].Name)
				if err != nil {
					return fmt.Errorf("build table ddl %s.%s: %w", owner, items[idx].Name, err)
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

func db2ListTables(ctx context.Context, db *sql.DB, owner string) ([]ObjectSnapshot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT RTRIM(TABNAME), COALESCE(CARD, 0)
		  FROM SYSCAT.TABLES
		 WHERE TABSCHEMA = ? AND TYPE = 'T'
		 ORDER BY TABNAME`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectSnapshot
	for rows.Next() {
		var name string
		var card int64
		if err := rows.Scan(&name, &card); err != nil {
			return nil, err
		}
		// CARD = -1 when stats are stale; clamp to 0.
		if card < 0 {
			card = 0
		}
		out = append(out, ObjectSnapshot{Name: name, Database: owner, Rows: card})
	}
	return out, rows.Err()
}

// db2BuildTableDDL assembles a CREATE TABLE statement from SYSCAT.COLUMNS +
// SYSCAT.KEYCOLUSE for the PK. Other constraints (FK, UK, CHECK) and indexes
// are emitted later via the Events bucket so they apply after all tables exist.
func db2BuildTableDDL(ctx context.Context, db *sql.DB, owner, name string) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT RTRIM(COLNAME), RTRIM(TYPENAME), LENGTH, SCALE,
		       CASE NULLS WHEN 'Y' THEN 1 ELSE 0 END,
		       COALESCE(DEFAULT, ''),
		       CASE IDENTITY WHEN 'Y' THEN 1 ELSE 0 END,
		       CASE GENERATED WHEN 'A' THEN 'ALWAYS' WHEN 'D' THEN 'DEFAULT' ELSE '' END,
		       COALESCE(REMARKS, '')
		  FROM SYSCAT.COLUMNS
		 WHERE TABSCHEMA = ? AND TABNAME = ?
		 ORDER BY COLNO`, owner, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type col struct {
		name, typ, def, generated, remarks string
		length, scale                      int
		nullable                           bool
		identity                           bool
	}
	var cols []col
	for rows.Next() {
		var c col
		var nullableInt, identityInt int
		if err := rows.Scan(&c.name, &c.typ, &c.length, &c.scale, &nullableInt,
			&c.def, &identityInt, &c.generated, &c.remarks); err != nil {
			return "", err
		}
		c.nullable = nullableInt == 1
		c.identity = identityInt == 1
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(cols) == 0 {
		// Defensive: empty column list should not happen for TYPE='T'.
		return "", fmt.Errorf("table %s.%s has no columns", owner, name)
	}

	// Primary key columns, in order.
	pkRows, err := db.QueryContext(ctx, `
		SELECT RTRIM(KCU.COLNAME)
		  FROM SYSCAT.TABCONST TC
		  JOIN SYSCAT.KEYCOLUSE KCU
		    ON KCU.CONSTNAME = TC.CONSTNAME
		   AND KCU.TABSCHEMA = TC.TABSCHEMA
		   AND KCU.TABNAME   = TC.TABNAME
		 WHERE TC.TABSCHEMA = ? AND TC.TABNAME = ? AND TC.TYPE = 'P'
		 ORDER BY KCU.COLSEQ`, owner, name)
	if err != nil {
		return "", err
	}
	defer pkRows.Close()
	var pkCols []string
	for pkRows.Next() {
		var c string
		if err := pkRows.Scan(&c); err != nil {
			return "", err
		}
		pkCols = append(pkCols, c)
	}
	if err := pkRows.Err(); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n", owner, name)
	for i, c := range cols {
		fmt.Fprintf(&b, "  %s %s", c.name, db2FormatType(c.typ, c.length, c.scale))
		if c.identity {
			b.WriteString(" GENERATED ")
			if c.generated != "" {
				b.WriteString(c.generated)
			} else {
				b.WriteString("ALWAYS")
			}
			b.WriteString(" AS IDENTITY")
		} else if c.def != "" {
			fmt.Fprintf(&b, " DEFAULT %s", c.def)
		}
		if !c.nullable {
			b.WriteString(" NOT NULL")
		}
		if i < len(cols)-1 || len(pkCols) > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	if len(pkCols) > 0 {
		fmt.Fprintf(&b, "  PRIMARY KEY (%s)\n", strings.Join(pkCols, ", "))
	}
	b.WriteString(")")
	return b.String(), nil
}

// db2FormatType returns the canonical DB2 type spec for a column. SYSCAT stores
// LENGTH meaning bytes for character types and precision for DECIMAL/NUMERIC.
func db2FormatType(typeName string, length, scale int) string {
	t := strings.TrimSpace(typeName)
	switch t {
	case "VARCHAR", "CHARACTER", "CHAR", "GRAPHIC", "VARGRAPHIC":
		return fmt.Sprintf("%s(%d)", t, length)
	case "DECIMAL", "NUMERIC":
		return fmt.Sprintf("%s(%d,%d)", t, length, scale)
	case "TIMESTAMP":
		// LENGTH on TIMESTAMP in SYSCAT is the column-storage size, not the
		// fractional seconds precision. Default 6 (microsecond) is fine.
		return "TIMESTAMP"
	case "DECFLOAT":
		if length == 8 {
			return "DECFLOAT(16)"
		}
		return "DECFLOAT(34)"
	default:
		return t
	}
}

// db2ListWithText runs a query that returns (name, text) and turns it into a
// slice of ObjectSnapshot with TEXT as the DDL.
func db2ListWithText(ctx context.Context, db *sql.DB, owner, query string) ([]ObjectSnapshot, error) {
	rows, err := db.QueryContext(ctx, query, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectSnapshot
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		out = append(out, ObjectSnapshot{Name: name, Database: owner, DDL: text})
	}
	return out, rows.Err()
}

func db2ListSequences(ctx context.Context, db *sql.DB, owner string) ([]ObjectSnapshot, error) {
	// SYSCAT.SEQUENCES on LUW exposes user sequences; SEQTYPE='S' filters out
	// implicit identity-column backing sequences (SEQTYPE='I'). MINVALUE,
	// MAXVALUE etc. are compiled into a portable CREATE SEQUENCE.
	rows, err := db.QueryContext(ctx, `
		SELECT RTRIM(SEQNAME), START, MINVALUE, MAXVALUE, INCREMENT, CYCLE
		  FROM SYSCAT.SEQUENCES
		 WHERE SEQSCHEMA = ? AND SEQTYPE = 'S'
		 ORDER BY SEQNAME`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectSnapshot
	for rows.Next() {
		var name string
		var start, minv, maxv, inc int64
		var cycle string
		if err := rows.Scan(&name, &start, &minv, &maxv, &inc, &cycle); err != nil {
			return nil, err
		}
		ddl := fmt.Sprintf("CREATE SEQUENCE %s.%s START WITH %d INCREMENT BY %d MINVALUE %d MAXVALUE %d",
			owner, name, start, inc, minv, maxv)
		if cycle == "Y" {
			ddl += " CYCLE"
		} else {
			ddl += " NO CYCLE"
		}
		out = append(out, ObjectSnapshot{Name: name, Database: owner, DDL: ddl})
	}
	return out, rows.Err()
}
