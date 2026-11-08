package spaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Revision is one saved policy document.
type Revision struct {
	Number    int64
	Doc       []byte
	SHA256    string
	CreatedAt time.Time
	CreatedBy string
	// ByOperator reports that the operator saved it, not an agent session.
	ByOperator bool
}

var (
	// ErrStale is returned when the revision a caller reviewed is no longer
	// the newest: someone saved another draft since.
	ErrStale = errors.New("a newer spaces draft was saved since the one reviewed")
	// ErrNoRevision is returned for a revision number that was never saved.
	ErrNoRevision = errors.New("no such spaces revision")
	// ErrTampered is returned for a revision whose document no longer hashes
	// to what was recorded when it was saved or applied.
	ErrTampered = errors.New("spaces revision was changed outside orchd")
	// ErrAgentDraft is returned when applying a draft an agent session saved
	// without the operator confirming they reviewed it as such.
	ErrAgentDraft = errors.New("this spaces draft was saved by an agent session, not by you")
)

// querier reads the store: an *sql.DB, or an *sql.Tx so a write checks the
// snapshot it changes.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SaveDraft stores p as a new revision and returns it. Saving changes nothing
// that is enforced. A policy identical to the newest revision, saved by the
// same kind of caller, returns that revision rather than adding a copy.
func SaveDraft(ctx context.Context, db *sql.DB, p Policy, by string, byOperator bool, now time.Time) (Revision, error) {
	if err := checkSize(p); err != nil {
		return Revision{}, err
	}
	doc, err := Canonical(p)
	if err != nil {
		return Revision{}, err
	}
	if len(doc) > maxDocBytes {
		return Revision{}, fmt.Errorf("spaces policy is %d bytes, over the %d-byte limit", len(doc), maxDocBytes)
	}
	sum := Hash(doc)
	tx, err := beginWrite(ctx, db)
	if err != nil {
		return Revision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	latest, err := Latest(ctx, tx)
	if err != nil {
		return Revision{}, err
	}
	if latest != nil && latest.SHA256 == sum && Hash(latest.Doc) == sum && latest.ByOperator == byOperator {
		return *latest, nil
	}
	at := now.UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO spaces_revisions (doc, sha256, created_at, created_by, by_operator) VALUES (?, ?, ?, ?, ?)`,
		string(doc), sum, at, by, byOperator)
	if err != nil {
		return Revision{}, fmt.Errorf("save spaces draft: %w", err)
	}
	n, err := res.LastInsertId()
	if err != nil {
		return Revision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Revision{}, err
	}
	return Revision{Number: n, Doc: doc, SHA256: sum, CreatedAt: now.UTC().Truncate(time.Second), CreatedBy: by, ByOperator: byOperator}, nil
}

const revisionColumns = `revision, doc, sha256, created_at, created_by, by_operator`

// Latest returns the newest saved revision, or nil when none was saved.
func Latest(ctx context.Context, q querier) (*Revision, error) {
	return scanRevision(q.QueryRowContext(ctx,
		`SELECT `+revisionColumns+` FROM spaces_revisions ORDER BY revision DESC LIMIT 1`))
}

// Get returns revision n.
func Get(ctx context.Context, q querier, n int64) (*Revision, error) {
	r, err := scanRevision(q.QueryRowContext(ctx,
		`SELECT `+revisionColumns+` FROM spaces_revisions WHERE revision = ?`, n))
	if err == nil && r == nil {
		return nil, fmt.Errorf("%w: %d", ErrNoRevision, n)
	}
	return r, err
}

func scanRevision(row *sql.Row) (*Revision, error) {
	var r Revision
	var doc, at string
	switch err := row.Scan(&r.Number, &doc, &r.SHA256, &at, &r.CreatedBy, &r.ByOperator); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read spaces revision: %w", err)
	}
	r.Doc = []byte(doc)
	r.CreatedAt, _ = time.Parse(time.RFC3339, at)
	return &r, nil
}

// Applied is a revision orchd enforces.
type Applied struct {
	Revision Revision
	Resolved *Resolved
}

// verify parses and validates r, checking its document against both hashes
// recorded for it.
func verify(r *Revision, appliedSHA string, env Env) (*Resolved, error) {
	sum := Hash(r.Doc)
	if sum != r.SHA256 || (appliedSHA != "" && sum != appliedSHA) {
		return nil, fmt.Errorf("%w: revision %d", ErrTampered, r.Number)
	}
	p, err := Parse(r.Doc)
	if err != nil {
		return nil, err
	}
	return Validate(p, env)
}

// Apply makes revision reviewed the enforced one. It refuses with ErrStale
// when a newer revision was saved since, so the operator applies exactly what
// they reviewed; with ErrAgentDraft when an agent session saved it and
// acceptAgentDraft is false; and with Problems when it does not validate. On
// any error the applied revision is unchanged.
func Apply(ctx context.Context, db *sql.DB, reviewed int64, by string, acceptAgentDraft bool, now time.Time, env Env) (Applied, error) {
	tx, err := beginWrite(ctx, db)
	if err != nil {
		return Applied{}, err
	}
	defer func() { _ = tx.Rollback() }()
	latest, err := Latest(ctx, tx)
	if err != nil {
		return Applied{}, err
	}
	switch {
	case latest == nil, reviewed > latest.Number:
		return Applied{}, fmt.Errorf("%w: %d", ErrNoRevision, reviewed)
	case reviewed != latest.Number:
		return Applied{}, fmt.Errorf("%w (reviewed %d, newest %d)", ErrStale, reviewed, latest.Number)
	case !latest.ByOperator && !acceptAgentDraft:
		return Applied{}, fmt.Errorf("%w (%s)", ErrAgentDraft, latest.CreatedBy)
	}
	res, err := verify(latest, "", env)
	if err != nil {
		return Applied{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO spaces_applied (revision, sha256, applied_at, applied_by) VALUES (?, ?, ?, ?)`,
		latest.Number, latest.SHA256, now.UTC().Format(time.RFC3339), by); err != nil {
		return Applied{}, fmt.Errorf("apply spaces: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Applied{}, err
	}
	return Applied{Revision: *latest, Resolved: res}, nil
}

// appliedEntry is one row of spaces_applied.
type appliedEntry struct {
	revision int64
	sha256   string
}

// newestApplied returns the newest row of spaces_applied, or nil.
func newestApplied(ctx context.Context, q querier) (*appliedEntry, error) {
	var e appliedEntry
	err := q.QueryRowContext(ctx, `SELECT revision, sha256 FROM spaces_applied ORDER BY seq DESC LIMIT 1`).
		Scan(&e.revision, &e.sha256)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read applied spaces: %w", err)
	}
	return &e, nil
}

// LoadApplied returns the newest applied revision, for orchd's startup. With
// nothing ever applied it returns nil: no policy, the implicit Shared space.
// When the newest apply no longer verifies it returns an error wrapping
// ErrTampered, never an older apply: falling back would let anyone who can
// write orch.db choose which earlier policy is enforced. The caller must then
// fail closed. Only the hashes and the policy's structure are checked here;
// filesystem rules (a terms file in a git tree) were checked at apply, and a
// later change on disk must not undo an apply.
func LoadApplied(ctx context.Context, q querier, env Env) (*Applied, error) {
	e, err := newestApplied(ctx, q)
	if err != nil || e == nil {
		return nil, err
	}
	r, err := Get(ctx, q, e.revision)
	if err != nil {
		if errors.Is(err, ErrNoRevision) {
			return nil, fmt.Errorf("%w: applied revision %d is missing", ErrTampered, e.revision)
		}
		return nil, err
	}
	env.InGitTree = func(string) bool { return false }
	res, err := verify(r, e.sha256, env)
	switch {
	case errors.Is(err, ErrTampered):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: applied revision %d no longer validates: %v", ErrTampered, r.Number, err)
	}
	return &Applied{Revision: *r, Resolved: res}, nil
}

// beginWrite starts a transaction that holds SQLite's write lock from the
// start, so two writers cannot both read the same newest revision.
func beginWrite(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spaces_applied WHERE 0`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}
