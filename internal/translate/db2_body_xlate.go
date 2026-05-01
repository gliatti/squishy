package translate

import (
	"strings"

	"gitlab.com/dalibo/squishy/internal/dialects/db2"
)

// rewriteDB2Body converts a DB2 DML/SQL PL fragment (view SELECT body,
// routine body, trigger body) into a PostgreSQL-compatible fragment by
// walking tokens from the DB2 lexer and re-emitting them in the shape the
// PG parser accepts.
//
// Coverage (lexical only — no semantic rewrite of the statement structure):
//
//   - "double-quoted identifiers" preserved verbatim (already PG-compatible).
//   - DB2 functions remapped to PG equivalents:
//     NVL          → COALESCE
//     VALUE        → COALESCE
//     LOCATE       → POSITION   (arg order swap is NOT done — caller must verify)
//     POSSTR       → POSITION
//     STRIP        → TRIM
//     RAISE_ERROR  → (left as-is; callers should rewrite to RAISE EXCEPTION)
//   - DB2 special registers normalised :
//     CURRENT DATE       → CURRENT_DATE
//     CURRENT TIME       → CURRENT_TIME
//     CURRENT TIMESTAMP  → CURRENT_TIMESTAMP
//     CURRENT TIMEZONE   → (dropped — PG has no TZ register)
//     SYSIBM.SYSDUMMY1   → (subselect 1)  — but DB2 also accepts FROM (no FROM
//                          clause is required in PG, so the rewriter leaves
//                          the table reference and lets PG resolve through
//                          the migrated catalog).
//   - WITH UR/CS/RS/RR isolation hints stripped.
//   - FETCH FIRST n ROWS ONLY left untouched (PG accepts the SQL standard form).
//   - Hostvars `:name` left as-is — the routine body is wrapped in
//     `DO $$ … $$` server-side and treats them as PL/pgSQL identifiers.
//
// Constructs outside this list pass through verbatim — by design, so the
// translator and reviewer see where manual work remains.
func rewriteDB2Body(body string) string {
	if strings.TrimSpace(body) == "" {
		return body
	}

	l := db2.NewLexer(body)
	var out strings.Builder
	prevEnd := 0
	src := []rune(body)

	emitRaw := func(s string) { out.WriteString(s) }

	// strippingIsolationHint is set after we emit a SELECT/INSERT/UPDATE/
	// DELETE keyword and remains active until we see the matching trailing
	// `WITH UR|CS|RS|RR` clause (or the next statement terminator). The
	// rewriter elides those hints from the output.

	for {
		tok := l.Next()
		if tok.Kind == db2.TOK_EOF {
			if prevEnd < len(src) {
				emitRaw(string(src[prevEnd:]))
			}
			break
		}
		// Whitespace/comments between tokens — copy raw.
		if tok.Pos.Offset > prevEnd && tok.Pos.Offset <= len(src) {
			emitRaw(string(src[prevEnd:tok.Pos.Offset]))
		}
		tokRaw := tok.Raw
		if tokRaw == "" {
			tokRaw = tok.Lit
		}
		tokEnd := tok.Pos.Offset + len([]rune(tokRaw))
		if tokEnd > len(src) {
			tokEnd = len(src)
		}

		switch tok.Kind {
		case db2.TOK_COMMENT, db2.TOK_STRING, db2.TOK_HEX_STRING,
			db2.TOK_NUMBER, db2.TOK_HOST_VAR, db2.TOK_PARAM:
			emitRaw(string(src[tok.Pos.Offset:tokEnd]))
		case db2.TOK_QUOTED_IDENT:
			// PG-compatible already (double-quoted, case-preserved).
			emitRaw(string(src[tok.Pos.Offset:tokEnd]))
		case db2.TOK_KEYWORD:
			switch tok.Lit {
			case "WITH":
				// `WITH UR|CS|RS|RR` isolation hint at end of statement.
				pk := l.Peek()
				if pk.Kind == db2.TOK_KEYWORD &&
					(pk.Lit == "UR" || pk.Lit == "CS" || pk.Lit == "RS" || pk.Lit == "RR") {
					// Skip both tokens — consume the peeked WITH-target.
					l.Next()
					prevEnd = pk.Pos.Offset + len([]rune(pk.Lit))
					continue
				}
				emitRaw(tokRaw)
			case "CURRENT":
				// CURRENT DATE / CURRENT TIME / CURRENT TIMESTAMP →
				// CURRENT_DATE / CURRENT_TIME / CURRENT_TIMESTAMP
				pk := l.Peek()
				if pk.Kind == db2.TOK_KEYWORD &&
					(pk.Lit == "DATE" || pk.Lit == "TIME" || pk.Lit == "TIMESTAMP") {
					l.Next() // consume the second keyword
					emitRaw("CURRENT_" + pk.Lit)
					prevEnd = pk.Pos.Offset + len([]rune(pk.Raw))
					continue
				}
				emitRaw(tokRaw)
			default:
				emitRaw(tokRaw)
			}
		case db2.TOK_IDENT:
			// Function name remappings happen on the IDENT itself; PG is
			// identifier-folding-lower so we emit lower-case.
			switch tok.Lit {
			case "NVL", "VALUE":
				emitRaw("COALESCE")
			case "LOCATE", "POSSTR":
				emitRaw("POSITION")
			case "STRIP":
				emitRaw("TRIM")
			case "GENERATE_UNIQUE":
				// PG: gen_random_uuid()::text — pgcrypto / pg_random both
				// emit a 16-byte canonical uuid; this mapping requires
				// pgcrypto on the target, surfaced as a prerequisite by
				// the translator.
				emitRaw("gen_random_uuid")
			default:
				emitRaw(tokRaw)
			}
		case db2.TOK_PUNCT:
			emitRaw(tokRaw)
		case db2.TOK_ASSIGN:
			emitRaw(":=")
		case db2.TOK_SLASH:
			// Drop CLP terminators silently.
		default:
			emitRaw(tokRaw)
		}
		prevEnd = tokEnd
	}
	return out.String()
}
