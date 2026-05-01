package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// With PostGIS available on the target, MySQL spatial columns map to
// `geometry(<subtype>)` and SPATIAL indexes lift to a GIST index. The
// downstream migration runs unmodified.
func TestSpatialColumnAndIndexWithPostGIS(t *testing.T) {
	src := `CREATE TABLE places (
		id INT PRIMARY KEY,
		loc POINT NOT NULL,
		shape GEOMETRY,
		SPATIAL KEY ix_loc (loc)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{
		TargetSchema:     "mig",
		SourceKind:       "mysql",
		TargetExtensions: []string{"postgis"},
	})
	cols := map[string]PGColumn{}
	for _, c := range res.Plan.Tables[0].Columns {
		cols[c.Name] = c
	}
	require.Equal(t, "geometry(POINT)", cols["loc"].Type)
	require.Equal(t, "geometry", cols["shape"].Type)

	require.Len(t, res.Plan.Indexes, 1)
	require.Equal(t, "gist", res.Plan.Indexes[0].Using)
	require.Equal(t, []string{"loc"}, res.Plan.Indexes[0].Columns)
}

// Without PostGIS, the column degrades to TEXT and a blocking
// Install-PostGIS prerequisite is queued. The SPATIAL index is dropped to
// avoid a broken GIST(text) statement at run time.
func TestSpatialColumnAndIndexWithoutPostGISDegrades(t *testing.T) {
	src := `CREATE TABLE places (
		id INT PRIMARY KEY,
		loc POINT NOT NULL,
		SPATIAL KEY ix_loc (loc)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	cols := res.Plan.Tables[0].Columns
	require.Equal(t, "TEXT", cols[1].Type, "POINT degrades to TEXT placeholder when PostGIS missing")

	// SPATIAL index must NOT be enqueued — PG would fail with `data type
	// text has no default operator class for access method "gist"`.
	for _, ix := range res.Plan.Indexes {
		require.NotContains(t, ix.Name, "ix_loc",
			"SPATIAL index without PostGIS must be deferred, not emitted")
	}

	// Prereq: PostGIS install must surface as a blocking item. The bullet
	// title is normalised across spatial-column and spatial-index sources
	// in prereqs.go, so we look for the title prefix.
	found := false
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "PostGIS") {
			found = true
			require.Equal(t, SeverityBlocking, p.Severity)
		}
	}
	require.True(t, found, "expected blocking PostGIS prerequisite; got %#v", res.Prerequisites)
}
