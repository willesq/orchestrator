// Package prunecap bounds how much a sync may delete because files vanished
// from a walk. A walk that suddenly misses many docs (a half-finished
// checkout, a sparse one, a moved mount) is more likely broken than a repo
// that lost them, so deletions under one prefix are capped at half of what
// was stored when a window began, counted across scans, so the cap cannot be
// walked down a half at a time. Past the cap, the sync reports it and waits
// for an operator to confirm.
package prunecap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Window is how long one baseline holds; after it a new one is taken.
const Window = 7 * 24 * time.Hour

// minAllowed is how many deletions are always allowed in a window.
const minAllowed = 2

// Guard is the prune budget, kept in orch.db.
type Guard struct {
	db  *sql.DB
	now func() time.Time
}

// New returns a guard over db.
func New(db *sql.DB) *Guard { return &Guard{db: db, now: time.Now} }

// SourceKey and DocsKey name what an operator confirms.
func SourceKey(id string) string  { return "source|" + id }
func DocsKey(owner string) string { return "docs|" + owner }

// Confirm records that the next prune under key may exceed the cap.
func (g *Guard) Confirm(ctx context.Context, key string) error {
	_, err := g.db.ExecContext(ctx, `INSERT INTO prune_confirm (key, at) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET at=excluded.at`, key, g.now().Unix())
	return err
}

// Allow reports whether gone deletions under prefix may proceed, given how
// many docs are stored there now, and when not, why. A confirmation for key
// lets one prune through and starts a new window.
func (g *Guard) Allow(ctx context.Context, key, prefix string, stored, gone int) (bool, string, error) {
	if gone == 0 {
		return true, "", nil
	}
	now := g.now()
	var baseline, pruned, since int64
	err := g.db.QueryRowContext(ctx, `SELECT baseline, pruned, since FROM prune_state WHERE prefix=?`, prefix).Scan(&baseline, &pruned, &since)
	if errors.Is(err, sql.ErrNoRows) || err == nil && now.Sub(time.Unix(since, 0)) > Window {
		baseline, pruned, since = int64(stored), 0, now.Unix()
		if _, err := g.db.ExecContext(ctx, `INSERT INTO prune_state (prefix, baseline, pruned, since) VALUES (?, ?, 0, ?)
			ON CONFLICT(prefix) DO UPDATE SET baseline=excluded.baseline, pruned=0, since=excluded.since`, prefix, baseline, since); err != nil {
			return false, "", err
		}
	} else if err != nil {
		return false, "", err
	}
	res, err := g.db.ExecContext(ctx, `DELETE FROM prune_confirm WHERE key=?`, key)
	if err != nil {
		return false, "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// Confirmed: this prune goes through, and the next window is measured
		// from what remains.
		_, err := g.db.ExecContext(ctx, `UPDATE prune_state SET baseline=?, pruned=0, since=? WHERE prefix=?`,
			stored-gone, now.Unix(), prefix)
		return err == nil, "", err
	}
	limit := max(int64(minAllowed), baseline/2)
	if pruned+int64(gone) <= limit {
		return true, "", nil
	}
	return false, fmt.Sprintf("%d docs are gone, %d removed since %s, of %d stored then; more than half, so none were removed",
		gone, pruned, time.Unix(since, 0).UTC().Format("2006-01-02"), baseline), nil
}

// Done records n deletions made under prefix.
func (g *Guard) Done(ctx context.Context, prefix string, n int) error {
	if n == 0 {
		return nil
	}
	_, err := g.db.ExecContext(ctx, `UPDATE prune_state SET pruned=pruned+? WHERE prefix=?`, n, prefix)
	return err
}
