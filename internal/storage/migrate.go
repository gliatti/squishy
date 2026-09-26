package storage

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationsDir is the directory inside embeddedMigrations holding the files.
const migrationsDir = "migrations"

// migrateLockKey is the pg_advisory_lock key serialising concurrent
// migrators (the api's boot-time Migrate and the `squishy-migrate` CLI used
// by the e2e/migrate compose service share the same ledger).
const migrateLockKey int64 = 0x5351_5549_5348_59 // "SQUISHY"

// Migration is one versioned schema change: a mandatory up script and an
// optional down script, both named <version>_<name>.(up|down).sql — the
// same layout `migrate create -ext sql -seq` scaffolds.
type Migration struct {
	Version  int64
	Name     string
	UpFile   string
	DownFile string
}

// LoadMigrations lists the migrations found in dir of fsys, ordered by
// numeric version. Ordering is numeric on purpose: a lexical sort puts
// "000002_x" before "0001_y", which is how the init script once ended up
// running after the migrations that alter its objects.
func LoadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	byVersion := map[int64]*Migration{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		file := e.Name()
		var direction string
		switch {
		case strings.HasSuffix(file, ".up.sql"):
			direction = "up"
		case strings.HasSuffix(file, ".down.sql"):
			direction = "down"
		default:
			continue
		}
		version, name, err := parseMigrationFilename(file)
		if err != nil {
			return nil, err
		}
		m, ok := byVersion[version]
		if !ok {
			m = &Migration{Version: version, Name: name}
			byVersion[version] = m
		} else if m.Name != name {
			return nil, fmt.Errorf("migration version %d used by two names: %q and %q", version, m.Name, name)
		}
		target := &m.UpFile
		if direction == "down" {
			target = &m.DownFile
		}
		if *target != "" {
			return nil, fmt.Errorf("migration version %d has two %s scripts: %s and %s", version, direction, *target, file)
		}
		*target = file
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.UpFile == "" {
			return nil, fmt.Errorf("migration version %d (%s) has no .up.sql script", m.Version, m.Name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseMigrationFilename splits "000002_add_db2.up.sql" into (2, "add_db2").
// It operates on a single file name, never on SQL content.
func parseMigrationFilename(file string) (int64, string, error) {
	base := strings.TrimSuffix(strings.TrimSuffix(file, ".up.sql"), ".down.sql")
	digits, name, ok := strings.Cut(base, "_")
	if !ok || digits == "" || name == "" {
		return 0, "", fmt.Errorf("migration %q: expected <version>_<name>.(up|down).sql", file)
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, "", fmt.Errorf("migration %q: version prefix %q is not numeric", file, digits)
		}
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q: %w", file, err)
	}
	return v, name, nil
}

// appliedVersions maps the ledger's recorded file names to versions. The
// ledger is keyed by file name for history, but a migration counts as
// applied by version, so renaming a file (0001_init → 000001_init) does not
// replay it.
func appliedVersions(ledgerFiles []string) (map[int64]string, error) {
	out := make(map[int64]string, len(ledgerFiles))
	for _, f := range ledgerFiles {
		v, _, err := parseMigrationFilename(f)
		if err != nil {
			return nil, fmt.Errorf("ledger entry: %w", err)
		}
		out[v] = f
	}
	return out, nil
}

// pendingMigrations returns the migrations not yet applied, in order.
func pendingMigrations(all []Migration, applied map[int64]string) []Migration {
	var out []Migration
	for _, m := range all {
		if _, ok := applied[m.Version]; !ok {
			out = append(out, m)
		}
	}
	return out
}

// rollbackMigrations returns the n most recent applied migrations, newest
// first. It fails when one of them has no down script.
func rollbackMigrations(all []Migration, applied map[int64]string, n int) ([]Migration, error) {
	var out []Migration
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		m := all[i]
		if _, ok := applied[m.Version]; !ok {
			continue
		}
		if m.DownFile == "" {
			return nil, fmt.Errorf("migration %d (%s) has no .down.sql script", m.Version, m.Name)
		}
		out = append(out, m)
	}
	return out, nil
}

// Migrate applies every pending embedded migration, in version order.
//
// There is a single ledger, squishy_meta._migrations, shared by the api's
// boot-time call and the `squishy-migrate` CLI (compose service `migrate`).
// It lives outside the squishy schema so DROP SCHEMA squishy CASCADE (the
// init down script) does not erase history.
func (d *DB) Migrate(ctx context.Context) error {
	all, err := LoadMigrations(embeddedMigrations, migrationsDir)
	if err != nil {
		return err
	}
	return d.withMigrateLock(ctx, func(conn *pgx.Conn) error {
		applied, err := readLedger(ctx, conn)
		if err != nil {
			return err
		}
		for _, m := range pendingMigrations(all, applied) {
			if err := runScript(ctx, conn, m.UpFile, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO squishy_meta._migrations(filename) VALUES ($1)`, m.UpFile)
				return err
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// MigrateDown rolls back the n most recently applied migrations.
func (d *DB) MigrateDown(ctx context.Context, n int) error {
	if n < 1 {
		return fmt.Errorf("migrate down: step count must be >= 1, got %d", n)
	}
	all, err := LoadMigrations(embeddedMigrations, migrationsDir)
	if err != nil {
		return err
	}
	return d.withMigrateLock(ctx, func(conn *pgx.Conn) error {
		applied, err := readLedger(ctx, conn)
		if err != nil {
			return err
		}
		todo, err := rollbackMigrations(all, applied, n)
		if err != nil {
			return err
		}
		for _, m := range todo {
			ledgerFile := applied[m.Version]
			if err := runScript(ctx, conn, m.DownFile, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM squishy_meta._migrations WHERE filename=$1`, ledgerFile)
				return err
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// MigrationStatus reports every embedded migration with its applied flag.
func (d *DB) MigrationStatus(ctx context.Context) ([]Migration, map[int64]string, error) {
	all, err := LoadMigrations(embeddedMigrations, migrationsDir)
	if err != nil {
		return nil, nil, err
	}
	var applied map[int64]string
	err = d.withMigrateLock(ctx, func(conn *pgx.Conn) error {
		var err error
		applied, err = readLedger(ctx, conn)
		return err
	})
	return all, applied, err
}

// withMigrateLock pins one connection, creates the ledger if needed and
// holds a session advisory lock while fn runs, so two migrators started
// concurrently never apply the same script twice.
func (d *DB) withMigrateLock(ctx context.Context, fn func(conn *pgx.Conn) error) error {
	pc, err := d.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migrate conn: %w", err)
	}
	defer pc.Release()
	conn := pc.Conn()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("migrate lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey) }()

	if _, err := conn.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS squishy_meta;
		CREATE TABLE IF NOT EXISTS squishy_meta._migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`); err != nil {
		return fmt.Errorf("create ledger: %w", err)
	}
	return fn(conn)
}

func readLedger(ctx context.Context, conn *pgx.Conn) (map[int64]string, error) {
	rows, err := conn.Query(ctx, `SELECT filename FROM squishy_meta._migrations`)
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	files, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	return appliedVersions(files)
}

// runScript executes one embedded script and its ledger update in a single
// transaction.
func runScript(ctx context.Context, conn *pgx.Conn, file string, record func(pgx.Tx) error) error {
	body, err := fs.ReadFile(embeddedMigrations, path.Join(migrationsDir, file))
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", file, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("exec %s: %w", file, err)
	}
	if err := record(tx); err != nil {
		return fmt.Errorf("record %s: %w", file, err)
	}
	return tx.Commit(ctx)
}
