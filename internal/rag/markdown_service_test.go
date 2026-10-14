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
		case r.Method == http.MethodGet:
			collectionJSON(w, LayoutDense)
			return
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
	const size = 40
	tests := []struct {
		name      string
		sections  bool
		wantHeads []string
	}{
		// Guide's intro and Setup fit one chunk; Use needs its own.
		{name: "sections", sections: true, wantHeads: []string{"Guide", "Guide › Use"}},
		{name: "token windows when off", sections: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, payloads := payloadStub(t)
			svc := NewService(fakeEmbedder{dim: 2, vec: []float32{1, 0}}, store, size, 2)
			svc.MarkdownSections = tt.sections
			n, err := svc.IngestText(context.Background(), doc, Meta{SourceURI: "file://guide.md", SourceType: "docs"})
			if err != nil {
				t.Fatal(err)
			}
			got := payloads()
			if n == 0 || len(got) != n {
				t.Fatalf("chunks = %d, payloads = %d", n, len(got))
			}
			m := newMeasure(nil) // the fake embedder has no tokenizer, so the service estimated
			var heads []string
			for i, p := range got {
				text, _ := p["text"].(string)
				if c := tokens(m, text); c > size {
					t.Errorf("chunk %d is %d tokens, over %d: %q", i, c, size, text)
				}
				h, _ := p["heading"].(string)
				heads = append(heads, h)
				if strings.Contains(text, "\n") != tt.sections {
					t.Errorf("chunk %d newlines kept = %v: %q", i, !tt.sections, text)
				}
			}
			if tt.sections && strings.Join(heads, "|") != strings.Join(tt.wantHeads, "|") {
				t.Fatalf("headings = %q, want %q", heads, tt.wantHeads)
			}
			if !tt.sections && strings.Join(heads, "") != "" {
				t.Fatalf("token windows carry headings: %q", heads)
			}
		})
	}
}
