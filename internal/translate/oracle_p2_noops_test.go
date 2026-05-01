package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dialects/oracle"
)

// Each Oracle CREATE/DDL kind without a PG counterpart parses cleanly and
// surfaces an info-level explanation. The tests are kept tight — one per
// kind — to lock in the dispatcher routing and the explanation tagging.
func TestOracleP2NoopsExplained(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// kind matches against the Source field of the emitted explanation
		// (case-sensitive substring).
		kind string
	}{
		{"cluster", `CREATE CLUSTER c (id NUMBER) SIZE 100;`, "CREATE CLUSTER"},
		{"context", `CREATE CONTEXT mig_ctx USING mig.set_ctx_pkg;`, "CREATE CONTEXT"},
		{"directory", `CREATE OR REPLACE DIRECTORY data_dir AS '/var/oracle/data';`, "CREATE DIRECTORY"},
		{"library", `CREATE OR REPLACE LIBRARY libfoo IS '/usr/lib/libfoo.so';`, "CREATE LIBRARY"},
		{"java", `CREATE OR REPLACE JAVA SOURCE NAMED "Hello" AS public class Hello {};`, "CREATE JAVA"},
		{"dblink", `CREATE DATABASE LINK remote_db CONNECT TO mig IDENTIFIED BY pw USING 'remote';`, "CREATE DATABASE LINK"},
		{"profile", `CREATE PROFILE app_user LIMIT SESSIONS_PER_USER 5;`, "CREATE PROFILE"},
		{"lockdown", `CREATE LOCKDOWN PROFILE strict;`, "CREATE LOCKDOWN PROFILE"},
		{"edition", `CREATE EDITION e1 AS CHILD OF ora$base;`, "CREATE EDITION"},
		{"attr_dim", `CREATE ATTRIBUTE DIMENSION my_dim USING customers ATTRIBUTES (id);`, "CREATE ATTRIBUTE DIMENSION"},
		{"hierarchy", `CREATE HIERARCHY h1 USING my_dim (a CHILD OF b);`, "CREATE HIERARCHY"},
		{"flashback_arch", `CREATE FLASHBACK ARCHIVE my_arch TABLESPACE users RETENTION 1 YEAR;`, "CREATE FLASHBACK ARCHIVE"},
		{"audit_policy", `CREATE AUDIT POLICY p ACTIONS SELECT ON orders;`, "CREATE AUDIT POLICY"},
		{"tablespace", `CREATE TABLESPACE users DATAFILE '/u01/users.dbf' SIZE 100M;`, "CREATE TABLESPACE"},
		{"role", `CREATE ROLE app_writer;`, "CREATE ROLE"},
		{"user", `CREATE USER app IDENTIFIED BY pw;`, "CREATE USER"},
		{"indextype", `CREATE INDEXTYPE my_ix FOR contains(VARCHAR2,VARCHAR2) USING ctx;`, "CREATE INDEXTYPE"},
		{"audit_action", `AUDIT SELECT ON orders;`, "AUDIT"},
		{"flashback_table", `FLASHBACK TABLE orders TO TIMESTAMP SYSTIMESTAMP - INTERVAL '1' DAY;`, "FLASHBACK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmts, errs := oracle.Parse(tc.src)
			require.Empty(t, errs, "parse should not error for: %s", tc.src)
			res := Translate(stmts, Options{TargetSchema: "mig", SourceKind: "oracle"})

			// No PG DDL emitted for any of these.
			for _, s := range res.Plan.PreActions {
				require.NotContains(t, s, tc.kind,
					"%s must not produce a PG pre-action", tc.kind)
			}
			// Explanation surfaces the kind.
			var sawExpl bool
			for _, e := range res.Explanations {
				if strings.Contains(e.Source, tc.kind) {
					sawExpl = true
				}
			}
			require.True(t, sawExpl, "expected an explanation for %s", tc.kind)
		})
	}
}
