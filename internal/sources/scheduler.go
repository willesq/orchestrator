package sources

import (
	"context"
	"time"
)

// Scheduler refreshes due sources when scanned. Sleep-safe: due-ness is computed
// from the persisted last_fetched against wall-clock, so after the laptop sleeps
// or orchd restarts, any overdue source is refreshed on the next scan (catch-up).
// Cadence is the caller's concern — orchd drives Scan from a supervisor unit.
type Scheduler struct {
	reg  *Registry
	ing  Ingester
	opts RefreshOpts
	now  func() int64
}

// NewScheduler builds a scheduler over a registry and an ingester.
func NewScheduler(reg *Registry, ing Ingester, opts RefreshOpts) *Scheduler {
	return &Scheduler{reg: reg, ing: ing, opts: opts, now: func() int64 { return time.Now().Unix() }}
}

// Scan refreshes every source whose interval has elapsed.
func (s *Scheduler) Scan(ctx context.Context) {
	now := s.now()
	due, err := s.reg.Due(ctx, now)
	if err != nil {
		return
	}
	for _, src := range due {
		status, _ := Refresh(ctx, src, s.ing, s.opts)
		_ = s.reg.MarkFetched(ctx, src.ID, now, status)
	}
}
