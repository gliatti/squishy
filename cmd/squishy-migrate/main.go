// Command squishy-migrate applies or rolls back the app schema migrations
// embedded in internal/storage, using the same ledger
// (squishy_meta._migrations) as the api's boot-time migrator.
//
// Usage:
//
//	squishy-migrate up          apply every pending migration
//	squishy-migrate down [N]    roll back the N latest migrations (default 1)
//	squishy-migrate status      list migrations and whether they are applied
//
// The DSN comes from SQUISHY_PG_DSN.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"gitlab.com/dalibo/squishy/internal/storage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "squishy-migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: squishy-migrate up | down [N] | status")
	}
	dsn := os.Getenv("SQUISHY_PG_DSN")
	if dsn == "" {
		return fmt.Errorf("SQUISHY_PG_DSN is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := storage.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "up":
		if err := db.Migrate(ctx); err != nil {
			return err
		}
	case "down":
		n := 1
		if len(args) > 1 {
			if n, err = strconv.Atoi(args[1]); err != nil {
				return fmt.Errorf("down: invalid step count %q", args[1])
			}
		}
		if err := db.MigrateDown(ctx, n); err != nil {
			return err
		}
	case "status":
	default:
		return fmt.Errorf("unknown command %q (want up, down or status)", args[0])
	}

	all, applied, err := db.MigrationStatus(ctx)
	if err != nil {
		return err
	}
	for _, m := range all {
		state := "pending"
		if _, ok := applied[m.Version]; ok {
			state = "applied"
		}
		fmt.Printf("%06d  %-8s %s\n", m.Version, state, m.Name)
	}
	return nil
}
