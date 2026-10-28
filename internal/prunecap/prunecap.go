// Package prunecap bounds how much a sync may delete because files vanished
// from a walk. A walk that suddenly misses many docs (a half-finished
// checkout, a sparse one, a moved mount) is more likely broken than a repo
// that lost them, so one prune of a repo prefix may remove at most half (at
// least 2) of what the last complete walk found stored there. A larger prune
// is refused and waits for the operator, who grants it for that prefix; the
// grant is used once, by that prefix's next prune past the cap and no larger
// than the one refused, expires after an hour, and is dropped by a walk that
// settles the prefix first.
package prunecap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GrantTTL is how long an operator's grant waits to be used.
const GrantTTL = time.Hour

// minAllowed is how many deletions one prune may always make.
const minAllowed = 2

// ErrNothingRefused means a confirmation found no refused prune to grant.
var ErrNothingRefused = errors.New("no prune is waiting for confirmation")

// Guard is the prune cap, kept in orch.db.
type Guard struct {
	db  *sql.DB
	now func() time.Time
}

// New returns a guard over db.
func New(db *sql.DB) *Guard { return &Guard{db: db, now: time.Now} }

// SourceKey and DocsKey name who confirms a prefix's refused prunes.
func SourceKey(id string) string  { return "source|" + id }
func DocsKey(owner string) string { return "docs|" + owner }

// Decide reports whether gone of the stored docs under prefix may be deleted
// now, and when not, why. The cap is measured against what the last complete
// walk left stored (Settled), or what is stored now when no walk has been
// recorded. Past the cap, an unexpired grant for exactly this prefix lets the
// prune through and is used up; otherwise the refusal is recorded under key,
// for Confirm to find.
func (g *Guard) Decide(ctx context.Context, key, prefix string, stored, gone int) (bool, string, error) {
	if gone <= 0 {
		return true, "", nil
	}
	baseline, refused := int64(stored), int64(0)
	err := g.db.QueryRowContext(ctx, `SELECT baseline, refused FROM prune_state WHERE prefix=?`, prefix).Scan(&baseline, &refused)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", err
	}
	if int64(gone) <= max(int64(minAllowed), baseline/2) {
		return true, "", nil
	}
	// A grant covers at most the prune the operator saw refused, and one
	// dated in the future (the clock moved back) is not used, so it cannot
	// outlive its hour.
	if int64(gone) <= refused {
		now := g.now()
		res, err := g.db.ExecContext(ctx, `DELETE FROM prune_confirm WHERE prefix=? AND at>=? AND at<=?`, prefix, now.Add(-GrantTTL).Unix(), now.Unix())
		if err != nil {
			return false, "", err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return true, "", nil
		}
	}
	if _, err := g.db.ExecContext(ctx, `INSERT INTO prune_state (prefix, key, baseline, refused) VALUES (?, ?, ?, ?)
		ON CONFLICT(prefix) DO UPDATE SET key=excluded.key, refused=excluded.refused`, prefix, key, baseline, gone); err != nil {
		return false, "", err
	}
	return false, fmt.Sprintf("%d of %d docs are gone, more than half; none were removed", gone, baseline), nil
}

// Settled records what a complete walk left stored under prefix, after any
// prune: the next prune is measured against it, nothing waits, and an unused
// grant is dropped so it cannot wait for a later, broken walk.
func (g *Guard) Settled(ctx context.Context, prefix string, count int) error {
	if _, err := g.db.ExecContext(ctx, `DELETE FROM prune_confirm WHERE prefix=?`, prefix); err != nil {
		return err
	}
	_, err := g.db.ExecContext(ctx, `INSERT INTO prune_state (prefix, baseline, refused) VALUES (?, ?, 0)
		ON CONFLICT(prefix) DO UPDATE SET baseline=excluded.baseline, refused=0`, prefix, count)
	return err
}

// Confirm grants, for an hour, each prefix whose prune was refused under key,
// and returns them. It grants nothing (ErrNothingRefused) when no prune is
// waiting, so a confirmation cannot lie in wait for a later, broken walk.
func (g *Guard) Confirm(ctx context.Context, key string) ([]string, error) {
	rows, err := g.db.QueryContext(ctx, `SELECT prefix FROM prune_state WHERE key=? AND refused>0 ORDER BY prefix`, key)
	if err != nil {
		return nil, err
	}
	var prefixes []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return nil, err
		}
		prefixes = append(prefixes, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		return nil, ErrNothingRefused
	}
	for _, p := range prefixes {
		if _, err := g.db.ExecContext(ctx, `INSERT INTO prune_confirm (prefix, at) VALUES (?, ?)
			ON CONFLICT(prefix) DO UPDATE SET at=excluded.at`, p, g.now().Unix()); err != nil {
			return nil, err
		}
	}
	return prefixes, nil
}
