package docs

import (
	"context"
	"time"
)

// Scheduler syncs due owners when scanned. Sleep-safe: due-ness is computed from
// persisted last_synced against wall-clock, so an owner whose window elapsed while
// the laptop slept is synced on the next scan (catch-up). Cadence is the caller's
// concern — orchd drives Scan from a supervisor unit on a short tick, which makes
// `orch docs sync` (which just marks an owner due) feel near-immediate.
type Scheduler struct {
	reg  *Registry
	ing  Ingester
	opts SyncOpts
}

// NewScheduler builds a docs scheduler over a registry and an ingester.
func NewScheduler(reg *Registry, ing Ingester, opts SyncOpts) *Scheduler {
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().Unix() }
	}
	return &Scheduler{reg: reg, ing: ing, opts: opts}
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
