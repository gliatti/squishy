package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestFulltextSingleColumnEmitsGINExpressionIndex(t *testing.T) {
	src := `CREATE TABLE articles (
		id INT PRIMARY KEY,
		body TEXT NOT NULL,
		FULLTEXT KEY ix_body (body)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	post := strings.Join(res.Plan.PostActions, "\n")
	require.Contains(t, post,
		`CREATE INDEX "ix_body" ON "mig"."articles" USING GIN (to_tsvector('simple', coalesce("body", '')));`,
		"FULLTEXT index must materialise as GIN(to_tsvector(...)) — got %q", post)
}

func TestFulltextMultipleColumnsConcatenated(t *testing.T) {
	src := `CREATE TABLE articles (
		id INT PRIMARY KEY,
		title VARCHAR(200),
		body TEXT,
		FULLTEXT KEY ix_search (title, body)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	post := strings.Join(res.Plan.PostActions, "\n")
	require.Contains(t, post,
		`coalesce("title", '') || ' ' || coalesce("body", '')`,
		"multi-column FULLTEXT must concatenate with NULL-safe coalesce")
	require.Contains(t, post, `USING GIN (to_tsvector(`)
}

// FULLTEXT must not produce a blocking warning anymore — it's an info note
// because the GIN expression index is a real working equivalent.
func TestFulltextNoLongerWarnsBlocking(t *testing.T) {
	src := `CREATE TABLE t (body TEXT, FULLTEXT KEY ix_b (body));`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "mysql"})
	for _, w := range res.Warnings {
		require.NotEqual(t, "index.fulltext", w.Kind, "FULLTEXT should be info-only, not a warning")
	}
	// Explanation should surface the mapping decision.
	found := false
	for _, e := range res.Explanations {
		if strings.Contains(e.Reason, "FULLTEXT mapped to a PG GIN index") {
			found = true
		}
	}
	require.True(t, found, "expected an info Explanation noting the GIN/to_tsvector mapping")
}
