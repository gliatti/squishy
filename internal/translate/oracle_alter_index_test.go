package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// `ALTER INDEX … RENAME TO …` lifts directly to PG's identical statement.
func TestOracleAlterIndexRename(t *testing.T) {
	src := `ALTER INDEX "MIG"."IX_OLD" RENAME TO "IX_NEW";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	var found string
	for _, s := range res.Plan.PreActions {
		if strings.Contains(s, "ALTER INDEX") {
			found = s
		}
	}
	require.NotEmpty(t, found, "expected an ALTER INDEX … RENAME TO pre-action")
	require.Contains(t, found, `"mig"."ix_old"`)
	require.Contains(t, found, `"ix_new"`)
}

// `ALTER INDEX … REBUILD` has no PG counterpart — dropped, info explanation.
func TestOracleAlterIndexRebuildDropped(t *testing.T) {
	src := `ALTER INDEX "MIG"."IX_FOO" REBUILD ONLINE TABLESPACE "USERS";`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "REBUILD",
			"REBUILD is a maintenance op and must not be emitted as DDL")
		require.NotContains(t, s, "ALTER INDEX",
			"ALTER INDEX … REBUILD has no PG counterpart and must be dropped")
	}

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "index.ix_foo" && strings.Contains(e.Source, "REBUILD") {
			sawExpl = true
			require.Contains(t, e.Reason, "REINDEX")
		}
	}
	require.True(t, sawExpl, "expected an info-level explanation pointing to REINDEX")
}

// VISIBLE/INVISIBLE — Oracle planner-visibility hint, no PG counterpart.
func TestOracleAlterIndexVisibilityDropped(t *testing.T) {
	for _, kind := range []string{"VISIBLE", "INVISIBLE"} {
		t.Run(kind, func(t *testing.T) {
			src := `ALTER INDEX "MIG"."IX_FOO" ` + kind + `;`
			stmts, errs := oracle.Parse(src)
			require.Empty(t, errs)
			res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

			for _, s := range res.Plan.PreActions {
				require.NotContains(t, s, kind)
			}
			var sawExpl bool
			for _, e := range res.Explanations {
				if strings.Contains(e.Source, kind) {
					sawExpl = true
				}
			}
			require.True(t, sawExpl, "expected explanation for "+kind)
		})
	}
}

// Partition-level DDL on a partitioned index — dropped wholesale.
func TestOracleAlterIndexPartitionDropped(t *testing.T) {
	src := `ALTER INDEX "MIG"."IX_PART" MODIFY PARTITION "P1" UNUSABLE;`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	for _, s := range res.Plan.PreActions {
		require.NotContains(t, s, "ALTER INDEX")
	}
	var sawExpl bool
	for _, e := range res.Explanations {
		if strings.Contains(e.Source, "MODIFY") || strings.Contains(e.Source, "PARTITION") {
			sawExpl = true
			require.Contains(t, e.Reason, "partition")
		}
	}
	require.True(t, sawExpl, "expected partition-level explanation")
}
