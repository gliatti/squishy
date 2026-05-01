// Package connection centralizes creation of source/target database connections
// and provides connectivity probes used by the wizard.
package connection

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/sijms/go-ora/v2"
)

// Params is the canonical connection spec persisted in squishy.connections.
type Params struct {
	Kind     string // mysql|mariadb|oracle|oracle19|db2|db2zos|postgres
	Host     string
	Port     int
	Database string // schema/db/service name depending on Kind
	Username string
	Password string
	SSLMode  string
	Extra    map[string]string
}

// MySQLDSN returns a go-sql-driver DSN (user:pass@tcp(host:port)/db?...).
func (p Params) MySQLDSN() string {
	q := url.Values{}
	q.Set("parseTime", "true")
	q.Set("multiStatements", "true")
	q.Set("charset", "utf8mb4")
	for k, v := range p.Extra {
		q.Set(k, v)
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
		p.Username, p.Password, p.Host, p.Port, p.Database, q.Encode())
}

// OracleDSN returns an oracle:// URL accepted by github.com/sijms/go-ora/v2.
// Database is the service name (e.g. FREEPDB1 for Oracle Free 23ai).
func (p Params) OracleDSN() string {
	q := url.Values{}
	for k, v := range p.Extra {
		q.Set(k, v)
	}
	u := url.URL{
		Scheme: "oracle",
		User:   url.UserPassword(p.Username, p.Password),
		Host:   fmt.Sprintf("%s:%d", p.Host, p.Port),
		Path:   "/" + p.Database,
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// OpenOracle returns a pooled *sql.DB backed by sijms/go-ora (pure Go, no
// Oracle Instant Client required).
func OpenOracle(ctx context.Context, p Params) (*sql.DB, error) {
	db, err := sql.Open("oracle", p.OracleDSN())
	if err != nil {
		return nil, fmt.Errorf("open oracle: %w", err)
	}
	// Pool sized for parallel introspection (DBMS_METADATA / SHOW CREATE /
	// SYSCAT walks fan out to inspectXxxConcurrency workers, each holding a
	// *sql.Conn). Idle == Max keeps connections hot across sections to avoid
	// the auth + session-setup overhead of reopening every section.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	pingCtx, cancel := contextWithTimeout(ctx)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping oracle: %w", err)
	}
	return db, nil
}

// DB2DSN builds a go_ibm_db connection string. The driver expects the legacy
// IBM CLI key=value format, NOT a URL.
//
//	"DATABASE=…;HOSTNAME=…;PORT=…;PROTOCOL=TCPIP;UID=…;PWD=…;[Security=SSL;]"
//
// For z/OS connections via DRDA/DDF, the same shape is used with PORT=446
// (default) and DATABASE=<location-name>. SSL is enabled when SSLMode is
// "require" or "verify-full"; additional CLI options can be passed via
// Extra (e.g. SSLServerCertificate=<path>).
func (p Params) DB2DSN() string {
	parts := []string{
		"DATABASE=" + p.Database,
		fmt.Sprintf("HOSTNAME=%s", p.Host),
		fmt.Sprintf("PORT=%d", p.Port),
		"PROTOCOL=TCPIP",
		"UID=" + p.Username,
		"PWD=" + p.Password,
	}
	switch p.SSLMode {
	case "require", "verify-full", "verify-ca":
		parts = append(parts, "Security=SSL")
	}
	for k, v := range p.Extra {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ";") + ";"
}

// OpenDB2 returns a pooled *sql.DB backed by github.com/ibmdb/go_ibm_db
// (CGO; needs libdb2/clidriver at runtime — see Dockerfile).
func OpenDB2(ctx context.Context, p Params) (*sql.DB, error) {
	db, err := sql.Open("go_ibm_db", p.DB2DSN())
	if err != nil {
		return nil, fmt.Errorf("open db2: %w", err)
	}
	// Pool sized for parallel introspection (DBMS_METADATA / SHOW CREATE /
	// SYSCAT walks fan out to inspectXxxConcurrency workers, each holding a
	// *sql.Conn). Idle == Max keeps connections hot across sections to avoid
	// the auth + session-setup overhead of reopening every section.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	pingCtx, cancel := contextWithTimeout(ctx)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db2: %w", err)
	}
	return db, nil
}

// PostgresDSN returns a libpq-style connection string.
func (p Params) PostgresDSN() string {
	ssl := p.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(p.Username, p.Password),
		Host:   fmt.Sprintf("%s:%d", p.Host, p.Port),
		Path:   "/" + p.Database,
	}
	q := u.Query()
	q.Set("sslmode", ssl)
	for k, v := range p.Extra {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// OpenMySQL returns a pooled *sql.DB.
func OpenMySQL(ctx context.Context, p Params) (*sql.DB, error) {
	db, err := sql.Open("mysql", p.MySQLDSN())
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	// Pool sized for parallel introspection (DBMS_METADATA / SHOW CREATE /
	// SYSCAT walks fan out to inspectXxxConcurrency workers, each holding a
	// *sql.Conn). Idle == Max keeps connections hot across sections to avoid
	// the auth + session-setup overhead of reopening every section.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	pingCtx, cancel := contextWithTimeout(ctx)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

// OpenPostgres returns a pooled *pgxpool.Pool.
func OpenPostgres(ctx context.Context, p Params) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(p.PostgresDSN())
	if err != nil {
		return nil, fmt.Errorf("parse pg dsn: %w", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg pool: %w", err)
	}
	pingCtx, cancel := contextWithTimeout(ctx)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping pg: %w", err)
	}
	return pool, nil
}

// TestResult is what the wizard shows after clicking "Test connection".
type TestResult struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Message string `json:"message"`
}

// Test probes a connection and returns the server version.
func Test(ctx context.Context, p Params) TestResult {
	switch p.Kind {
	case "mysql", "mariadb":
		db, err := OpenMySQL(ctx, p)
		if err != nil {
			return TestResult{OK: false, Message: err.Error()}
		}
		defer db.Close()
		var v string
		if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
			return TestResult{OK: false, Message: "ping OK but version failed: " + err.Error()}
		}
		return TestResult{OK: true, Version: v, Message: "connected"}
	case "oracle", "oracle19":
		db, err := OpenOracle(ctx, p)
		if err != nil {
			return TestResult{OK: false, Message: err.Error()}
		}
		defer db.Close()
		var v string
		if err := db.QueryRowContext(ctx,
			"SELECT banner_full FROM v$version WHERE rownum=1").Scan(&v); err != nil {
			return TestResult{OK: false, Message: "ping OK but version failed: " + err.Error()}
		}
		return TestResult{OK: true, Version: v, Message: "connected"}
	case "db2":
		db, err := OpenDB2(ctx, p)
		if err != nil {
			return TestResult{OK: false, Message: err.Error()}
		}
		defer db.Close()
		var v string
		// env_get_inst_info() returns "DB2 v11.5.x.0" on LUW. Fallback to
		// SYSIBM.SYSDUMMY1 for restricted users without EXECUTE on the
		// SYSPROC routine.
		if err := db.QueryRowContext(ctx,
			"SELECT service_level FROM TABLE(sysproc.env_get_inst_info())").Scan(&v); err != nil {
			if err2 := db.QueryRowContext(ctx,
				"SELECT GETVARIABLE('SYSIBM.VERSION') FROM SYSIBM.SYSDUMMY1").Scan(&v); err2 != nil {
				return TestResult{OK: false, Message: "ping OK but version failed: " + err.Error()}
			}
		}
		return TestResult{OK: true, Version: v, Message: "connected"}
	case "db2zos":
		db, err := OpenDB2(ctx, p)
		if err != nil {
			return TestResult{OK: false, Message: err.Error()}
		}
		defer db.Close()
		var v string
		if err := db.QueryRowContext(ctx,
			"SELECT GETVARIABLE('SYSIBM.VERSION') FROM SYSIBM.SYSDUMMY1").Scan(&v); err != nil {
			return TestResult{OK: false, Message: "ping OK but version failed: " + err.Error()}
		}
		return TestResult{OK: true, Version: v, Message: "connected"}
	case "postgres":
		pool, err := OpenPostgres(ctx, p)
		if err != nil {
			return TestResult{OK: false, Message: err.Error()}
		}
		defer pool.Close()
		var v string
		if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
			return TestResult{OK: false, Message: "ping OK but server_version failed: " + err.Error()}
		}
		return TestResult{OK: true, Version: v, Message: "connected"}
	default:
		return TestResult{OK: false, Message: "unsupported kind " + p.Kind}
	}
}

func contextWithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	// 10s default probe timeout — short enough to keep UI responsive.
	return contextWithTimeout10s(ctx)
}

// Separated for easy override in tests.
var contextWithTimeout10s = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return contextWithTimeoutN(ctx, 10)
}

// dummy for readability; actual timeout use import time
func contextWithTimeoutN(parent context.Context, seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeoutSeconds(seconds))
}

var _ = pgx.Identifier{} // keep pgx import for helpers
