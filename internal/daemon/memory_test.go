package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
)

// optsRag records the options a search asked for.
type optsRag struct {
	fakeRag
	opts *rag.SearchOpts
}

func (o optsRag) Search(_ context.Context, _ string, opts rag.SearchOpts) ([]rag.Hit, string, error) {
	*o.opts = opts
	return []rag.Hit{{SourceURI: "u", Heading: "A › B"}}, "", nil
}

func TestSearchPassesRankingAndCap(t *testing.T) {
	var got rag.SearchOpts
	s := &server{start: time.Now(), rag: optsRag{opts: &got}}
	resp, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q", Ranking: "plain", PerSource: 2})
	if err != nil || got.Ranking != "plain" || got.PerSource != 2 || resp.GetHits()[0].GetHeading() != "A › B" {
		t.Fatalf("opts = %+v, resp = %+v, %v", got, resp, err)
	}
	for _, bad := range []*orchdv1.SearchRequest{{Query: "q", Ranking: "fancy"}, {Query: "q", PerSource: maxSearchTopK + 1}} {
		if _, err := s.Search(context.Background(), bad); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("%+v err = %v", bad, err)
		}
	}
}

func TestRankingParams(t *testing.T) {
	def := rag.DefaultRanking()
	if p := rankingParams(config.SearchSettings{}); p.Profile != def.Profile || p.HalfLife != 0 || p.Floor != 0 || len(p.TypeWeights) != len(def.TypeWeights) {
		t.Fatalf("zero settings = %+v", p)
	}
	p := rankingParams(config.SearchSettings{Ranking: "plain", RecencyHalfLifeDays: 30, RecencyFloor: 0.5,
		SourceWeights: map[string]float64{"web": 0.5, "bad": -1, "huge": 11}})
	if p.Profile != rag.RankingPlain || p.HalfLife != 30*24*time.Hour || p.Floor != 0.5 || p.TypeWeights["web"] != 0.5 ||
		p.TypeWeights["session_transcript"] != 0.85 || p.TypeWeights["bad"] != 0 || p.TypeWeights["huge"] != 0 {
		t.Fatalf("settings = %+v", p)
	}
	if p := rankingParams(config.SearchSettings{Ranking: "bogus", RecencyHalfLifeDays: -1, RecencyFloor: 2}); p.Profile != def.Profile ||
		p.HalfLife != def.HalfLife || p.Floor != def.Floor {
		t.Fatalf("out of range = %+v", p)
	}
}

func TestDefaultsWireRanking(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	v, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := rankingParams(s.RAG.Search)
	if p.Profile != rag.RankingWeighted || p.HalfLife != 180*24*time.Hour || p.Floor != 0.85 ||
		p.TypeWeights["session_transcript"] != 0.85 || s.RAG.Search.PerSource != 2 {
		t.Fatalf("defaults = %+v, per_source %d", p, s.RAG.Search.PerSource)
	}
	if s.Hooks.AutoRAG.Format != "pointers" || s.Hooks.AutoRAG.Untrusted != "mark" {
		t.Fatalf("auto_rag defaults = %+v", s.Hooks.AutoRAG)
	}
}

func TestReindexKeywordsRPC(t *testing.T) {
	ctx := context.Background()
	if _, err := (&server{}).ReindexKeywords(ctx, &orchdv1.ReindexKeywordsRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("no migrator err = %v", err)
	}
	fm := &fakeMigrator{reindex: rag.ReindexReport{Collection: "c", Version: 2, Total: 10, Stale: 4, Updated: 4}}
	s := &server{migrator: fm}
	resp, err := s.ReindexKeywords(ctx, &orchdv1.ReindexKeywordsRequest{Apply: true})
	if err != nil || resp.GetCollection() != "c" || resp.GetTokenizerVersion() != 2 || resp.GetStale() != 4 || resp.GetUpdated() != 4 || !resp.GetApplied() {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	// A run that stops part-way still reports what it did.
	fm.reindexErr = errors.New("qdrant went away")
	resp, err = s.ReindexKeywords(ctx, &orchdv1.ReindexKeywordsRequest{Apply: true})
	if err != nil || resp.GetError() == "" || resp.GetUpdated() != 4 {
		t.Fatalf("partial = %+v, %v", resp, err)
	}
	// A refused plan is an error.
	fm.reindexErr = errors.New("no keyword index")
	if _, err := s.ReindexKeywords(ctx, &orchdv1.ReindexKeywordsRequest{}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("plan err = %v", err)
	}
	migrating.Lock()
	_, err = s.ReindexKeywords(ctx, &orchdv1.ReindexKeywordsRequest{})
	migrating.Unlock()
	if grpcstatus.Code(err) != codes.Aborted {
		t.Fatalf("during a migration err = %v", err)
	}
}

// canonRecorder is a store that also answers Copy and DeleteSource.
type canonRecorder struct {
	storeRecorder
	copies  map[string]rag.StoredCopy
	copyErr error
	deleted []string
}

func (c *canonRecorder) Copy(_ context.Context, uri string) (rag.StoredCopy, bool, error) {
	cp, ok := c.copies[uri]
	return cp, ok, c.copyErr
}

func (c *canonRecorder) DeleteSource(_ context.Context, uri string) (int, error) {
	c.deleted = append(c.deleted, uri)
	return 1, nil
}

func TestStoreFileSkipsOnlyIdenticalSyncedCopies(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	dir := handoff.Dir()
	repoDir := t.TempDir()
	body := "# Plan\n\ntext"
	files := map[string]string{"SAME.md": body, "EDITED.md": body + " on a branch", "CLIENT.md": body, "DRAFT.md": body}
	for name, text := range files {
		p := filepath.Join(repoDir, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := handoff.PutFile(dir, handoff.File{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	canonical := func(_ context.Context, path string) []string {
		if filepath.Base(path) == "DRAFT.md" {
			return nil
		}
		return []string{"repo://o/r/" + filepath.Base(path)}
	}
	h := rag.ContentHash(body)
	st := &canonRecorder{copies: map[string]rag.StoredCopy{
		"repo://o/r/SAME.md":   {Writer: rag.WriterDocs, ContentHash: h},
		"repo://o/r/EDITED.md": {Writer: rag.WriterDocs, ContentHash: h},
		"repo://o/r/CLIENT.md": {Writer: rag.WriterClient, ContentHash: h}, // an agent wrote the repo:// URI
	}}
	storeFiles(context.Background(), st, dir, time.Now().Add(time.Hour), canonical)
	stored := func(name string) bool {
		for uri := range st.texts {
			if strings.HasSuffix(uri, "/"+name) {
				return true
			}
		}
		return false
	}
	if stored("SAME.md") || len(st.deleted) != 1 || !strings.HasSuffix(st.deleted[0], "/SAME.md") {
		t.Errorf("identical synced copy: stored %v, deleted %v", stored("SAME.md"), st.deleted)
	}
	for _, name := range []string{"EDITED.md", "CLIENT.md", "DRAFT.md"} {
		if !stored(name) {
			t.Errorf("%s was not stored under its path", name)
		}
	}
	// A failed lookup is retried rather than storing a duplicate.
	st2 := &canonRecorder{copyErr: errors.New("down")}
	if _, err := canonicalStored(context.Background(), st2, canonical, "file://x", "/x.md", h); err == nil {
		t.Error("lookup error swallowed")
	}
	if synced, err := canonicalStored(context.Background(), &storeRecorder{}, canonical, "file://x", "/x.md", h); synced || err != nil {
		t.Error("skipped without Copy support")
	}
	if synced, err := canonicalStored(context.Background(), st, nil, "file://x", "/x.md", h); synced || err != nil {
		t.Error("skipped without a mapping")
	}
}

// metaRag records the Meta of each write.
type metaRag struct {
	fakeRag
	metas *[]rag.Meta
}

func (m metaRag) IngestText(_ context.Context, _ string, meta rag.Meta) (int, error) {
	*m.metas = append(*m.metas, meta)
	return 1, nil
}

func TestIngestRPCIsAlwaysAClient(t *testing.T) {
	var metas []rag.Meta
	s := &server{start: time.Now(), rag: metaRag{metas: &metas}}
	for _, req := range []*orchdv1.IngestRequest{
		{Text: "x", SourceUri: "repo://acme/core/SECURITY.md", SourceType: "repo_doc"},
		{Text: "x", SourceUri: "session://s/record", SourceType: "session_record"},
	} {
		if _, err := s.Ingest(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range metas {
		if m.Writer != rag.WriterClient {
			t.Errorf("%s written as %q, want a client", m.SourceURI, m.Writer)
		}
	}
}

func TestSearchAndDocumentCarryProvenance(t *testing.T) {
	var got rag.SearchOpts
	s := &server{start: time.Now(), rag: optsRag{opts: &got}}
	resp, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q"})
	if err != nil || resp.GetHits()[0].GetTrust() != "" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	cs, _, _ := catalogServer(t)
	cs.documents = fakeDocuments{doc: rag.Document{Meta: rag.Meta{SourceType: "repo_doc", Writer: rag.WriterDocs, ContentHash: "h"}, Chunks: 1}}
	doc, err := cs.GetDocument(context.Background(), &orchdv1.GetDocumentRequest{SourceUri: "repo://o/r/a.md"})
	if err != nil || doc.GetWriter() != rag.WriterDocs || doc.GetContentHash() != "h" || doc.GetTrust() != rag.TrustRepo {
		t.Fatalf("doc = %+v, %v", doc, err)
	}
}

func TestPurgeRemovesOnlyTheSourcesCheckoutDocs(t *testing.T) {
	ctx := context.Background()
	s, r, _ := catalogServer(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "notes")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{Kind: "repo", Uri: repo, RefreshSecs: 3600}}); err != nil {
		t.Fatal(err)
	}
	pre := sources.CheckoutURI(repo) + "/"
	r.indexed = []indexedDoc{
		{uri: pre + "README.md", sourceType: "repo_doc", tags: []string{"repo:notes"}, writer: rag.WriterSources},
		{uri: pre + "AGENT.md", sourceType: "repo_doc", tags: []string{"repo:notes"}, writer: rag.WriterClient},
		{uri: sources.CheckoutURI(repo+"2") + "/README.md", sourceType: "repo_doc", tags: []string{"repo:notes"}, writer: rag.WriterSources},
	}
	resp, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "repo|" + repo, Purge: true, Confirm: true})
	if err != nil || strings.Join(r.deleted, ",") != pre+"README.md" || resp.GetPurgedSources() != 1 {
		t.Fatalf("purge = %v, %v, deleted %v", resp, err, r.deleted)
	}
}
