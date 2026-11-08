package rag

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/bm25"
	"github.com/willesq/orchestrator/internal/qdrant"
)

// Spaces cost benchmark (SP-002). It measures what D-10, one Qdrant
// collection per space, costs against one collection filtered by payload:
// hybrid search latency over a 40,000-chunk synthetic corpus, and the wall
// time to relabel every chunk with a filtered set_payload.
const (
	benchChunks   = 40000
	benchSpaces   = 4
	benchQueries  = 200
	benchWarmup   = 20
	benchDim      = 384
	benchClusters = 256
	benchVocab    = 8000
	benchWords    = 110 // words per chunk, about one 512-token chunk of prose
	benchPerSrc   = 25  // chunks per source
)

// benchEmbedder places a text near one of the corpus's cluster centres, chosen
// by a hash of the text, so queries land in populated regions of the space.
type benchEmbedder struct{ centres [][]float32 }

func (e *benchEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		h := fnv.New64a()
		_, _ = h.Write([]byte(t))
		rng := rand.New(rand.NewSource(int64(h.Sum64())))
		out[i] = noisy(rng, e.centres[rng.Intn(len(e.centres))], 0.35)
	}
	return out, nil
}

func (e *benchEmbedder) Dimension(context.Context) (int, error) { return benchDim, nil }

func noisy(rng *rand.Rand, centre []float32, sigma float64) []float32 {
	v := make([]float32, len(centre))
	var n float64
	for i := range v {
		x := float64(centre[i]) + sigma*rng.NormFloat64()/math.Sqrt(float64(len(centre)))
		v[i] = float32(x)
		n += x * x
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] /= float32(n)
	}
	return v
}

type benchCorpus struct {
	emb     *benchEmbedder
	points  [benchSpaces][]Point
	queries []string
}

// newBenchCorpus builds a deterministic corpus: Zipf-distributed words over a
// synthetic vocabulary (with some code identifiers), dense vectors clustered
// around shared centres, and BM25 vectors from the real tokenizer.
func newBenchCorpus() *benchCorpus {
	rng := rand.New(rand.NewSource(20261108))
	centres := make([][]float32, benchClusters)
	zero := make([]float32, benchDim)
	for i := range centres {
		centres[i] = noisy(rng, zero, 1)
	}
	vocab := make([]string, benchVocab)
	for i := range vocab {
		switch {
		case i%40 == 0:
			vocab[i] = fmt.Sprintf("sync_meta_%d", i)
		case i%41 == 0:
			vocab[i] = fmt.Sprintf("loadConfig%d", i)
		default:
			vocab[i] = fmt.Sprintf("w%dx", i)
		}
	}
	zipf := rand.NewZipf(rng, 1.1, 2, benchVocab-1)
	words := func(n int) string {
		var b strings.Builder
		for j := range n {
			if j > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(vocab[zipf.Uint64()])
		}
		return b.String()
	}
	c := &benchCorpus{emb: &benchEmbedder{centres: centres}}
	now := time.Now().Unix()
	for i := range benchChunks {
		sp := i % benchSpaces
		src := i / benchSpaces / benchPerSrc
		idx := i / benchSpaces % benchPerSrc
		uri := fmt.Sprintf("bench://s%d/doc%04d.md", sp, src)
		text := words(benchWords)
		c.points[sp] = append(c.points[sp], Point{
			ID:     pointID(uri, idx),
			Vector: noisy(rng, centres[rng.Intn(benchClusters)], 0.35),
			Sparse: bm25.Document(text, bm25.DefaultParams),
			Payload: map[string]any{
				"text": text, "source_uri": uri, "source_type": "repo_doc", "scope": "work",
				"tags": []string{fmt.Sprintf("space:s%d", sp)}, "space": fmt.Sprintf("s%d", sp), "labels_v": 1,
				"chunk_index": idx, "chunk_total": benchPerSrc, "ingested_at": now,
				"started_at": now - int64(rng.Intn(400*86400)), "ended_at": now, fieldBM25Version: bm25.TokenizerVersion,
			},
		})
	}
	for range benchQueries + benchWarmup {
		c.queries = append(c.queries, words(3+rng.Intn(6)))
	}
	return c
}

// load creates a collection the way orchd does, adds the space index, writes
// points and waits until Qdrant has finished indexing them.
func (c *benchCorpus) load(t *testing.T, ctx context.Context, st *QdrantStore, pts []Point) {
	t.Helper()
	// ORCH_QDRANT_BENCH_SEGMENTS overrides Qdrant's default segment count
	// (one per CPU), which every request fans out over.
	if seg := os.Getenv("ORCH_QDRANT_BENCH_SEGMENTS"); seg != "" {
		n, err := strconv.Atoi(seg)
		if err != nil {
			t.Fatalf("ORCH_QDRANT_BENCH_SEGMENTS: %v", err)
		}
		if err := st.do(ctx, http.MethodPut, st.path(""), map[string]any{
			"vectors":           map[string]any{denseName: map[string]any{"size": benchDim, "distance": "Cosine"}},
			"sparse_vectors":    map[string]any{sparseName: map[string]any{"modifier": "idf"}},
			"optimizers_config": map[string]any{"default_segment_number": n},
		}, nil); err != nil {
			t.Fatal(err)
		}
		for _, f := range append([]string{"source_uri", fieldWriter}, searchIndexFields...) {
			if err := st.EnsureIndex(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.EnsureCollection(ctx, benchDim); err != nil {
		t.Fatalf("create %s: %v", st.Collection(), err)
	}
	if err := st.EnsureIndex(ctx, "space"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(pts); i += 2048 {
		if err := st.Upsert(ctx, pts[i:min(i+2048, len(pts))]); err != nil {
			t.Fatalf("upsert %s: %v", st.Collection(), err)
		}
	}
	waitGreen(t, ctx, st)
}

func waitGreen(t *testing.T, ctx context.Context, st *QdrantStore) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var info struct {
			Result struct {
				Status string `json:"status"`
			} `json:"result"`
		}
		if err := st.do(ctx, http.MethodGet, st.path(""), nil, &info); err != nil {
			t.Fatal(err)
		}
		if info.Result.Status == "green" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never went green", st.Collection())
}

// fuseRRF merges per-collection result lists by reciprocal rank, as a reader
// of several space collections would.
func fuseRRF(lists [][]Hit, topK int) []Hit {
	type acc struct {
		hit   Hit
		score float64
	}
	byKey := map[string]*acc{}
	for _, l := range lists {
		for r, h := range l {
			k := h.SourceURI + "\x00" + h.Text
			a := byKey[k]
			if a == nil {
				a = &acc{hit: h}
				byKey[k] = a
			}
			a.score += 1 / float64(60+r+1)
		}
	}
	all := make([]*acc, 0, len(byKey))
	for _, a := range byKey {
		all = append(all, a)
	}
	slices.SortFunc(all, func(a, b *acc) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.hit.SourceURI, b.hit.SourceURI)
	})
	out := make([]Hit, 0, topK)
	for _, a := range all[:min(topK, len(all))] {
		out = append(out, a.hit)
	}
	return out
}

type latencies []time.Duration

func (l latencies) pct(p float64) time.Duration {
	s := slices.Clone(l)
	slices.Sort(s)
	return s[min(len(s)-1, int(math.Ceil(p*float64(len(s))))-1)]
}

// TestIntegrationSpacesCost runs the SP-002 benchmark against the real
// native qdrant engine on isolated ports and temporary storage. Gated: set
// ORCH_QDRANT_BENCH=1 (and ORCH_QDRANT_BIN_DIR to reuse a downloaded binary;
// ORCH_QDRANT_BENCH_SEGMENTS to set the segments per collection).
func TestIntegrationSpacesCost(t *testing.T) {
	if os.Getenv("ORCH_QDRANT_BENCH") != "1" {
		t.Skip("set ORCH_QDRANT_BENCH=1 to run the spaces cost benchmark")
	}
	dir := t.TempDir()
	binDir := os.Getenv("ORCH_QDRANT_BIN_DIR")
	if binDir == "" {
		binDir = filepath.Join(dir, "bin")
	}
	p := qdrant.New(qdrant.Config{Version: "v1.17.1", BinDir: binDir, Storage: filepath.Join(dir, "store"), HTTPPort: 6421, GRPCPort: 6422})
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = p.Stop() }()
	if !p.Ready(ctx) {
		t.Fatal("engine not ready")
	}
	base := NewQdrantStore("http://127.0.0.1:6421", "bench-one", "")

	c := newBenchCorpus()
	start := time.Now()
	one := base.WithCollection("bench-one")
	var all []Point
	for _, pts := range c.points {
		all = append(all, pts...)
	}
	c.load(t, ctx, one, all)
	split := make([]*QdrantStore, benchSpaces)
	for i := range split {
		split[i] = base.WithCollection(fmt.Sprintf("bench-s%d", i))
		c.load(t, ctx, split[i], c.points[i])
	}
	t.Logf("loaded %d chunks into 1 + %d collections in %s", len(all), benchSpaces, time.Since(start).Round(time.Second))
	if n, _ := one.Count(ctx, nil); n != benchChunks {
		t.Fatalf("bench-one holds %d chunks", n)
	}

	const topK = 10
	allowed := []string{"space:s0", "space:s1"}
	svcOne := NewService(c.emb, one, 512, 64)
	svcA := NewService(c.emb, split[0], 512, 64)
	svcB := NewService(c.emb, split[1], 512, 64)

	baseline := func(q string) []Hit {
		hits, note, err := svcOne.Search(ctx, q, SearchOpts{TopK: topK, Tags: allowed})
		if err != nil || note != "" {
			t.Fatalf("baseline search %q: %v %s", q, err, note)
		}
		return hits
	}
	// The payload filter stays as the second layer (D-10), so each space
	// collection is still searched with its own space tag.
	multi := func(q string) []Hit {
		var wg sync.WaitGroup
		lists := make([][]Hit, 2)
		errs := make([]error, 2)
		for i, s := range []*Service{svcA, svcB} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var note string
				lists[i], note, errs[i] = s.Search(ctx, q, SearchOpts{TopK: topK, Tags: allowed[i : i+1]})
				if errs[i] == nil && note != "" {
					errs[i] = fmt.Errorf("fell back: %s", note)
				}
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("multi search %q: %v", q, err)
			}
		}
		return fuseRRF(lists, topK)
	}

	for _, q := range c.queries[:benchWarmup] {
		baseline(q)
		multi(q)
	}
	// Interleave the two so drift in machine load lands on both equally.
	var tb, tm latencies
	overlap := 0
	for _, q := range c.queries[benchWarmup:] {
		t0 := time.Now()
		hb := baseline(q)
		tb = append(tb, time.Since(t0))
		t0 = time.Now()
		hm := multi(q)
		tm = append(tm, time.Since(t0))
		for _, h := range hb {
			if !slices.Contains(allowed, h.Tags[0]) {
				t.Fatalf("baseline leaked %s", h.SourceURI)
			}
		}
		for _, h := range hm {
			if !slices.Contains(allowed, h.Tags[0]) {
				t.Fatalf("multi leaked %s", h.SourceURI)
			}
			for _, x := range hb {
				if x.SourceURI == h.SourceURI && x.Text == h.Text {
					overlap++
				}
			}
		}
	}

	// Breakdown: where a search's time goes, request by request, so a reader
	// can see which part a change to the layout or the query would move.
	timed := func(f func()) time.Duration { t0 := time.Now(); f(); return time.Since(t0) }
	type part struct {
		name string
		lat  latencies
		run  func(q string, vec []float32, bq bm25.Vector)
	}
	parts := []*part{
		{name: "one collection: dense request", run: func(_ string, v []float32, _ bm25.Vector) {
			_, _ = one.Search(ctx, v, 40, nil, allowed, nil, TimeRange{})
		}},
		{name: "one collection: keyword request (returns dense vectors)", run: func(_ string, _ []float32, bq bm25.Vector) {
			_, _ = one.KeywordSearch(ctx, bq, 80, nil, allowed, nil, TimeRange{})
		}},
		{name: "space collection: dense request", run: func(_ string, v []float32, _ bm25.Vector) {
			_, _ = split[0].Search(ctx, v, 40, nil, allowed[:1], nil, TimeRange{})
		}},
		{name: "space collection: keyword request (returns dense vectors)", run: func(_ string, _ []float32, bq bm25.Vector) {
			_, _ = split[0].KeywordSearch(ctx, bq, 80, nil, allowed[:1], nil, TimeRange{})
		}},
		{name: "space collection: keyword request without vectors", run: func(_ string, _ []float32, bq bm25.Vector) {
			_ = split[0].do(ctx, http.MethodPost, split[0].path("/points/query"), map[string]any{"query": bq, "using": sparseName,
				"limit": 80, "with_payload": true, "filter": buildFilter(nil, allowed[:1], nil, TimeRange{})}, nil)
		}},
		{name: "2 space collections: 4 raw requests in parallel", run: func(_ string, v []float32, bq bm25.Vector) {
			var wg sync.WaitGroup
			for _, st := range split[:2] {
				wg.Add(2)
				go func() { defer wg.Done(); _, _ = st.Search(ctx, v, 40, nil, allowed, nil, TimeRange{}) }()
				go func() { defer wg.Done(); _, _ = st.KeywordSearch(ctx, bq, 80, nil, allowed, nil, TimeRange{}) }()
			}
			wg.Wait()
		}},
		{name: "space collection: whole hybrid search", run: func(q string, _ []float32, _ bm25.Vector) {
			_, _, _ = svcA.Search(ctx, q, SearchOpts{TopK: topK, Tags: allowed[:1]})
		}},
		{name: "2 space collections: hybrid searches one after the other", run: func(q string, _ []float32, _ bm25.Vector) {
			_, _, _ = svcA.Search(ctx, q, SearchOpts{TopK: topK, Tags: allowed[:1]})
			_, _, _ = svcB.Search(ctx, q, SearchOpts{TopK: topK, Tags: allowed[1:]})
		}},
	}
	for _, q := range c.queries[benchWarmup:] {
		v, _ := c.emb.Embed(ctx, []string{q})
		bq := bm25.Query(q)
		for _, p := range parts {
			p.lat = append(p.lat, timed(func() { p.run(q, v[0], bq) }))
		}
	}
	for _, p := range parts {
		t.Logf("breakdown: %s: p50 %s p95 %s", p.name, p.lat.pct(0.5), p.lat.pct(0.95))
	}
	ratio := float64(tm.pct(0.95)) / float64(tb.pct(0.95))
	t.Logf("SP-002.1 baseline (1 collection, %d chunks, filter tags any %v): %d queries p50 %s p95 %s",
		benchChunks, allowed, len(tb), tb.pct(0.5), tb.pct(0.95))
	t.Logf("SP-002.2 multi (%d collections, search 2 in parallel + RRF): %d queries p50 %s p95 %s; p95 ratio %.2fx",
		benchSpaces, len(tm), tm.pct(0.5), tm.pct(0.95), ratio)
	t.Logf("top-%d agreement between layouts: %.1f%%", topK, 100*float64(overlap)/float64(topK*len(tb)))

	// Relabel: one set_payload whose filter matches all 40,000 chunks.
	relabel := func(st *QdrantStore, filter map[string]any) time.Duration {
		t0 := time.Now()
		err := st.do(ctx, http.MethodPost, st.path("/points/payload")+"?wait=true",
			map[string]any{"payload": map[string]any{"labels_v": 2, "space": "relabelled"}, "filter": filter}, nil)
		if err != nil {
			t.Fatalf("relabel %s: %v", st.Collection(), err)
		}
		return time.Since(t0)
	}
	v1 := map[string]any{"must": []any{map[string]any{"key": "labels_v", "match": map[string]any{"value": 1}}}}
	spaces := map[string]any{"must": []any{map[string]any{"key": "space", "match": map[string]any{"any": []string{"s0", "s1", "s2", "s3"}}}}}
	dOne := relabel(one, spaces)
	if n, _ := one.Count(ctx, EqFilter("space", "relabelled")); n != benchChunks {
		t.Fatalf("relabelled %d of %d", n, benchChunks)
	}
	var dSplit time.Duration
	for _, st := range split {
		dSplit += relabel(st, v1)
	}
	relabelled := 0
	for _, st := range split {
		n, _ := st.Count(ctx, EqFilter("space", "relabelled"))
		relabelled += n
	}
	if relabelled != benchChunks {
		t.Fatalf("relabelled %d of %d across space collections", relabelled, benchChunks)
	}
	t.Logf("SP-002.2 relabel by set_payload with a filter: %d chunks in one collection %s; across %d collections %s",
		benchChunks, dOne.Round(time.Millisecond), benchSpaces, dSplit.Round(time.Millisecond))

	if limit := 120 * time.Second; dOne > limit || dSplit > limit {
		t.Errorf("relabel over %s", limit)
	}
	// The latency threshold is reported, not enforced: SP-002.2 says to
	// document a result over 1.3x and revisit the layout decision.
	if ratio > 1.3 {
		t.Logf("multi-collection p95 is %.2fx baseline, over the 1.3x threshold", ratio)
	}
}
