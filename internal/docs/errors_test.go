package docs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// scriptedGit fails the named repos with their error and clones the rest.
type scriptedGit struct {
	mu     sync.Mutex
	fail   map[string]error
	block  map[string]bool // repos whose clone waits for its context
	cloned []string
}

func (g *scriptedGit) Sync(ctx context.Context, _, repo, _ string) error {
	g.mu.Lock()
	g.cloned = append(g.cloned, repo)
	g.mu.Unlock()
	if g.block[repo] {
		<-ctx.Done()
		return ctx.Err()
	}
	return g.fail[repo]
}

var (
	errTimeout = errors.New("exit status 128: ssh: connect to host github.com port 22: Operation timed out fatal: Could not read from remote repository.")
	errAuth    = errors.New("exit status 128: git@github.com: Permission denied (publickey). fatal: Could not read from remote repository.")
	errAccess  = errors.New("exit status 128: ERROR: Repository not found. fatal: Could not read from remote repository.")
)

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		kind string
	}{
		{errTimeout, kindNetwork},
		{errors.New("error connecting to api.github.com"), kindNetwork},
		{fmt.Errorf("clone took longer than 3m0s: %w", context.DeadlineExceeded), kindNetwork},
		{errAuth, kindAuth},
		{errors.New("gh auth login required"), kindAuth},
		{errAccess, kindAccess},
		{errors.New("gh: HTTP 404"), kindAccess},
		{errors.New("disk full"), kindOther},
		{fmt.Errorf("gh repo clone o/r: %w: no progress for 45s", errStalled), kindStalled},
	}
	for _, tt := range tests {
		if got := classify(tt.err); got.kind != tt.kind || got.hint == "" {
			t.Errorf("classify(%q) = %+v, want %s", tt.err, got, tt.kind)
		}
	}
	long := errors.New(strings.Repeat("x", 500))
	if d := classify(long).describe("clone", long); len(d) > 450 || !strings.HasSuffix(d, "…") {
		t.Errorf("describe did not cap: %d", len(d))
	}
	if d := classify(errTimeout).describe("clone", errors.New("a\nb")); strings.Contains(d, "\n") {
		t.Errorf("describe kept a newline: %q", d)
	}
}

func repos(names ...string) []Repo {
	var out []Repo
	for _, n := range names {
		out = append(out, Repo{Name: n, HeadSHA: "sha-" + n})
	}
	return out
}

func TestSyncStopsAfterRepeatedNetworkFailures(t *testing.T) {
	git := &scriptedGit{fail: map[string]error{"b": errTimeout, "c": errTimeout, "d": errTimeout}}
	reg, ing, opts := syncDeps(t, fakeGH{repos: repos("a", "b", "c", "d", "e", "f")}, git)
	var progress []string
	opts.Progress = func(done, total int, repo string) {
		progress = append(progress, fmt.Sprintf("%d/%d %s", done, total, repo))
	}
	res := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts)
	if res.Status != "error" || res.Synced != 1 || res.Failed != 3 || res.NotTried != 2 || res.Repos != 6 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Reason, "could not be cloned (network)") || !strings.Contains(res.Reason, "2 not tried") {
		t.Errorf("reason = %q", res.Reason)
	}
	if strings.Join(git.cloned, ",") != "a,b,c,d" {
		t.Errorf("cloned %v; e and f should not be tried", git.cloned)
	}
	if strings.Join(progress, "|") != "1/6 a|2/6 b|3/6 c|4/6 d" {
		t.Errorf("progress = %v", progress)
	}
	states, _ := reg.ListRepos(context.Background(), "acme")
	byName := map[string]RepoState{}
	for _, s := range states {
		byName[s.Repo] = s
	}
	if byName["b"].Status != "error" || !strings.Contains(byName["b"].Error, "clone failed (network)") {
		t.Errorf("b = %+v", byName["b"])
	}
	if _, tried := byName["e"]; tried {
		t.Error("e was recorded though never tried")
	}
}

func TestSyncContinuesPastOtherFailures(t *testing.T) {
	// Failures of other kinds, and network failures that are not consecutive,
	// do not stop the sync.
	git := &scriptedGit{fail: map[string]error{"a": errAuth, "b": errTimeout, "d": errTimeout, "e": errAccess}}
	reg, ing, opts := syncDeps(t, fakeGH{repos: repos("a", "b", "c", "d", "e")}, git)
	res := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts)
	if res.Status != "partial" || res.Failed != 4 || res.Synced != 1 || res.NotTried != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Reason, "4 of 5 repos failed") {
		t.Errorf("reason = %q", res.Reason)
	}
	states, _ := reg.ListRepos(context.Background(), "acme")
	for _, s := range states {
		switch s.Repo {
		case "a":
			if !strings.Contains(s.Error, "(authentication)") || !strings.Contains(s.Error, "SSH agent") {
				t.Errorf("a = %q", s.Error)
			}
		case "e":
			if !strings.Contains(s.Error, "(access)") {
				t.Errorf("e = %q", s.Error)
			}
		case "c":
			if s.Status != "ok" || s.Error != "" {
				t.Errorf("c = %+v", s)
			}
		}
	}
}

func TestAFixedRepoClearsItsError(t *testing.T) {
	git := &scriptedGit{fail: map[string]error{"a": errAuth}}
	reg, ing, opts := syncDeps(t, fakeGH{repos: repos("a")}, git)
	ctx := context.Background()
	_ = SyncOwner(ctx, reg, Owner{Owner: "acme"}, ing, opts)
	git.fail = nil
	if res := SyncOwner(ctx, reg, Owner{Owner: "acme"}, ing, opts); res.Status != "ok" || res.Reason != "" {
		t.Fatalf("result = %+v", res)
	}
	states, _ := reg.ListRepos(ctx, "acme")
	if states[0].Status != "ok" || states[0].Error != "" {
		t.Errorf("state = %+v", states[0])
	}
}

func TestACloneThatHangsIsCutOff(t *testing.T) {
	saved := cloneTimeout
	cloneTimeout = 30 * time.Millisecond
	t.Cleanup(func() { cloneTimeout = saved })
	git := &scriptedGit{block: map[string]bool{"slow": true}}
	reg, ing, opts := syncDeps(t, fakeGH{repos: repos("slow", "fast")}, git)
	res := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts)
	if res.Failed != 1 || res.Synced != 1 {
		t.Fatalf("result = %+v", res)
	}
	states, _ := reg.ListRepos(context.Background(), "acme")
	for _, s := range states {
		if s.Repo == "slow" && !strings.Contains(s.Error, "took longer than") {
			t.Errorf("slow = %q", s.Error)
		}
	}
}

func TestACancelledSyncStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reg, ing, opts := syncDeps(t, fakeGH{repos: repos("a", "b")}, &scriptedGit{})
	res := SyncOwner(ctx, reg, Owner{Owner: "acme"}, ing, opts)
	if res.Status != "error" || res.NotTried != 2 || !strings.HasPrefix(res.Reason, "stopped") {
		t.Errorf("result = %+v", res)
	}
}

func TestScanRunsOneSyncAtATime(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	_ = reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 1})
	release := make(chan struct{})
	started := make(chan struct{})
	gh := ghFunc(func(context.Context, string) ([]Repo, error) {
		close(started)
		<-release
		return repos("a"), nil
	})
	s := NewScheduler(reg, &fakeIng{}, SyncOpts{RepoCache: t.TempDir(), GH: gh, Git: &scriptedGit{}, Now: func() int64 { return 100 }}, 0)

	done := make(chan struct{})
	go func() { s.Scan(ctx); close(done) }()
	<-started
	if p := s.Progress(); !p.Running || p.Owner != "acme" {
		t.Fatalf("progress while running = %+v", p)
	}
	// A second scan while one runs returns at once and queues nothing.
	second := make(chan struct{})
	go func() { s.Scan(ctx); close(second) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("a scan during a running sync waited instead of returning")
	}
	close(release)
	<-done
	p := s.Progress()
	if p.Running || p.Done != 1 || p.Total != 1 || p.FinishedAt.IsZero() {
		t.Errorf("progress after = %+v", p)
	}
	owners, _ := reg.ListOwners(ctx)
	if owners[0].Status != "ok" || owners[0].Error != "" {
		t.Errorf("owner = %+v", owners[0])
	}
}

func TestScanRecordsWhyAnOwnerFailed(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	_ = reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 1})
	git := &scriptedGit{fail: map[string]error{"a": errTimeout, "b": errTimeout, "c": errTimeout}}
	s := NewScheduler(reg, &fakeIng{}, SyncOpts{RepoCache: t.TempDir(), GH: fakeGH{repos: repos("a", "b", "c")}, Git: git, Now: func() int64 { return 100 }}, 0)
	s.Scan(ctx)
	owners, _ := reg.ListOwners(ctx)
	if owners[0].Status != "error" || !strings.Contains(owners[0].Error, "could not be cloned") {
		t.Errorf("owner = %+v", owners[0])
	}
	if s.Running() {
		t.Error("still running after the scan")
	}
}

func TestStallsCountTowardStopping(t *testing.T) {
	stall := fmt.Errorf("gh repo clone: %w: no progress for 45s", errStalled)
	git := &scriptedGit{fail: map[string]error{"a": stall, "b": errTimeout, "c": stall}}
	reg, ing, opts := syncDeps(t, fakeGH{repos: repos("a", "b", "c", "d")}, git)
	res := SyncOwner(context.Background(), reg, Owner{Owner: "acme"}, ing, opts)
	if res.Status != "error" || res.NotTried != 1 || !strings.Contains(res.Reason, "(stalled)") || !strings.Contains(res.Reason, "1Password") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunWatched(t *testing.T) {
	ctx := context.Background()

	// Silent: stopped once it has written nothing for the stall time, along
	// with the child it started.
	pidFile := t.TempDir() + "/child.pid"
	start := time.Now()
	_, err := runWatched(ctx, 200*time.Millisecond, "sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	if !errors.Is(err, errStalled) || time.Since(start) > 10*time.Second {
		t.Fatalf("silent command: err = %v after %s", err, time.Since(start))
	}
	time.Sleep(100 * time.Millisecond)
	if b, rerr := os.ReadFile(pidFile); rerr == nil {
		var pid int
		fmt.Sscan(strings.TrimSpace(string(b)), &pid)
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			t.Errorf("child %d still running after the stall", pid)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}

	// Keeps writing progress for longer than the stall time: not stopped.
	out, err := runWatched(ctx, 300*time.Millisecond, "sh", "-c",
		"for i in 1 2 3 4 5 6; do echo progress $i% >&2; sleep 0.1; done; echo done")
	if err != nil || strings.TrimSpace(string(out)) != "done" {
		t.Fatalf("progressing command: %q, %v", out, err)
	}

	// Fails: the error keeps the end of stderr, without the progress lines.
	_, err = runWatched(ctx, time.Second, "sh", "-c",
		"echo 'Receiving objects: 50%' >&2; echo 'fatal: Could not read from remote repository.' >&2; exit 128")
	if err == nil || !strings.Contains(err.Error(), "Could not read from remote repository") || strings.Contains(err.Error(), "50%") {
		t.Fatalf("failing command err = %v", err)
	}

	if _, err := runWatched(ctx, time.Second, "/no/such/command"); err == nil {
		t.Error("a missing command should fail")
	}
}

func TestLastLines(t *testing.T) {
	got := lastLines([]byte("Cloning into 'x'...\rReceiving objects:  10%\rReceiving objects: 100%\nwarning: a\nfatal: b\n"))
	if got != "Cloning into 'x'... warning: a fatal: b" {
		t.Errorf("lastLines = %q", got)
	}
}

func TestRequestNeverInterruptsARunningSync(t *testing.T) {
	reg := testRegistry(t)
	ctx := context.Background()
	_ = reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 3600})
	release := make(chan struct{})
	listing := make(chan struct{})
	gh := ghFunc(func(context.Context, string) ([]Repo, error) {
		close(listing)
		<-release
		return repos("a"), nil
	})
	// The clock is past the owner's refresh interval, so it is due.
	s := NewScheduler(reg, &fakeIng{}, SyncOpts{RepoCache: t.TempDir(), GH: gh, Git: &scriptedGit{}, Now: func() int64 { return 10_000 }}, time.Hour)
	_ = reg.MarkDue(ctx, "acme")

	done := make(chan struct{})
	go func() { s.Scan(ctx); close(done) }()
	<-listing
	prepared := false
	if err := s.Request(func() error { prepared = true; return nil }); !errors.Is(err, ErrBusy) || prepared {
		t.Fatalf("request during a sync = %v (prepared %v), want ErrBusy without preparing", err, prepared)
	}
	close(release)
	<-done

	// Idle: the request prepares, and the next Tick scans though the
	// interval has not passed.
	if err := s.Request(func() error { return reg.MarkDue(ctx, "acme") }); err != nil {
		t.Fatal(err)
	}
	s.lastScan = time.Now()
	listed := 0
	s.opts.GH = ghFunc(func(context.Context, string) ([]Repo, error) { listed++; return repos("a"), nil })
	s.Tick(ctx)
	if listed != 1 {
		t.Errorf("a requested Tick listed %d times, want 1", listed)
	}
	s.Tick(ctx)
	if listed != 1 {
		t.Error("a Tick without a request rescanned before the interval")
	}
	if err := s.Request(func() error { return errors.New("db locked") }); err == nil || s.kicked.Load() {
		t.Error("a failed preparation should fail and not kick")
	}
}

func TestAStoppedSyncLeavesTheOwnerDue(t *testing.T) {
	reg := testRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	_ = reg.AddOwner(ctx, Owner{Owner: "acme", RefreshSecs: 3600})
	gh := ghFunc(func(context.Context, string) ([]Repo, error) { cancel(); return repos("a", "b"), nil })
	s := NewScheduler(reg, &fakeIng{}, SyncOpts{RepoCache: t.TempDir(), GH: gh, Git: &scriptedGit{}, Now: func() int64 { return 5000 }}, 0)
	s.Scan(ctx)
	owners, _ := reg.ListOwners(context.Background())
	if owners[0].LastSynced != 0 || owners[0].Status != "error" || !strings.HasPrefix(owners[0].Error, "stopped") {
		t.Errorf("owner = %+v, want still due with the reason", owners[0])
	}
	if s.Running() {
		t.Error("still running after a stopped scan")
	}
}

func TestCleanReason(t *testing.T) {
	tests := []struct{ in, want string }{
		{"fatal: unable to access 'https://oauth2:s3cr3t@github.example/x.git/'", "fatal: unable to access 'https://[REDACTED]@github.example/x.git/'"},
		{"remote: \x1b[31mred\x1b[0m \u202eevil\nnext\tline", "remote: [31mred[0m evil next line"},
		{"bad \xff byte", "bad \uFFFD byte"},
		{"token ghp_abcdefghijklmnopqrstuvwxyz0123456789 here", ""},
	}
	for _, tt := range tests {
		got := cleanReason(tt.in, 300)
		if tt.want != "" && got != tt.want {
			t.Errorf("cleanReason(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if strings.Contains(got, "s3cr3t") || strings.Contains(got, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
			t.Errorf("cleanReason(%q) kept a secret: %q", tt.in, got)
		}
	}
	long := strings.Repeat("é", 400)
	if got := cleanReason(long, 300); !utf8.ValidString(got) || len([]rune(got)) != 301 {
		t.Errorf("cut %d runes, valid %v", len([]rune(got)), utf8.ValidString(got))
	}
	if f := classify(fmt.Errorf("gh repo clone: %w", exec.ErrNotFound)); f.kind != kindOther || !strings.Contains(f.hint, "not installed") {
		t.Errorf("missing gh = %+v", f)
	}
	if f := classify(errors.New("exec: \"gh\": executable file not found in $PATH")); f.kind == kindAccess {
		t.Errorf("a missing binary is not an access problem: %+v", f)
	}
}

func TestRunWatchedIsBoundedByAnEscapedChild(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("needs perl to start a child in a new session")
	}
	saved := waitDelay
	waitDelay = 300 * time.Millisecond
	t.Cleanup(func() { waitDelay = saved })
	pidFile := t.TempDir() + "/escaped.pid"
	// The child leaves the process group (setsid) but keeps stderr open, then
	// the command fails at once.
	script := `perl -MPOSIX -e 'if (fork()==0) { POSIX::setsid(); open(my $f, ">", "` + pidFile + `"); print $f $$; close $f; sleep 30; exit 0 } sleep 1; exit 1'`
	start := time.Now()
	_, err := runWatched(context.Background(), 10*time.Second, "sh", "-c", script)
	if err == nil || time.Since(start) > 8*time.Second {
		t.Errorf("err = %v after %s; want a prompt failure", err, time.Since(start))
	}
	if b, rerr := os.ReadFile(pidFile); rerr == nil {
		var pid int
		fmt.Sscan(strings.TrimSpace(string(b)), &pid)
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
