// Package status holds porter's last-pass state and serves it over gRPC
// (cwb.v1.BackupStatusService) — the map's backup clock. State is in-memory:
// porter restarts show "no pass yet" until the next tick, which is honest.
package status

import (
	"sync"
	"time"
)

// Source is one backed-up source in the last successful pass.
type Source struct {
	Name      string
	SizeBytes int64
}

// Snapshot is a copy of the holder's state (pointers are nil when unset).
type Snapshot struct {
	LastSuccess *time.Time
	LastAttempt *time.Time
	LastError   string
	Sources     []Source
	NextDue     time.Time
}

// Holder records sync-pass outcomes; safe for concurrent use.
type Holder struct {
	mu       sync.Mutex
	interval time.Duration
	s        Snapshot
}

// NewHolder builds a Holder; interval is the sync ticker period (for NextDue).
func NewHolder(interval time.Duration) *Holder {
	return &Holder{interval: interval}
}

// RecordSuccess notes a completed pass and its sources.
func (h *Holder) RecordSuccess(at time.Time, sources []Source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := at
	h.s.LastSuccess = &t
	h.s.LastAttempt = &t
	h.s.LastError = ""
	h.s.Sources = append([]Source(nil), sources...)
	h.s.NextDue = at.Add(h.interval)
}

// RecordFailure notes a failed pass; the last good sources/success stand.
func (h *Holder) RecordFailure(at time.Time, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := at
	h.s.LastAttempt = &t
	h.s.LastError = msg
	h.s.NextDue = at.Add(h.interval)
}

// Get returns a copy of the current snapshot.
func (h *Holder) Get() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.s
	out.Sources = append([]Source(nil), h.s.Sources...)
	return out
}
