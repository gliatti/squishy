package translate

import (
	"fmt"
	"strings"
)

// buildFulltextIndexDDL renders a PG GIN expression index over a
// `to_tsvector('simple', col1 || ' ' || col2 || …)` aggregate, NULL-safe via
// COALESCE. This is the closest workable equivalent to MySQL's FULLTEXT
// indexes: queries rewrite to `tsv @@ plainto_tsquery(...)` instead of
// `MATCH() AGAINST()`.
//
// We deliberately use the `simple` configuration (no stemming, no stopwords)
// because the source dump never tells us the column language, and choosing
// `english` silently for a French corpus would silently degrade matches.
// Users who need stemming swap the config in the emitted DDL.
func buildFulltextIndexDDL(schema, table, indexName string, cols []string) string {
	if len(cols) == 0 {
		return "-- skipped: FULLTEXT index " + indexName + " has no columns"
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("coalesce(%s, '')", quoteIdent(c))
	}
	expr := strings.Join(parts, " || ' ' || ")
	return fmt.Sprintf(
		"CREATE INDEX %s ON %s.%s USING GIN (to_tsvector('simple', %s));",
		quoteIdent(indexName),
		quoteIdent(schema), quoteIdent(table),
		expr,
	)
}
