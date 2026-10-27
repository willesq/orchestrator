package docs

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBusy means a docs sync is already running; the request was not queued.
var ErrBusy = errors.New("a docs sync is already running")

// Scheduler syncs due owners when scanned. Sleep-safe: due-ness is computed from
// persisted last_synced against wall-clock, so an owner whose window elapsed while
// the laptop slept is synced on the next scan (catch-up).
//
// Only one sync runs at a time. A scan that finds one running returns at once,
// and a request while one runs is refused: nothing is queued, so repeated
// requests cannot pile up work.
type Scheduler struct {
	reg  *Registry
	ing  Ingester
	opts SyncOpts
	// every is how often Tick scans for due owners.
	every time.Duration

	// run is held for a whole scan, and while a request prepares one.
	run sync.Mutex
	// kicked asks the next Tick to scan now.
	kicked atomic.Bool
	// lastScan is when Tick last scanned; only Tick touches it.
	lastScan time.Time

	mu       sync.Mutex
	progress Progress
}

// Progress is what the scheduler is doing, or last did.
type Progress struct {
	Running bool
	// Owner is the owner being synced, or last synced; empty while the due
	// owners are being looked up.
	Owner      string
	StartedAt  time.Time
	FinishedAt time.Time
	// Done of Total repos are handled for Owner; Repo is the last one.
	Done, Total int
	Repo        string
}

// NewScheduler builds a docs scheduler over a registry and an ingester that
// scans for due owners every `every` (a minute when zero).
func NewScheduler(reg *Registry, ing Ingester, opts SyncOpts, every time.Duration) *Scheduler {
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().Unix() }
	}
	if every <= 0 {
		every = time.Minute
	}
	return &Scheduler{reg: reg, ing: ing, opts: opts, every: every}
}

// Progress reports the running sync, or the last one.
func (s *Scheduler) Progress() Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress
}

// Running reports whether a sync is in progress.
func (s *Scheduler) Running() bool { return s.Progress().Running }

// Request prepares a sync (marking owners due, clearing caches) while no sync
// runs, then asks the next Tick to scan. It returns ErrBusy, having done
// nothing, while a sync runs: a running sync is never interrupted to start
// another.
func (s *Scheduler) Request(prepare func() error) error {
	if !s.run.TryLock() {
		return ErrBusy
	}
	err := prepare()
	s.run.Unlock()
	if err != nil {
		return err
	}
	s.kicked.Store(true)
	return nil
}

// Tick scans when a sync was requested or the scan interval has passed. orchd
// calls it on a short cadence so a request starts within seconds.
func (s *Scheduler) Tick(ctx context.Context) {
	if !s.kicked.Swap(false) && time.Since(s.lastScan) < s.every {
		return
	}
	s.lastScan = time.Now()
	s.Scan(ctx)
}

// Scan syncs every owner whose interval has elapsed, unless a sync is
// already running, in which case it does nothing.
func (s *Scheduler) Scan(ctx context.Context) {
	if !s.run.TryLock() {
		return
	}
	defer s.run.Unlock()
	// Running from the moment the scan holds the lock, so a request in the
	// gaps (listing due owners, between owners) is refused too.
	s.update(func(p *Progress) {
		*p = Progress{Running: true, StartedAt: time.Unix(s.opts.Now(), 0)}
	})
	defer s.update(func(p *Progress) { p.Running, p.FinishedAt = false, time.Unix(s.opts.Now(), 0) })
	now := s.opts.Now()
	due, err := s.reg.DueOwners(ctx, now)
	if err != nil {
		log.Printf("docs: list due owners: %v", err)
		return
	}
	// Bookkeeping must land even when a stop cancels the sync midway.
	keep := context.WithoutCancel(ctx)
	for _, o := range due {
		s.update(func(p *Progress) {
			p.Owner, p.StartedAt, p.Done, p.Total, p.Repo = o.Owner, time.Unix(s.opts.Now(), 0), 0, 0, ""
		})
		opts := s.opts
		opts.Progress = func(done, total int, repo string) {
			s.update(func(p *Progress) { p.Done, p.Total, p.Repo = done, total, repo })
		}
		res := SyncOwner(ctx, s.reg, o, s.ing, opts)
		if res.Status != "ok" {
			log.Printf("docs: %s: sync %s: %s", o.Owner, res.Status, res.Reason)
		}
		if ctx.Err() != nil {
			// Stopped (orchd stopping or restarting): keep the owner due so
			// the repos not tried are synced next time, not a full interval
			// from now.
			if err := s.reg.MarkStopped(keep, o.Owner, res.Reason); err != nil {
				log.Printf("docs: mark %s stopped: %v", o.Owner, err)
			}
			return
		}
		if err := s.reg.MarkSynced(keep, o.Owner, now, res.Status, res.Reason); err != nil {
			log.Printf("docs: mark %s synced: %v", o.Owner, err)
		}
	}
}

func (s *Scheduler) update(change func(*Progress)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.progress)
}
