package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/rageval"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// storeClient serves a small store: sources by type and prefix, their text,
// and deletions.
type storeClient struct {
	*fakeClient
	docs    map[string]*orchdv1.Document
	deleted []string
}

func (s *storeClient) ListSources(_ context.Context, req *orchdv1.ListSourcesRequest, _ ...grpc.CallOption) (*orchdv1.ListSourcesResponse, error) {
	resp := &orchdv1.ListSourcesResponse{}
	for uri, d := range s.docs {
		if strings.HasPrefix(uri, req.GetUriPrefix()) && (req.GetSourceType() == "" || req.GetSourceType() == d.GetSourceType()) {
			resp.Sources = append(resp.Sources, &orchdv1.SourceSummary{SourceUri: uri, SourceType: d.GetSourceType(), Chunks: 2})
		}
	}
	return resp, nil
}

func (s *storeClient) GetDocument(_ context.Context, req *orchdv1.GetDocumentRequest, _ ...grpc.CallOption) (*orchdv1.Document, error) {
	if d, ok := s.docs[req.GetSourceUri()]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

func (s *storeClient) DeleteSource(_ context.Context, req *orchdv1.DeleteSourceRequest, _ ...grpc.CallOption) (*orchdv1.DeleteSourceResponse, error) {
	if !req.GetConfirm() {
		return nil, errors.New("confirm required")
	}
	s.deleted = append(s.deleted, req.GetSourceUri())
	return &orchdv1.DeleteSourceResponse{}, nil
}

func doc(uri, sourceType, text string) *orchdv1.Document {
	return &orchdv1.Document{SourceUri: uri, SourceType: sourceType, Text: text}
}

func TestRAGEvalSeed(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("ORCH_CONFIG_DIR", cfgDir)
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	// The old YAML set is used until the JSONL one exists, then imported.
	if err := rageval.SaveSet(legacyEvalSet(), rageval.Set{Cases: []rageval.Case{{Query: "old case", Expect: []string{"u"}}}}); err != nil {
		t.Fatal(err)
	}
	if defaultEvalSet() != legacyEvalSet() {
		t.Fatalf("default = %s", defaultEvalSet())
	}
	sc := &storeClient{fakeClient: &fakeClient{}, docs: map[string]*orchdv1.Document{
		"session://a/record": doc("session://a/record", "session_record", "## Decisions\n\n- Keep the pointer format for injection"),
		"repo://o/r/a.md":    doc("repo://o/r/a.md", "repo_doc", "## How the store is dated\n\nSee `rag.search.recency_floor`."),
		"repo://o/r/b.md":    doc("repo://o/r/b.md", "repo_doc", ""),
		"file:///x/a.md":     doc("file:///x/a.md", "repo_doc", "## Not seeded from file copies"),
	}}
	cf := func() (orchdv1.OrchdClient, func(), error) { return sc, func() {}, nil }
	if _, err := run(cf, "", "rag", "eval", "add", "my hard one", "repo://o/r/a.md", "--span", "dated"); err != nil {
		t.Fatal(err)
	}
	out, err := run(cf, "", "rag", "eval", "seed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "from 1 session records and 1 repo docs") {
		t.Fatalf("seed output:\n%s", out)
	}
	if defaultEvalSet() != jsonlEvalSet() {
		t.Fatal("the JSONL set is not the default once it exists")
	}
	s, err := rageval.LoadSet(jsonlEvalSet())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range s.Cases {
		got = append(got, c.Section+":"+c.Query)
	}
	want := "legacy:old case|hard:my hard one|seed-decision:what did we decide about Keep the pointer format for injection|" +
		"seed-heading:How the store is dated|seed-ident:rag.search.recency_floor"
	if strings.Join(got, "|") != want {
		t.Fatalf("seeded %q", got)
	}
	// The hand-added case went to the legacy YAML (the default then) and was
	// imported with its section and span.
	if s.Cases[1].Span != "dated" {
		t.Fatalf("imported case = %+v", s.Cases[1])
	}
	if _, err := run(cf, "", "rag", "eval", "add", "jsonl hard", "repo://o/r/a.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(cf, "", "rag", "eval", "seed", "--headings", "0"); err != nil {
		t.Fatal(err)
	}
	s, _ = rageval.LoadSet(jsonlEvalSet())
	if len(s.Cases) != 5 || s.Cases[2].Query != "jsonl hard" {
		t.Fatalf("reseed = %+v", s.Cases)
	}
}

func TestRAGDedupe(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	root := t.TempDir()
	checkout := filepath.Join(root, "acme", "widget")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:acme/widget.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inCheckout := filepath.Join(checkout, "docs", "PLAN.md")
	untracked := filepath.Join(checkout, "docs", "DRAFT.md")
	unrelated := filepath.Join(root, "notes", "mine.md")
	for _, p := range []string{inCheckout, untracked, unrelated} {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	gone := filepath.Join(root, "widget.worktrees", "feat", "x", "docs", "PLAN.md")
	goneOther := filepath.Join(root, "elsewhere", "README.md")
	sc := &storeClient{fakeClient: &fakeClient{}, docs: map[string]*orchdv1.Document{
		"repo://acme/widget/docs/PLAN.md": doc("repo://acme/widget/docs/PLAN.md", "repo_doc", "the plan"),
		"repo://acme/widget/README.md":    doc("repo://acme/widget/README.md", "repo_doc", "readme"),
		"file://" + inCheckout:            doc("file://"+inCheckout, "repo_doc", "an edited plan"),
		"file://" + untracked:             doc("file://"+untracked, "repo_doc", "draft"),
		"file://" + gone:                  doc("file://"+gone, "repo_doc", "branch plan"),
		"file://" + goneOther:             doc("file://"+goneOther, "repo_doc", "other readme"),
		"file://" + unrelated:             doc("file://"+unrelated, "repo_doc", "the plan"),
		"claude://agents/x.md":            doc("claude://agents/x.md", "claude_config", "agent"),
	}}
	cf := func() (orchdv1.OrchdClient, func(), error) { return sc, func() {}, nil }
	out, err := run(cf, "", "rag", "dedupe")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"file://" + inCheckout, "repo://acme/widget/docs/PLAN.md holds the doc",
		"file://" + gone, "file is gone; repo://…/docs/PLAN.md holds the doc",
		"file://" + unrelated, "same text as repo://acme/widget/docs/PLAN.md",
		"3 duplicate source(s), 6 chunk(s); rerun with --apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
	for _, kept := range []string{untracked, goneOther, "claude://agents/x.md"} {
		if strings.Contains(out, kept) {
			t.Errorf("%s planned for deletion:\n%s", kept, out)
		}
	}
	if len(sc.deleted) != 0 {
		t.Fatal("a plan deleted")
	}
	if out, err := run(cf, "", "rag", "dedupe", "--apply"); err != nil || len(sc.deleted) != 3 || !strings.Contains(out, "deleted 3 duplicate") {
		t.Fatalf("apply = %s, %v, deleted %v", out, err, sc.deleted)
	}
	if pathOf("claude://agents/x.md", "/home/me") != "/home/me/.claude/agents/x.md" || pathOf("claude://../../etc/passwd", "/h") != "/h/.claude/etc/passwd" ||
		pathOf("repo://x", "/h") != "" || pathOf("file://relative", "/h") != "" {
		t.Error("pathOf")
	}
}

func TestRAGReindexKeywords(t *testing.T) {
	saved := fakeReindex
	t.Cleanup(func() { fakeReindex = saved })
	var applied []bool
	fakeReindex = func(req *orchdv1.ReindexKeywordsRequest) (*orchdv1.ReindexKeywordsResponse, error) {
		applied = append(applied, req.GetApply())
		r := &orchdv1.ReindexKeywordsResponse{Collection: "c", TokenizerVersion: 2, Total: 10, Stale: 4, Applied: req.GetApply()}
		if req.GetApply() {
			r.Updated = 4
		}
		return r, nil
	}
	cf := factory(&fakeClient{}, nil)
	out, err := run(cf, "", "rag", "reindex-keywords")
	if err != nil || !strings.Contains(out, "10 chunks, 4 indexed by an older tokenizer (current version 2)") || !strings.Contains(out, "--apply") {
		t.Fatalf("plan = %s, %v", out, err)
	}
	if out, err := run(cf, "", "rag", "reindex-keywords", "--apply"); err != nil || !strings.Contains(out, "re-indexed 4 chunk(s)") {
		t.Fatalf("apply = %s, %v", out, err)
	}
	fakeReindex = func(*orchdv1.ReindexKeywordsRequest) (*orchdv1.ReindexKeywordsResponse, error) {
		return &orchdv1.ReindexKeywordsResponse{Applied: true, Updated: 2, Error: "qdrant went away"}, nil
	}
	if out, err := run(cf, "", "rag", "reindex-keywords", "--apply"); err == nil || !strings.Contains(out, "rerun to continue") {
		t.Fatalf("partial = %s, %v", out, err)
	}
	fakeReindex = func(*orchdv1.ReindexKeywordsRequest) (*orchdv1.ReindexKeywordsResponse, error) {
		return nil, errors.New("unavailable")
	}
	if _, err := run(cf, "", "rag", "reindex-keywords"); err == nil {
		t.Fatal("RPC error swallowed")
	}
	if len(applied) != 2 || applied[0] || !applied[1] {
		t.Fatalf("apply flags = %v", applied)
	}
}

func TestSearchPassesRankingFlags(t *testing.T) {
	client := &fakeClient{search: &orchdv1.SearchResponse{}}
	if _, err := run(factory(client, nil), "", "search", "--ranking", "plain", "--per-source", "-1", "q"); err != nil {
		t.Fatal(err)
	}
	if client.searchReq.GetRanking() != "plain" || client.searchReq.GetPerSource() != -1 {
		t.Fatalf("req = %+v", client.searchReq)
	}
}
