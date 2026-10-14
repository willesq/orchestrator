package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/bm25"
)

func TestLayoutAndHybridWire(t *testing.T) {
	f, url := newFakeQdrant(t)
	ctx := context.Background()
	f.seed("old", map[string]any{"source_uri": "x", "chunk_index": 0, "text": "x"})
	old := NewQdrantStore(url, "old", "")
	if l, err := old.Layout(ctx); err != nil || l != LayoutDense || l.String() != "dense" {
		t.Fatalf("old layout = %v, %v", l, err)
	}
	fresh := NewQdrantStore(url, "fresh", "")
	if l, _ := fresh.Layout(ctx); l != LayoutUnknown || l.String() != "unknown" {
		t.Fatalf("missing layout = %v", l)
	}
	// A new collection is created hybrid.
	if err := fresh.EnsureCollection(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if l, _ := fresh.Layout(ctx); l != LayoutHybrid || l.String() != "hybrid" {
		t.Fatalf("new layout = %v", l)
	}
	// EnsureCollection leaves an existing dense collection as it is.
	if err := old.EnsureCollection(ctx, 2); err != nil || f.collection("old").layout != LayoutDense {
		t.Fatal("existing collection changed")
	}
	pts := []Point{
		{ID: "a", Vector: []float32{1, 0}, Sparse: bm25.Document("sync outbox", bm25.DefaultParams), Payload: map[string]any{"source_uri": "a"}},
		{ID: "b", Vector: []float32{1, 0}, Sparse: bm25.Document("", bm25.DefaultParams), Payload: map[string]any{"source_uri": "b"}},
	}
	if err := fresh.Upsert(ctx, pts); err != nil {
		t.Fatal(err)
	}
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(f.collection("fresh").points["a"].Vector, &a)
	_ = json.Unmarshal(f.collection("fresh").points["b"].Vector, &b)
	if a["dense"] == nil || a["bm25"] == nil || b["dense"] == nil || b["bm25"] != nil {
		t.Fatalf("hybrid wire: a=%v b=%v (an empty sparse vector must be left out)", a, b)
	}
	if err := old.Upsert(ctx, pts[:1]); err != nil {
		t.Fatal(err)
	}
	if v := string(f.collection("old").points["a"].Vector); !strings.HasPrefix(v, "[") {
		t.Fatalf("dense wire = %s", v)
	}
	if got := fresh.WithCollection("other").Collection(); got != "other" {
		t.Fatalf("WithCollection = %s", got)
	}
}

func TestHybridSearchUsesTheNamedVector(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			collectionJSON(w, LayoutHybrid)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, map[string]any{"result": []any{}})
	}))
	t.Cleanup(srv.Close)
	q := NewQdrantStore(srv.URL, "c", "")
	if _, err := q.Search(context.Background(), []float32{1, 0}, 3, nil, nil); err != nil {
		t.Fatal(err)
	}
	vec, _ := body["vector"].(map[string]any)
	if vec["name"] != "dense" {
		t.Fatalf("search body = %v", body)
	}
}

func TestLayoutErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	q := NewQdrantStore(srv.URL, "c", "")
	if _, err := q.Layout(context.Background()); err == nil {
		t.Fatal("server error not reported")
	}
	if err := q.Upsert(context.Background(), []Point{{ID: "a"}}); err == nil {
		t.Fatal("upsert without a layout succeeded")
	}
	if _, err := q.Search(context.Background(), nil, 1, nil, nil); err == nil {
		t.Fatal("search without a layout succeeded")
	}
}

func TestUpsertBatchesLargeWrites(t *testing.T) {
	f, base := newFakeQdrant(t)
	f.seed("c")
	q := NewQdrantStore(base, "c", "")
	pts := make([]Point, upsertBatch*2+1)
	for i := range pts {
		pts[i] = Point{ID: pointID("u", i), Vector: []float32{1, 0}, Payload: map[string]any{"source_uri": "u"}}
	}
	if err := q.Upsert(context.Background(), pts); err != nil {
		t.Fatal(err)
	}
	if got := f.upserts["c"]; got != 3 {
		t.Fatalf("%d write requests, want 3", got)
	}
	if got := len(f.collection("c").points); got != len(pts) {
		t.Fatalf("stored %d points, want %d", got, len(pts))
	}
}

func TestPartialUpsertIsReported(t *testing.T) {
	f, base := newFakeQdrant(t)
	f.seed("c")
	f.failUpsertNo["c"] = 2
	q := NewQdrantStore(base, "c", "")
	pts := make([]Point, upsertBatch+1)
	for i := range pts {
		pts[i] = Point{ID: pointID("u", i), Vector: []float32{1, 0}, Payload: map[string]any{"source_uri": "u", "chunk_index": i}}
	}
	err := writePoints(context.Background(), q, 2, "u", pts)
	var partial *PartialWriteError
	if !errors.As(err, &partial) || partial.Written != upsertBatch || partial.Total != len(pts) ||
		!strings.Contains(err.Error(), "re-ingest") {
		t.Fatalf("err = %v", err)
	}
	// A failure on the first batch writes nothing and is not partial.
	f.upserts["c"], f.failUpsertNo["c"] = 0, 1
	if err := q.Upsert(context.Background(), pts[:1]); err == nil || errors.As(err, &partial) {
		t.Fatalf("first-batch err = %v", err)
	}
}
