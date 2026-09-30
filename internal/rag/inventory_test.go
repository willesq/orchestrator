package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// inventoryStub serves the scroll/count/delete endpoints ListSources and
// DeleteSource touch. pages are returned in order, each as one scroll page; the
// last page reports no next offset. It records every delete filter it is sent.
type inventoryStub struct {
	pages      [][]map[string]any
	count      int
	countErr   bool
	deletes    []map[string]any
	scrollCall int
}

func newInventoryStore(t *testing.T, st *inventoryStub) *QdrantStore {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/scroll"):
			i := st.scrollCall
			st.scrollCall++
			var pts []map[string]any
			if i < len(st.pages) {
				pts = st.pages[i]
			}
			var next any
			if i+1 < len(st.pages) {
				next = "offset-" + string(rune('a'+i))
			}
			writeJSON(w, map[string]any{"result": map[string]any{
				"points": pts, "next_page_offset": next,
			}})
		case strings.HasSuffix(r.URL.Path, "/points/count"):
			if st.countErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"result": map[string]any{"count": st.count}})
		case strings.Contains(r.URL.Path, "/points/delete"):
			var body struct {
				Filter map[string]any `json:"filter"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			st.deletes = append(st.deletes, body.Filter)
			writeJSON(w, map[string]any{"result": map[string]any{}})
		default:
			writeJSON(w, map[string]any{"result": map[string]any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return NewQdrantStore(srv.URL, "c", "")
}

func point(uri, typ string) map[string]any {
	return map[string]any{"id": uri, "payload": map[string]any{
		"source_uri": uri, "source_type": typ, "scope": "",
	}}
}

func TestListSources(t *testing.T) {
	tests := []struct {
		name      string
		pages     [][]map[string]any
		opts      ListSourcesOpts
		wantURIs  []string
		wantFirst int // chunks on the first summary
		wantCount int // distinct sources before limit
		wantTotal int // total chunks
	}{
		{
			name: "aggregates chunks per source, most chunks first",
			pages: [][]map[string]any{{
				point("a://1", "manual"), point("a://1", "manual"), point("a://1", "manual"),
				point("b://2", "web"),
			}},
			wantURIs: []string{"a://1", "b://2"}, wantFirst: 3, wantCount: 2, wantTotal: 4,
		},
		{
			name:      "empty store",
			pages:     [][]map[string]any{{}},
			wantURIs:  nil,
			wantCount: 0, wantTotal: 0,
		},
		{
			name: "follows pagination across pages",
			pages: [][]map[string]any{
				{point("a://1", "manual")},
				{point("a://1", "manual"), point("b://2", "web")},
			},
			wantURIs: []string{"a://1", "b://2"}, wantFirst: 2, wantCount: 2, wantTotal: 3,
		},
		{
			name: "uri_prefix filters and excludes non-matching from totals",
			pages: [][]map[string]any{{
				point("session://x", "session_record"), point("note://y", "manual"),
			}},
			opts:     ListSourcesOpts{URIPrefix: "session://"},
			wantURIs: []string{"session://x"}, wantFirst: 1, wantCount: 1, wantTotal: 1,
		},
		{
			name: "limit truncates the list but not the reported distinct count",
			pages: [][]map[string]any{{
				point("a://1", "manual"), point("a://1", "manual"), point("b://2", "web"),
			}},
			opts:     ListSourcesOpts{Limit: 1},
			wantURIs: []string{"a://1"}, wantFirst: 2, wantCount: 2, wantTotal: 3,
		},
		{
			name: "ties break on uri so ordering is stable",
			pages: [][]map[string]any{{
				point("z://last", "manual"), point("a://first", "manual"),
			}},
			wantURIs: []string{"a://first", "z://last"}, wantFirst: 1, wantCount: 2, wantTotal: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &inventoryStub{pages: tt.pages}
			svc := NewService(fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}, newInventoryStore(t, st), 100, 10)

			got, distinct, total, err := svc.ListSources(context.Background(), tt.opts)
			if err != nil {
				t.Fatalf("ListSources: %v", err)
			}
			if len(got) != len(tt.wantURIs) {
				t.Fatalf("got %d sources, want %d (%v)", len(got), len(tt.wantURIs), got)
			}
			for i, want := range tt.wantURIs {
				if got[i].SourceURI != want {
					t.Errorf("source[%d] = %q, want %q", i, got[i].SourceURI, want)
				}
			}
			if len(got) > 0 && got[0].Chunks != tt.wantFirst {
				t.Errorf("first source chunks = %d, want %d", got[0].Chunks, tt.wantFirst)
			}
			if distinct != tt.wantCount {
				t.Errorf("distinct = %d, want %d", distinct, tt.wantCount)
			}
			if total != tt.wantTotal {
				t.Errorf("total chunks = %d, want %d", total, tt.wantTotal)
			}
		})
	}
}

func TestListSourcesScrollError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	svc := NewService(fakeEmbedder{dim: 3}, NewQdrantStore(srv.URL, "c", ""), 100, 10)

	if _, _, _, err := svc.ListSources(context.Background(), ListSourcesOpts{}); err == nil {
		t.Fatal("expected an error when the store is unreachable")
	}
}

func TestDeleteSource(t *testing.T) {
	tests := []struct {
		name        string
		uri         string
		count       int
		countErr    bool
		wantDeleted int
		wantErr     bool
		wantCalls   int
	}{
		{name: "deletes every chunk of the source", uri: "a://1", count: 4, wantDeleted: 4, wantCalls: 1},
		{name: "missing source is not an error", uri: "gone://x", count: 0, wantDeleted: 0, wantCalls: 0},
		{name: "empty uri is rejected", uri: "", wantErr: true, wantCalls: 0},
		{name: "count failure propagates", uri: "a://1", countErr: true, wantErr: true, wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &inventoryStub{count: tt.count, countErr: tt.countErr}
			svc := NewService(fakeEmbedder{dim: 3}, newInventoryStore(t, st), 100, 10)

			got, err := svc.DeleteSource(context.Background(), tt.uri)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.wantDeleted {
				t.Errorf("deleted = %d, want %d", got, tt.wantDeleted)
			}
			if len(st.deletes) != tt.wantCalls {
				t.Errorf("delete calls = %d, want %d", len(st.deletes), tt.wantCalls)
			}
		})
	}
}

// A shrinking re-ingest must drop the previous version's surplus chunks:
// point IDs are uri#index, so higher indices are never overwritten.
func TestIngestPrunesSurplusChunksFromAShorterRewrite(t *testing.T) {
	st := &inventoryStub{count: 3} // 3 stale chunks sit past the new end
	svc := NewService(fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}, newInventoryStore(t, st), 1000, 0)

	if _, err := svc.IngestText(context.Background(), "short", Meta{SourceURI: "a://1"}); err != nil {
		t.Fatalf("IngestText: %v", err)
	}
	if len(st.deletes) != 1 {
		t.Fatalf("expected one prune delete, got %d", len(st.deletes))
	}
	must, _ := st.deletes[0]["must"].([]any)
	if len(must) != 2 {
		t.Fatalf("prune filter should constrain both source_uri and chunk_index, got %v", st.deletes[0])
	}
}

func TestIngestSkipsPruneWhenNothingIsStale(t *testing.T) {
	st := &inventoryStub{count: 0}
	svc := NewService(fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}, newInventoryStore(t, st), 1000, 0)

	if _, err := svc.IngestText(context.Background(), "hello", Meta{SourceURI: "a://1"}); err != nil {
		t.Fatalf("IngestText: %v", err)
	}
	if len(st.deletes) != 0 {
		t.Errorf("expected no delete when no surplus chunks exist, got %d", len(st.deletes))
	}
}

func TestDeleteByFilterRefusesNilFilter(t *testing.T) {
	st := &inventoryStub{}
	store := newInventoryStore(t, st)

	if err := store.DeleteByFilter(context.Background(), nil); err == nil {
		t.Fatal("expected a nil filter to be refused rather than deleting the collection")
	}
	if len(st.deletes) != 0 {
		t.Errorf("a refused delete must not reach the server, got %d calls", len(st.deletes))
	}
}

// A store nobody has ingested into has no collection yet. Reads must report it
// as empty rather than surfacing qdrant's raw 404 — caught in end-to-end
// testing against a fresh instance.
func TestReadsTreatAMissingCollectionAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection ` + "`orchestrator`" + ` doesn't exist!"}}`))
	}))
	t.Cleanup(srv.Close)
	store := NewQdrantStore(srv.URL, "orchestrator", "")
	svc := NewService(fakeEmbedder{dim: 3}, store, 100, 10)
	ctx := context.Background()

	t.Run("ListSources is empty, not an error", func(t *testing.T) {
		got, distinct, chunks, err := svc.ListSources(ctx, ListSourcesOpts{})
		if err != nil {
			t.Fatalf("ListSources: %v", err)
		}
		if len(got) != 0 || distinct != 0 || chunks != 0 {
			t.Errorf("got %d sources / %d distinct / %d chunks, want all zero", len(got), distinct, chunks)
		}
	})

	t.Run("DeleteSource removes nothing, without erroring", func(t *testing.T) {
		n, err := svc.DeleteSource(ctx, "a://1")
		if err != nil {
			t.Fatalf("DeleteSource: %v", err)
		}
		if n != 0 {
			t.Errorf("deleted = %d, want 0", n)
		}
	})

	// A genuine 404 (a bad path, say) must still surface as an error.
	t.Run("an unrelated 404 still fails", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"no such route"}}`))
		}))
		t.Cleanup(bad.Close)
		s2 := NewService(fakeEmbedder{dim: 3}, NewQdrantStore(bad.URL, "c", ""), 100, 10)
		if _, _, _, err := s2.ListSources(ctx, ListSourcesOpts{}); err == nil {
			t.Fatal("expected an unrelated 404 to remain an error")
		}
	})
}

// An unconstrained filter means "match everything" to Qdrant, so DeleteByFilter
// must refuse it — and must not issue the request while deciding.
func TestDeleteByFilterRequiresAConstraint(t *testing.T) {
	tests := []struct {
		name   string
		filter map[string]any
	}{
		{name: "nil filter", filter: nil},
		{name: "empty map", filter: map[string]any{}},
		{name: "empty must list", filter: map[string]any{"must": []any{}}},
		{name: "must of the wrong type", filter: map[string]any{"must": "everything"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Error("an unconstrained delete must never reach the server")
			}))
			t.Cleanup(srv.Close)
			store := NewQdrantStore(srv.URL, "c", "")

			if err := store.DeleteByFilter(context.Background(), tt.filter); err == nil {
				t.Fatal("expected the delete to be refused")
			}
		})
	}
}

// IngestText deletes as well as writes (the prune), so an empty source_uri would
// match every URI-less point in the store. Validate where the delete happens,
// not only in the callers that happen to check today.
func TestIngestRejectsAnEmptySourceURI(t *testing.T) {
	st := &inventoryStub{count: 5}
	svc := NewService(fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}, newInventoryStore(t, st), 100, 10)

	_, err := svc.IngestText(context.Background(), "some text", Meta{SourceURI: ""})
	if err == nil {
		t.Fatal("expected an empty source_uri to be rejected")
	}
	if len(st.deletes) != 0 {
		t.Errorf("a rejected ingest must not delete anything, got %d delete(s)", len(st.deletes))
	}
}

// A server that keeps handing back the same cursor would otherwise spin forever.
func TestListSourcesStopsOnANonAdvancingCursor(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/points/scroll") {
			calls++
			if calls > 50 {
				t.Fatal("scroll looped well past the point of detecting a stuck cursor")
			}
			writeJSON(w, map[string]any{"result": map[string]any{
				"points":           []map[string]any{point("a://1", "manual")},
				"next_page_offset": "stuck",
			}})
			return
		}
		writeJSON(w, map[string]any{"result": map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	svc := NewService(fakeEmbedder{dim: 3}, NewQdrantStore(srv.URL, "c", ""), 100, 10)

	_, _, _, err := svc.ListSources(context.Background(), ListSourcesOpts{})
	if err == nil {
		t.Fatal("expected an error when the scroll cursor stops advancing")
	}
	if !strings.Contains(err.Error(), "did not advance") {
		t.Errorf("error %q should name the stuck cursor", err)
	}
}
