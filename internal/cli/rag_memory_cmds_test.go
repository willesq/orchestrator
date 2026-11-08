package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/config/configtest"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/rageval"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	dreg := docs.NewRegistry(conn)
	ctx := context.Background()
	_ = dreg.AddOwner(ctx, docs.Owner{Owner: "acme", RefreshSecs: 3600})
	_ = dreg.SetRepoState(ctx, docs.RepoState{Owner: "acme", Repo: "widget", HeadSHA: "s", Status: "ok"})
	_ = conn.Close()

	root := t.TempDir()
	checkout := filepath.Join(root, "acme", "widget")
	writeGit := func(dir, origin string) {
		_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+origin+"\n"), 0o644)
	}
	writeGit(checkout, "git@github.com:acme/widget.git")
	writeGit(filepath.Join(home, ".claude"), "git@github.com:acme/widget.git")
	file := func(p string) string {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
		return p
	}
	same := file(filepath.Join(checkout, "docs", "PLAN.md"))
	older := file(filepath.Join(checkout, "docs", "OLDER.md")) // on disk "x", like the synced copy
	edited := file(filepath.Join(checkout, "docs", "EDITED.md"))
	byClient := file(filepath.Join(checkout, "docs", "NOTE.md"))
	untracked := file(filepath.Join(checkout, "docs", "DRAFT.md"))
	unrelated := file(filepath.Join(root, "notes", "PLAN.md"))
	claudeMD := file(filepath.Join(home, ".claude", "CLAUDE.md"))
	gone := filepath.Join(root, "widget.worktrees", "feat", "x", "docs", "PLAN.md")
	synced := func(uri, writer, hash string) *orchdv1.Document {
		return &orchdv1.Document{SourceUri: uri, SourceType: "repo_doc", Writer: writer, ContentHash: hash, Text: "the plan"}
	}
	copyOf := func(uri, hash string) *orchdv1.Document {
		return &orchdv1.Document{SourceUri: uri, SourceType: "repo_doc", ContentHash: hash, Text: "the plan"}
	}
	sc := &storeClient{fakeClient: &fakeClient{}, docs: map[string]*orchdv1.Document{
		"repo://acme/widget/docs/PLAN.md":   synced("repo://acme/widget/docs/PLAN.md", "docs", "h1"),
		"repo://acme/widget/docs/EDITED.md": synced("repo://acme/widget/docs/EDITED.md", "docs", "h2"),
		"repo://acme/widget/docs/NOTE.md":   synced("repo://acme/widget/docs/NOTE.md", "client", "h3"),
		"repo://acme/widget/CLAUDE.md":      synced("repo://acme/widget/CLAUDE.md", "docs", "h4"),
		"repo://acme/widget/docs/OLDER.md":  synced("repo://acme/widget/docs/OLDER.md", "docs", rag.ContentHash("x")),
		"file://" + older:                   {SourceUri: "file://" + older, Text: "an older draft"},
		"file://" + same:                    copyOf("file://"+same, "h1"),
		"file://" + edited:                  copyOf("file://"+edited, "h2-branch"),
		"file://" + byClient:                copyOf("file://"+byClient, "h3"),
		"file://" + untracked:               copyOf("file://"+untracked, "h1"),
		"file://" + unrelated:               copyOf("file://"+unrelated, "h1"), // same text, another project
		"file://" + gone:                    copyOf("file://"+gone, "h1"),
		"claude://CLAUDE.md":                copyOf("claude://CLAUDE.md", "h4"), // ~/.claude is not a repo copy
	}}
	_ = claudeMD
	cf := func() (orchdv1.OrchdClient, func(), error) { return sc, func() {}, nil }
	out, err := run(cf, "", "rag", "dedupe")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "file://"+same) || !strings.Contains(out, "identical to repo://acme/widget/docs/PLAN.md, stored by the docs sync") ||
		!strings.Contains(out, "file://"+older) || !strings.Contains(out, "the stored copy is an older version") ||
		!strings.Contains(out, "2 duplicate source(s), 4 chunk(s); rerun with --apply") {
		t.Errorf("plan:\n%s", out)
	}
	for _, kept := range []string{edited, byClient, untracked, unrelated, gone, "claude://CLAUDE.md"} {
		if strings.Contains(out, kept) {
			t.Errorf("%s planned for deletion:\n%s", kept, out)
		}
	}
	if len(sc.deleted) != 0 {
		t.Fatal("a plan deleted")
	}
	if out, err := run(cf, "", "rag", "dedupe", "--apply"); err != nil || len(sc.deleted) != 2 || !strings.Contains(out, "deleted 2 duplicate") {
		t.Fatalf("apply = %s, %v, deleted %v", out, err, sc.deleted)
	}
	if pathOf("claude://agents/x.md", "/home/me") != "/home/me/.claude/agents/x.md" || pathOf("claude://../../etc/passwd", "/h") != "/h/.claude/etc/passwd" ||
		pathOf("repo://x", "/h") != "" || pathOf("file://relative", "/h") != "" {
		t.Error("pathOf")
	}
	if sameContent(&orchdv1.Document{Text: " "}, &orchdv1.Document{Text: " "}) || !sameContent(&orchdv1.Document{Text: "a"}, &orchdv1.Document{Text: "a"}) {
		t.Error("sameContent")
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

func TestConfirmPruneFlags(t *testing.T) {
	saved := docsPollInterval
	docsPollInterval = time.Millisecond
	t.Cleanup(func() { docsPollInterval = saved })
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	fc := &fakeClient{}
	cf := factory(fc, nil)
	if _, err := run(cf, "", "source", "refresh", "--confirm-prune"); err == nil {
		t.Fatal("--confirm-prune without an id accepted")
	}
	out, err := run(cf, "", "source", "refresh", "repo|/x", "--confirm-prune")
	if err != nil || fc.refreshReq.GetId() != "repo|/x" || !fc.refreshReq.GetConfirmPrune() || !strings.Contains(out, "confirmed") {
		t.Fatalf("source confirm = %q, %v, %+v", out, err, fc.refreshReq)
	}
	if _, err := run(cf, "", "docs", "sync", "--confirm-prune"); err == nil {
		t.Fatal("docs --confirm-prune without an owner accepted")
	}
	seedDocsOwner(t, "ok", "")
	fc.docsStates = []*orchdv1.DocsSyncState{{Owner: "willesq", StartedAt: time.Now().Unix() + 5, FinishedAt: time.Now().Unix() + 6}}
	if _, err := run(cf, "", "docs", "sync", "willesq", "--confirm-prune", "--background"); err != nil || !fc.docsSyncReq.GetConfirmPrune() || fc.docsSyncReq.GetOwner() != "willesq" {
		t.Fatalf("docs confirm = %v, %+v", err, fc.docsSyncReq)
	}
}

func TestDedupeListsLegacyLocalSourceCopies(t *testing.T) {
	cfgDir, roots := t.TempDir(), t.TempDir()
	roots = sources.ResolvePath(roots)
	t.Setenv("ORCH_CONFIG_DIR", cfgDir)
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	configtest.Set(t, map[string]string{"sources.known_roots": `["` + roots + `"]`})
	notes := filepath.Join(roots, "local", "notes")
	if err := os.MkdirAll(filepath.Join(roots, "mine", "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	dupA, dupB := filepath.Join(roots, "a", "dup"), filepath.Join(roots, "b", "dup")
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = sources.NewRegistry(conn).Add(ctx, sources.Source{Kind: sources.KindOwner, URI: "mine", RefreshSecs: 3600})
	_ = sources.NewRegistry(conn).Add(ctx, sources.Source{Kind: sources.KindRepo, URI: notes, RefreshSecs: 3600})
	_ = sources.NewRegistry(conn).Add(ctx, sources.Source{Kind: sources.KindRepo, URI: dupA, RefreshSecs: 3600})
	_ = sources.NewRegistry(conn).Add(ctx, sources.Source{Kind: sources.KindRepo, URI: dupB, RefreshSecs: 3600})
	_ = docs.NewRegistry(conn).SetHash(ctx, "repo://local/notes/SYNCED.md", "h", 1)
	_ = conn.Close()
	notesCopy := sources.CheckoutURI(filepath.Join(notes, "README.md"))
	ownerCopy := sources.CheckoutURI(filepath.Join(roots, "mine", "proj", "README.md"))
	sc := &storeClient{fakeClient: &fakeClient{}, docs: map[string]*orchdv1.Document{
		notesCopy:                    {SourceUri: notesCopy, SourceType: "repo_doc", Writer: "sources", Text: "new"},
		ownerCopy:                    {SourceUri: ownerCopy, SourceType: "repo_doc", Writer: "sources", Text: "new owner copy"},
		"repo://local/notes/ONLY.md": {SourceUri: "repo://local/notes/ONLY.md", SourceType: "repo_doc", Text: "no replacement yet"},
		// Two repo sources named dup: the old copy may be either's, so it stays.
		sources.CheckoutURI(filepath.Join(dupB, "README.md")): {SourceUri: sources.CheckoutURI(filepath.Join(dupB, "README.md")), Writer: "sources", Text: "b"},
		"repo://local/dup/README.md":                          {SourceUri: "repo://local/dup/README.md", SourceType: "repo_doc", Text: "a or b"},
		"repo://local/notes/../x.md":                          {SourceUri: "repo://local/notes/../x.md", SourceType: "repo_doc", Text: "dot segment"},
		"repo://local/notes/README.md":                        {SourceUri: "repo://local/notes/README.md", SourceType: "repo_doc", Text: "old"},
		"repo://local/notes/SYNCED.md":                        {SourceUri: "repo://local/notes/SYNCED.md", SourceType: "repo_doc", Text: "has a hash"},
		"repo://local/notes/NEW.md":                           {SourceUri: "repo://local/notes/NEW.md", SourceType: "repo_doc", Writer: "client", Text: "a client's"},
		"repo://mine/proj/README.md":                          {SourceUri: "repo://mine/proj/README.md", SourceType: "repo_doc", Text: "old owner copy"},
		"repo://other/proj/README.md":                         {SourceUri: "repo://other/proj/README.md", SourceType: "repo_doc", Text: "not ours"},
	}}
	out, err := run(func() (orchdv1.OrchdClient, func(), error) { return sc, func() {}, nil }, "", "rag", "dedupe")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repo://local/notes/README.md", "repo://mine/proj/README.md", "stored by an earlier sources refresh"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, kept := range []string{"SYNCED.md", "NEW.md", "ONLY.md", "repo://other/", "repo://local/dup/", "x.md"} {
		if strings.Contains(out, kept) {
			t.Errorf("%s listed:\n%s", kept, out)
		}
	}
}
