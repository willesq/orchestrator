package rag

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/willesq/orchestrator/internal/bm25"
)

// fieldBM25Version records which tokenizer made a chunk's sparse vector, so a
// re-index can find the chunks it has not reached yet.
const fieldBM25Version = "bm25_v"

// reindexPage is how many chunks a re-index reads at a time. Only their text
// is held, so memory stays at a few hundred kilobytes whatever the store's
// size.
const reindexPage = 256

// ReindexReport describes a keyword re-index, planned or done.
type ReindexReport struct {
	Collection string
	// Version is the tokenizer the stale chunks are brought to.
	Version int
	// Total counts the collection's chunks; Stale those whose keyword vector
	// an older tokenizer made (or that predate the version stamp).
	Total, Stale int
	// Updated counts chunks re-indexed by this run.
	Updated int
	Applied bool
}

// ReindexKeywords recomputes, in place, the BM25 sparse vector of every chunk
// whose vector an older tokenizer made, from the chunk's stored text. Dense
// vectors and the rest of each payload are left alone, so nothing is
// re-embedded and searches keep working throughout: a chunk not yet reached
// still matches on the terms both tokenizers share. Without apply it only
// counts.
//
// It is resumable by construction: each chunk is stamped with the tokenizer
// version once re-indexed, and a run only reads unstamped chunks, so an
// interrupted run picks up where it stopped. Each source's chunks are
// re-read and updated under the lock live writes take, so a write that lands
// mid-run is never overwritten with a vector of its old text.
func (s *Service) ReindexKeywords(ctx context.Context, apply bool) (ReindexReport, error) {
	base := s.active()
	rep := ReindexReport{Collection: base.Collection(), Version: bm25.TokenizerVersion}
	switch l, err := base.Layout(ctx); {
	case err != nil:
		return rep, err
	case l != LayoutHybrid:
		return rep, fmt.Errorf("collection %q has no keyword index (run orch rag migrate-hybrid first)", base.Collection())
	}
	if _, shadow := s.stores(); shadow != nil {
		return rep, errors.New("a migration is running; re-index after it finishes")
	}
	// Every space's collection is re-indexed with the base (SP-024); the
	// report sums them.
	all, err := s.allStores(ctx, base, true)
	if err != nil {
		return rep, err
	}
	stale := staleFilter()
	var todo []*QdrantStore
	for _, ss := range all {
		st := ss.st
		if st != base {
			switch l, err := st.Layout(ctx); {
			case err != nil:
				return rep, err
			case l == LayoutUnknown:
				continue
			case l != LayoutHybrid:
				return rep, fmt.Errorf("collection %q has no keyword index (run orch rag migrate-hybrid first)", st.Collection())
			}
		}
		total, err := st.Count(ctx, nil)
		if err != nil {
			return rep, err
		}
		n, err := st.Count(ctx, stale)
		if err != nil {
			return rep, err
		}
		rep.Total += total
		rep.Stale += n
		if n > 0 {
			todo = append(todo, st)
		}
	}
	if !apply || rep.Stale == 0 {
		rep.Applied = apply
		return rep, nil
	}
	rep.Applied = true
	for _, st := range todo {
		if err := s.reindexStore(ctx, st, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// reindexStore re-indexes the stale chunks of one collection, adding to rep.
func (s *Service) reindexStore(ctx context.Context, st *QdrantStore, rep *ReindexReport) error {
	stale := staleFilter()
	last := time.Now()
	var offset any
	for page := 0; page < maxScrollPages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		pts, next, err := st.Scroll(ctx, stale, []string{"source_uri"}, reindexPage, offset)
		if err != nil {
			return err
		}
		bySource := map[string][]string{}
		var order []string
		for _, p := range pts {
			uri := str(p.Payload["source_uri"])
			if _, ok := bySource[uri]; !ok {
				order = append(order, uri)
			}
			bySource[uri] = append(bySource[uri], p.ID)
		}
		for _, uri := range order {
			n, err := s.reindexSource(ctx, st, uri, bySource[uri])
			rep.Updated += n
			if err != nil {
				return fmt.Errorf("%s: %w", uri, err)
			}
		}
		if time.Since(last) >= 10*time.Second {
			// Reason: the connection count tells a reused keep-alive (a few)
			// from a dial per request (thousands), the issue #98 symptom.
			log.Printf("rag: keyword re-index of %s: %d of %d chunks, %d Qdrant connections opened so far",
				st.Collection(), rep.Updated, rep.Stale, st.Dials())
			last = time.Now()
		}
		if next == nil {
			return nil
		}
		offset = next
	}
	return fmt.Errorf("keyword re-index: exceeded %d pages", maxScrollPages)
}

// staleFilter matches chunks not stamped with the current tokenizer version.
func staleFilter() map[string]any {
	return map[string]any{"must_not": []any{
		map[string]any{"key": fieldBM25Version, "match": map[string]any{"value": bm25.TokenizerVersion}},
	}}
}

// reindexSource re-indexes some of one source's chunks under its write lock,
// reading their text again there.
func (s *Service) reindexSource(ctx context.Context, st *QdrantStore, uri string, ids []string) (int, error) {
	if uri != "" {
		defer s.lockSource(uri)()
	}
	pts, err := st.Retrieve(ctx, ids, []string{"text", fieldBM25Version})
	if err != nil {
		return 0, err
	}
	var vecs []sparseUpdate
	var done []string
	for _, p := range pts {
		if v, ok := p.Payload[fieldBM25Version].(float64); ok && int(v) == bm25.TokenizerVersion {
			continue // rewritten by a live write since it was listed
		}
		// Text with no terms has no keyword vector under either tokenizer
		// (the new one only adds terms); it is stamped and left as it is.
		if sp := bm25.Document(str(p.Payload["text"]), s.BM25); !sp.IsEmpty() {
			vecs = append(vecs, sparseUpdate{ID: p.ID, Sparse: sp})
		}
		done = append(done, p.ID)
	}
	if len(done) == 0 {
		return 0, nil
	}
	if len(vecs) > 0 {
		if err := st.UpdateSparse(ctx, vecs); err != nil {
			return 0, err
		}
	}
	if err := st.SetPayload(ctx, done, map[string]any{fieldBM25Version: bm25.TokenizerVersion}); err != nil {
		return 0, err
	}
	return len(done), nil
}

// sparseUpdate is one point's new keyword vector.
type sparseUpdate struct {
	ID     string
	Sparse bm25.Vector
}

// Retrieve reads points by id, with the given payload fields.
func (q *QdrantStore) Retrieve(ctx context.Context, ids []string, fields []string) ([]ScrollPoint, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var resp struct {
		Result []ScrollPoint `json:"result"`
	}
	err := q.do(ctx, http.MethodPost, q.path("/points"),
		map[string]any{"ids": ids, "with_payload": fields, "with_vector": false}, &resp)
	return resp.Result, err
}

// UpdateSparse replaces points' BM25 vectors, leaving their dense vectors and
// payloads as they are.
func (q *QdrantStore) UpdateSparse(ctx context.Context, updates []sparseUpdate) error {
	wire := make([]map[string]any, len(updates))
	for i, u := range updates {
		wire[i] = map[string]any{"id": u.ID, "vector": map[string]any{sparseName: u.Sparse}}
	}
	return q.do(ctx, http.MethodPut, q.path("/points/vectors")+"?wait=true", map[string]any{"points": wire}, nil)
}

// SetPayload merges fields into points' payloads.
func (q *QdrantStore) SetPayload(ctx context.Context, ids []string, payload map[string]any) error {
	return q.do(ctx, http.MethodPost, q.path("/points/payload")+"?wait=true",
		map[string]any{"points": ids, "payload": payload}, nil)
}
