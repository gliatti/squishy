package discover

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	// LUW: SYSCAT.SCHEMATA exposes user + system schemas in one view; we
	// drop the catalogue / monitoring / SQLJ / NULLID schemas here and let
	// filterDB2() apply the in-Go blacklist as a safety net.
	db2LUWQuery = `
		SELECT schemaname FROM SYSCAT.SCHEMATA
		 WHERE schemaname NOT LIKE 'SYS%'
		 ORDER BY schemaname`

	// z/OS: SYSIBM.SYSSCHEMATA names schemas without owner-status flags.
	// SYS* prefix filtering is the strongest signal there.
	db2zOSQuery = `
		SELECT NAME FROM SYSIBM.SYSSCHEMATA
		 WHERE NAME NOT LIKE 'SYS%'
		 ORDER BY NAME`
)

// discoverDB2LUW lists user schemas on a LUW source.
func discoverDB2LUW(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, db2LUWQuery)
	if err != nil {
		return nil, fmt.Errorf("discover db2: SYSCAT.SCHEMATA: %w", err)
	}
	defer rows.Close()
	out, err := scanStrings(rows)
	if err != nil {
		return nil, fmt.Errorf("discover db2: SYSCAT scan: %w", err)
	}
	return filterDB2(out), nil
}

// discoverDB2zOS lists user schemas on a DB2 for z/OS source.
func discoverDB2zOS(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, db2zOSQuery)
	if err != nil {
		return nil, fmt.Errorf("discover db2zos: SYSIBM.SYSSCHEMATA: %w", err)
	}
	defer rows.Close()
	out, err := scanStrings(rows)
	if err != nil {
		return nil, fmt.Errorf("discover db2zos: SYSIBM scan: %w", err)
	}
	return filterDB2(out), nil
}

// filterDB2 drops schemas in db2SystemSchemas and any obviously-internal
// IBM/operational accounts.
func filterDB2(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		u := strings.ToUpper(strings.TrimSpace(s))
		if u == "" {
			continue
		}
		if db2SystemSchemas[u] {
			continue
		}
		if hasInternalPrefix(u) {
			continue
		}
		// SYSCAT.SCHEMATA returns CHAR(128) values padded with trailing
		// spaces; emit the trimmed form so target_db_name etc. don't carry
		// whitespace into PG.
		out = append(out, strings.TrimSpace(s))
	}
	return out
}
