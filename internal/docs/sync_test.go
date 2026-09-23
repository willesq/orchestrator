package docs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/rag"
)

// --- fakes -------------------------------------------------------------------

type fakeGH struct {
	repos []Repo
	err   error
}

func (f fakeGH) ListRepos(context.Context, string) ([]Repo, error) { return f.repos, f.err }

// fakeGit writes a predefined file layout per repo and counts clone calls.
type fakeGit struct {
	layout map[string]map[string]string // repo -> relpath -> content
	synced []string
	err    error
}

func (f *fakeGit) Sync(_ context.Context, _, repo, dest string) error {
	f.synced = append(f.synced, repo)
	if f.err != nil {
		return f.err
	}
	for rel, content := range f.layout[repo] {
		p := filepath.Join(dest, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(content), 0o644)
	}
	return nil
}

type fakeIng struct{ uris []string }

func (f *fakeIng) IngestText(_ context.Context, _ string, m rag.Meta) (int, error) {
	f.uris = append(f.uris, m.SourceURI)
	return 1, nil
}

func syncDeps(t *testing.T, gh GitHub, git Git) (*Registry, *fakeIng, SyncOpts) {
	t.Helper()
	reg := testRegistry(t)
	ing := &fakeIng{}
	opts := SyncOpts{RepoCache: t.TempDir(), GH: gh, Git: git, Now: func() int64 { return 42 }}
	return reg, ing, opts
}

// --- SyncOwner ---------------------------------------------------------------

func TestSyncOwner(t *testing.T) {
	tests := []struct {
		name          string
		repos         []Repo
		layout        map[string]map[string]string
		preSHA        map[string]string // repo -> stored head sha
		includeArch   bool
		includeForks  bool
		force         bool
		wantStatus    string
		wantIngested  []string
		wantSyncedNum int // number of git clones expected
	}{
		{
			name:          "new repo ingests markdown",
			repos:         []Repo{{Name: "docs", HeadSHA: "s1"}},
			layout:        map[string]map[string]string{"docs": {"README.md": "hello", "sub/x.md": "world", "code.go": "ignored"}},
			wantStatus:    "ok",
			wantIngested:  []string{"repo://acme/docs/README.md", "repo://acme/docs/sub/x.md"},
			wantSyncedNum: 1,
		},
		{
			name:          "unchanged repo (same HEAD sha) is skipped entirely",
			repos:         []Repo{{Name: "docs", HeadSHA: "s1"}},
			layout:        map[string]map[string]string{"docs": {"README.md": "hello"}},
			preSHA:        map[string]string{"docs": "s1"},
			wantStatus:    "ok",
			wantIngested:  nil,
			wantSyncedNum: 0,
		},
		{
			name:          "archived + fork excluded by default",
			repos:         []Repo{{Name: "arch", HeadSHA: "a", IsArchived: true}, {Name: "fork", HeadSHA: "f", IsFork: true}, {Name: "live", HeadSHA: "l"}},
			layout:        map[string]map[string]string{"live": {"README.md": "x"}},
			wantStatus:    "ok",
			wantIngested:  []string{"repo://acme/live/README.md"},
			wantSyncedNum: 1,
		},
		{
			name:          "include archived + forks when configured",
			repos:         []Repo{{Name: "arch", HeadSHA: "a", IsArchived: true}, {Name: "fork", HeadSHA: "f", IsFork: true}},
			layout:        map[string]map[string]string{"arch": {"A.md": "a"}, "fork": {"F.md": "f"}},
			includeArch:   true,
			includeForks:  true,
			wantStatus:    "ok",
			wantIngested:  []string{"repo://acme/arch/A.md", "repo://acme/fork/F.md"},
			wantSyncedNum: 2,
		},
		{
			name:          "force re-syncs even an unchanged repo",
			repos:         []Repo{{Name: "docs", HeadSHA: "s1"}},
			layout:        map[string]map[string]string{"docs": {"README.md": "hello"}},
			preSHA:        map[string]string{"docs": "s1"},
			force:         true,
			wantStatus:    "ok",
			wantIngested:  []string{"repo://acme/docs/README.md"},
			wantSyncedNum: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			git := &fakeGit{layout: tt.layout}
			reg, ing, opts := syncDeps(t, fakeGH{repos: tt.repos}, git)
			opts.IncludeArchived = tt.includeArch
			opts.IncludeForks = tt.includeForks
			opts.Force = tt.force
			ctx := context.Background()
			for repo, sha := range tt.preSHA {
				_ = reg.SetRepoState(ctx, RepoState{Owner: "acme", Repo: repo, HeadSHA: sha})
			}

			status, err := SyncOwner(ctx, reg, Owner{Owner: "acme"}, ing, opts)
			if err != nil {
				t.Fatalf("SyncOwner: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if len(git.synced) != tt.wantSyncedNum {
				t.Errorf("git clones = %d, want %d", len(git.synced), tt.wantSyncedNum)
			}
			sort.Strings(ing.uris)
			want := append([]string{}, tt.wantIngested...)
			sort.Strings(want)
			if strings.Join(ing.uris, ",") != strings.Join(want, ",") {
				t.Errorf("ingested = %v, want %v", ing.uris, want)
			}
		})
	}
}

func TestSyncOwnerEnumerateError(t *testing.T) {
	tests := []struct{ name string }{{name: "gh enumerate failure → status error, retain"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, ing, opts := syncDeps(t, fakeGH{err: errors.New("rate limited")}, &fakeGit{})
			status, err := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts)
			if err == nil || status != "error" {
				t.Fatalf("want (error, err), got (%q, %v)", status, err)
			}
		})
	}
}

func TestFileHashSkip(t *testing.T) {
	tests := []struct{ name string }{{name: "unchanged file skipped on a re-sync of a moved repo"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := map[string]map[string]string{"docs": {"README.md": "same", "CHANGED.md": "v1"}}
			git := &fakeGit{layout: layout}
			reg, ing, opts := syncDeps(t, fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s1"}}}, git)
			ctx := context.Background()

			if _, err := SyncOwner(ctx, reg, Owner{Owner: "acme"}, ing, opts); err != nil {
				t.Fatal(err)
			}
			if len(ing.uris) != 2 {
				t.Fatalf("first sync should ingest 2 files, got %v", ing.uris)
			}

			// Repo moves (new HEAD) but README.md content is unchanged; CHANGED.md differs.
			layout["docs"]["CHANGED.md"] = "v2"
			ing.uris = nil
			git2 := &fakeGit{layout: layout}
			opts.GH = fakeGH{repos: []Repo{{Name: "docs", HeadSHA: "s2"}}}
			opts.Git = git2
			if _, err := SyncOwner(ctx, reg, Owner{Owner: "acme"}, ing, opts); err != nil {
				t.Fatal(err)
			}
			if len(ing.uris) != 1 || !strings.HasSuffix(ing.uris[0], "/CHANGED.md") {
				t.Fatalf("second sync should ingest only the changed file, got %v", ing.uris)
			}
		})
	}
}

// --- parseGraphQLPage --------------------------------------------------------

func TestParseGraphQLPage(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantRepos []Repo
		wantNext  bool
		wantErr   bool
	}{
		{
			name: "nodes with and without default branch",
			body: `{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":"C1"},"nodes":[
				{"name":"a","isArchived":false,"isFork":false,"defaultBranchRef":{"target":{"oid":"sha-a"}}},
				{"name":"empty","isArchived":false,"isFork":false,"defaultBranchRef":null}]}}}}`,
			wantRepos: []Repo{{Name: "a", HeadSHA: "sha-a"}, {Name: "empty", HeadSHA: ""}},
			wantNext:  true,
		},
		{name: "invalid json errors", body: `{bad`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos, hasNext, cursor, err := parseGraphQLPage([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if hasNext != tt.wantNext || cursor != "C1" {
				t.Errorf("hasNext=%v cursor=%q", hasNext, cursor)
			}
			if len(repos) != len(tt.wantRepos) {
				t.Fatalf("repos = %+v", repos)
			}
			for i, r := range repos {
				if r != tt.wantRepos[i] {
					t.Errorf("repo[%d] = %+v, want %+v", i, r, tt.wantRepos[i])
				}
			}
		})
	}
}

// --- default gh/git impls via stubbed run seam -------------------------------

func TestGhGitHubListReposPaginates(t *testing.T) {
	tests := []struct{ name string }{{name: "follows endCursor across two pages"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page1 := `{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":"C1"},"nodes":[{"name":"r1","defaultBranchRef":{"target":{"oid":"s1"}}}]}}}}`
			page2 := `{"data":{"repositoryOwner":{"repositories":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"name":"r2","defaultBranchRef":{"target":{"oid":"s2"}}}]}}}}`
			var gotCursors []string
			gh := ghGitHub{run: func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "gh" {
					t.Errorf("want gh, got %s", name)
				}
				cur := ""
				for i, a := range args {
					if a == "cursor=C1" || strings.HasPrefix(a, "cursor=") {
						cur = args[i]
					}
				}
				gotCursors = append(gotCursors, cur)
				if cur == "" {
					return []byte(page1), nil
				}
				return []byte(page2), nil
			}}
			repos, err := gh.ListRepos(context.Background(), "acme")
			if err != nil {
				t.Fatalf("ListRepos: %v", err)
			}
			if len(repos) != 2 || repos[0].Name != "r1" || repos[1].Name != "r2" {
				t.Fatalf("repos = %+v", repos)
			}
			if len(gotCursors) != 2 {
				t.Fatalf("expected 2 pages, got %d", len(gotCursors))
			}
		})
	}
}

func TestGhGitHubListReposError(t *testing.T) {
	tests := []struct{ name string }{{name: "run error propagates"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := ghGitHub{run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("boom")
			}}
			if _, err := gh.ListRepos(context.Background(), "x"); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGhGitSync(t *testing.T) {
	tests := []struct {
		name    string
		runErr  error
		wantErr bool
	}{
		{name: "clones to dest", runErr: nil, wantErr: false},
		{name: "clone error propagates", runErr: errors.New("no auth"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgs []string
			g := ghGit{run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				gotArgs = args
				return nil, tt.runErr
			}}
			dest := filepath.Join(t.TempDir(), "acme", "docs")
			err := g.Sync(context.Background(), "acme", "docs", dest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				joined := strings.Join(gotArgs, " ")
				if !strings.Contains(joined, "repo clone acme/docs") || !strings.Contains(joined, "--depth 1") {
					t.Errorf("clone args = %q", joined)
				}
			}
		})
	}
}

func TestExecRun(t *testing.T) {
	tests := []struct{ name string }{{name: "captures stdout"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := execRun(context.Background(), "echo", "hi")
			if err != nil {
				t.Fatalf("execRun: %v", err)
			}
			if strings.TrimSpace(string(out)) != "hi" {
				t.Errorf("out = %q", out)
			}
		})
	}
}

func TestConstructorsNonNil(t *testing.T) {
	tests := []struct{ name string }{{name: "NewGitHub + NewGit return usable seams"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if NewGitHub() == nil || NewGit() == nil {
				t.Fatal("nil constructor result")
			}
		})
	}
}
