package discover

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// discoverSQLServer lists the user databases of a SQL Server instance.
// The discovery unit is the database (not the T-SQL schema): a migration
// takes one database and keeps its schemas (dbo, Sales, …) apart on the
// PG side. database_id 1..4 are master/tempdb/model/msdb; state 0 is
// ONLINE (an OFFLINE / RESTORING / SUSPECT database cannot be inspected).
// The in-Go blacklist then drops the shipped service databases (SSRS,
// replication, SSIS, PolyBase) that also live above id 4.
func discoverSQLServer(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sys.databases WHERE database_id > 4 AND state = 0`)
	if err != nil {
		return nil, fmt.Errorf("discover sqlserver: %w", err)
	}
	defer rows.Close()

	raw, err := scanStrings(rows)
	if err != nil {
		return nil, fmt.Errorf("discover sqlserver: scan: %w", err)
	}
	return filterSQLServer(raw), nil
}

// filterSQLServer drops the system/service databases. Database names
// compare case-insensitively: the instance collation is CI by default,
// so `ReportServer` and `reportserver` are the same database. The fold
// applies to one database name at a time, never to SQL text.
func filterSQLServer(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if sqlserverSystemDatabases[strings.ToLower(s)] {
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
