package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixedEmbedder embeds every text as the same unit vector.
type fixedEmbedder struct{ vec []float32 }

func (f fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = f.vec
	}
	return out, nil
}
func (f fixedEmbedder) Dimension(context.Context) (int, error) { return len(f.vec), nil }

// hybridStub serves a hybrid collection whose dense and keyword searches
// return fixed lists, recording which were called.
func hybridStub(t *testing.T, layout Layout, dense, keyword []map[string]any, keywordFails bool) (*QdrantStore, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var denseCalls, keywordCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			collectionJSON(w, layout)
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			denseCalls.Add(1)
			writeJSON(w, map[string]any{"result": dense})
		case strings.HasSuffix(r.URL.Path, "/points/query"):
			keywordCalls.Add(1)
			if keywordFails {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"result": map[string]any{"points": keyword}})
		}
	}))
	t.Cleanup(srv.Close)
	return NewQdrantStore(srv.URL, "c", ""), &denseCalls, &keywordCalls
}

func hit(id string, score float64, text string) map[string]any {
	return map[string]any{"id": id, "score": score, "payload": map[string]any{"text": text, "source_uri": "u://" + id}}
}

func TestHybridSearchFusesByRank(t *testing.T) {
	dense := []map[string]any{hit("a", 0.9, "alpha"), hit("b", 0.8, "beta"), hit("c", 0.3, "has sync_meta here")}
	kd := hit("d", 3.0, "the sync_meta table")
	kd["vector"] = map[string]any{"dense": []float32{0.6, 0.8}}
	keyword := []map[string]any{hit("c", 5.0, "has sync_meta here"), kd}
	st, dc, kc := hybridStub(t, LayoutHybrid, dense, keyword, false)
	svc := NewService(fixedEmbedder{[]float32{1, 0}}, st, 110, 16)
	hits, note, err := svc.Search(context.Background(), "where is sync_meta", SearchOpts{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("note = %q", note)
	}
	if dc.Load() != 1 || kc.Load() != 1 {
		t.Fatalf("calls dense=%d keyword=%d", dc.Load(), kc.Load())
	}
	// RRF with k=60: c is 3rd in dense and 1st in keyword, so it leads.
	var got []string
	for _, h := range hits {
		got = append(got, strings.TrimPrefix(h.SourceURI, "u://"))
	}
	if !slices.Equal(got, []string{"c", "a", "b", "d"}) {
		t.Fatalf("order = %v", got)
	}
	byURI := map[string]Hit{}
	for _, h := range hits {
		byURI[h.SourceURI] = h
	}
	if c := byURI["u://c"]; c.Score != 0.3 || c.Keyword != 5 || !c.Exact {
		t.Fatalf("c = %+v: its cosine must come from the dense list", c)
	}
	// d was found only by keyword: its cosine is computed from its vector.
	if d := byURI["u://d"]; math.Abs(float64(d.Score)-0.6) > 1e-6 || d.Keyword != 3 || !d.Exact {
		t.Fatalf("d = %+v", d)
	}
	if a := byURI["u://a"]; a.Keyword != 0 || a.Exact {
		t.Fatalf("a = %+v", a)
	}
}

func TestSearchModes(t *testing.T) {
	dense := []map[string]any{hit("a", 0.9, "alpha")}
	keyword := []map[string]any{hit("b", 2, "beta")}
	ctx := context.Background()
	newSvc := func(t *testing.T, layout Layout, keywordFails bool) (*Service, *atomic.Int32) {
		st, _, kc := hybridStub(t, layout, dense, keyword, keywordFails)
		return NewService(fixedEmbedder{[]float32{1, 0}}, st, 110, 16), kc
	}
	t.Run("dense mode on a hybrid collection", func(t *testing.T) {
		svc, kc := newSvc(t, LayoutHybrid, false)
		hits, note, err := svc.Search(ctx, "q", SearchOpts{Mode: ModeDense})
		if err != nil || note != "" || len(hits) != 1 || kc.Load() != 0 {
			t.Fatalf("hits=%v note=%q err=%v keyword calls=%d", hits, note, err, kc.Load())
		}
	})
	t.Run("configured hybrid on a dense collection falls back with a note", func(t *testing.T) {
		svc, kc := newSvc(t, LayoutDense, false)
		hits, note, err := svc.Search(ctx, "q", SearchOpts{})
		if err != nil || len(hits) != 1 || kc.Load() != 0 || !strings.Contains(note, "no keyword index") {
			t.Fatalf("hits=%v note=%q err=%v keyword calls=%d", hits, note, err, kc.Load())
		}
	})
	t.Run("explicit hybrid on a dense collection fails", func(t *testing.T) {
		svc, _ := newSvc(t, LayoutDense, false)
		if _, _, err := svc.Search(ctx, "q", SearchOpts{Mode: ModeHybrid}); !errors.Is(err, ErrHybridUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the service mode applies when the request names none", func(t *testing.T) {
		svc, kc := newSvc(t, LayoutHybrid, false)
		svc.SearchMode = ModeDense
		if _, note, err := svc.Search(ctx, "q", SearchOpts{}); err != nil || note != "" || kc.Load() != 0 {
			t.Fatalf("note=%q err=%v keyword calls=%d", note, err, kc.Load())
		}
	})
	t.Run("a failing keyword search answers by meaning with a note", func(t *testing.T) {
		svc, _ := newSvc(t, LayoutHybrid, true)
		hits, note, err := svc.Search(ctx, "beta", SearchOpts{})
		if err != nil || len(hits) != 1 || hits[0].SourceURI != "u://a" || !strings.Contains(note, "keyword search failed") {
			t.Fatalf("hits=%v note=%q err=%v", hits, note, err)
		}
	})
	t.Run("an explicit hybrid search fails when keyword search does", func(t *testing.T) {
		svc, _ := newSvc(t, LayoutHybrid, true)
		if _, _, err := svc.Search(ctx, "beta", SearchOpts{Mode: ModeHybrid}); !errors.Is(err, ErrHybridUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an unreadable layout falls back, or fails when explicit", func(t *testing.T) {
		st := NewQdrantStore("http://127.0.0.1:1", "c", "")
		svc := NewService(fixedEmbedder{[]float32{1, 0}}, st, 110, 16)
		if _, _, err := svc.Search(ctx, "q", SearchOpts{Mode: ModeHybrid}); !errors.Is(err, ErrHybridUnavailable) {
			t.Fatalf("explicit err = %v", err)
		}
		if _, _, err := svc.Search(ctx, "q", SearchOpts{}); err == nil || errors.Is(err, ErrHybridUnavailable) {
			t.Fatalf("implicit err = %v: want the dense search's own error", err)
		}
	})
	t.Run("a stopword-only query skips keyword search", func(t *testing.T) {
		svc, kc := newSvc(t, LayoutHybrid, false)
		if _, _, err := svc.Search(ctx, "the of", SearchOpts{}); err != nil || kc.Load() != 0 {
			t.Fatalf("keyword calls=%d", kc.Load())
		}
	})
}

func TestHybridSearchCapsEachSource(t *testing.T) {
	// One source repeats the term in many chunks; it may place at most
	// perSourceCap of them, leaving room for the other source.
	var keyword []map[string]any
	for i := range 6 {
		h := hit(fmt.Sprintf("f%d", i), float64(9-i), "flood sync_meta")
		h["payload"].(map[string]any)["source_uri"] = "u://flood"
		keyword = append(keyword, h)
	}
	keyword = append(keyword, hit("good", 1, "sync_meta table"))
	st, _, _ := hybridStub(t, LayoutHybrid, nil, keyword, false)
	svc := NewService(fixedEmbedder{[]float32{1, 0}}, st, 110, 16)
	hits, _, err := svc.Search(context.Background(), "sync_meta", SearchOpts{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, h := range hits {
		count[h.SourceURI]++
	}
	if count["u://flood"] != perSourceCap || count["u://good"] != 1 || len(hits) != perSourceCap+1 {
		t.Fatalf("hits per source = %v", count)
	}
}

func TestHybridSearchKeywordWeight(t *testing.T) {
	// a leads dense, b leads keyword; their ranks tie, so the weight decides.
	dense := []map[string]any{hit("a", 0.9, "alpha"), hit("b", 0.5, "beta")}
	keyword := []map[string]any{hit("b", 5, "beta"), hit("a", 1, "alpha")}
	for _, tt := range []struct {
		weight float64
		first  string
	}{{0.5, "u://a"}, {2, "u://b"}, {math.NaN(), "u://a"}} {
		st, _, _ := hybridStub(t, LayoutHybrid, dense, keyword, false)
		svc := NewService(fixedEmbedder{[]float32{1, 0}}, st, 110, 16)
		svc.KeywordWeight = tt.weight
		hits, _, err := svc.Search(context.Background(), "beta alpha", SearchOpts{TopK: 2})
		if err != nil || len(hits) != 2 || hits[0].SourceURI != tt.first {
			t.Fatalf("weight %v: hits=%v err=%v", tt.weight, hits, err)
		}
	}
}

func TestSearchClampsTopK(t *testing.T) {
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			collectionJSON(w, LayoutHybrid)
			return
		}
		var body struct {
			Limit int64 `json:"limit"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Limit > asked.Load() {
			asked.Store(body.Limit)
		}
		if strings.HasSuffix(r.URL.Path, "/points/query") {
			writeJSON(w, map[string]any{"result": map[string]any{"points": []any{}}})
			return
		}
		writeJSON(w, map[string]any{"result": []any{}})
	}))
	t.Cleanup(srv.Close)
	svc := NewService(fixedEmbedder{[]float32{1, 0}}, NewQdrantStore(srv.URL, "c", ""), 110, 16)
	if _, _, err := svc.Search(context.Background(), "sync_meta", SearchOpts{TopK: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if got := asked.Load(); got != maxCandidates {
		t.Fatalf("largest limit asked of qdrant = %d, want %d", got, maxCandidates)
	}
}

func TestQueryIdentifiers(t *testing.T) {
	got := queryIdentifiers("set queue.worker.verify.repos and ProcessActivity, VALID_SOURCE_TYPES, internal/testsupport, cmd/orch/main, rag.service, plain words, x.y")
	want := []string{"queue.worker.verify.repos", "processactivity", "valid_source_types", "internal/testsupport", "cmd/orch/main", "rag.service"}
	if !slices.Equal(got, want) {
		t.Fatalf("identifiers = %q", got)
	}
	// Ordinary prose, numbers, versions, abbreviations and common file names
	// are not code identifiers.
	for _, prose := range []string{
		"how does sync work",
		"follow-up on the e.g. and i.e. notes at 10:30",
		"bump to 1.5 or v1.17.1, about 2026/10/14",
		"edit go.mod, README.md and package.json",
		"the well-known re-run step",
		"use and/or, on/off, true/false, input/output and pros/cons, a.k.a. the usual",
		"see github.com, docs.python.org and api.github.com",
		"edit config.yaml, notes.txt and settings.json",
	} {
		if ids := queryIdentifiers(prose); ids != nil {
			t.Errorf("queryIdentifiers(%q) = %q", prose, ids)
		}
	}
	long := strings.Repeat("word ", maxIdentQuery) + "late.identifier"
	if ids := queryIdentifiers(long); ids != nil {
		t.Errorf("identifiers past the query cap = %q", ids)
	}
	many := strings.Repeat("alpha.beta ", 100)
	if ids := queryIdentifiers(many); len(ids) != maxQueryIdents {
		t.Errorf("got %d identifiers, want the cap %d", len(ids), maxQueryIdents)
	}
}

func TestContainsAnyMatchesWholeTokens(t *testing.T) {
	ids := []string{"internal/testsupport", "sync_meta"}
	for text, want := range map[string]bool{
		"See INTERNAL/TestSupport docs": true,
		"the sync_meta table":           true,
		"see internal/testsupport/x.go": false, // a longer path is a different name
		"the sync_metadata table":       false,
		"resync_meta":                   false,
		"":                              false,
	} {
		if got := containsAny(text, ids); got != want {
			t.Errorf("containsAny(%q) = %v, want %v", text, got, want)
		}
	}
	if containsAny("x", nil) {
		t.Fatal("no identifiers matched")
	}
}

func TestRateLog(t *testing.T) {
	var r rateLog
	r.printf("first %d", 1)
	first := r.last
	r.printf("second %d", 2)
	if r.last != first {
		t.Fatal("a second line within the minute was logged")
	}
}

func TestCosineWith(t *testing.T) {
	raw, _ := json.Marshal([]float32{0, 2})
	if got := cosineWith([]float32{0, 1}, raw); math.Abs(float64(got)-1) > 1e-6 {
		t.Fatalf("cosine = %v", got)
	}
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`"x"`), json.RawMessage(`[1,2,3]`), json.RawMessage(`[0,0]`)} {
		if cosineWith([]float32{1, 0}, bad) != 0 {
			t.Fatalf("bad vector %s gave a cosine", bad)
		}
	}
}

func TestHybridSearchLeavesDenseUncapped(t *testing.T) {
	// A search narrowed to one source still fills from the dense list.
	var dense []map[string]any
	for i := range 5 {
		h := hit(fmt.Sprintf("s%d", i), 0.9-float64(i)/10, "chunk")
		h["payload"].(map[string]any)["source_uri"] = "u://one"
		dense = append(dense, h)
	}
	st, _, _ := hybridStub(t, LayoutHybrid, dense, nil, false)
	svc := NewService(fixedEmbedder{[]float32{1, 0}}, st, 110, 16)
	hits, _, err := svc.Search(context.Background(), "chunk", SearchOpts{TopK: 5})
	if err != nil || len(hits) != 5 {
		t.Fatalf("hits=%d err=%v", len(hits), err)
	}
}

// indexRecorder fails the first attempt to add an index, then records them.
type indexRecorder struct {
	mu     sync.Mutex
	fail   bool
	fields []string
	done   chan struct{}
}

func (r *indexRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Field string `json:"field_name"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		r.fail = false
		w.WriteHeader(http.StatusInternalServerError)
		r.done <- struct{}{}
		return
	}
	r.fields = append(r.fields, body.Field)
	_, _ = w.Write([]byte(`{"status":"ok","result":{}}`))
	if len(r.fields) == len(searchIndexFields) {
		r.done <- struct{}{}
	}
}

func TestEnsureSearchIndexesRunsOnceAndRetriesAFailure(t *testing.T) {
	rec := &indexRecorder{fail: true, done: make(chan struct{}, 4)}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	s := &Service{store: NewQdrantStore(srv.URL, "c", "")}
	wait := func() {
		t.Helper()
		select {
		case <-rec.done:
		case <-time.After(5 * time.Second):
			t.Fatal("no index request")
		}
	}
	s.ensureSearchIndexes()
	wait()
	for s.searchIndexed.Load() {
		time.Sleep(time.Millisecond) // the failure resets it
	}
	s.ensureSearchIndexes()
	s.ensureSearchIndexes() // already under way: no second run
	wait()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !slices.Equal(rec.fields, searchIndexFields) {
		t.Fatalf("indexed %v", rec.fields)
	}
}
