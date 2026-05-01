package dataxfer

import (
	"encoding/json"
	"time"
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
