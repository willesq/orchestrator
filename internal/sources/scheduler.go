package sources

import (
	"context"
	"os"
	"path/filepath"
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
	opts := s.opts
	if all, err := s.reg.List(ctx); err == nil {
		opts.Shared = sharedPrefixes(all, opts.KnownRoots)
	} else {
		// Without the full list, sharing cannot be ruled out: prune nothing.
		opts.Shared = map[string]bool{"": true}
	}
	for _, src := range due {
		status, _ := Refresh(ctx, src, s.ing, opts)
		_ = s.reg.MarkFetched(ctx, src.ID, now, status)
	}
}

// sharedPrefixes returns the repo:// prefixes more than one source stores
// under: two repo sources with the same directory name, or a repo source and
// an owner source's subdirectory. One's walk must not prune the other's docs.
func sharedPrefixes(srcs []Source, roots []string) map[string]bool {
	count := map[string]int{}
	for _, s := range srcs {
		switch s.Kind {
		case KindRepo:
			count["repo://"+ownerLabel(s.Owner)+"/"+filepath.Base(filepath.Clean(s.URI))+"/"]++
		case KindOwner:
			seen := map[string]bool{}
			for _, r := range roots {
				entries, _ := os.ReadDir(filepath.Join(r, s.URI))
				for _, e := range entries {
					if e.IsDir() && !seen[e.Name()] {
						seen[e.Name()] = true
						count["repo://"+s.URI+"/"+e.Name()+"/"]++
					}
				}
			}
		}
	}
	shared := map[string]bool{}
	for p, n := range count {
		if n > 1 {
			shared[p] = true
		}
	}
	return shared
}
