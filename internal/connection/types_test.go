package connection

import (
	"net/url"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/microsoft/go-mssqldb/msdsn"
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

// The SQL Server DSN must survive credentials carrying URL metacharacters
// (SA passwords need symbols), map SSLMode onto the driver's TLS knobs and
// let Extra override that mapping. Parsed back with net/url so the test
// checks what go-mssqldb will actually read, not the encoded spelling.
func TestSQLServerDSN(t *testing.T) {
	cases := []struct {
		name  string
		p     Params
		query map[string]string
	}{
		{
			name:  "disable",
			p:     Params{SSLMode: "disable"},
			query: map[string]string{"database": "Northwind", "encrypt": "disable"},
		},
		{
			name:  "require",
			p:     Params{SSLMode: "require"},
			query: map[string]string{"database": "Northwind", "encrypt": "true"},
		},
		{
			name:  "default trusts the server certificate",
			p:     Params{},
			query: map[string]string{"database": "Northwind", "TrustServerCertificate": "true"},
		},
		{
			name:  "extra wins over the ssl mapping",
			p:     Params{SSLMode: "disable", Extra: map[string]string{"encrypt": "strict", "app name": "squishy"}},
			query: map[string]string{"database": "Northwind", "encrypt": "strict", "app name": "squishy"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			p.Host, p.Port, p.Database = "sqlserver-sample", 1433, "Northwind"
			p.Username, p.Password = "sa", "Squ!shy@2026#x%/?"
			u, err := url.Parse(p.SQLServerDSN())
			require.NoError(t, err)
			require.Equal(t, "sqlserver", u.Scheme)
			require.Equal(t, "sqlserver-sample:1433", u.Host)
			require.Equal(t, "sa", u.User.Username())
			pw, ok := u.User.Password()
			require.True(t, ok)
			require.Equal(t, "Squ!shy@2026#x%/?", pw)
			got := map[string]string{}
			for k, v := range u.Query() {
				got[k] = v[0]
			}
			require.Equal(t, tc.query, got)

			// And the driver itself reads the same connection.
			cfg, err := msdsn.Parse(p.SQLServerDSN())
			require.NoError(t, err)
			require.Equal(t, "sqlserver-sample", cfg.Host)
			require.Equal(t, uint64(1433), cfg.Port)
			require.Equal(t, "Northwind", cfg.Database)
			require.Equal(t, "sa", cfg.User)
			require.Equal(t, "Squ!shy@2026#x%/?", cfg.Password)
		})
	}
}
