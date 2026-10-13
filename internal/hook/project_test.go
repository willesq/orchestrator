package hook

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// isolateGit keeps the developer's global git config (signing, hooks, templates)
// out of fixture repositories.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepo creates <tmp>/<name> as a git repository with one commit and, when
// remote is non-empty, an origin pointing at it. It returns the directory with
// symlinks resolved, as git reports it (macOS temp dirs live under a symlink).
func newRepo(t *testing.T, name, remote string) string {
	t.Helper()
	isolateGit(t)
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	if remote != "" {
		runGit(t, dir, "remote", "add", "origin", remote)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestRepoNameFromRemote(t *testing.T) {
	tests := []struct{ url, want string }{
		{"git@github.com:willesq/orchestrator.git", "orchestrator"},
		{"https://github.com/willesq/orchestrator.git", "orchestrator"},
		{"https://github.com/willesq/orchestrator", "orchestrator"},
		{"https://github.com/willesq/orchestrator/", "orchestrator"},
		{"ssh://git@host:2222/team/widget.git\n", "widget"},
		{"/srv/git/widget.git", "widget"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := repoNameFromRemote(tt.url); got != tt.want {
				t.Errorf("repoNameFromRemote(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestDetectProject(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (cwd string, want project)
	}{
		{name: "empty cwd", setup: func(*testing.T) (string, project) { return "", project{} }},
		{name: "outside a repository", setup: func(t *testing.T) (string, project) {
			isolateGit(t)
			return t.TempDir(), project{}
		}},
		{name: "nonexistent directory", setup: func(t *testing.T) (string, project) {
			return filepath.Join(t.TempDir(), "gone"), project{}
		}},
		{name: "origin remote names the repo", setup: func(t *testing.T) (string, project) {
			dir := newRepo(t, "local-folder", "git@github.com:acme/widget.git")
			return dir, project{name: "widget", checkout: dir}
		}},
		{name: "subdirectory resolves to its repo", setup: func(t *testing.T) (string, project) {
			dir := newRepo(t, "folder", "https://github.com/acme/widget.git")
			sub := filepath.Join(dir, "internal", "pkg")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			return sub, project{name: "widget", checkout: dir}
		}},
		{name: "no remote falls back to checkout name", setup: func(t *testing.T) (string, project) {
			dir := newRepo(t, "gadget", "")
			return dir, project{name: "gadget", checkout: dir}
		}},
		{name: "linked worktree resolves to its main checkout", setup: func(t *testing.T) (string, project) {
			main := newRepo(t, "gadget", "")
			wt := filepath.Join(t.TempDir(), "gadget.worktrees", "feat", "x")
			runGit(t, main, "worktree", "add", "-q", "-b", "feat/x", wt)
			return wt, project{name: "gadget", checkout: main}
		}},
		{name: "inherited GIT_DIR does not redirect the probe", setup: func(t *testing.T) (string, project) {
			other := newRepo(t, "other", "git@github.com:acme/other.git")
			dir := newRepo(t, "gadget", "")
			t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
			return dir, project{name: "gadget", checkout: dir}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd, want := tt.setup(t)
			if got := detectProject(cwd); got != want {
				t.Errorf("detectProject = %+v, want %+v", got, want)
			}
		})
	}
}

func TestProjectTags(t *testing.T) {
	tests := []struct {
		name        string
		p           project
		wantSearch  []string
		wantSession []string
	}{
		{name: "zero project", p: project{}, wantSearch: nil, wantSession: nil},
		{name: "repo", p: project{name: "widget", checkout: "/src/widget"},
			wantSearch:  []string{"repo:widget", "checkout:/src/widget"},
			wantSession: []string{"checkout:/src/widget"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.searchTags(); !slices.Equal(got, tt.wantSearch) {
				t.Errorf("searchTags = %v, want %v", got, tt.wantSearch)
			}
			if got := tt.p.sessionTags(); !slices.Equal(got, tt.wantSession) {
				t.Errorf("sessionTags = %v, want %v", got, tt.wantSession)
			}
		})
	}
}

func hit(uri, text string, score float32) *orchdv1.SearchHit {
	return &orchdv1.SearchHit{SourceUri: uri, Text: text, Score: score}
}

func hits(h ...*orchdv1.SearchHit) *orchdv1.SearchResponse {
	return &orchdv1.SearchResponse{Hits: h}
}

func uris(hs []*orchdv1.SearchHit) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.GetSourceUri())
	}
	return out
}

func TestProjectFirstHits(t *testing.T) {
	proj := func(n string) *orchdv1.SearchHit { return hit("repo://acme/widget/"+n, n, 0.6) }
	glob := func(n string) *orchdv1.SearchHit { return hit("repo://other/"+n, n, 0.9) }
	scoped := []string{"repo:widget", "checkout:/src/widget"}
	tests := []struct {
		name         string
		tags         []string
		topK         int
		client       *fakeClient
		want         []string
		wantSearches int
	}{
		{
			name: "project hits fill every slot, no global search",
			tags: scoped, topK: 2,
			client:       &fakeClient{projectSearch: hits(proj("a"), proj("b")), search: hits(glob("g"))},
			want:         []string{"repo://acme/widget/a", "repo://acme/widget/b"},
			wantSearches: 1,
		},
		{
			name: "project hits rank ahead of stronger global hits",
			tags: scoped, topK: 3,
			client:       &fakeClient{projectSearch: hits(proj("a")), search: hits(glob("g1"), glob("g2"), glob("g3"))},
			want:         []string{"repo://acme/widget/a", "repo://other/g1", "repo://other/g2"},
			wantSearches: 2,
		},
		{
			name: "duplicates across passes are kept once",
			tags: scoped, topK: 3,
			client:       &fakeClient{projectSearch: hits(proj("a")), search: hits(proj("a"), glob("g"))},
			want:         []string{"repo://acme/widget/a", "repo://other/g"},
			wantSearches: 2,
		},
		{
			name: "weak project hits are filtered like global ones",
			tags: scoped, topK: 2,
			client:       &fakeClient{projectSearch: hits(hit("repo://acme/widget/weak", "w", 0.1)), search: hits(glob("g"))},
			want:         []string{"repo://other/g"},
			wantSearches: 2,
		},
		{
			name: "failed project search falls back to global",
			tags: scoped, topK: 2,
			client:       &fakeClient{projectErr: errors.New("boom"), search: hits(glob("g"))},
			want:         []string{"repo://other/g"},
			wantSearches: 2,
		},
		{
			name: "outside a project only the global search runs",
			tags: nil, topK: 2,
			client:       &fakeClient{search: hits(glob("g"))},
			want:         []string{"repo://other/g"},
			wantSearches: 1,
		},
		{
			name: "unset top_k uses the daemon default",
			tags: nil, topK: 0,
			client:       &fakeClient{search: hits(glob("1"), glob("2"), glob("3"), glob("4"), glob("5"), glob("6"))},
			want:         []string{"repo://other/1", "repo://other/2", "repo://other/3", "repo://other/4", "repo://other/5"},
			wantSearches: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultAutoRAG()
			cfg.TopK = tt.topK
			got := projectFirstHits(context.Background(), tt.client, "q", tt.tags, cfg, nil)
			if !slices.Equal(uris(got), tt.want) {
				t.Errorf("hits = %v, want %v", uris(got), tt.want)
			}
			if len(tt.client.searchReqs) != tt.wantSearches {
				t.Fatalf("searches = %d, want %d", len(tt.client.searchReqs), tt.wantSearches)
			}
			if len(tt.tags) > 0 && !slices.Equal(tt.client.searchReqs[0].GetTags(), tt.tags) {
				t.Errorf("first search tags = %v, want %v", tt.client.searchReqs[0].GetTags(), tt.tags)
			}
			if last := tt.client.searchReqs[len(tt.client.searchReqs)-1]; tt.wantSearches == 2 && len(last.GetTags()) != 0 {
				t.Errorf("fallback search must be unscoped, got tags %v", last.GetTags())
			}
		})
	}
}

func TestUserPromptSubmitScopesToCwdProject(t *testing.T) {
	dir := newRepo(t, "folder", "git@github.com:acme/widget.git")
	fc := &fakeClient{projectSearch: hits(hit("repo://acme/widget/README.md", "widget notes", 0.7)), search: hitsResp(0.9)}
	var out bytes.Buffer
	p := Payload{Prompt: "how do we deploy the widget?", Cwd: dir}
	if err := userPromptSubmit(defaultAutoRAG(), p, &out, factory(fc, nil)); err != nil {
		t.Fatalf("handler must never error: %v", err)
	}
	want := []string{"repo:widget", "checkout:" + dir}
	if len(fc.searchReqs) == 0 || !slices.Equal(fc.searchReqs[0].GetTags(), want) {
		t.Fatalf("first search not scoped to %v: %v", want, fc.searchReqs)
	}
	if !bytes.Contains(out.Bytes(), []byte("### [1] repo://acme/widget/README.md")) {
		t.Errorf("project hit not injected first:\n%s", out.String())
	}
}

func TestSessionEndTagsCheckoutOnly(t *testing.T) {
	tests := []struct {
		name string
		cwd  func(t *testing.T) (cwd string, wantTags []string)
	}{
		{name: "inside a repo", cwd: func(t *testing.T) (string, []string) {
			dir := newRepo(t, "folder", "git@github.com:acme/widget.git")
			return dir, []string{"checkout:" + dir}
		}},
		{name: "origin claiming another project's name earns no repo tag", cwd: func(t *testing.T) (string, []string) {
			dir := newRepo(t, "untrusted", "git@github.com:willesq/orchestrator.git")
			return dir, []string{"checkout:" + dir}
		}},
		{name: "outside a repo", cwd: func(t *testing.T) (string, []string) { isolateGit(t); return t.TempDir(), nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			cwd, wantTags := tt.cwd(t)
			fc := &fakeClient{}
			p := Payload{SessionID: "sid", Cwd: cwd, TranscriptPath: writeSampleTranscript(t)}
			if err := sessionEnd(p, factory(fc, nil)); err != nil {
				t.Fatalf("sessionEnd: %v", err)
			}
			if len(fc.ingested) != 2 {
				t.Fatalf("ingests = %d, want 2", len(fc.ingested))
			}
			for _, req := range fc.ingested {
				if !slices.Equal(req.GetTags(), wantTags) {
					t.Errorf("%s tags = %v, want %v", req.GetSourceUri(), req.GetTags(), wantTags)
				}
			}
		})
	}
}
