package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/bm25"
	"github.com/willesq/orchestrator/internal/qdrant"
)

// TestIntegrationHybridMigration runs the migration against the real native
// qdrant engine on isolated ports and storage, then searches the new
// collection by keyword. Gated: set ORCH_QDRANT_IT=1 (and ORCH_QDRANT_BIN_DIR
// to reuse an already-downloaded binary).
func TestIntegrationHybridMigration(t *testing.T) {
	if os.Getenv("ORCH_QDRANT_IT") != "1" {
		t.Skip("set ORCH_QDRANT_IT=1 to run against the real qdrant engine")
	}
	dir := t.TempDir()
	binDir := os.Getenv("ORCH_QDRANT_BIN_DIR")
	if binDir == "" {
		binDir = filepath.Join(dir, "bin")
	}
	p := qdrant.New(qdrant.Config{Version: "v1.17.1", BinDir: binDir, Storage: filepath.Join(dir, "store"), HTTPPort: 6411, GRPCPort: 6412})
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = p.Stop() }()
	if !p.Ready(ctx) {
		t.Fatal("engine not ready")
	}
	base := "http://127.0.0.1:6411"

	// The old layout: one unnamed dense vector.
	put := func(path string, body any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, base+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode >= 300 {
			t.Fatalf("PUT %s: %v %v", path, err, resp)
		}
		_ = resp.Body.Close()
	}
	put("/collections/orchestrator", map[string]any{"vectors": map[string]any{"size": 2, "distance": "Cosine"}})
	docs := map[string]string{
		"repo://w/SYNC.md":   "the sync_meta table records the last pulled cursor for each device",
		"repo://w/DATA.md":   "households own items; every row carries owner_id and household_id",
		"repo://w/DEPLOY.md": "deploy behind the Zoraxy reverse proxy on TrueNAS",
	}
	var pts []map[string]any
	for uri, text := range docs {
		pts = append(pts, map[string]any{"id": pointID(uri, 0), "vector": []float32{1, 0},
			"payload": map[string]any{"source_uri": uri, "source_type": "repo_doc", "chunk_index": 0, "text": text}})
	}
	put("/collections/orchestrator/points?wait=true", map[string]any{"points": pts})

	svc := NewService(&hookEmbedder{}, NewQdrantStore(base, "orchestrator", ""), 110, 16)
	svc.StateDir = t.TempDir()
	rep, err := svc.MigrateHybrid(ctx, "", true, nil)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !rep.Applied || rep.Sources != 3 || rep.NewChunks != 3 {
		t.Fatalf("report = %+v", rep)
	}
	dst := NewQdrantStore(base, "orchestrator-hybrid", "")
	if l, err := dst.Layout(ctx); err != nil || l != LayoutHybrid {
		t.Fatalf("layout = %v, %v", l, err)
	}
	if n, _ := dst.Count(ctx, nil); n != 3 {
		t.Fatalf("hybrid holds %d points", n)
	}

	// Keyword search on the BM25 vector: Qdrant applies IDF.
	q := bm25.Query("sync_meta")
	body, _ := json.Marshal(map[string]any{"query": map[string]any{"indices": q.Indices, "values": q.Values},
		"using": "bm25", "limit": 3, "with_payload": true})
	resp, err := http.Post(base+"/collections/orchestrator-hybrid/points/query", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Points []struct {
				Score   float64        `json:"score"`
				Payload map[string]any `json:"payload"`
			} `json:"points"`
		} `json:"result"`
		Status any `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode >= 300 {
		t.Fatalf("query: %v status %d %v", err, resp.StatusCode, out.Status)
	}
	if len(out.Result.Points) == 0 || out.Result.Points[0].Payload["source_uri"] != "repo://w/SYNC.md" {
		t.Fatalf("keyword search = %+v", out.Result.Points)
	}
	fmt.Printf("keyword top hit %s score %.3f of %d\n", out.Result.Points[0].Payload["source_uri"], out.Result.Points[0].Score, len(out.Result.Points))
	// Through the service: every text embeds the same, so meaning cannot tell
	// the documents apart, and the identifier must be found by keyword.
	hits, note, err := svc.Search(ctx, "where is sync_meta kept", SearchOpts{TopK: 3})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if note != "" {
		t.Fatalf("hybrid search fell back: %s", note)
	}
	if len(hits) == 0 || hits[0].SourceURI != "repo://w/SYNC.md" || !hits[0].Exact || hits[0].Keyword <= 0 {
		t.Fatalf("hybrid hits = %+v", hits)
	}
	if dense, _, err := svc.Search(ctx, "where is sync_meta kept", SearchOpts{TopK: 3, Mode: ModeDense}); err != nil || len(dense) != 3 {
		t.Fatalf("dense hits = %+v, %v", dense, err)
	}
	fmt.Printf("hybrid top hit %s (cosine %.3f, keyword %.3f, exact %v)\n", hits[0].SourceURI, hits[0].Score, hits[0].Keyword, hits[0].Exact)
	// The backup is untouched.
	if n, _ := NewQdrantStore(base, "orchestrator", "").Count(ctx, nil); n != 3 {
		t.Fatalf("backup holds %d", n)
	}
}
