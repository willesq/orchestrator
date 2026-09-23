package docs

import (
	"context"
	"time"
)

// Scheduler periodically syncs due owners. Sleep-safe: due-ness is computed from
// persisted last_synced against wall-clock, so an owner whose window elapsed while
// the laptop slept is synced on the next scan (catch-up). A short tick makes
// `orch docs sync` (which just marks an owner due) feel near-immediate.
type Scheduler struct {
	reg  *Registry
	ing  Ingester
	opts SyncOpts
	tick time.Duration
}

// NewScheduler builds a docs scheduler. tick is how often the registry is scanned.
func NewScheduler(reg *Registry, ing Ingester, opts SyncOpts, tick time.Duration) *Scheduler {
	if tick <= 0 {
		tick = time.Minute
	}
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().Unix() }
	}
	return &Scheduler{reg: reg, ing: ing, opts: opts, tick: tick}
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

// Scan syncs every owner whose interval has elapsed.
func (s *Scheduler) Scan(ctx context.Context) {
	now := s.opts.Now()
	due, err := s.reg.DueOwners(ctx, now)
	if err != nil {
		return
	}
	for _, o := range due {
		status, _ := SyncOwner(ctx, s.reg, o, s.ing, s.opts)
		_ = s.reg.MarkSynced(ctx, o.Owner, now, status)
	}
}
