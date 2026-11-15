// Package evidence labels finished tasks with outcomes routing can learn from:
// the user's own rating, and the fate of the pull requests a task opened.
package evidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Outcome labels.
const (
	Good   = "good"
	Rework = "rework"
	Bad    = "bad"
)

// ValidOutcome reports whether s is an outcome label.
func ValidOutcome(s string) bool { return s == Good || s == Rework || s == Bad }

// ErrNoTask is returned when rating a task that does not exist.
var ErrNoTask = errors.New("no such task")

// Rate records the user's verdict on a finished task. An unfinished task cannot
// be rated: its outcome is not known yet.
func Rate(ctx context.Context, db *sql.DB, id, rating string, now int64) error {
	if !ValidOutcome(rating) {
		return fmt.Errorf("rating must be %s, %s or %s", Good, Rework, Bad)
	}
	var status string
	err := db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNoTask, id)
	}
	if err != nil {
		return err
	}
	switch status {
	case "complete", "failed", "blocked":
	default:
		return fmt.Errorf("task %s is %s; rate it once it has finished", id, status)
	}
	_, err = db.ExecContext(ctx, `UPDATE tasks SET rating = ?, rated_at = ? WHERE id = ?`, rating, now, id)
	return err
}

// GH runs the gh CLI; a seam for tests.
type GH func(ctx context.Context, args ...string) ([]byte, error)

func realGH(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "gh", args...).Output()
}

// Limits on what one sweep may cost. A session's PR list is text it wrote, so
// these keep a long or hostile list from spending the user's GitHub rate limit.
const (
	maxPRsPerTask = 10
	callBudget    = 50
	callTimeout   = 30 * time.Second
	// maxAge stops re-checking a task whose PRs never settle.
	maxAge = 30 * 24 * time.Hour
)

// prURL is the only PR form attributed; anything else is ignored.
var prURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+$`)

// Attributor labels routed tasks from the PRs they opened.
type Attributor struct {
	DB *sql.DB
	GH GH
	// Recheck is how long an undecided task waits before its PRs are looked at
	// again, so a long-open PR costs one gh call per interval, not per sweep.
	Recheck time.Duration
	// Batch caps how many tasks one sweep checks.
	Batch int
	// LoginFor, when set, names the GitHub account whose PRs count for a PR
	// under owner: the login of the gh account that owner's space uses
	// (SP-074). nil counts the default login's PRs ('gh api user').
	LoginFor func(ctx context.Context, owner string) (string, error)
	now      func() time.Time
}

// NewAttributor builds an attributor over orch.db using the gh CLI.
func NewAttributor(db *sql.DB) *Attributor {
	return &Attributor{DB: db, GH: realGH, Recheck: 6 * time.Hour, Batch: 20, now: time.Now}
}

type pending struct {
	id, prs  string
	created  time.Time
	finished time.Time
}

// sweep is one pass's shared state: who the user is for each PR owner, and
// the calls left.
type sweep struct {
	a      *Attributor
	logins map[string]string
	budget int
}

// login returns the account whose PRs count under owner, asking once per
// owner each sweep.
func (w *sweep) login(ctx context.Context, owner string) (string, error) {
	key := strings.ToLower(owner)
	if w.a.LoginFor == nil {
		key = ""
	}
	if l, ok := w.logins[key]; ok {
		return l, nil
	}
	var l string
	if w.a.LoginFor != nil {
		var err error
		if l, err = w.a.LoginFor(ctx, owner); err != nil {
			return "", err
		}
	} else {
		out, err := w.gh(ctx, "api", "user", "--jq", ".login")
		if err != nil {
			return "", fmt.Errorf("gh api user: %w", err)
		}
		if l = strings.TrimSpace(string(out)); l == "" {
			return "", errors.New("gh api user: no login")
		}
	}
	if l == "" {
		return "", errors.New("no GitHub login")
	}
	w.logins[key] = l
	return l, nil
}

func (w *sweep) gh(ctx context.Context, args ...string) ([]byte, error) {
	if w.budget <= 0 {
		return nil, errBudget
	}
	w.budget--
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return w.a.GH(ctx, args...)
}

var errBudget = errors.New("gh call budget for this sweep is spent")

// Sweep checks routed, finished tasks with PRs and no PR outcome yet. A PR
// counts only if the user opened it after the task was dispatched: the URLs
// come from text the session wrote, and must not let it claim someone else's
// merged work. A task is labeled once every PR it opened has closed, and the
// worst PR decides.
func (a *Attributor) Sweep(ctx context.Context) error {
	now := a.now()
	rows, err := a.DB.QueryContext(ctx, `
		SELECT id, prs, created_at, updated_at FROM tasks
		WHERE status = 'complete' AND COALESCE(task_kind,'') <> '' AND COALESCE(prs,'') <> ''
		  AND COALESCE(created_at,0) > ? AND COALESCE(pr_outcome,'') = '' AND COALESCE(pr_checked_at,0) <= ?
		ORDER BY updated_at LIMIT ?`, now.Add(-maxAge).Unix(), now.Add(-a.Recheck).Unix(), a.Batch)
	if err != nil {
		return err
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var created int64
		var updated string
		if err := rows.Scan(&p.id, &p.prs, &created, &updated); err != nil {
			_ = rows.Close()
			return err
		}
		p.created = time.Unix(created, 0)
		// SQLite datetime('now') is UTC "YYYY-MM-DD HH:MM:SS". Without a finish
		// time a later commit cannot be told from an earlier one, so skip it.
		if p.finished, err = time.Parse(time.DateTime, updated); err != nil {
			continue
		}
		todo = append(todo, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}
	w := &sweep{a: a, budget: callBudget, logins: map[string]string{}}
	if a.LoginFor == nil {
		// Reason: one account for every PR; failing here leaves every task
		// unchecked rather than recorded as checked.
		if _, err := w.login(ctx, ""); err != nil {
			return err
		}
	}
	var errs []error
	for _, p := range todo {
		outcome, err := w.judge(ctx, p)
		if errors.Is(err, errBudget) {
			break // leave the rest unchecked for the next sweep
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", p.id, err))
		}
		// Record the check either way, so a failing or still-open PR is not
		// queried on every sweep.
		if _, err := a.DB.ExecContext(ctx, `UPDATE tasks SET pr_outcome = ?, pr_checked_at = ? WHERE id = ?`,
			outcome, now.Unix(), p.id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var severity = map[string]int{Good: 0, Rework: 1, Bad: 2}

// judge returns the worst outcome across the task's own PRs, or "" while any is
// open, none qualifies, or the list is too long to be a real task's.
func (w *sweep) judge(ctx context.Context, p pending) (string, error) {
	var urls []string
	for _, u := range strings.Split(p.prs, ",") {
		if prURL.MatchString(u) {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 || len(urls) > maxPRsPerTask {
		return "", nil
	}
	worst, owned := Good, 0
	for _, u := range urls {
		o, mine, err := w.prOutcome(ctx, u, p)
		if err != nil {
			return "", err
		}
		if !mine {
			continue
		}
		if o == "" {
			return "", nil
		}
		owned++
		if severity[o] > severity[worst] {
			worst = o
		}
	}
	if owned == 0 {
		return "", nil
	}
	return worst, nil
}

// prOutcome reads one PR. mine is false for a PR the task cannot have opened:
// someone else's, or one created before the task was dispatched. For its own
// PRs: merged as the task left it is good, merged after more authored commits
// is rework, closed unmerged is bad, and open is undecided. Merge commits (such
// as GitHub's "Update branch") and rebases, which re-stamp commit times but not
// authored ones, do not count as rework.
func (w *sweep) prOutcome(ctx context.Context, url string, p pending) (outcome string, mine bool, err error) {
	out, err := w.gh(ctx, "pr", "view", url, "--json", "state,createdAt,author,commits")
	if err != nil {
		if errors.Is(err, errBudget) {
			return "", false, err
		}
		return "", false, fmt.Errorf("gh pr view %s: %w", url, err)
	}
	var v struct {
		State     string    `json:"state"`
		CreatedAt time.Time `json:"createdAt"`
		Author    struct {
			Login string `json:"login"`
		} `json:"author"`
		Commits []struct {
			AuthoredDate    time.Time `json:"authoredDate"`
			MessageHeadline string    `json:"messageHeadline"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", false, fmt.Errorf("gh pr view %s: unreadable output", url)
	}
	// The PR counts only as the account its owner's space uses; a profile
	// it cannot reach is an error, never another login's answer.
	owner, _, _ := strings.Cut(strings.TrimPrefix(url, "https://github.com/"), "/")
	login, err := w.login(ctx, owner)
	if err != nil {
		return "", false, fmt.Errorf("the GitHub login for %s: %w", url, err)
	}
	if !strings.EqualFold(v.Author.Login, login) || v.CreatedAt.Before(p.created) {
		return "", false, nil
	}
	switch v.State {
	case "MERGED":
		for _, c := range v.Commits {
			if c.AuthoredDate.After(p.finished) && !strings.HasPrefix(c.MessageHeadline, "Merge ") {
				return Rework, true, nil
			}
		}
		return Good, true, nil
	case "CLOSED":
		return Bad, true, nil
	}
	return "", true, nil
}
