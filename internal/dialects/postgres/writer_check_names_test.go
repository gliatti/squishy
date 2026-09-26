package postgres

import "testing"

// A named table-level CHECK keeps its source name (`CONSTRAINT "n"
// CHECK (…)`); an unnamed one, or one past the end of a shorter
// CheckNames slice, is emitted bare so PG generates the name.
func TestWriteCreateTable_CheckNames(t *testing.T) {
	got := Write([]Stmt{&CreateTable{
		Schema:     "public",
		Name:       "job_history",
		Columns:    []ColumnDef{{Name: "start_date", Type: "DATE"}, {Name: "end_date", Type: "DATE"}},
		PrimaryKey: []string{"start_date"},
		Checks:     []string{`("end_date" > "start_date")`, `("end_date" < '2100-01-01')`, `("start_date" > '1900-01-01')`},
		CheckNames: []string{"jhist_date_interval", ""},
	}})
	want := `CREATE TABLE "public"."job_history" (
  "start_date" DATE,
  "end_date" DATE,
  PRIMARY KEY ("start_date"),
  CONSTRAINT "jhist_date_interval" CHECK (("end_date" > "start_date")),
  CHECK (("end_date" < '2100-01-01')),
  CHECK (("start_date" > '1900-01-01'))
);
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
