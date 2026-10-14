package evidence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func insert(t *testing.T, conn *sql.DB, id, status, kind, prs, finished string) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at, task_kind, provider, model, prs)
		VALUES (?, 'dev', 'b', '', ?, ?, ?, 'claude-code', 'opus', ?)`, id, status, finished, kind, prs); err != nil {
		t.Fatal(err)
	}
}

func labels(t *testing.T, conn *sql.DB, id string) (rating, pr string, checked int64) {
	t.Helper()
	if err := conn.QueryRow(`SELECT COALESCE(rating,''), COALESCE(pr_outcome,''), COALESCE(pr_checked_at,0) FROM tasks WHERE id = ?`, id).
		Scan(&rating, &pr, &checked); err != nil {
		t.Fatal(err)
	}
	return
}

func TestRate(t *testing.T) {
	conn := testDB(t)
	insert(t, conn, "done", "complete", "code", "", "2026-10-01 10:00:00")
	insert(t, conn, "busy", "in_progress", "code", "", "2026-10-01 10:00:00")
	ctx := context.Background()
	if err := Rate(ctx, conn, "done", Rework, 5); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := labels(t, conn, "done"); r != Rework {
		t.Fatalf("rating = %q", r)
	}
	if err := Rate(ctx, conn, "done", Good, 6); err != nil { // re-rating replaces
		t.Fatal(err)
	}
	if r, _, _ := labels(t, conn, "done"); r != Good {
		t.Fatalf("rating = %q", r)
	}
	if err := Rate(ctx, conn, "busy", Good, 1); err == nil || !strings.Contains(err.Error(), "once it has finished") {
		t.Fatalf("unfinished err = %v", err)
	}
	if err := Rate(ctx, conn, "nope", Good, 1); !errors.Is(err, ErrNoTask) {
		t.Fatalf("missing err = %v", err)
	}
	if err := Rate(ctx, conn, "done", "great", 1); err == nil {
		t.Fatal("bad label accepted")
	}
}

// fakeGH answers `gh api user` and `gh pr view <url>` from a table.
type fakeGH struct {
	login   string
	prs     map[string]string // url → JSON
	calls   int
	userErr error
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls++
	if len(args) >= 2 && args[0] == "api" && args[1] == "user" {
		return []byte(f.login + "\n"), f.userErr
	}
	if len(args) < 3 || args[0] != "pr" || args[1] != "view" {
		return nil, fmt.Errorf("unexpected gh %v", args)
	}
	out, ok := f.prs[args[2]]
	if !ok {
		return nil, errors.New("gh: not found")
	}
	return []byte(out), nil
}

// The task under test is dispatched at 09:00 and finishes at 10:00 on Oct 1.
var (
	dispatched = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	finished   = "2026-10-01 10:00:00"
	opened     = "2026-10-01T09:20:00Z"
	before     = "2026-10-01T09:30:00Z"
	after      = "2026-10-02T08:00:00Z"
)

type commit struct{ authored, headline string }

func pr(state, author, created string, commits ...commit) string {
	var cs []string
	for _, c := range commits {
		h := c.headline
		if h == "" {
			h = "fix: thing"
		}
		cs = append(cs, fmt.Sprintf(`{"authoredDate":%q,"messageHeadline":%q}`, c.authored, h))
	}
	return fmt.Sprintf(`{"state":%q,"createdAt":%q,"author":{"login":%q},"commits":[%s]}`, state, created, author, strings.Join(cs, ","))
}

func mine(state string, commits ...commit) string { return pr(state, "me", opened, commits...) }

func insertAt(t *testing.T, conn *sql.DB, id, kind, prs string, created time.Time, fin string) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at, task_kind, provider, model, prs, created_at)
		VALUES (?, 'dev', 'b', '', 'complete', ?, ?, 'claude-code', 'opus', ?, ?)`, id, fin, kind, prs, created.Unix()); err != nil {
		t.Fatal(err)
	}
}

func TestAttributorSweep(t *testing.T) {
	u := func(n int) string { return fmt.Sprintf("https://github.com/acme/w/pull/%d", n) }
	gh := &fakeGH{login: "me", prs: map[string]string{
		u(1):  mine("MERGED", commit{authored: before}),
		u(2):  mine("MERGED", commit{authored: before}, commit{authored: after}),
		u(3):  mine("CLOSED", commit{authored: before}),
		u(4):  mine("OPEN", commit{authored: before}),
		u(5):  `not json`,
		u(6):  pr("MERGED", "someone-else", opened, commit{authored: before}),
		u(7):  pr("MERGED", "me", "2026-09-01T00:00:00Z", commit{authored: "2026-09-01T00:00:00Z"}),
		u(8):  mine("MERGED", commit{authored: before}, commit{authored: after, headline: "Merge branch 'main' into feat/x"}),
		u(9):  mine("CLOSED"),
		u(10): pr("MERGED", "ME", opened),
	}}
	conn := testDB(t)
	add := func(id, kind, prs string) { insertAt(t, conn, id, kind, prs, dispatched, finished) }
	add("clean", "code", u(1))
	add("reworked", "code", u(2))
	add("closed", "code", u(3))
	add("open", "code", u(4))
	add("worst-wins", "code", u(1)+","+u(2))
	add("one-open", "code", u(1)+","+u(4))
	add("unreadable", "code", u(5))
	add("foreign-pr", "code", u(6))                 // someone else's merged PR: never claimable
	add("old-pr", "code", u(7))                     // opened before the task existed
	add("foreign-ignored", "code", u(6)+","+u(3))   // the foreign PR is skipped, its own decides
	add("merge-commit", "code", u(8))               // "Update branch" is not rework
	add("login-case", "code", u(10))                // GitHub logins are case-insensitive
	add("fragment", "code", u(1)+"#issuecomment-1") // not a canonical PR URL
	add("not-github", "code", "--repo=x")
	add("too-many", "code", strings.TrimSuffix(strings.Repeat(u(1)+",", maxPRsPerTask+1), ","))
	add("unrouted", "", u(1))
	add("no-prs", "code", "")
	add("bad-time", "code", u(1))
	if _, err := conn.Exec(`UPDATE tasks SET updated_at = 'yesterday' WHERE id = 'bad-time'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at, task_kind, provider, model, prs)
		VALUES ('legacy', 'dev', 'b', '', 'complete', ?, 'code', 'claude-code', 'opus', ?)`, finished, u(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at, task_kind, provider, model, prs, created_at)
		VALUES ('failed', 'dev', 'b', '', 'failed', ?, 'code', 'claude-code', 'opus', ?, ?)`, finished, u(1), dispatched.Unix()); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	a := &Attributor{DB: conn, GH: gh.run, Recheck: time.Hour, Batch: 50, now: func() time.Time { return now }}
	err := a.Sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v, want the unreadable task reported", err)
	}
	want := map[string]string{
		"clean": Good, "reworked": Rework, "closed": Bad, "open": "", "worst-wins": Rework, "one-open": "",
		"unreadable": "", "foreign-pr": "", "old-pr": "", "foreign-ignored": Bad, "merge-commit": Good,
		"login-case": Good, "fragment": "", "not-github": "", "too-many": "", "unrouted": "", "no-prs": "",
		"bad-time": "", "legacy": "", "failed": "",
	}
	for id, w := range want {
		if _, got, _ := labels(t, conn, id); got != w {
			t.Errorf("%s: pr_outcome = %q, want %q", id, got, w)
		}
	}
	if _, _, checked := labels(t, conn, "open"); checked != now.Unix() {
		t.Errorf("open task check time = %d", checked)
	}
	for _, id := range []string{"unrouted", "legacy", "failed"} {
		if _, _, checked := labels(t, conn, id); checked != 0 {
			t.Errorf("%s was checked", id)
		}
	}

	// A second sweep inside the recheck window asks gh nothing.
	calls := gh.calls
	if err := a.Sweep(context.Background()); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if gh.calls != calls {
		t.Fatalf("recheck window ignored: %d more gh calls", gh.calls-calls)
	}
	// After it, the open PR is looked at again and now decides the task.
	gh.prs[u(4)] = mine("MERGED", commit{authored: before})
	now = now.Add(2 * time.Hour)
	_ = a.Sweep(context.Background())
	if _, got, _ := labels(t, conn, "open"); got != Good {
		t.Fatalf("open task after merge = %q", got)
	}
	if _, got, _ := labels(t, conn, "one-open"); got != Good {
		t.Fatalf("one-open task after merge = %q", got)
	}
	// Past the age cap nothing is looked at again.
	gh.prs[u(5)] = mine("MERGED")
	now = now.Add(maxAge)
	calls = gh.calls
	_ = a.Sweep(context.Background())
	if gh.calls != calls {
		t.Fatalf("aged-out tasks were re-checked: %d gh calls", gh.calls-calls)
	}
}

func TestAttributorBudgetAndErrors(t *testing.T) {
	conn := testDB(t)
	url := "https://github.com/acme/w/pull/1"
	recent := time.Now().Add(-time.Hour)
	fin := recent.Add(10 * time.Minute).UTC().Format(time.DateTime)
	for i := 0; i < callBudget+10; i++ {
		insertAt(t, conn, fmt.Sprintf("t%03d", i), "code", url, recent, fin)
	}
	gh := &fakeGH{login: "me", prs: map[string]string{url: pr("OPEN", "me", recent.Add(time.Minute).Format(time.RFC3339))}}
	a := &Attributor{DB: conn, GH: gh.run, Recheck: time.Hour, Batch: 100, now: time.Now}
	if err := a.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gh.calls != callBudget {
		t.Fatalf("sweep made %d gh calls, budget is %d", gh.calls, callBudget)
	}
	var unchecked int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM tasks WHERE COALESCE(pr_checked_at,0) = 0`).Scan(&unchecked)
	if unchecked != 11 {
		t.Fatalf("%d tasks left for the next sweep, want 11", unchecked)
	}

	gh.userErr = errors.New("gh: not logged in")
	if _, err := conn.Exec(`UPDATE tasks SET pr_checked_at = 0`); err != nil {
		t.Fatal(err)
	}
	if err := a.Sweep(context.Background()); err == nil || !strings.Contains(err.Error(), "gh api user") {
		t.Fatalf("err = %v", err)
	}
	gh.userErr, gh.login = nil, ""
	if err := a.Sweep(context.Background()); err == nil || !strings.Contains(err.Error(), "no login") {
		t.Fatalf("err = %v", err)
	}

	_ = conn.Close()
	if err := a.Sweep(context.Background()); err == nil {
		t.Fatal("closed database: want error")
	}
	if NewAttributor(conn).GH == nil {
		t.Fatal("default attributor has no gh")
	}
}

func TestAttributorIdleSweepCallsNothing(t *testing.T) {
	gh := &fakeGH{login: "me"}
	a := &Attributor{DB: testDB(t), GH: gh.run, Recheck: time.Hour, Batch: 10, now: time.Now}
	if err := a.Sweep(context.Background()); err != nil || gh.calls != 0 {
		t.Fatalf("idle sweep: err=%v calls=%d", err, gh.calls)
	}
}
