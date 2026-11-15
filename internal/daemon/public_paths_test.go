package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Explicit public paths (SP-076, D-65): oss lists folders under its public
// root, the whole code folder, whose local content may be public.

// TestPublicPathsDecideLocalContentUnderPublicRoot is SP-076.1.a3: under
// the public root, a plain folder, a checkout and a folder a session
// registered as a source are public only when a public_paths pattern
// matches them, through the sources refresh and as explained; everything
// else is quarantined. A .git at, above or inside the public root changes
// no placement. A matched checkout of a private repo, or one whose remote
// names a stricter space's org, stays out of public. A private or isolated
// root nested inside a '**' pattern keeps its own space.
func TestPublicPathsDecideLocalContentUnderPublicRoot(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	listed := filepath.Join(e.code, "oss")
	notes := filepath.Join(listed, "notes")
	writeFile(t, filepath.Join(notes, "README.md"), "# notes")
	lib := filepath.Join(listed, "lib") // oss-org/lib, verified public
	defaultCheckout(t, lib, "oss-org", "lib")
	writeFile(t, filepath.Join(lib, "docs", "guide.md"), "# lib")
	gitlab := filepath.Join(listed, "gl") // a checkout with no GitHub remote
	defaultCheckout(t, gitlab, "x", "y")
	writeFile(t, filepath.Join(gitlab, ".git", "config"), "[remote \"origin\"]\n\turl = git@gitlab.com:x/y.git\n"+
		"[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n")
	writeFile(t, filepath.Join(gitlab, "README.md"), "# gitlab")
	secret := filepath.Join(listed, "secret") // oss-org/secret, private
	defaultCheckout(t, secret, "oss-org", "secret")
	writeFile(t, filepath.Join(secret, "README.md"), "# secret")
	steered := filepath.Join(listed, "steered") // GitHub: acme/moved, private
	defaultCheckout(t, steered, "oss-org", "moved")
	writeFile(t, filepath.Join(steered, "README.md"), "# acme internals")
	unlisted := filepath.Join(e.code, "elsewhere")
	writeFile(t, filepath.Join(unlisted, "README.md"), "# elsewhere")
	staged := filepath.Join(listed, "staged") // a session registers it
	writeFile(t, filepath.Join(staged, "NOTES.md"), "# staged")

	applyOSS(t, &e, nil, nil, []string{listed + "/**"})
	e.s.sourcesReg = sources.NewRegistry(mustDB(t))
	job := sessionCtx(ctx, sessionPin{SessionID: "s-acme", Space: "acme", Revision: e.rev})
	reg, err := e.s.SaveSource(job, &orchdv1.SaveSourceRequest{Source: &orchdv1.RegisteredSource{Kind: "repo", Uri: staged, RefreshSecs: 3600}})
	if err != nil {
		t.Fatalf("a session registering a source: %v", err)
	}
	for _, dir := range []string{notes, lib, gitlab, secret, steered, unlisted, reg.GetUri()} {
		if st, err := sources.Refresh(ctx, sources.Source{Kind: sources.KindRepo, URI: dir}, e.svc,
			sources.RefreshOpts{LeavesPublic: e.placer().leavesPublic}); err != nil || st != "ok" {
			t.Fatalf("%s: refresh = %q, %v", dir, st, err)
		}
	}
	want := map[string]string{
		filepath.Join(notes, "README.md"):      "oss",
		filepath.Join(lib, "docs", "guide.md"): "oss",
		filepath.Join(gitlab, "README.md"):     "oss",
		filepath.Join(staged, "NOTES.md"):      "oss",
		filepath.Join(secret, "README.md"):     spaces.Quarantine,
		filepath.Join(steered, "README.md"):    spaces.Quarantine,
		filepath.Join(unlisted, "README.md"):   spaces.Quarantine,
	}
	for f, space := range want {
		e.placedAs(t, sources.CheckoutURI(f), space, rag.OriginFS)
	}
	check := func(when string) {
		t.Helper()
		for f, space := range want {
			m := rag.Meta{Writer: rag.WriterSources, Origin: rag.Origin{System: rag.OriginFS, Path: f}}
			if err := e.placer().place(ctx, &m); err != nil || m.Space != space {
				t.Errorf("%s: %s placed in %q, %v; want %s", when, f, m.Space, err, space)
			}
			x, err := e.s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: f})
			if err != nil || x.GetSpace() != space {
				t.Errorf("%s: %s explained as %q, %v; want %s", when, f, x.GetSpace(), err, space)
			}
		}
	}
	check("as found")
	for _, dir := range []string{e.code, filepath.Dir(e.code), listed, unlisted} {
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
		check("with a .git at " + dir)
		if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
	}

	// A '**' over the whole public root: the stricter roots inside it keep
	// their own spaces.
	applyOSS(t, &e, nil, nil, []string{e.code + "/**"})
	for f, space := range map[string]string{
		filepath.Join(e.code, "willesq", "x", "a.md"):  "personal",
		filepath.Join(e.code, "company1", "y", "a.md"): "acme",
		filepath.Join(unlisted, "README.md"):           "oss",
	} {
		m := rag.Meta{Writer: rag.WriterSources, Origin: rag.Origin{System: rag.OriginFS, Path: f}}
		if err := e.placer().place(ctx, &m); err != nil || m.Space != space {
			t.Errorf("under %s/**: %s placed in %q, %v; want %s", e.code, f, m.Space, err, space)
		}
	}

	// A listed folder's harness settings are its root's.
	if space, placed, err := e.s.placeCheckout(ctx, lib); err != nil || !placed || space != "oss" {
		t.Fatalf("checkout settings for %s = %q %v, %v", lib, space, placed, err)
	}
	// Edit hand-offs never reach public, listed or not.
	m := rag.Meta{Writer: rag.WriterFile, Origin: rag.Origin{System: rag.OriginFS, Path: filepath.Join(notes, "README.md")}}
	if err := e.placer().place(ctx, &m); err != nil || m.Space != "personal" {
		t.Errorf("a hand-off in a listed folder placed in %q, %v", m.Space, err)
	}
}

// TestExplainNamesThePublicPath is SP-076.1.a4: 'orch spaces why' (alias
// explain) names the pattern that made a path public, or says none matched.
// A folder is explained for the files directly in it. An agent sees the
// public answer but not the pattern.
func TestExplainNamesThePublicPath(t *testing.T) {
	e := newWritesEnv(t, orgNodes{})
	listed := filepath.Join(e.code, "oss")
	writeFile(t, filepath.Join(listed, "README.md"), "# oss")
	writeFile(t, filepath.Join(listed, "deeper", "a.md"), "# deeper")
	pattern := listed
	applyOSS(t, &e, nil, nil, []string{pattern})

	for _, c := range []struct {
		target, rule, space, says string
	}{
		{filepath.Join(listed, "README.md"), "public_path", "oss", `matches it (public_paths pattern "` + pattern + `")`},
		{listed, "public_path", "oss", `(public_paths pattern "` + pattern + `")`},
		{filepath.Join(listed, "deeper", "a.md"), "no_public_proof", "quarantine", "no public_paths pattern of public space oss matches it"},
		{filepath.Join(listed, "deeper"), "no_public_proof", "quarantine", "no public_paths pattern of public space oss matches it"},
	} {
		x, err := e.s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: c.target})
		if err != nil {
			t.Fatalf("%s: %v", c.target, err)
		}
		if x.GetRule() != c.rule || x.GetSpace() != c.space || !strings.Contains(x.GetReason(), c.says) {
			t.Errorf("%s = %s %s %q, want %s %s with %q", c.target, x.GetRule(), x.GetSpace(), x.GetReason(), c.rule, c.space, c.says)
		}
	}
	agent := callerCtx(callerAuth{pid: 7, harness: "claude"})
	x, err := e.s.ExplainPlacement(agent, &orchdv1.ExplainPlacementRequest{Target: filepath.Join(listed, "README.md")})
	if err != nil || x.GetRule() != "public_path" || strings.Contains(x.GetReason(), pattern) {
		t.Errorf("agent explanation = %v, %v", x, err)
	}
}

// TestListedFolderKeepsPublicCopyWithoutGitHubAnswer is RC-402 for SP-076:
// a GitHub outage or a spent lookup budget does not take a listed folder's
// public copy out of public, while GitHub answering that the repo is
// private, or refusing to show it, does. New writes stay out of public until
// GitHub answers.
func TestListedFolderKeepsPublicCopyWithoutGitHubAnswer(t *testing.T) {
	ctx := context.Background()
	e := newWritesEnv(t, orgNodes{})
	listed := filepath.Join(e.code, "oss")
	lib := filepath.Join(listed, "lib")
	defaultCheckout(t, lib, "oss-org", "lib")
	secret := filepath.Join(listed, "secret")
	defaultCheckout(t, secret, "oss-org", "secret")
	gone := filepath.Join(listed, "gone")
	defaultCheckout(t, gone, "oss-org", "gone") // GitHub answers 404
	applyOSS(t, &e, nil, nil, []string{listed + "/**"})
	meta := func(dir string) rag.Meta {
		return rag.Meta{Writer: rag.WriterSources, Origin: rag.Origin{System: rag.OriginFS, Path: filepath.Join(dir, "README.md")}}
	}
	if e.placer().leavesPublic(ctx, "oss", meta(lib)) {
		t.Fatal("a verified public listed checkout left public")
	}
	for _, dir := range []string{secret, gone} {
		if !e.placer().leavesPublic(ctx, "oss", meta(dir)) {
			t.Errorf("%s stayed public after GitHub answered", dir)
		}
	}

	// GitHub stops answering once its answers have expired from the cache.
	clock := time.Now()
	e.s.repos.now = func() time.Time { return clock }
	clock = clock.Add(repoLookupTTL + time.Minute)
	outage := 0
	e.s.repos.gh = func(context.Context, ...string) ([]byte, error) {
		outage++
		return nil, errors.New("dial tcp: connection refused")
	}
	if e.placer().leavesPublic(ctx, "oss", meta(lib)) || outage == 0 {
		t.Errorf("an outage took a listed folder out of public (%d calls)", outage)
	}
	m := meta(lib)
	if err := e.placer().place(ctx, &m); err != nil || m.Space != spaces.Quarantine {
		t.Errorf("a write during an outage placed in %q, %v", m.Space, err)
	}
	// A repo GitHub never said was public gets no grace.
	if !e.placer().leavesPublic(ctx, "oss", meta(secret)) {
		t.Error("an outage kept a repo GitHub had said is private in public")
	}
	// Outside every pattern an unanswered repo still leaves.
	applyOSS(t, &e, nil, nil, []string{filepath.Join(e.code, "other")})
	if !e.placer().leavesPublic(ctx, "oss", meta(lib)) {
		t.Error("an unlisted checkout stayed public during an outage")
	}

	applyOSS(t, &e, nil, nil, []string{listed + "/**"})
	e.s.repos.spent, e.s.repos.window = repoLookupsPerMin, clock
	if e.placer().leavesPublic(ctx, "oss", meta(lib)) {
		t.Error("a spent lookup budget took a listed folder out of public")
	}
	// The grace ends unansweredGrace after GitHub last said it is public,
	// so a caller that keeps the budget spent cannot keep it public.
	clock = clock.Add(unansweredGrace)
	e.s.repos.spent, e.s.repos.window = repoLookupsPerMin, clock
	if !e.placer().leavesPublic(ctx, "oss", meta(lib)) {
		t.Error("a spent lookup budget kept a listed folder public past the grace")
	}
}
