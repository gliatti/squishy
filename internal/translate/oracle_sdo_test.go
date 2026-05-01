package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// SDO_DISTANCE / SDO_GEOM.SDO_DISTANCE → ST_Distance, dropping any
// trailing tolerance / unit args.
func TestSDODistanceRewrite(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SDO_DISTANCE(g1, g2)", "ST_Distance(g1, g2)"},
		{"SDO_DISTANCE(g1, g2, 0.005)", "ST_Distance(g1, g2)"},
		{"SDO_GEOM.SDO_DISTANCE(g1, g2, 0.005, 'unit=METER')", "ST_Distance(g1, g2)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := rewriteOracleCollectionTokens(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// SDO_GEOM.SDO_AREA / SDO_GEOM.SDO_LENGTH — drop tolerance/unit, keep only g.
func TestSDOAreaLengthRewrite(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SDO_GEOM.SDO_AREA(g, 0.005)", "ST_Area(g)"},
		{"SDO_GEOM.SDO_LENGTH(g)", "ST_Length(g)"},
		{"SDO_GEOM.SDO_LENGTH(g, 0.005, 'unit=KM')", "ST_Length(g)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := rewriteOracleCollectionTokens(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// SDO_GEOM.SDO_INTERSECTION / UNION / DIFFERENCE → ST_*.
func TestSDOSetOpsRewrite(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SDO_GEOM.SDO_INTERSECTION(g1, g2, 0.005)", "ST_Intersection(g1, g2)"},
		{"SDO_GEOM.SDO_UNION(g1, g2, 0.005)", "ST_Union(g1, g2)"},
		{"SDO_GEOM.SDO_DIFFERENCE(g1, g2, 0.005)", "ST_Difference(g1, g2)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := rewriteOracleCollectionTokens(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// SDO_RELATE — mask drives the predicate selection.
func TestSDORelateRewrite(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SDO_RELATE(g1, g2, 'mask=anyinteract')", "ST_Intersects(g1, g2)"},
		{"SDO_RELATE(g1, g2, 'MASK=CONTAINS')", "ST_Contains(g1, g2)"},
		{"SDO_RELATE(g1, g2, 'mask=inside')", "ST_Within(g1, g2)"},
		{"SDO_RELATE(g1, g2, 'mask=covers')", "ST_Covers(g1, g2)"},
		{"SDO_RELATE(g1, g2, 'mask=touch')", "ST_Touches(g1, g2)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := rewriteOracleCollectionTokens(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// SDO_RELATE with an unknown mask → left untouched (PG surfaces the error).
func TestSDORelateUnknownMaskUntouched(t *testing.T) {
	in := "SDO_RELATE(g1, g2, 'mask=mystery')"
	got := rewriteOracleCollectionTokens(in)
	require.Equal(t, in, got, "unknown mask must leave SDO_RELATE untouched")
}

// SDO_WITHIN_DISTANCE → ST_DWithin with extracted distance.
func TestSDOWithinDistanceRewrite(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SDO_WITHIN_DISTANCE(g1, g2, 'distance=100')", "ST_DWithin(g1, g2, 100)"},
		{"SDO_WITHIN_DISTANCE(g1, g2, 'distance=2.5 unit=METER')", "ST_DWithin(g1, g2, 2.5)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := rewriteOracleCollectionTokens(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// SDO functions inside a string literal must NOT be rewritten.
func TestSDOInStringLiteralUntouched(t *testing.T) {
	in := "RAISE NOTICE 'call SDO_DISTANCE(g1,g2)'"
	got := rewriteOracleCollectionTokens(in)
	require.Equal(t, in, got)
}

// End-to-end: SDO functions inside a view body get rewritten when the view
// translation runs the Oracle expression rewriter.
func TestSDOEndToEndInViewBody(t *testing.T) {
	src := `
		CREATE OR REPLACE VIEW "MIG"."V_NEAR" AS
		  SELECT a.id, b.id AS other_id
		  FROM points a, points b
		  WHERE SDO_RELATE(a.geom, b.geom, 'mask=anyinteract') = 'TRUE';`
	stmts, errs := oracle.Parse(src)
	require.Empty(t, errs)
	res := Translate(stmts, Options{
		TargetSchema:     "mig",
		SourceKind:       "oracle",
		TargetExtensions: []string{"postgis"},
	})
	require.Len(t, res.Plan.Views, 1)
	ddl := res.Plan.Views[0].DDL
	require.Contains(t, ddl, "ST_Intersects(",
		"SDO_RELATE in view body must rewrite to ST_Intersects, got: %s", ddl)
	require.NotContains(t, strings.ToUpper(ddl), "SDO_RELATE(")
}
