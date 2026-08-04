package sources

import (
	"context"
	"time"
)

// Scheduler periodically refreshes due sources. Sleep-safe: due-ness is computed
// from the persisted last_fetched against wall-clock, so after the laptop sleeps
// or orchd restarts, any overdue source is refreshed on the next scan (catch-up).
type Scheduler struct {
	reg  *Registry
	ing  Ingester
	opts RefreshOpts
	tick time.Duration
	now  func() int64
}

// NewScheduler builds a scheduler. tick is how often the registry is scanned.
func NewScheduler(reg *Registry, ing Ingester, opts RefreshOpts, tick time.Duration) *Scheduler {
	if tick <= 0 {
		tick = 5 * time.Minute
	}
	return &Scheduler{reg: reg, ing: ing, opts: opts, tick: tick, now: func() int64 { return time.Now().Unix() }}
}

// Run scans immediately (catch-up) then every tick until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	s.Scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Scan(ctx)
		}
	}
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
