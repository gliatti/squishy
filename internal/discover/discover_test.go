package discover

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestSchemasUnsupportedKind(t *testing.T) {
	_, err := Schemas(context.Background(), nil, "postgres")
	if err == nil {
		t.Fatal("expected error for unsupported kind")
	}
	if got, want := err.Error(), "discover: unsupported kind postgres"; got != want {
		t.Fatalf("unexpected error message: got %q want %q", got, want)
	}
}

func TestFilterMySQL(t *testing.T) {
	in := []string{"information_schema", "mysql", "performance_schema", "sys", "app", "shop"}
	got := filterMySQL(in)
	want := []string{"app", "shop"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterMySQL = %v, want %v", got, want)
	}
}

func TestFilterMySQLEmpty(t *testing.T) {
	got := filterMySQL(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestFilterOracle(t *testing.T) {
	in := []string{
		"SYS", "SYSTEM", "XDB", "MDSYS",
		"APEX_220200", "ORDS_PUBLIC_USER", "FLOWS_FILES",
		// PDBADMIN is the 23ai PDB-local admin: not flagged
		// oracle_maintained='Y' in DBA_USERS, but still not migration
		// material — must be caught by the in-Go safety net.
		"PDBADMIN",
		"HR", "SCOTT", "APP_USER",
	}
	got := filterOracle(in)
	want := []string{"APP_USER", "HR", "SCOTT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterOracle = %v, want %v", got, want)
	}
}

func TestIsOraTableMissing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection reset"), false},
		{"ora-00942 bare", errors.New("ORA-00942: table or view does not exist"), true},
		{"ora-00942 wrapped", errors.New("godror: query failed: ORA-00942: ..."), true},
		{"different ora code", errors.New("ORA-01017: invalid username/password"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOraTableMissing(c.err); got != c.want {
				t.Fatalf("isOraTableMissing(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestHasInternalPrefix(t *testing.T) {
	cases := map[string]bool{
		"APEX_220100":      true,
		"ORDS_METADATA":    true,
		"FLOWS_FILES":      true,
		"HR":               false,
		"APP":              false,
		"APEXFOO":          false, // no underscore — not a match
	}
	for in, want := range cases {
		if got := hasInternalPrefix(in); got != want {
			t.Errorf("hasInternalPrefix(%q) = %v, want %v", in, got, want)
		}
	}
}
