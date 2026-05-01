// Package events is a tiny in-process pub/sub with SSE adapter, used by the
// worker pool to emit run / step / batch progress events.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	RunID   uuid.UUID      `json:"run_id"`
	StepID  *uuid.UUID     `json:"step_id,omitempty"`
	BatchID *uuid.UUID     `json:"batch_id,omitempty"`
	Kind    string         `json:"kind"` // run.status | step.status | batch.progress | log
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
	Ts      time.Time      `json:"ts"`
}

type Bus struct {
	mu   sync.RWMutex
	subs map[uuid.UUID]map[chan Event]struct{}

	db *pgxpool.Pool // optional: persist events into step_events
}

func NewBus(db *pgxpool.Pool) *Bus {
	return &Bus{
		subs: map[uuid.UUID]map[chan Event]struct{}{},
		db:   db,
	}
}

// Publish emits e to all subscribers of the run and persists it in step_events.
func (b *Bus) Publish(ctx context.Context, e Event) {
	if e.Ts.IsZero() {
		e.Ts = time.Now().UTC()
	}
	if b.db != nil {
		_ = b.persist(ctx, e) // best-effort
	}
	b.mu.RLock()
	subs := b.subs[e.RunID]
	for ch := range subs {
		select {
		case ch <- e:
		default:
			// slow consumer; drop to avoid blocking the publisher
		}
	}
	b.mu.RUnlock()
}

func (b *Bus) Subscribe(runID uuid.UUID) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	if b.subs[runID] == nil {
		b.subs[runID] = map[chan Event]struct{}{}
	}
	b.subs[runID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[runID], ch)
		if len(b.subs[runID]) == 0 {
			delete(b.subs, runID)
		}
		b.mu.Unlock()
		close(ch)
	}
}

func (b *Bus) persist(ctx context.Context, e Event) error {
	data, _ := json.Marshal(e.Data)
	_, err := b.db.Exec(ctx, `
		INSERT INTO squishy.step_events (run_id, step_id, batch_id, level, kind, message, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.RunID, e.StepID, e.BatchID, coalesceLevel(e.Level), e.Kind, e.Message, data)
	return err
}

func coalesceLevel(l string) string {
	if l == "" {
		return "info"
	}
	return l
}

// ServeSSE streams events for runID to w using Server-Sent Events. Blocks
// until the client disconnects, the context is cancelled, or the run reaches
// a terminal state (caller's responsibility to signal by closing the channel
// via a terminal Event).
func (b *Bus) ServeSSE(ctx context.Context, runID uuid.UUID, w http.ResponseWriter) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx

	sub, cancel := b.Subscribe(runID)
	defer cancel()

	// Replay any recent events so the UI catches up on reconnect.
	if err := b.replayRecent(ctx, runID, w, flusher); err != nil {
		return err
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, ok := <-sub:
			if !ok {
				return nil
			}
			body, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Kind, body)
			flusher.Flush()
		}
	}
}

func (b *Bus) replayRecent(ctx context.Context, runID uuid.UUID, w http.ResponseWriter, flusher http.Flusher) error {
	if b.db == nil {
		return nil
	}
	rows, err := b.db.Query(ctx, `
		SELECT run_id, step_id, batch_id, level, kind, message, data, created_at
		  FROM squishy.step_events
		 WHERE run_id=$1
		 ORDER BY id DESC
		 LIMIT 200`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var data []byte
		if err := rows.Scan(&e.RunID, &e.StepID, &e.BatchID, &e.Level, &e.Kind, &e.Message, &data, &e.Ts); err != nil {
			return err
		}
		if len(data) > 0 {
			_ = json.Unmarshal(data, &e.Data)
		}
		events = append(events, e)
	}
	// replay in chronological order
	for i := len(events) - 1; i >= 0; i-- {
		body, _ := json.Marshal(events[i])
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", events[i].Kind, body)
	}
	flusher.Flush()
	return nil
}
