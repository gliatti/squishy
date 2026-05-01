package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

// MySQL UNSIGNED narrows the value domain to non-negative integers. The
// translator must add a CHECK ≥ 0 so that PG enforces the same constraint
// the original column did. Without it, a row with -1 would silently round-
// trip into the migrated table.
func TestUnsignedIntEmitsNonNegativeCheck(t *testing.T) {
	src := `CREATE TABLE t (
		a TINYINT UNSIGNED NOT NULL,
		b SMALLINT UNSIGNED NOT NULL,
		c MEDIUMINT UNSIGNED NOT NULL,
		d INT UNSIGNED NOT NULL,
		e BIGINT UNSIGNED NOT NULL
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	cols := map[string]PGColumn{}
	for _, c := range res.Plan.Tables[0].Columns {
		cols[c.Name] = c
	}
	require.Equal(t, "SMALLINT", cols["a"].Type)
	require.Contains(t, cols["a"].Check, `"a" >= 0`)
	require.Equal(t, "INTEGER", cols["b"].Type)
	require.Contains(t, cols["b"].Check, `"b" >= 0`)
	require.Equal(t, "INTEGER", cols["c"].Type)
	require.Contains(t, cols["c"].Check, `"c" >= 0`)
	require.Equal(t, "BIGINT", cols["d"].Type)
	require.Contains(t, cols["d"].Check, `"d" >= 0`)
	require.Equal(t, "NUMERIC(20,0)", cols["e"].Type)
	require.Contains(t, cols["e"].Check, `"e" >= 0`)
}

// Signed integer columns retain the existing mapping with no synthetic CHECK.
func TestSignedIntNoCheck(t *testing.T) {
	src := `CREATE TABLE t (a INT NOT NULL, b BIGINT NOT NULL);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	cols := map[string]PGColumn{}
	for _, c := range res.Plan.Tables[0].Columns {
		cols[c.Name] = c
	}
	require.Empty(t, cols["a"].Check, "signed columns must not carry a synthetic CHECK")
	require.Empty(t, cols["b"].Check)
}

// ZEROFILL is purely a display attribute. The translator should drop it
// (PG cannot replicate the formatting) but surface a note so the user is
// aware their column-format expectations no longer hold.
func TestZerofillSurfacesAsNote(t *testing.T) {
	src := `CREATE TABLE t (id INT(6) UNSIGNED ZEROFILL NOT NULL);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Equal(t, "BIGINT", res.Plan.Tables[0].Columns[0].Type)

	found := false
	for _, m := range res.TypeMappings {
		if m.Object == "t.id" && strings.Contains(m.Note, "ZEROFILL") {
			found = true
		}
	}
	require.True(t, found, "expected ZEROFILL note in TypeMappings; got %#v", res.TypeMappings)
}

// PK columns can be UNSIGNED. The CHECK must reach the emitted DDL so the
// post-migration table actually rejects negatives.
func TestUnsignedCheckEmittedToDDL(t *testing.T) {
	src := `CREATE TABLE t (id INT UNSIGNED NOT NULL PRIMARY KEY);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	require.Contains(t, res.DDLScript, `CHECK ("id" >= 0)`,
		"emitted DDL must enforce the UNSIGNED non-negative invariant")
}
