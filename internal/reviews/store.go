// Package reviews watches the PRs the user picked for review. Once a PR is
// ready it dispatches a read-only review task, records the verdict that task
// returns, and posts to GitHub only what the user opted that PR in to.
package reviews

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// What orch may post for a PR once it has a verdict.
const (
	// ModeVerdict posts nothing: the verdict is for the user alone.
	ModeVerdict = "verdict"
	// ModeApprove approves the PR when the verdict is approve, with the
	// drafted comment as the review body.
	ModeApprove = "approve"
	// ModeComment posts the drafted comment once, as a plain review comment
	// whatever the verdict, then falls back to ModeVerdict.
	ModeComment = "comment"
	// ModeFeedback posts the drafted comment when the verdict is not approve,
	// once per reviewed commit, so the author can fix what it found; the next
	// push is reviewed again. It never approves.
	ModeFeedback = "feedback"
	// ModeAuto is ModeApprove and ModeFeedback together.
	ModeAuto = "auto"
)

// Where a watched PR's review stands.
const (
	StateWaiting   = "waiting"   // not ready yet, or ready and about to be reviewed
	StateHeld      = "held"      // ready, but its author is untrusted: waits for the user's go-ahead
	StateReviewing = "reviewing" // a review task is running
	StateVerdict   = "verdict"   // the verdict covers the PR's current head
	StateFailed    = "failed"    // the review task failed; a new head tries again
	StateClosed    = "closed"    // the PR was merged or closed
	StateStopped   = "stopped"   // the user stopped watching it
)

// Verdicts a review task returns.
const (
	VerdictApprove = "approve"
	VerdictWait    = "wait"
	VerdictChanges = "changes"
)

// What orch posted for the verdict's head.
const (
	PostedApproved  = "approved"
	PostedCommented = "commented"
	PostedFailed    = "failed"
)

// ValidMode reports whether m is a mode a client may set.
func ValidMode(m string) bool {
	return m == ModeVerdict || m == ModeApprove || m == ModeComment || m == ModeFeedback || m == ModeAuto
}

// approves and comments report whether a mode lets orch approve, or post the
// drafted comment.
func approves(m string) bool { return m == ModeApprove || m == ModeAuto }
func comments(m string) bool { return m == ModeComment || m == ModeFeedback || m == ModeAuto }

// Review is one watched PR.
type Review struct {
	URL    string
	Repo   string // owner/name
	Number int
	Mode   string
	State  string
	// HeadSHA is the commit the verdict covers; PendingSHA the one under review.
	HeadSHA, PendingSHA string
	TaskID              string
	// Nonce marks the review task's verdict block, so a block quoted from
	// the PR cannot pass for it.
	Nonce   string
	Verdict string
	Reason  string
	// Comment is the drafted review comment; the user may edit it.
	Comment string
	// Instructions are the user's own notes for this PR's reviews, added to
	// the task prompt.
	Instructions string
	// ForUser answers the user's notes and says what only the user needs to
	// know; Report is the full review. Neither is ever posted.
	ForUser, Report string
	// GoAhead lets the next ready commit be reviewed although its author is
	// not trusted; a review consumes it.
	GoAhead bool
	// Posted is what orch posted for HeadSHA, or empty.
	Posted string
	// Note is the last problem, or why nothing was posted.
	Note                            string
	CreatedAt, UpdatedAt, VerdictAt time.Time
}

// Active reports whether the watcher still works on the review.
func (r Review) Active() bool { return r.State != StateClosed && r.State != StateStopped }

var prURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9][A-Za-z0-9._-]{0,99})/pull/([1-9][0-9]{0,9})$`)

// ParseURL splits a PR URL into its repo and number. Only exactly
// https://github.com/<owner>/<repo>/pull/<n> is accepted, since the URL is
// passed to gh and into a task prompt.
func ParseURL(raw string) (repo string, number int, err error) {
	m := prURL.FindStringSubmatch(raw)
	if m == nil {
		return "", 0, fmt.Errorf("%q is not a GitHub pull request URL", raw)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", 0, err
	}
	return m[1] + "/" + m[2], n, nil
}

// Store keeps reviews in orch.db's pr_reviews table.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore builds a store over db.
func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

const columns = `url, repo, number, mode, state, head_sha, pending_sha, task_id, nonce, verdict, reason,
	comment, instructions, for_user, report, go_ahead, posted, note, created_at, updated_at, verdict_at`

func scan(row interface{ Scan(...any) error }) (Review, error) {
	var r Review
	var created, updated, verdict int64
	err := row.Scan(&r.URL, &r.Repo, &r.Number, &r.Mode, &r.State, &r.HeadSHA, &r.PendingSHA, &r.TaskID, &r.Nonce,
		&r.Verdict, &r.Reason, &r.Comment, &r.Instructions, &r.ForUser, &r.Report, &r.GoAhead, &r.Posted, &r.Note, &created, &updated, &verdict)
	r.CreatedAt, r.UpdatedAt, r.VerdictAt = unix(created), unix(updated), unix(verdict)
	return r, err
}

func unix(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

func seconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// List returns every review, the most recently changed first.
func (s *Store) List(ctx context.Context) ([]Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM pr_reviews ORDER BY updated_at DESC, url`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Review
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns the review of url, and whether there is one.
func (s *Store) Get(ctx context.Context, url string) (Review, bool, error) {
	r, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM pr_reviews WHERE url = ?`, url))
	if errors.Is(err, sql.ErrNoRows) {
		return Review{}, false, nil
	}
	return r, err == nil, err
}

// Put writes r, stamping its update time (and creation time when new).
func (s *Store) Put(ctx context.Context, r *Review) error {
	now := s.now().UTC().Truncate(time.Second)
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO pr_reviews (`+columns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET repo = excluded.repo, number = excluded.number, mode = excluded.mode,
			state = excluded.state, head_sha = excluded.head_sha, pending_sha = excluded.pending_sha,
			task_id = excluded.task_id, nonce = excluded.nonce, verdict = excluded.verdict, reason = excluded.reason,
			comment = excluded.comment, instructions = excluded.instructions, for_user = excluded.for_user, report = excluded.report, go_ahead = excluded.go_ahead, posted = excluded.posted, note = excluded.note,
			updated_at = excluded.updated_at, verdict_at = excluded.verdict_at`,
		r.URL, r.Repo, r.Number, r.Mode, r.State, r.HeadSHA, r.PendingSHA, r.TaskID, r.Nonce, r.Verdict, r.Reason,
		r.Comment, r.Instructions, r.ForUser, r.Report, r.GoAhead, r.Posted, r.Note, seconds(r.CreatedAt), seconds(r.UpdatedAt), seconds(r.VerdictAt))
	return err
}

// Delete removes the review of url; a missing one is not an error.
func (s *Store) Delete(ctx context.Context, url string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pr_reviews WHERE url = ?`, url)
	return err
}
