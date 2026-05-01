package dataxfer

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CopyOpts describes a single batch copy to perform.
type CopyOpts struct {
	SourceDB      *sql.DB
	SourceDialect SourceDialect // MySQL / Oracle / …; defaults to MySQL when nil
	TargetPool    *pgxpool.Pool

	SrcSchema string
	SrcTable  string

	DstSchema string
	DstTable  string

	// Columns to transfer. When nil, the copier introspects the source table
	// and transfers all columns in ordinal order.
	Columns []string

	// One of RangeBy{PK,Offset} must be set.
	RangeKind string // "pk" | "offset"
	Low       map[string]any
	High      map[string]any
}

func (o *CopyOpts) dialect() SourceDialect {
	if o.SourceDialect != nil {
		return o.SourceDialect
	}
	return MySQLSource()
}

// normalizeDstIdent mirrors translate.normalizeOracleIdent: for Oracle
// sources, all-caps idents fold to lowercase (so PG-side unquoted names
// match the translator's output); mixed-case idents round-trip verbatim.
// `#` (legal in Oracle, rejected by PG outside double quotes) is also
// replaced with `_` here so the COPY target name agrees with what the
// DDL translator emits for the same table — must stay in lockstep with
// translate.normalizeOracleIdent.
// Non-Oracle sources get the identifier untouched.
func normalizeDstIdent(kind, s string) string {
	if kind != "oracle" || s == "" {
		return s
	}
	hasLower, hasUpper := false, false
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
	}
	out := s
	if hasUpper && !hasLower {
		b := make([]byte, 0, len(s))
		for _, r := range s {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			b = append(b, byte(r))
		}
		out = string(b)
	}
	if strings.ContainsRune(out, '#') {
		out = strings.ReplaceAll(out, "#", "_")
	}
	return out
}

// CopyBatch streams the rows matching the given range from MySQL into PG.
// It introspects the PG target column types, DELETEs the target range first
// (idempotency), then COPY FROM STDIN with coerced values.
func CopyBatch(ctx context.Context, o CopyOpts) (int64, error) {
	if len(o.Columns) == 0 {
		cols, err := listSourceColumns(ctx, o.SourceDB, o.dialect(), o.SrcSchema, o.SrcTable)
		if err != nil {
			return 0, err
		}
		o.Columns = cols
	}

	// Source-side columns (used in the Oracle/MySQL SELECT) keep their
	// original case; PG-side columns need to match the names the translator
	// emitted (all-caps Oracle idents get folded to lowercase).
	pgCols := make([]string, len(o.Columns))
	for i, c := range o.Columns {
		pgCols[i] = normalizeDstIdent(o.dialect().Kind(), c)
	}

	// Introspect target types once for this batch — used to coerce values.
	targetTypes, err := listTargetTypes(ctx, o.TargetPool, o.DstSchema, o.DstTable, pgCols)
	if err != nil {
		return 0, fmt.Errorf("target types: %w", err)
	}

	if err := o.deleteTargetRange(ctx); err != nil {
		return 0, err
	}

	rows, err := o.selectSource(ctx)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	src := &sqlRowSource{rows: rows, cols: o.Columns, targetTypes: targetTypes}
	n, err := o.TargetPool.CopyFrom(ctx,
		pgx.Identifier{o.DstSchema, o.DstTable}, pgCols, src)
	if err != nil {
		return 0, fmt.Errorf("COPY FROM STDIN: %w", err)
	}
	if src.err != nil {
		return n, src.err
	}
	return n, nil
}

func (o *CopyOpts) deleteTargetRange(ctx context.Context) error {
	if o.RangeKind == "pk" {
		for col, lowV := range o.Low {
			highV := o.High[col]
			pgCol := normalizeDstIdent(o.dialect().Kind(), col)
			_, err := o.TargetPool.Exec(ctx,
				fmt.Sprintf(`DELETE FROM %q.%q WHERE %q BETWEEN $1 AND $2`,
					o.DstSchema, o.DstTable, pgCol),
				lowV, highV)
			return err
		}
	}
	return nil
}

func (o *CopyOpts) selectSource(ctx context.Context) (*sql.Rows, error) {
	d := o.dialect()
	switch o.RangeKind {
	case "pk":
		for col, lowV := range o.Low {
			highV := o.High[col]
			q := d.SelectRangeQuery(o.SrcSchema, o.SrcTable, o.Columns, col)
			return o.SourceDB.QueryContext(ctx, q, lowV, highV)
		}
	case "offset":
		off := o.Low["offset"]
		lim := o.High["limit"]
		q := d.SelectOffsetQuery(o.SrcSchema, o.SrcTable, o.Columns)
		// Argument order is dialect-specific: MySQL's LIMIT ? OFFSET ? wants
		// (limit, offset); Oracle's OFFSET :1 ROWS FETCH NEXT :2 ROWS wants
		// (offset, limit). Encode this once in the dialect and let the
		// caller see a consistent (offset, limit) pair.
		if d.Kind() == "oracle" {
			return o.SourceDB.QueryContext(ctx, q, off, lim)
		}
		return o.SourceDB.QueryContext(ctx, q, lim, off)
	}
	return nil, fmt.Errorf("unknown RangeKind %q", o.RangeKind)
}

// listTargetTypes returns the PG udt_name of each column in the same order as
// `cols`. Missing columns (renamed/dropped upstream) map to "".
func listTargetTypes(ctx context.Context, pool *pgxpool.Pool, schema, table string, cols []string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT column_name, udt_name
		  FROM information_schema.columns
		 WHERE table_schema=$1 AND table_name=$2`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]string{}
	for rows.Next() {
		var n, udt string
		if err := rows.Scan(&n, &udt); err != nil {
			return nil, err
		}
		byName[n] = strings.ToLower(udt)
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = byName[c]
	}
	return out, nil
}

// sqlRowSource adapts *sql.Rows to pgx.CopyFromSource. Values are coerced to
// the Go types pgx's binary COPY protocol expects, based on the target column
// udt_name (see coerce()).
type sqlRowSource struct {
	rows        *sql.Rows
	cols        []string
	targetTypes []string

	cur []any
	err error
}

func (s *sqlRowSource) Next() bool {
	if !s.rows.Next() {
		return false
	}
	ptrs := make([]any, len(s.cols))
	vals := make([]any, len(s.cols))
	for i := range ptrs {
		ptrs[i] = &vals[i]
	}
	if err := s.rows.Scan(ptrs...); err != nil {
		s.err = err
		return false
	}
	for i, v := range vals {
		udt := ""
		if i < len(s.targetTypes) {
			udt = s.targetTypes[i]
		}
		vals[i] = coerce(v, udt)
	}
	s.cur = vals
	return true
}

func (s *sqlRowSource) Values() ([]any, error) { return s.cur, nil }
func (s *sqlRowSource) Err() error             { return s.err }

// coerce maps a raw MySQL driver value to the Go type expected by pgx for the
// given PG column type (udt_name, lowercase). Returns nil untouched.
//
// Known udt_name values include: bool, int2, int4, int8, numeric, float4,
// float8, text, varchar, bpchar, bytea, date, time, timetz, timestamp,
// timestamptz, jsonb, json, uuid, bit, varbit, _text (TEXT[]).
func coerce(v any, udt string) any {
	if v == nil {
		return nil
	}
	switch udt {
	case "bool":
		return toBool(v)
	case "bytea":
		return toBytes(v)
	case "text", "varchar", "bpchar", "char", "citext":
		return stripNUL(toString(v))
	case "jsonb", "json":
		return stripNUL(toString(v))
	case "time":
		return normalizeTime(v)
	case "uuid":
		return toString(v)
	case "numeric":
		return toString(v)
	case "_text":
		return toTextArray(v)
	case "bit", "varbit":
		return toBitString(v)
	case "int2":
		return toIntClamp(v, -32768, 32767)
	case "int4":
		return toIntClamp(v, -2147483648, 2147483647)
	case "int8":
		return toIntClamp(v, -9223372036854775808, 9223372036854775807)
	case "interval":
		return toPGInterval(v)
	case "xml":
		return toString(v)
	}
	// float4/float8/date/timestamp/timestamptz — passthrough
	switch x := v.(type) {
	case []byte:
		return string(x)
	default:
		return x
	}
}

// toIntClamp converts a MySQL integer-ish value to int64 and returns nil if
// it doesn't fit the target PG integer range. This handles UNSIGNED overflow
// (BIGINT UNSIGNED max = 2^64-1, exceeds PG BIGINT's int8 range).
func toIntClamp(v any, lo, hi int64) any {
	var n int64
	var err error
	switch x := v.(type) {
	case nil:
		return nil
	case int64:
		n = x
	case int:
		n = int64(x)
	case int32:
		n = int64(x)
	case uint64:
		if x > uint64(hi) {
			return nil
		}
		n = int64(x)
	case float64:
		if x < float64(lo) || x > float64(hi) {
			return nil
		}
		n = int64(x)
	case []byte:
		n, err = parseInt64(string(x))
		if err != nil {
			return nil
		}
	case string:
		n, err = parseInt64(x)
		if err != nil {
			return nil
		}
	default:
		return v
	}
	if n < lo || n > hi {
		return nil
	}
	return n
}

// parseInt64 handles values that exceed int64 range (e.g. "18446744073709551615")
// by returning an error so the caller can coerce to NULL.
func parseInt64(s string) (int64, error) {
	s = strings.TrimSpace(s)
	return strconv.ParseInt(s, 10, 64)
}

// stripNUL removes \x00 bytes that PG text types refuse. Keeps other control
// characters intact.
func stripNUL(v any) any {
	switch x := v.(type) {
	case string:
		if strings.IndexByte(x, 0) < 0 {
			return x
		}
		return strings.ReplaceAll(x, "\x00", "")
	}
	return v
}

// toTextArray splits a MySQL SET value ("a,b,c") into a Go []string, which
// pgx encodes as PG TEXT[].
func toTextArray(v any) any {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return nil
	}
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

// toBitString converts a MySQL BIT value (raw bytes, big-endian) into a PG
// bit string literal. PG expects a string of '0'/'1' of exact length matching
// the bit(N) declared width. For simplicity we return the raw bytes as a
// minimal-width string and rely on PG to zero-extend on insert; if widths
// don't match, PG complains, so we left-pad to a multiple of 8.
func toBitString(v any) any {
	var raw []byte
	switch x := v.(type) {
	case []byte:
		raw = x
	case string:
		raw = []byte(x)
	case int64:
		// small BIT(n<=8) values arrive as int64 sometimes
		if x == 0 {
			return "0"
		}
		return strconv.FormatInt(x, 2)
	default:
		return v
	}
	if len(raw) == 0 {
		return "0"
	}
	var b strings.Builder
	for _, by := range raw {
		for bit := 7; bit >= 0; bit-- {
			if by&(1<<uint(bit)) != 0 {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
	}
	return b.String()
}

func toBool(v any) any {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case []byte:
		s := strings.TrimSpace(string(x))
		if s == "" {
			return false
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n != 0
		}
		switch strings.ToLower(s) {
		case "t", "true", "y", "yes":
			return true
		}
		return false
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n != 0
		}
		return false
	}
	return false
}

func toBytes(v any) any {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	}
	return nil
}

func toString(v any) any {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		// If the bytes aren't valid UTF-8 (e.g. raw BIT/BINARY data routed
		// into a text target), emit them as a '0'/'1' bit string — lossless
		// and safe for PG text columns.
		if !isValidUTF8(x) {
			return bytesToBitString(x)
		}
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(x)
	}
}

func isValidUTF8(b []byte) bool { return utf8.Valid(b) }

func bytesToBitString(raw []byte) string {
	var b strings.Builder
	for _, by := range raw {
		for bit := 7; bit >= 0; bit-- {
			if by&(1<<uint(bit)) != 0 {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
	}
	return b.String()
}

// normalizeTime converts a MySQL TIME value (which can be negative or >24h)
// into a PG TIME-compatible string. Out-of-range values are returned as nil
// (NULL in PG) to avoid failing the whole batch.
func normalizeTime(v any) any {
	var s string
	switch x := v.(type) {
	case []byte:
		s = string(x)
	case string:
		s = x
	default:
		return v
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// PG TIME does not accept negative or >24h values.
	if strings.HasPrefix(s, "-") {
		return nil
	}
	parts := strings.Split(s, ":")
	if len(parts) >= 1 {
		if h, err := strconv.Atoi(parts[0]); err == nil && (h < 0 || h > 23) {
			return nil
		}
	}
	return s
}

// toPGInterval parses an Oracle interval literal into a pgtype.Interval.
// Oracle emits two shapes:
//   "+YY-MM"                       (INTERVAL YEAR TO MONTH)
//   "+DD HH24:MI:SS[.FFFFFF]"      (INTERVAL DAY TO SECOND)
// The sign prefix is optional; "-" flips every component. Returns nil on
// NULL and falls back to string on an unrecognized shape so pgx at least
// has a chance via the text codec.
func toPGInterval(v any) any {
	var s string
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return v
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	neg := false
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	// YEAR TO MONTH:  YY-MM
	if idx := strings.IndexByte(s, '-'); idx > 0 && !strings.ContainsAny(s, " :") {
		y, _ := strconv.Atoi(s[:idx])
		m, _ := strconv.Atoi(s[idx+1:])
		months := int32(y*12 + m)
		if neg {
			months = -months
		}
		return pgtype.Interval{Months: months, Valid: true}
	}
	// DAY TO SECOND:  DD HH:MM:SS[.FFFFFF]
	if idx := strings.IndexByte(s, ' '); idx > 0 {
		days, _ := strconv.Atoi(s[:idx])
		tail := s[idx+1:]
		frac := ""
		if dot := strings.IndexByte(tail, '.'); dot >= 0 {
			frac = tail[dot+1:]
			tail = tail[:dot]
		}
		parts := strings.Split(tail, ":")
		var h, m, sec int
		if len(parts) > 0 {
			h, _ = strconv.Atoi(parts[0])
		}
		if len(parts) > 1 {
			m, _ = strconv.Atoi(parts[1])
		}
		if len(parts) > 2 {
			sec, _ = strconv.Atoi(parts[2])
		}
		micros := int64(h)*3600_000_000 + int64(m)*60_000_000 + int64(sec)*1_000_000
		if frac != "" {
			// Left-pad / truncate to 6 digits of microseconds.
			if len(frac) > 6 {
				frac = frac[:6]
			} else if len(frac) < 6 {
				frac = frac + strings.Repeat("0", 6-len(frac))
			}
			if f, err := strconv.ParseInt(frac, 10, 64); err == nil {
				micros += f
			}
		}
		if neg {
			days = -days
			micros = -micros
		}
		return pgtype.Interval{Days: int32(days), Microseconds: micros, Valid: true}
	}
	return s
}
