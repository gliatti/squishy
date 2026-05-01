package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// External tables: ORGANIZATION EXTERNAL (TYPE … DEFAULT DIRECTORY …
// ACCESS PARAMETERS (...) LOCATION (...)) must (a) parse cleanly past
// the trailing parens, (b) emit a blocking manual-review prereq pointing
// at file_fdw / similar, and (c) not break the surrounding statement.
func TestOracleExternalTableSurfacesFDWPrereq(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."LOAD_RAW" (
		  "ID" NUMBER,
		  "PAYLOAD" VARCHAR2(200)
		)
		ORGANIZATION EXTERNAL (
		  TYPE ORACLE_LOADER
		  DEFAULT DIRECTORY "TMP_DIR"
		  ACCESS PARAMETERS (
		    RECORDS DELIMITED BY NEWLINE FIELDS TERMINATED BY ',' (id, payload)
		  )
		  LOCATION ('feed.csv')
		);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Tables, 1, "table must still appear in the plan")

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "external table") && strings.Contains(p.Title, "load_raw") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
			require.Contains(t, p.Remediation, "file_fdw")
		}
	}
	require.True(t, sawPrereq)
}

// Index-organized tables: ORGANIZATION INDEX with PCTTHRESHOLD/INCLUDING
// must parse cleanly and surface as an info-level explanation pointing at
// CLUSTER + INCLUDE as the closest PG approximation.
func TestOracleIOTSurfacesClusterHint(t *testing.T) {
	src := `
		CREATE TABLE "MIG"."IDX_HOT" (
		  "ID" NUMBER NOT NULL,
		  "PAYLOAD" VARCHAR2(200),
		  CONSTRAINT "PK_IDX_HOT" PRIMARY KEY ("ID")
		)
		ORGANIZATION INDEX (PCTTHRESHOLD 50 INCLUDING "PAYLOAD" OVERFLOW);`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Tables, 1)

	var sawExpl bool
	for _, e := range res.Explanations {
		if e.Object == "table.idx_hot" && strings.Contains(e.Source, "ORGANIZATION INDEX") {
			sawExpl = true
			require.Contains(t, e.Reason, "CLUSTER")
		}
	}
	require.True(t, sawExpl)
}
