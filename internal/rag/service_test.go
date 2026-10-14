package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decodeJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

type fakeEmbedder struct {
	dim int
	vec []float32
	err error
}

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}

func (f fakeEmbedder) Dimension(context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.dim, nil
}

// qdrantStub serves the minimal endpoints IngestText/Search touch and records
// how many points were upserted.
func qdrantStub(t *testing.T, searchResult []map[string]any) (*QdrantStore, *int) {
	t.Helper()
	var upserted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			collectionJSON(w, LayoutDense)
		case http.MethodPut:
			var body struct {
				Points []Point `json:"points"`
			}
			_ = decodeJSON(r, &body)
			upserted += len(body.Points)
			w.WriteHeader(http.StatusOK)
		case http.MethodPost:
			writeJSON(w, map[string]any{"result": searchResult})
		}
	}))
	t.Cleanup(srv.Close)
	return NewQdrantStore(srv.URL, "c", ""), &upserted
}

func TestIngestText(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		emb        fakeEmbedder
		wantChunks int
		wantErr    bool
	}{
		{
			name:       "single chunk ingested",
			text:       "hello world from the orchestrator",
			emb:        fakeEmbedder{dim: 2, vec: []float32{0.1, 0.2}},
			wantChunks: 1,
		},
		{
			name:       "empty text ingests nothing",
			text:       "   ",
			emb:        fakeEmbedder{dim: 2, vec: []float32{0.1, 0.2}},
			wantChunks: 0,
		},
		{
			name:    "embed error propagates",
			text:    "some text",
			emb:     fakeEmbedder{err: errors.New("embed boom")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, upserted := qdrantStub(t, nil)
			svc := NewService(tt.emb, store, 400, 60)
			n, err := svc.IngestText(context.Background(), tt.text, Meta{SourceURI: "manual://x"})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("IngestText: %v", err)
			}
			if n != tt.wantChunks {
				t.Errorf("chunks = %d, want %d", n, tt.wantChunks)
			}
			if *upserted != tt.wantChunks {
				t.Errorf("upserted %d points, want %d", *upserted, tt.wantChunks)
			}
		})
	}
}

func TestServiceSearch(t *testing.T) {
	tests := []struct {
		name     string
		result   []map[string]any
		emb      fakeEmbedder
		wantHits int
		wantErr  bool
	}{
		{
			name: "maps payload into hits",
			result: []map[string]any{
				{"score": 0.9, "payload": map[string]any{
					"text": "t", "source_uri": "u", "source_type": "manual",
					"scope": "work", "tags": []any{"go"},
				}},
			},
			emb:      fakeEmbedder{dim: 2, vec: []float32{0.1, 0.2}},
			wantHits: 1,
		},
		{
			name:     "no results",
			result:   nil,
			emb:      fakeEmbedder{dim: 2, vec: []float32{0.1, 0.2}},
			wantHits: 0,
		},
		{
			name:    "embed error propagates",
			emb:     fakeEmbedder{err: errors.New("boom")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := qdrantStub(t, tt.result)
			svc := NewService(tt.emb, store, 400, 60)
			hits, _, err := svc.Search(context.Background(), "q", SearchOpts{Scope: "work", SourceType: "manual", Tags: []string{"go"}})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(hits) != tt.wantHits {
				t.Errorf("hits = %d, want %d", len(hits), tt.wantHits)
			}
			if tt.wantHits > 0 && hits[0].Tags[0] != "go" {
				t.Errorf("tags not mapped: %+v", hits[0])
			}
		})
	}
}
