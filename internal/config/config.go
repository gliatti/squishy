package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr     string
	PGDSN        string
	Workers      int
	WorkerID     string
	LogLevel     string
	BatchSize    int
	MigrationDir string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:     getenv("SQUISHY_HTTP_ADDR", ":8080"),
		PGDSN:        os.Getenv("SQUISHY_PG_DSN"),
		WorkerID:     getenv("SQUISHY_WORKER_ID", hostnameOr("api")),
		LogLevel:     strings.ToLower(getenv("SQUISHY_LOG_LEVEL", "info")),
		MigrationDir: getenv("SQUISHY_MIGRATIONS_DIR", "internal/storage/migrations"),
	}
	w, err := strconv.Atoi(getenv("SQUISHY_WORKERS", "4"))
	if err != nil || w < 0 {
		return c, fmt.Errorf("SQUISHY_WORKERS invalid: %v", err)
	}
	c.Workers = w

	b, err := strconv.Atoi(getenv("SQUISHY_BATCH_SIZE", "10000"))
	if err != nil || b < 1 {
		return c, fmt.Errorf("SQUISHY_BATCH_SIZE invalid: %v", err)
	}
	c.BatchSize = b

	if c.PGDSN == "" {
		return c, fmt.Errorf("SQUISHY_PG_DSN is required")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func hostnameOr(fallback string) string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return fallback
	}
	return h
}
