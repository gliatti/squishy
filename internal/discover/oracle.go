package discover

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const (
	oracleDBAQuery = `SELECT username FROM dba_users WHERE oracle_maintained = 'N' AND account_status = 'OPEN' ORDER BY username`
	oracleAllQuery = `SELECT username FROM all_users WHERE account_status = 'OPEN' OR account_status IS NULL`
)

func discoverOracle(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, oracleDBAQuery)
	if err != nil {
		if isOraTableMissing(err) {
			return discoverOracleFallback(ctx, db)
		}
		return nil, fmt.Errorf("discover oracle: dba_users: %w", err)
	}
	defer rows.Close()

	out, err := scanStrings(rows)
	if err != nil {
		return nil, fmt.Errorf("discover oracle: dba_users scan: %w", err)
	}
	// Apply the in-Go blacklist as a safety net: some maintenance accounts
	// (e.g. PDBADMIN in 23ai) are not flagged oracle_maintained='Y' but are
	// still not migration material.
	return filterOracle(out), nil
}

func discoverOracleFallback(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, oracleAllQuery)
	if err != nil {
		return nil, fmt.Errorf("discover oracle: all_users: %w", err)
	}
	defer rows.Close()

	raw, err := scanStrings(rows)
	if err != nil {
		return nil, fmt.Errorf("discover oracle: all_users scan: %w", err)
	}
	return filterOracle(raw), nil
}

// filterOracle drops schemas in the in-Go blacklist and any obvious Oracle
// internal accounts (APEX_*, ORDS_*, FLOWS_*, etc.).
func filterOracle(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if oracleSystemSchemas[s] {
			continue
		}
		if hasInternalPrefix(s) {
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func hasInternalPrefix(s string) bool {
	for _, p := range []string{"APEX_", "ORDS_", "FLOWS_"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func isOraTableMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ORA-00942")
}
