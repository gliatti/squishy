package dataxfer

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// convertValue maps a raw MySQL driver value into a PG-friendly Go value.
// The go-sql-driver returns []byte for most scalars; we upcast strings, detect
// JSON, and coerce tinyint(1) where we can (the driver gives us int64 anyway).
func convertValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		// JSON columns come through as []byte; keep as string — pgx converts
		// to JSONB when the target column is jsonb.
		return string(x)
	case string:
		return x
	case int64, float64, bool:
		return x
	case time.Time:
		return x
	case json.RawMessage:
		return string(x)
	}
	return v
}

// coerceFrom applies the fixes a given source driver needs before the
// generic, target-type driven coerce(). kind is SourceDialect.Kind().
func coerceFrom(kind string, v any, udt string) any {
	if kind == "sqlserver" {
		v = sqlserverValue(v, udt)
	}
	return coerce(v, udt)
}

// sqlserverValue normalizes the go-mssqldb values that coerce() cannot
// interpret on its own. What the driver hands back (types.go, read
// functions of go-mssqldb v1.11):
//   - uniqueidentifier: a 16-byte []byte in SQL Server's mixed-endian
//     wire order (first three groups little-endian) unless the DSN sets
//     guid conversion. Formatting those bytes naively gives a different
//     UUID, so a 16-byte value bound for a PG uuid column goes through
//     mssql.UniqueIdentifier, whose Scan undoes the byte swap.
//   - money / smallmoney / decimal / numeric: []byte holding the decimal
//     text ("12.3400") — coerce() passes it to numeric as a string.
//   - bit: bool; tinyint..bigint: int64; float / real: float64.
//   - date, time, datetime, datetime2, smalldatetime: time.Time (UTC
//     location); datetimeoffset: time.Time carrying its offset — pgx
//     encodes all of them for date / time / timestamp / timestamptz.
//   - char/varchar/nchar/nvarchar/text/ntext/xml: string;
//     binary/varbinary/image: []byte.
func sqlserverValue(v any, udt string) any {
	if udt == "geography" || udt == "geometry" {
		if b, ok := v.([]byte); ok {
			if ewkb, ok := sridWKBToEWKB(b); ok {
				return ewkb
			}
		}
		return v
	}
	if udt != "uuid" {
		return v
	}
	b, ok := v.([]byte)
	if !ok || len(b) != 16 {
		return v
	}
	var u mssql.UniqueIdentifier
	if err := u.Scan(b); err != nil {
		return v
	}
	// UniqueIdentifier.String() is upper-case; PG prints uuids lower-case,
	// so emit that canonical form (one UUID value, not SQL text).
	return strings.ToLower(u.String())
}

// sridWKBToEWKB turns the spatial value the SQL Server batch SELECT reads
// for a PostGIS column — its SRID as 4 big-endian bytes, then its WKB
// (sqlserverSelectExpr) — into PostGIS EWKB, the binary input of
// geography / geometry: the WKB with the SRID flag (0x20000000) set in
// its geometry type and the SRID inserted after it, both in the WKB's own
// byte order. Only the outermost geometry carries the SRID (EWKB rule);
// ISO type codes for Z / M (1001, 2001, 3001, …), which AsBinaryZM
// emits, are kept: PostGIS reads them alongside the SRID flag. A value
// too short to hold a WKB header is not converted.
func sridWKBToEWKB(b []byte) ([]byte, bool) {
	if len(b) < 4+5 {
		return nil, false
	}
	srid := binary.BigEndian.Uint32(b[:4])
	wkb := b[4:]
	var order interface {
		binary.ByteOrder
		binary.AppendByteOrder
	}
	switch wkb[0] {
	case 0:
		order = binary.BigEndian
	case 1:
		order = binary.LittleEndian
	default:
		return nil, false
	}
	typ := order.Uint32(wkb[1:5]) | 0x20000000
	out := make([]byte, 0, len(wkb)+4)
	out = append(out, wkb[0])
	out = order.AppendUint32(out, typ)
	out = order.AppendUint32(out, srid)
	out = append(out, wkb[5:]...)
	return out, true
}
