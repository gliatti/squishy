package translate

import (
	"embed"
	"strings"
)

// tsql_shim.go — the PL/pgSQL runtime the T-SQL rewriters call (schema
// tsql: like, dateadd, datediff, datepart, datename, isnumeric, isdate,
// convert_style).
//
// The SQL lives in tsql_shim/*.sql, one file per function family, so a
// migration creates only the functions its translated objects call: the
// rewriters record every tsql.* call they leave on the AST
// (recordTSQLShimUse → HookRun.Use), and tsqlShimPreActions, registered
// as the SQL Server pre-actions provider, emits the matching files
// (dependencies included) at the head of Plan.PreActions. The files are
// never split or scanned in Go: each one is emitted whole.
//
// postgres-init/03-tsql-shim.sql is the concatenation of the pieces in
// file-name order, loaded into the dev cluster at bootstrap;
// TestTSQLShim_InitFileMatchesPieces keeps it identical.

//go:embed tsql_shim/*.sql
var tsqlShimFS embed.FS

// tsqlShimSchemaFile creates the tsql schema; it precedes every other
// piece.
const tsqlShimSchemaFile = "00_schema.sql"

// tsqlShimFuncs maps each shim function the rewriters emit (FuncCall
// name) to the shim files it needs, dependencies first.
var tsqlShimFuncs = map[string][]string{
	"tsql.like":          {"20_like.sql"},
	"tsql.dateadd":       {"10_datepart_canon.sql", "30_dateadd.sql"},
	"tsql.datediff":      {"10_datepart_canon.sql", "31_datediff.sql"},
	"tsql.datepart":      {"10_datepart_canon.sql", "32_datepart.sql"},
	"tsql.datename":      {"10_datepart_canon.sql", "32_datepart.sql", "33_datename.sql"},
	"tsql.isnumeric":     {"40_isnumeric.sql"},
	"tsql.isdate":        {"41_isdate.sql"},
	"tsql.convert_style": {"50_convert_style.sql"},
}

// tsqlShimFileNames returns the shim pieces in file-name order (the
// embed.FS directory listing is sorted).
func tsqlShimFileNames() []string {
	entries, err := tsqlShimFS.ReadDir("tsql_shim")
	if err != nil {
		panic("tsql shim: " + err.Error())
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// tsqlShimFile returns one shim piece.
func tsqlShimFile(name string) string {
	b, err := tsqlShimFS.ReadFile("tsql_shim/" + name)
	if err != nil {
		panic("tsql shim: " + err.Error())
	}
	return string(b)
}

// tsqlShimScript is the whole shim, as postgres-init/03-tsql-shim.sql
// holds it.
func tsqlShimScript() string {
	var b strings.Builder
	for _, f := range tsqlShimFileNames() {
		b.WriteString(tsqlShimFile(f))
	}
	return b.String()
}

// tsqlShimPreActions returns the shim pieces the run's translated
// objects call (nil when none): the schema first, then each needed file
// once, in file-name order — which is dependency order.
func tsqlShimPreActions(run *HookRun) []string {
	if run == nil {
		return nil
	}
	need := map[string]bool{}
	for fn, files := range tsqlShimFuncs {
		if !run.Used(fn) {
			continue
		}
		for _, f := range files {
			need[f] = true
		}
	}
	if len(need) == 0 {
		return nil
	}
	out := []string{tsqlShimFile(tsqlShimSchemaFile)}
	for _, f := range tsqlShimFileNames() {
		if need[f] {
			out = append(out, tsqlShimFile(f))
		}
	}
	return out
}
