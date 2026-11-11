package spaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for ReadEnforcement
)

// orchd holds the switches in memory while it runs and writes each change
// to orch.db: the row in spaces_enforcement and the same values appended to
// spaces_enforcement_log, in one transaction. At startup a row must match
// its layer's newest log entry (LoadVerified), so an edit to one table alone
// changes nothing; while orchd runs, its own copy wins over orch.db.

// LoadEnforcement reads every layer's switch from orch.db as stored. A layer
// never switched is at its default. A row with a state its layer does not
// have, or for an unknown layer, is skipped (the layer stays at its
// default); one whose timer cannot be read is taken as expired.
func LoadEnforcement(ctx context.Context, q querier) (Enforcement, error) {
	rows, err := q.QueryContext(ctx, `SELECT layer, state, set_at, set_by, until, previous FROM spaces_enforcement`)
	if err != nil {
		return nil, fmt.Errorf("read enforcement switches: %w", err)
	}
	defer rows.Close()
	e := DefaultEnforcement()
	for rows.Next() {
		var layer, state, setAt, setBy, previous string
		var until sql.NullString
		if err := rows.Scan(&layer, &state, &setAt, &setBy, &until, &previous); err != nil {
			return nil, fmt.Errorf("read enforcement switches: %w", err)
		}
		l, err := ParseLayer(layer)
		if err != nil || l.CheckState(state) != nil {
			continue
		}
		s := Switch{Layer: l, State: state, SetBy: setBy, Previous: previous}
		var perr error
		if s.SetAt, perr = time.Parse(time.RFC3339, setAt); perr != nil || s.SetAt.IsZero() {
			// Reason: a row orchd wrote always has its time; one without
			// must not read as never switched, which LoadVerified trusts.
			s.SetAt = time.Unix(1, 0).UTC()
		}
		if until.Valid {
			if s.Until, perr = time.Parse(time.RFC3339, until.String); perr != nil || s.Until.IsZero() {
				// Reason: a timer that cannot be read must not make a
				// relaxed switch permanent; it reads as already expired.
				s.Until = time.Unix(1, 0).UTC()
			}
			if l.CheckState(previous) != nil {
				// Reason: a timed switch must have something to revert to;
				// the default is the enforcing choice.
				s.Previous = l.Default()
			}
		}
		e[l] = s
	}
	return e, rows.Err()
}

// LoadVerified is LoadEnforcement for orchd's startup: a layer whose row
// does not match its newest change-log entry (a row written, or a log entry
// appended, outside orchd) is at its default, and problems names it. A
// layer with log entries but no row is at its default too.
func LoadVerified(ctx context.Context, q querier) (Enforcement, []string, error) {
	e, err := LoadEnforcement(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	logged, err := newestChanges(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	var problems []string
	for _, l := range Layers {
		s := e[l]
		c, ok := logged[l]
		switch {
		case s.SetAt.IsZero() && !ok:
			continue
		case ok && c.State == s.State && c.By == s.SetBy && c.At.Equal(s.SetAt) && c.Until.Equal(s.Until) &&
			(s.Until.IsZero() || c.RevertsTo == s.Previous):
			continue
		}
		problems = append(problems, fmt.Sprintf("the %s switch in orch.db does not match its change log", l))
		e[l] = Switch{Layer: l, State: l.Default()}
	}
	return e, problems, nil
}

// newestChanges returns each layer's newest change-log entry.
func newestChanges(ctx context.Context, q querier) (map[Layer]SwitchChange, error) {
	rows, err := q.QueryContext(ctx, `SELECT at, layer, state, previous, by, until, reverts_to, expired FROM spaces_enforcement_log
		WHERE seq IN (SELECT MAX(seq) FROM spaces_enforcement_log GROUP BY layer)`)
	if err != nil {
		return nil, fmt.Errorf("read enforcement log: %w", err)
	}
	out := map[Layer]SwitchChange{}
	cs, err := scanChanges(rows)
	for _, c := range cs {
		out[c.Layer] = c
	}
	return out, err
}

// NextSwitch is what setting layer l to state does to cur at now, as set by
// by: the change to record and the layer's new switch. A positive forDur
// makes it a timed switch that reverts at now+forDur to the state in force
// before it; a timed switch set over another keeps the first one's revert
// state, so a chain of timed switches ends where it began. An untimed set
// replaces any timer. Setting the state a layer already has, untimed and
// with no timer running, changes nothing: ok is false.
func NextSwitch(cur Enforcement, l Layer, state, by string, forDur time.Duration, now time.Time) (SwitchChange, Switch, bool, error) {
	if _, err := ParseLayer(string(l)); err != nil {
		return SwitchChange{}, Switch{}, false, err
	}
	if err := l.CheckState(state); err != nil {
		return SwitchChange{}, Switch{}, false, err
	}
	if forDur < 0 || forDur > MaxSwitchFor {
		return SwitchChange{}, Switch{}, false, ErrBadDuration
	}
	old, ok := cur[l]
	if !ok || old.State == "" {
		old = Switch{Layer: l, State: l.Default()}
	}
	if forDur == 0 && old.State == state && old.Until.IsZero() {
		return SwitchChange{}, old, false, nil
	}
	now = now.UTC().Truncate(time.Second)
	c := SwitchChange{Layer: l, State: state, Previous: old.State, At: now, By: by}
	sw := Switch{Layer: l, State: state, SetAt: now, SetBy: by}
	if forDur > 0 {
		c.Until = now.Add(forDur)
		sw.Until, sw.Previous = c.Until, old.State
		if !old.Until.IsZero() {
			sw.Previous = old.Previous
		}
		c.RevertsTo = sw.Previous
	}
	return c, sw, true, nil
}

// Expired returns the reverts of every timed switch in cur whose time is up
// at now, each with the layer's switch after it.
func Expired(cur Enforcement, now time.Time) ([]SwitchChange, []Switch) {
	var cs []SwitchChange
	var sws []Switch
	at := now.UTC().Truncate(time.Second)
	for _, l := range Layers {
		s, ok := cur[l]
		if !ok || s.Until.IsZero() || now.Before(s.Until) {
			continue
		}
		cs = append(cs, SwitchChange{Layer: l, State: s.Previous, Previous: s.State, At: at, By: "timer", Expired: true})
		sws = append(sws, Switch{Layer: l, State: s.Previous, SetAt: at, SetBy: "timer"})
	}
	return cs, sws
}

// WriteSwitch stores sw as its layer's switch and appends c, the change
// that made it, to the change log, in one transaction.
func WriteSwitch(ctx context.Context, db *sql.DB, c SwitchChange, sw Switch) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var until any
	if !sw.Until.IsZero() {
		until = sw.Until.UTC().Format(time.RFC3339)
	}
	at := sw.SetAt.UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO spaces_enforcement (layer, state, set_at, set_by, until, previous) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(layer) DO UPDATE SET state = excluded.state, set_at = excluded.set_at, set_by = excluded.set_by,
		   until = excluded.until, previous = excluded.previous`,
		string(sw.Layer), sw.State, at, sw.SetBy, until, sw.Previous); err != nil {
		return fmt.Errorf("set enforcement switch: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO spaces_enforcement_log (at, layer, state, previous, by, until, reverts_to, expired) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		at, string(c.Layer), c.State, c.Previous, c.By, until, sw.Previous, c.Expired); err != nil {
		return fmt.Errorf("log enforcement switch: %w", err)
	}
	return tx.Commit()
}

// SameSwitches reports whether a and b hold the same switches.
func SameSwitches(a, b Enforcement) bool {
	for _, l := range Layers {
		x, y := a[l], b[l]
		if x.State != y.State || x.SetBy != y.SetBy || !x.SetAt.Equal(y.SetAt) || !x.Until.Equal(y.Until) ||
			(!x.Until.IsZero() && x.Previous != y.Previous) {
			return false
		}
	}
	return true
}

// SwitchHistory returns the newest limit changes, newest first.
func SwitchHistory(ctx context.Context, q querier, limit int) ([]SwitchChange, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT at, layer, state, previous, by, until, reverts_to, expired FROM spaces_enforcement_log ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read enforcement log: %w", err)
	}
	return scanChanges(rows)
}

func scanChanges(rows *sql.Rows) ([]SwitchChange, error) {
	defer rows.Close()
	var out []SwitchChange
	for rows.Next() {
		var at, layer string
		var until sql.NullString
		var c SwitchChange
		if err := rows.Scan(&at, &layer, &c.State, &c.Previous, &c.By, &until, &c.RevertsTo, &c.Expired); err != nil {
			return nil, fmt.Errorf("read enforcement log: %w", err)
		}
		c.Layer = Layer(layer)
		c.At, _ = time.Parse(time.RFC3339, at)
		if until.Valid {
			c.Until, _ = time.Parse(time.RFC3339, until.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReadEnforcement reads the switches from the orch.db at dbPath read-only
// and without migrating it, for orch install and the hooks, which run
// whether or not orchd does. A missing store, or one that predates the
// switches, holds none, so every layer is at its default (enforcing). A
// layer that does not match its change log is at its default, and the
// error names it alongside the switches.
func ReadEnforcement(ctx context.Context, dbPath string) (Enforcement, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return DefaultEnforcement(), nil
	}
	// Reason: built as a URL, so a data directory with ?, # or % in its
	// path cannot turn into connection options (and undo mode=ro).
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer conn.Close()
	e, problems, err := LoadVerified(ctx, conn)
	switch {
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return DefaultEnforcement(), nil
	case err != nil:
		return nil, err
	case len(problems) > 0:
		return e, errors.New(strings.Join(problems, "; "))
	}
	return e, nil
}
