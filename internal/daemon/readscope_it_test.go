package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/qdrant"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/spaces"
)

// ingestSeeder stores each chunk through rag.Service.IngestText, so the
// real write path creates every space's hybrid collection (dense and BM25
// keyword vectors) and places the chunk where its payload says.
type ingestSeeder struct {
	t   *testing.T
	svc *rag.Service
	// labels passes the chunk's labels_v to the service's placer.
	labels *int64
}

func (g ingestSeeder) seed(_, _ string, payload map[string]any) {
	g.t.Helper()
	var tags []string
	if raw, ok := payload["tags"].([]any); ok {
		for _, x := range raw {
			tags = append(tags, fmt.Sprint(x))
		}
	}
	space, _ := payload["space"].(string)
	if space == "" {
		space = spaces.Shared
	}
	text := fmt.Sprint(payload["text"])
	if h, ok := payload["heading"].(string); ok {
		text = "# " + h + "\n\n" + text
	}
	m := rag.Meta{SourceURI: fmt.Sprint(payload["source_uri"]), SourceType: fmt.Sprint(payload["source_type"]),
		Tags: tags, Writer: rag.WriterClient, Origin: rag.Origin{System: rag.OriginClient, WriterSpace: space}}
	if sc, ok := payload["scope"].(string); ok {
		m.Scope = sc
	}
	if v, ok := payload["labels_v"].(float64); ok {
		*g.labels = int64(v)
	}
	if _, err := g.svc.IngestText(context.Background(), text, m); err != nil {
		g.t.Fatalf("ingest %s: %v", m.SourceURI, err)
	}
}

// TestIntegrationReadPathsRealQdrant is SP-025.1.a1 against the real
// native qdrant engine on its own ports and storage: two isolated spaces,
// a public and a private one with 200 chunks each, written through the
// real write path; from acme, every read path queried with the verbatim
// text of globex's chunks returns nothing of globex's. Gated: set
// ORCH_QDRANT_IT=1 and ORCH_QDRANT_BIN_DIR to a folder holding the engine
// binary (it is not downloaded into a shared folder).
func TestIntegrationReadPathsRealQdrant(t *testing.T) {
	if os.Getenv("ORCH_QDRANT_IT") != "1" {
		t.Skip("set ORCH_QDRANT_IT=1 to run against the real qdrant engine")
	}
	binDir := os.Getenv("ORCH_QDRANT_BIN_DIR")
	if binDir == "" {
		t.Skip("set ORCH_QDRANT_BIN_DIR to a folder holding the qdrant binary")
	}
	dir := t.TempDir()
	p := qdrant.New(qdrant.Config{Version: "v1.17.1", BinDir: binDir, Storage: filepath.Join(dir, "store"), HTTPPort: 6431, GRPCPort: 6432})
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	svc := rag.NewService(wordEmbedder{}, rag.NewQdrantStore("http://127.0.0.1:6431", "sp-025-it", ""), 200, 0)
	var labels int64
	svc.Place = func(_ context.Context, m *rag.Meta) error {
		m.Space, m.LabelsV = m.Origin.WriterSpace, labels
		return nil
	}
	e := newReadEnvOn(t, svc, ingestSeeder{t: t, svc: svc, labels: &labels})

	acme := inSpace("acme")
	allowed := []string{"acme", "personal", "oss"}
	cross, globexSeen := 0, 0
	for i := range chunksPerSpace {
		got := e.readAll(t, acme, memoText("globex", i), memoURI("globex", i))
		cross += len(spaceLeaks(allowed, got...))
	}
	client := e.clientFor(t, resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-a", Space: "acme"}})
	for _, prompt := range []string{memoText("globex", 7), "what did I work on yesterday?"} {
		cross += len(spaceLeaks(allowed, explained(t, client, prompt, e.code+"/globex/repo")...))
	}
	// The same query from globex finds globex's chunk: the text is
	// searchable, and only the caller's spaces keep it from acme.
	for i := range 20 {
		for _, g := range e.readAll(t, inSpace("globex"), memoText("globex", i), memoURI("globex", i)) {
			if g == memoURI("globex", i) {
				globexSeen++
				break
			}
		}
	}
	if cross != 0 || globexSeen != 20 {
		t.Fatalf("cross-space results %d (want 0); globex found its own chunk %d of 20 times", cross, globexSeen)
	}
	if hits, _, err := svc.Search(ctx, memoText("globex", 3), rag.SearchOpts{TopK: 3}); err != nil || len(hits) == 0 || !strings.Contains(hits[0].SourceURI, "globex") {
		t.Fatalf("an unfiltered search does not rank globex's chunk first: %v, %v", hits, err)
	}
	t.Logf("real engine: cross-space results across all read paths: %d (%d globex chunks queried)", cross, chunksPerSpace)
}
