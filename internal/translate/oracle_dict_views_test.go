package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Bare references to ALL_OBJECTS / ALL_TABLES must inline a self-contained
// pg_catalog sub-SELECT — orafce ships only the user_* family and the
// target should not depend on any compat view being provisioned. The
// substitution must keep the surrounding query syntactically valid in the
// two common Oracle shapes:
//
//   - aliased: `FROM ALL_TABLES A ON A.OWNER = …`     → `FROM (sub) A ON …`
//   - bare:    `FROM ALL_OBJECTS WHERE owner = '…'`   → `FROM (sub) AS all_objects WHERE …`
//
// The expanded SELECT must reference pg_catalog.pg_class and (for
// ALL_OBJECTS) pg_catalog.pg_proc so callers see procedures as well as
// relations.
func TestRewriteOracleDictionaryViews(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustHave []string
		mustMiss []string
	}{
		{
			name: "ALL_OBJECTS bare gets AS alias",
			in:   "SELECT count(*) FROM ALL_OBJECTS WHERE owner = 'X' AND object_name = 'PKG_ITV';",
			mustHave: []string{
				"FROM pg_catalog.pg_class",
				"FROM pg_catalog.pg_proc",
				") AS all_objects",
				"WHERE owner = 'X'",
			},
			mustMiss: []string{"FROM ALL_OBJECTS"},
		},
		{
			name: "ALL_TABLES with user alias keeps the user's alias",
			in:   "SELECT * FROM ALL_TABLES A WHERE A.OWNER = 'X';",
			mustHave: []string{
				"FROM pg_catalog.pg_class",
				") A WHERE",
			},
			mustMiss: []string{
				"FROM ALL_TABLES",
				"AS all_tables",
			},
		},
		{
			name: "schema-qualified reference left alone",
			in:   "SELECT * FROM mig.all_objects WHERE owner = 'X';",
			mustHave: []string{
				"FROM mig.all_objects WHERE owner = 'X'",
			},
			mustMiss: []string{"pg_catalog"},
		},
		{
			name: "ALL_OBJECTS in JOIN with user alias",
			in:   "SELECT * FROM tab_trc W JOIN ALL_OBJECTS B ON B.object_name = W.tab_name;",
			mustHave: []string{
				"JOIN (",
				") B ON B.object_name = W.tab_name",
			},
			mustMiss: []string{"AS all_objects"},
		},
		{
			name: "ALL_TAB_COLUMNS expanded with type mapping",
			in:   "SELECT count(*) FROM ALL_TAB_COLUMNS WHERE DATA_TYPE in ('BLOB','SDO_GEOMETRY');",
			mustHave: []string{
				"pg_catalog.pg_attribute",
				"WHEN t.typname = 'bytea' THEN 'BLOB'",
				"WHEN t.typname = 'geometry' THEN 'SDO_GEOMETRY'",
				") AS all_tab_columns",
			},
			mustMiss: []string{"FROM ALL_TAB_COLUMNS"},
		},
		{
			name: "ALL_INDEXES + ALL_IND_COLUMNS join",
			in:   "FROM ALL_INDEXES I JOIN ALL_IND_COLUMNS C ON I.INDEX_NAME = C.INDEX_NAME",
			mustHave: []string{
				"FROM pg_catalog.pg_index",
				") I JOIN (",
				") C ON I.INDEX_NAME = C.INDEX_NAME",
			},
			mustMiss: []string{"ALL_INDEXES I", "ALL_IND_COLUMNS C"},
		},
		{
			name: "ALL_TAB_COLUMNS ordered before ALL_TABLES (no overlap)",
			in:   "SELECT * FROM ALL_TAB_COLUMNS;",
			mustHave: []string{
				") AS all_tab_columns",
				"pg_catalog.pg_attribute",
			},
			// must NOT have been turned into ALL_TABLES + bare _COLUMNS
			mustMiss: []string{
				"AS all_tables",
			},
		},
		{
			// Idempotency: applying the rewrite to its own output must
			// be a no-op. The first pass emits `(sub) AS all_tab_columns`;
			// without an alias-position guard, a second pass would
			// re-expand the trailing alias and produce a malformed
			// `(sub1) AS (sub2) AS all_tab_columns`.
			name:     "rewrite is idempotent on already-expanded text",
			in:       rewriteOracleDictionaryViews("SELECT * FROM ALL_TAB_COLUMNS WHERE x = 1;"),
			mustHave: []string{") AS all_tab_columns WHERE x = 1"},
			mustMiss: []string{") AS (SELECT", "AS all_tab_columns WHERE x = 1) AS"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteOracleDictionaryViews(tc.in)
			for _, want := range tc.mustHave {
				require.True(t, strings.Contains(got, want),
					"output missing %q\noutput:\n%s", want, got)
			}
			for _, miss := range tc.mustMiss {
				require.False(t, strings.Contains(got, miss),
					"output unexpectedly contains %q\noutput:\n%s", miss, got)
			}
		})
	}
}

// nextLooksLikeAlias is the heuristic that drives whether expandDictView
// emits an `AS <view>` or relies on the user's alias.
func TestNextLooksLikeAlias(t *testing.T) {
	cases := []struct {
		name string
		src  string
		at   int
		want bool
	}{
		{name: "user alias", src: "ALL_TABLES A WHERE", at: len("ALL_TABLES"), want: true},
		{name: "WHERE clause", src: "ALL_OBJECTS WHERE x=1", at: len("ALL_OBJECTS"), want: false},
		{name: "ON clause", src: "ALL_TABLES B ON B.x=1", at: len("ALL_TABLES"), want: true},
		{name: "ON immediately", src: "ALL_TABLES ON x=1", at: len("ALL_TABLES"), want: false},
		{name: "JOIN immediately", src: "ALL_OBJECTS JOIN y", at: len("ALL_OBJECTS"), want: false},
		{name: "comma", src: "ALL_OBJECTS, b", at: len("ALL_OBJECTS"), want: false},
		{name: "close paren", src: "ALL_OBJECTS)", at: len("ALL_OBJECTS"), want: false},
		{name: "end of input", src: "ALL_OBJECTS", at: len("ALL_OBJECTS"), want: false},
		{name: "GROUP BY", src: "ALL_OBJECTS GROUP BY x", at: len("ALL_OBJECTS"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextLooksLikeAlias(tc.src, tc.at)
			require.Equal(t, tc.want, got)
		})
	}
}
