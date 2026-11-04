package tsql

import (
	"testing"

	"gitlab.com/dalibo/squishy/internal/dialects/postgres"
	"gitlab.com/dalibo/squishy/internal/sqlparse/ast"
)

// TestParseXQuery_RoundTrip parses the XQuery shapes of the AdventureWorks
// views and procedures and prints them back as XPath 1.0.
func TestParseXQuery_RoundTrip(t *testing.T) {
	cases := []struct {
		src, want string
		ns        []ast.XQNamespace
		def       string
	}{
		{src: `declare default element namespace "urn:r";
        (/Resume/Name/Name.Prefix)[1]`, want: `(/Resume/Name/Name.Prefix)[1]`, def: "urn:r"},
		{src: `(Address/Addr.Location/Location/Loc.CountryRegion)[1]`, want: `(Address/Addr.Location/Location/Loc.CountryRegion)[1]`},
		{src: `declare namespace ci="urn:ci";
        declare namespace act="urn:act";
        (act:telephoneNumber)[1]/act:number`, want: `(act:telephoneNumber)[1]/act:number`,
			ns: []ast.XQNamespace{{Prefix: "ci", URI: "urn:ci"}, {Prefix: "act", URI: "urn:act"}}},
		{src: `(act:eMail/act:SpecialInstructions/text())[1]`, want: `(act:eMail/act:SpecialInstructions/text())[1]`},
		{src: `@LocationID[1]`, want: `@LocationID[1]`},
		{src: `string(.)[1]`, want: `string(.)[1]`},
		{src: `TotalPurchaseYTD[1]`, want: `TotalPurchaseYTD[1]`},
		{src: `/`, want: `/`},
		{src: `//a[@x = "1" and b eq 2]/c`, want: `//a[@x = "1" and b = 2]/c`},
		{src: `/a/child::b/descendant-or-self::node()/..`, want: `/a/b/descendant-or-self::node()/..`},
		{src: `count(/a/*) > 1`, want: `count(/a/*) > 1`},
		{src: `(: comment :) /p:a/p:*`, want: `/p:a/p:*`},
		{src: `/a[. = 'it''s']`, want: `/a[. = "it's"]`},
	}
	for _, c := range cases {
		q, err := ParseXQuery(c.src)
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if got := postgres.WriteXPath1(q.Body); got != c.want {
			t.Errorf("%q:\n got %s\nwant %s", c.src, got, c.want)
		}
		if q.DefaultElementNS != c.def {
			t.Errorf("%q: default namespace %q, want %q", c.src, q.DefaultElementNS, c.def)
		}
		if len(c.ns) > 0 && (len(q.Namespaces) != len(c.ns) || q.Namespaces[1] != c.ns[1]) {
			t.Errorf("%q: namespaces %+v", c.src, q.Namespaces)
		}
	}
}

// TestParseXQuery_Shapes checks the node shapes the translator relies on.
func TestParseXQuery_Shapes(t *testing.T) {
	q, err := ParseXQuery(`/Resume`)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := q.Body.(*ast.XQPath)
	if !ok || !p.Root || len(p.Steps) != 1 || p.Steps[0].Test == nil || p.Steps[0].Test.Local != "Resume" {
		t.Fatalf("absolute one-step path: %#v", q.Body)
	}
	q, err = ParseXQuery(`string(.)[1]`)
	if err != nil {
		t.Fatal(err)
	}
	p, ok = q.Body.(*ast.XQPath)
	if !ok || p.Root || len(p.Steps) != 1 || len(p.Steps[0].Preds) != 1 {
		t.Fatalf("filter step: %#v", q.Body)
	}
	if fc, ok := p.Steps[0].Primary.(*ast.XQFuncCall); !ok || fc.Name != "string" {
		t.Fatalf("filter primary: %#v", p.Steps[0].Primary)
	}
	q, err = ParseXQuery(`.`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Body.(*ast.XQContextItem); !ok {
		t.Fatalf("context item: %#v", q.Body)
	}
}

// TestParseXQuery_Unsupported lists constructs outside the subset: each
// is an error, never a silent misparse.
func TestParseXQuery_Unsupported(t *testing.T) {
	for _, src := range []string{
		`for $i in /a return $i`,
		`replace value of (/a)[1] with 1`,
		`/a[sql:column("t.c") = 1]`,
		`$x/a`,
		`<a/>`,
		`/a, /b`,
		`declare boundary-space preserve; /a`,
		`"unterminated`,
	} {
		if _, err := ParseXQuery(src); err == nil {
			t.Errorf("%q: want an error", src)
		}
	}
}
