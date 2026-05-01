package translate

import (
	"crypto/sha1"
	"fmt"
	"sort"
	"strings"
)

// buildPrerequisites folds the translator's Warnings into actionable
// Prerequisites with remediation instructions. Every warning produces at
// least one prereq; the kind field drives the remediation template.
//
// Called at the end of Translate, so all Warnings and Plan.Events/Routines
// are populated.
func (t *translator) buildPrerequisites() {
	// Deduplicate by (category,title) so the checklist stays readable when
	// several objects share the same remediation.
	seen := map[string]*Prerequisite{}
	// Seed with prerequisites already enqueued by the per-statement
	// translators (e.g. CREATE TABLE … AS SELECT manual review). Without
	// this, the assignment at the end of buildPrerequisites would wipe them.
	for _, p := range t.res.Prerequisites {
		key := string(p.Category) + "|" + p.Title
		cp := p
		if cp.ID == "" {
			cp.ID = prereqID(cp.Category, cp.Title, cp.Object)
		}
		seen[key] = &cp
	}
	add := func(p Prerequisite) {
		key := string(p.Category) + "|" + p.Title
		if existing, ok := seen[key]; ok {
			if !strings.Contains(existing.Object, p.Object) && p.Object != "" {
				existing.Object = strings.TrimSuffix(existing.Object, ",") + ", " + p.Object
			}
			return
		}
		p.ID = prereqID(p.Category, p.Title, p.Object)
		cp := p
		seen[key] = &cp
	}

	for _, w := range t.res.Warnings {
		switch w.Kind {
		case "type":
			if strings.Contains(w.Message, "spatial type") {
				add(Prerequisite{
					Severity:    SeverityBlocking,
					Category:    CatInstallExtension,
					Object:      w.Object,
					Title:       "Install PostGIS for spatial columns",
					Description: "The source schema contains spatial types (GEOMETRY/POINT/…) that PostgreSQL can only represent once the PostGIS extension is installed. Without PostGIS, squishy stored these columns as TEXT placeholders and the source data is not migrated.",
					Remediation: `# On the target Postgres host:
sudo apt-get install postgresql-17-postgis-3   # Debian/Ubuntu
# or
dnf install postgis34_17                       # RHEL/Rocky

# Inside psql, connected to the target DB:
CREATE EXTENSION IF NOT EXISTS postgis;

# Then re-plan the migration so spatial columns map to 'geometry' instead of 'TEXT'.`,
				})
			} else if strings.Contains(w.Message, "pgvector") {
				add(Prerequisite{
					Severity:    SeverityBlocking,
					Category:    CatInstallExtension,
					Object:      w.Object,
					Title:       "Install pgvector for VECTOR columns",
					Description: "The source schema contains VECTOR columns (MariaDB 11.7+ or Oracle 23ai). PostgreSQL has no built-in vector type — pgvector is the de-facto standard and the only way to preserve distance operators (<->, <=>, <#>) and HNSW/IVFFlat indexing. squishy will not run the migration until pgvector is installed on the target.",
					Remediation: `# On the target Postgres host:
sudo apt-get install postgresql-17-pgvector   # Debian/Ubuntu (package may be named pgvector)
# or build from source (https://github.com/pgvector/pgvector):
git clone https://github.com/pgvector/pgvector.git
cd pgvector && make && sudo make install

# Inside psql, connected to the target DB:
CREATE EXTENSION IF NOT EXISTS vector;

# Re-plan the migration; VECTOR columns will then map to pgvector's vector(N) type.`,
				})
			} else {
				add(Prerequisite{
					Severity:    SeverityInfo,
					Category:    CatManualReview,
					Object:      w.Object,
					Title:       "Review type mapping",
					Description: w.Message,
					Remediation: "No automatic remediation. Open the target DDL preview (wizard step 3) and verify the mapped column type suits your application's expectations.",
				})
			}
		case "view.functions":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualReview,
				Object:      w.Object,
				Title:       "Review views that use MySQL-specific functions",
				Description: "One or more views use MySQL-specific functions (JSON_EXTRACT, GROUP_CONCAT, …). squishy applies a token-level rewrite (backticks → quotes, function renames, GROUP_CONCAT → string_agg) but some calls may still be incorrect — especially JSON path syntax which differs between MySQL and PG.",
				Remediation: `Open the generated view DDL in wizard step 3 (the "post-copy" block).
For each flagged view, verify:
  * JSON_EXTRACT(col, '$.a.b')   → col -> 'a' -> 'b'           (JSONB)
  * JSON_UNQUOTE(JSON_EXTRACT(col,'$.a'))
                                 → col ->> 'a'
  * GROUP_CONCAT(x SEPARATOR ',') → string_agg(x::text, ',')   (already auto-rewritten)
Paste the corrected SELECT into the view body if needed, then re-plan.`,
			})
		case "routine.untranslated_construct":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualReview,
				Object:      w.Object,
				Title:       "Review partially-translated routine body",
				Description: "squishy auto-translated the procedure/function/trigger body from MySQL PL/SQL to PL/pgSQL (SET → :=, WHILE DO → WHILE LOOP, LEAVE → EXIT, JSON_EXTRACT → ->, …), but the following constructs don't map cleanly and need human review:\n\n  • " + w.Message + "\n\nThe generated routine will be created on the target but may behave differently from the source.",
				Remediation: `Compare the MySQL source and the PG output:
  docker compose exec mysql-sample mysql -u sakila -psakila sakila \
    -e "SHOW CREATE <TYPE> <name>"
  docker compose exec postgres psql -U squishy -d squishy -c '\sf mig."<name>"'
Adjust the PG body for the flagged construct (format strings, dynamic SQL,
session variables, SIGNAL, GROUP_CONCAT ORDER BY, …). Most common remaps:
  • DATE_FORMAT(d,'%Y-%m-%d')  → to_char(d,'YYYY-MM-DD')
  • STR_TO_DATE(s,'%Y-%m-%d')  → to_date(s,'YYYY-MM-DD')
  • @session_var               → local DECLARE var; var := …
  • SIGNAL SQLSTATE '45000'    → RAISE EXCEPTION USING ERRCODE='45000'`,
			})
		case "trigger.follows":
			// The trigger translator now auto-renames triggers carrying
			// an Oracle FOLLOWS/PRECEDES dependency so PG's alphabetical
			// firing order matches the source's intent (autoRenameForOrder
			// in translator.go). The remaining `trigger.follows` warning
			// is purely informational — surface it as info so the user
			// can confirm the rename without it blocking the run.
			add(Prerequisite{
				Severity:    SeverityInfo,
				Category:    CatManualReview,
				Object:      w.Object,
				Title:       "Trigger execution order (FOLLOWS/PRECEDES) auto-resolved",
				Description: "Oracle's FOLLOWS/PRECEDES clause was preserved by auto-renaming the dependent trigger (or by checking that alphabetical order already matches). PostgreSQL fires same-event triggers in alphabetical order, so the rename keeps the original execution semantics. No user action required.",
				Remediation: "",
			})
		case "event.pg_cron_required":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatInstallExtension,
				Object:      w.Object,
				Title:       "Install pg_cron for scheduled events",
				Description: "The source schema contains one or more MySQL EVENTs (scheduled tasks). PostgreSQL has no native equivalent; pg_cron is the community standard replacement. Without it, the scheduled work is not migrated.",
				Remediation: cronRemediation(t),
			})
		case "event.one_shot":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualSQL,
				Object:      w.Object,
				Title:       "Schedule one-shot events manually in pg_cron",
				Description: "pg_cron is installed but it doesn't natively express one-shot (`ON SCHEDULE AT <ts>`) schedules. Recurring MySQL EVENTs were scheduled automatically; this one requires a manual cron expression matching your desired fire time, or using a deferred job queue.",
				Remediation: `Decide the firing moment and convert it to a cron expression, then run:
  SELECT cron.schedule('<name>', '<cron expr>', $$ <statement> $$);

Example (fire at 2026-06-01 03:00 UTC, i.e. minute 0 hour 3 day 1 month 6):
  SELECT cron.schedule('ev_once', '0 3 1 6 *', $$ CALL mig.p_recalc_total(42); $$);
Remember to call cron.unschedule('ev_once') after it has run, because pg_cron
keeps recurring schedules — there is no built-in "run once" semantics.`,
			})
		case "index.fulltext":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualSQL,
				Object:      w.Object,
				Title:       "Rebuild FULLTEXT indexes as tsvector + GIN",
				Description: "MySQL FULLTEXT indexes do not map 1:1 to PG. The recommended pattern is a generated tsvector column plus a GIN index.",
				Remediation: `For each flagged table/column list:
  ALTER TABLE "mig"."<table>"
    ADD COLUMN ft_search tsvector
      GENERATED ALWAYS AS (to_tsvector('simple', coalesce("<col1>",'') || ' ' || coalesce("<col2>",''))) STORED;
  CREATE INDEX ix_<table>_ft ON "mig"."<table>" USING GIN (ft_search);
Then update application queries: WHERE ft_search @@ plainto_tsquery('simple', :term).`,
			})
		case "index.spatial":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatInstallExtension,
				Object:      w.Object,
				Title:       "Install PostGIS to restore SPATIAL indexes",
				Description: "SPATIAL indexes require PostGIS, same as spatial columns.",
				Remediation: "See the PostGIS installation prerequisite.",
			})
		case "parse":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatFixSource,
				Object:      w.Object,
				Title:       "Parse errors in source DDL",
				Description: "The MySQL parser could not fully interpret the source DDL:\n\n" + w.Message + "\n\nUnparsed statements are silently dropped from the plan. squishy will not run a migration while parse errors remain, because doing so would quietly lose objects.",
				Remediation: `Inspect the failing object via:
  docker compose exec mysql-sample mysql -u sakila -psakila sakila \
    -e "SHOW CREATE <TYPE> <name>"
Either:
  * adjust the source (drop SET sql_mode directives MySQL 8 injects into SHOW CREATE VIEW output), or
  * file a parser extension ticket against internal/dialects/mysql/parser.go (grammar reference is in reference/MySqlParser.g4).`,
			})
		case "engine":
			add(Prerequisite{
				Severity:    SeverityInfo,
				Category:    CatManualReview,
				Object:      w.Object,
				Title:       "Non-InnoDB storage engine ignored",
				Description: w.Message,
				Remediation: "PostgreSQL has a single storage engine. No action required unless you relied on MEMORY/MyISAM-specific semantics.",
			})
		case "table.system_versioning":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualSQL,
				Object:      w.Object,
				Title:       "Rebuild MariaDB SYSTEM VERSIONING with PG triggers",
				Description: "MariaDB's `WITH SYSTEM VERSIONING` automatically maintains a shadow history table and exposes `FOR SYSTEM_TIME AS OF / BETWEEN` query syntax. PostgreSQL has no native equivalent — squishy migrates the current row only. Historical rows and AS OF queries are NOT replicated.",
				Remediation: `Pick one of two remediation paths:
  1. Install the temporal_tables extension (https://github.com/arkhipov/temporal_tables):
       CREATE EXTENSION IF NOT EXISTS temporal_tables;
       -- one history table per versioned table:
       CREATE TABLE "mig"."<table>_history" (LIKE "mig"."<table>");
       CREATE TRIGGER versioning_trigger
         BEFORE INSERT OR UPDATE OR DELETE ON "mig"."<table>"
         FOR EACH ROW EXECUTE PROCEDURE versioning(
           'sys_period', '"mig"."<table>_history"', true);
     Then rewrite application AS OF queries against the history table.

  2. DIY: maintain a <table>_history shadow yourself with row-level
     BEFORE UPDATE / DELETE triggers that INSERT the OLD row into the
     shadow with a tstzrange validity column.

If your application doesn't actually rely on the historical rows, ack
this prerequisite to migrate the current rows only.`,
			})
		case "package.unsupported":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualReview,
				Object:      w.Object,
				Title:       "Rewrite MariaDB Oracle-compat package routines manually",
				Description: "MariaDB 10.3+ supports `CREATE PACKAGE` / `PACKAGE BODY` in `sql_mode=ORACLE`. PostgreSQL has no PACKAGE concept — every routine in the package needs to be promoted to a top-level FUNCTION or PROCEDURE in the target schema. Package-level state (variables, cursors) must be moved to a dedicated table or to GUC custom settings.",
				Remediation: `For each package:
  1. Inspect the source:
       SHOW CREATE PACKAGE <name>;
       SHOW CREATE PACKAGE BODY <name>;
  2. Promote each procedure/function to a top-level PG routine in the target
     schema (e.g. mig.<pkg>_<routine>). Translate PL/SQL → PL/pgSQL by hand
     for the body of each.
  3. Replace package-private variables with either:
       - a single-row state table (mig.<pkg>_state), or
       - PG GUC custom settings (current_setting / set_config).
  4. Update application call sites: <pkg>.<routine>(...) becomes mig.<pkg>_<routine>(...).`,
			})
		case "table.application_period":
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatManualReview,
				Object:      w.Object,
				Title:       "Application-time PERIOD FOR not replicated",
				Description: "MariaDB's `PERIOD FOR <name> (start_col, end_col)` (application-time period) lets queries use `FOR PORTION OF <period> FROM ... TO ...` semantics for partial-row updates and DELETEs. PostgreSQL has no equivalent: the two timestamp columns migrate as plain columns, but range-overlap protection and FOR PORTION OF rewriting are NOT generated.",
				Remediation: `For the flagged tables:
  * Add an EXCLUDE constraint to forbid overlapping rows for the same key:
      ALTER TABLE "mig"."<table>"
        ADD CONSTRAINT no_overlap EXCLUDE USING gist (
          <key> WITH =,
          tstzrange(<start_col>, <end_col>) WITH &&);
    (requires the btree_gist extension: CREATE EXTENSION btree_gist;)
  * Rewrite any FOR PORTION OF UPDATE/DELETE in application code as
    explicit DELETE + INSERT pairs that reflect the new ranges.`,
			})
		}
	}

	// Oracle package state → PG GUCs. squishy rewrote every `<pkg>.<var>`
	// reference in routine bodies to `current_setting('squishy.<pkg>.<var>',
	// true)::<type>` (read) or `set_config(...)` (write). PG accepts custom
	// GUCs at runtime without configuration since 9.2, but persisting
	// defaults across restarts (or pre-seeding the value before the first
	// read) requires a `custom_variable_classes`-style declaration in
	// postgresql.conf and/or per-database / per-role ALTER statements. We
	// surface the full list so the operator can drop them in.
	// orafce extension prereq — emitted iff any rewritten routine body
	// references an Oracle built-in package (UTL_FILE, DBMS_OUTPUT,
	// DBMS_APPLICATION_INFO, DBMS_LOB, DBMS_RANDOM, DBMS_UTILITY). orafce
	// (https://github.com/orafce/orafce) ships these as PG functions with
	// signatures matching Oracle 1:1, so the auto-translated routines
	// compile and run unchanged once the extension is installed. Mark
	// blocking so the migration can't be launched before the operator
	// confirms orafce is on the target.
	if t.usedAdminpack {
		hasIt := false
		for _, ext := range t.res.Plan.TargetExtensions {
			if strings.EqualFold(ext, "orafce") {
				hasIt = true
				break
			}
		}
		if !hasIt {
			add(Prerequisite{
				Severity:    SeverityBlocking,
				Category:    CatInstallExtension,
				Object:      "extension.orafce",
				Title:       "Install the orafce extension on the target",
				Description: "squishy left Oracle UTL_FILE / DBMS_LOB / DBMS_RANDOM / DBMS_UTILITY calls in place (lowercased and unquoted so PG name resolution finds them) and relies on the `orafce` extension to provide the matching schemas + functions. Oracle's PL/SQL built-ins map onto orafce 1:1 — UTL_FILE.FOPEN/PUT_LINE/PUT/PUTF/NEW_LINE/FCLOSE all exist with the same signatures.\n\nDiagnostic-only built-ins were rewritten to PG core (no extension required): DBMS_OUTPUT.PUT_LINE → RAISE NOTICE '%', msg ; DBMS_APPLICATION_INFO.SET_MODULE/SET_ACTION/SET_CLIENT_INFO → RAISE NOTICE.\n\nWithout the orafce extension the routines that DO call UTL_FILE / DBMS_LOB / DBMS_RANDOM / DBMS_UTILITY will fail at runtime with `schema \"utl_file\" does not exist` (or similar).",
				Remediation: "On the target PG cluster, as a SUPERUSER:\n\n  -- Debian / Ubuntu:  apt install postgresql-<ver>-orafce\n  -- RHEL / Fedora :   dnf install orafce_<ver>\n  -- macOS / source :  github.com/orafce/orafce → make && make install\n\n  CREATE EXTENSION IF NOT EXISTS orafce;\n\nVerify by running, for example:\n\n  SELECT utl_file.fopen('/tmp', 'foo', 'w');\n  SELECT dbms_output.put_line('hello');\n\nNote: UTL_FILE FOPEN paths must be absolute and reachable by the postgres OS user. orafce's UTL_FILE_DIR equivalent is configured via the `utl_file.utl_file_dir` GUC — set it in postgresql.conf or per-database via ALTER DATABASE.",
			})
		}
	}

	if len(t.packageVars) > 0 {
		// Dedup (pkg.var) pairs and cap the rendered list so the prereq
		// stays readable when a dump declares thousands of package vars.
		seenVar := map[string]bool{}
		type pv struct{ key, line string }
		var rows []pv
		for pkg, vs := range t.packageVars {
			for _, v := range vs {
				k := pkg + "." + v.Name
				if seenVar[k] {
					continue
				}
				seenVar[k] = true
				rows = append(rows, pv{
					key:  k,
					line: "  ALTER DATABASE :DB SET squishy." + pkg + "." + v.Name + " = '<initial_value>';  -- ::" + v.PG,
				})
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })

		const maxRenderedVars = 40
		var lines []string
		for i, r := range rows {
			if i >= maxRenderedVars {
				lines = append(lines, "  -- … and "+itoa(len(rows)-maxRenderedVars)+" more (see migration JSON for the full list)")
				break
			}
			lines = append(lines, r.line)
		}

		add(Prerequisite{
			Severity:    SeverityInfo,
			Category:    CatManualSQL,
			Object:      "package_vars (" + itoa(len(rows)) + ")",
			Title:       "Initialise migrated Oracle package variables (PG custom GUCs)",
			Description: "Oracle package state has no direct PG counterpart. squishy rewrote every read of an Oracle package variable to `current_setting('squishy.<pkg>.<var>', true)::<type>` and every write to `set_config('squishy.<pkg>.<var>', …, false)`. This preserves the per-session lifetime your code relied on, but the GUC must hold a starting value before the first read — otherwise current_setting returns NULL.\n\nRun the per-variable ALTER DATABASE shown below (or set them in postgresql.conf, depending on your PG version) so the values survive across sessions. " + itoa(len(rows)) + " variable(s) to declare.",
			Remediation: "After the migration completes, on the target PG cluster:\n\n" + strings.Join(lines, "\n") + "\n\nReplace `:DB` with the migrated database name and `<initial_value>` with the source-side default (Oracle initialises package vars to NULL by default, which matches PG's missing-GUC fallback when the read site uses the `, true` missing_ok flag squishy emitted).",
		})
	}

	out := make([]Prerequisite, 0, len(seen))
	for _, p := range seen {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == SeverityBlocking
		}
		return out[i].Title < out[j].Title
	})
	t.res.Prerequisites = out
}

// itoa is a local int→string shortcut used in human-readable prereq text;
// the standard library `strconv` import is intentionally avoided here so
// this file's import set stays minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func cronRemediation(t *translator) string {
	var snippets []string
	for _, ev := range t.res.Plan.Events {
		if ev.PgCron != "" {
			snippets = append(snippets, ev.PgCron)
		}
	}
	body := strings.Join(snippets, "\n\n")
	return fmt.Sprintf(`# On the target Postgres host, as superuser:
sudo apt-get install postgresql-17-cron       # Debian/Ubuntu (or build pg_cron from source)
# Then in postgresql.conf:
#   shared_preload_libraries = 'pg_cron'
#   cron.database_name = 'squishy'
# Restart the cluster.

# Inside psql:
CREATE EXTENSION IF NOT EXISTS pg_cron;

# Schedule the migrated events:
%s`, body)
}

func prereqID(cat Category, title, object string) string {
	h := sha1.Sum([]byte(string(cat) + "|" + title + "|" + object))
	return fmt.Sprintf("%x", h[:8])
}
