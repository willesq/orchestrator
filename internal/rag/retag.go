package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
)

// Retag (SP-071, D-14, INV-15) moves one source to the space the operator
// names: its chunks are copied with their vectors and payloads to the space's
// collection, the copy is counted, and then the source is deleted from every
// other collection. Nothing is embedded again: it reuses the relabel's move
// (copyInto), so the source's chunk count is unchanged and it exists only in
// the new space afterwards.
//
// It is resumable by construction, as a relabel is: the source is copied
// before it is deleted, and the copy keeps each chunk's point ID. A retag
// stopped between the copy and the delete leaves the source in both
// collections; running it again copies over the same point IDs and deletes
// the old copy, so nothing is duplicated or lost. The sticky override that
// makes later writes honor the retag is orchd's to record (spaces.Options
// Override); this file only moves data.

// ErrRetagNotStored reports that no collection holds the source.
var ErrRetagNotStored = errors.New("no chunks of that source are stored")

// RetagOpts names the source and where it goes.
type RetagOpts struct {
	// SourceURI is the source to move. It must be non-empty: chunks are
	// found and deleted by their source_uri.
	SourceURI string
	// Space is the space the source goes to: a space of the applied policy,
	// spaces.Quarantine, or spaces.Shared. orchd checks it against the
	// policy; this layer only checks it can name a collection.
	Space string
	// Rev is the applied policy revision, stamped as the chunks' labels_v,
	// so a later relabel sees them as current.
	Rev int64
	// Check, when set, is called under the source's lock with the spaces
	// that hold the source (as RetagReport.From names them), before
	// anything is changed; an error refuses the retag and is returned as it
	// is. orchd checks what the operator confirmed and records the retag
	// there, so the check and the move see the same store.
	Check func(from []string) error
}

// RetagReport says what a retag found and did. It names spaces and counts.
type RetagReport struct {
	// From lists the spaces whose collections held the source before the
	// retag, sorted; Unplaced names the base collection. A resumed retag
	// lists the target too.
	From []string
	// Chunks is how many chunks the source has in the target collection
	// after the retag.
	Chunks int
	// Moved is set when chunks changed collection; a source already in the
	// target collection alone is only stamped with the new labels.
	Moved bool
}

// Retag moves opts.SourceURI to opts.Space's collection. The caller keeps
// migrations and relabels out for the whole call (orchd holds its migration
// lock); live writes of the source wait on the source's lock.
func (s *Service) Retag(ctx context.Context, opts RetagOpts) (RetagReport, error) {
	var rep RetagReport
	switch {
	case opts.SourceURI == "":
		return rep, errors.New("retag needs a source URI")
	case opts.Rev <= 0:
		return rep, errors.New("no spaces policy is applied, so there is no space to retag to")
	case !spaceSuffix.MatchString(opts.Space):
		return rep, fmt.Errorf("%q is not a space name", opts.Space)
	}
	// The shadow is checked under the source lock: a migration installs it
	// under the write gate the lock holds shared, so it cannot appear while
	// the retag runs and miss the move.
	defer s.lockSource(opts.SourceURI)()
	if _, shadow := s.stores(); shadow != nil {
		return rep, errors.New("a migration is running; retag after it finishes")
	}
	base := s.active()
	all, err := s.allStores(ctx, base, true)
	if err != nil {
		return rep, retagStoreFailed(err)
	}
	target := s.derive(base, opts.Space)
	var holders []*QdrantStore
	for _, ss := range all {
		n, err := ss.st.Count(ctx, EqFilter("source_uri", opts.SourceURI))
		if err != nil {
			return rep, retagStoreFailed(err)
		}
		if n == 0 {
			continue
		}
		holders = append(holders, ss.st)
		sp, _ := spaceOf(base.Collection(), ss.st.Collection())
		rep.From = append(rep.From, spaceName(sp))
	}
	slices.Sort(rep.From)
	if len(holders) == 0 {
		return rep, ErrRetagNotStored
	}
	if len(holders) > 1 {
		if err := sameWrite(ctx, holders, opts.SourceURI); err != nil {
			return rep, err
		}
	}
	if opts.Check != nil {
		if err := opts.Check(slices.Clone(rep.From)); err != nil {
			return rep, err
		}
	}
	// Bumped before the lock is released, so a write placed before this
	// retag is refused rather than stored (IngestText).
	defer s.bumpRetag(opts.SourceURI)
	labels := map[string]any{fieldSpace: opts.Space, fieldLabelsV: opts.Rev}
	from := retagSource(holders, target)
	if from == nil {
		// Only the target holds it: a retag to where it already is, or the
		// second run of one stopped after its delete. Stamping makes its
		// labels name the space and revision.
		pts, err := sourcePoints(ctx, target, opts.SourceURI, false)
		if err != nil {
			return rep, retagStoreFailed(err)
		}
		ids := make([]string, len(pts))
		for i, p := range pts {
			ids[i] = p.ID
		}
		if err := target.SetPayload(ctx, ids, labels); err != nil {
			return rep, retagStoreFailed(err)
		}
		rep.Chunks = len(pts)
		return rep, nil
	}
	pts, err := sourcePoints(ctx, from, opts.SourceURI, true)
	if err != nil {
		return rep, retagFailed(err)
	}
	if len(pts) == 0 {
		// Deleted between the count and the read: nothing left to move.
		return rep, ErrRetagNotStored
	}
	// copyInto counts the target after copying and refuses to go on with
	// fewer chunks than were read, so the old copy is deleted only once the
	// new one is whole.
	if err := s.copyInto(ctx, base, target, opts.Space, opts.SourceURI, pts, labels); err != nil {
		return rep, retagFailed(err)
	}
	rep.Moved = true
	for _, h := range holders {
		if h.Collection() == target.Collection() {
			continue
		}
		if _, err := deleteSource(ctx, h, opts.SourceURI); err != nil {
			return rep, retagStoreFailed(err)
		}
	}
	n, err := target.Count(ctx, EqFilter("source_uri", opts.SourceURI))
	if err != nil {
		return rep, retagStoreFailed(err)
	}
	rep.Chunks = n
	return rep, nil
}

// errRetagDiverged refuses a source stored in several collections by
// different writes: which one is current cannot be told from the store.
var errRetagDiverged = errors.New("the source is stored in more than one space with different contents; ingest it again, then retag it")

// sameWrite checks that every collection holding uri holds the same write of
// it (the ingest hash of its first chunk), as a retag or relabel stopped
// between its copy and its delete leaves it. Reason: copies from different
// writes are left by something else, and keeping either one could keep the
// stale one and delete the current one.
func sameWrite(ctx context.Context, holders []*QdrantStore, uri string) error {
	want := ""
	for i, h := range holders {
		got, err := h.Retrieve(ctx, []string{pointID(uri, 0)}, []string{fieldIngestHash})
		if err != nil {
			return retagStoreFailed(err)
		}
		hash := ""
		if len(got) > 0 {
			hash = str(got[0].Payload[fieldIngestHash])
		}
		if hash == "" || (i > 0 && hash != want) {
			return errRetagDiverged
		}
		want = hash
	}
	return nil
}

// ErrRetaggedMidWrite refuses a write placed before a retag of its source
// finished; writing it again places it where the retag put the source.
var ErrRetaggedMidWrite = errors.New("the source was retagged while this write was being prepared; write it again")

// retagGeneration is how many retags uri has had in this process.
func (s *Service) retagGeneration(uri string) uint64 {
	s.retagMu.Lock()
	defer s.retagMu.Unlock()
	return s.retagGens[uri]
}

// bumpRetag counts a retag of uri.
func (s *Service) bumpRetag(uri string) {
	s.retagMu.Lock()
	defer s.retagMu.Unlock()
	if s.retagGens == nil {
		s.retagGens = map[string]uint64{}
	}
	s.retagGens[uri]++
}

// retagSource picks the collection a retag copies from: the one other than
// target that holds the source. Several holders hold the same write
// (sameWrite), so any one serves; the first is taken and the rest are
// deleted with it. nil means only target holds it.
func retagSource(holders []*QdrantStore, target *QdrantStore) *QdrantStore {
	for _, h := range holders {
		if h.Collection() != target.Collection() {
			return h
		}
	}
	return nil
}

// errRetagStore is what a caller learns of a failed store request: Qdrant's
// errors name collections, whose names contain space names (RC-350).
var errRetagStore = errors.New("retag: a store request failed; see orchd's log, then run it again")

// retagStoreFailed logs err in full and returns errRetagStore; a cancelled
// or expired context is returned as it is.
func retagStoreFailed(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Printf("rag: retag stopped: %v", err)
	return errRetagStore
}

// retagFailed is retagStoreFailed, except that a chunk that cannot be moved as
// it is says so: the operator learns the source needs a fresh ingest rather
// than another try.
func retagFailed(err error) error {
	if errors.Is(err, errBadChunk) {
		return fmt.Errorf("retag: %s; ingest the source again instead", strings.TrimPrefix(err.Error(), errBadChunk.Error()+": "))
	}
	return retagStoreFailed(err)
}

// WriteRetagReport prints r for the operator: counts and space names only.
func WriteRetagReport(w io.Writer, space string, r RetagReport) {
	verb := "stamped"
	if r.Moved {
		verb = "moved"
	}
	fmt.Fprintf(w, "%s %d chunk(s) to %s (was in %s)\n", verb, r.Chunks, space, strings.Join(r.From, ", "))
}
