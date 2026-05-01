package storage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

type DB struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() {
	if d != nil && d.Pool != nil {
		d.Pool.Close()
	}
}

// WithTx executes fn inside a transaction, committing on nil error.
func (d *DB) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Migrate applies all *.up.sql files embedded under migrations/ in order.
// Keeps track in a lightweight table squishy._migrations (id, applied_at).
// This is a minimal self-contained runner to avoid dragging the full golang-migrate
// runtime into the binary (the migrate/migrate image in docker-compose is used for
// explicit runs in the e2e profile).
func (d *DB) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(embeddedMigrations, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	ups := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".up.sql") {
			ups = append(ups, name)
		}
	}
	sort.Strings(ups)

	// Ledger table lives outside the squishy schema so DROP SCHEMA squishy CASCADE (down) doesn't nuke history.
	if _, err := d.Pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS squishy_meta;
		CREATE TABLE IF NOT EXISTS squishy_meta._migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`); err != nil {
		return fmt.Errorf("create ledger: %w", err)
	}

	for _, f := range ups {
		var exists bool
		if err := d.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM squishy_meta._migrations WHERE filename=$1)`, f,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check %s: %w", f, err)
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(embeddedMigrations, filepath.ToSlash(filepath.Join("migrations", f)))
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if err := d.WithTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("exec %s: %w", f, err)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO squishy_meta._migrations(filename) VALUES ($1)`, f)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// ErrNotFound is the canonical not-found error for repositories.
var ErrNotFound = errors.New("not found")
