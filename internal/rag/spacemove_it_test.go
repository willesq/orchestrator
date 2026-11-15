package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/qdrant"
	"github.com/willesq/orchestrator/internal/spaces"
)

// TestIntegrationSpaceMoveRoundTrip runs the spaces switch's store moves on
// a real engine: content stored in four spaces, quarantine and Shared is
// merged into the base (spaces off), searched, then split back (spaces on),
// each direction interrupted once and resumed. It runs its own engine on
// 127.0.0.1:6481 with temporary storage and collections named spaces-off-*,
// never orch's own. Gated like the other engine tests: ORCH_QDRANT_BENCH=1,
// and ORCH_QDRANT_BIN_DIR to reuse a downloaded engine.
func TestIntegrationSpaceMoveRoundTrip(t *testing.T) {
	if os.Getenv("ORCH_QDRANT_BENCH") != "1" {
		t.Skip("set ORCH_QDRANT_BENCH=1 to run the store move on a real engine")
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

	embeds := 0
	emb := embedCounter{Embedder: &benchEmbedder{centres: newBenchCorpus().emb.centres}, n: &embeds}
	svc := NewService(emb, NewQdrantStore("http://127.0.0.1:6481", "spaces-off-it", ""), 110, 16)
	svc.Place = func(_ context.Context, m *Meta) error {
		// it://<space>/<n> belongs in <space>.
		sp, _, _ := strings.Cut(strings.TrimPrefix(m.SourceURI, "it://"), "/")
		m.Space, m.LabelsV = sp, 3
		return nil
	}
	baseline := NewService(emb, NewQdrantStore("http://127.0.0.1:6481", "spaces-off-baseline", ""), 110, 16)
	homes := []string{"acme", "globex", "initech", "umbrella", spaces.Quarantine, spaces.Shared}
	var uris []string
	for i := 0; i < 240; i++ {
		uri := fmt.Sprintf("it://%s/%d", homes[i%len(homes)], i)
		text := fmt.Sprintf("Runbook %d for the %s deploy: rotate keys, drain workers, check sync_meta_%d, then page the on-call. %s",
			i, homes[i%len(homes)], i%17, strings.Repeat("Notes on retries and backoff. ", 1+i%5))
		for _, s := range []*Service{svc, baseline} {
			if _, err := s.IngestText(ctx, text, Meta{SourceURI: uri, SourceType: "note", Origin: Origin{System: OriginClient}}); err != nil {
				t.Fatal(err)
			}
		}
		uris = append(uris, uri)
	}
	before := realPoints(t, ctx, svc, uris)
	// moveEmbeds counts embeds during the moves alone; searches embed.
	moveEmbeds := 0
	moved := func(run func()) {
		n := embeds
		run()
		moveEmbeds += embeds - n
	}
	queries := []string{"rotate keys before deploy", "sync_meta_3", "drain workers and page the on-call", "retries and backoff"}

	// Off: interrupt the merge after a few sources, then resume it.
	cctx, cancel := context.WithCancel(ctx)
	moved(func() {
		if _, err := svc.MergeSpaces(cctx, func(r MoveReport) {
			if r.Sources == 25 {
				cancel()
			}
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted merge: %v", err)
		}
	})
	if got := searchAll(t, ctx, svc, queries); len(got) == 0 {
		t.Fatal("nothing found mid-merge")
	}
	moved(func() {
		if rep, err := svc.MergeSpaces(ctx, nil); err != nil || rep.Failed != 0 {
			t.Fatalf("merge = %+v, %v", rep, err)
		}
	})
	cols, err := svc.active().Collections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		if strings.HasPrefix(c, "spaces-off-it__") {
			t.Fatalf("collection %s left after the merge", c)
		}
	}
	if got, want := searchAll(t, ctx, svc, queries), searchAll(t, ctx, baseline, queries); !reflect.DeepEqual(got, want) {
		t.Fatalf("merged search differs from one store:\n%v\n%v", got, want)
	}
	merged := realPoints(t, ctx, svc, uris)
	for k, p := range before {
		if m := merged[k]; m.payload != p.payload || m.vector != p.vector {
			t.Fatalf("%s changed in the merge", k)
		}
	}

	// On: interrupt the split, then resume it.
	cctx, cancel = context.WithCancel(ctx)
	moved(func() {
		if _, err := svc.SplitSpaces(cctx, func(r MoveReport) {
			if r.Sources == 25 {
				cancel()
			}
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted split: %v", err)
		}
		if rep, err := svc.SplitSpaces(ctx, nil); err != nil || rep.Failed != 0 {
			t.Fatalf("split = %+v, %v", rep, err)
		}
	})
	if after := realPoints(t, ctx, svc, uris); !reflect.DeepEqual(after, before) {
		t.Fatal("the round trip changed the store")
	}
	if moveEmbeds != 0 {
		t.Fatalf("the moves embedded %d time(s)", moveEmbeds)
	}
}

// realPoint is one stored chunk: where it is, its payload and its dense
// vector.
type realPoint struct{ collection, payload, vector string }

// realPoints reads every chunk of uris from whichever collection holds it.
func realPoints(t *testing.T, ctx context.Context, svc *Service, uris []string) map[string]realPoint {
	t.Helper()
	all, err := svc.allStores(ctx, svc.active(), true)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]realPoint{}
	for _, ss := range all {
		for _, uri := range uris {
			pts, err := sourcePoints(ctx, ss.st, uri, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range pts {
				key := p.ID
				if prev, ok := out[key]; ok {
					t.Fatalf("%s in both %s and %s", key, prev.collection, ss.st.Collection())
				}
				pl, _ := json.Marshal(p.Payload)
				vec, _ := json.Marshal(p.Dense)
				out[key] = realPoint{collection: ss.st.Collection(), payload: string(pl), vector: string(vec)}
			}
		}
	}
	return out
}

// searchAll runs each query over every collection and lists the hits'
// sources and scores, sorted. Reason for dense: the engine computes keyword
// IDF per segment, so two collections holding the same chunks, written in
// another order, rank keyword ties differently; cosine is exact at this
// size, so a dense search must match one store exactly.
func searchAll(t *testing.T, ctx context.Context, svc *Service, queries []string) []string {
	t.Helper()
	var out []string
	for _, q := range queries {
		hits, _, err := svc.Search(ctx, q, SearchOpts{TopK: 10, Mode: ModeDense})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			out = append(out, fmt.Sprintf("%s|%s|%.5f", q, h.SourceURI, h.Score))
		}
		// A hybrid search of the merged store works and finds something.
		if hits, _, err := svc.Search(ctx, q, SearchOpts{TopK: 10}); err != nil || len(hits) == 0 {
			t.Fatalf("hybrid search %q: %d hit(s), %v", q, len(hits), err)
		}
	}
	sort.Strings(out)
	return out
}
