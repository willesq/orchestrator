// Package prunecap bounds how much a sync may delete because files vanished
// from a walk. A walk that suddenly misses many docs (a half-finished
// checkout, a sparse one, a moved mount) is more likely broken than a repo
// that lost them.
//
// Each repo prefix has a baseline: what was stored at its last confirmed
// prune, or at a clean walk (nothing gone) with no unconfirmed prune still
// counted against it. Prunes without a confirmation may remove,
// together, at most half (at least 2) of it, and one prune at most half of
// what is stored now, so the cap cannot be walked down a half at a time; a
// legitimate gradual shrink asks for one confirmation now and then. Past the
// cap the prune is refused and waits for the operator, who grants it for that
// prefix. A grant covers that refusal only: a later, larger refusal drops it,
// it is used once, by that prefix's next prune past the cap, and it expires
// after an hour. A clean walk clears a refusal and its grant.
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

// minAllowed is how many deletions the cap always allows.
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

// state is one prefix's row.
type state struct {
	baseline, removed, refused int64
}

func (g *Guard) load(ctx context.Context, prefix string, stored int) (state, error) {
	st := state{baseline: int64(stored)}
	err := g.db.QueryRowContext(ctx, `SELECT baseline, removed, refused FROM prune_state WHERE prefix=?`, prefix).
		Scan(&st.baseline, &st.removed, &st.refused)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return st, err
}

func (g *Guard) save(ctx context.Context, key, prefix string, st state, refusedAt int64) error {
	_, err := g.db.ExecContext(ctx, `INSERT INTO prune_state (prefix, key, baseline, removed, refused, refused_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(prefix) DO UPDATE SET key=excluded.key, baseline=excluded.baseline, removed=excluded.removed,
		refused=excluded.refused, refused_at=excluded.refused_at`, prefix, key, st.baseline, st.removed, st.refused, refusedAt)
	return err
}

func (g *Guard) dropGrant(ctx context.Context, prefix string) error {
	_, err := g.db.ExecContext(ctx, `DELETE FROM prune_confirm WHERE prefix=?`, prefix)
	return err
}

// Decide reports whether gone of the stored docs under prefix may be deleted
// now, and when not, why; an allowed prune is recorded as made. Past the cap,
// an unexpired grant for exactly this prefix, issued for a refusal at least
// this large, lets the prune through, is used up and resets the baseline;
// otherwise any grant is dropped and the refusal is recorded under key, for
// Confirm to find.
func (g *Guard) Decide(ctx context.Context, key, prefix string, stored, gone int) (bool, string, error) {
	if gone <= 0 {
		return true, "", nil
	}
	st, err := g.load(ctx, prefix, stored)
	if err != nil {
		return false, "", err
	}
	// Docs added since the baseline raise it: growth is what is stored, not
	// what a walk claims.
	st.baseline = max(st.baseline, int64(stored))
	if st.removed+int64(gone) <= max(minAllowed, st.baseline/2) && int64(gone) <= max(minAllowed, int64(stored)/2) {
		st.removed += int64(gone)
		st.refused = 0
		if err := g.dropGrant(ctx, prefix); err != nil {
			return false, "", err
		}
		return true, "", g.save(ctx, key, prefix, st, 0)
	}
	now := g.now()
	var at, granted int64
	scanErr := g.db.QueryRowContext(ctx, `SELECT at, refused FROM prune_confirm WHERE prefix=?`, prefix).Scan(&at, &granted)
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		return false, "", scanErr
	}
	// Used or not, the grant goes: one prune past the cap ends it.
	if err := g.dropGrant(ctx, prefix); err != nil {
		return false, "", err
	}
	// A grant dated in the future (the clock moved back) is not used, so it
	// cannot outlive its hour; one for a smaller refusal does not cover this.
	if scanErr == nil && at >= now.Add(-GrantTTL).Unix() && at <= now.Unix() && int64(gone) <= granted {
		return true, "", g.save(ctx, key, prefix, state{baseline: int64(max(stored-gone, 0))}, 0)
	}
	st.refused = int64(gone)
	if err := g.save(ctx, key, prefix, st, now.Unix()); err != nil {
		return false, "", err
	}
	if st.removed > 0 {
		return false, fmt.Sprintf("%d docs are gone, on top of %d removed since the last full walk of %d, more than half; none were removed",
			gone, st.removed, st.baseline), nil
	}
	return false, fmt.Sprintf("%d of %d docs are gone, more than half; none were removed", gone, st.baseline), nil
}

// Settled records a clean walk of prefix, one that found nothing gone, and
// what it left stored: nothing waits, and an unused grant is dropped so it
// cannot wait for a later, broken walk. The baseline becomes count unless
// unconfirmed prunes are still counted against it and the repo has not grown
// back: the walk after a prune finds nothing more gone, and resetting there
// would let a broken tree lose half again on every other refresh.
func (g *Guard) Settled(ctx context.Context, prefix string, count int) error {
	if err := g.dropGrant(ctx, prefix); err != nil {
		return err
	}
	_, err := g.db.ExecContext(ctx, `INSERT INTO prune_state (prefix, baseline) VALUES (?, ?)
		ON CONFLICT(prefix) DO UPDATE SET refused=0, refused_at=0,
		baseline=CASE WHEN removed=0 OR excluded.baseline>=baseline THEN excluded.baseline ELSE baseline END,
		removed=CASE WHEN removed=0 OR excluded.baseline>=baseline THEN 0 ELSE removed END`, prefix, count)
	return err
}

// Forget clears the refusals under key whose prefix is not in current: the
// latest complete walk of that key did not see them refused again (the repo
// was moved, removed, skipped or recovered), so there is nothing left for the
// operator to confirm there.
func (g *Guard) Forget(ctx context.Context, key string, current []string) error {
	refused, err := g.refusedUnder(ctx, key)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, p := range current {
		keep[p] = true
	}
	for _, p := range refused {
		if keep[p] {
			continue
		}
		if err := g.dropGrant(ctx, p); err != nil {
			return err
		}
		if _, err := g.db.ExecContext(ctx, `UPDATE prune_state SET refused=0, refused_at=0 WHERE prefix=?`, p); err != nil {
			return err
		}
	}
	return nil
}

func (g *Guard) refusedUnder(ctx context.Context, key string) ([]string, error) {
	rows, err := g.db.QueryContext(ctx, `SELECT prefix FROM prune_state WHERE key=? AND refused>0 ORDER BY prefix`, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Confirm grants, for an hour, each prefix whose prune is refused under key,
// for that refusal's size, and returns them. It grants nothing
// (ErrNothingRefused) when no prune is waiting, so a confirmation cannot lie
// in wait for a later, broken walk.
func (g *Guard) Confirm(ctx context.Context, key string) ([]string, error) {
	prefixes, err := g.refusedUnder(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		return nil, ErrNothingRefused
	}
	for _, p := range prefixes {
		if _, err := g.db.ExecContext(ctx, `INSERT INTO prune_confirm (prefix, at, refused)
			SELECT prefix, ?, refused FROM prune_state WHERE prefix=? AND true
			ON CONFLICT(prefix) DO UPDATE SET at=excluded.at, refused=excluded.refused`, g.now().Unix(), p); err != nil {
			return nil, err
		}
	}
	return prefixes, nil
}
