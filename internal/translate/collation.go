package translate

import "strings"

// mapMySQLColumnCollation maps a MySQL/MariaDB column-level collation (and
// optional CHARSET) to a PG collation identifier ready for inclusion in DDL.
// Returns (pgCollation, note). When pgCollation is empty the source
// collation has no safe PG equivalent and the note explains the behavior
// difference for the wizard. When non-empty, pgCollation already includes
// the surrounding double-quotes (PG requires quoting collation names).
//
// Strategy:
//   - `*_bin` → `"C"`  (byte-wise comparison, always present in PG)
//   - case-insensitive collations (`*_ci`) → no PG mapping; user is
//     advised to switch to CITEXT or to install the matching ICU collation
//   - any other collation → preserved verbatim with a quoting attempt;
//     surfaced as a note so DDL execution either succeeds (if the same name
//     exists in PG, e.g. ICU collations) or fails loudly with the user
//     already informed.
func mapMySQLColumnCollation(coll, charset string) (pgColl, note string) {
	c := strings.ToLower(strings.TrimSpace(coll))
	switch {
	case c == "":
		// Charset-only — nothing to enforce at column level in PG.
		if charset != "" {
			return "", "MySQL CHARSET=" + charset + " is per-column in MySQL; PG charsets are per-database. Source CHARSET preserved as a note only."
		}
		return "", ""
	case strings.HasSuffix(c, "_bin"):
		return `"C"`, "MySQL " + coll + " (byte-wise) → PG `\"C\"` collation; ordering and equality are byte-wise on both sides."
	case strings.HasSuffix(c, "_ci"):
		return "", "MySQL " + coll + " is case-insensitive; PG has no built-in case-insensitive text collation. Use CITEXT (extension) or an ICU collation like `\"und-x-icu\"` if your PG has the icu_provider linked."
	}
	// Unknown collation — keep verbatim, double-quoted, and warn.
	return `"` + coll + `"`, "MySQL collation " + coll + " preserved literally; ensure the same name exists in PG (DDL will fail otherwise)."
}

func collationSource(coll, charset string) string {
	switch {
	case coll != "" && charset != "":
		return "CHARSET=" + charset + " COLLATE " + coll
	case coll != "":
		return "COLLATE " + coll
	case charset != "":
		return "CHARSET=" + charset
	}
	return ""
}

func collationTarget(pgColl string) string {
	if pgColl == "" {
		return "(note only)"
	}
	return "COLLATE " + pgColl
}
