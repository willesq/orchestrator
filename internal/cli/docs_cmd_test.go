package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/docs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestDocsCLIFlow(t *testing.T) {
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
	if out := mustRun("docs", "sync", "willesq", "--background"); !strings.Contains(out, "orchd is syncing") {
		t.Errorf("sync --background: %q", out)
	}
	if out := mustRun("docs", "remove", "willesq"); !strings.Contains(out, "removed docs owner willesq") {
		t.Errorf("remove: %q", out)
	}
	if out := mustRun("docs", "status"); !strings.Contains(out, "no docs owners") {
		t.Errorf("status after remove: %q", out)
	}
}

// seedDocsOwner registers willesq with one healthy and one failing repo.
func seedDocsOwner(t *testing.T, status, reason string) {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	reg := docs.NewRegistry(conn)
	ctx := context.Background()
	if err := reg.AddOwner(ctx, docs.Owner{Owner: "willesq", RefreshSecs: 3600}); err != nil {
		t.Fatal(err)
	}
	_ = reg.SetRepoState(ctx, docs.RepoState{Owner: "willesq", Repo: "infra", HeadSHA: "abc", Files: 2, Status: "ok"})
	if status != "ok" {
		_ = reg.SetRepoState(ctx, docs.RepoState{Owner: "willesq", Repo: "airflow", Status: "error",
			Error: "clone failed (network): GitHub could not be reached from this machine"})
	}
	_ = reg.MarkSynced(ctx, "willesq", time.Now().Unix(), status, reason)
}

func TestDocsSyncFollowsOrchd(t *testing.T) {
	saved := docsPollInterval
	docsPollInterval = time.Millisecond
	t.Cleanup(func() { docsPollInterval = saved })
	now := time.Now().Unix() + 5

	tests := []struct {
		name      string
		args      []string
		status    string
		reason    string
		states    []*orchdv1.DocsSyncState
		syncErr   error
		wantForce bool
		wantOwner string
		want      []string
		wantErr   string
	}{
		{
			name:   "follows progress and reports a clean sync",
			args:   []string{"docs", "sync", "willesq"},
			status: "ok",
			states: []*orchdv1.DocsSyncState{
				{Running: true, Owner: "willesq", StartedAt: now, Done: 1, Total: 2, Repo: "infra"},
				{Running: true, Owner: "willesq", StartedAt: now, Done: 2, Total: 2, Repo: "docs"},
				{Owner: "willesq", StartedAt: now, FinishedAt: now + 1, Done: 2, Total: 2},
			},
			wantOwner: "willesq",
			want:      []string{"willesq: 1/2 repos (last: infra)", "willesq: 2/2 repos", "repos=1 files=2 status=ok"},
		},
		{
			name:   "a failed repo needs attention",
			args:   []string{"docs", "refresh"},
			status: "error",
			reason: "stopped after 3 repos in a row could not reach GitHub; 40 not tried: check the network connection",
			states: []*orchdv1.DocsSyncState{
				{Running: true, Owner: "willesq", StartedAt: now, Done: 3, Total: 44, Repo: "airflow"},
				{Owner: "willesq", StartedAt: now, FinishedAt: now + 1},
			},
			wantForce: true,
			want:      []string{"stopped after 3 repos", "airflow: clone failed (network)"},
			wantErr:   "needs attention",
		},
		{
			name:    "a running sync refuses the request instead of queueing it",
			args:    []string{"docs", "sync"},
			status:  "ok",
			syncErr: grpcstatus.Error(codes.Aborted, "a docs sync is already running (willesq, started 2m0s ago, 10 of 52 repos); this request was not queued"),
			wantErr: "already running",
		},
		{
			name:    "an unknown owner is refused",
			args:    []string{"docs", "sync", "nobody"},
			status:  "ok",
			syncErr: grpcstatus.Error(codes.NotFound, "nobody is not a docs owner"),
			wantErr: "not a docs owner",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			seedDocsOwner(t, tt.status, tt.reason)
			fc := &fakeClient{docsSyncErr: tt.syncErr, docsStates: tt.states}
			out, err := run(factory(fc, nil), "", tt.args...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q (out %q)", err, tt.wantErr, out)
				}
			} else if err != nil {
				t.Fatalf("err = %v (out %q)", err, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			if tt.syncErr == nil && (fc.docsSyncReq.GetForce() != tt.wantForce || fc.docsSyncReq.GetOwner() != tt.wantOwner) {
				t.Errorf("request = %v", fc.docsSyncReq)
			}
		})
	}
}

func TestDocsSyncWhenOrchdHasNotStarted(t *testing.T) {
	savedPoll, savedWait := docsPollInterval, docsStartWait
	docsPollInterval, docsStartWait = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { docsPollInterval, docsStartWait = savedPoll, savedWait })
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	seedDocsOwner(t, "ok", "")
	// Only an older, finished run is ever reported.
	fc := &fakeClient{docsStates: []*orchdv1.DocsSyncState{{Owner: "willesq", StartedAt: 1, FinishedAt: 2}}}
	out, err := run(factory(fc, nil), "", "docs", "sync", "willesq")
	if err != nil || !strings.Contains(out, "next scan") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestDocsListShowsEachDocWithStaleness(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	reg := docs.NewRegistry(conn)
	for _, uri := range []string{"repo://willesq/infra/README.md", "repo://willesq/infra/docs/guide.md"} {
		_ = reg.SetHash(context.Background(), uri, "h", time.Now().Unix())
	}
	_ = conn.Close()
	out, err := run(factory(&fakeClient{}, nil), "", "docs", "list")
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
		name  string
		cf    clientFactory
		args  []string
		check func(t *testing.T, reg *docs.Registry)
	}{
		{name: "dial fails", cf: factory(nil, errors.New("no socket")), args: []string{"docs", "sync", "willesq"}},
		{
			name: "orchd unavailable, refresh clears caches", cf: factory(&fakeClient{docsSyncErr: grpcstatus.Error(codes.Unavailable, "orchd down")}, nil),
			args: []string{"docs", "refresh", "willesq"},
			check: func(t *testing.T, reg *docs.Registry) {
				if sha, _ := reg.RepoSHA(context.Background(), "willesq", "infra"); sha != "" {
					t.Errorf("caches kept: %q", sha)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			seedDocsOwner(t, "ok", "")
			out, err := run(tt.cf, "", tt.args...)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if !strings.Contains(out, "orchd unreachable") || !strings.Contains(out, "next scan") {
				t.Errorf("output %q missing deferral notice", out)
			}
			conn, _ := db.Open()
			defer func() { _ = conn.Close() }()
			reg := docs.NewRegistry(conn)
			owners, _ := reg.ListOwners(context.Background())
			if len(owners) != 1 || owners[0].LastSynced != 0 {
				t.Errorf("owner not marked due: %+v", owners)
			}
			if tt.check != nil {
				tt.check(t, reg)
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
