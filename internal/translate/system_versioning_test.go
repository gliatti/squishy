package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// Without the source's probed ROW END sentinel (Options.MariaDBRowEndMax
// unset) a system-versioned table — here in the implicit form, hidden
// row_start / row_end columns — is not emulated: the table migrates as a
// plain table and a blocking prerequisite surfaces the history loss.
// With the sentinel both the implicit and the explicit-column forms are
// emulated — see mariadb_sysver_emulation_test.go and
// mariadb_features_xl_test.go.
func TestTranslateSystemVersionedTableEmitsBlockingPrereq(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		name VARCHAR(50)
	) WITH SYSTEM VERSIONING;`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})

	// The current row must still migrate.
	require.Len(t, res.Plan.Tables, 1)
	tbl := res.Plan.Tables[0]
	require.Nil(t, tbl.SystemVersioning, "implicit system versioning is not emulated")
	for _, c := range tbl.Columns {
		require.NotEqual(t, "row_start", c.Name)
		require.NotEqual(t, "row_end", c.Name)
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
	// A key over the period (`booking WITHOUT OVERLAPS`) needs an EXCLUDE
	// constraint squishy does not generate: that key is what stays
	// blocking. The bare period itself is replicated (columns + CHECK),
	// see TestTranslateApplicationTimePeriodEmitsNamedCheck.
	src := `CREATE TABLE rooms (
		room_id INT,
		valid_from DATE,
		valid_to DATE,
		PERIOD FOR booking (valid_from, valid_to),
		UNIQUE (room_id, booking WITHOUT OVERLAPS)
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

// MariaDB enforces an application-time period through an implicit CHECK
// (start < end) named after the period; the translator replicates it and
// raises no warning for the bare period.
func TestTranslateApplicationTimePeriodEmitsNamedCheck(t *testing.T) {
	src := `CREATE TABLE rooms (
		room_id INT,
		valid_from DATE,
		valid_to DATE,
		PERIOD FOR booking (valid_from, valid_to)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.Empty(t, res.Warnings)
	for _, p := range res.Prerequisites {
		require.NotContains(t, p.Title, "Application-time PERIOD")
	}
	require.Len(t, res.Plan.Tables, 1)
	require.Equal(t, []string{"booking"}, res.Plan.Tables[0].CheckNames)
	require.Contains(t, res.DDLScript, `CONSTRAINT "booking" CHECK ("valid_from" < "valid_to")`)
}

// A `<period> WITHOUT OVERLAPS` key part is not emitted as a plain unique
// index (the period is not a column, and a plain key would not forbid
// overlapping ranges).
func TestTranslateWithoutOverlapsKeyNotEmittedAsPlainIndex(t *testing.T) {
	src := `CREATE TABLE rooms (
		room_id INT,
		valid_from DATE,
		valid_to DATE,
		PERIOD FOR booking (valid_from, valid_to),
		UNIQUE KEY uq_room (room_id, booking WITHOUT OVERLAPS)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig"})
	for _, idx := range res.Plan.Indexes {
		require.NotEqual(t, "uq_room", idx.Name)
	}
	require.Len(t, res.Warnings, 1)
	require.Equal(t, "table.application_period", res.Warnings[0].Kind)
	require.Contains(t, res.Warnings[0].Message, "booking WITHOUT OVERLAPS")
}
