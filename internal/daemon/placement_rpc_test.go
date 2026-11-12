package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/orchdclient"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// fakeRepos answers gh repo lookups: "owner/name" -> current owner/name and
// visibility. Unknown repos fail, as GitHub's 404 does.
type fakeRepos struct {
	mu    sync.Mutex
	repos map[string][3]string // owner, name, "private"|"public"
	calls int
}

func (f *fakeRepos) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	key := strings.TrimPrefix(args[1], "repos/")
	r, ok := f.repos[key]
	if !ok {
		return nil, errors.New("gh: Not Found (HTTP 404)")
	}
	return json.Marshal(map[string]any{"owner": r[0], "name": r[1], "private": r[2] == "private"})
}

// checkout makes a git checkout at dir whose origin is github.com/owner/name.
func checkout(t *testing.T, dir, owner, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[remote \"origin\"]\n\turl = git@github.com:%s/%s.git\n", owner, name)
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// placementServer applies the plan's example layout under a temp "code"
// folder and returns the server and that folder.
func placementServer(t *testing.T) (*server, string, *fakeRepos) {
	t.Helper()
	return placementServerWith(t, orgNodes{})
}

// placementServerWith is placementServer with GitHub org identities from
// nodes.
func placementServerWith(t *testing.T, nodes orgNodes) (*server, string, *fakeRepos) {
	t.Helper()
	s := spacesServerWith(t, nodes)
	code, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := spaces.Policy{
		Spaces: []spaces.Space{
			{Name: "oss", Kind: spaces.Public, GitHubOrgs: []string{"oss-org"}, Roots: []string{code}, WebPrefixes: []string{"https://docs.oss.dev/"}},
			{Name: "personal", Kind: spaces.Private, GitHubOrgs: []string{"willesq"}, Roots: []string{code + "/willesq"}},
			{Name: "acme", Kind: spaces.Isolated, GitHubOrgs: []string{"acme"}, Roots: []string{code + "/company1"}, Chat: []string{"slack:TACME"}},
			{Name: "globex", Kind: spaces.Isolated, GitHubOrgs: []string{"globex"}, Chat: []string{"teams:globex-tenant"}},
		},
		PublicRootSessions: "personal",
	}
	doc, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	d := draft(t, s, string(doc))
	if len(d.GetProblems()) > 0 {
		t.Fatalf("policy problems: %v", d.GetProblems())
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeRepos{repos: map[string][3]string{
		"acme/app":       {"acme", "app", "private"},
		"acme/fork":      {"acme", "fork", "public"},
		"oss-org/lib":    {"oss-org", "lib", "public"},
		"oss-org/secret": {"oss-org", "secret", "private"},
		"oss-org/moved":  {"acme", "moved", "private"}, // transferred to acme
		"globex/x":       {"globex", "x", "private"},
		"willesq/notes":  {"willesq", "notes", "private"},
		"stranger/pub":   {"stranger", "pub", "public"},
	}}
	s.repos = newRepoLookups(gh.run)
	checkout(t, code+"/company1/app", "acme", "app")
	checkout(t, code+"/company1/fork", "acme", "fork")
	checkout(t, code+"/lib", "oss-org", "lib")
	checkout(t, code+"/secret", "oss-org", "secret")
	checkout(t, code+"/moved", "oss-org", "moved")
	checkout(t, code+"/willesq/app", "acme", "app")
	checkout(t, code+"/company1/lib", "oss-org", "lib")
	checkout(t, code+"/company1/x", "globex", "x")
	checkout(t, code+"/probe-unbound", "stranger", "pub")
	checkout(t, code+"/probe-bound", "acme", "app")
	return s, code, gh
}

// TestExplainPlacementCases is SP-022.2.a1: for every case of SP-022.1 that
// can be named as a target, the answer lists the matching roots shallowest
// first, the remote's effect, the deciding rule and the result.
func TestExplainPlacementCases(t *testing.T) {
	s, code, _ := placementServer(t)
	cases := []struct {
		name, target, kind string
		roots              []string
		remote             string // owner/repo (visibility) -> space, "" for none
		rule, space        string
		session            [2]string // session_space, session_reads for paths
	}{
		{"repo in isolated org", "acme/app", "repo", nil, "acme/app (private) -> acme", "remote", "acme", [2]string{}},
		{"fork owned by job org with public parent", code + "/company1/fork", "path", []string{"oss", "acme"}, "acme/fork (public) -> acme", "root_and_remote_agree", "acme", [2]string{"acme", "acme"}},
		{"public repo in public org", code + "/lib", "path", []string{"oss"}, "oss-org/lib (public) -> oss", "root_and_remote_agree", "oss", [2]string{"personal", "oss"}},
		{"private repo in public org", code + "/secret", "path", []string{"oss"}, "oss-org/secret (private) -> oss", "not_verified_public", "quarantine", [2]string{"quarantine", ""}},
		{"repo transferred between orgs", code + "/moved", "path", []string{"oss"}, "acme/moved (private) -> acme", "remote_stricter", "acme", [2]string{"acme", "acme"}},
		{"root and remote agree", code + "/company1/app/main.go", "path", []string{"oss", "acme"}, "acme/app (private) -> acme", "root_and_remote_agree", "acme", [2]string{"acme", "acme"}},
		{"remote stricter than root", code + "/willesq/app", "path", []string{"oss", "personal"}, "acme/app (private) -> acme", "remote_stricter", "acme", [2]string{"acme", "acme"}},
		{"root stricter than remote", code + "/company1/lib", "path", []string{"oss", "acme"}, "oss-org/lib (public) -> oss", "root_stricter", "acme", [2]string{"acme", "acme"}},
		{"two isolated spaces", code + "/company1/x", "path", []string{"oss", "acme"}, "globex/x (private) -> globex", "conflict", "quarantine", [2]string{"quarantine", ""}},
		{"nested: public root", code + "/tools", "path", []string{"oss"}, "", "root", "oss", [2]string{"personal", "oss"}},
		{"nested: private root, deeper", code + "/willesq/deep/x", "path", []string{"oss", "personal"}, "", "root", "personal", [2]string{"personal", "personal"}},
		{"nested: isolated root, deeper", code + "/company1/svc/x", "path", []string{"oss", "acme"}, "", "root", "acme", [2]string{"acme", "acme"}},
		{"remote only, no root", "willesq/notes", "repo", nil, "willesq/notes (private) -> personal", "remote", "personal", [2]string{}},
		{"no root and no remote", "/nonexistent-root/x", "path", nil, "", "unmatched", "quarantine", [2]string{"quarantine", ""}},
		{"allowlisted web prefix", "https://docs.oss.dev/guide", "url", nil, "", "web_prefix", "oss", [2]string{}},
		{"unbound web page", "https://example.com/x", "url", nil, "", "unmatched", "quarantine", [2]string{}},
		{"Slack tenant", "slack://TACME/C1/123", "chat", nil, "", "chat_tenant", "acme", [2]string{}},
		{"Teams tenant", "teams:globex-tenant", "chat", nil, "", "chat_tenant", "globex", [2]string{}},
		{"unmatched repo", "stranger/x", "repo", nil, "stranger/x () -> ", "unmatched", "quarantine", [2]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: c.target})
			if err != nil {
				t.Fatal(err)
			}
			if e.GetTargetKind() != c.kind || e.GetRule() != c.rule || e.GetSpace() != c.space || e.GetReason() == "" {
				t.Fatalf("got %s %s -> %s (%s)", e.GetTargetKind(), e.GetRule(), e.GetSpace(), e.GetReason())
			}
			var roots []string
			for _, r := range e.GetRoots() {
				roots = append(roots, r.GetSpace())
			}
			if !slices.Equal(roots, c.roots) {
				t.Errorf("roots %v, want %v", roots, c.roots)
			}
			remote := ""
			if r := e.GetRemote(); r != nil {
				remote = fmt.Sprintf("%s/%s (%s) -> %s", r.GetOwner(), r.GetRepo(), r.GetVisibility(), r.GetSpace())
				if r.GetEffect() == "" {
					t.Error("remote has no effect text")
				}
			}
			if remote != c.remote {
				t.Errorf("remote %q, want %q", remote, c.remote)
			}
			if got := [2]string{e.GetSessionSpace(), e.GetSessionReads()}; got != c.session {
				t.Errorf("session %v, want %v", got, c.session)
			}
		})
	}
}

// TestExplainPlacementAgents is SP-022.2.a1's limit: agents get an answer
// only when it names nothing but public spaces, and never a session's.
func TestExplainPlacementAgents(t *testing.T) {
	s, code, _ := placementServer(t)
	harness := callerCtx(callerAuth{pid: 7, harness: "claude"})
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	spawned := callerCtx(callerAuth{pid: 8, spawned: true})
	for _, ctx := range []context.Context{harness, queued, spawned, context.Background()} {
		e, err := s.ExplainPlacement(ctx, &orchdv1.ExplainPlacementRequest{Target: code + "/lib"})
		if err != nil || e.GetSpace() != "oss" || e.GetSessionSpace() != "" {
			t.Fatalf("public path: %v, %v", e, err)
		}
		for _, target := range []string{
			code + "/company1/app", code + "/willesq/x", code + "/secret", code + "/company1/x",
			"/nonexistent-root/x", "acme/app", "slack:TACME", "https://example.com/",
			// A planted .git under a public root must not tell an unbound
			// org from one bound to a job.
			code + "/probe-unbound", code + "/probe-bound", "stranger/pub",
		} {
			_, err := s.ExplainPlacement(ctx, &orchdv1.ExplainPlacementRequest{Target: target})
			if grpcstatus.Code(err) != codes.PermissionDenied || err.Error() != errPlacementHidden.Error() {
				t.Errorf("%s: %v", target, err)
			}
		}
	}
	if !taskCallable[orchdv1.Orchd_ExplainPlacement_FullMethodName] {
		t.Error("queued tasks are refused before the handler can answer public targets")
	}
}

func TestExplainPlacementNoPolicy(t *testing.T) {
	s := spacesServer(t)
	harness := callerCtx(callerAuth{pid: 7, harness: "codex"})
	e, err := s.ExplainPlacement(harness, &orchdv1.ExplainPlacementRequest{Target: "acme/app"})
	if err != nil || e.GetSpace() != spaces.Shared || e.GetRule() != "no_policy" {
		t.Fatalf("got %v, %v", e, err)
	}
	e, err = s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: "/tmp"})
	if err != nil || e.GetSessionSpace() != spaces.Shared || e.GetSessionReads() != spaces.Shared {
		t.Fatalf("session: %v, %v", e, err)
	}
}

func TestExplainPlacementBadTargets(t *testing.T) {
	s := spacesServer(t)
	for _, target := range []string{"", "  ", "relative/dir/x", "slack:", "slack://", "teams:/x", "ftp://x", "not a target", strings.Repeat("/a", 3000)} {
		_, err := s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: target})
		if grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v", target, err)
		}
	}
	if _, err := (&server{}).ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: "/x"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("no engine: %v", err)
	}
}

func TestRepoLookups(t *testing.T) {
	gh := &fakeRepos{repos: map[string][3]string{"acme/app": {"acme", "app", "public"}}}
	l := newRepoLookups(gh.run)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	ctx := context.Background()
	if r := l.lookup(ctx, "acme", "app"); r.Visibility != spaces.VisibilityPublic {
		t.Fatalf("got %+v", r)
	}
	if l.lookup(ctx, "ACME", "App"); gh.calls != 1 {
		t.Fatalf("cache missed: %d calls", gh.calls)
	}
	if r := l.lookup(ctx, "acme", "gone"); r.Visibility != spaces.VisibilityUnknown || r.Owner != "acme" {
		t.Fatalf("failed lookup: %+v", r)
	}
	now = now.Add(repoLookupTTL)
	l.lookup(ctx, "acme", "app")
	if gh.calls != 3 {
		t.Fatalf("expired entry not refreshed: %d calls", gh.calls)
	}
	now = now.Add(time.Minute)
	gh.calls = 0
	for i := 0; i < repoLookupsPerMin+5; i++ {
		l.lookup(ctx, "acme", fmt.Sprintf("r%d", i))
	}
	if gh.calls != repoLookupsPerMin {
		t.Fatalf("rate limit: %d calls in a minute", gh.calls)
	}
	now = now.Add(time.Minute)
	l.lookup(ctx, "acme", "fresh")
	if gh.calls != repoLookupsPerMin+1 {
		t.Fatalf("limit did not reset: %d calls", gh.calls)
	}
	for i := 0; i < repoLookupEntries+1; i++ {
		l.entries[fmt.Sprint(i)] = repoEntry{at: now}
	}
	gh.repos["acme/last"] = [3]string{"acme", "last", "private"}
	now = now.Add(time.Minute)
	if r := l.lookup(ctx, "acme", "last"); r.Visibility != spaces.VisibilityPrivate || len(l.entries) != 1 {
		t.Fatalf("full cache not cleared: %d entries, %+v", len(l.entries), r)
	}
	before, spent := gh.calls, l.spent
	if r := l.lookup(ctx, "-bad-", "x"); r.Visibility != spaces.VisibilityUnknown || gh.calls != before || l.spent != spent {
		t.Fatalf("invalid owner spent the budget: %+v, %d calls, %d spent", r, gh.calls-before, l.spent-spent)
	}
	var none *repoLookups
	if r := none.lookup(ctx, "a", "b"); r.Owner != "a" || r.Visibility != spaces.VisibilityUnknown {
		t.Fatalf("nil lookups: %+v", r)
	}
}

// TestExplainPlacementProbe: the operator sees an unbound public repo under a
// public root placed in public; an agent is refused it exactly as it is
// refused a repo of a job's org.
func TestExplainPlacementProbe(t *testing.T) {
	s, code, _ := placementServer(t)
	e, err := s.ExplainPlacement(operatorCtx, &orchdv1.ExplainPlacementRequest{Target: code + "/probe-unbound"})
	if err != nil || e.GetSpace() != "oss" || e.GetRule() != "root" {
		t.Fatalf("operator: %v, %v", e, err)
	}
}

// TestExplainPlacementErrorsHidden: an error placing a target reaches the
// operator, while an agent gets the same refusal as for any hidden target.
func TestExplainPlacementErrorsHidden(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	e, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }, LookupOrg: orgNodes{}.lookup})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{spaces: e}
	d := draft(t, s, spacesDoc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	req := &orchdv1.ExplainPlacementRequest{Target: "acme/app"}
	if _, err := s.ExplainPlacement(callerCtx(callerAuth{pid: 7, harness: "claude"}), req); err == nil || err.Error() != errPlacementHidden.Error() {
		t.Errorf("agent: %v", err)
	}
	if _, err := s.ExplainPlacement(operatorCtx, req); grpcstatus.Code(err) != codes.Internal {
		t.Errorf("operator: %v", err)
	}
}
