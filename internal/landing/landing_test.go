package landing

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

const (
	testURL  = "https://github.com/acme/widget/pull/7"
	testPush = "https://github.com/acme/widget.git"
)

// world is a real "GitHub" (a local bare repository), a real agent worktree,
// and a service wired to both. Only gh and the push URL are faked.
type world struct {
	t        *testing.T
	home     string
	remote   string
	worktree string
	base     string
	sha      string
	author   string
	state    string
	result   string
	status   string
	ghErr    error
	svc      *Service
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a",
		"GIT_COMMITTER_EMAIL=a@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content, subject string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "--quiet", "-m", subject)
	return run(t, dir, "rev-parse", "HEAD")
}

func newWorld(t *testing.T) *world {
	t.Helper()
	// Commands must not read the developer's git config.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "a")
	t.Setenv("GIT_AUTHOR_EMAIL", "a@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "a")
	t.Setenv("GIT_COMMITTER_EMAIL", "a@example.com")
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, home: home, remote: filepath.Join(home, "remote.git"), worktree: filepath.Join(home, "wt"),
		author: "me", state: "OPEN", status: "complete"}
	run(t, home, "init", "--quiet", "--bare", "--initial-branch=main", w.remote)
	run(t, home, "clone", "--quiet", w.remote, w.worktree)
	run(t, w.worktree, "checkout", "--quiet", "-b", "feat/x")
	run(t, w.worktree, "config", "user.name", "a")
	run(t, w.worktree, "config", "user.email", "a@example.com")
	w.base = commit(t, w.worktree, "a.txt", "one\n", "first")
	run(t, w.worktree, "push", "--quiet", "origin", "feat/x")
	w.sha = commit(t, w.worktree, "a.txt", "one\ntwo\n", "second")
	w.result = "I did it.\n\nPROPOSED: " + w.worktree + " " + w.sha + "\n"

	t.Setenv("ORCH_DATA_DIR", filepath.Join(home, "data"))
	database, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	w.svc = &Service{
		Store: NewStore(database),
		Dir:   filepath.Join(home, "landings"),
		Home:  home,
		GH:    w.gh,
		Git: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			for i, arg := range args {
				if arg == testPush {
					args[i] = w.remote
				}
			}
			return Git(ctx, dir, args...)
		},
		Login: func(context.Context) (string, error) { return "ME", nil },
		Task: func(_ context.Context, id string) (Task, bool, error) {
			if id != "task-1" {
				return Task{}, false, nil
			}
			return Task{Body: "Fix it\nPR: " + testURL + "\n\nbrief", Result: w.result, Status: w.status}, true, nil
		},
	}
	return w
}

func (w *world) remoteHead() string { return run(w.t, w.remote, "rev-parse", "refs/heads/feat/x") }

func (w *world) gh(_ context.Context, args ...string) ([]byte, error) {
	if w.ghErr != nil {
		return nil, w.ghErr
	}
	return json.Marshal(map[string]any{
		"state": w.state, "headRefOid": w.remoteHead(), "headRefName": "feat/x", "baseRefName": "main",
		"isCrossRepository": false, "author": map[string]string{"login": w.author},
	})
}

func TestParseProposal(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, result string
		want         Proposal
		ok           bool
	}{
		{"last line", "report\n\nPROPOSED: /a/b " + sha, Proposal{"/a/b", sha}, true},
		{"trailing blank lines", "PROPOSED: /a/b " + sha + "\n\n  \n", Proposal{"/a/b", sha}, true},
		{"in backticks", "`PROPOSED: /a/b " + sha + "`", Proposal{"/a/b", sha}, true},
		{"space in path", "PROPOSED: /a b/c " + sha, Proposal{"/a b/c", sha}, true},
		{"quoted earlier, not last", "PROPOSED: /a/b " + sha + "\nand more", Proposal{}, false},
		{"relative path", "PROPOSED: a/b " + sha, Proposal{}, false},
		{"short sha", "PROPOSED: /a/b abc123", Proposal{}, false},
		{"empty", "", Proposal{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseProposal(tc.result)
			if got != tc.want || ok != tc.ok {
				t.Errorf("got %+v, %v", got, ok)
			}
		})
	}
}

func TestPRURL(t *testing.T) {
	for body, want := range map[string]string{
		"x\nPR: " + testURL + "\ny":                   testURL,
		"> PR: " + testURL:                            "",
		"PR: https://github.com/acme/widget/issues/7": "",
		"PR: https://evil.example/acme/widget/pull/7": "",
		"nothing": "",
	} {
		if got := PRURL(body); got != want {
			t.Errorf("PRURL(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestCheckPath(t *testing.T) {
	home := t.TempDir()
	if err := checkPath(home+"/sub/../..", home); err == nil {
		t.Error("an unclean path passed")
	}
	if err := checkPath("/etc", home); err == nil {
		t.Error("a path outside home passed")
	}
	if err := checkPath(home+"/gone", home); err == nil {
		t.Error("a missing path passed")
	}
	if err := checkPath(home+"/x", ""); err == nil {
		t.Error("no home passed")
	}
	if err := os.Mkdir(home+"/here", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkPath(home+"/here", home); err != nil {
		t.Errorf("a good path failed: %v", err)
	}
}

func TestGetShowsAProposalWithItsRealDiff(t *testing.T) {
	w := newWorld(t)
	l, err := w.svc.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != StateProposed || !l.CanLand || l.Blocked != "" {
		t.Fatalf("got %+v", l)
	}
	if l.URL != testURL || l.Branch != "feat/x" || l.BaseSHA != w.base || l.SHA != w.sha || l.Path != w.worktree {
		t.Errorf("got %+v", l)
	}
	if l.TotalCommits != 1 || len(l.Commits) != 1 || l.Commits[0].Subject != "second" || l.Commits[0].SHA != w.sha {
		t.Errorf("commits: %+v", l.Commits)
	}
	if len(l.Files) != 1 || l.Files[0] != (File{Path: "a.txt", Additions: 1}) {
		t.Errorf("files: %+v", l.Files)
	}
	if !strings.Contains(l.Diff, "+two") || l.DiffTruncated {
		t.Errorf("diff: %q", l.Diff)
	}
	if w.remoteHead() != w.base {
		t.Error("reading a proposal changed the remote")
	}
}

func TestGetWithNothingProposed(t *testing.T) {
	for name, change := range map[string]func(*world){
		"no line":       func(w *world) { w.result = "all done" },
		"still running": func(w *world) { w.status = "in_progress" },
		"failed anyway": func(w *world) { w.status = "failed" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			change(w)
			l, err := w.svc.Get(context.Background(), "task-1")
			if err != nil || l.State != StateNone || l.CanLand {
				t.Errorf("got %+v, %v", l, err)
			}
		})
	}
}

func TestGetUnknownAndBadTask(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.Get(context.Background(), "task-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := w.svc.Get(context.Background(), "../../evil"); !errors.Is(err, ErrRefused) {
		t.Errorf("traversal: %v", err)
	}
}

func TestGetWhenThePRMovedOn(t *testing.T) {
	w := newWorld(t)
	other := filepath.Join(w.home, "other")
	run(t, w.home, "clone", "--quiet", "--branch", "feat/x", w.remote, other)
	run(t, other, "config", "user.name", "b")
	run(t, other, "config", "user.email", "b@example.com")
	commit(t, other, "b.txt", "b\n", "someone else")
	run(t, other, "push", "--quiet", "origin", "feat/x")

	l, err := w.svc.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != StateStale || l.CanLand || !strings.Contains(l.Problem, "fast-forward") {
		t.Errorf("got %+v", l)
	}
	if _, err := w.svc.Land(context.Background(), "task-1", w.sha); !errors.Is(err, ErrRefused) {
		t.Errorf("land: %v", err)
	}
}

func TestGetWhenTheWorktreeMoved(t *testing.T) {
	w := newWorld(t)
	commit(t, w.worktree, "a.txt", "three\n", "later")
	l, err := w.svc.Get(context.Background(), "task-1")
	if err != nil || l.State != StateInvalid || !strings.Contains(l.Problem, "moved on") {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestGetKeepsTheCommitOnceFetched(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Get(ctx, "task-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(w.worktree); err != nil {
		t.Fatal(err)
	}
	l, err := w.svc.Get(ctx, "task-1")
	if err != nil || l.State != StateProposed {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestGetWithAPathOutsideHomeNeverFetches(t *testing.T) {
	w := newWorld(t)
	w.result = "PROPOSED: /etc " + w.sha
	l, err := w.svc.Get(context.Background(), "task-1")
	if err != nil || l.State != StateInvalid || !strings.Contains(l.Problem, "outside your home") {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestGetWhenTheAgentPushedAnyway(t *testing.T) {
	w := newWorld(t)
	run(t, w.worktree, "push", "--quiet", "origin", "feat/x")
	l, err := w.svc.Get(context.Background(), "task-1")
	if err != nil || l.State != StateOnPR || l.CanLand {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestGetWhenGitHubIsDown(t *testing.T) {
	w := newWorld(t)
	w.ghErr = errors.New("gh: offline")
	if _, err := w.svc.Get(context.Background(), "task-1"); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("got %v", err)
	}
}

func TestLandAndUndo(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	l, err := w.svc.Land(ctx, "task-1", w.sha)
	if err != nil {
		t.Fatal(err)
	}
	if l.State != StateLanded || !l.CanUndo || l.BaseSHA != w.base || l.TotalCommits != 1 || l.DecidedAt == 0 {
		t.Fatalf("got %+v", l)
	}
	if w.remoteHead() != w.sha {
		t.Fatalf("remote at %s, want %s", w.remoteHead(), w.sha)
	}
	// Landing twice is refused rather than pushed again.
	if _, err := w.svc.Land(ctx, "task-1", w.sha); !errors.Is(err, ErrRefused) {
		t.Errorf("second land: %v", err)
	}

	l, err = w.svc.Undo(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != StateUndone || l.CanUndo || l.RevertSHA == "" {
		t.Fatalf("got %+v", l)
	}
	head := w.remoteHead()
	if head != l.RevertSHA {
		t.Errorf("remote at %s, want the revert %s", head, l.RevertSHA)
	}
	// Forward only: the landed commit stays in history, and the tree is back.
	if run(t, w.remote, "rev-parse", head+"^") != w.sha {
		t.Error("the revert is not on top of the landed commit")
	}
	if run(t, w.remote, "rev-parse", head+"^{tree}") != run(t, w.remote, "rev-parse", w.base+"^{tree}") {
		t.Error("the revert did not restore the earlier tree")
	}
	if _, err := w.svc.Undo(ctx, "task-1"); !errors.Is(err, ErrRefused) {
		t.Errorf("second undo: %v", err)
	}
}

func TestLandRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*world)
		sha    string
		want   string
	}{
		"another commit":    {sha: strings.Repeat("b", 40), want: "not the commit"},
		"someone else's PR": {change: func(w *world) { w.author = "them" }, want: "Someone else"},
		"closed":            {change: func(w *world) { w.state = "MERGED" }, want: "no longer open"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if tc.change != nil {
				tc.change(w)
			}
			sha := tc.sha
			if sha == "" {
				sha = w.sha
			}
			_, err := w.svc.Land(context.Background(), "task-1", sha)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v", err)
			}
			if w.remoteHead() != w.base {
				t.Error("the remote moved")
			}
		})
	}
}

func TestBlockedBranches(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, tc := range []struct {
		pr   PRState
		want string
	}{
		{PRState{State: "OPEN", Branch: "main", Base: "dev", Author: "me"}, "protected"},
		{PRState{State: "OPEN", Branch: "TRUNK", Base: "dev", Author: "me"}, "protected"},
		{PRState{State: "OPEN", Branch: "release", Base: "Release", Author: "me"}, "protected"},
		{PRState{State: "OPEN", Branch: "-x", Base: "main", Author: "me"}, "not one orch"},
		{PRState{State: "OPEN", Branch: "a..b", Base: "main", Author: "me"}, "not one orch"},
		{PRState{State: "OPEN", Branch: "x", Base: "main", Author: "me", CrossRepository: true}, "fork"},
	} {
		got, err := w.svc.blocked(ctx, testURL, tc.pr)
		if err != nil || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: got %q, %v", tc.pr, got, err)
		}
	}
}

func TestPushFailureLeavesItProposed(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	git := w.svc.Git
	w.svc.Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "push" {
				return nil, errors.New("git: protected branch hook declined")
			}
		}
		return git(ctx, dir, args...)
	}
	if _, err := w.svc.Land(ctx, "task-1", w.sha); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("got %v", err)
	}
	l, err := w.svc.Get(ctx, "task-1")
	if err != nil || l.State != StateProposed {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestUndoRefusedOnceTheBranchMoved(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Land(ctx, "task-1", w.sha); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(w.home, "other")
	run(t, w.home, "clone", "--quiet", "--branch", "feat/x", w.remote, other)
	run(t, other, "config", "user.name", "b")
	run(t, other, "config", "user.email", "b@example.com")
	later := commit(t, other, "b.txt", "b\n", "on top")
	run(t, other, "push", "--quiet", "origin", "feat/x")

	l, err := w.svc.Get(ctx, "task-1")
	if err != nil || l.CanUndo || !strings.Contains(l.Blocked, "moved") {
		t.Errorf("got %+v, %v", l, err)
	}
	if _, err := w.svc.Undo(ctx, "task-1"); !errors.Is(err, ErrRefused) {
		t.Errorf("undo: %v", err)
	}
	if w.remoteHead() != later {
		t.Error("the refused undo moved the branch")
	}
}

func TestUndoWithoutGitHubKeepsTheLanding(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Land(ctx, "task-1", w.sha); err != nil {
		t.Fatal(err)
	}
	w.ghErr = errors.New("gh: offline")
	l, err := w.svc.Get(ctx, "task-1")
	if err != nil || l.State != StateLanded || l.CanUndo || !strings.Contains(l.Blocked, "offline") {
		t.Errorf("got %+v, %v", l, err)
	}
	if _, err := w.svc.Undo(ctx, "task-1"); err == nil {
		t.Error("undo ran without GitHub")
	}
}

func TestDiscardAndRestore(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	l, err := w.svc.Discard(ctx, "task-1", false)
	if err != nil || l.State != StateDiscarded || l.CanLand {
		t.Fatalf("got %+v, %v", l, err)
	}
	if _, err := w.svc.Land(ctx, "task-1", w.sha); !errors.Is(err, ErrRefused) {
		t.Errorf("land after discard: %v", err)
	}
	if _, err := w.svc.Discard(ctx, "task-1", false); !errors.Is(err, ErrRefused) {
		t.Errorf("second discard: %v", err)
	}
	decisions, err := w.svc.Store.List(ctx)
	if err != nil || len(decisions) != 1 || decisions[0].State != StateDiscarded {
		t.Errorf("list: %+v, %v", decisions, err)
	}
	// The worktree is the agent's and may hold other work: discard leaves it.
	if _, err := os.Stat(w.worktree); err != nil {
		t.Error("discard touched the worktree")
	}
	l, err = w.svc.Discard(ctx, "task-1", true)
	if err != nil || l.State != StateProposed {
		t.Fatalf("restore: %+v, %v", l, err)
	}
	if _, err := w.svc.Discard(ctx, "task-1", true); !errors.Is(err, ErrRefused) {
		t.Errorf("restoring what is not discarded: %v", err)
	}
}

func TestLandedStateIsNotDiscardable(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Land(ctx, "task-1", w.sha); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Discard(ctx, "task-1", false); !errors.Is(err, ErrRefused) {
		t.Errorf("got %v", err)
	}
	if _, err := w.svc.Discard(ctx, "task-1", true); !errors.Is(err, ErrRefused) {
		t.Errorf("restore got %v", err)
	}
}

func TestGitIgnoresARepositoryNamedInItsEnvironment(t *testing.T) {
	w := newWorld(t)
	t.Setenv("GIT_DIR", w.remote)
	out, err := Git(context.Background(), w.worktree, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "remote.git") {
		t.Errorf("GIT_DIR was obeyed: %s", out)
	}
}

// Undo signs when the user signs commits, and a signer that fails stops it
// before anything is pushed.
func TestUndoSignsWhenTheUserDoes(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Land(ctx, "task-1", w.sha); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(w.home, "gitconfig")
	if err := os.WriteFile(config, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /usr/bin/false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	if _, err := w.svc.Undo(ctx, "task-1"); err == nil {
		t.Fatal("undo went ahead without a signature")
	}
	if w.remoteHead() != w.sha {
		t.Error("the failed undo moved the branch")
	}
	l, err := w.svc.Get(ctx, "task-1")
	if err != nil || l.State != StateLanded || !l.CanUndo {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestPruneRemovesOnlyOldRepositories(t *testing.T) {
	dir := t.TempDir()
	svc := &Service{Dir: dir}
	for _, name := range []string{"old.git", "new.git", "keep-not-a-repo"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"old.git", "keep-not-a-repo"} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	svc.Prune(24*time.Hour, time.Now())
	for name, want := range map[string]bool{"old.git": false, "new.git": true, "keep-not-a-repo": true} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", name, err == nil, want)
		}
	}
	(&Service{Dir: filepath.Join(dir, "missing")}).Prune(time.Hour, time.Now())
}

func TestGitVersionSafe(t *testing.T) {
	for output, want := range map[string]bool{
		"git version 2.56.0\n":               true,
		"git version 2.45.1":                 true,
		"git version 2.45.1 (Apple Git-150)": true,
		"git version 3.0.0":                  true,
		"git version 2.45.0":                 false,
		"git version 2.39.5 (Apple Git-154)": false,
		"git version 1.99.9":                 false,
		"git version nope":                   false,
		"git version 2.45":                   false,
		"nonsense":                           false,
	} {
		if got := gitVersionSafe(output); got != want {
			t.Errorf("%q: got %v", output, got)
		}
	}
}

// An agent runs as the same user, so it can write where the private repository
// lives: whatever it plants must not be used.
func TestAPlantedRepositoryIsThrownAway(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, repo string){
		"config": func(t *testing.T, repo string) {
			t.Helper()
			run(t, repo, "config", "url.https://evil.example/.pushInsteadOf", testPush)
		},
		"hooks": func(t *testing.T, repo string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(repo, "hooks"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"replace refs": func(t *testing.T, repo string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(repo, "refs", "replace"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			if _, err := w.svc.Get(ctx, "task-1"); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(w.svc.Dir, "task-1.git")
			plant(t, repo)
			if repoTrusted(repo) {
				t.Fatal("the planted repository passed")
			}
			l, err := w.svc.Get(ctx, "task-1")
			if err != nil || l.State != StateProposed {
				t.Fatalf("get: %+v, %v", l, err)
			}
			if !repoTrusted(repo) {
				t.Error("reading did not rebuild it")
			}
			if _, err := w.svc.Land(ctx, "task-1", w.sha); err != nil {
				t.Fatal(err)
			}
			if w.remoteHead() != w.sha {
				t.Error("the commit did not reach the PR's branch")
			}
		})
	}
}

// A replaced loose object can make the diff show one change while the push
// sends another, so the push must not trust what it showed.
func TestLandRefusesAForgedObject(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Get(ctx, "task-1"); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(w.svc.Dir, "task-1.git")
	blob := run(t, repo, "rev-parse", w.sha+":a.txt")
	path := filepath.Join(repo, "objects", blob[:2], blob[2:])
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	var forged bytes.Buffer
	zw := zlib.NewWriter(&forged)
	if _, err := zw.Write([]byte("blob 5\x00harm\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, forged.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Land(ctx, "task-1", w.sha); !errors.Is(err, ErrRefused) {
		t.Errorf("land: %v", err)
	}
	if w.remoteHead() != w.base {
		t.Error("the forged commit reached the remote")
	}
}

// Git prefers a .git inside the directory it runs in, so a landing repository
// must be named, not discovered.
func TestAPlantedDotGitIsNotRead(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Get(ctx, "task-1"); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(w.svc.Dir, "task-1.git")
	other := filepath.Join(w.home, "other.git")
	run(t, w.home, "init", "--quiet", "--bare", other)
	run(t, other, "config", "user.name", "planted")
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := Git(ctx, repo, "config", "--local", "--get", "user.name"); err == nil {
		t.Errorf("the planted repository's config was read: %q", out)
	}
	if repoTrusted(repo) {
		t.Error("a planted .git passed")
	}
}

func TestLandRefusesARepositoryChangedAfterTheLastCheck(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.Land(ctx, "task-1", w.sha); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(w.svc.Dir, "task-1.git")
	run(t, repo, "config", "gpg.program", "/tmp/evil")
	if _, err := w.svc.Undo(ctx, "task-1"); !errors.Is(err, ErrRefused) {
		t.Errorf("undo: %v", err)
	}
	if w.remoteHead() != w.sha {
		t.Error("the refused undo moved the branch")
	}
}

func TestCheckPathFollowsSymlinks(t *testing.T) {
	home := t.TempDir()
	link := filepath.Join(home, "out")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := checkPath(link, home); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("got %v", err)
	}
}

func TestBinaryAndHugeChangesAreCounted(t *testing.T) {
	w := newWorld(t)
	if err := os.WriteFile(filepath.Join(w.worktree, "b.bin"), []byte{0, 1, 2, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range maxFiles + 2 {
		if err := os.WriteFile(filepath.Join(w.worktree, "f"+strconv.Itoa(i)+".txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w.sha = commit(t, w.worktree, "a.txt", "one\ntwo\n3\n", "big")
	w.result = "PROPOSED: " + w.worktree + " " + w.sha
	l, err := w.svc.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.BinaryFiles != 1 || l.TotalFiles != maxFiles+4 || len(l.Files) != maxFiles {
		t.Errorf("binary %d, total %d, listed %d", l.BinaryFiles, l.TotalFiles, len(l.Files))
	}
	if l.Additions != maxFiles+2+2 {
		t.Errorf("additions %d", l.Additions)
	}
}

func TestCapDiffAndParsers(t *testing.T) {
	huge := []byte(strings.Repeat("line\n", maxDiffSize))
	out, truncated := capDiff(huge)
	if !truncated || len(out) > maxDiffSize || !strings.HasSuffix(out, "\n") {
		t.Errorf("cap: %d, %v", len(out), truncated)
	}
	if out, truncated := capDiff([]byte("a\xff\n")); truncated || out != "a?\n" {
		t.Errorf("utf8: %q", out)
	}
	files := parseNumstat("1\t2\ta.txt\x00-\t-\tbin\x00\x00garbage\x00")
	if len(files) != 2 || files[0] != (File{Path: "a.txt", Additions: 1, Deletions: 2}) || files[1] != (File{Path: "bin", Binary: true}) {
		t.Errorf("numstat: %+v", files)
	}
	if got := parseCommits("s\x00subj\x00me\nnot a commit\n"); len(got) != 1 || got[0].Author != "me" {
		t.Errorf("commits: %+v", got)
	}
}

func TestReadPRRejectsBadOutput(t *testing.T) {
	w := newWorld(t)
	w.svc.GH = func(context.Context, ...string) ([]byte, error) { return []byte("nope"), nil }
	if _, err := w.svc.readPR(context.Background(), testURL); err == nil {
		t.Error("bad JSON passed")
	}
	w.svc.GH = func(context.Context, ...string) ([]byte, error) { return []byte(`{"headRefOid":"short"}`), nil }
	if _, err := w.svc.readPR(context.Background(), testURL); err == nil {
		t.Error("a missing head passed")
	}
}

// TestLandAsTheOwnersProfile (SP-063): with AccountFor set, the PR must be
// the owner's account's, and the push and its undo run git in that account's
// environment without the user's git configuration; a refused owner pushes
// nothing.
func TestLandAsTheOwnersProfile(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.author = "work-me"
	var owners []string
	w.svc.AccountFor = func(_ context.Context, owner string) (Account, error) {
		owners = append(owners, owner)
		return Account{Login: "Work-Me", Env: append(os.Environ(), "GH_CONFIG_DIR=/profiles/acme", "SSH_ASKPASS=/bin/false")}, nil
	}
	var pushes [][]string
	w.svc.GitAs = func(env []string) GitRunner {
		return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			pushes = append(pushes, env)
			for i, arg := range args {
				if arg == testPush {
					args[i] = w.remote
				}
			}
			return gitIn(ctx, env, profileGitEnv, dir, args...)
		}
	}
	if _, err := w.svc.Land(ctx, "task-1", w.sha); err != nil {
		t.Fatal(err)
	}
	if w.remoteHead() != w.sha {
		t.Fatalf("remote at %s, want %s", w.remoteHead(), w.sha)
	}
	if _, err := w.svc.Undo(ctx, "task-1"); err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 2 {
		t.Fatalf("pushes as the profile = %d, want land and undo", len(pushes))
	}
	for _, env := range pushes {
		if !slices.Contains(env, "GH_CONFIG_DIR=/profiles/acme") {
			t.Errorf("push env lacks the profile: %v", env)
		}
	}
	for _, o := range owners {
		if o != "acme" {
			t.Errorf("asked for owner %q", o)
		}
	}

	w2 := newWorld(t)
	w2.svc.AccountFor = func(context.Context, string) (Account, error) {
		return Account{}, errors.New("its space has no gh profile")
	}
	before := w2.remoteHead()
	if _, err := w2.svc.Land(ctx, "task-1", w2.sha); err == nil || !strings.Contains(err.Error(), "no gh profile") {
		t.Errorf("refused owner: err = %v", err)
	}
	if w2.remoteHead() != before {
		t.Error("a refused owner's proposal was pushed")
	}
}

func TestGitWithEnvDropsUserConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	ctx := context.Background()
	get := []string{"config", "--global", "--get", "url.git@github.com:.insteadof"}
	if out, err := Git(ctx, t.TempDir(), get...); err != nil || strings.TrimSpace(string(out)) != "https://github.com/" {
		t.Fatalf("Git reads the user's config: %q, %v", out, err)
	}
	if out, err := GitWithEnv(os.Environ())(ctx, t.TempDir(), get...); err == nil || len(out) != 0 {
		t.Errorf("GitWithEnv read the user's insteadOf rule: %q, %v", out, err)
	}
}
