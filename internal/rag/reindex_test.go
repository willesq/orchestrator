package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/bm25"
)

// hybridFake seeds a hybrid collection with chunks an older tokenizer indexed.
func hybridFake(t *testing.T, n int) (*fakeQdrant, *Service) {
	t.Helper()
	f, url := newFakeQdrant(t)
	var payloads []map[string]any
	for i := range n {
		payloads = append(payloads, map[string]any{"source_uri": fmt.Sprintf("repo://o/r/%d.md", i%3), "chunk_index": i,
			"text": fmt.Sprintf("set rag.search.keyword_weight %d", i)})
	}
	payloads = append(payloads, map[string]any{"source_uri": "repo://o/r/empty.md", "chunk_index": 0, "text": "the of"})
	f.seed("c", payloads...)
	f.collections["c"].layout = LayoutHybrid
	return f, NewService(&hookEmbedder{}, NewQdrantStore(url, "c", ""), 40, 4)
}

func TestReindexKeywords(t *testing.T) {
	f, svc := hybridFake(t, reindexPage+10)
	ctx := context.Background()
	plan, err := svc.ReindexKeywords(ctx, false)
	if err != nil || plan.Total != reindexPage+11 || plan.Stale != plan.Total || plan.Applied || plan.Updated != 0 || plan.Version != bm25.TokenizerVersion {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	for _, p := range f.collection("c").points {
		if _, ok := p.Payload[fieldBM25Version]; ok {
			t.Fatal("a plan wrote")
		}
	}
	rep, err := svc.ReindexKeywords(ctx, true)
	if err != nil || !rep.Applied || rep.Updated != plan.Stale {
		t.Fatalf("apply = %+v, %v", rep, err)
	}
	for id, p := range f.collection("c").points {
		if v, _ := p.Payload[fieldBM25Version].(float64); int(v) != bm25.TokenizerVersion {
			t.Fatalf("%s not stamped: %+v", id, p.Payload)
		}
		if strings.Contains(str(p.Payload["text"]), "keyword_weight") {
			var vec map[string]bm25.Vector
			if err := json.Unmarshal(p.Vector, &vec); err != nil {
				t.Fatalf("%s vector %s: %v", id, p.Vector, err)
			}
			want := bm25.Document(str(p.Payload["text"]), svc.BM25)
			if fmt.Sprint(vec[sparseName].Indices) != fmt.Sprint(want.Indices) {
				t.Fatalf("%s sparse = %v, want %v", id, vec[sparseName].Indices, want.Indices)
			}
		}
	}
	// A second run finds nothing to do.
	again, err := svc.ReindexKeywords(ctx, true)
	if err != nil || again.Stale != 0 || again.Updated != 0 {
		t.Fatalf("rerun = %+v, %v", again, err)
	}
}

func TestReindexResumesAfterAFailure(t *testing.T) {
	f, svc := hybridFake(t, 5)
	f.failVectors = 1
	if _, err := svc.ReindexKeywords(context.Background(), true); err == nil {
		t.Fatal("a failed update was not reported")
	}
	rep, err := svc.ReindexKeywords(context.Background(), true)
	if err != nil || rep.Updated != rep.Stale || rep.Stale == 0 {
		t.Fatalf("resume = %+v, %v", rep, err)
	}
}

func TestReindexSkipsChunksRewrittenSinceListed(t *testing.T) {
	f, svc := hybridFake(t, 2)
	id := pointID("repo://o/r/0.md", 0)
	f.collection("c").points[id].Payload[fieldBM25Version] = float64(bm25.TokenizerVersion)
	n, err := svc.reindexSource(context.Background(), svc.active(), "repo://o/r/0.md", []string{id})
	if err != nil || n != 0 {
		t.Fatalf("rewritten chunk re-indexed: %d, %v", n, err)
	}
}

func TestReindexRefusals(t *testing.T) {
	f, url := newFakeQdrant(t)
	f.seed("c", map[string]any{"source_uri": "u", "chunk_index": 0, "text": "x"})
	svc := NewService(&hookEmbedder{}, NewQdrantStore(url, "c", ""), 40, 4)
	if _, err := svc.ReindexKeywords(context.Background(), false); err == nil || !strings.Contains(err.Error(), "no keyword index") {
		t.Fatalf("dense collection err = %v", err)
	}
	_, svc = hybridFake(t, 1)
	svc.setShadow(svc.active().WithCollection("shadow"))
	if _, err := svc.ReindexKeywords(context.Background(), true); err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("during migration err = %v", err)
	}
	if pts, err := svc.active().Retrieve(context.Background(), nil, nil); pts != nil || err != nil {
		t.Fatal("empty retrieve")
	}
}

func TestWritesStampTheTokenizerVersion(t *testing.T) {
	svc, f := datedService(t)
	if _, err := svc.IngestText(context.Background(), "hello world", Meta{SourceURI: "manual://n"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.collection("c").points {
		if v, _ := p.Payload[fieldBM25Version].(float64); int(v) != bm25.TokenizerVersion {
			t.Fatalf("not stamped: %+v", p.Payload)
		}
	}
	ok, err := svc.HasSource(context.Background(), "manual://n")
	if err != nil || !ok {
		t.Fatalf("HasSource = %v, %v", ok, err)
	}
	if ok, _ := svc.HasSource(context.Background(), "manual://none"); ok {
		t.Fatal("HasSource of a missing source")
	}
	if ok, _ := svc.HasSource(context.Background(), ""); ok {
		t.Fatal("HasSource of an empty URI")
	}
}
