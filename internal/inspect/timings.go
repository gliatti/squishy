package inspect

import (
	"encoding/json"
	"time"
)

// InspectTimings captures how long each section of an introspection took and
// how many objects each section produced. It is exposed in the plan response so
// large-schema slowness can be diagnosed without re-running with a profiler.
type InspectTimings struct {
	Total       time.Duration
	BySection   map[string]time.Duration
	ObjectCount map[string]int
}

// NewInspectTimings returns a zero-valued struct with maps allocated. Safe to
// use even when the caller doesn't need timings (the struct is still cheap).
func NewInspectTimings() *InspectTimings {
	return &InspectTimings{
		BySection:   map[string]time.Duration{},
		ObjectCount: map[string]int{},
	}
}

// Section runs fn while measuring its wall-clock duration and recording it
// under name. The closure may set count via the returned setter, which is
// useful for sections that produce a slice whose length isn't known up-front.
func (t *InspectTimings) Section(name string, fn func(setCount func(int)) error) error {
	start := time.Now()
	err := fn(func(n int) { t.ObjectCount[name] = n })
	t.BySection[name] = time.Since(start)
	return err
}

// MarshalJSON serializes durations as integer milliseconds (rounded down) so
// the wire format stays human-readable in API responses and logs.
func (t *InspectTimings) MarshalJSON() ([]byte, error) {
	if t == nil {
		return []byte("null"), nil
	}
	bySection := make(map[string]int64, len(t.BySection))
	for k, v := range t.BySection {
		bySection[k] = v.Milliseconds()
	}
	return json.Marshal(struct {
		TotalMs     int64            `json:"total_ms"`
		BySectionMs map[string]int64 `json:"by_section_ms"`
		ObjectCount map[string]int   `json:"object_count"`
	}{
		TotalMs:     t.Total.Milliseconds(),
		BySectionMs: bySection,
		ObjectCount: t.ObjectCount,
	})
}
