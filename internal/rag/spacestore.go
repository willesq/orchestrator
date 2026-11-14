package rag

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/spaces"
)

// Each space's content lives in its own Qdrant collection (SP-024, D-10):
// physical separation means a read that forgets a filter still cannot reach
// another space, and BM25's IDF statistics are each space's own. The
// collections are named after the active collection (the base), so a
// migration or rollback that switches the base moves every space with it:
//
//	base               unplaced content, and Shared (no policy applied)
//	base__<space>      one per space, Quarantine included
//
// The payload's "space" field stays as a second layer: a search of a space's
// collection also filters on it.

// spaceSep joins the base collection and a space name. Space names never
// contain it (they start with a letter and are checked by the policy), and
// Qdrant accepts it in a collection name.
const spaceSep = "__"

// spaceSuffix is a collection suffix that names a space: the policy's space
// name pattern, plus Quarantine.
var spaceSuffix = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// SpaceCollection names the collection holding space's content when base is
// the active collection: base itself for unplaced content and for Shared,
// base__<space> for every other space.
func SpaceCollection(base, space string) string {
	if space == "" || space == spaces.Shared {
		return base
	}
	return base + spaceSep + space
}

// spaceOf returns the space whose collection under base is name, and whether
// name is one of base's collections at all ("" for base itself).
func spaceOf(base, name string) (string, bool) {
	if name == base {
		return "", true
	}
	rest, ok := strings.CutPrefix(name, base+spaceSep)
	if !ok || !spaceSuffix.MatchString(rest) || rest == spaces.Shared {
		return "", false
	}
	return rest, true
}

// spaceStore is one collection a read or a write reaches, and the space a
// search of it filters on as the second layer ("" for none).
type spaceStore struct {
	st    *QdrantStore
	space string
}

// derive returns the store for space's collection under base. Stores are
// kept by collection name, so each keeps its cached layout across calls.
func (s *Service) derive(base *QdrantStore, space string) *QdrantStore {
	name := SpaceCollection(base.Collection(), space)
	if name == base.Collection() {
		return base
	}
	s.derivedMu.Lock()
	defer s.derivedMu.Unlock()
	if st, ok := s.derived[name]; ok {
		return st
	}
	if s.derived == nil {
		s.derived = map[string]*QdrantStore{}
	}
	st := base.WithCollection(name)
	s.derived[name] = st
	return st
}

// storeFor returns the active store for space's content and, during a
// migration, its shadow.
func (s *Service) storeFor(space string) (active, shadow *QdrantStore) {
	a, sh := s.stores()
	active = s.derive(a, space)
	if sh != nil {
		shadow = s.derive(sh, space)
	}
	return active, shadow
}

// collectionsTTL bounds how long the list of a base's space collections is
// reused. Only this process creates them, and it notes each one it creates,
// so the list goes stale only when a collection is removed outside orchd.
const collectionsTTL = time.Minute

// spaceCollections lists the spaces that have a collection under base, ""
// (base itself) first and the rest sorted. A cached list younger than
// collectionsTTL is reused unless fresh is set.
func (s *Service) spaceCollections(ctx context.Context, base *QdrantStore, fresh bool) ([]string, error) {
	s.derivedMu.Lock()
	cached, ok := s.lists[base.Collection()]
	s.derivedMu.Unlock()
	if ok && !fresh && time.Since(cached.at) < collectionsTTL {
		return slices.Clone(cached.spaces), nil
	}
	names, err := base.Collections(ctx)
	if errors.Is(err, errCollectionMissing) {
		// A server that has nothing to list: only the base collection is
		// looked for, as before spaces.
		names, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{""}
	for _, n := range names {
		if sp, ok := spaceOf(base.Collection(), n); ok && sp != "" {
			out = append(out, sp)
		}
	}
	s.derivedMu.Lock()
	// Reason: a collection this process created while the listing was in
	// flight may be missing from it; what orchd noted is always added, so
	// an older answer never hides a newer collection.
	for sp := range s.noted[base.Collection()] {
		if !slices.Contains(out, sp) {
			out = append(out, sp)
		}
	}
	sort.Strings(out[1:])
	if s.lists == nil {
		s.lists = map[string]spaceList{}
	}
	s.lists[base.Collection()] = spaceList{spaces: out, at: time.Now()}
	s.derivedMu.Unlock()
	return slices.Clone(out), nil
}

// spaceList is a cached list of a base's space collections.
type spaceList struct {
	spaces []string
	at     time.Time
}

// noteCollection records that space's collection under base now exists, so
// a cached list includes it without asking Qdrant again, and so does every
// later listing until the base's collections are dropped.
func (s *Service) noteCollection(base *QdrantStore, space string) {
	if space == "" || space == spaces.Shared {
		return
	}
	s.derivedMu.Lock()
	defer s.derivedMu.Unlock()
	if s.noted == nil {
		s.noted = map[string]map[string]bool{}
	}
	if s.noted[base.Collection()] == nil {
		s.noted[base.Collection()] = map[string]bool{}
	}
	s.noted[base.Collection()][space] = true
	l, ok := s.lists[base.Collection()]
	if !ok || slices.Contains(l.spaces, space) {
		return
	}
	l.spaces = append(l.spaces, space)
	sort.Strings(l.spaces[1:])
	s.lists[base.Collection()] = l
}

// forgetCollections drops the cached list for base, after its collections
// were dropped.
func (s *Service) forgetCollections(base string) {
	s.derivedMu.Lock()
	defer s.derivedMu.Unlock()
	delete(s.lists, base)
	delete(s.noted, base)
}

// allStores returns every collection under base: base itself first, then
// each space's. None of them filters on space: they are read whole, as one
// collection was before spaces.
func (s *Service) allStores(ctx context.Context, base *QdrantStore, fresh bool) ([]spaceStore, error) {
	names, err := s.spaceCollections(ctx, base, fresh)
	if err != nil {
		return nil, err
	}
	out := make([]spaceStore, 0, len(names))
	for _, sp := range names {
		out = append(out, spaceStore{st: s.derive(base, sp)})
	}
	return out, nil
}

// readStores returns the collections a search for the given spaces reads.
// nil reads every collection, as reads did before spaces were enforced
// (SP-025 decides each caller's spaces); an empty, non-nil list reads
// nothing. Each named space is read only from its own collection and, as the
// second layer, only where the payload names it. Shared has no second layer:
// it is the store as it was before spaces, whose older chunks carry no space.
func (s *Service) readStores(ctx context.Context, names []string) ([]spaceStore, error) {
	base := s.active()
	if names == nil {
		all, err := s.allStores(ctx, base, false)
		if err != nil {
			// Reason: before SP-025 a search reads everything it can, so a
			// failed listing degrades to the base collection, as search
			// worked before spaces, rather than failing the prompt.
			s.fallbackLog.printf("rag: listing the space collections failed, searching %s only: %v", base.Collection(), err)
			return []spaceStore{{st: base}}, nil
		}
		return all, nil
	}
	var out []spaceStore
	seen := map[string]bool{}
	for _, sp := range names {
		if sp == "" || seen[sp] {
			continue
		}
		seen[sp] = true
		if !spaceSuffix.MatchString(sp) {
			// Not a space name, so no collection holds it.
			continue
		}
		filter := sp
		if sp == spaces.Shared {
			filter = ""
		}
		out = append(out, spaceStore{st: s.derive(base, sp), space: filter})
	}
	return out, nil
}

type collectionsResponse struct {
	Result struct {
		Collections []struct {
			Name string `json:"name"`
		} `json:"collections"`
	} `json:"result"`
}

// Collections lists every collection on the server.
func (q *QdrantStore) Collections(ctx context.Context) ([]string, error) {
	var resp collectionsResponse
	if err := q.do(ctx, http.MethodGet, "/collections", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Result.Collections))
	for _, c := range resp.Result.Collections {
		out = append(out, c.Name)
	}
	return out, nil
}

// sourceKey names one source in one space's collection, for the migration's
// bookkeeping.
func sourceKey(space, uri string) string { return space + "\x00" + uri }

// splitKey reverses sourceKey.
func splitKey(key string) (space, uri string) {
	space, uri, _ = strings.Cut(key, "\x00")
	return space, uri
}

// ErrStoredInAnotherSpace refuses a client's write over a source another
// space holds: moving it would delete that space's copy.
var ErrStoredInAnotherSpace = errors.New("the source is stored in another space; store the note under another source_uri")

// removeElsewhere deletes m's source from every collection other than its
// space's, in the active store and in a migration's shadow: a source lives in
// one space. The caller holds the source's lock.
//
// A client's write never moves a source out of another space: orchd's Ingest
// RPC refuses that before it gets here, and this refuses it again
// (ErrStoredInAnotherSpace) should that check have failed to read the store,
// reading the collections afresh. A source in the base that no space claims
// (stored before placement, or Shared) may be replaced by anyone, as before.
//
// A Shared or unplaced write also clears copies a policy put in space
// collections, but a store that never had a policy has none, and a failed
// listing does not fail such a write: with no policy orch writes as it did
// before spaces (INV-10).
func (s *Service) removeElsewhere(ctx context.Context, m Meta) error {
	uri, space := m.SourceURI, m.Space
	unplaced := space == "" || space == spaces.Shared
	client := m.Writer == WriterClient
	active, shadow := s.stores()
	others, err := s.allStores(ctx, active, client)
	if err != nil {
		if unplaced {
			s.fallbackLog.printf("rag: listing the space collections failed; %s is written without looking for copies in other spaces: %v", uri, err)
			return nil
		}
		return err
	}
	keep := SpaceCollection(active.Collection(), space)
	for _, o := range others {
		if o.st.Collection() == keep {
			continue
		}
		sp, _ := spaceOf(active.Collection(), o.st.Collection())
		if client {
			held, err := heldByAnotherSpace(ctx, o.st, sp, uri, space)
			if err != nil {
				return err
			}
			if held {
				return fmt.Errorf("%s: %w", uri, ErrStoredInAnotherSpace)
			}
		}
		if _, err := deleteSource(ctx, o.st, uri); err != nil {
			if shadow != nil {
				s.markDirty(sourceKey(sp, uri))
			}
			return err
		}
		if shadow != nil {
			_, err := deleteSource(ctx, s.derive(shadow, sp), uri)
			s.shadowResult(sourceKey(sp, uri), err)
		}
	}
	return nil
}

// heldByAnotherSpace reports whether st, the collection of space sp ("" for
// the base), holds uri for a space other than space: any copy in another
// space's collection, or a base copy whose payload names another space (one
// SP-023 stored before collections were split).
func heldByAnotherSpace(ctx context.Context, st *QdrantStore, sp, uri, space string) (bool, error) {
	if sp != "" {
		n, err := st.Count(ctx, EqFilter("source_uri", uri))
		return n > 0, err
	}
	pts, err := st.Retrieve(ctx, []string{pointID(uri, 0)}, []string{fieldSpace, "source_uri"})
	if errors.Is(err, errCollectionMissing) {
		return false, nil
	}
	if err != nil || len(pts) == 0 || str(pts[0].Payload["source_uri"]) != uri {
		return false, err
	}
	held := str(pts[0].Payload[fieldSpace])
	return held != "" && held != spaces.Shared && held != space, nil
}
