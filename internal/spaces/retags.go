package spaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The operator's retags in orch.db (SP-071, D-14, migration 0031): one row
// per retagged source_uri, holding the space every later write of it goes to
// (the sticky override, Options.Override) and the state of the move of its
// stored chunks. A row is written as RetagMoving before the move starts and
// set to RetagDone after it finished, so a move an exit interrupted is
// resumed. Only orchd writes the table, for the operator's RetagSource.

// RetagState is how far a retag's move got.
type RetagState string

// The states of a retag's move.
const (
	RetagMoving RetagState = "moving"
	RetagDone   RetagState = "done"
)

// Retag is one stored retag.
type Retag struct {
	SourceURI string
	Space     string
	State     RetagState
	// HeldIn lists the spaces whose collections held the source when the
	// retag started.
	HeldIn []string
	// Confirmed is set when the operator confirmed the move (into a public
	// space, or out of an isolated space or quarantine).
	Confirmed bool
	At        time.Time
	By        string
}

// SaveRetag records that uri goes to space and that its move is starting,
// replacing an earlier retag of uri.
func (e *Engine) SaveRetag(ctx context.Context, uri, space string, heldIn []string, confirmed bool, by string) error {
	if uri == "" || space == "" {
		return errors.New("a retag needs a source and a space")
	}
	_, err := e.db.ExecContext(ctx, `INSERT INTO spaces_retags (source_uri, space, state, held_in, confirmed, at, by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_uri) DO UPDATE SET space = excluded.space, state = excluded.state,
			held_in = excluded.held_in, confirmed = excluded.confirmed, at = excluded.at, by = excluded.by`,
		uri, space, string(RetagMoving), strings.Join(heldIn, ","), confirmed, e.now().UTC().Format(time.RFC3339Nano), by)
	return err
}

// FinishRetag marks uri's move to space done. A retag of uri to another
// space since then is left as it is.
func (e *Engine) FinishRetag(ctx context.Context, uri, space string) error {
	_, err := e.db.ExecContext(ctx, `UPDATE spaces_retags SET state = ?, at = ? WHERE source_uri = ? AND space = ?`,
		string(RetagDone), e.now().UTC().Format(time.RFC3339Nano), uri, space)
	return err
}

// RetagOf returns the space uri was retagged to, or "" for none.
func (e *Engine) RetagOf(ctx context.Context, uri string) (string, error) {
	var space string
	err := e.db.QueryRowContext(ctx, `SELECT space FROM spaces_retags WHERE source_uri = ?`, uri).Scan(&space)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return space, err
}

// PendingRetags lists the retags whose move has not finished, oldest first.
func (e *Engine) PendingRetags(ctx context.Context) ([]Retag, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT source_uri, space, state, held_in, confirmed, at, by FROM spaces_retags
		WHERE state = ? ORDER BY at`, string(RetagMoving))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Retag
	for rows.Next() {
		var r Retag
		var state, heldIn, at string
		if err := rows.Scan(&r.SourceURI, &r.Space, &state, &heldIn, &r.Confirmed, &at, &r.By); err != nil {
			return nil, err
		}
		r.State = RetagState(state)
		if heldIn != "" {
			r.HeldIn = strings.Split(heldIn, ",")
		}
		if r.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("retag of a source: bad time %q", at)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
