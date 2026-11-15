package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"

	"github.com/willesq/orchestrator/internal/bm25"
)

// Relabel (SP-027, INV-08) brings every chunk up to the applied spaces policy.
// A chunk whose labels_v is not the applied revision's (or that has none,
// stored before placement) is stale. Each stale source is placed again from
// what was stored with it, and its chunks are stamped with the new space and
// revision where they are, or moved to the new space's collection with their
// vectors and payloads: nothing is re-embedded.
//
// It is resumable by construction. A source's chunks are copied to the new
// collection before they are deleted from the old one, under the lock live
// writes take, and a run reads only stale chunks: a run stopped between the
// copy and the delete leaves a stale copy behind, which the next run copies
// again over the same point IDs and then deletes, so nothing is duplicated or
// lost. A source that cannot be placed now (its placer failed) stays stale
// and is counted; the next run tries it again.

// Unplaced names, in a relabel's transitions, chunks stored with no space.
const Unplaced = "unplaced"

// RelabelOpts is what a relabel brings the store up to.
type RelabelOpts struct {
	// Rev is the applied policy revision: a chunk whose labels_v differs is
	// stale. It must be positive; with no policy applied nothing is stale.
	Rev int64
	// Place places a stale source again from what was stored with it: its
	// origin, URI, type, tags and writer. It sets m.Space, and may set
	// m.Origin for content stored without one, which is then stored with it;
	// the revision stamped is always Rev. An error leaves the source stale.
	Place PlaceFunc
	// Apply stamps and moves; without it the run only counts.
	Apply bool
}

// Transition counts the chunks going from one space to another.
type Transition struct {
	From, To string
	Chunks   int
}

// RelabelReport says what a relabel found and did. It names spaces and counts
// only: never a source or its content.
type RelabelReport struct {
	// Rev is the revision chunks were brought up to.
	Rev int64
	// Total is how many chunks every collection holds; Stale how many of
	// them were not labelled by Rev when the run read them.
	Total, Stale int
	// Changed is how many chunks were stamped or moved (or, without Apply,
	// would be); Moved how many of those changed collection.
	Changed, Moved int
	// Failed is how many stale chunks were left stale: their source could
	// not be placed, or was written again by a live write mid-run.
	Failed int
	// Transitions are the stale chunks by the space they were in and the
	// space they were placed in, sorted.
	Transitions []Transition
	// Applied is set when the run stamped and moved.
	Applied bool
}

// relabelPage is how many chunks one page of a relabel's scroll reads.
const relabelPage = 1000

// staleSource is one source's stale chunks in one collection.
type staleSource struct {
	uri    string
	chunks int
}

// Relabel brings every collection's stale chunks up to opts.Rev. Without
// opts.Apply it places each stale source and counts, changing nothing.
// The caller keeps migrations out for the whole run (orchd holds its
// migration lock): the check for a migration's shadow here is made once.
func (s *Service) Relabel(ctx context.Context, opts RelabelOpts) (RelabelReport, error) {
	rep := RelabelReport{Rev: opts.Rev}
	switch {
	case opts.Rev <= 0:
		return rep, errors.New("no spaces policy is applied, so no chunk is stale")
	case opts.Place == nil:
		return rep, errors.New("relabel needs a placer")
	}
	if _, shadow := s.stores(); shadow != nil {
		return rep, errors.New("a migration is running; relabel after it finishes")
	}
	base := s.active()
	all, err := s.allStores(ctx, base, true)
	if err != nil {
		return rep, storeFailed(err)
	}
	moves := map[[2]string]int{}
	rep.Applied = opts.Apply
	// Every collection is counted and listed before any is changed, so a
	// chunk moved into a collection not yet read is counted once.
	todo := make([][]staleSource, len(all))
	for i, ss := range all {
		total, err := ss.st.Count(ctx, nil)
		if err != nil {
			return rep, storeFailed(err)
		}
		rep.Total += total
		var n int
		if todo[i], n, err = staleSources(ctx, ss.st, opts.Rev); err != nil {
			return rep, storeFailed(err)
		}
		rep.Stale += n
	}
	for i, ss := range all {
		from, _ := spaceOf(base.Collection(), ss.st.Collection())
		for _, src := range todo[i] {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			err := s.relabelSource(ctx, base, ss.st, from, src, opts, &rep, moves)
			switch {
			case errors.Is(err, errBadChunk):
				// One source's damaged data must not hold up every other.
				log.Printf("rag: relabel left a source stale in %s: %v", describeCollection(base.Collection(), ss.st.Collection()), err)
				rep.Failed += src.chunks
			case err != nil:
				return rep, storeFailed(err)
			}
		}
	}
	for k, n := range moves {
		rep.Transitions = append(rep.Transitions, Transition{From: k[0], To: k[1], Chunks: n})
	}
	sort.Slice(rep.Transitions, func(i, j int) bool {
		a, b := rep.Transitions[i], rep.Transitions[j]
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	return rep, nil
}

// errRelabelStore is what a caller learns of a failed store request: Qdrant's
// errors name collections, whose names contain space names (RC-350).
var errRelabelStore = errors.New("relabel: a store request failed; see orchd's log, then run it again")

// errBadChunk marks a source whose stored chunks cannot be moved as they are.
var errBadChunk = errors.New("a stored chunk cannot be moved")

// storeFailed logs err in full and returns errRelabelStore; a cancelled or
// expired context is returned as it is.
func storeFailed(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Printf("rag: relabel stopped: %v", err)
	return errRelabelStore
}

// describeCollection names a collection in an error without naming a space:
// space names are what spaces keep apart.
func describeCollection(base, name string) string {
	if name == base {
		return fmt.Sprintf("collection %q", base)
	}
	return "a space's collection"
}

// staleFilterFor matches chunks not labelled by rev, including chunks with
// no labels_v at all.
func staleFilterFor(rev int64) map[string]any {
	return map[string]any{"must_not": []any{
		map[string]any{"key": fieldLabelsV, "match": map[string]any{"value": rev}},
	}}
}

// staleSources lists the sources in st with chunks not labelled by rev, and
// how many such chunks there are. The whole list is read before anything is
// changed, so moving a source never shifts the scroll.
func staleSources(ctx context.Context, st *QdrantStore, rev int64) ([]staleSource, int, error) {
	idx := map[string]int{}
	var out []staleSource
	total := 0
	var offset any
	for page := 0; page < maxScrollPages; page++ {
		pts, next, err := st.Scroll(ctx, staleFilterFor(rev), []string{"source_uri"}, relabelPage, offset)
		if err != nil {
			return nil, 0, err
		}
		for _, p := range pts {
			total++
			uri := str(p.Payload["source_uri"])
			i, ok := idx[uri]
			if !ok {
				i = len(out)
				idx[uri] = i
				out = append(out, staleSource{uri: uri})
			}
			out[i].chunks++
		}
		if next == nil {
			return out, total, nil
		}
		offset = next
	}
	return nil, 0, fmt.Errorf("relabel: exceeded %d pages", maxScrollPages)
}

// spaceName is how a transition names a space: Unplaced for none.
func spaceName(sp string) string {
	if sp == "" {
		return Unplaced
	}
	return sp
}

// relabelSource places one source's stale chunks in st (the collection of
// space from, "" for the base) and stamps or moves them, adding to rep. Only
// a store error is returned; a source that cannot be placed is counted as
// failed and left stale.
func (s *Service) relabelSource(ctx context.Context, base, st *QdrantStore, from string, src staleSource, opts RelabelOpts,
	rep *RelabelReport, moves map[[2]string]int) error {
	if src.uri == "" {
		// Reason: chunks are found, moved and deleted by their source_uri;
		// one without it cannot be addressed without reaching others.
		rep.Failed += src.chunks
		return nil
	}
	defer s.lockSource(src.uri)()
	pts, err := sourcePoints(ctx, st, src.uri, opts.Apply)
	if err != nil {
		return err
	}
	stale := pts[:0:0]
	for _, p := range pts {
		if labelsV(p.Payload[fieldLabelsV]) != opts.Rev {
			stale = append(stale, p)
		}
	}
	if len(stale) == 0 {
		// A live write replaced it since it was listed: it is current.
		return nil
	}
	m := metaOf(src.uri, stale[0].Payload)
	if err := opts.Place(ctx, &m); err != nil || m.Space == "" {
		log.Printf("rag: relabel could not place a source in %s, left stale: %v", describeCollection(base.Collection(), st.Collection()), err)
		rep.Failed += len(stale)
		return nil
	}
	target := SpaceCollection(base.Collection(), m.Space)
	same := target == st.Collection()
	if opts.Apply {
		labels := map[string]any{fieldSpace: m.Space, fieldLabelsV: opts.Rev}
		if o := m.Origin.payload(); len(o) > 0 {
			// An origin the placer reconstructed is stored, so the next
			// relabel places the source from it.
			labels[fieldOrigin] = o
		}
		if err := s.applyPlacement(ctx, base, st, target, m.Space, src.uri, pts, stale, labels); err != nil {
			return err
		}
	}
	// Counted once done: each chunk's transition starts where its payload
	// says it was placed; a chunk of the base with no space is unplaced.
	for _, p := range stale {
		was := str(p.Payload[fieldSpace])
		if was == "" {
			was = from
		}
		moves[[2]string{spaceName(was), m.Space}]++
	}
	rep.Changed += len(stale)
	if !same {
		// Every chunk of the source moves, current ones included: a source
		// lives in one collection.
		rep.Moved += len(pts)
		rep.Changed += len(pts) - len(stale)
	}
	return nil
}

// applyPlacement stamps a source's stale chunks in st, or copies the whole
// source to target, the collection of space; it also deletes the source from
// every other collection. Reason: a run stopped after copying, followed by a
// run that places the source elsewhere (a repo's visibility or a checkout
// changed meanwhile), would otherwise leave a current-labelled copy readable
// in the first space. The stale copy in st is changed last, so a run that
// fails before then leaves it stale and the next run finishes the job. The
// caller holds the source's lock.
func (s *Service) applyPlacement(ctx context.Context, base, st *QdrantStore, target, space, uri string,
	pts, stale []vectorPoint, labels map[string]any) error {
	move := target != st.Collection()
	if move {
		if err := s.copyInto(ctx, base, s.derive(base, space), space, uri, pts, labels); err != nil {
			return err
		}
	}
	all, err := s.allStores(ctx, base, false)
	if err != nil {
		return err
	}
	for _, o := range all {
		if c := o.st.Collection(); c == target || c == st.Collection() {
			continue
		}
		if _, err := deleteSource(ctx, o.st, uri); err != nil {
			return err
		}
	}
	if move {
		return st.DeleteByFilter(ctx, EqFilter("source_uri", uri))
	}
	ids := make([]string, len(stale))
	for i, p := range stale {
		ids[i] = p.ID
	}
	return st.SetPayload(ctx, ids, labels)
}

// copyInto copies a source's points to dst, the collection of space, with
// labels merged into their payloads, and checks dst holds them all. A newer
// copy already in dst (a live write placed it there) is kept as it is.
func (s *Service) copyInto(ctx context.Context, base, dst *QdrantStore, space, uri string, pts []vectorPoint, labels map[string]any) error {
	newer, err := holdsNewer(ctx, dst, uri, labels[fieldLabelsV].(int64), str(pts[0].Payload[fieldIngestHash]))
	if err != nil || newer {
		return err
	}
	s.noteCollection(base, space)
	if err := s.copyPoints(ctx, dst, pts, labels); err != nil {
		return err
	}
	last := 0
	for _, p := range pts {
		last = max(last, int(num(p.Payload["chunk_index"])))
	}
	// An older, longer copy left in dst would outlive the move.
	if err := pruneChunksFrom(ctx, dst, uri, last+1); err != nil {
		return err
	}
	n, err := dst.Count(ctx, EqFilter("source_uri", uri))
	if err != nil {
		return err
	}
	if n < len(pts) {
		return fmt.Errorf("copied %d of %d chunks; the old copy is kept", n, len(pts))
	}
	return nil
}

// metaOf is what a source's first stored chunk says about it, for its placer.
func metaOf(uri string, p map[string]any) Meta {
	return Meta{SourceURI: uri, SourceType: str(p["source_type"]), Scope: str(p["scope"]), Tags: strSlice(p["tags"]),
		Writer: str(p[fieldWriter]), Origin: originFrom(p[fieldOrigin]), Space: str(p[fieldSpace]),
		LabelsV: labelsV(p[fieldLabelsV])}
}

// holdsNewer reports whether dst already holds uri written under rev by a
// write other than the one being moved (whose ingest hash is hash).
func holdsNewer(ctx context.Context, dst *QdrantStore, uri string, rev int64, hash string) (bool, error) {
	got, err := dst.Retrieve(ctx, []string{pointID(uri, 0)}, []string{"source_uri", fieldLabelsV, fieldIngestHash})
	if errors.Is(err, errCollectionMissing) {
		return false, nil
	}
	if err != nil || len(got) == 0 {
		return false, err
	}
	p := got[0].Payload
	return str(p["source_uri"]) == uri && labelsV(p[fieldLabelsV]) == rev && str(p[fieldIngestHash]) != hash, nil
}

// copyPoints writes pts to dst as they are, with labels merged into their
// payloads. dst is created hybrid when missing; a dense chunk going to a
// hybrid collection gets its keyword vector from its stored text.
func (s *Service) copyPoints(ctx context.Context, dst *QdrantStore, pts []vectorPoint, labels map[string]any) error {
	out := make([]Point, 0, len(pts))
	for _, p := range pts {
		payload := make(map[string]any, len(p.Payload)+len(labels))
		for k, v := range p.Payload {
			payload[k] = v
		}
		for k, v := range labels {
			payload[k] = v
		}
		pt := Point{ID: p.ID, Vector: p.Dense, Sparse: p.Sparse, Payload: payload}
		if pt.Sparse.IsEmpty() {
			pt.Sparse = bm25.Document(str(p.Payload["text"]), s.BM25)
		}
		out = append(out, pt)
	}
	if len(out[0].Vector) == 0 {
		return fmt.Errorf("%w: it has no dense vector", errBadChunk)
	}
	if err := dst.EnsureCollection(ctx, len(out[0].Vector)); err != nil {
		return err
	}
	return dst.Upsert(ctx, out)
}

// num reads a stored number: JSON numbers decode as float64.
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// vectorPoint is a stored point with its payload and vectors.
type vectorPoint struct {
	ID      string
	Payload map[string]any
	Dense   []float32
	Sparse  bm25.Vector
}

// wirePoint is a point as Qdrant returns it with its vectors: one unnamed
// dense vector (dense layout) or named dense and keyword vectors (hybrid).
type wirePoint struct {
	ID      string          `json:"id"`
	Payload map[string]any  `json:"payload"`
	Vector  json.RawMessage `json:"vector"`
}

// decode reads w's vectors.
func (w wirePoint) decode() (vectorPoint, error) {
	p := vectorPoint{ID: w.ID, Payload: w.Payload}
	if len(w.Vector) == 0 || string(w.Vector) == "null" {
		return p, nil
	}
	if w.Vector[0] == '[' {
		return p, json.Unmarshal(w.Vector, &p.Dense)
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(w.Vector, &named); err != nil {
		return p, err
	}
	if d, ok := named[denseName]; ok {
		if err := json.Unmarshal(d, &p.Dense); err != nil {
			return p, err
		}
	}
	if sp, ok := named[sparseName]; ok {
		if err := json.Unmarshal(sp, &p.Sparse); err != nil {
			return p, err
		}
	}
	return p, nil
}

// sourcePoints reads every chunk of uri in st with its whole payload and,
// when vectors is set, its vectors.
func sourcePoints(ctx context.Context, st *QdrantStore, uri string, vectors bool) ([]vectorPoint, error) {
	var out []vectorPoint
	var offset any
	for page := 0; page < maxScrollPages; page++ {
		body := map[string]any{"limit": relabelPage, "with_payload": true, "with_vector": vectors,
			"filter": EqFilter("source_uri", uri)}
		if offset != nil {
			body["offset"] = offset
		}
		var resp struct {
			Result struct {
				Points         []wirePoint `json:"points"`
				NextPageOffset any         `json:"next_page_offset"`
			} `json:"result"`
		}
		if err := st.do(ctx, http.MethodPost, st.path("/points/scroll"), body, &resp); err != nil {
			if errors.Is(err, errCollectionMissing) {
				return nil, nil
			}
			return nil, err
		}
		for _, w := range resp.Result.Points {
			p, err := w.decode()
			if err != nil {
				return nil, fmt.Errorf("%w: its vectors do not decode: %v", errBadChunk, err)
			}
			out = append(out, p)
		}
		if resp.Result.NextPageOffset == nil {
			return out, nil
		}
		offset = resp.Result.NextPageOffset
	}
	return nil, fmt.Errorf("relabel: exceeded %d pages", maxScrollPages)
}

// WriteRelabelReport prints r for the operator: counts, and the spaces chunks
// go from and to. It never prints a source, its content or a collection.
func WriteRelabelReport(w io.Writer, r RelabelReport) {
	fmt.Fprintf(w, "policy revision %d: %d chunk(s), %d stale\n", r.Rev, r.Total, r.Stale)
	for _, t := range r.Transitions {
		fmt.Fprintf(w, "  %s -> %s: %d chunk(s)\n", t.From, t.To, t.Chunks)
	}
	verb := "would change"
	if r.Applied {
		verb = "changed"
	}
	fmt.Fprintf(w, "%s %d chunk(s), %d of them moved to another collection\n", verb, r.Changed, r.Moved)
	if r.Failed > 0 {
		fmt.Fprintf(w, "%d chunk(s) could not be placed and stay stale; run again to retry them\n", r.Failed)
	}
}
