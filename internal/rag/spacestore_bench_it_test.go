package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/qdrant"
)

// TestIntegrationSpaceCollectionsSearch measures SP-024.3: search p95 through
// the layout SP-024 ships (Service.Search over the readable spaces'
// collections, merged and fused) against the SP-002 baseline (Service.Search
// on one collection of all 40,000 chunks filtered to the same spaces), on the
// SP-002 corpus. It runs its own engine on 127.0.0.1:6431 with temporary
// storage and collections named sp-024-*. Gated like TestIntegrationSpacesCost:
// ORCH_QDRANT_BENCH=1, ORCH_QDRANT_BIN_DIR to reuse a downloaded engine,
// ORCH_QDRANT_BENCH_SEGMENTS to set the segments per collection.
func TestIntegrationSpaceCollectionsSearch(t *testing.T) {
	if os.Getenv("ORCH_QDRANT_BENCH") != "1" {
		t.Skip("set ORCH_QDRANT_BENCH=1 to run the space collections benchmark")
	}
	dir := t.TempDir()
	binDir := os.Getenv("ORCH_QDRANT_BIN_DIR")
	if binDir == "" {
		binDir = filepath.Join(dir, "bin")
	}
	p := qdrant.New(qdrant.Config{Version: "v1.17.1", BinDir: binDir, Storage: filepath.Join(dir, "store"), HTTPPort: 6431, GRPCPort: 6432})
	ctx := context.Background()
	if p.Ready(ctx) {
		t.Fatal("an engine is already serving 127.0.0.1:6431; stop it first")
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	c := newBenchCorpus()
	one := NewQdrantStore("http://127.0.0.1:6431", "sp-024-one", "")
	var all []Point
	for _, pts := range c.points {
		all = append(all, pts...)
	}
	start := time.Now()
	c.load(t, ctx, one, all)
	base := one.WithCollection("sp-024-bench")
	for i := range c.points {
		c.load(t, ctx, base.WithCollection(SpaceCollection("sp-024-bench", fmt.Sprintf("s%d", i))), c.points[i])
	}
	t.Logf("loaded %d chunks into 1 + %d collections in %s", len(all), benchSpaces, time.Since(start).Round(time.Second))

	const topK = 10
	allowed := []string{"s0", "s1"}
	svcOne := NewService(c.emb, one, 512, 64)
	svc := NewService(c.emb, base, 512, 64)
	// The same one collection, searched with the request pattern several
	// collections use, separates the layout's cost from the request's.
	svcLazy := NewService(c.emb, one, 512, 64)
	svcLazy.lazyVectors = true
	baseline := func(q string) []Hit {
		hits, note, err := svcOne.Search(ctx, q, SearchOpts{TopK: topK, Tags: []string{"space:s0", "space:s1"}})
		if err != nil || note != "" {
			t.Fatalf("baseline %q: %v %s", q, err, note)
		}
		return hits
	}
	spaced := func(q string) []Hit {
		hits, note, err := svc.Search(ctx, q, SearchOpts{TopK: topK, Spaces: allowed})
		if err != nil || note != "" {
			t.Fatalf("space search %q: %v %s", q, err, note)
		}
		return hits
	}
	for _, q := range c.queries[:benchWarmup] {
		baseline(q)
		spaced(q)
	}
	lazyOne := func(q string) {
		if _, note, err := svcLazy.Search(ctx, q, SearchOpts{TopK: topK, Tags: []string{"space:s0", "space:s1"}}); err != nil || note != "" {
			t.Fatalf("lazy baseline %q: %v %s", q, err, note)
		}
	}
	for _, q := range c.queries[:benchWarmup] {
		lazyOne(q)
	}
	var tb, ts, tl latencies
	same := 0
	for _, q := range c.queries[benchWarmup:] {
		t0 := time.Now()
		hb := baseline(q)
		tb = append(tb, time.Since(t0))
		t0 = time.Now()
		lazyOne(q)
		tl = append(tl, time.Since(t0))
		t0 = time.Now()
		hs := spaced(q)
		ts = append(ts, time.Since(t0))
		for _, h := range hs {
			if sp := h.Tags[0]; sp != "space:s0" && sp != "space:s1" {
				t.Fatalf("a space search returned %s from %s", h.SourceURI, sp)
			}
			for _, x := range hb {
				if x.SourceURI == h.SourceURI && x.Text == h.Text {
					same++
				}
			}
		}
	}
	ratio := float64(ts.pct(0.95)) / float64(tb.pct(0.95))
	t.Logf("SP-024.3 baseline (1 collection, %d chunks, filtered to 2 spaces): %d queries p50 %s p95 %s",
		benchChunks, len(tb), tb.pct(0.5), tb.pct(0.95))
	t.Logf("SP-024.3 space collections (Search over %v of %d): p50 %s p95 %s; p95 ratio %.2fx",
		allowed, benchSpaces, ts.pct(0.5), ts.pct(0.95), ratio)
	t.Logf("SP-024.3 one collection with the same request pattern: p50 %s p95 %s; space collections are %.2fx that at p95",
		tl.pct(0.5), tl.pct(0.95), float64(ts.pct(0.95))/float64(tl.pct(0.95)))
	t.Logf("top-%d agreement with the baseline: %.1f%%", topK, 100*float64(same)/float64(topK*len(tb)))
	if ratio > 1.3 {
		t.Errorf("space collections p95 is %.2fx the baseline, over 1.3x", ratio)
	}
}
