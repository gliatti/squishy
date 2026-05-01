package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateSystemVersionedTableEmitsBlockingPrereq(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		name VARCHAR(50),
		valid_from TIMESTAMP(6) GENERATED ALWAYS AS ROW START,
		valid_to TIMESTAMP(6) GENERATED ALWAYS AS ROW END,
		PERIOD FOR SYSTEM_TIME (valid_from, valid_to)
	) WITH SYSTEM VERSIONING;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})

	// The current row must still migrate.
	require.Len(t, res.Plan.Tables, 1)
	tbl := res.Plan.Tables[0]
	// ROW START/END columns are dropped.
	for _, c := range tbl.Columns {
		require.NotEqual(t, "valid_from", c.Name)
		require.NotEqual(t, "valid_to", c.Name)
	}

	// A blocking prerequisite must surface system-versioning loss.
	var found bool
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && strings.Contains(p.Title, "SYSTEM VERSIONING") {
			found = true
			require.Contains(t, p.Object, "t")
			require.Contains(t, p.Remediation, "temporal_tables")
			break
		}
	}
	require.True(t, found, "expected a blocking SYSTEM VERSIONING prerequisite")
}

func TestTranslateApplicationTimePeriodEmitsBlockingPrereq(t *testing.T) {
	src := `CREATE TABLE rooms (
		room_id INT,
		valid_from DATE,
		valid_to DATE,
		PERIOD FOR booking (valid_from, valid_to)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})

	require.Len(t, res.Plan.Tables, 1)
	// Application-time period must NOT mark the table as system-versioned,
	// nor drop its columns.
	tbl := res.Plan.Tables[0]
	var sawStart, sawEnd bool
	for _, c := range tbl.Columns {
		if c.Name == "valid_from" {
			sawStart = true
		}
		if c.Name == "valid_to" {
			sawEnd = true
		}
	}
	require.True(t, sawStart && sawEnd, "application-time period columns must NOT be dropped")

	var found bool
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && strings.Contains(p.Title, "Application-time PERIOD") {
			found = true
			require.Contains(t, p.Remediation, "EXCLUDE")
			break
		}
	}
	require.True(t, found, "expected a blocking application-time PERIOD prerequisite")
}

func TestTranslateNonVersionedTableHasNoTemporalPrereq(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(50));`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	for _, p := range res.Prerequisites {
		require.NotContains(t, p.Title, "SYSTEM VERSIONING")
		require.NotContains(t, p.Title, "Application-time PERIOD")
	}
}
