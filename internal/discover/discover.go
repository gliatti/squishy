// Package discover lists non-system schemas/databases on a source DB. The
// caller opens the *sql.DB with the appropriate driver (see internal/connection)
// and passes it in along with the dialect kind.
package discover

import (
	"context"
	"database/sql"
	"errors"
)

// Schemas returns the list of non-system source schemas (Oracle, DB2) or
// databases (MySQL/MariaDB) reachable through the open *sql.DB.
func Schemas(ctx context.Context, db *sql.DB, kind string) ([]string, error) {
	switch kind {
	case "mysql", "mariadb":
		return discoverMySQL(ctx, db)
	case "oracle", "oracle19":
		return discoverOracle(ctx, db)
	case "db2":
		return discoverDB2LUW(ctx, db)
	case "db2zos":
		return discoverDB2zOS(ctx, db)
	default:
		return nil, errors.New("discover: unsupported kind " + kind)
	}
}
