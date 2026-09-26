package connection

import (
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Reviewer robustness point: the MySQL/MariaDB session was never pinned
// to UTC, so on a source whose time_zone is not UTC the copier read
// shifted TIMESTAMP values (a system-versioned ROW END of 07:28:15
// instead of 06:28:15 under +01:00, no longer equal to the emulation's
// sentinel). The DSN now forces `time_zone='+00:00'` and loc=UTC, even
// over a connection Extra override.
func TestMySQLDSNPinsSessionToUTC(t *testing.T) {
	p := Params{Host: "h", Port: 3306, Database: "db", Username: "u", Password: "p",
		Extra: map[string]string{"time_zone": "'Europe/Paris'", "loc": "Local"}}
	cfg, err := mysql.ParseDSN(p.MySQLDSN())
	require.NoError(t, err)
	require.Equal(t, "'+00:00'", cfg.Params["time_zone"])
	require.Equal(t, time.UTC, cfg.Loc)
	require.True(t, cfg.ParseTime)
}
