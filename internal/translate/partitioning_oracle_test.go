package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// stripOracleAdvancedPartitioning is the unit-level entry point for the
// pre-processing pass; assert it strips each advanced clause cleanly so
// the simple regex parser downstream sees a normalised body.
func TestStripOracleAdvancedPartitioning_Interval(t *testing.T) {
	body := `("TIME_ID")
		INTERVAL (NUMTOYMINTERVAL(1, 'MONTH'))
		(PARTITION "P1" VALUES LESS THAN (TO_DATE(' 2020-01-01', 'YYYY-MM-DD')))`
	// Mimic what parseOraclePartitioning hands us: header already consumed,
	// so feed only the post-header body.
	post := strings.TrimSpace(`INTERVAL (NUMTOYMINTERVAL(1, 'MONTH'))
		(PARTITION "P1" VALUES LESS THAN (TO_DATE(' 2020-01-01', 'YYYY-MM-DD')))`)
	stripped, notes := stripOracleAdvancedPartitioning(post, "RANGE", oraclePartNotes{})
	require.True(t, notes.HasInterval, "INTERVAL clause should be detected")
	require.Equal(t, "NUMTOYMINTERVAL(1, 'MONTH')", notes.IntervalExpr)
	require.NotContains(t, strings.ToUpper(stripped), "INTERVAL", "INTERVAL keyword must be removed from body")
	_ = body
}

// AUTOMATIC LIST is stripped without affecting the partition definitions
// that follow.
func TestStripOracleAdvancedPartitioning_AutomaticList(t *testing.T) {
	body := `AUTOMATIC (PARTITION "P_EU" VALUES ('FR','DE','IT'))`
	stripped, notes := stripOracleAdvancedPartitioning(body, "LIST", oraclePartNotes{})
	require.True(t, notes.HasAutomaticList)
	require.NotContains(t, strings.ToUpper(stripped), "AUTOMATIC")
	require.Contains(t, stripped, "P_EU")
}

// SUBPARTITION BY ... is stripped along with its template/SUBPARTITIONS N
// trailer; the top-level partition list is preserved.
func TestStripOracleAdvancedPartitioning_Composite(t *testing.T) {
	body := `SUBPARTITION BY HASH ("CUST_ID") SUBPARTITIONS 4
		(PARTITION "P_2019" VALUES LESS THAN (TO_DATE(' 2020-01-01', 'YYYY-MM-DD')),
		 PARTITION "P_2020" VALUES LESS THAN (MAXVALUE))`
	stripped, notes := stripOracleAdvancedPartitioning(body, "RANGE", oraclePartNotes{})
	require.True(t, notes.HasSubpartitioning)
	require.Equal(t, "HASH", notes.SubpartitionMethod)
	require.NotContains(t, strings.ToUpper(stripped), "SUBPARTITION")
	require.Contains(t, stripped, "P_2019")
	require.Contains(t, stripped, "MAXVALUE")
}

// STORE IN (ts1, ts2) at depth 0 (after PARTITIONS N or after a
// subpartition spec) is stripped silently.
func TestStripOracleAdvancedPartitioning_StoreIn(t *testing.T) {
	body := `PARTITIONS 4 STORE IN (USERS, USERS2)`
	stripped, _ := stripOracleAdvancedPartitioning(body, "HASH", oraclePartNotes{})
	require.NotContains(t, strings.ToUpper(stripped), "STORE")
	require.Contains(t, stripped, "PARTITIONS 4")
}

// End-to-end: RANGE INTERVAL must lift to a PG RANGE-partitioned table
// (the existing snapshot of partitions) AND emit a blocking prerequisite
// telling the user that auto-creation is gone (use pg_partman).
func TestOracleRangeIntervalDowngrade(t *testing.T) {
	src := `
		CREATE TABLE "SH"."SALES_INT" (
		  "PROD_ID" NUMBER(6,0) NOT NULL,
		  "TIME_ID" DATE NOT NULL
		)
		PARTITION BY RANGE ("TIME_ID")
		INTERVAL (NUMTOYMINTERVAL(1,'MONTH'))
		(PARTITION "SALES_2019" VALUES LESS THAN (TO_DATE(' 2020-01-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')));`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	require.Len(t, res.Plan.Tables, 1)
	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning, "RANGE part of an INTERVAL partitioning must still lift")
	require.Equal(t, "RANGE", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 1)
	require.Equal(t, "sales_2019", tbl.Partitioning.Partitions[0].Name)

	// Warning surface
	var sawWarn bool
	for _, w := range res.Warnings {
		if w.Kind == "partitioning.interval" {
			sawWarn = true
			require.Contains(t, w.Message, "no native interval partitioning")
		}
	}
	require.True(t, sawWarn, "expected a partitioning.interval warning")

	// Blocking prerequisite for the user
	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "INTERVAL") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
			require.Equal(t, CatManualReview, p.Category)
			require.Contains(t, p.Remediation, "pg_partman")
		}
	}
	require.True(t, sawPrereq, "expected an INTERVAL-partitioning prerequisite")
}

// AUTOMATIC LIST: top-level LIST partitioning lifts; warning + prereq
// surface the missing auto-creation behaviour.
func TestOracleAutomaticListDowngrade(t *testing.T) {
	src := `
		CREATE TABLE "SH"."CUST_BY_REGION" (
		  "CUST_ID" NUMBER NOT NULL,
		  "REGION" VARCHAR2(2) NOT NULL
		)
		PARTITION BY LIST ("REGION") AUTOMATIC
		(PARTITION "P_EU" VALUES ('FR','DE','IT'));`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "LIST", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 1)
	require.Equal(t, "p_eu", tbl.Partitioning.Partitions[0].Name)

	var sawWarn bool
	for _, w := range res.Warnings {
		if w.Kind == "partitioning.automatic_list" {
			sawWarn = true
		}
	}
	require.True(t, sawWarn)

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "AUTOMATIC LIST") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
		}
	}
	require.True(t, sawPrereq)
}

// Composite RANGE-HASH: only the RANGE level is lifted; the SUBPARTITION
// BY HASH spec is dropped with a warning + prereq.
func TestOracleCompositeRangeHashFlattens(t *testing.T) {
	src := `
		CREATE TABLE "SH"."SALES_RH" (
		  "PROD_ID" NUMBER(6,0) NOT NULL,
		  "CUST_ID" NUMBER NOT NULL,
		  "TIME_ID" DATE NOT NULL
		)
		PARTITION BY RANGE ("TIME_ID")
		SUBPARTITION BY HASH ("CUST_ID") SUBPARTITIONS 4
		(PARTITION "SALES_2019" VALUES LESS THAN (TO_DATE(' 2020-01-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')),
		 PARTITION "SALES_2020" VALUES LESS THAN (MAXVALUE));`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning, "RANGE top-level must still lift")
	require.Equal(t, "RANGE", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 2,
		"two top-level RANGE partitions, subpartitions are flattened away")

	var sawWarn bool
	for _, w := range res.Warnings {
		if w.Kind == "partitioning.subpartition" {
			sawWarn = true
			require.Contains(t, w.Message, "HASH")
		}
	}
	require.True(t, sawWarn, "subpartition flattening warning expected")

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "composite") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
		}
	}
	require.True(t, sawPrereq)
}

// REFERENCE partitioning: no PG equivalent → table emitted unpartitioned
// + blocking prereq pointing at the manual workaround.
func TestOracleReferencePartitioningFallback(t *testing.T) {
	src := `
		CREATE TABLE "SH"."ORDER_ITEMS" (
		  "ITEM_ID" NUMBER NOT NULL,
		  "ORDER_ID" NUMBER NOT NULL,
		  CONSTRAINT "FK_ITEMS_ORDER" FOREIGN KEY ("ORDER_ID") REFERENCES "ORDERS" ("ID")
		)
		PARTITION BY REFERENCE ("FK_ITEMS_ORDER");`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.Nil(t, tbl.Partitioning,
		"REFERENCE partitioning has no PG equivalent — table must be emitted unpartitioned")

	var sawWarn bool
	for _, w := range res.Warnings {
		if w.Kind == "partitioning.reference" {
			sawWarn = true
		}
	}
	require.True(t, sawWarn)

	var sawPrereq bool
	for _, p := range res.Prerequisites {
		if strings.Contains(p.Title, "REFERENCE partitioning") {
			sawPrereq = true
			require.Equal(t, SeverityBlocking, p.Severity)
			require.Equal(t, CatManualReview, p.Category)
		}
	}
	require.True(t, sawPrereq)
}

// HASH partitioning with `STORE IN (ts1, ts2)` after PARTITIONS N must
// still produce 4 children (the store-in tablespace list is silently
// stripped — PG has no per-partition tablespace concept worth preserving
// in this code path).
func TestOracleHashPartitioningWithStoreIn(t *testing.T) {
	src := `
		CREATE TABLE "SH"."CUST_HASHED" (
		  "CUST_ID" NUMBER NOT NULL
		)
		PARTITION BY HASH ("CUST_ID") PARTITIONS 4 STORE IN (USERS, USERS2);`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning,
		"HASH partitioning with STORE IN must still lift")
	require.Equal(t, "HASH", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 4)
}

// RANGE with per-partition tablespace trailers (`PARTITION p VALUES LESS
// THAN (10) TABLESPACE users`) must still produce the partition with the
// correct bound — the trailer is dropped.
func TestOracleRangePerPartitionTrailerStripped(t *testing.T) {
	src := `
		CREATE TABLE "SH"."SALES_TS" (
		  "TIME_ID" DATE NOT NULL
		)
		PARTITION BY RANGE ("TIME_ID")
		(PARTITION "P1" VALUES LESS THAN (TO_DATE(' 2020-01-01 00:00:00', 'SYYYY-MM-DD HH24:MI:SS', 'NLS_CALENDAR=GREGORIAN')) TABLESPACE USERS,
		 PARTITION "P2" VALUES LESS THAN (MAXVALUE) TABLESPACE USERS2);`

	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

	tbl := res.Plan.Tables[0]
	require.NotNil(t, tbl.Partitioning)
	require.Equal(t, "RANGE", tbl.Partitioning.Method)
	require.Len(t, tbl.Partitioning.Partitions, 2)
	require.Equal(t, "p1", tbl.Partitioning.Partitions[0].Name)
	require.Equal(t, "'2020-01-01 00:00:00'", tbl.Partitioning.Partitions[0].UpperBound)
	require.Equal(t, "MAXVALUE", tbl.Partitioning.Partitions[1].UpperBound)
}
