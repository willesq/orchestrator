package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureCollection(t *testing.T) {
	tests := []struct {
		name      string
		getStatus int
		wantPut   bool
	}{
		{name: "already exists, no create", getStatus: http.StatusOK, wantPut: false},
		{name: "missing, creates it", getStatus: http.StatusNotFound, wantPut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var putCalled bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					putCalled = true
					w.WriteHeader(http.StatusOK)
					return
				}
				if tt.getStatus == http.StatusOK {
					collectionJSON(w, LayoutDense)
					return
				}
				collectionJSON(w, LayoutUnknown)
			}))
			defer srv.Close()

			q := NewQdrantStore(srv.URL, "c", "")
			if err := q.EnsureCollection(context.Background(), 384); err != nil {
				t.Fatalf("EnsureCollection: %v", err)
			}
			if putCalled != tt.wantPut {
				t.Errorf("put called = %v, want %v", putCalled, tt.wantPut)
			}
		})
	}
}

func TestUpsert(t *testing.T) {
	tests := []struct {
		name       string
		points     []Point
		wantCalled bool
	}{
		{name: "no points is a no-op", points: nil, wantCalled: false},
		{
			name:       "writes points",
			points:     []Point{{ID: "a", Vector: []float32{0.1}, Payload: map[string]any{"k": "v"}}},
			wantCalled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					collectionJSON(w, LayoutDense)
					return
				}
				called = true
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			q := NewQdrantStore(srv.URL, "c", "")
			if err := q.Upsert(context.Background(), tt.points); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			if called != tt.wantCalled {
				t.Errorf("server called = %v, want %v", called, tt.wantCalled)
			}
		})
	}
}

func TestSearch(t *testing.T) {
	tests := []struct {
		name      string
		apiKey    string
		wantKey   string
		wantScore float32
	}{
		{name: "parses hits and forwards api-key", apiKey: "secret", wantKey: "secret", wantScore: 0.9},
		{name: "no api-key sends empty header", apiKey: "", wantKey: "", wantScore: 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotKey string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotKey = r.Header.Get("api-key")
				if r.Method == http.MethodGet {
					collectionJSON(w, LayoutDense)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"result": []map[string]any{
						{"score": tt.wantScore, "payload": map[string]any{"text": "hi"}},
					},
				})
			}))
			defer srv.Close()

			q := NewQdrantStore(srv.URL, "c", tt.apiKey)
			hits, err := q.Search(context.Background(), []float32{0.1}, 5,
				map[string]string{"scope": "work"}, []string{"go"})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(hits) != 1 || hits[0].Score != tt.wantScore {
				t.Fatalf("unexpected hits: %+v", hits)
			}
			if gotKey != tt.wantKey {
				t.Errorf("api-key header = %q, want %q", gotKey, tt.wantKey)
			}
		})
	}
}

func TestQdrantErrorStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "5xx becomes an error", status: http.StatusInternalServerError},
		{name: "4xx becomes an error", status: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", tt.status)
			}))
			defer srv.Close()

			q := NewQdrantStore(srv.URL, "c", "")
			if _, err := q.Search(context.Background(), []float32{0.1}, 1, nil, nil); err == nil {
				t.Errorf("expected error on %d response", tt.status)
			}
		})
	}
}

// collectionJSON answers a collection lookup the way Qdrant does: the
// collection's config, or a 404 that says it does not exist.
func collectionJSON(w http.ResponseWriter, l Layout) {
	switch l {
	case LayoutDense:
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"config": map[string]any{"params": map[string]any{
			"vectors": map[string]any{"size": 2, "distance": "Cosine"}}}}})
	case LayoutHybrid:
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"config": map[string]any{"params": map[string]any{
			"vectors":        map[string]any{"dense": map[string]any{"size": 2, "distance": "Cosine"}},
			"sparse_vectors": map[string]any{"bm25": map[string]any{"modifier": "idf"}}}}}})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection doesn't exist!"}}`))
	}
}
