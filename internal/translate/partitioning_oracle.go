package translate

import (
	"fmt"
	"strconv"
	"strings"
)

// oraclePartNotes records advanced-form features detected in the raw clause
// that the lift-to-PG path could not preserve verbatim. The translator turns
// each non-zero field into a Warning + Prerequisite so the user knows exactly
// what semantic was dropped during the downgrade.
type oraclePartNotes struct {
	HasInterval         bool
	IntervalExpr        string // raw `INTERVAL (…)` body, kept for the explanation
	HasAutomaticList    bool
	HasSubpartitioning  bool
	SubpartitionMethod  string // "RANGE" / "LIST" / "HASH"
	HasSubpartTemplate  bool
	HasReference        bool
	ReferenceConstraint string
	HasSystem           bool
}

// parseOraclePartitioning lifts Oracle's `PARTITION BY {RANGE|LIST|HASH} (col)`
// clause (captured raw by the dialect parser) into a PG-shaped PGPartitioning.
//
// Advanced forms (INTERVAL, AUTOMATIC LIST, composite SUBPARTITION BY,
// SUBPARTITION TEMPLATE, STORE IN tablespace lists, per-partition tablespace
// /storage trailers) are detected, recorded in the returned notes, and
// stripped from the working copy before falling through to the simple
// RANGE/LIST/HASH parser. REFERENCE and SYSTEM partitioning have no PG
// equivalent — the function returns (nil, notes, nil) so the translator can
// emit a blocking prerequisite and fall back to an unpartitioned table.
//
// Supported simple input shapes (what DBMS_METADATA emits, plus the sugar
// form for HASH that Oracle developers write by hand):
//
//	-- RANGE
//	PARTITION BY RANGE ("TIME_ID")
//	(PARTITION "P1" VALUES LESS THAN (TO_DATE(' 2019-01-01 00:00:00', '...', '...')),
//	 PARTITION "P2" VALUES LESS THAN (MAXVALUE));
//
//	-- LIST
//	PARTITION BY LIST ("REGION")
//	(PARTITION "P_EU"  VALUES ('FR','DE','IT'),
//	 PARTITION "P_OTH" VALUES (DEFAULT));
//
//	-- HASH (developer sugar)
//	PARTITION BY HASH ("CUST_ID") PARTITIONS 4
//
//	-- HASH (DBMS_METADATA expanded form)
//	PARTITION BY HASH ("CUST_ID")
//	(PARTITION "P1", PARTITION "P2", PARTITION "P3", PARTITION "P4")
func parseOraclePartitioning(raw string) (*PGPartitioning, oraclePartNotes, error) {
	var notes oraclePartNotes

	// Preflight: REFERENCE and SYSTEM partitioning have no PG counterpart;
	// surface them in the notes and bail out so the translator emits a
	// blocking prerequisite. The header parser below would not recognise
	// them either, so this also keeps the error path quiet.
	if name, ok := matchOraclePartReference(raw); ok {
		notes.HasReference = true
		notes.ReferenceConstraint = strings.TrimSpace(stripQuotes(name))
		return nil, notes, nil
	}
	if matchOraclePartSystem(raw) {
		notes.HasSystem = true
		return nil, notes, nil
	}

	method, colsRaw, headerLen, ok := matchOraclePartHeader(raw)
	if !ok {
		return nil, notes, fmt.Errorf("unrecognized header (expected `PARTITION BY {RANGE|LIST|HASH} (col)`)")
	}
	cols := splitPartitionCols(colsRaw)
	if len(cols) == 0 {
		return nil, notes, fmt.Errorf("no partition column extracted from %q", colsRaw)
	}

	out := &PGPartitioning{Method: method, Columns: cols}

	// Body is everything after the header. Apply the advanced-form sweep
	// before any parsing so the rest of the function only deals with the
	// simple skeleton.
	body := strings.TrimSpace(raw[headerLen:])
	body, notes = stripOracleAdvancedPartitioning(body, method, notes)
	body = strings.TrimSpace(body)

	if method == "HASH" {
		if cnt, ok := matchOracleHashCount(body); ok {
			n, err := strconv.Atoi(cnt)
			if err != nil || n <= 0 {
				return nil, notes, fmt.Errorf("invalid HASH partition count %q", cnt)
			}
			for i := 0; i < n; i++ {
				out.Partitions = append(out.Partitions, PGPartition{
					Name:      fmt.Sprintf("p_h%d", i),
					Modulus:   n,
					Remainder: i,
				})
			}
			return out, notes, nil
		}
	}

	// Otherwise: a parenthesised list of partition definitions.
	body = strings.TrimPrefix(body, "(")
	if strings.HasSuffix(body, ")") {
		body = body[:len(body)-1]
	}
	parts, err := splitTopLevel(body, ',')
	if err != nil {
		return nil, notes, fmt.Errorf("split partition list: %w", err)
	}
	if len(parts) == 0 {
		return nil, notes, fmt.Errorf("no partitions found")
	}

	switch method {
	case "RANGE":
		for _, raw := range parts {
			p, err := parseRangePartition(raw)
			if err != nil {
				return nil, notes, err
			}
			out.Partitions = append(out.Partitions, p)
		}
	case "LIST":
		for _, raw := range parts {
			p, err := parseListPartition(raw)
			if err != nil {
				return nil, notes, err
			}
			out.Partitions = append(out.Partitions, p)
		}
	case "HASH":
		// Explicit list form: PARTITION p1, PARTITION p2, …. Oracle stores
		// no explicit MODULUS/REMAINDER; assign in source order so the
		// HASH function's distribution stays stable.
		n := len(parts)
		for i, raw := range parts {
			name, err := parseHashPartitionName(raw)
			if err != nil {
				return nil, notes, err
			}
			out.Partitions = append(out.Partitions, PGPartition{
				Name:      name,
				Modulus:   n,
				Remainder: i,
			})
		}
	default:
		return nil, notes, fmt.Errorf("unsupported partitioning method %q", method)
	}
	return out, notes, nil
}

// matchOraclePartHeader recognises the leading
//   PARTITION BY {RANGE|LIST|HASH} (col [, col]*)
// of an Oracle partitioning clause. Returns the upper-case method, the raw
// column list (interior of the parens, untrimmed), and the byte length
// consumed (so the caller can advance past it). Replaces the regex
//   (?is)^\s*PARTITION\s+BY\s+(RANGE|LIST|HASH)\s*\(([^)]*)\)\s*
func matchOraclePartHeader(s string) (method, cols string, n int, ok bool) {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "PARTITION") {
		return
	}
	p += len("PARTITION")
	p = skipWS(s, p)
	if !startsKeywordCI(s, p, "BY") {
		return
	}
	p += len("BY")
	p = skipWS(s, p)
	switch {
	case startsKeywordCI(s, p, "RANGE"):
		method = "RANGE"
		p += len("RANGE")
	case startsKeywordCI(s, p, "LIST"):
		method = "LIST"
		p += len("LIST")
	case startsKeywordCI(s, p, "HASH"):
		method = "HASH"
		p += len("HASH")
	default:
		return
	}
	p = skipWS(s, p)
	if p >= len(s) || s[p] != '(' {
		return
	}
	close := strings.IndexByte(s[p:], ')')
	if close < 0 {
		return
	}
	cols = s[p+1 : p+close]
	p += close + 1
	p = skipWS(s, p)
	return method, cols, p, true
}

// matchOraclePartReference recognises `PARTITION BY REFERENCE (constraint)`.
// Returns the constraint expression as-is and ok=true on match.
func matchOraclePartReference(s string) (string, bool) {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "PARTITION") {
		return "", false
	}
	p += len("PARTITION")
	p = skipWS(s, p)
	if !startsKeywordCI(s, p, "BY") {
		return "", false
	}
	p += len("BY")
	p = skipWS(s, p)
	if !startsKeywordCI(s, p, "REFERENCE") {
		return "", false
	}
	p += len("REFERENCE")
	p = skipWS(s, p)
	if p >= len(s) || s[p] != '(' {
		return "", false
	}
	close := strings.IndexByte(s[p:], ')')
	if close < 0 {
		return "", false
	}
	return s[p+1 : p+close], true
}

// matchOraclePartSystem reports whether s starts (after optional whitespace)
// with `PARTITION BY SYSTEM` as a whole-word phrase.
func matchOraclePartSystem(s string) bool {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "PARTITION") {
		return false
	}
	p += len("PARTITION")
	p = skipWS(s, p)
	if !startsKeywordCI(s, p, "BY") {
		return false
	}
	p += len("BY")
	p = skipWS(s, p)
	return startsKeywordCI(s, p, "SYSTEM")
}

// matchOracleHashCount recognises a `PARTITIONS N` body anchored at start
// with only trailing whitespace allowed. Replaces (?is)^PARTITIONS\s+(\d+)\s*$.
func matchOracleHashCount(s string) (string, bool) {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "PARTITIONS") {
		return "", false
	}
	p += len("PARTITIONS")
	ws := skipWS(s, p)
	if ws == p {
		return "", false
	}
	digStart := ws
	for ws < len(s) && s[ws] >= '0' && s[ws] <= '9' {
		ws++
	}
	if ws == digStart {
		return "", false
	}
	dig := s[digStart:ws]
	tail := skipWS(s, ws)
	if tail != len(s) {
		return "", false
	}
	return dig, true
}

// matchOraclePartName extracts the partition name from a segment beginning
// with `PARTITION <name>`. Returns the name and the offset just after it.
// Replaces (?is)^\s*PARTITION\s+([^\s(]+).
func matchOraclePartName(s string) (string, int, bool) {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "PARTITION") {
		return "", 0, false
	}
	p += len("PARTITION")
	ws := skipWS(s, p)
	if ws == p {
		return "", 0, false
	}
	start := ws
	for ws < len(s) {
		c := s[ws]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(' {
			break
		}
		ws++
	}
	if ws == start {
		return "", 0, false
	}
	return s[start:ws], ws, true
}

// matchOracleToDate / matchOracleToTimestamp recognise the conversion
// function with a leading single-quoted literal as the first argument and
// optional extra arguments. The function call must span the whole expression
// (no trailing content). The first literal is returned. Replaces
//   (?is)^TO_DATE\s*\(\s*'([^']*)'\s*(?:,[^)]*)?\)\s*$
//   (?is)^TO_TIMESTAMP(?:_TZ)?\s*\(\s*'([^']*)'\s*(?:,[^)]*)?\)\s*$
func matchOracleToDate(s string) (string, bool) {
	return matchOracleConversionFunc(s, []string{"TO_DATE"})
}

func matchOracleToTimestamp(s string) (string, bool) {
	return matchOracleConversionFunc(s, []string{"TO_TIMESTAMP_TZ", "TO_TIMESTAMP"})
}

func matchOracleConversionFunc(s string, names []string) (string, bool) {
	p := skipWS(s, 0)
	matched := ""
	for _, n := range names {
		if startsKeywordCI(s, p, n) {
			matched = n
			break
		}
	}
	if matched == "" {
		return "", false
	}
	p += len(matched)
	p = skipWS(s, p)
	if p >= len(s) || s[p] != '(' {
		return "", false
	}
	end, ok := findMatchingParen(s, p)
	if !ok {
		return "", false
	}
	if skipWS(s, end+1) != len(s) {
		return "", false
	}
	inner := strings.TrimSpace(s[p+1 : end])
	if len(inner) == 0 || inner[0] != '\'' {
		return "", false
	}
	close := strings.IndexByte(inner[1:], '\'')
	if close < 0 {
		return "", false
	}
	return inner[1 : 1+close], true
}

// matchOracleInterval reports whether s matches an Oracle/PG interval literal:
//   INTERVAL '<value>' [UNIT [(prec)] [TO UNIT]]
// with only trailing whitespace allowed. Replaces
//   (?is)^INTERVAL\s+'[^']*'(?:\s+\w+(?:\s*\(\s*\d+\s*\))?(?:\s+TO\s+\w+)?)?\s*$
func matchOracleInterval(s string) bool {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "INTERVAL") {
		return false
	}
	p += len("INTERVAL")
	ws := skipWS(s, p)
	if ws == p || ws >= len(s) || s[ws] != '\'' {
		return false
	}
	closeQ := strings.IndexByte(s[ws+1:], '\'')
	if closeQ < 0 {
		return false
	}
	p = ws + 1 + closeQ + 1
	tail := skipWS(s, p)
	if tail == len(s) {
		return true
	}
	// optional unit identifier
	if !isIdentStart(s[tail]) {
		return false
	}
	uStart := tail
	for tail < len(s) && isIdentByte(s[tail]) {
		tail++
	}
	_ = uStart
	tail = skipWS(s, tail)
	// optional precision (\d+)
	if tail < len(s) && s[tail] == '(' {
		end, ok := findMatchingParen(s, tail)
		if !ok {
			return false
		}
		inner := strings.TrimSpace(s[tail+1 : end])
		for i := 0; i < len(inner); i++ {
			if inner[i] < '0' || inner[i] > '9' {
				return false
			}
		}
		if len(inner) == 0 {
			return false
		}
		tail = end + 1
	}
	tail = skipWS(s, tail)
	if tail == len(s) {
		return true
	}
	// optional TO <unit>
	if !startsKeywordCI(s, tail, "TO") {
		return false
	}
	tail += len("TO")
	tail = skipWS(s, tail)
	if tail >= len(s) || !isIdentStart(s[tail]) {
		return false
	}
	for tail < len(s) && isIdentByte(s[tail]) {
		tail++
	}
	tail = skipWS(s, tail)
	return tail == len(s)
}

// isNumberLiteral reports whether s is a signed integer or fixed-point
// numeric literal — replaces ^-?\d+(?:\.\d+)?$.
func isNumberLiteral(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i = 1
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		digits++
		i++
	}
	if digits == 0 {
		return false
	}
	if i == len(s) {
		return true
	}
	if s[i] != '.' {
		return false
	}
	i++
	frac := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		frac++
		i++
	}
	return frac > 0 && i == len(s)
}

// isSingleQuotedLiteral reports whether s is a single-quoted SQL string
// literal with the standard `''` escape. Replaces ^'(?:[^']|'')*'$.
func isSingleQuotedLiteral(s string) bool {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return false
	}
	i := 1
	for i < len(s)-1 {
		if s[i] == '\'' {
			if i+1 < len(s)-1 && s[i+1] == '\'' {
				i += 2
				continue
			}
			return false
		}
		i++
	}
	return true
}

// stripOracleAdvancedPartitioning removes INTERVAL / AUTOMATIC / STORE IN /
// SUBPARTITION BY clauses from the partition body, recording what it found
// in the notes so the translator can warn intelligently. After this call
// the body is structurally equivalent to a plain RANGE/LIST/HASH clause and
// can flow through the existing parsing logic unchanged.
func stripOracleAdvancedPartitioning(body, method string, notes oraclePartNotes) (string, oraclePartNotes) {
	// 1. INTERVAL (expr)  — RANGE only. We can't keep the auto-creation
	//    semantics in PG, so the existing partitions become a fixed snapshot.
	if loc, ok := findKeywordOutsideParens(body, "INTERVAL"); ok {
		// Skip whitespace then expect '(' for the expression.
		i := loc + len("INTERVAL")
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
			i++
		}
		if i < len(body) && body[i] == '(' {
			end, _ := findMatchingParen(body, i)
			if end > i {
				notes.HasInterval = true
				notes.IntervalExpr = strings.TrimSpace(body[i+1 : end])
				body = body[:loc] + body[end+1:]
			}
		}
	}

	// 2. AUTOMATIC — LIST only.
	if method == "LIST" {
		if loc, ok := findKeywordOutsideParens(body, "AUTOMATIC"); ok {
			notes.HasAutomaticList = true
			body = body[:loc] + body[loc+len("AUTOMATIC"):]
		}
	}

	// 3. STORE IN (ts1, ts2, …) — appears after INTERVAL/AUTOMATIC and after
	//    SUBPARTITIONS N. Strip every occurrence at depth 0.
	for {
		loc, ok := findKeywordOutsideParens(body, "STORE")
		if !ok {
			break
		}
		// Expect `IN (`. Skip optional whitespace + IN keyword.
		j := loc + len("STORE")
		for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r') {
			j++
		}
		if j+2 > len(body) || !strings.EqualFold(body[j:j+2], "IN") {
			// Not the STORE IN clause we expected — bail to avoid an infinite
			// loop. Move past this STORE so subsequent passes can find others.
			body = body[:loc] + " " + body[loc+len("STORE"):]
			continue
		}
		j += 2
		for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r') {
			j++
		}
		if j >= len(body) || body[j] != '(' {
			body = body[:loc] + " " + body[loc+len("STORE"):]
			continue
		}
		end, _ := findMatchingParen(body, j)
		if end <= j {
			break
		}
		body = body[:loc] + body[end+1:]
	}

	// 4. SUBPARTITION BY {RANGE|LIST|HASH} (cols) [SUBPARTITIONS N | SUBPARTITION TEMPLATE (...)]
	//    Composite partitioning: top-level method is preserved; subpartitions
	//    are flattened with a warning.
	if loc, ok := findKeywordOutsideParens(body, "SUBPARTITION"); ok {
		notes.HasSubpartitioning = true
		rest := body[loc:]
		if m, ok := matchOracleSubpartHeader(rest); ok {
			notes.SubpartitionMethod = m
		}
		// The SUBPARTITION BY clause runs until the next top-level '('
		// (which opens the partition-definition list) OR end-of-body.
		end := stripSubpartitionByClause(body, loc)
		body = body[:loc] + body[end:]
	}

	// 5. SUBPARTITION TEMPLATE (…) appearing on its own (not inside the
	//    SUBPARTITION BY just stripped).
	if loc, ok := findKeywordOutsideParens(body, "SUBPARTITION"); ok {
		// Anything left starting with SUBPARTITION at depth 0 must be a
		// stray TEMPLATE clause — strip until matching ')'.
		rest := body[loc:]
		if matchOracleSubpartTemplate(rest) {
			notes.HasSubpartTemplate = true
			pi := strings.Index(rest, "(")
			if pi >= 0 {
				absOpen := loc + pi
				absClose, _ := findMatchingParen(body, absOpen)
				if absClose > absOpen {
					body = body[:loc] + body[absClose+1:]
				}
			}
		}
	}

	// 6. Strip per-partition trailing options/subpartition specs from each
	//    PARTITION segment in the final partition-definition list. We scan
	//    each segment, keep the partition name + optional VALUES (...) bound,
	//    and discard everything that follows.
	body = stripPerPartitionTrailers(body)

	return body, notes
}

// matchOracleSubpartHeader extracts the subpartition method from the head of
// s, expecting `SUBPARTITION BY {RANGE|LIST|HASH}` as a whole-word phrase.
// Replaces (?is)^SUBPARTITION\s+BY\s+(RANGE|LIST|HASH)\b.
func matchOracleSubpartHeader(s string) (string, bool) {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "SUBPARTITION") {
		return "", false
	}
	p += len("SUBPARTITION")
	p = skipWS(s, p)
	if !startsKeywordCI(s, p, "BY") {
		return "", false
	}
	p += len("BY")
	p = skipWS(s, p)
	switch {
	case startsKeywordCI(s, p, "RANGE"):
		return "RANGE", true
	case startsKeywordCI(s, p, "LIST"):
		return "LIST", true
	case startsKeywordCI(s, p, "HASH"):
		return "HASH", true
	}
	return "", false
}

// matchOracleSubpartTemplate reports whether s starts with a stray
// `SUBPARTITION TEMPLATE` clause. Replaces (?is)^SUBPARTITION\s+TEMPLATE\b.
func matchOracleSubpartTemplate(s string) bool {
	p := skipWS(s, 0)
	if !startsKeywordCI(s, p, "SUBPARTITION") {
		return false
	}
	p += len("SUBPARTITION")
	p = skipWS(s, p)
	return startsKeywordCI(s, p, "TEMPLATE")
}

// stripSubpartitionByClause walks `body` from offset `start` (which must
// point at the SUBPARTITION keyword) and returns the offset where the
// clause ends. The clause covers SUBPARTITION BY {METHOD} (cols) optionally
// followed by SUBPARTITIONS N or SUBPARTITION TEMPLATE (...). It stops at
// the next top-level '(' that follows the spec — that paren opens the main
// partition-definition list. Falls back to end-of-body if no such '(' is
// found.
func stripSubpartitionByClause(body string, start int) int {
	// Skip the SUBPARTITION BY (cols) — first balanced (...) at depth 0.
	i := start
	for i < len(body) && body[i] != '(' {
		i++
	}
	if i >= len(body) {
		return len(body)
	}
	end, _ := findMatchingParen(body, i)
	if end <= i {
		return len(body)
	}
	i = end + 1
	// Skip whitespace.
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i++
	}
	// Optional SUBPARTITIONS N
	if upperHasPrefix(body, i, "SUBPARTITIONS") {
		i += len("SUBPARTITIONS")
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		// number
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
		}
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		// Optional STORE IN (...)
		if upperHasPrefix(body, i, "STORE") {
			j := i + len("STORE")
			for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
				j++
			}
			if upperHasPrefix(body, j, "IN") {
				j += len("IN")
				for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
					j++
				}
				if j < len(body) && body[j] == '(' {
					ce, _ := findMatchingParen(body, j)
					if ce > j {
						i = ce + 1
					}
				}
			}
		}
	}
	// Optional SUBPARTITION TEMPLATE (...)
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i++
	}
	if upperHasPrefix(body, i, "SUBPARTITION") {
		j := i + len("SUBPARTITION")
		for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
			j++
		}
		if upperHasPrefix(body, j, "TEMPLATE") {
			j += len("TEMPLATE")
			for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n') {
				j++
			}
			if j < len(body) && body[j] == '(' {
				ce, _ := findMatchingParen(body, j)
				if ce > j {
					i = ce + 1
				}
			}
		}
	}
	return i
}

// stripPerPartitionTrailers walks the parenthesised partition-definition
// list (if any) and rewrites each `PARTITION name VALUES … <trailing>`
// segment to keep only `PARTITION name` and the first balanced (...) after
// VALUES. Trailers like `TABLESPACE foo`, `STORE IN (...)`, `(SUBPARTITION
// sp1 …)` and `SUBPARTITIONS N` are dropped. RANGE partitions without an
// explicit VALUES clause (composite-RANGE shorthand) are left as-is — the
// downstream regex parser will report them.
func stripPerPartitionTrailers(body string) string {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "(") || !strings.HasSuffix(body, ")") {
		return body
	}
	inner := body[1 : len(body)-1]
	parts, err := splitTopLevel(inner, ',')
	if err != nil {
		return body
	}
	if len(parts) == 0 {
		return body
	}
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		out = append(out, trimPartitionSegment(seg))
	}
	return "(" + strings.Join(out, ",") + ")"
}

// trimPartitionSegment shortens one `PARTITION p VALUES …` segment to its
// header-only form. If a VALUES clause is present, we keep the partition's
// name + VALUES + the first balanced (...) and drop the rest. If there's
// no VALUES (e.g. HASH partitions in the explicit list, which are bare
// `PARTITION p`), the segment is returned unchanged.
func trimPartitionSegment(seg string) string {
	upper := strings.ToUpper(seg)
	idx := indexAtDepthZero(upper, "VALUES")
	if idx < 0 {
		return seg
	}
	// Find the first '(' after VALUES at depth 0.
	open := -1
	depth := 0
	inSingle := false
	for i := idx; i < len(seg); i++ {
		c := seg[i]
		switch {
		case inSingle:
			if c == '\'' {
				if i+1 < len(seg) && seg[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		case c == '\'':
			inSingle = true
		case c == '(':
			if depth == 0 {
				open = i
			}
			depth++
		case c == ')':
			depth--
		}
		if open >= 0 && depth == 0 {
			// Found the matching close after the first '('.
			return seg[:i+1]
		}
	}
	return seg
}

// findKeywordOutsideParens returns the first offset at which keyword `kw`
// appears at parenthesis depth 0 (case-insensitive, must be at a word
// boundary). Returns ok=false if not found.
func findKeywordOutsideParens(body, kw string) (int, bool) {
	upper := strings.ToUpper(body)
	depth := 0
	inSingle := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inSingle:
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		case c == '\'':
			inSingle = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		default:
			if depth == 0 && i+len(kw) <= len(body) && upper[i:i+len(kw)] == kw {
				if isWordBoundary(body, i-1) && isWordBoundary(body, i+len(kw)) {
					return i, true
				}
			}
		}
	}
	return -1, false
}

// indexAtDepthZero is a depth-aware variant of strings.Index, case-sensitive
// (caller should pre-upper the haystack if needed).
func indexAtDepthZero(haystack, needle string) int {
	depth := 0
	inSingle := false
	for i := 0; i+len(needle) <= len(haystack); i++ {
		c := haystack[i]
		switch {
		case inSingle:
			if c == '\'' {
				if i+1 < len(haystack) && haystack[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		case c == '\'':
			inSingle = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		default:
			if depth == 0 && haystack[i:i+len(needle)] == needle {
				if isWordBoundary(haystack, i-1) && isWordBoundary(haystack, i+len(needle)) {
					return i
				}
			}
		}
	}
	return -1
}

// findMatchingParen returns the offset of the ')' matching the '(' at
// `open`. On unbalanced input returns (open, false).
func findMatchingParen(body string, open int) (int, bool) {
	if open >= len(body) || body[open] != '(' {
		return open, false
	}
	depth := 0
	inSingle := false
	for i := open; i < len(body); i++ {
		c := body[i]
		switch {
		case inSingle:
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		case c == '\'':
			inSingle = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return open, false
}

func isWordBoundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	c := s[i]
	return !(c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'))
}

func upperHasPrefix(s string, off int, prefix string) bool {
	if off < 0 || off+len(prefix) > len(s) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[off+i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c != prefix[i] {
			return false
		}
	}
	return isWordBoundary(s, off+len(prefix))
}

// parseRangePartition extracts (name, upperBound) from one segment of a
// RANGE partition list. It tolerates trailing per-partition options that
// stripPerPartitionTrailers may not have removed (e.g. unrecognised vendor
// keywords) by extracting the partition name and the first balanced
// `VALUES LESS THAN (...)` paren.
func parseRangePartition(seg string) (PGPartition, error) {
	seg = strings.TrimSpace(seg)
	nm, _, ok := matchOraclePartName(seg)
	if !ok {
		return PGPartition{}, fmt.Errorf("could not parse RANGE partition %q", seg)
	}
	name := strings.ToLower(stripQuotes(nm))

	// Find `VALUES LESS THAN (`.
	upper := strings.ToUpper(seg)
	idx := indexAtDepthZero(upper, "VALUES")
	if idx < 0 {
		return PGPartition{}, fmt.Errorf("partition %s: no VALUES clause", name)
	}
	// Skip past `VALUES LESS THAN`.
	i := idx + len("VALUES")
	for i < len(seg) && (seg[i] == ' ' || seg[i] == '\t' || seg[i] == '\n' || seg[i] == '\r') {
		i++
	}
	if upperHasPrefix(seg, i, "LESS") {
		i += len("LESS")
		for i < len(seg) && (seg[i] == ' ' || seg[i] == '\t' || seg[i] == '\n' || seg[i] == '\r') {
			i++
		}
		if upperHasPrefix(seg, i, "THAN") {
			i += len("THAN")
		}
	}
	for i < len(seg) && (seg[i] == ' ' || seg[i] == '\t' || seg[i] == '\n' || seg[i] == '\r') {
		i++
	}
	if i >= len(seg) || seg[i] != '(' {
		return PGPartition{}, fmt.Errorf("partition %s: expected `(` after VALUES LESS THAN", name)
	}
	end, ok := findMatchingParen(seg, i)
	if !ok {
		return PGPartition{}, fmt.Errorf("partition %s: unbalanced `(` after VALUES LESS THAN", name)
	}
	bodyExpr := strings.TrimSpace(seg[i+1 : end])
	bound, err := translateOracleScalarBound(bodyExpr)
	if err != nil {
		return PGPartition{}, fmt.Errorf("partition %s: %w", name, err)
	}
	return PGPartition{Name: name, UpperBound: bound}, nil
}

func parseListPartition(seg string) (PGPartition, error) {
	seg = strings.TrimSpace(seg)
	nm, _, ok := matchOraclePartName(seg)
	if !ok {
		return PGPartition{}, fmt.Errorf("could not parse LIST partition %q", seg)
	}
	name := strings.ToLower(stripQuotes(nm))

	upper := strings.ToUpper(seg)
	idx := indexAtDepthZero(upper, "VALUES")
	if idx < 0 {
		return PGPartition{}, fmt.Errorf("partition %s: no VALUES clause", name)
	}
	i := idx + len("VALUES")
	for i < len(seg) && (seg[i] == ' ' || seg[i] == '\t' || seg[i] == '\n' || seg[i] == '\r') {
		i++
	}
	if i >= len(seg) || seg[i] != '(' {
		return PGPartition{}, fmt.Errorf("partition %s: expected `(` after VALUES", name)
	}
	end, ok := findMatchingParen(seg, i)
	if !ok {
		return PGPartition{}, fmt.Errorf("partition %s: unbalanced `(` after VALUES", name)
	}
	bodyExpr := strings.TrimSpace(seg[i+1 : end])
	if strings.EqualFold(bodyExpr, "DEFAULT") {
		return PGPartition{Name: name, IsDefault: true}, nil
	}
	rawVals, err := splitTopLevel(bodyExpr, ',')
	if err != nil {
		return PGPartition{}, fmt.Errorf("partition %s: %w", name, err)
	}
	vals := make([]string, 0, len(rawVals))
	for _, v := range rawVals {
		conv, err := translateOracleScalarBound(strings.TrimSpace(v))
		if err != nil {
			return PGPartition{}, fmt.Errorf("partition %s value: %w", name, err)
		}
		vals = append(vals, conv)
	}
	return PGPartition{Name: name, Values: vals}, nil
}

func parseHashPartitionName(seg string) (string, error) {
	seg = strings.TrimSpace(seg)
	nm, _, ok := matchOraclePartName(seg)
	if !ok {
		return "", fmt.Errorf("could not parse HASH partition %q", seg)
	}
	return strings.ToLower(stripQuotes(nm)), nil
}

// translateOracleScalarBound converts an Oracle bound/value expression into
// its PG equivalent. Recognised:
//
//   - MAXVALUE / DEFAULT → keyword preserved (caller decides if applicable)
//   - TO_DATE('lit', '...')      → 'lit'  (first string literal extracted)
//   - TO_TIMESTAMP[_TZ]('lit', …) → 'lit'  (same)
//   - INTERVAL '1' YEAR …        → kept verbatim (PG accepts the literal)
//   - numeric literal             → kept verbatim
//   - 'string literal'            → kept verbatim (single quotes preserved)
//
// Anything else is passed through and let PG's parser report a precise error
// at the right offset.
func translateOracleScalarBound(expr string) (string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", fmt.Errorf("empty bound expression")
	}
	upper := strings.ToUpper(expr)
	if upper == "MAXVALUE" {
		return "MAXVALUE", nil
	}
	if upper == "DEFAULT" {
		return "DEFAULT", nil
	}
	if upper == "MINVALUE" {
		return "MINVALUE", nil
	}
	if lit, ok := matchOracleToDate(expr); ok {
		return "'" + strings.TrimSpace(lit) + "'", nil
	}
	if lit, ok := matchOracleToTimestamp(expr); ok {
		return "'" + strings.TrimSpace(lit) + "'", nil
	}
	if matchOracleInterval(expr) {
		// PG accepts the same literal form.
		return expr, nil
	}
	if isNumberLiteral(expr) {
		return expr, nil
	}
	if isSingleQuotedLiteral(expr) {
		return expr, nil
	}
	// Unknown shape — pass through so PG can flag it. We don't fail here
	// because Oracle bounds may include vendor functions we haven't seen
	// (e.g. sysdate offsets) that PG rejects with a clearer message.
	return expr, nil
}

// splitPartitionCols splits the column list inside the header parens,
// stripping quotes and lowercasing Oracle uppercase identifiers.
func splitPartitionCols(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, strings.ToLower(stripQuotes(p)))
	}
	return out
}

func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// splitTopLevel splits s on `sep` only at parenthesis depth 0. Single-quote
// strings (with `''` escape) are honored so commas inside literals don't
// trigger a split.
func splitTopLevel(s string, sep byte) ([]string, error) {
	var out []string
	depth := 0
	inSingle := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		case c == '\'':
			inSingle = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced parentheses near offset %d", i)
			}
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses (depth=%d)", depth)
	}
	tail := s[start:]
	if strings.TrimSpace(tail) != "" {
		out = append(out, tail)
	}
	return out, nil
}
