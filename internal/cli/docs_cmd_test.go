package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// fakeGH / fakeGit stand in for the gh + git CLIs so a foreground sync is
// exercised end to end without network access.
type fakeGH struct {
	repos []docs.Repo
	err   error
}

func (f fakeGH) ListRepos(context.Context, string) ([]docs.Repo, error) { return f.repos, f.err }

type fakeGit struct {
	files map[string]string // relative path → contents, written into every dest
	err   error
}

func (f fakeGit) Sync(_ context.Context, _, _, dest string) error {
	if f.err != nil {
		return f.err
	}
	for rel, body := range f.files {
		p := filepath.Join(dest, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// stubDocsTooling points the foreground sync at the fakes for one test.
func stubDocsTooling(t *testing.T, gh docs.GitHub, g docs.Git) {
	t.Helper()
	origGH, origGit := newDocsGitHub, newDocsGit
	newDocsGitHub = func() docs.GitHub { return gh }
	newDocsGit = func() docs.Git { return g }
	t.Cleanup(func() { newDocsGitHub, newDocsGit = origGH, origGit })
}

func TestDocsCLIFlow(t *testing.T) {
	tests := []struct{ name string }{{name: "add, list, status, sync, refresh, remove"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			f := factory(&fakeClient{}, nil)
			mustRun := func(args ...string) string {
				out, err := run(f, "", args...)
				if err != nil {
					t.Fatalf("%v: %v", args, err)
				}
				return out
			}
			if out := mustRun("docs", "add", "willesq", "--refresh", "6h", "--tag", "x"); !strings.Contains(out, "added docs owner willesq") {
				t.Errorf("add: %q", out)
			}
			if out := mustRun("docs", "owners", "list"); !strings.Contains(out, "willesq") {
				t.Errorf("owners list: %q", out)
			}
			if out := mustRun("docs", "list"); !strings.Contains(out, "no docs ingested") {
				t.Errorf("list before sync: %q", out)
			}
			if out := mustRun("docs", "status"); !strings.Contains(out, "willesq") || !strings.Contains(out, "last=never") {
				t.Errorf("status: %q", out)
			}
			if out := mustRun("docs", "sync", "willesq", "--background"); !strings.Contains(out, "marked for sync") {
				t.Errorf("sync --background: %q", out)
			}
			if out := mustRun("docs", "refresh", "--background"); !strings.Contains(out, "caches cleared") {
				t.Errorf("refresh: %q", out)
			}
			if out := mustRun("docs", "remove", "willesq"); !strings.Contains(out, "removed docs owner willesq") {
				t.Errorf("remove: %q", out)
			}
			if out := mustRun("docs", "owners", "list"); !strings.Contains(out, "no docs owners") {
				t.Errorf("owners list after remove: %q", out)
			}
			if out := mustRun("docs", "status"); !strings.Contains(out, "no docs owners") {
				t.Errorf("status after remove: %q", out)
			}
		})
	}
}

func TestDocsSyncForeground(t *testing.T) {
	tests := []struct {
		name     string
		gh       fakeGH
		git      fakeGit
		args     []string
		want     string
		wantErr  bool
		wantSkip string // substring that must NOT appear
	}{
		{
			name: "syncs one owner now and reports repos + files",
			gh:   fakeGH{repos: []docs.Repo{{Name: "infra", HeadSHA: "abc"}}},
			git:  fakeGit{files: map[string]string{"README.md": "# hello"}},
			args: []string{"docs", "sync", "willesq"},
			want: "synced repos=1 files=1",
		},
		{
			name:     "syncs all owners when none is named",
			gh:       fakeGH{repos: []docs.Repo{{Name: "infra", HeadSHA: "abc"}}},
			git:      fakeGit{files: map[string]string{"docs/guide.md": "# guide"}},
			args:     []string{"docs", "sync"},
			want:     "syncing willesq",
			wantSkip: "orchd will sync on its next scan",
		},
		{
			name: "ingests nothing when the owner has no repos",
			gh:   fakeGH{},
			args: []string{"docs", "sync"},
			want: "synced repos=0 files=0",
		},
		{
			name:    "unknown owner is an error",
			gh:      fakeGH{},
			args:    []string{"docs", "sync", "nobody"},
			wantErr: true,
		},
		{
			name:    "repo enumeration failure surfaces as an error",
			gh:      fakeGH{err: errors.New("gh: rate limited")},
			args:    []string{"docs", "sync", "willesq"},
			want:    "status=error",
			wantErr: true,
		},
		{
			name: "refresh re-ingests in the foreground",
			gh:   fakeGH{repos: []docs.Repo{{Name: "infra", HeadSHA: "abc"}}},
			git:  fakeGit{files: map[string]string{"README.md": "# hello"}},
			args: []string{"docs", "refresh", "willesq"},
			want: "synced repos=1 files=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			stubDocsTooling(t, tt.gh, tt.git)
			f := factory(&fakeClient{ingest: &orchdv1.IngestResponse{Chunks: 1}}, nil)
			if _, err := run(f, "", "docs", "add", "willesq"); err != nil {
				t.Fatalf("add: %v", err)
			}
			out, err := run(f, "", tt.args...)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v (out %q)", err, tt.wantErr, out)
			}
			if tt.want != "" && !strings.Contains(out, tt.want) {
				t.Errorf("output %q missing %q", out, tt.want)
			}
			if tt.wantSkip != "" && strings.Contains(out, tt.wantSkip) {
				t.Errorf("output %q should not contain %q", out, tt.wantSkip)
			}
		})
	}
}

func TestDocsListShowsEachDocWithStaleness(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	stubDocsTooling(t,
		fakeGH{repos: []docs.Repo{{Name: "infra", HeadSHA: "abc"}}},
		fakeGit{files: map[string]string{"README.md": "# hello", "docs/guide.md": "# guide"}})
	f := factory(&fakeClient{ingest: &orchdv1.IngestResponse{Chunks: 1}}, nil)
	for _, args := range [][]string{{"docs", "add", "willesq"}, {"docs", "sync", "willesq"}} {
		if out, err := run(f, "", args...); err != nil {
			t.Fatalf("%v: %v (%q)", args, err, out)
		}
	}
	out, err := run(f, "", "docs", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := []string{"repo://willesq/infra/README.md", "repo://willesq/infra/docs/guide.md"}
	if len(lines) != len(want) {
		t.Fatalf("want %d lines, got %q", len(want), out)
	}
	for i, uri := range want {
		if !strings.HasSuffix(lines[i], uri) || !strings.HasPrefix(lines[i], "just now") {
			t.Errorf("line %d = %q, want staleness + %s", i, lines[i], uri)
		}
	}
}

func TestDocsSyncDefersWhenDaemonUnreachable(t *testing.T) {
	tests := []struct {
		name string
		cf   clientFactory
	}{
		{name: "dial fails", cf: factory(nil, errors.New("no socket"))},
		{name: "ping fails", cf: factory(&fakeClient{err: errors.New("orchd down")}, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			stubDocsTooling(t, fakeGH{}, fakeGit{})
			if _, err := run(tt.cf, "", "docs", "add", "willesq"); err != nil {
				t.Fatalf("add: %v", err)
			}
			out, err := run(tt.cf, "", "docs", "sync", "willesq")
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if !strings.Contains(out, "orchd unreachable") || !strings.Contains(out, "next scan") {
				t.Errorf("output %q missing deferral notice", out)
			}
		})
	}
}

func TestRPCIngester(t *testing.T) {
	tests := []struct {
		name       string
		client     *fakeClient
		wantChunks int
		wantErr    bool
	}{
		{name: "returns chunk count", client: &fakeClient{ingest: &orchdv1.IngestResponse{Chunks: 3}}, wantChunks: 3},
		{name: "propagates rpc error", client: &fakeClient{err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := rag.Meta{SourceURI: "repo://willesq/infra/README.md", SourceType: "repo_doc", Tags: []string{"owner:willesq"}}
			got, err := rpcIngester{client: tt.client}.IngestText(context.Background(), "text", meta)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantChunks {
				t.Errorf("chunks = %d, want %d", got, tt.wantChunks)
			}
		})
	}
}

func TestDocsAddInvalidRefresh(t *testing.T) {
	tests := []struct{ name string }{{name: "bad --refresh errors"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			_, err := run(factory(&fakeClient{}, nil), "", "docs", "add", "willesq", "--refresh", "notaduration")
			if err == nil {
				t.Fatal("expected error for invalid --refresh")
			}
		})
	}
}

func TestHumanAgoAndNextDue(t *testing.T) {
	now := time.Now().Unix()
	tests := []struct {
		name        string
		last        int64
		refresh     int64
		wantAgoSub  string
		wantNextSub string
	}{
		{name: "never synced", last: 0, refresh: 3600, wantAgoSub: "never", wantNextSub: "now"},
		{name: "recent, not yet due", last: now - 60, refresh: 3600, wantAgoSub: "ago", wantNextSub: "in "},
		{name: "overdue", last: now - 7200, refresh: 3600, wantAgoSub: "ago", wantNextSub: "now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanAgo(tt.last); !strings.Contains(got, tt.wantAgoSub) {
				t.Errorf("humanAgo(%d) = %q, want contains %q", tt.last, got, tt.wantAgoSub)
			}
			if got := nextDue(tt.last, tt.refresh); !strings.Contains(got, tt.wantNextSub) {
				t.Errorf("nextDue = %q, want contains %q", got, tt.wantNextSub)
			}
		})
	}
}
