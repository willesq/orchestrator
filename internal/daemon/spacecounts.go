package daemon

import (
	"context"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// What the spaces hold, for orch status (SP-031): chunks per space, how far a
// relabel has got, and live sessions in a conflict. orchd counts every
// collection at most once per spaceCountTTL and narrows the answer to each
// caller: the operator (Orch.app) sees every space and the conflicts; anyone
// else sees only the spaces it reads (readableBy), so a session never learns
// how much another job's space holds (INV-01).

// spaceCounter is what the counts need from the RAG store (*rag.Service).
type spaceCounter interface {
	CountSpaces(ctx context.Context, rev int64) ([]rag.CollectionCount, error)
}

// spaceCountTTL is how long counts are reused: each count asks Qdrant twice
// per collection, and Status is polled by the app and the CLI.
const spaceCountTTL = 30 * time.Second

// spaceCountTimeout keeps a slow store or a slow placement from delaying
// Status.
const spaceCountTimeout = 3 * time.Second

// maxConflictChecks bounds how many live quarantined sessions one count
// places again; each may read a checkout and look up its GitHub repo.
const maxConflictChecks = 50

// errCountsUnavailable is what a caller learns when the store could not be
// counted: the store's own error names collections, whose names hold space
// names (RC-350), so it stays in orchd's log.
const errCountsUnavailable = "the memory store could not be counted (see orchd log)"

// spaceCountCache holds the last counts of every collection and when they
// were taken, for the revision they were counted against.
type spaceCountCache struct {
	mu        sync.Mutex
	rev       int64
	at        time.Time
	counts    []rag.CollectionCount
	conflicts int
	// failed says the last count failed, so Status does not count again
	// until the TTL ends while the store is slow or down.
	failed bool
}

// load returns the cached counts for rev, counting again when they are older
// than spaceCountTTL or were taken for another revision.
func (c *spaceCountCache) load(ctx context.Context, rev int64, count func(context.Context) ([]rag.CollectionCount, int, error)) (counts []rag.CollectionCount, conflicts int, at time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rev != rev || c.at.IsZero() || time.Since(c.at) >= spaceCountTTL {
		// Reason: the count is shared by every caller for the TTL, so one
		// caller hanging up early must not record a failure for all.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spaceCountTimeout)
		defer cancel()
		got, n, err := count(ctx)
		c.rev, c.at, c.failed = rev, time.Now(), err != nil
		c.counts, c.conflicts = got, n
		if err != nil {
			log.Printf("orchd: counting the spaces for status: %v", err)
			c.counts, c.conflicts = nil, 0
		}
	}
	return c.counts, c.conflicts, c.at, !c.failed
}

// spacesCounts is the counts part of SpacesStatus for ctx's caller; nil
// with no policy configured or no store that can count.
func (s *server) spacesCounts(ctx context.Context) *orchdv1.SpacesCounts {
	counter, ok := s.rag.(spaceCounter)
	if !ok || counter == nil || s.spaces == nil {
		return nil
	}
	res, rev, err := s.spaces.CurrentRevision()
	if err != nil || !res.Configured() || rev <= 0 {
		return nil
	}
	counts, conflicts, at, ok := s.spaceCounts.load(ctx, rev, func(ctx context.Context) ([]rag.CollectionCount, int, error) {
		cs, err := counter.CountSpaces(ctx, rev)
		if err != nil {
			return nil, 0, err
		}
		n, err := s.countConflicts(ctx, res)
		return cs, n, err
	})
	if !ok {
		return &orchdv1.SpacesCounts{Error: errCountsUnavailable}
	}
	return narrowCounts(res, readableBy(res, callerOf(ctx)), counts, conflicts, at)
}

// narrowCounts is what a caller reading readable (nil: every space) may see
// of counts: one entry per policy space it reads, the relabel totals over
// those spaces' collections (every collection for the operator, unplaced
// content and quarantine included), and the conflicts for the operator only.
func narrowCounts(res *spaces.Resolved, readable []string, counts []rag.CollectionCount, conflicts int, at time.Time) *orchdv1.SpacesCounts {
	every := readable == nil
	out := &orchdv1.SpacesCounts{EverySpace: every, CountedAt: at.Unix()}
	byCollection := map[string]rag.CollectionCount{}
	for _, c := range counts {
		byCollection[c.Space] = c
		if every {
			out.Chunks += count32(c.Chunks)
			out.Stale += count32(c.Stale)
		}
	}
	for _, sp := range res.Policy.Spaces {
		if !every && !slices.Contains(readable, sp.Name) {
			continue
		}
		// A policy space is never named Shared or Quarantine (the policy
		// refuses them), so each has its own collection.
		c := byCollection[sp.Name]
		out.Spaces = append(out.Spaces, &orchdv1.SpaceCount{Space: sp.Name, Kind: string(sp.Kind),
			Chunks: count32(c.Chunks), Stale: count32(c.Stale)})
		if !every {
			out.Chunks += count32(c.Chunks)
			out.Stale += count32(c.Stale)
		}
	}
	slices.SortFunc(out.Spaces, func(a, b *orchdv1.SpaceCount) int {
		switch {
		case a.Space < b.Space:
			return -1
		case a.Space > b.Space:
			return 1
		}
		return 0
	})
	if every {
		out.Conflicts, out.ConflictsCounted = count32(conflicts), true
	}
	return out
}

// countConflicts counts live sessions pinned to quarantine whose first
// folder is a conflict under res: two isolated spaces claim it, so the
// session has no space. A pin does not record why it went to quarantine, so
// the folder is placed again under the policy applied now (RC-502); a
// session whose folder cannot be placed again is not counted. Running out of
// time is an error, so a partial count is never reported as the answer.
func (s *server) countConflicts(ctx context.Context, res *spaces.Resolved) (int, error) {
	if s.pins == nil {
		return 0, nil
	}
	rows, err := s.pins.db.QueryContext(ctx,
		`SELECT first_cwd FROM session_pins WHERE ended_at IS NULL AND space = ? ORDER BY rowid DESC LIMIT ?`,
		spaces.Quarantine, maxConflictChecks)
	if err != nil {
		return 0, err
	}
	var cwds []string
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			_ = rows.Close()
			return 0, err
		}
		cwds = append(cwds, cwd)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	n := 0
	for _, cwd := range cwds {
		o, _, err := s.placementOrigin(ctx, cwd)
		if err != nil {
			continue
		}
		o.System = spaces.SystemSession
		p, err := s.spaces.PlaceUnder(ctx, res, o)
		if err == nil && p.Outcome == spaces.Conflict {
			n++
		}
	}
	return n, ctx.Err()
}
