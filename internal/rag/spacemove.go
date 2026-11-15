package rag

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync/atomic"

	"github.com/willesq/orchestrator/internal/spaces"
)

// The store moves of the memory spaces switch (spaces.enabled). Turning
// spaces off merges every space's collection (base__<space>, quarantine
// included) back into the base collection, so memory is one store as it was
// before spaces. Turning them on splits the base again by each chunk's stored
// space. Both copy chunks with their vectors and payloads, so nothing is
// re-embedded and every chunk keeps its space, labels_v and origin.
//
// Both are resumable by construction, as a relabel is: a source is copied,
// counted, then deleted from where it was, under the lock live writes take.
// A run stopped between the copy and the delete leaves two copies with the
// same point IDs, and the next run copies again over them, then deletes. A
// merge moves whatever is outside the base; a split moves whatever the base
// holds for a space other than Shared. Neither needs a record of how far it
// got.

// Move directions.
const (
	MoveMerge = "merge"
	MoveSplit = "split"
)

// MoveReport says how far a store move got. It names no source or space.
type MoveReport struct {
	Direction string
	// Collections is how many collections the move reads from; Done how
	// many it has finished.
	Collections, Done int
	// Sources and Chunks are what was moved; Failed counts chunks left
	// where they were (a damaged chunk), which the next run tries again.
	Sources, Chunks, Failed int
	// Dropped is how many emptied space collections a merge removed.
	Dropped int
}

// moving is set while a store move runs: a search then reads one copy of
// each point, since a source being moved is briefly in two collections.
type moveFlag struct{ n atomic.Int32 }

func (m *moveFlag) start() func() { m.n.Add(1); return func() { m.n.Add(-1) } }
func (m *moveFlag) on() bool      { return m.n.Load() > 0 }

// errMoveMigrating refuses a move while a hybrid migration has a shadow.
var errMoveMigrating = errors.New("a migration is running; the store move runs after it finishes")

// MergeSpaces moves every chunk of every space collection under the active
// base into the base, then removes the emptied collections. progress, when
// set, is told the report after each source. The caller keeps migrations
// and relabels out for the whole run (orchd holds its migration lock).
func (s *Service) MergeSpaces(ctx context.Context, progress func(MoveReport)) (MoveReport, error) {
	rep := MoveReport{Direction: MoveMerge}
	if _, shadow := s.stores(); shadow != nil {
		return rep, errMoveMigrating
	}
	defer s.moves.start()()
	base := s.active()
	all, err := s.allStores(ctx, base, true)
	if err != nil {
		return rep, moveFailed(err)
	}
	var from []*QdrantStore
	for _, ss := range all {
		if ss.st.Collection() != base.Collection() {
			from = append(from, ss.st)
		}
	}
	rep.Collections = len(from)
	report(progress, rep)
	for _, st := range from {
		srcs, err := sourcesOf(ctx, st, nil)
		if err != nil {
			return rep, moveFailed(err)
		}
		for _, src := range srcs {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			if err := s.moveSource(ctx, base, st, base, "", src, &rep); err != nil {
				return rep, err
			}
			report(progress, rep)
		}
		// Reason: an emptied collection is removed, so the store is the one
		// collection it was before spaces; one still holding a chunk that
		// could not move is kept, and the next run tries again.
		if n, err := st.Count(ctx, nil); err == nil && n == 0 {
			if err := st.DeleteCollection(ctx); err != nil && !errors.Is(err, errCollectionMissing) {
				return rep, moveFailed(err)
			}
			rep.Dropped++
		} else if err != nil {
			return rep, moveFailed(err)
		}
		rep.Done++
		report(progress, rep)
	}
	if rep.Dropped > 0 {
		s.forgetCollections(base.Collection())
	}
	return rep, nil
}

// SplitSpaces moves every chunk the base holds for a space other than
// Shared to that space's collection, by the space stored with it. It places
// nothing: a chunk keeps the space and labels_v it has, and a relabel brings
// stale ones up to the applied policy afterwards. A chunk whose stored space
// is not a space name goes to quarantine's collection.
func (s *Service) SplitSpaces(ctx context.Context, progress func(MoveReport)) (MoveReport, error) {
	rep := MoveReport{Direction: MoveSplit, Collections: 1}
	if _, shadow := s.stores(); shadow != nil {
		return rep, errMoveMigrating
	}
	defer s.moves.start()()
	base := s.active()
	report(progress, rep)
	srcs, err := sourcesOf(ctx, base, func(p map[string]any) bool { return labelledAway(str(p[fieldSpace])) })
	if err != nil {
		return rep, moveFailed(err)
	}
	for _, src := range srcs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		to := src.space
		if !spaceSuffix.MatchString(to) {
			// Reason, fail closed: a stored space that is not a space name
			// cannot be read by any space's readers, and must not stay
			// readable as Shared in the base.
			log.Printf("rag: split moved %d chunk(s) whose stored space is not a space name to quarantine", src.chunks)
			to = spaces.Quarantine
		}
		if err := s.moveSource(ctx, base, base, s.derive(base, to), to, src, &rep); err != nil {
			return rep, err
		}
		report(progress, rep)
	}
	rep.Done = 1
	report(progress, rep)
	return rep, nil
}

// labelledAway reports whether a chunk labelled space belongs in a space's
// own collection rather than the base.
func labelledAway(space string) bool { return space != "" && space != spaces.Shared }

func report(progress func(MoveReport), rep MoveReport) {
	if progress != nil {
		progress(rep)
	}
}

// movedSource is one source a move reads, with the space a split moves it
// to and how many chunks it had when listed.
type movedSource struct {
	uri, space string
	chunks     int
}

// sourcesOf lists the sources in st with a chunk keep accepts (every chunk
// when keep is nil), by source and stored space. The whole list is read
// before anything moves, so moving a source never shifts the scroll.
func sourcesOf(ctx context.Context, st *QdrantStore, keep func(map[string]any) bool) ([]movedSource, error) {
	idx := map[[2]string]int{}
	var out []movedSource
	var offset any
	for page := 0; page < maxScrollPages; page++ {
		pts, next, err := st.Scroll(ctx, nil, []string{"source_uri", fieldSpace}, relabelPage, offset)
		if errors.Is(err, errCollectionMissing) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, p := range pts {
			if keep != nil && !keep(p.Payload) {
				continue
			}
			k := [2]string{str(p.Payload["source_uri"]), str(p.Payload[fieldSpace])}
			i, ok := idx[k]
			if !ok {
				i = len(out)
				idx[k] = i
				out = append(out, movedSource{uri: k[0], space: k[1]})
			}
			out[i].chunks++
		}
		if next == nil {
			sort.Slice(out, func(i, j int) bool { return out[i].uri < out[j].uri })
			return out, nil
		}
		offset = next
	}
	return nil, fmt.Errorf("store move: exceeded %d pages", maxScrollPages)
}

// moveSource moves src's chunks from st to dst (the collection of space,
// "" for the base) with their vectors and payloads as they are. A copy dst
// already holds that a later write stored is kept, and st's is only
// deleted. A source's damaged chunks stay where they are, counted as
// failed.
func (s *Service) moveSource(ctx context.Context, base, st, dst *QdrantStore, space string, src movedSource, rep *MoveReport) error {
	if src.uri == "" {
		// Reason: chunks are moved and deleted by their source_uri; one
		// without it cannot be addressed without reaching others.
		rep.Failed += src.chunks
		return nil
	}
	defer s.lockSource(src.uri)()
	pts, err := sourcePoints(ctx, st, src.uri, true)
	switch {
	case errors.Is(err, errBadChunk):
		log.Printf("rag: store move left a source where it was: %v", err)
		rep.Failed += src.chunks
		return nil
	case err != nil:
		return moveFailed(err)
	case len(pts) == 0:
		// A live write moved or removed it since it was listed.
		return nil
	}
	newer, err := laterAt(ctx, dst, src.uri, pts[0].Payload)
	if err != nil {
		return moveFailed(err)
	}
	if !newer {
		s.noteCollection(base, space)
		if err := s.copyPoints(ctx, dst, pts, nil); err != nil {
			if errors.Is(err, errBadChunk) {
				log.Printf("rag: store move left a source where it was: %v", err)
				rep.Failed += len(pts)
				return nil
			}
			return moveFailed(err)
		}
		last := 0
		for _, p := range pts {
			last = max(last, int(num(p.Payload["chunk_index"])))
		}
		// An older, longer copy left in dst would outlive the move.
		if err := pruneChunksFrom(ctx, dst, src.uri, last+1); err != nil {
			return moveFailed(err)
		}
		n, err := dst.Count(ctx, EqFilter("source_uri", src.uri))
		if err != nil {
			return moveFailed(err)
		}
		if n < len(pts) {
			return moveFailed(fmt.Errorf("copied %d of %d chunks; the old copy is kept", n, len(pts)))
		}
	}
	if err := st.DeleteByFilter(ctx, EqFilter("source_uri", src.uri)); err != nil {
		return moveFailed(err)
	}
	rep.Sources++
	rep.Chunks += len(pts)
	return nil
}

// laterAt reports whether dst holds uri as written later than the copy
// whose first chunk's payload is p: a different write (another ingest hash)
// stored after it. Reason: a write made while a move runs goes where the
// switch now puts it, and must not be replaced by the older copy.
func laterAt(ctx context.Context, dst *QdrantStore, uri string, p map[string]any) (bool, error) {
	got, err := dst.Retrieve(ctx, []string{pointID(uri, 0)}, []string{"source_uri", fieldIngestHash, fieldIngestedAt})
	if errors.Is(err, errCollectionMissing) {
		return false, nil
	}
	if err != nil || len(got) == 0 {
		return false, err
	}
	q := got[0].Payload
	if str(q["source_uri"]) != uri || str(q[fieldIngestHash]) == str(p[fieldIngestHash]) {
		return false, nil
	}
	// ingested_at is Unix seconds; a chunk without it counts as oldest.
	return num(q[fieldIngestedAt]) > num(p[fieldIngestedAt]), nil
}

// errMoveStore is what a caller learns of a failed store request: Qdrant's
// errors name collections, whose names contain space names (RC-350).
var errMoveStore = errors.New("store move: a store request failed; see orchd's log, then it runs again")

// moveFailed logs err in full and returns errMoveStore; a cancelled or
// expired context is returned as it is.
func moveFailed(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Printf("rag: store move stopped: %v", err)
	return errMoveStore
}

// onePerPoint keeps, while a store move runs, the first copy of each point
// across the lists of a search of several collections (they come in
// collection order, the base first): a source being moved is briefly in
// both collections, and must be found once. Outside a move the lists are
// returned as they are.
func (s *Service) onePerPoint(lists [][]storeHit) [][]storeHit {
	if !s.moves.on() || len(lists) < 2 {
		return lists
	}
	seen := map[string]bool{}
	out := make([][]storeHit, len(lists))
	for i, l := range lists {
		for _, h := range l {
			if id := h.PointID(); !seen[id] {
				seen[id] = true
				out[i] = append(out[i], h)
			}
		}
	}
	return out
}

// Moving reports whether a store move is running.
func (s *Service) Moving() bool { return s.moves.on() }
