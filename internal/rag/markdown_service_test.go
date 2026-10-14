package rag

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// payloadStub records the payloads of upserted points.
func payloadStub(t *testing.T) (*QdrantStore, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			var body struct {
				Points []struct {
					Payload map[string]any `json:"payload"`
				} `json:"points"`
			}
			_ = decodeJSON(r, &body)
			mu.Lock()
			for _, p := range body.Points {
				got = append(got, p.Payload)
			}
			mu.Unlock()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/count"):
			writeJSON(w, map[string]any{"result": map[string]any{"count": 0}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return NewQdrantStore(srv.URL, "c", ""), func() []map[string]any { mu.Lock(); defer mu.Unlock(); return got }
}

func TestIngestMarkdownBySection(t *testing.T) {
	doc := "# Guide\n\nintro\n\n## Setup\n\n" + strings.Repeat("step ", 12) + "\n\n## Use\n\n" + strings.Repeat("run ", 12)
	tests := []struct {
		name       string
		sections   bool
		wantHeads  []string
		wantNewlns bool
	}{
		{name: "sections", sections: true, wantHeads: []string{"Guide", "Guide › Setup", "Guide › Use"}, wantNewlns: true},
		{name: "word windows when off", sections: false, wantHeads: []string{"", "", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, payloads := payloadStub(t)
			svc := NewService(fakeEmbedder{dim: 2, vec: []float32{1, 0}}, store, 16, 2)
			svc.MarkdownSections = tt.sections
			n, err := svc.IngestText(context.Background(), doc, Meta{SourceURI: "file://guide.md", SourceType: "docs"})
			if err != nil {
				t.Fatal(err)
			}
			got := payloads()
			if n != len(tt.wantHeads) || len(got) != n {
				t.Fatalf("chunks = %d (payloads %d), want %d", n, len(got), len(tt.wantHeads))
			}
			for i, p := range got {
				h, _ := p["heading"].(string)
				if h != tt.wantHeads[i] {
					t.Errorf("chunk %d heading = %q, want %q", i, h, tt.wantHeads[i])
				}
				if text, _ := p["text"].(string); strings.Contains(text, "\n") != tt.wantNewlns {
					t.Errorf("chunk %d newlines kept = %v: %q", i, !tt.wantNewlns, text)
				}
			}
		})
	}
}
