package rag

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"
)

// MigrationProgress is how far a running migration has got.
type MigrationProgress struct {
	Target  string
	Done    int // sources copied
	Total   int // sources to copy
	Chunks  int // chunks written to the target
	Started time.Time
}

// MigrationProgress reports the running migration, if any.
func (s *Service) MigrationProgress() (MigrationProgress, bool) {
	if p := s.progress.Load(); p != nil {
		return *p, true
	}
	return MigrationProgress{}, false
}

// progressEvery is how often a running migration logs how far it has got.
var progressEvery = 30 * time.Second

// progressTracker publishes a migration's progress and logs it periodically.
type progressTracker struct {
	s      *Service
	p      MigrationProgress
	logged time.Time
}

func (s *Service) trackProgress(target string, total int) *progressTracker {
	now := time.Now()
	t := &progressTracker{s: s, p: MigrationProgress{Target: target, Total: total, Started: now}, logged: now}
	p := t.p
	s.progress.Store(&p)
	return t
}

func (t *progressTracker) step(chunks int) {
	t.p.Done++
	t.p.Chunks = chunks
	p := t.p
	t.s.progress.Store(&p)
	if time.Since(t.logged) >= progressEvery {
		t.logged = time.Now()
		log.Printf("rag: migrating to %s: %d/%d sources, %d chunks, %s elapsed",
			p.Target, p.Done, p.Total, p.Chunks, time.Since(p.Started).Round(time.Second))
	}
}

func (t *progressTracker) finish() { t.s.progress.Store(nil) }

// ErrNoCollection means the collection to drop does not exist.
var ErrNoCollection = errors.New("collection does not exist")

// DropReport describes a backup collection that is, or would be, dropped.
type DropReport struct {
	Collection string
	// Points counts the points of the collection and of every space
	// collection under it.
	Points  int
	Dropped bool
	// Spaces names the spaces whose collections under it are, or would be,
	// dropped with it.
	Spaces []string
}

// DropCollection deletes a collection this service is not using, such as the
// backup a migration kept, and every space's collection under it (SP-024).
// Without apply it only reports what would go. It refuses the active or
// shadow collection, and one a migration has not finished copying into.
func (s *Service) DropCollection(ctx context.Context, name string, apply bool) (DropReport, error) {
	rep := DropReport{Collection: name}
	if !ValidCollectionName(name) {
		return rep, fmt.Errorf("collection name %q must be letters, digits, '_' or '-'", name)
	}
	active, shadow := s.stores()
	if name == active.Collection() || (shadow != nil && name == shadow.Collection()) {
		return rep, fmt.Errorf("orchd is using %q; restart orchd after a rollback before dropping anything", name)
	}
	// Reason: a name under the active (or shadow) base is one of its space
	// collections, which a backup drop must never reach.
	for _, b := range []*QdrantStore{active, shadow} {
		if b == nil {
			continue
		}
		if sp, ok := spaceOf(b.Collection(), name); ok && sp != "" {
			return rep, fmt.Errorf("orchd is using %q; restart orchd after a rollback before dropping anything", name)
		}
		// Reason: the reverse also holds when names contain "__": with the
		// active base x__y, dropping x would read x__y as x's space y.
		if sp, ok := spaceOf(name, b.Collection()); ok && sp != "" {
			return rep, fmt.Errorf("orchd is using %q, which reads as one of %q's space collections; not dropping it", b.Collection(), name)
		}
	}
	// Without a state directory an unfinished migration cannot be ruled out.
	marker := s.markerPath(name)
	if marker == "" {
		return rep, errors.New("no state directory for migration markers")
	}
	if _, err := os.Stat(marker); err == nil {
		return rep, fmt.Errorf("a migration into %q has not finished; run it again or pick another collection", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return rep, err
	}
	base := active.WithCollection(name)
	names, err := s.spaceCollections(ctx, base, true)
	if err != nil {
		return rep, err
	}
	var stores []*QdrantStore
	for _, sp := range names {
		st := base
		if sp != "" {
			st = base.WithCollection(SpaceCollection(name, sp))
		}
		l, err := st.Layout(ctx)
		if err != nil {
			return rep, err
		}
		if l == LayoutUnknown {
			continue
		}
		n, err := st.Count(ctx, nil)
		if err != nil {
			return rep, err
		}
		rep.Points += n
		if sp != "" {
			rep.Spaces = append(rep.Spaces, sp)
		}
		stores = append(stores, st)
	}
	if len(stores) == 0 {
		return rep, fmt.Errorf("%w: %q", ErrNoCollection, name)
	}
	if !apply {
		return rep, nil
	}
	defer s.forgetCollections(name)
	// The space collections go first, so a drop that fails part-way still
	// leaves the base, and running it again finds what is left.
	for i := len(stores) - 1; i >= 0; i-- {
		if err := stores[i].DeleteCollection(ctx); err != nil {
			return rep, err
		}
	}
	rep.Dropped = true
	return rep, nil
}
