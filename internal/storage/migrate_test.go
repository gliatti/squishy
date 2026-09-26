package storage

import (
	"strings"
	"testing"
	"testing/fstest"
)

func versions(ms []Migration) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.Version
	}
	return out
}

func equalVersions(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A lexical sort puts "000002_" before "0001_": the loader must order by
// numeric version so the init script always runs first.
func TestLoadMigrationsOrdersNumerically(t *testing.T) {
	fsys := fstest.MapFS{
		"m/000002_b.up.sql":   {Data: []byte("SELECT 2")},
		"m/000003_c.up.sql":   {Data: []byte("SELECT 3")},
		"m/000003_c.down.sql": {Data: []byte("SELECT -3")},
		"m/0001_a.up.sql":     {Data: []byte("SELECT 1")},
		"m/0001_a.down.sql":   {Data: []byte("SELECT -1")},
		"m/10_j.up.sql":       {Data: []byte("SELECT 10")},
		"m/README.md":         {Data: []byte("ignored")},
	}
	ms, err := LoadMigrations(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := versions(ms), []int64{1, 2, 3, 10}; !equalVersions(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if ms[0].UpFile != "0001_a.up.sql" || ms[0].DownFile != "0001_a.down.sql" {
		t.Fatalf("pairing of version 1 = %+v", ms[0])
	}
	if ms[1].DownFile != "" {
		t.Fatalf("version 2 has no down script, got %q", ms[1].DownFile)
	}
}

func TestLoadMigrationsRejectsAmbiguousLayouts(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"duplicate version": {
			"m/0001_a.up.sql":   {},
			"m/000001_b.up.sql": {},
		},
		"same version twice for one direction": {
			"m/0001_a.up.sql":   {},
			"m/000001_a.up.sql": {},
		},
		"non numeric prefix": {"m/v1_a.up.sql": {}},
		"no name":            {"m/0001.up.sql": {}},
		"down without up":    {"m/0001_a.down.sql": {}},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadMigrations(fsys, "m"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// The embedded set must load cleanly, start with the init script and use
// the 6-digit sequence naming produced by `make migrate-new`.
func TestEmbeddedMigrations(t *testing.T) {
	ms, err := LoadMigrations(embeddedMigrations, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].Version != 1 || ms[0].Name != "init" {
		t.Fatalf("first migration must be 000001_init, got %+v", ms)
	}
	for i, m := range ms {
		if m.Version != int64(i+1) {
			t.Fatalf("versions must be contiguous from 1, got %v", versions(ms))
		}
		digits, _, _ := strings.Cut(m.UpFile, "_")
		if len(digits) != 6 {
			t.Fatalf("%s: version prefix must be 6 digits", m.UpFile)
		}
		if m.DownFile == "" {
			t.Fatalf("%s: missing down script", m.UpFile)
		}
	}
}

// Ledger rows written under a previous file name (0001_init.up.sql) still
// mark their version as applied, so the rename never replays the init.
func TestPendingMigrationsMatchesLedgerByVersion(t *testing.T) {
	all := []Migration{
		{Version: 1, Name: "init", UpFile: "000001_init.up.sql", DownFile: "000001_init.down.sql"},
		{Version: 2, Name: "b", UpFile: "000002_b.up.sql", DownFile: "000002_b.down.sql"},
		{Version: 3, Name: "c", UpFile: "000003_c.up.sql"},
	}
	applied, err := appliedVersions([]string{"0001_init.up.sql", "000002_b.up.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(pendingMigrations(all, applied)); !equalVersions(got, []int64{3}) {
		t.Fatalf("pending = %v, want [3]", got)
	}
	if got := versions(pendingMigrations(all, map[int64]string{})); !equalVersions(got, []int64{1, 2, 3}) {
		t.Fatalf("pending on empty ledger = %v", got)
	}

	back, err := rollbackMigrations(all, applied, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(back); !equalVersions(got, []int64{2, 1}) {
		t.Fatalf("rollback = %v, want [2 1]", got)
	}
	if applied[1] != "0001_init.up.sql" {
		t.Fatalf("rollback must delete the ledger row under its recorded name, got %q", applied[1])
	}

	applied[3] = "000003_c.up.sql"
	if _, err := rollbackMigrations(all, applied, 1); err == nil {
		t.Fatal("rolling back a migration without down script must fail")
	}
}

func TestAppliedVersionsRejectsGarbageLedger(t *testing.T) {
	if _, err := appliedVersions([]string{"init.sql"}); err == nil {
		t.Fatal("expected an error for an unparseable ledger entry")
	}
}
