package inspect

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

// inspectSQLServerConcurrency caps the catalog queries in flight during
// introspection. Matches connection.OpenSQLServer's MaxOpenConns.
const inspectSQLServerConcurrency = 16

// InspectSQLServer collects a SourceSchema for one SQL Server database.
//
// SQL Server has no SHOW CREATE TABLE, so each table's DDL is a canonical
// T-SQL script synthesized from the sys.* catalog (columns, types,
// identity, computed columns, defaults, keys, foreign keys, checks,
// indexes, system-time period, MS_Description extended properties). That
// script is parsed by the same tsql parser as real scripts (Northwind,
// AdventureWorks), so there is a single AST path from either source.
// Views, triggers, procedures and functions are sys.sql_modules
// definitions, verbatim.
//
// The catalog is read set-based — one query per catalog view for the
// whole database, the queries running concurrently on their own
// connections — and the DDL is then assembled in Go. Per-object round
// trips (the SHOW CREATE fan-out of InspectMySQL) would cost one query per
// table per catalog view; set-based reads keep a 10 000-table database at
// a dozen queries.
//
// database is the SQL Server database to inspect (the discovery unit);
// empty means the connection's current database. Every snapshot carries
// Schema = its T-SQL schema and Database = database.
func InspectSQLServer(ctx context.Context, db *sql.DB, database string, log zerolog.Logger) (*SourceSchema, *InspectTimings, error) {
	timings := NewInspectTimings()
	totalStart := time.Now()
	defer func() { timings.Total = time.Since(totalStart) }()

	if database == "" {
		if err := db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&database); err != nil {
			return nil, timings, fmt.Errorf("resolve current database: %w", err)
		}
	}
	s := &SourceSchema{Database: database}
	var banner string
	if err := db.QueryRowContext(ctx,
		"SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128)), @@VERSION").Scan(&s.Version, &banner); err != nil {
		return nil, timings, fmt.Errorf("version: %w", err)
	}
	log.Info().Str("dialect", "sqlserver").Str("database", database).
		Str("version", s.Version).Str("banner", banner).Msg("inspect start")

	logSection := func(name string) {
		log.Info().
			Str("dialect", "sqlserver").
			Str("section", name).
			Int("count", timings.ObjectCount[name]).
			Dur("duration", timings.BySection[name]).
			Msg("inspect section done")
	}

	var cat *ssCatalog
	if err := timings.Section("catalog", func(setCount func(int)) error {
		var err error
		cat, err = ssLoadCatalog(ctx, db, database, log)
		if err != nil {
			return err
		}
		setCount(len(cat.tables) + len(cat.tableTypes))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("catalog")

	// --- tables ---
	if err := timings.Section("tables", func(setCount func(int)) error {
		for _, t := range cat.tables {
			s.Tables = append(s.Tables, ObjectSnapshot{
				Name: t.Name, Schema: t.Schema, Database: database,
				Rows: t.Rows, DDL: ssTableDDL(t),
			})
		}
		setCount(len(s.Tables))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("tables")

	// --- views ---
	if err := timings.Section("views", func(setCount func(int)) error {
		out, err := ssListModules(ctx, db, database, log, "view", `
			SELECT SCHEMA_NAME(o.schema_id), o.name, m.definition, ISNULL(OBJECTPROPERTY(o.object_id, 'IsEncrypted'), 0)
			  FROM sys.views o
			  LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id
			 WHERE o.is_ms_shipped = 0
			 ORDER BY 1, 2`)
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

	// --- triggers (DML triggers on tables and views) ---
	// parent_class 1 = object; 0 = database-level DDL triggers, which have
	// no PG counterpart (event triggers are cluster-wide and react to
	// different events) — listed in the log only. CLR triggers ('TA')
	// have no T-SQL definition and are skipped likewise.
	if err := timings.Section("triggers", func(setCount func(int)) error {
		out, err := ssListModules(ctx, db, database, log, "trigger", `
			SELECT OBJECT_SCHEMA_NAME(tr.parent_id), tr.name, m.definition, ISNULL(OBJECTPROPERTY(tr.object_id, 'IsEncrypted'), 0)
			  FROM sys.triggers tr
			  JOIN sys.sql_modules m ON m.object_id = tr.object_id
			 WHERE tr.parent_class = 1 AND tr.is_ms_shipped = 0 AND tr.type = 'TR'
			 ORDER BY 1, 2`)
		if err != nil {
			return fmt.Errorf("list triggers: %w", err)
		}
		s.Triggers = out
		setCount(len(out))
		return ssLogSkipped(ctx, db, database, log, "trigger", `
			SELECT name, CASE WHEN parent_class = 0 THEN 'DDL trigger' ELSE 'CLR trigger' END
			  FROM sys.triggers
			 WHERE is_ms_shipped = 0 AND (parent_class = 0 OR type = 'TA')
			 ORDER BY name`)
	}); err != nil {
		return nil, timings, err
	}
	logSection("triggers")

	// --- procedures (T-SQL only; CLR 'PC', extended 'X' are skipped) ---
	if err := timings.Section("procedures", func(setCount func(int)) error {
		out, err := ssListModules(ctx, db, database, log, "procedure", `
			SELECT SCHEMA_NAME(o.schema_id), o.name, m.definition, ISNULL(OBJECTPROPERTY(o.object_id, 'IsEncrypted'), 0)
			  FROM sys.objects o
			  LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id
			 WHERE o.type = 'P' AND o.is_ms_shipped = 0
			 ORDER BY 1, 2`)
		if err != nil {
			return fmt.Errorf("list procedures: %w", err)
		}
		s.Procedures = out
		setCount(len(out))
		return ssLogSkipped(ctx, db, database, log, "procedure", `
			SELECT SCHEMA_NAME(schema_id) + '.' + name, type_desc
			  FROM sys.objects
			 WHERE type IN ('PC', 'X') AND is_ms_shipped = 0
			 ORDER BY 1`)
	}); err != nil {
		return nil, timings, err
	}
	logSection("procedures")

	// --- functions: scalar (FN), inline table-valued (IF), multi-statement
	// table-valued (TF). CLR functions (FS, FT, AF) are skipped. ---
	if err := timings.Section("functions", func(setCount func(int)) error {
		out, err := ssListModules(ctx, db, database, log, "function", `
			SELECT SCHEMA_NAME(o.schema_id), o.name, m.definition, ISNULL(OBJECTPROPERTY(o.object_id, 'IsEncrypted'), 0)
			  FROM sys.objects o
			  LEFT JOIN sys.sql_modules m ON m.object_id = o.object_id
			 WHERE o.type IN ('FN', 'IF', 'TF') AND o.is_ms_shipped = 0
			 ORDER BY 1, 2`)
		if err != nil {
			return fmt.Errorf("list functions: %w", err)
		}
		s.Functions = out
		setCount(len(out))
		return ssLogSkipped(ctx, db, database, log, "function", `
			SELECT SCHEMA_NAME(schema_id) + '.' + name, type_desc
			  FROM sys.objects
			 WHERE type IN ('FS', 'FT', 'AF') AND is_ms_shipped = 0
			 ORDER BY 1`)
	}); err != nil {
		return nil, timings, err
	}
	logSection("functions")

	// --- user types and sequences — stashed in Events ---
	// The planner only copies Tables, and none of these has rows. Their
	// position relative to the tables does not matter either: the
	// translator pre-registers user types in a pre-pass
	// (translator.preRegisterUserTypes scans every statement before
	// translating tables), so a column of an alias type resolves even
	// though its CREATE TYPE comes later in the source.
	if err := timings.Section("types", func(setCount func(int)) error {
		for _, a := range cat.aliasTypes {
			s.Events = append(s.Events, ObjectSnapshot{
				Name: a.Name, Schema: a.Schema, Database: database, DDL: ssAliasTypeDDL(a),
			})
		}
		for _, tt := range cat.tableTypes {
			s.Events = append(s.Events, ObjectSnapshot{
				Name: tt.Name, Schema: tt.Schema, Database: database, DDL: ssTableTypeDDL(tt),
			})
		}
		setCount(len(cat.aliasTypes) + len(cat.tableTypes))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("types")

	if err := timings.Section("sequences", func(setCount func(int)) error {
		for _, q := range cat.sequences {
			s.Events = append(s.Events, ObjectSnapshot{
				Name: q.Name, Schema: q.Schema, Database: database, DDL: ssSequenceDDL(q),
			})
		}
		setCount(len(cat.sequences))
		return nil
	}); err != nil {
		return nil, timings, err
	}
	logSection("sequences")

	return s, timings, nil
}

// ---------------------------------------------------------------------------
// Connection scoping
// ---------------------------------------------------------------------------

// ssWithDatabase runs fn on a dedicated connection whose current database
// is database. The pool may be opened on another database (an instance
// pool is opened on its login database, typically master), so a USE is
// issued when needed. A connection switched that way is discarded rather
// than returned to the pool afterwards: the next user of the pool must
// not silently land in the inspected database.
func ssWithDatabase(ctx context.Context, db *sql.DB, database string, fn func(*sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var current string
	if err := conn.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&current); err != nil {
		return fmt.Errorf("current database: %w", err)
	}
	if current != database {
		defer func() {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}()
		if _, err := conn.ExecContext(ctx, "USE "+ssQuoteIdent(database)); err != nil {
			return fmt.Errorf("use %s: %w", database, err)
		}
	}
	return fn(conn)
}

// ssQueryRows runs query on a connection scoped to database and hands each
// row to scan.
func ssQueryRows(ctx context.Context, db *sql.DB, database, query string, scan func(*sql.Rows) error) error {
	return ssWithDatabase(ctx, db, database, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// ssListModules reads (schema, name, definition, is_encrypted) rows into
// snapshots. A NULL definition of a WITH ENCRYPTION module cannot be
// read by anyone, so the snapshot only carries a comment and the log
// says so; a NULL definition of any other module means the login lacks
// VIEW DEFINITION on it, which fails the inspection (ssHidden) rather
// than passing the module off as encrypted.
func ssListModules(ctx context.Context, db *sql.DB, database string, log zerolog.Logger, kind, query string) ([]ObjectSnapshot, error) {
	var out []ObjectSnapshot
	hide := &ssHidden{}
	err := ssQueryRows(ctx, db, database, query, func(rows *sql.Rows) error {
		var schema, name string
		var def sql.NullString
		var encrypted int
		if err := rows.Scan(&schema, &name, &def, &encrypted); err != nil {
			return err
		}
		ddl := ssModuleDDL(def.String)
		if !def.Valid && encrypted != 1 {
			hide.add(0, kind+" "+schema+"."+name)
			return nil
		}
		if !def.Valid {
			log.Warn().Str("dialect", "sqlserver").Str("kind", kind).
				Str("object", schema+"."+name).Msg("encrypted module: definition not readable, skipped")
			ddl = "-- encrypted " + kind + " " + ssCommentSafe(ssQuoteName(schema, name)) + ": definition not readable\n"
		}
		out = append(out, ObjectSnapshot{Name: name, Schema: schema, Database: database, DDL: ddl})
		return nil
	})
	if err == nil {
		err = hide.err(database, nil)
	}
	return out, err
}

// ssLogSkipped logs the (name, reason) rows of objects squishy does not
// migrate.
func ssLogSkipped(ctx context.Context, db *sql.DB, database string, log zerolog.Logger, kind, query string) error {
	return ssQueryRows(ctx, db, database, query, func(rows *sql.Rows) error {
		var name, why string
		if err := rows.Scan(&name, &why); err != nil {
			return err
		}
		log.Warn().Str("dialect", "sqlserver").Str("kind", kind).
			Str("object", name).Str("type", why).Msg("not migrated, skipped")
		return nil
	})
}

// ssModuleDDL returns a sys.sql_modules definition with a trailing
// newline, so concatenated snapshots never glue two statements together.
func ssModuleDDL(def string) string {
	if def == "" || def[len(def)-1] == '\n' {
		return def
	}
	return def + "\n"
}

// ---------------------------------------------------------------------------
// Catalog model
// ---------------------------------------------------------------------------

// ssType is a column / alias / sequence type as the catalog describes it.
type ssType struct {
	Name        string // sys.types.name: system type, or the user type's name
	Schema      string // schema of a user-defined type
	UserDefined bool
	MaxLength   int // bytes; -1 = (max)
	Precision   int
	Scale       int
}

type ssColumn struct {
	ID        int
	Name      string
	Type      ssType
	Nullable  bool
	Identity  bool
	Seed      string // sys.identity_columns.seed_value as text
	Increment string
	RowGUID   bool
	Sparse    bool

	DefaultName string // sys.default_constraints
	DefaultDef  string

	Computed    bool // sys.computed_columns
	ComputedDef string
	Persisted   bool

	// GeneratedAlways is sys.columns.generated_always_type: 1 = AS ROW
	// START, 2 = AS ROW END (system-time period columns).
	GeneratedAlways int
	Hidden          bool
}

type ssKeyCol struct {
	Name string
	Desc bool
}

// ssKey is a PRIMARY KEY or UNIQUE constraint.
type ssKey struct {
	Name      string
	Primary   bool
	Clustered bool
	Cols      []ssKeyCol
}

type ssFK struct {
	Name      string
	Cols      []string
	RefSchema string
	RefTable  string
	RefCols   []string
	// sys.foreign_keys.{delete,update}_referential_action:
	// 0 NO ACTION, 1 CASCADE, 2 SET NULL, 3 SET DEFAULT.
	OnDelete int
	OnUpdate int
}

type ssCheck struct {
	Name string
	Def  string
}

// ssIndex is a non-constraint index (sys.indexes, not PK / UQ).
type ssIndex struct {
	Name     string
	Type     int    // sys.indexes.type: 1 clustered, 2 nonclustered, 3 XML, 4 spatial, 5/6 columnstore, 7 hash
	TypeDesc string // sys.indexes.type_desc
	Unique   bool
	Filter   string // sys.indexes.filter_definition
	Keys     []ssKeyCol
	Include  []string
}

// ssTable is a table or a table type (same body grammar).
type ssTable struct {
	ObjectID      int64
	Schema        string
	Name          string
	TemporalType  int // sys.tables.temporal_type: 0 none, 1 history, 2 system-versioned
	HistorySchema string
	HistoryName   string
	Rows          int64

	Columns []ssColumn
	Keys    []ssKey
	FKs     []ssFK
	Checks  []ssCheck
	Indexes []ssIndex

	PeriodStart string // PERIOD FOR SYSTEM_TIME columns
	PeriodEnd   string

	Description        string         // MS_Description of the table
	ColumnDescriptions map[int]string // column_id → MS_Description
	Fulltext           bool           // has a fulltext index (not migrated)
}

type ssAliasType struct {
	Schema   string
	Name     string
	Base     ssType
	Nullable bool
}

type ssSequence struct {
	Schema    string
	Name      string
	Type      ssType
	Start     string // sys.sequences values, as text (numeric up to 38 digits)
	Increment string
	Min       string
	Max       string
	Current   string
	Cycle     bool
}

type ssCatalog struct {
	tables     []*ssTable // ordered by schema, name
	tableTypes []*ssTable
	aliasTypes []ssAliasType
	sequences  []ssSequence
}

// ---------------------------------------------------------------------------
// Catalog loading
// ---------------------------------------------------------------------------

// ssObjectFilter selects the column-bearing objects squishy reads: user
// tables and user table types (whose columns live under
// type_table_object_id).
const ssObjectFilter = `(
	SELECT t.object_id FROM sys.tables t WHERE t.is_ms_shipped = 0
	UNION ALL
	SELECT tt.type_table_object_id FROM sys.table_types tt WHERE tt.is_user_defined = 1)`

// ssLoadCatalog reads every catalog view squishy needs, one query per view,
// concurrently, then stitches the rows onto their tables.
func ssLoadCatalog(ctx context.Context, db *sql.DB, database string, log zerolog.Logger) (*ssCatalog, error) {
	byID := map[int64]*ssTable{}
	cat := &ssCatalog{}

	// Tables and table types first: the other rows attach to them.
	// temporal_type 1 (history tables) is left out — the current table's
	// SYSTEM_VERSIONING clause names it, and its rows are not migrated as
	// a table of their own. dbo.sysdiagrams is SSMS's diagram store.
	if err := ssQueryRows(ctx, db, database, `
		SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name, t.temporal_type,
		       ISNULL(SCHEMA_NAME(h.schema_id), ''), ISNULL(h.name, '')
		  FROM sys.tables t
		  LEFT JOIN sys.tables h ON h.object_id = t.history_table_id
		 WHERE t.is_ms_shipped = 0 AND t.is_external = 0 AND t.temporal_type <> 1
		   AND NOT (t.schema_id = SCHEMA_ID('dbo') AND t.name = N'sysdiagrams')
		 ORDER BY 2, 3`, func(rows *sql.Rows) error {
		t := &ssTable{}
		if err := rows.Scan(&t.ObjectID, &t.Schema, &t.Name, &t.TemporalType, &t.HistorySchema, &t.HistoryName); err != nil {
			return err
		}
		byID[t.ObjectID] = t
		cat.tables = append(cat.tables, t)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	if err := ssQueryRows(ctx, db, database, `
		SELECT tt.type_table_object_id, SCHEMA_NAME(tt.schema_id), tt.name
		  FROM sys.table_types tt
		 WHERE tt.is_user_defined = 1
		 ORDER BY 2, 3`, func(rows *sql.Rows) error {
		t := &ssTable{}
		if err := rows.Scan(&t.ObjectID, &t.Schema, &t.Name); err != nil {
			return err
		}
		byID[t.ObjectID] = t
		cat.tableTypes = append(cat.tableTypes, t)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list table types: %w", err)
	}

	// Each loader below fills its own result; they are merged after Wait
	// so no two goroutines touch the same table.
	type colRow struct {
		obj int64
		col ssColumn
	}
	type keyRow struct {
		obj, keyID int64
		key        ssKey
		col        ssKeyCol
	}
	type fkRow struct {
		obj, fkID int64
		fk        ssFK
		col, ref  string
	}
	type checkRow struct {
		obj   int64
		check ssCheck
	}
	type idxRow struct {
		obj   int64
		idxID int
		idx   ssIndex
		col   string
		desc  bool
		incl  bool
	}
	type periodRow struct {
		obj        int64
		start, end string
	}
	type descRow struct {
		obj   int64
		minor int
		text  string
	}
	var (
		cols      []colRow
		keys      []keyRow
		fks       []fkRow
		checks    []checkRow
		idxs      []idxRow
		periods   []periodRow
		descs     []descRow
		rowCounts = map[int64]int64{}
		fulltext  = map[int64]bool{}
	)

	hide := &ssHidden{}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(inspectSQLServerConcurrency)

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database, `
			SELECT c.object_id, c.column_id, c.name,
			       ty.name, ISNULL(SCHEMA_NAME(ty.schema_id), ''), ty.is_user_defined,
			       c.max_length, c.precision, c.scale, c.is_nullable,
			       c.is_identity,
			       ISNULL(CAST(ic.seed_value AS nvarchar(64)), ''),
			       ISNULL(CAST(ic.increment_value AS nvarchar(64)), ''),
			       c.is_rowguidcol, c.is_sparse,
			       c.is_computed, cc.definition, ISNULL(cc.is_persisted, 0),
			       ISNULL(dc.name, ''), dc.definition,
			       c.generated_always_type, c.is_hidden
			  FROM sys.columns c
			  JOIN sys.types ty ON ty.user_type_id = c.user_type_id
			  LEFT JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
			  LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
			  LEFT JOIN sys.default_constraints dc ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
			 WHERE c.object_id IN `+ssObjectFilter+`
			 ORDER BY c.object_id, c.column_id`, func(rows *sql.Rows) error {
			var r colRow
			c := &r.col
			var computedDef, defaultDef sql.NullString
			if err := rows.Scan(&r.obj, &c.ID, &c.Name,
				&c.Type.Name, &c.Type.Schema, &c.Type.UserDefined,
				&c.Type.MaxLength, &c.Type.Precision, &c.Type.Scale, &c.Nullable,
				&c.Identity, &c.Seed, &c.Increment,
				&c.RowGUID, &c.Sparse,
				&c.Computed, &computedDef, &c.Persisted,
				&c.DefaultName, &defaultDef,
				&c.GeneratedAlways, &c.Hidden); err != nil {
				return err
			}
			c.ComputedDef, c.DefaultDef = computedDef.String, defaultDef.String
			if c.Computed && !computedDef.Valid {
				hide.add(r.obj, "computed column "+c.Name)
			}
			if c.DefaultName != "" && !defaultDef.Valid {
				hide.add(r.obj, "default "+c.DefaultName)
			}
			cols = append(cols, r)
			return nil
		})
		if err != nil {
			return fmt.Errorf("columns: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database, `
			SELECT kc.parent_object_id, kc.object_id, kc.name, kc.type, i.type,
			       col.name, ic.is_descending_key
			  FROM sys.key_constraints kc
			  JOIN sys.indexes i ON i.object_id = kc.parent_object_id AND i.index_id = kc.unique_index_id
			  JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.key_ordinal > 0
			  JOIN sys.columns col ON col.object_id = ic.object_id AND col.column_id = ic.column_id
			 WHERE kc.parent_object_id IN `+ssObjectFilter+`
			 ORDER BY kc.parent_object_id, kc.type, kc.name, ic.key_ordinal`, func(rows *sql.Rows) error {
			var r keyRow
			var typ string
			var idxType int
			if err := rows.Scan(&r.obj, &r.keyID, &r.key.Name, &typ, &idxType, &r.col.Name, &r.col.Desc); err != nil {
				return err
			}
			r.key.Primary = typ == "PK"
			r.key.Clustered = idxType == 1
			keys = append(keys, r)
			return nil
		})
		if err != nil {
			return fmt.Errorf("key constraints: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database, `
			SELECT fk.parent_object_id, fk.object_id, fk.name,
			       SCHEMA_NAME(rt.schema_id), rt.name,
			       fk.delete_referential_action, fk.update_referential_action,
			       pc.name, rc.name
			  FROM sys.foreign_keys fk
			  JOIN sys.objects rt ON rt.object_id = fk.referenced_object_id
			  JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
			  JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
			  JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
			 WHERE fk.is_ms_shipped = 0
			 ORDER BY fk.parent_object_id, fk.name, fkc.constraint_column_id`, func(rows *sql.Rows) error {
			var r fkRow
			if err := rows.Scan(&r.obj, &r.fkID, &r.fk.Name, &r.fk.RefSchema, &r.fk.RefTable,
				&r.fk.OnDelete, &r.fk.OnUpdate, &r.col, &r.ref); err != nil {
				return err
			}
			fks = append(fks, r)
			return nil
		})
		if err != nil {
			return fmt.Errorf("foreign keys: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database, `
			SELECT parent_object_id, name, definition
			  FROM sys.check_constraints
			 WHERE parent_object_id IN `+ssObjectFilter+`
			 ORDER BY parent_object_id, name`, func(rows *sql.Rows) error {
			var r checkRow
			var def sql.NullString
			if err := rows.Scan(&r.obj, &r.check.Name, &def); err != nil {
				return err
			}
			r.check.Def = def.String
			if !def.Valid {
				hide.add(r.obj, "check constraint "+r.check.Name)
			}
			checks = append(checks, r)
			return nil
		})
		if err != nil {
			return fmt.Errorf("check constraints: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		// Key columns in key order, then included columns in declaration
		// order (key_ordinal is 0 for them).
		err := ssQueryRows(gctx, db, database, `
			SELECT i.object_id, i.index_id, i.name, i.type, i.type_desc, i.is_unique,
			       i.has_filter, i.filter_definition,
			       col.name, ic.is_descending_key, ic.is_included_column
			  FROM sys.indexes i
			  JOIN sys.tables t ON t.object_id = i.object_id AND t.is_ms_shipped = 0
			  JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
			  JOIN sys.columns col ON col.object_id = ic.object_id AND col.column_id = ic.column_id
			 WHERE i.type > 0 AND i.is_primary_key = 0 AND i.is_unique_constraint = 0
			   AND i.is_hypothetical = 0
			 ORDER BY i.object_id, i.index_id, ic.is_included_column, ic.key_ordinal, ic.index_column_id`, func(rows *sql.Rows) error {
			var r idxRow
			var hasFilter bool
			var filter sql.NullString
			if err := rows.Scan(&r.obj, &r.idxID, &r.idx.Name, &r.idx.Type, &r.idx.TypeDesc, &r.idx.Unique,
				&hasFilter, &filter, &r.col, &r.desc, &r.incl); err != nil {
				return err
			}
			r.idx.Filter = filter.String
			if hasFilter && !filter.Valid {
				hide.add(r.obj, "filter of index "+r.idx.Name)
			}
			idxs = append(idxs, r)
			return nil
		})
		if err != nil {
			return fmt.Errorf("indexes: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database, `
			SELECT p.object_id, sc.name, ec.name
			  FROM sys.periods p
			  JOIN sys.columns sc ON sc.object_id = p.object_id AND sc.column_id = p.start_column_id
			  JOIN sys.columns ec ON ec.object_id = p.object_id AND ec.column_id = p.end_column_id
			 WHERE p.period_type = 1`, func(rows *sql.Rows) error {
			var r periodRow
			if err := rows.Scan(&r.obj, &r.start, &r.end); err != nil {
				return err
			}
			periods = append(periods, r)
			return nil
		})
		if err != nil {
			return fmt.Errorf("periods: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		// class 1 = object or column (minor_id = column_id, 0 for the
		// object itself).
		err := ssQueryRows(gctx, db, database, `
			SELECT ep.major_id, ep.minor_id, CAST(ep.value AS nvarchar(max))
			  FROM sys.extended_properties ep
			 WHERE ep.class = 1 AND ep.name = N'MS_Description' AND ep.value IS NOT NULL
			 ORDER BY ep.major_id, ep.minor_id`, func(rows *sql.Rows) error {
			var r descRow
			if err := rows.Scan(&r.obj, &r.minor, &r.text); err != nil {
				return err
			}
			descs = append(descs, r)
			return nil
		})
		if err != nil {
			return fmt.Errorf("extended properties: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		// sys.dm_db_partition_stats is exact but needs VIEW DATABASE
		// STATE; sys.partitions.rows (approximate, no extra permission)
		// is the fallback. Heap (0) or clustered index (1) only, so
		// nonclustered indexes do not count the rows twice.
		scan := func(rows *sql.Rows) error {
			var obj, n int64
			if err := rows.Scan(&obj, &n); err != nil {
				return err
			}
			rowCounts[obj] = n
			return nil
		}
		err := ssQueryRows(gctx, db, database, `
			SELECT object_id, SUM(row_count) FROM sys.dm_db_partition_stats
			 WHERE index_id IN (0, 1) GROUP BY object_id`, scan)
		if err == nil {
			return nil
		}
		log.Warn().Err(err).Str("dialect", "sqlserver").
			Msg("sys.dm_db_partition_stats unavailable, row counts from sys.partitions")
		for k := range rowCounts {
			delete(rowCounts, k)
		}
		if err := ssQueryRows(gctx, db, database, `
			SELECT object_id, SUM(rows) FROM sys.partitions
			 WHERE index_id IN (0, 1) GROUP BY object_id`, scan); err != nil {
			return fmt.Errorf("row counts: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database,
			`SELECT object_id FROM sys.fulltext_indexes`, func(rows *sql.Rows) error {
				var obj int64
				if err := rows.Scan(&obj); err != nil {
					return err
				}
				fulltext[obj] = true
				return nil
			})
		if err != nil {
			return fmt.Errorf("fulltext indexes: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database, `
			SELECT SCHEMA_NAME(t.schema_id), t.name, TYPE_NAME(t.system_type_id),
			       t.max_length, t.precision, t.scale, t.is_nullable
			  FROM sys.types t
			 WHERE t.is_user_defined = 1 AND t.is_table_type = 0 AND t.is_assembly_type = 0
			 ORDER BY 1, 2`, func(rows *sql.Rows) error {
			var a ssAliasType
			if err := rows.Scan(&a.Schema, &a.Name, &a.Base.Name,
				&a.Base.MaxLength, &a.Base.Precision, &a.Base.Scale, &a.Nullable); err != nil {
				return err
			}
			cat.aliasTypes = append(cat.aliasTypes, a)
			return nil
		})
		if err != nil {
			return fmt.Errorf("alias types: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		err := ssQueryRows(gctx, db, database, `
			SELECT SCHEMA_NAME(s.schema_id), s.name, TYPE_NAME(s.system_type_id),
			       s.precision, s.scale,
			       CAST(s.start_value AS nvarchar(64)), CAST(s.increment AS nvarchar(64)),
			       CAST(s.minimum_value AS nvarchar(64)), CAST(s.maximum_value AS nvarchar(64)),
			       ISNULL(CAST(s.current_value AS nvarchar(64)), ''), s.is_cycling
			  FROM sys.sequences s
			 WHERE s.is_ms_shipped = 0
			 ORDER BY 1, 2`, func(rows *sql.Rows) error {
			var q ssSequence
			if err := rows.Scan(&q.Schema, &q.Name, &q.Type.Name, &q.Type.Precision, &q.Type.Scale,
				&q.Start, &q.Increment, &q.Min, &q.Max, &q.Current, &q.Cycle); err != nil {
				return err
			}
			cat.sequences = append(cat.sequences, q)
			return nil
		})
		if err != nil {
			return fmt.Errorf("sequences: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := hide.err(database, byID); err != nil {
		return nil, err
	}

	// --- merge (rows arrive ordered per object, see each ORDER BY) ---
	for _, r := range cols {
		if t := byID[r.obj]; t != nil {
			t.Columns = append(t.Columns, r.col)
		}
	}
	lastKey := map[int64]int64{}
	for _, r := range keys {
		t := byID[r.obj]
		if t == nil {
			continue
		}
		if id, ok := lastKey[r.obj]; !ok || id != r.keyID {
			t.Keys = append(t.Keys, r.key)
			lastKey[r.obj] = r.keyID
		}
		k := &t.Keys[len(t.Keys)-1]
		k.Cols = append(k.Cols, r.col)
	}
	lastFK := map[int64]int64{}
	for _, r := range fks {
		t := byID[r.obj]
		if t == nil {
			continue
		}
		if id, ok := lastFK[r.obj]; !ok || id != r.fkID {
			t.FKs = append(t.FKs, r.fk)
			lastFK[r.obj] = r.fkID
		}
		f := &t.FKs[len(t.FKs)-1]
		f.Cols = append(f.Cols, r.col)
		f.RefCols = append(f.RefCols, r.ref)
	}
	for _, r := range checks {
		if t := byID[r.obj]; t != nil {
			t.Checks = append(t.Checks, r.check)
		}
	}
	lastIdx := map[int64]int{}
	for _, r := range idxs {
		t := byID[r.obj]
		if t == nil {
			continue
		}
		if id, ok := lastIdx[r.obj]; !ok || id != r.idxID {
			t.Indexes = append(t.Indexes, r.idx)
			lastIdx[r.obj] = r.idxID
		}
		ix := &t.Indexes[len(t.Indexes)-1]
		if r.incl {
			ix.Include = append(ix.Include, r.col)
		} else {
			ix.Keys = append(ix.Keys, ssKeyCol{Name: r.col, Desc: r.desc})
		}
	}
	for _, r := range periods {
		if t := byID[r.obj]; t != nil {
			t.PeriodStart, t.PeriodEnd = r.start, r.end
		}
	}
	for _, r := range descs {
		t := byID[r.obj]
		if t == nil {
			continue
		}
		if r.minor == 0 {
			t.Description = r.text
			continue
		}
		if t.ColumnDescriptions == nil {
			t.ColumnDescriptions = map[int]string{}
		}
		t.ColumnDescriptions[r.minor] = r.text
	}
	for _, t := range cat.tables {
		t.Rows = rowCounts[t.ObjectID]
		t.Fulltext = fulltext[t.ObjectID]
	}
	return cat, nil
}

// ---------------------------------------------------------------------------
// DDL synthesis — pure functions over catalog values
// ---------------------------------------------------------------------------
//
// Identifiers and literals below are catalog *values* (names, comment
// text, numbers) being quoted into a script, not SQL fragments being
// inspected. The only SQL fragments copied in are the expressions SQL
// Server itself stores normalized (default / computed / check / filter
// definitions), passed through verbatim for the tsql parser.

// ssQuoteIdent brackets one identifier, doubling `]` (the QUOTENAME rule).
func ssQuoteIdent(id string) string {
	var b strings.Builder
	b.Grow(len(id) + 2)
	b.WriteByte('[')
	for _, r := range id {
		if r == ']' {
			b.WriteByte(']')
		}
		b.WriteRune(r)
	}
	b.WriteByte(']')
	return b.String()
}

// ssQuoteName renders [schema].[name] ([name] when schema is empty).
func ssQuoteName(schema, name string) string {
	if schema == "" {
		return ssQuoteIdent(name)
	}
	return ssQuoteIdent(schema) + "." + ssQuoteIdent(name)
}

// ssNString renders a Unicode string literal N'…', doubling quotes.
func ssNString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 3)
	b.WriteString("N'")
	for _, r := range s {
		if r == '\'' {
			b.WriteByte('\'')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// ssCommentSafe makes a value safe inside a `--` line comment: a line
// break would end the comment and turn the rest of an object name into
// script text.
func ssCommentSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\n', '\r', ' ', ' ', '\v', '\f', '\u0085':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ssNumeric reports whether s is a plain (optionally signed, optionally
// fractional) decimal number — the shape of the identity seeds and
// sequence bounds read from sql_variant / numeric catalog columns.
func ssNumeric(s string) bool {
	digits := 0
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r == '-' || r == '+') && i == 0:
		case r == '.':
		default:
			return false
		}
	}
	return digits > 0
}

// ssSystemTypes are the system type names emitted bare. Anything else
// (unknown future types) is bracketed so a name can never leak as script.
var ssSystemTypes = map[string]bool{
	"bigint": true, "int": true, "smallint": true, "tinyint": true, "bit": true,
	"decimal": true, "numeric": true, "money": true, "smallmoney": true,
	"float": true, "real": true,
	"date": true, "time": true, "datetime": true, "datetime2": true,
	"smalldatetime": true, "datetimeoffset": true,
	"char": true, "varchar": true, "text": true,
	"nchar": true, "nvarchar": true, "ntext": true,
	"binary": true, "varbinary": true, "image": true,
	"uniqueidentifier": true, "xml": true, "sql_variant": true,
	"hierarchyid": true, "geography": true, "geometry": true,
	"sysname": true, "timestamp": true,
}

// ssFormatType renders a column / alias base / sequence type with its
// length, precision or scale. Collations are not emitted (identifier and
// data case rules are handled by the translator, not per column).
func ssFormatType(t ssType) string {
	if t.UserDefined {
		return ssQuoteName(t.Schema, t.Name)
	}
	switch t.Name {
	case "char", "varchar", "binary", "varbinary":
		return t.Name + ssLength(t.MaxLength, 1)
	case "nchar", "nvarchar":
		// max_length counts bytes; one UTF-16 code unit is two.
		return t.Name + ssLength(t.MaxLength, 2)
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", t.Name, t.Precision, t.Scale)
	case "datetime2", "time", "datetimeoffset":
		return fmt.Sprintf("%s(%d)", t.Name, t.Scale)
	case "float":
		// sys.columns.precision is 53 for float(25..53), 24 for float(1..24).
		if t.Precision > 0 && t.Precision != 53 {
			return fmt.Sprintf("float(%d)", t.Precision)
		}
		return "float"
	case "timestamp":
		// sys.types still calls rowversion "timestamp"; the synonym avoids
		// any confusion with the ISO timestamp type.
		return "rowversion"
	}
	if ssSystemTypes[t.Name] {
		return t.Name
	}
	return ssQuoteIdent(t.Name)
}

func ssLength(maxLength, unit int) string {
	if maxLength < 0 {
		return "(max)"
	}
	return "(" + strconv.Itoa(maxLength/unit) + ")"
}

// ssColumnDef renders one column definition of a CREATE TABLE / CREATE
// TYPE … AS TABLE body.
func ssColumnDef(c ssColumn) string {
	var b strings.Builder
	b.WriteString(ssQuoteIdent(c.Name))
	if c.Computed {
		b.WriteString(" AS (")
		b.WriteString(c.ComputedDef)
		b.WriteString(")")
		if c.Persisted {
			b.WriteString(" PERSISTED")
			// NOT NULL is only legal (and only meaningful) on a persisted
			// computed column.
			if !c.Nullable {
				b.WriteString(" NOT NULL")
			}
		}
		return b.String()
	}
	b.WriteByte(' ')
	b.WriteString(ssFormatType(c.Type))
	if c.Sparse {
		b.WriteString(" SPARSE")
	}
	if c.Identity {
		b.WriteString(" IDENTITY")
		if ssNumeric(c.Seed) && ssNumeric(c.Increment) {
			b.WriteString("(" + c.Seed + "," + c.Increment + ")")
		}
	}
	switch c.GeneratedAlways {
	case 1:
		b.WriteString(" GENERATED ALWAYS AS ROW START")
	case 2:
		b.WriteString(" GENERATED ALWAYS AS ROW END")
	}
	if c.GeneratedAlways != 0 && c.Hidden {
		b.WriteString(" HIDDEN")
	}
	if c.RowGUID {
		b.WriteString(" ROWGUIDCOL")
	}
	if c.Nullable {
		b.WriteString(" NULL")
	} else {
		b.WriteString(" NOT NULL")
	}
	if c.DefaultDef != "" {
		if c.DefaultName != "" {
			b.WriteString(" CONSTRAINT " + ssQuoteIdent(c.DefaultName))
		}
		b.WriteString(" DEFAULT (" + c.DefaultDef + ")")
	}
	return b.String()
}

func ssKeyCols(cols []ssKeyCol) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		dir := " ASC"
		if c.Desc {
			dir = " DESC"
		}
		parts[i] = ssQuoteIdent(c.Name) + dir
	}
	return strings.Join(parts, ", ")
}

func ssIdentList(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = ssQuoteIdent(n)
	}
	return strings.Join(parts, ", ")
}

func ssKeyDef(k ssKey) string {
	var b strings.Builder
	b.WriteString("CONSTRAINT " + ssQuoteIdent(k.Name))
	switch {
	case k.Primary && k.Clustered:
		b.WriteString(" PRIMARY KEY CLUSTERED")
	case k.Primary:
		b.WriteString(" PRIMARY KEY NONCLUSTERED")
	case k.Clustered:
		b.WriteString(" UNIQUE CLUSTERED")
	default:
		b.WriteString(" UNIQUE")
	}
	b.WriteString(" (" + ssKeyCols(k.Cols) + ")")
	return b.String()
}

func ssFKAction(a int) string {
	switch a {
	case 1:
		return "CASCADE"
	case 2:
		return "SET NULL"
	case 3:
		return "SET DEFAULT"
	}
	return ""
}

func ssFKDef(f ssFK) string {
	var b strings.Builder
	b.WriteString("CONSTRAINT " + ssQuoteIdent(f.Name))
	b.WriteString(" FOREIGN KEY (" + ssIdentList(f.Cols) + ")")
	b.WriteString(" REFERENCES " + ssQuoteName(f.RefSchema, f.RefTable))
	b.WriteString(" (" + ssIdentList(f.RefCols) + ")")
	if a := ssFKAction(f.OnDelete); a != "" {
		b.WriteString(" ON DELETE " + a)
	}
	if a := ssFKAction(f.OnUpdate); a != "" {
		b.WriteString(" ON UPDATE " + a)
	}
	return b.String()
}

// ssTableBody renders the parenthesized element list shared by CREATE
// TABLE and CREATE TYPE … AS TABLE.
func ssTableBody(t *ssTable) string {
	var elems []string
	for _, c := range t.Columns {
		elems = append(elems, ssColumnDef(c))
	}
	for _, k := range t.Keys {
		elems = append(elems, ssKeyDef(k))
	}
	for _, f := range t.FKs {
		elems = append(elems, ssFKDef(f))
	}
	for _, c := range t.Checks {
		elems = append(elems, "CONSTRAINT "+ssQuoteIdent(c.Name)+" CHECK ("+c.Def+")")
	}
	if t.PeriodStart != "" && t.PeriodEnd != "" {
		elems = append(elems, "PERIOD FOR SYSTEM_TIME ("+
			ssQuoteIdent(t.PeriodStart)+", "+ssQuoteIdent(t.PeriodEnd)+")")
	}
	return "(\n    " + strings.Join(elems, ",\n    ") + "\n)"
}

// ssTableDDL renders the canonical script of one table: CREATE TABLE,
// then its non-constraint indexes, then its MS_Description properties,
// each statement `;`-terminated.
func ssTableDDL(t *ssTable) string {
	var b strings.Builder
	qn := ssQuoteName(t.Schema, t.Name)
	b.WriteString("CREATE TABLE " + qn + " " + ssTableBody(t))
	if t.TemporalType == 2 && t.HistoryName != "" {
		b.WriteString(" WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = " +
			ssQuoteName(t.HistorySchema, t.HistoryName) + "))")
	}
	b.WriteString(";\n")

	for _, ix := range t.Indexes {
		b.WriteString(ssIndexDDL(qn, ix))
	}
	if t.Fulltext {
		b.WriteString("-- skipped fulltext index on " + ssCommentSafe(qn) + " (FULLTEXT)\n")
	}

	if t.Description != "" {
		b.WriteString(ssDescriptionDDL(t.Description, t.Schema, t.Name, ""))
	}
	if len(t.ColumnDescriptions) > 0 {
		for _, c := range t.Columns {
			if d, ok := t.ColumnDescriptions[c.ID]; ok && d != "" {
				b.WriteString(ssDescriptionDDL(d, t.Schema, t.Name, c.Name))
			}
		}
	}
	return b.String()
}

// ssIndexDDL renders one CREATE INDEX, or a comment line for the index
// kinds PG has no counterpart for (XML, spatial, columnstore).
func ssIndexDDL(table string, ix ssIndex) string {
	var kind string
	switch ix.Type {
	case 1:
		kind = "CLUSTERED"
	case 2, 7: // 7 = hash index of a memory-optimized table: a plain index for PG.
		kind = "NONCLUSTERED"
	default:
		return "-- skipped index " + ssCommentSafe(ssQuoteIdent(ix.Name)) + " on " +
			ssCommentSafe(table) + " (" + ssCommentSafe(ix.TypeDesc) + ")\n"
	}
	var b strings.Builder
	b.WriteString("CREATE ")
	if ix.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString(kind + " INDEX " + ssQuoteIdent(ix.Name) + " ON " + table)
	b.WriteString(" (" + ssKeyCols(ix.Keys) + ")")
	if len(ix.Include) > 0 {
		b.WriteString(" INCLUDE (" + ssIdentList(ix.Include) + ")")
	}
	if ix.Filter != "" {
		b.WriteString(" WHERE (" + ix.Filter + ")")
	}
	b.WriteString(";\n")
	return b.String()
}

// ssDescriptionDDL renders the sp_addextendedproperty call that sets a
// table's (column == "") or a column's MS_Description — the same call
// real scripts carry, so both reach the translator as one AST node.
func ssDescriptionDDL(text, schema, table, column string) string {
	var b strings.Builder
	b.WriteString("EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=")
	b.WriteString(ssNString(text))
	b.WriteString(", @level0type=N'SCHEMA', @level0name=" + ssNString(schema))
	b.WriteString(", @level1type=N'TABLE', @level1name=" + ssNString(table))
	if column != "" {
		b.WriteString(", @level2type=N'COLUMN', @level2name=" + ssNString(column))
	}
	b.WriteString(";\n")
	return b.String()
}

// ssTableTypeDDL renders CREATE TYPE … AS TABLE.
func ssTableTypeDDL(t *ssTable) string {
	return "CREATE TYPE " + ssQuoteName(t.Schema, t.Name) + " AS TABLE " + ssTableBody(t) + ";\n"
}

// ssAliasTypeDDL renders CREATE TYPE … FROM base [NOT NULL].
func ssAliasTypeDDL(a ssAliasType) string {
	null := " NULL"
	if !a.Nullable {
		null = " NOT NULL"
	}
	return "CREATE TYPE " + ssQuoteName(a.Schema, a.Name) + " FROM " + ssFormatType(a.Base) + null + ";\n"
}

// ssSequenceDDL renders CREATE SEQUENCE positioned on the next value the
// source would hand out, so PG never repeats a value SQL Server already
// returned.
//
// sys.sequences.current_value is the last value handed out — or, on a
// sequence never used, its start value (which NEXT VALUE FOR would still
// return). The two cases are indistinguishable on SQL Server 2016
// (last_used_value only exists from 2017), so the PG sequence always
// starts one increment past current_value: a fresh sequence loses its
// first value (a gap, harmless), a used one never repeats. A sequence at
// its bound restarts at the opposite bound when it cycles; an exhausted
// non-cycling one keeps current_value (PG rejects a START WITH outside
// MINVALUE..MAXVALUE) and says so in a comment.
func ssSequenceDDL(q ssSequence) string {
	typ := q.Type
	if typ.Name == "decimal" || typ.Name == "numeric" {
		typ.Scale = 0 // sequences are integral
	}
	start, note := ssSequenceNext(q)
	var b strings.Builder
	if note != "" {
		b.WriteString("-- " + ssCommentSafe(ssQuoteName(q.Schema, q.Name)) + ": " + note + "\n")
	}
	b.WriteString("CREATE SEQUENCE " + ssQuoteName(q.Schema, q.Name) + " AS " + ssFormatType(typ))
	if start != "" {
		b.WriteString(" START WITH " + start)
	}
	if ssNumeric(q.Increment) {
		b.WriteString(" INCREMENT BY " + q.Increment)
	}
	if ssNumeric(q.Min) {
		b.WriteString(" MINVALUE " + q.Min)
	}
	if ssNumeric(q.Max) {
		b.WriteString(" MAXVALUE " + q.Max)
	}
	if q.Cycle {
		b.WriteString(" CYCLE")
	} else {
		b.WriteString(" NO CYCLE")
	}
	b.WriteString(";\n")
	return b.String()
}

// ssSequenceNext computes the START WITH value (see ssSequenceDDL) with
// arbitrary precision — decimal(38,0) sequences exceed int64. It falls
// back to the declared start value when the catalog values do not parse.
func ssSequenceNext(q ssSequence) (string, string) {
	fallback := ""
	if ssNumeric(q.Start) {
		fallback = q.Start
	}
	cur, ok1 := new(big.Int).SetString(q.Current, 10)
	inc, ok2 := new(big.Int).SetString(q.Increment, 10)
	minV, ok3 := new(big.Int).SetString(q.Min, 10)
	maxV, ok4 := new(big.Int).SetString(q.Max, 10)
	if !ok1 || !ok2 || !ok3 || !ok4 || inc.Sign() == 0 {
		return fallback, ""
	}
	next := new(big.Int).Add(cur, inc)
	if next.Cmp(minV) >= 0 && next.Cmp(maxV) <= 0 {
		return next.String(), ""
	}
	if q.Cycle {
		if inc.Sign() > 0 {
			return minV.String(), ""
		}
		return maxV.String(), ""
	}
	return cur.String(), "exhausted on the source (current value " + cur.String() + " is its bound)"
}

// ---------------------------------------------------------------------------
// Definitions the login cannot see
// ---------------------------------------------------------------------------

// ssHidden collects the catalog definitions that read as NULL although
// the object has one: SQL Server returns NULL for sys.check_constraints /
// default_constraints / computed_columns / indexes.filter_definition and
// sys.sql_modules.definition when the login lacks VIEW DEFINITION on the
// object (a db_datareader-only migration login). Read as empty strings
// they would silently drop defaults and index filters, render computed
// columns as `AS ()` and modules as encrypted: inspection fails instead,
// naming them and the missing permission. Loaders run concurrently.
type ssHidden struct {
	mu    sync.Mutex
	items []ssHiddenItem
}

type ssHiddenItem struct {
	obj  int64  // owning table's object_id (0: a module, named in what)
	what string // "default DF_x", "check constraint CK_y", "view dbo.v", …
}

func (h *ssHidden) add(obj int64, what string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append(h.items, ssHiddenItem{obj: obj, what: what})
}

// err returns the inspection error listing the hidden definitions (at
// most 10, then a count), nil when there is none.
func (h *ssHidden) err(database string, byID map[int64]*ssTable) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.items) == 0 {
		return nil
	}
	names := make([]string, 0, len(h.items))
	for _, it := range h.items {
		name := it.what
		if t := byID[it.obj]; t != nil {
			name += " of " + t.Schema + "." + t.Name
		}
		names = append(names, name)
	}
	more := ""
	if len(names) > 10 {
		more = fmt.Sprintf(" and %d more", len(names)-10)
		names = names[:10]
	}
	return fmt.Errorf("sqlserver: the login cannot read the definition of %s%s in database %s (SQL Server returns NULL without VIEW DEFINITION): GRANT VIEW DEFINITION ON DATABASE::%s TO the migration login, then inspect again",
		strings.Join(names, ", "), more, database, ssQuoteIdent(database))
}
