package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The public-space write gate (SP-030): only content verified public
// reaches the public space "oss" of the plan's example policy, whose root is
// the whole code folder and whose org is oss-org.

func (e writesEnv) placer() *writePlacer {
	return &writePlacer{spaces: e.s.spaces, pins: e.s.pins, repos: e.s.repos}
}

// syncOSS runs one docs sync of oss-org listing repos, with the gate's
// visibility re-check, cloning through git.
func (e writesEnv) syncOSS(t *testing.T, reg *docs.Registry, git docs.Git, repos ...docs.Repo) docs.Result {
	t.Helper()
	return docs.SyncOwner(context.Background(), reg, docs.Owner{Owner: "oss-org"}, e.svc, docs.SyncOpts{
		RepoCache: filepath.Join(e.code, ".cache"), GH: docsGH(repos), Git: git,
		CheckOwner: docsOwnerCheck(e.s.spaces), LeavesPublic: e.placer().leavesPublic,
	})
}

// TestPrivateRepoInPublicOrgIsQuarantined is SP-030.1.a1: a private repo of
// the org bound to the public space goes to quarantine, through the docs
// sync and through a local checkout of it, while its public sibling is
// public.
func TestPrivateRepoInPublicOrgIsQuarantined(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	res := e.syncOSS(t, docs.NewRegistry(mustDB(t)), &docsGit{},
		docs.Repo{Name: "lib", HeadSHA: "l1", OwnerNodeID: "O_oss"},
		docs.Repo{Name: "secret", HeadSHA: "s1", Private: true, OwnerNodeID: "O_oss"})
	if res.Status != "ok" || res.Synced != 2 {
		t.Fatalf("sync = %+v", res)
	}
	e.placedAs(t, "repo://oss-org/lib/README.md", "oss", rag.OriginGitHub)
	e.placedAs(t, "repo://oss-org/secret/README.md", spaces.Quarantine, rag.OriginGitHub)

	secret := filepath.Join(e.code, "secret") // a checkout of oss-org/secret (private)
	defaultCheckout(t, secret, "oss-org", "secret")
	writeFile(t, filepath.Join(secret, "README.md"), "# secret")
	if st, err := sources.Refresh(context.Background(), sources.Source{Kind: sources.KindRepo, URI: secret}, e.svc,
		sources.RefreshOpts{LeavesPublic: e.placer().leavesPublic}); err != nil || st != "ok" {
		t.Fatalf("refresh = %q, %v", st, err)
	}
	e.placedAs(t, sources.CheckoutURI(filepath.Join(secret, "README.md")), spaces.Quarantine, rag.OriginFS)
}

// checkingGit clones like docsGit and, before cloning any repo but the one
// that turned private, checks that repo's doc is no longer public.
type checkingGit struct {
	docsGit
	t       *testing.T
	e       writesEnv
	flipped string
	checked int
}

func (g *checkingGit) Sync(ctx context.Context, owner, repo, dest string) error {
	if repo != g.flipped {
		for _, p := range g.e.q.stored("repo://oss-org/" + g.flipped + "/README.md") {
			if p["space"] == "oss" {
				g.t.Errorf("%s was cloned while %s was still public", repo, g.flipped)
			}
		}
		g.checked++
	}
	return g.docsGit.Sync(ctx, owner, repo, dest)
}

// TestRepoTurnedPrivateLeavesPublicInTheSameSync is SP-030.1.a2: a repo
// GitHub now reports private, with the same HEAD, leaves the public space
// in the next sync before any other repo is cloned or indexed, and is
// placed in quarantine in that same sync.
func TestRepoTurnedPrivateLeavesPublicInTheSameSync(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	reg := docs.NewRegistry(mustDB(t))
	if res := e.syncOSS(t, reg, &docsGit{}, docs.Repo{Name: "lib", HeadSHA: "l1", OwnerNodeID: "O_oss"}); res.Synced != 1 {
		t.Fatalf("first sync = %+v", res)
	}
	e.placedAs(t, "repo://oss-org/lib/README.md", "oss", rag.OriginGitHub)

	git := &checkingGit{t: t, e: e, flipped: "lib"}
	res := e.syncOSS(t, reg, git,
		docs.Repo{Name: "aaa", HeadSHA: "a1", OwnerNodeID: "O_oss"}, // listed first
		docs.Repo{Name: "lib", HeadSHA: "l1", Private: true, OwnerNodeID: "O_oss"})
	if res.Status != "ok" || res.Synced != 2 || git.checked != 1 {
		t.Fatalf("second sync = %+v, other repos checked %d", res, git.checked)
	}
	if strings.Join(git.synced, ",") != "lib,aaa" {
		t.Fatalf("cloned %v, want the repo that left public first", git.synced)
	}
	e.placedAs(t, "repo://oss-org/lib/README.md", spaces.Quarantine, rag.OriginGitHub)
	e.placedAs(t, "repo://oss-org/aaa/README.md", "oss", rag.OriginGitHub)
}

// TestUnreadableVisibilityIsPrivate is SP-030.1.a3: a repo whose
// visibility GitHub did not vouch for (listed without its owner's node ID,
// or a checkout whose repo cannot be looked up) is treated as private: it
// is never placed public, and a public copy leaves.
func TestUnreadableVisibilityIsPrivate(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	reg := docs.NewRegistry(mustDB(t))
	e.syncOSS(t, reg, &docsGit{}, docs.Repo{Name: "lib", HeadSHA: "l1", OwnerNodeID: "O_oss"})
	e.placedAs(t, "repo://oss-org/lib/README.md", "oss", rag.OriginGitHub)
	if res := e.syncOSS(t, reg, &docsGit{}, docs.Repo{Name: "lib", HeadSHA: "l1"}, docs.Repo{Name: "new", HeadSHA: "n1"}); res.Synced != 2 {
		t.Fatalf("sync = %+v", res)
	}
	e.placedAs(t, "repo://oss-org/lib/README.md", spaces.Quarantine, rag.OriginGitHub)
	e.placedAs(t, "repo://oss-org/new/README.md", spaces.Quarantine, rag.OriginGitHub)

	// A checkout whose repo GitHub will not show this login.
	e.s.repos = newRepoLookups(func(context.Context, ...string) ([]byte, error) { return nil, errors.New("gh: Not Found (HTTP 404)") })
	lib := filepath.Join(e.code, "lib") // a checkout of oss-org/lib
	writeFile(t, filepath.Join(lib, "README.md"), "# lib")
	m := rag.Meta{SourceURI: sources.CheckoutURI(filepath.Join(lib, "README.md")), Writer: rag.WriterSources,
		Origin: rag.Origin{System: rag.OriginFS, Path: filepath.Join(lib, "README.md")}}
	if err := e.placer().place(ctx, &m); err != nil || m.Space != spaces.Quarantine {
		t.Fatalf("unreadable checkout placed in %q, %v", m.Space, err)
	}
	if !e.placer().leavesPublic(ctx, "oss", m) {
		t.Fatal("a public copy of a checkout whose visibility cannot be read stays public")
	}
}

// TestSessionWritersNeverTargetPublic is SP-030.1.a4: a session record, a
// summary, a hand-off and a client note never land in the public space,
// even when their session is pinned to it or the file is in a verified
// public repo under the public root.
func TestSessionWritersNeverTargetPublic(t *testing.T) {
	ctx := context.Background()
	pinOSS := func(t *testing.T, e writesEnv, id string) {
		t.Helper()
		if err := e.s.pins.insert(ctx, sessionPin{SessionID: id, Harness: "claude", Process: procstat.Identity{PID: 9, Started: time.Unix(1, 0)},
			FirstCwd: filepath.Join(e.code, "lib"), Space: "oss", Revision: e.rev, PinnedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("session", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		pinOSS(t, e, "s-pub")
		if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "s-pub", Summary: "x", Cwd: e.code}); err != nil {
			t.Fatal(err)
		}
		if err := storeSession(ctx, e.svc, handoff.Session{ID: "s-pub", TranscriptPath: sessionTranscript(t, "s-pub"), Record: true}); err != nil {
			t.Fatal(err)
		}
		for _, uri := range []string{"session://s-pub/transcript", "session://s-pub/record"} {
			e.placedAs(t, uri, spaces.Quarantine, rag.OriginSession)
		}
	})

	t.Run("summary", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		pinOSS(t, e, "s-sum")
		if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: "s-sum", Cwd: e.code}); err != nil {
			t.Fatal(err)
		}
		summarizeOne(ctx, fakeSummarizer{summary: "the summary"}, e.svc, summaryJob{SessionID: "s-sum", TranscriptPath: sessionTranscript(t, "s-sum")}, time.Minute)
		e.placedAs(t, "session://s-sum/record", spaces.Quarantine, rag.OriginSession)
	})

	t.Run("hand-off", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		// oss-org/lib is public and bound to oss, under the public root;
		// oss-org/secret is private.
		doc, secret := filepath.Join(e.code, "lib", "NOTES.md"), filepath.Join(e.code, "secret", "NOTES.md")
		for _, f := range []string{doc, secret} {
			writeFile(t, f, "# notes")
			// The sources copy is quarantined (D-65); placing it also puts
			// the repo's visibility in the cache hand-offs read.
			m := rag.Meta{Writer: rag.WriterSources, Origin: rag.Origin{System: rag.OriginFS, Path: f}}
			if err := e.placer().place(ctx, &m); err != nil || m.Space != spaces.Quarantine {
				t.Fatalf("the sources copy placed in %q, %v", m.Space, err)
			}
			if err := storeFile(ctx, e.svc, handoff.File{Path: f}, "", nil); err != nil {
				t.Fatal(err)
			}
		}
		// The hand-off is session content under a public root, so it goes
		// to public_root_sessions (personal); a private repo's stays out.
		e.placedAs(t, "file://"+doc, "personal", rag.OriginFS)
		e.placedAs(t, "file://"+secret, spaces.Quarantine, rag.OriginFS)
	})

	t.Run("client", func(t *testing.T) {
		e := newWritesEnv(t, orgNodes{})
		pin := sessionPin{SessionID: "s-cl", Space: "oss", Revision: e.rev}
		for _, uri := range []string{"manual://note", "https://docs.oss.dev/page"} {
			if _, err := e.s.Ingest(sessionCtx(ctx, pin), &orchdv1.IngestRequest{SourceUri: uri, Text: "a note"}); err != nil {
				t.Fatal(err)
			}
			e.placedAs(t, uri, spaces.Quarantine, rag.OriginClient)
		}
	})
}

// applyOSSRoots applies the example policy again with extra public roots
// and returns the new revision.
func applyOSSRoots(t *testing.T, e *writesEnv, extra ...string) {
	t.Helper()
	applyOSS(t, e, extra, nil)
}

// applyOSSPrefix applies the example policy again with an extra web prefix
// bound to oss.
func applyOSSPrefix(t *testing.T, e *writesEnv, prefix string) {
	t.Helper()
	applyOSS(t, e, nil, []string{prefix})
}

func applyOSS(t *testing.T, e *writesEnv, roots, prefixes []string) {
	t.Helper()
	p := spaces.Policy{
		Spaces: []spaces.Space{
			{Name: "oss", Kind: spaces.Public, GitHubOrgs: []string{"oss-org"}, Roots: append([]string{e.code}, roots...), WebPrefixes: append([]string{"https://docs.oss.dev/"}, prefixes...)},
			{Name: "personal", Kind: spaces.Private, GitHubOrgs: []string{"willesq"}, Roots: []string{e.code + "/willesq"}},
			{Name: "acme", Kind: spaces.Isolated, GitHubOrgs: []string{"acme"}, Roots: []string{e.code + "/company1"}, Chat: []string{"slack:TACME"}},
			{Name: "globex", Kind: spaces.Isolated, GitHubOrgs: []string{"globex"}, Chat: []string{"teams:globex-tenant"}},
		},
		PublicRootSessions: "personal",
	}
	doc, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	d := draft(t, e.s, string(doc))
	if len(d.GetProblems()) > 0 {
		t.Fatalf("policy problems: %v", d.GetProblems())
	}
	if _, err := e.s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	if _, e.rev, err = e.s.spaces.CurrentRevision(); err != nil {
		t.Fatal(err)
	}
}

// TestCheckoutWithoutGitHubRemoteNeedsProofUnderPublicRoot is SP-030.1.a5
// (D-50, D-65): under the public root, a checkout whose remote is not on
// GitHub, or that has none, is quarantined, and so is every other local
// folder: nothing on disk declares a folder public. A .git at, above or
// inside the public root, a symlinked .git, a verified public GitHub
// checkout, and a public root bound exactly at the folder all leave it in
// quarantine until the policy's public_paths (SP-076) or a retag.
func TestCheckoutWithoutGitHubRemoteNeedsProofUnderPublicRoot(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	gitlab := filepath.Join(e.code, "gl")
	defaultCheckout(t, gitlab, "x", "y")
	writeFile(t, filepath.Join(gitlab, ".git", "config"), "[remote \"origin\"]\n\turl = git@gitlab.com:x/y.git\n"+
		"[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n")
	writeFile(t, filepath.Join(gitlab, "README.md"), "# gitlab")
	local := filepath.Join(e.code, "local") // a checkout with no remote
	writeFile(t, filepath.Join(local, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(local, "docs", "README.md"), "# local")
	symlinked := filepath.Join(e.code, "repo-tool") // its .git is a symbolic link
	gitdir := filepath.Join(t.TempDir(), "gitdir")
	writeFile(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(symlinked, "README.md"), "# tool")
	if err := os.Symlink(gitdir, filepath.Join(symlinked, ".git")); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(e.code, "notes") // no checkout at all
	writeFile(t, filepath.Join(plain, "README.md"), "# notes")
	lib := filepath.Join(e.code, "lib") // oss-org/lib, verified public
	writeFile(t, filepath.Join(lib, "README.md"), "# lib")
	files := []string{filepath.Join(gitlab, "README.md"), filepath.Join(local, "docs", "README.md"),
		filepath.Join(symlinked, "README.md"), filepath.Join(plain, "README.md"), filepath.Join(lib, "README.md")}

	type where struct {
		Space string
		Rule  spaces.PlaceRule
	}
	quarantined := where{spaces.Quarantine, spaces.PlaceNoPublicProof}
	place := func(path string) where {
		t.Helper()
		p, err := e.s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: path})
		if err != nil {
			t.Fatal(err)
		}
		m := rag.Meta{Writer: rag.WriterSources, Origin: rag.Origin{System: rag.OriginFS, Path: path}}
		if err := e.placer().place(ctx, &m); err != nil || m.Space != p.GetSpace() {
			t.Fatalf("%s: written to %q, %v; explained %q", path, m.Space, err, p.GetSpace())
		}
		return where{p.GetSpace(), spaces.PlaceRule(p.GetRule())}
	}
	check := func(when string) {
		t.Helper()
		for _, f := range files {
			if got := place(f); got != quarantined {
				t.Errorf("%s: %s = %+v, want %+v", when, f, got, quarantined)
			}
		}
	}
	check("as found")
	if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindRepo, URI: gitlab}, e.svc, sources.RefreshOpts{}); err != nil || st != "ok" {
		t.Fatalf("refresh = %q, %v", st, err)
	}
	e.placedAs(t, sources.CheckoutURI(filepath.Join(gitlab, "README.md")), spaces.Quarantine, rag.OriginFS)

	// A .git at the public root, above it, or inside it (around the plain
	// folder) declares nothing.
	for _, dir := range []string{e.code, filepath.Dir(e.code), plain} {
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
		check("with a .git at " + dir)
		if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
	}
	// Nor does a public root bound exactly at each folder.
	applyOSSRoots(t, &e, gitlab, filepath.Join(local, "docs"), symlinked, plain, lib)
	check("with a public root at each folder")

	// The folders' harness settings are still their root's, as before
	// SP-030; a repo GitHub does not confirm public keeps the rules of a
	// checkout with no space.
	for _, dir := range []string{gitlab, lib} {
		if space, placed, err := e.s.placeCheckout(ctx, dir); err != nil || !placed || space != "oss" {
			t.Fatalf("checkout settings for %s = %q %v, %v", dir, space, placed, err)
		}
	}
	if _, placed, err := e.s.placeCheckout(ctx, filepath.Join(e.code, "secret")); err != nil || placed {
		t.Fatalf("checkout settings for a private repo: placed %v, %v", placed, err)
	}
}

// TestLeavesPublicNeedsAPublicCopy: only a copy stored in a public space of
// an applied policy can leave it; with no policy nothing moves.
func TestLeavesPublicNeedsAPublicCopy(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	private := rag.Meta{Writer: rag.WriterDocs, Origin: rag.Origin{System: rag.OriginGitHub, RepoOwner: "oss-org", RepoName: "x", Visibility: "private"}}
	public := rag.Meta{Writer: rag.WriterDocs, Origin: rag.Origin{System: rag.OriginGitHub, RepoOwner: "oss-org", RepoName: "x", Visibility: "public"}}
	conflict := rag.Meta{Writer: rag.WriterSources, Origin: rag.Origin{System: rag.OriginFS, Path: filepath.Join(e.code, "company1", "x", "a.md")}}
	checkout(t, filepath.Join(e.code, "company1", "x"), "globex", "x") // acme root, globex remote
	for _, c := range []struct {
		name, stored string
		m            rag.Meta
		leaves       bool
	}{
		{"stored in public, private now", "oss", private, true},
		{"stored in public, still public", "oss", public, false},
		{"stored in public, a conflict now", "oss", conflict, true},
		{"stored outside public", "acme", private, false},
		{"stored in a space the policy lacks", "gone", private, false},
		{"unplaced", "", private, false},
	} {
		if got := e.placer().leavesPublic(ctx, c.stored, c.m); got != c.leaves {
			t.Errorf("%s: leaves = %v", c.name, got)
		}
	}
	none := spacesServer(t)
	if (&writePlacer{spaces: none.spaces}).leavesPublic(ctx, "oss", private) || (*writePlacer)(nil).leavesPublic(ctx, "oss", private) {
		t.Error("something left public with no policy")
	}
	if leavesPublic(nil) != nil || leavesPublic(e.placer()) == nil {
		t.Error("leavesPublic wiring")
	}
}

// TestLocalContentUnderBroadPublicRootNeverPublic is D-64's regression: local
// content under a public root the operator did not declare for it (a plain
// folder, a checkout of a verified public repo, and a folder a session
// registered as a source) is quarantined, while a page under a bound web
// prefix the sources refresh fetches is still public.
func TestLocalContentUnderBroadPublicRootNeverPublic(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	plain := filepath.Join(e.code, "notes")
	writeFile(t, filepath.Join(plain, "README.md"), "# notes")
	lib := filepath.Join(e.code, "lib") // oss-org/lib, verified public
	defaultCheckout(t, lib, "oss-org", "lib")
	writeFile(t, filepath.Join(lib, "README.md"), "# lib")
	// A job's session stages files in a new folder under the public root
	// and registers it as a source.
	staged := filepath.Join(e.code, "staged")
	writeFile(t, filepath.Join(staged, "SECRET.md"), "# acme internals")
	// Or stages them beside a .git naming another job's org, to move them
	// into that job's space.
	steered := filepath.Join(e.code, "steered")
	defaultCheckout(t, steered, "oss-org", "moved") // GitHub: acme/moved, private
	writeFile(t, filepath.Join(steered, "SECRET.md"), "# acme internals")
	steeredPrivate := filepath.Join(e.code, "willesq", "steered") // under the private root
	defaultCheckout(t, steeredPrivate, "oss-org", "moved")
	writeFile(t, filepath.Join(steeredPrivate, "SECRET.md"), "# acme internals")
	e.s.sourcesReg = sources.NewRegistry(mustDB(t))
	job := sessionCtx(ctx, sessionPin{SessionID: "s-acme", Space: "acme", Revision: e.rev})
	reg, err := e.s.SaveSource(job, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{Kind: "repo", Uri: staged, RefreshSecs: 3600}})
	if err != nil {
		t.Fatalf("a session registering a source: %v", err)
	}
	for _, dir := range []string{plain, lib, reg.GetUri(), steered, steeredPrivate} {
		if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindRepo, URI: dir}, e.svc,
			sources.RefreshOpts{LeavesPublic: e.placer().leavesPublic}); err != nil || st != "ok" {
			t.Fatalf("%s: refresh = %q, %v", dir, st, err)
		}
	}
	for _, f := range []string{filepath.Join(plain, "README.md"), filepath.Join(lib, "README.md"), filepath.Join(staged, "SECRET.md"),
		filepath.Join(steered, "SECRET.md"), filepath.Join(steeredPrivate, "SECRET.md")} {
		e.placedAs(t, sources.CheckoutURI(f), spaces.Quarantine, rag.OriginFS)
	}
	// A .git planted at the broad root, or above it, declares nothing.
	for _, dir := range []string{e.code, filepath.Dir(e.code)} {
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
		m := rag.Meta{Writer: rag.WriterSources, Origin: rag.Origin{System: rag.OriginFS, Path: filepath.Join(staged, "SECRET.md")}}
		if err := e.placer().place(ctx, &m); err != nil || m.Space != spaces.Quarantine {
			t.Fatalf("with a .git at %s: placed in %q, %v", dir, m.Space, err)
		}
		if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
	}

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a guide")) }))
	defer page.Close()
	applyOSSPrefix(t, &e, page.URL+"/")
	if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindWeb, URI: page.URL + "/guide"}, e.svc, sources.RefreshOpts{}); err != nil || st != "ok" {
		t.Fatalf("web refresh = %q, %v", st, err)
	}
	e.placedAs(t, page.URL+"/guide", "oss", rag.OriginWeb)

	// A registered page under the bound prefix that redirects off it is
	// placed by where it landed.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not public")) }))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/x", http.StatusFound)
	}))
	defer redirecting.Close()
	applyOSSPrefix(t, &e, redirecting.URL+"/")
	if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindWeb, URI: redirecting.URL + "/moved"}, e.svc, sources.RefreshOpts{}); err != nil || st != "ok" {
		t.Fatalf("redirected refresh = %q, %v", st, err)
	}
	if o := e.placedAs(t, redirecting.URL+"/moved", spaces.Quarantine, rag.OriginWeb); o["url"] != elsewhere.URL+"/x" {
		t.Fatalf("redirected page origin = %v", o)
	}
}
