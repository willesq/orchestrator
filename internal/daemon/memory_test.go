package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
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
		p.TypeWeights["session_transcript"] != 0.7 || p.TypeWeights["bad"] != 0 || p.TypeWeights["huge"] != 0 {
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
	if p.Profile != rag.RankingWeighted || p.HalfLife != 180*24*time.Hour || p.Floor != 0.7 ||
		p.TypeWeights["session_transcript"] != 0.7 || s.RAG.Search.PerSource != 2 {
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

// canonRecorder is a store that also answers HasSource and DeleteSource.
type canonRecorder struct {
	storeRecorder
	has     map[string]bool
	hasErr  error
	deleted []string
}

func (c *canonRecorder) HasSource(_ context.Context, uri string) (bool, error) {
	return c.has[uri], c.hasErr
}
func (c *canonRecorder) DeleteSource(_ context.Context, uri string) (int, error) {
	c.deleted = append(c.deleted, uri)
	return 1, nil
}

func TestStoreFileSkipsDocsStoredUnderTheirRepoURI(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", t.TempDir())
	dir := handoff.Dir()
	repoDir := t.TempDir()
	synced := filepath.Join(repoDir, "PLAN.md")
	draft := filepath.Join(repoDir, "DRAFT.md")
	for _, p := range []string{synced, draft} {
		if err := os.WriteFile(p, []byte("# Plan\n\ntext"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := handoff.PutFile(dir, handoff.File{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	canonical := func(_ context.Context, path string) []string {
		return []string{"repo://o/r/" + filepath.Base(path)}
	}
	st := &canonRecorder{has: map[string]bool{"repo://o/r/PLAN.md": true}}
	storeFiles(context.Background(), st, dir, time.Now().Add(time.Hour), canonical)
	if _, ok := st.stored("file://" + synced); ok {
		t.Error("a doc synced under its repo URI was stored again under its path")
	}
	if len(st.deleted) != 1 || st.deleted[0] != "file://"+synced {
		t.Errorf("the old path-keyed copy was not removed: %v", st.deleted)
	}
	if _, ok := st.stored("file://" + draft); !ok {
		t.Error("a doc no sync stores (an untracked draft) was not stored")
	}
	// A failed lookup is retried rather than storing a duplicate.
	st2 := &canonRecorder{hasErr: errors.New("down")}
	if _, err := canonicalStored(context.Background(), st2, canonical, "file://x", "/x.md"); err == nil {
		t.Error("lookup error swallowed")
	}
	// Without the store's support or a mapping, nothing is skipped.
	if synced, err := canonicalStored(context.Background(), &storeRecorder{}, canonical, "file://x", "/x.md"); synced || err != nil {
		t.Error("skipped without HasSource support")
	}
	if synced, err := canonicalStored(context.Background(), st, nil, "file://x", "/x.md"); synced || err != nil {
		t.Error("skipped without a mapping")
	}
}
