package discover

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

func discoverMySQL(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("discover mysql: %w", err)
	}
	defer rows.Close()

	raw, err := scanStrings(rows)
	if err != nil {
		return nil, fmt.Errorf("discover mysql: scan: %w", err)
	}
	return filterMySQL(raw), nil
}

func filterMySQL(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if mysqlSystemDatabases[s] {
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// scanStrings reads a single-column string result set into a slice.
func scanStrings(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
