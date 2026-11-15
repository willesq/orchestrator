package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/qdrant"
)

// TestIntegrationRetag2000 measures SP-071.2.a3: retagging one 2,000-chunk
// source from one space's collection to another's, with its vectors, on a
// real engine. It runs its own engine on 127.0.0.1:6481 with temporary
// storage and collections named sp-071-*. Gated like the other benchmarks:
// ORCH_QDRANT_BENCH=1, ORCH_QDRANT_BIN_DIR to reuse a downloaded engine.
func TestIntegrationRetag2000(t *testing.T) {
	if os.Getenv("ORCH_QDRANT_BENCH") != "1" {
		t.Skip("set ORCH_QDRANT_BENCH=1 to run the retag benchmark")
	}
	dir := t.TempDir()
	binDir := os.Getenv("ORCH_QDRANT_BIN_DIR")
	if binDir == "" {
		binDir = filepath.Join(dir, "bin")
	}
	p := qdrant.New(qdrant.Config{Version: "v1.17.1", BinDir: binDir, Storage: filepath.Join(dir, "store"), HTTPPort: 6481, GRPCPort: 6482})
	ctx := context.Background()
	if p.Ready(ctx) {
		t.Fatal("an engine is already serving 127.0.0.1:6481; stop it first")
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	const chunks, uri = 2000, "bench://big/doc.md"
	c := newBenchCorpus()
	pts := make([]Point, 0, chunks)
	for _, sp := range c.points {
		for _, pt := range sp {
			if len(pts) == chunks {
				break
			}
			i := len(pts)
			pt.ID = pointID(uri, i)
			pt.Payload["source_uri"], pt.Payload["chunk_index"], pt.Payload["chunk_total"] = uri, i, chunks
			pt.Payload[fieldSpace] = "s0"
			pts = append(pts, pt)
		}
	}
	if len(pts) != chunks {
		t.Fatalf("the corpus has %d chunks, want %d", len(pts), chunks)
	}
	base := NewQdrantStore("http://127.0.0.1:6481", "sp-071-bench", "")
	from := base.WithCollection(SpaceCollection(base.Collection(), "s0"))
	c.load(t, ctx, from, pts)
	embeds := 0
	svc := NewService(embedCounter{Embedder: c.emb, n: &embeds}, base, 512, 64)

	start := time.Now()
	rep, err := svc.Retag(ctx, RetagOpts{SourceURI: uri, Space: "s1", Rev: 2})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retagged a %d-chunk source in %s; report %+v", chunks, took.Round(time.Millisecond), rep)
	to := base.WithCollection(SpaceCollection(base.Collection(), "s1"))
	left, err := from.Count(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := to.Count(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if left != 0 || moved != chunks || rep.Chunks != chunks || embeds != 0 {
		t.Fatalf("after the retag: %d left, %d moved, report %+v, %d embed call(s)", left, moved, rep, embeds)
	}
	if took > 20*time.Second {
		t.Fatalf("retagging %d chunks took %s, over the 20 s threshold", chunks, took)
	}
}
