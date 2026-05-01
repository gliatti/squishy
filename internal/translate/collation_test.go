package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// `*_bin` collations enforce byte-wise ordering — PG's `"C"` is the
// equivalent and is always present.
func TestColumnCollationBinMapsToCCollation(t *testing.T) {
	src := `CREATE TABLE t (
		hashval VARCHAR(64) COLLATE utf8mb4_bin NOT NULL
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Equal(t, `"C"`, res.Plan.Tables[0].Columns[0].Collation)
	require.Contains(t, res.DDLScript, `COLLATE "C"`)
}

// Case-insensitive collations have no built-in PG equivalent. Surface as
// an info note pointing to CITEXT, but don't emit a COLLATE clause that
// would fail at DDL time.
func TestColumnCollationCaseInsensitiveSurfacesNote(t *testing.T) {
	src := `CREATE TABLE t (
		name VARCHAR(64) COLLATE utf8mb4_unicode_ci NOT NULL
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Empty(t, res.Plan.Tables[0].Columns[0].Collation,
		"case-insensitive collation must not emit a COLLATE clause that PG would reject")

	found := false
	for _, e := range res.Explanations {
		if strings.Contains(e.Reason, "case-insensitive") && strings.Contains(e.Reason, "CITEXT") {
			found = true
		}
	}
	require.True(t, found, "expected CITEXT-suggesting explanation; got %#v", res.Explanations)
}

// Per-column CHARSET (no explicit collation) surfaces as an info note. PG
// has no per-column charset — there's nothing to emit beyond the
// explanation.
func TestColumnCharsetOnlySurfacesNote(t *testing.T) {
	src := `CREATE TABLE t (
		body TEXT CHARACTER SET utf8mb4 NOT NULL
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})

	found := false
	for _, e := range res.Explanations {
		if strings.Contains(e.Reason, "PG charsets are per-database") {
			found = true
		}
	}
	require.True(t, found)
}

// Unknown collations preserved literally so DDL fails loudly if the user
// hasn't installed the matching PG collation.
func TestColumnCollationUnknownPreserved(t *testing.T) {
	src := `CREATE TABLE t (
		name VARCHAR(64) COLLATE custom_locale NOT NULL
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Equal(t, `"custom_locale"`, res.Plan.Tables[0].Columns[0].Collation)
}
