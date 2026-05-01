package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/mysql"
)

func TestTranslateVectorColumnAlwaysEmitsPgvectorType(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, e VECTOR(384));`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)

	// pgvector NOT installed: column type must STILL be vector(384) so the
	// generated DDL fails loudly until the user installs pgvector.
	res := Translate(stmts, Options{TargetSchema: "mig"})
	require.Len(t, res.Plan.Tables, 1)
	var found bool
	for _, c := range res.Plan.Tables[0].Columns {
		if c.Name == "e" {
			require.Equal(t, "vector(384)", c.Type)
			found = true
		}
	}
	require.True(t, found)

	// A blocking install_extension prerequisite for pgvector must surface.
	var prereqFound bool
	for _, p := range res.Prerequisites {
		if p.Severity == SeverityBlocking && strings.Contains(p.Title, "pgvector") {
			prereqFound = true
		}
	}
	require.True(t, prereqFound, "expected blocking pgvector install prerequisite")
}

func TestTranslateVectorColumnWithPgvectorInstalledNoBlockingPrereq(t *testing.T) {
	src := `CREATE TABLE t (id INT PRIMARY KEY, e VECTOR(3));`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", TargetExtensions: []string{"vector"}})
	require.Len(t, res.Plan.Tables, 1)
	require.Equal(t, "vector(3)", res.Plan.Tables[0].Columns[1].Type)
	for _, p := range res.Prerequisites {
		require.NotContains(t, p.Title, "pgvector")
	}
}

func TestTranslateVectorIndexMapsToHnswCosine(t *testing.T) {
	src := `CREATE TABLE t (
		id INT PRIMARY KEY,
		e VECTOR(3) NOT NULL,
		VECTOR INDEX v_idx (e)
	);`
	stmts, errs := mysql.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", TargetExtensions: []string{"vector"}})
	require.Len(t, res.Plan.Indexes, 1)
	idx := res.Plan.Indexes[0]
	require.Equal(t, "hnsw", idx.Using)
	require.Equal(t, "v_idx", idx.Name)
	require.Len(t, idx.Columns, 1)
	require.Contains(t, idx.Columns[0], "vector_cosine_ops")
	require.True(t, idx.ColumnIsExpr[0], "operator class must be emitted as an expression key")
}
