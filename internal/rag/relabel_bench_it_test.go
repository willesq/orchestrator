package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/qdrant"
)

// TestIntegrationRelabel40k measures SP-027.2.a1: relabelling 40,000 chunks,
// the SP-002 corpus stored unplaced in one base collection, so every chunk
// is placed and moved (with its vectors) to one of four space collections:
// the worst case, since a chunk that stays put is only re-stamped. It runs
// its own engine on 127.0.0.1:6471 with temporary storage and collections
// named sp-027-*. Gated like TestIntegrationSpacesCost: ORCH_QDRANT_BENCH=1,
// ORCH_QDRANT_BIN_DIR to reuse a downloaded engine.
func TestIntegrationRelabel40k(t *testing.T) {
	if os.Getenv("ORCH_QDRANT_BENCH") != "1" {
		t.Skip("set ORCH_QDRANT_BENCH=1 to run the relabel benchmark")
	}
	dir := t.TempDir()
	binDir := os.Getenv("ORCH_QDRANT_BIN_DIR")
	if binDir == "" {
		binDir = filepath.Join(dir, "bin")
	}
	p := qdrant.New(qdrant.Config{Version: "v1.17.1", BinDir: binDir, Storage: filepath.Join(dir, "store"), HTTPPort: 6471, GRPCPort: 6472})
	ctx := context.Background()
	if p.Ready(ctx) {
		t.Fatal("an engine is already serving 127.0.0.1:6471; stop it first")
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	c := newBenchCorpus()
	base := NewQdrantStore("http://127.0.0.1:6471", "sp-027-bench", "")
	var all []Point
	for _, pts := range c.points {
		for _, pt := range pts {
			// Stored before placement: no space, no labels_v.
			delete(pt.Payload, fieldSpace)
			delete(pt.Payload, fieldLabelsV)
			all = append(all, pt)
		}
	}
	c.load(t, ctx, base, all)
	svc := NewService(c.emb, base, 512, 64)
	place := func(_ context.Context, m *Meta) error {
		// bench://s<N>/doc.md belongs in space s<N>.
		sp, _, _ := strings.Cut(strings.TrimPrefix(m.SourceURI, "bench://"), "/")
		m.Space = sp
		return nil
	}

	start := time.Now()
	rep, err := svc.Relabel(ctx, RelabelOpts{Rev: 2, Place: place, Apply: true})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("relabelled %d chunks (%d moved, %d failed) in %s; transitions %v", rep.Changed, rep.Moved, rep.Failed, took.Round(time.Millisecond), rep.Transitions)
	if rep.Total != len(all) || rep.Changed != len(all) || rep.Moved != len(all) || rep.Failed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	n := 0
	for i := range c.points {
		k, err := base.WithCollection(SpaceCollection(base.Collection(), spaceOfBench(i))).Count(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		n += k
	}
	if left, err := base.Count(ctx, nil); err != nil || left != 0 || n != len(all) {
		t.Fatalf("after relabel: %d in the base (%v), %d in space collections, want 0 and %d", left, err, n, len(all))
	}
	again, err := svc.Relabel(ctx, RelabelOpts{Rev: 2, Place: place, Apply: true})
	if err != nil || again.Changed != 0 {
		t.Fatalf("second run = %+v, %v", again, err)
	}
	if took > 5*time.Minute {
		t.Fatalf("relabel of %d chunks took %s, over the 5 min threshold", len(all), took)
	}
}

func spaceOfBench(i int) string { return "s" + string(rune('0'+i)) }
