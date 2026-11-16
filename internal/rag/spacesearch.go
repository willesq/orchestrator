package rag

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Searching several space collections (SP-024): each collection is searched
// with its space as the second layer, the lists are merged by score, and the
// vectors of keyword-only hits are fetched only where the result needs them.

// must adds the second layer, the filter on the collection's own space, to a
// search's exact-match conditions, and for a read by policy the filter on
// the policy's revision (INV-08). Every read leaves chat direct messages out
// unless the store keeps them (SP-060): a whole-store read, the operator's
// included, is no exception, since auto-recall runs as whoever is at the
// prompt.
func (t spaceStore) must(must map[string]string) map[string]string {
	out := make(map[string]string, len(must)+3)
	for k, v := range must {
		out[k] = v
	}
	if !t.directMessages {
		out[excludeDirectMessages] = "1"
	}
	if t.space == "" {
		return out
	}
	out[fieldSpace] = t.space
	if t.labelsV > 0 {
		out[fieldLabelsV] = strconv.FormatInt(t.labelsV, 10)
	}
	return out
}

// withLabels has every named space's store in stores read only chunks
// labelled under revision v (INV-08: a chunk labelled under another
// revision is unreadable until it is relabelled). 0 keeps every chunk, as
// does a store read whole (the base, or every collection).
func withLabels(stores []spaceStore, v int64) []spaceStore {
	if v <= 0 {
		return stores
	}
	for i := range stores {
		if stores[i].space != "" {
			stores[i].labelsV = v
		}
	}
	return stores
}

// layoutOf is the layout a search of targets can use: hybrid only when every
// collection is. Collections that do not exist yet hold nothing and are left
// out when there are several; a lone one is kept, so a search of a single
// collection fails over exactly as it did before spaces.
func layoutOf(ctx context.Context, targets []spaceStore) ([]spaceStore, Layout, error) {
	if len(targets) == 1 {
		l, err := targets[0].st.Layout(ctx)
		return targets, l, err
	}
	kept := targets[:0:0]
	common := LayoutHybrid
	for _, t := range targets {
		l, err := t.st.Layout(ctx)
		if err != nil {
			return targets, LayoutUnknown, err
		}
		if l == LayoutUnknown {
			continue
		}
		if l != LayoutHybrid {
			common = l
		}
		kept = append(kept, t)
	}
	return kept, common, nil
}

// denseCollection names, for an error a client sees, the collection of
// targets without a keyword index: the base by name, as before spaces, and a
// space's collection without naming it, since space names are what spaces
// keep apart.
func denseCollection(ctx context.Context, base string, targets []spaceStore) string {
	for _, t := range targets {
		if l, _ := t.st.Layout(ctx); l != LayoutHybrid {
			if t.st.Collection() == base {
				return fmt.Sprintf("collection %q", base)
			}
			return "a space's collection"
		}
	}
	return "a collection"
}

// storeHit is a hit and the collection it came from: two spaces can hold
// points with the same id (one source written in both), and they are
// different chunks.
type storeHit struct {
	SearchHit
	collection string
}

// key identifies the hit across collections.
func (h storeHit) key() string { return h.collection + "/" + h.PointID() }

// eachStore runs search on every target in parallel and returns each one's
// hits, in target order. The first error wins.
func eachStore(ctx context.Context, targets []spaceStore, search func(spaceStore) ([]SearchHit, error)) ([][]storeHit, error) {
	out := make([][]storeHit, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hits, err := search(t)
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = make([]storeHit, len(hits))
			for j, h := range hits {
				out[i][j] = storeHit{SearchHit: h, collection: t.st.Collection()}
			}
		}()
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// mergeByScore merges ranked lists from several collections into one ranked
// list of at most n, highest score first; ties keep collection order. One
// list is returned as it is.
func mergeByScore(lists [][]storeHit, n int) []storeHit {
	if len(lists) == 1 {
		return lists[0]
	}
	var all []storeHit
	for _, l := range lists {
		all = append(all, l...)
	}
	slices.SortStableFunc(all, func(a, b storeHit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// finishLazy ranks candidates some of whose cosines are not known yet
// (needsCos), and fetches only the vectors the result depends on: those of
// candidates that tie another's weighted score (the cosine breaks the tie)
// and of those returned (the cosine is their score). Every other candidate's
// place is fixed by its score alone, so the result is what finish would
// return with every cosine known.
func (s *Service) finishLazy(ctx context.Context, targets []spaceStore, cands []candidate, vec []float32, opts SearchOpts, idents []string) ([]Hit, error) {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	weigh(cands, s.Ranking, opts.Ranking, now)
	scores := make(map[float64]int, len(cands))
	for _, c := range cands {
		scores[c.score]++
	}
	if err := fetchCosines(ctx, targets, cands, vec, func(c candidate) bool { return scores[c.score] > 1 }); err != nil {
		return nil, err
	}
	ranked := order(cands, s.perSource(opts), opts.TopK)
	if err := fetchCosines(ctx, targets, ranked, vec, func(candidate) bool { return true }); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(ranked))
	for _, c := range ranked {
		hits = append(hits, toHit(c.hit, c.collection, c.cosine, c.keyword, idents))
	}
	return hits, nil
}

// fetchCosines reads the dense vectors of the candidates that still need
// their cosine and that want selects, one request per collection, and sets
// each one's cosine to vec.
func fetchCosines(ctx context.Context, targets []spaceStore, cands []candidate, vec []float32, want func(candidate) bool) error {
	byCollection := map[string][]int{}
	for i, c := range cands {
		if c.needsCos && want(c) {
			byCollection[c.collection] = append(byCollection[c.collection], i)
		}
	}
	for _, t := range targets {
		idx := byCollection[t.st.Collection()]
		if len(idx) == 0 {
			continue
		}
		ids := make([]string, len(idx))
		for i, j := range idx {
			ids[i] = cands[j].hit.PointID()
		}
		vecs, err := t.st.DenseVectors(ctx, ids)
		if err != nil {
			return err
		}
		for _, j := range idx {
			cands[j].cosine, cands[j].needsCos = cosineWith(vec, vecs[cands[j].hit.PointID()]), false
		}
	}
	return nil
}
