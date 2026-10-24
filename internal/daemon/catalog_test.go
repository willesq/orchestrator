package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// catalogServer is a server over a fresh orch.db with one synced docs owner.
func catalogServer(t *testing.T) (*server, *recordingRag, *fakeSupervisor) {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	dr := docs.NewRegistry(conn)
	if err := dr.AddOwner(ctx, docs.Owner{Owner: "willesq", RefreshSecs: 3600, Tags: []string{"mine"}}); err != nil {
		t.Fatal(err)
	}
	if err := dr.SetRepoState(ctx, docs.RepoState{Owner: "willesq", Repo: "orchestrator", HeadSHA: sha, LastSynced: 100, Files: 2, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"repo://willesq/orchestrator/README.md", "repo://willesq/orchestrator/docs/a b.md", "repo://bad"} {
		if err := dr.SetHash(ctx, uri, "h", 200); err != nil {
			t.Fatal(err)
		}
	}
	r := &recordingRag{}
	sup := &fakeSupervisor{}
	return &server{docsReg: dr, sourcesReg: sources.NewRegistry(conn), rag: r, sup: sup}, r, sup
}

// indexedDoc is one source in recordingRag's store.
type indexedDoc struct {
	uri, sourceType string
	tags            []string
}

// recordingRag lists indexed sources and records deletions.
type recordingRag struct {
	fakeRag
	indexed   []indexedDoc
	deleted   []string
	deleteErr error
	listErr   error
}

func (r *recordingRag) ListSources(_ context.Context, opts rag.ListSourcesOpts) ([]rag.SourceSummary, int, int, error) {
	if r.listErr != nil {
		return nil, 0, 0, r.listErr
	}
	var out []rag.SourceSummary
	for _, d := range r.indexed {
		if strings.HasPrefix(d.uri, opts.URIPrefix) && (opts.SourceType == "" || opts.SourceType == d.sourceType) {
			out = append(out, rag.SourceSummary{SourceURI: d.uri, SourceType: d.sourceType, Tags: d.tags, Chunks: 2})
		}
	}
	return out, len(out), 2 * len(out), nil
}

func (r *recordingRag) DeleteSource(_ context.Context, uri string) (int, error) {
	if r.deleteErr != nil {
		return 0, r.deleteErr
	}
	r.deleted = append(r.deleted, uri)
	return 2, nil
}

type fakeDocuments struct {
	doc rag.Document
	err error
}

func (f fakeDocuments) Document(context.Context, string) (rag.Document, error) { return f.doc, f.err }

func code(err error) codes.Code { return grpcstatus.Code(err) }

func TestListDocs(t *testing.T) {
	s, r, _ := catalogServer(t)
	r.indexed = []indexedDoc{
		{uri: "repo://willesq/orchestrator/README.md", sourceType: "repo_doc"},
		{uri: "repo://willesq/orchestrator/docs/a b.md", sourceType: "repo_doc"},
		// In the store but not recorded by the docs sync (a forged memory, or a
		// local owner folder of the same name): left out.
		{uri: "repo://willesq/scratch/NOTES.md", sourceType: "repo_doc"},
		// Not a synced owner's, or not a doc: left out.
		{uri: "repo://local/app/README.md", sourceType: "repo_doc"},
		{uri: "repo://willesq/orchestrator/x.md", sourceType: "manual"},
	}
	resp, err := s.ListDocs(context.Background(), &orchdv1.ListDocsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetOwners()) != 1 || resp.GetOwners()[0].GetOwner() != "willesq" || len(resp.GetOwners()[0].GetRepos()) != 1 {
		t.Fatalf("owners = %v", resp.GetOwners())
	}
	repo := resp.GetOwners()[0].GetRepos()[0]
	if repo.GetHeadSha() != sha || repo.GetFiles() != 2 || repo.GetWebUrl() != "https://github.com/willesq/orchestrator" {
		t.Errorf("repo = %v", repo)
	}
	files := resp.GetFiles()
	// The registry also recorded repo://bad, which the store does not hold.
	if len(files) != 2 {
		t.Fatalf("files = %v, want the two docs both stored and recorded", files)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.GetPath()] = f.GetWebUrl()
		if f.GetChunks() != 2 {
			t.Errorf("%s chunks = %d, want the store's count", f.GetPath(), f.GetChunks())
		}
	}
	if _, ok := got["NOTES.md"]; ok {
		t.Error("a doc the sync never recorded was listed")
	}
	if files[0].GetUpdatedAt() != 200 {
		t.Errorf("updated_at = %d, want the registry's change time", files[0].GetUpdatedAt())
	}
	if got["README.md"] != "https://github.com/willesq/orchestrator/blob/"+sha+"/README.md" {
		t.Errorf("README url = %q", got["README.md"])
	}
	if got["docs/a b.md"] != "https://github.com/willesq/orchestrator/blob/"+sha+"/docs/a%20b.md" {
		t.Errorf("escaped url = %q", got["docs/a b.md"])
	}
	if _, err := (&server{}).ListDocs(context.Background(), nil); code(err) != codes.Unavailable {
		t.Errorf("no registry err = %v", err)
	}
	r.listErr = errors.New("scroll failed")
	if _, err := s.ListDocs(context.Background(), nil); err == nil {
		t.Error("a failed store listing should fail ListDocs")
	}
	s.rag = nil
	if _, err := s.ListDocs(context.Background(), nil); code(err) != codes.Unavailable {
		t.Errorf("no store err = %v", err)
	}
}

func TestGetDocument(t *testing.T) {
	ctx := context.Background()
	s, _, _ := catalogServer(t)
	s.documents = fakeDocuments{doc: rag.Document{Meta: rag.Meta{SourceType: "repo_doc", Scope: "work", Tags: []string{"repo:orchestrator"}},
		Text: "# Hi", Chunks: 3}}

	doc, err := s.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: "repo://willesq/orchestrator/README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if doc.GetText() != "# Hi" || doc.GetChunks() != 3 || doc.GetSourceType() != "repo_doc" ||
		doc.GetWebUrl() != "https://github.com/willesq/orchestrator/blob/"+sha+"/README.md" {
		t.Errorf("doc = %v", doc)
	}

	tests := []struct {
		name         string
		uri          string
		docs         documentReader
		wantURL      string
		wantOversize bool
		code         codes.Code
	}{
		{name: "a web page links to itself", uri: "https://go.dev/doc", docs: fakeDocuments{doc: rag.Document{Meta: rag.Meta{SourceType: "web"}, Chunks: 1}}, wantURL: "https://go.dev/doc"},
		{name: "a URL-named memory is not offered as a link", uri: "https://phish.example/x", docs: fakeDocuments{doc: rag.Document{Meta: rag.Meta{SourceType: "manual"}, Chunks: 1}}},
		{name: "too large to return", uri: "manual://big", docs: fakeDocuments{doc: rag.Document{Text: strings.Repeat("x", maxDocumentBytes+1), Chunks: 9}}, wantOversize: true},
		{name: "a local owner directory's doc has no link", uri: "repo://willesq/other/README.md", docs: fakeDocuments{doc: rag.Document{Meta: rag.Meta{SourceType: "repo_doc"}, Chunks: 1}}},
		{name: "a manual note has no link", uri: "manual://note", docs: fakeDocuments{doc: rag.Document{Chunks: 1}}},
		{name: "nothing indexed", uri: "manual://none", docs: fakeDocuments{err: rag.ErrSourceNotFound}, code: codes.NotFound},
		{name: "store failure", uri: "manual://x", docs: fakeDocuments{err: errors.New("qdrant down")}, code: codes.Unknown},
		{name: "empty uri", uri: "", docs: fakeDocuments{}, code: codes.InvalidArgument},
		{name: "overlong uri", uri: strings.Repeat("x", maxSourceURI+1), docs: fakeDocuments{}, code: codes.InvalidArgument},
		{name: "no document store", uri: "manual://x", code: codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.documents = tt.docs
			doc, err := s.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: tt.uri})
			if tt.code != codes.OK {
				if code(err) != tt.code {
					t.Fatalf("err = %v, want %v", err, tt.code)
				}
				return
			}
			if err != nil || doc.GetWebUrl() != tt.wantURL || doc.GetOversize() != tt.wantOversize ||
				(tt.wantOversize && doc.GetText() != "") {
				t.Errorf("doc = %.200v, %v; want url %q, oversize %v", doc, err, tt.wantURL, tt.wantOversize)
			}
		})
	}
}

func TestSourceLifecycleRPCs(t *testing.T) {
	ctx := context.Background()
	s, r, sup := catalogServer(t)

	web, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{
		Kind: "web", Uri: " https://go.dev/doc ", RefreshSecs: 86400, Tags: []string{"go", "go"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if web.GetId() != "web|https://go.dev/doc" || web.GetStatus() != "new" || web.GetWebUrl() != "https://go.dev/doc" ||
		web.GetSearchSourceUri() != "https://go.dev/doc" || len(web.GetTags()) != 1 {
		t.Errorf("saved web = %v", web)
	}
	owner, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{Kind: "owner", Uri: "acme", RefreshSecs: 3600}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(owner.GetSearchAllTags(), ",") != "owner:acme" || owner.GetWebUrl() != "" {
		t.Errorf("saved owner = %v", owner)
	}
	if _, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{Kind: "owner", Uri: "../etc", RefreshSecs: 3600}}); code(err) != codes.InvalidArgument {
		t.Errorf("traversal err = %v, want InvalidArgument", err)
	}

	// Editing the URL replaces the entry.
	edited, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{
		Source:    &orchdv1.RegisteredSource{Kind: "web", Uri: "https://go.dev/ref", RefreshSecs: 3600},
		ReplaceId: web.GetId(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{
		Source: &orchdv1.RegisteredSource{Kind: "web", Uri: "https://x.dev", RefreshSecs: 3600}, ReplaceId: "web|missing",
	}); code(err) != codes.NotFound {
		t.Errorf("replace missing err = %v, want NotFound", err)
	}
	list, err := s.ListRegisteredSources(ctx, &orchdv1.ListRegisteredSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, src := range list.GetSources() {
		ids = append(ids, src.GetId())
	}
	sort.Strings(ids)
	if strings.Join(ids, " ") != "owner|acme "+edited.GetId() {
		t.Errorf("ids after edit = %v", ids)
	}

	// Refresh one, then all; each restarts the scheduler.
	if resp, err := s.RefreshSource(ctx, &orchdv1.RefreshSourceRequest{Id: edited.GetId()}); err != nil || resp.GetMarked() != 1 {
		t.Errorf("refresh one = %v, %v", resp, err)
	}
	if resp, err := s.RefreshSource(ctx, &orchdv1.RefreshSourceRequest{}); err != nil || resp.GetMarked() != 2 {
		t.Errorf("refresh all = %v, %v", resp, err)
	}
	// The second refresh came within the gap, so the scheduler restarted once.
	if len(sup.restarted) != 1 || sup.restarted[0] != UnitSources {
		t.Errorf("scheduler restarts = %v", sup.restarted)
	}
	s.lastNudge = s.lastNudge.Add(-refreshNudgeGap)
	if _, err := s.RefreshSource(ctx, &orchdv1.RefreshSourceRequest{Id: "web|missing"}); code(err) != codes.NotFound {
		t.Errorf("refresh missing err = %v", err)
	}
	sup.restartErr = errors.New("busy")
	if _, err := s.RefreshSource(ctx, &orchdv1.RefreshSourceRequest{}); err != nil {
		t.Errorf("a failed nudge should not fail the refresh: %v", err)
	}

	// Purging an owner deletes the markdown its refresh stored, and nothing
	// else under the prefix: not untagged memories, not other types.
	r.indexed = []indexedDoc{
		{uri: "repo://acme/notes/todo.md", sourceType: "repo_doc", tags: []string{"owner:acme", "repo:notes"}},
		{uri: "repo://acme/notes/agent.md", sourceType: "manual", tags: []string{"owner:acme"}},
		{uri: "repo://acme/notes/untagged.md", sourceType: "repo_doc"},
		{uri: "repo://other/x.md", sourceType: "repo_doc", tags: []string{"owner:other"}},
	}
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "owner|acme", Purge: true}); code(err) != codes.FailedPrecondition {
		t.Errorf("purge without confirm err = %v", err)
	}
	r.deleteErr = errors.New("qdrant down")
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "owner|acme", Purge: true, Confirm: true}); err == nil {
		t.Error("a failed purge should fail the removal")
	}
	if _, err := s.findSource(ctx, "owner|acme"); err != nil {
		t.Errorf("a failed purge unregistered the source: %v", err)
	}
	r.deleteErr = nil
	resp, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "owner|acme", Purge: true, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.deleted, ",") != "repo://acme/notes/todo.md" || resp.GetPurgedSources() != 1 || resp.GetPurgedChunks() != 2 {
		t.Errorf("purged %v, resp %v", r.deleted, resp)
	}

	// An owner the GitHub docs sync tracks keeps its docs, even ones whose
	// hashes were cleared.
	r.deleted = nil
	r.indexed = []indexedDoc{
		{uri: "repo://willesq/orchestrator/README.md", sourceType: "repo_doc", tags: []string{"owner:willesq"}},
		{uri: "repo://willesq/cleared/README.md", sourceType: "repo_doc", tags: []string{"owner:willesq"}},
	}
	if _, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{Kind: "owner", Uri: "willesq", RefreshSecs: 3600}}); err != nil {
		t.Fatal(err)
	}
	if resp, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "owner|willesq", Purge: true, Confirm: true}); err != nil || len(r.deleted) != 0 || resp.GetPurgedSources() != 0 {
		t.Errorf("synced owner purge = %v, %v, deleted %v", resp, err, r.deleted)
	}

	// A web source purges its URL; removing without purge deletes nothing.
	r.deleted = nil
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: edited.GetId()}); err != nil || len(r.deleted) != 0 {
		t.Errorf("plain remove = %v, deleted %v", err, r.deleted)
	}
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: edited.GetId()}); code(err) != codes.NotFound {
		t.Errorf("remove twice err = %v", err)
	}
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{}); code(err) != codes.InvalidArgument {
		t.Errorf("remove without id err = %v", err)
	}
}

func TestPurgeWebAndRepoSources(t *testing.T) {
	ctx := context.Background()
	s, r, _ := catalogServer(t)
	for _, src := range []*orchdv1.RegisteredSource{
		{Kind: "web", Uri: "https://go.dev/doc", RefreshSecs: 3600},
		{Kind: "repo", Uri: "/Users/me/code/app", RefreshSecs: 3600},
	} {
		if _, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	r.indexed = []indexedDoc{
		{uri: "repo://local/app/README.md", sourceType: "repo_doc", tags: []string{"repo:app"}},
		{uri: "repo://local/application/x.md", sourceType: "repo_doc", tags: []string{"repo:application"}},
	}
	for _, id := range []string{"web|https://go.dev/doc", "repo|/Users/me/code/app"} {
		if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: id, Purge: true, Confirm: true}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(r.deleted, ",") != "https://go.dev/doc,repo://local/app/README.md" {
		t.Errorf("deleted = %v", r.deleted)
	}
	list, _ := s.ListRegisteredSources(ctx, nil)
	if len(list.GetSources()) != 0 {
		t.Errorf("left = %v", list.GetSources())
	}
	// Two repo sources of one name share their content: neither purges.
	for _, uri := range []string{"/x/app", "/y/app"} {
		if _, err := s.SaveSource(ctx, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{Kind: "repo", Uri: uri, RefreshSecs: 3600}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "repo|/y/app", Purge: true, Confirm: true}); code(err) != codes.FailedPrecondition {
		t.Errorf("shared-name purge err = %v, want FailedPrecondition", err)
	}
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "repo|/y/app"}); err != nil {
		t.Fatal(err)
	}
	r.listErr = errors.New("scroll failed")
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "repo|/x/app", Purge: true, Confirm: true}); err == nil {
		t.Error("a failed listing should fail the purge")
	}
	repo, _ := s.findSource(ctx, "repo|/x/app")
	if got := registeredSource(repo).GetSearchAllTags(); strings.Join(got, ",") != "repo:app" {
		t.Errorf("repo search tags = %v", got)
	}
}

func TestPurgeRefusesAnInvalidRow(t *testing.T) {
	ctx := context.Background()
	s, r, _ := catalogServer(t)
	// A row stored before validation existed: an owner that climbs out of
	// the known roots.
	if err := s.sourcesReg.Add(ctx, sources.Source{Kind: "owner", URI: "../etc", RefreshSecs: 3600}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "owner|../etc", Purge: true, Confirm: true}); code(err) != codes.FailedPrecondition {
		t.Errorf("invalid row purge err = %v, want FailedPrecondition", err)
	}
	if _, err := s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{Id: "owner|../etc"}); err != nil || len(r.deleted) != 0 {
		t.Errorf("plain removal = %v, deleted %v", err, r.deleted)
	}
}

func TestListDocsIsBounded(t *testing.T) {
	ctx := context.Background()
	s, r, _ := catalogServer(t)
	for i := range maxDocFiles + 5 {
		uri := fmt.Sprintf("repo://willesq/orchestrator/f%05d.md", i)
		r.indexed = append(r.indexed, indexedDoc{uri: uri, sourceType: "repo_doc"})
		if err := s.docsReg.SetHash(ctx, uri, "h", 1); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := s.ListDocs(ctx, &orchdv1.ListDocsRequest{})
	if err != nil || len(resp.GetFiles()) != maxDocFiles || !resp.GetTruncated() {
		t.Errorf("files = %d, truncated %v, err %v", len(resp.GetFiles()), resp.GetTruncated(), err)
	}
}

func TestListDocsFitsTheMessageLimit(t *testing.T) {
	ctx := context.Background()
	s, r, _ := catalogServer(t)
	long := strings.Repeat("deeply/nested/", 30)
	for i := range 6000 {
		uri := fmt.Sprintf("repo://willesq/orchestrator/%s%05d.md", long, i)
		r.indexed = append(r.indexed, indexedDoc{uri: uri, sourceType: "repo_doc"})
		if err := s.docsReg.SetHash(ctx, uri, "h", 1); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := s.ListDocs(ctx, &orchdv1.ListDocsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if size := proto.Size(resp); size > 4<<20 || !resp.GetTruncated() {
		t.Errorf("reply is %d bytes with %d files (truncated %v); want under 4 MiB and truncated", size, len(resp.GetFiles()), resp.GetTruncated())
	}
}

func TestSearchTagLimits(t *testing.T) {
	s := &server{rag: fakeRag{}}
	ctx := context.Background()
	if _, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q", AllTags: make([]string, maxFilterTags+1)}); code(err) != codes.InvalidArgument {
		t.Errorf("too many tags err = %v", err)
	}
	if _, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q", Tags: []string{strings.Repeat("t", maxFilterTagLen+1)}}); code(err) != codes.InvalidArgument {
		t.Errorf("long tag err = %v", err)
	}
	if _, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q", AllTags: []string{"owner:o", "repo:r"}, SourceUri: "u"}); err != nil {
		t.Errorf("valid filters err = %v", err)
	}
}

func TestCatalogUnavailable(t *testing.T) {
	ctx := context.Background()
	s := &server{}
	for name, err := range map[string]error{
		"list":    second(s.ListRegisteredSources(ctx, nil)),
		"save":    second(s.SaveSource(ctx, &orchdv1.SaveSourceRequest{})),
		"remove":  second(s.RemoveSource(ctx, &orchdv1.RemoveSourceRequest{})),
		"refresh": second(s.RefreshSource(ctx, &orchdv1.RefreshSourceRequest{})),
	} {
		if code(err) != codes.Unavailable {
			t.Errorf("%s err = %v, want Unavailable", name, err)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestGitHubURLs(t *testing.T) {
	tests := []struct {
		owner, repo, sha, path, want string
	}{
		{"o", "r", sha, "a/b.md", "https://github.com/o/r/blob/" + sha + "/a/b.md"},
		{"o", "r", "", "README.md", "https://github.com/o/r/blob/HEAD/README.md"},
		{"o", "r", "not-a-sha", "x#y?.md", "https://github.com/o/r/blob/HEAD/x%23y%3F.md"},
		{"o", "r", sha, "a/../b.md", ""},
		{"o", "r", sha, "a//b.md", ""},
		{"bad owner", "r", sha, "a.md", ""},
		{"o", "r/x", sha, "a.md", ""},
		{"o", "..", sha, "a.md", ""},
	}
	for _, tt := range tests {
		if got := gitHubFileURL(tt.owner, tt.repo, tt.sha, tt.path); got != tt.want {
			t.Errorf("gitHubFileURL(%q, %q, %q, %q) = %q, want %q", tt.owner, tt.repo, tt.sha, tt.path, got, tt.want)
		}
	}
	for _, uri := range []string{"repo://o/r", "repo:///r/p", "https://x/y", "repo://o//p"} {
		if _, _, _, ok := splitRepoURI(uri); ok {
			t.Errorf("splitRepoURI(%q) ok", uri)
		}
	}
	for raw, want := range map[string]bool{"https://x.dev/a": true, "http://x": true, "ftp://x": false, "https://u:p@x": false, "https:///p": false} {
		if isWebURL(raw) != want {
			t.Errorf("isWebURL(%q) = %v", raw, !want)
		}
	}
}
