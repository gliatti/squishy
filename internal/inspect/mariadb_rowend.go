package inspect

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// isMariaDBVersion reports whether a `SELECT VERSION()` banner comes from
// MariaDB (e.g. "11.8.8-MariaDB-ubu2404"). The banner is a server
// version string, not SQL.
func isMariaDBVersion(v string) bool {
	return strings.Contains(strings.ToLower(v), "mariadb")
}

// mariadbProbeRowEndMax returns the ROW END value a MariaDB server stores
// on current rows of a timestamp-based system-versioned table, as UTC
// wall-clock text. That value is the server's TIMESTAMP maximum, which
// depends on the server (MariaDB 11.5+ on 64-bit platforms extended the
// range to 2106-02-07 06:28:15.999999 UTC; older servers stop at
// 2038-01-19 03:14:07.999999 UTC), so it is probed rather than assumed:
// FROM_UNIXTIME follows the same range and returns NULL past it.
//
// The probe runs on a dedicated connection pinned to UTC so the result
// does not depend on the server's or the pool's session time_zone.
func mariadbProbeRowEndMax(ctx context.Context, db *sql.DB) (string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		return "", err
	}
	var wide, narrow sql.NullTime
	if err := conn.QueryRowContext(ctx,
		"SELECT FROM_UNIXTIME(4294967295.999999), FROM_UNIXTIME(2147483647.999999)").
		Scan(&wide, &narrow); err != nil {
		return "", err
	}
	return rowEndMaxText(wide, narrow)
}

// rowEndMaxText picks the widest TIMESTAMP maximum the server returned
// and formats it as "YYYY-MM-DD hh:mm:ss.ffffff" (UTC).
func rowEndMaxText(wide, narrow sql.NullTime) (string, error) {
	var t time.Time
	switch {
	case wide.Valid:
		t = wide.Time
	case narrow.Valid:
		t = narrow.Time
	default:
		return "", fmt.Errorf("server returned no TIMESTAMP maximum")
	}
	return t.UTC().Format("2006-01-02 15:04:05.000000"), nil
}
