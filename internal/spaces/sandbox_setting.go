package spaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The sandbox setting (spaces plan D-61). Whether orch writes its sandbox
// rules is the operator's lasting choice, apart from the sandbox layer's
// SP-070 switch, which stays for debugging:
//
//   - auto (the default): rules only for a session that another isolated
//     space's folders would be denied to;
//   - on: rules for every session while a policy is applied;
//   - off: no rules, by choice.
//
// Unlike a switch, the setting never marks start digests (INV-16 as amended
// by D-61); orch status and the app show it. It is stored in orch.db beside
// the switches, under its own key, in the same row-plus-change-log form, so
// every change is logged with its time and caller and an edit to one table
// alone is found.

// SandboxMode is the sandbox setting's value.
type SandboxMode string

// The sandbox setting's values.
const (
	SandboxAuto SandboxMode = "auto"
	SandboxOn   SandboxMode = "on"
	SandboxOff  SandboxMode = "off"
)

// SandboxModes lists the values, the default first.
var SandboxModes = []SandboxMode{SandboxAuto, SandboxOn, SandboxOff}

// sandboxSettingKey is the setting's row in spaces_enforcement and its
// entries in spaces_enforcement_log. Reason for a key that is not a layer:
// LoadEnforcement and the hooks' ReadEnforcement skip rows of unknown
// layers, so the setting can never read as a switch and mark a digest.
const sandboxSettingKey = "sandbox-setting"

// ErrBadSandboxMode is returned for a value that is not one of SandboxModes.
var ErrBadSandboxMode = errors.New("not a sandbox setting")

// ParseSandboxMode returns the setting named s.
func ParseSandboxMode(s string) (SandboxMode, error) {
	m := SandboxMode(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range SandboxModes {
		if v == m {
			return m, nil
		}
	}
	return "", fmt.Errorf("%w %q (one of auto, on, off)", ErrBadSandboxMode, s)
}

// SandboxSetting is the setting as stored: SetAt is zero when the operator
// never set it.
type SandboxSetting struct {
	Mode  SandboxMode
	SetAt time.Time
	SetBy string
}

// DefaultSandboxSetting is the setting never set: auto.
func DefaultSandboxSetting() SandboxSetting { return SandboxSetting{Mode: SandboxAuto} }

// LoadSandboxSetting reads the setting from orch.db and checks it against
// its newest change-log entry, as LoadVerified does for the switches. A row
// that does not match, holds an unknown value, or a log entry with no row
// makes it on, the strictest value, and problem says why. Reason: as a
// switch that does not verify enforces, so does a setting; auto could be
// weaker than what the operator chose.
func LoadSandboxSetting(ctx context.Context, q querier) (s SandboxSetting, problem string, err error) {
	s = DefaultSandboxSetting()
	var state, setAt, setBy string
	err = q.QueryRowContext(ctx, `SELECT state, set_at, set_by FROM spaces_enforcement WHERE layer = ?`, sandboxSettingKey).
		Scan(&state, &setAt, &setBy)
	row := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, "", fmt.Errorf("read the sandbox setting: %w", err)
	}
	var lState, lAt, lBy string
	err = q.QueryRowContext(ctx, `SELECT state, at, by FROM spaces_enforcement_log WHERE layer = ? ORDER BY seq DESC LIMIT 1`,
		sandboxSettingKey).Scan(&lState, &lAt, &lBy)
	logged := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, "", fmt.Errorf("read the sandbox setting's log: %w", err)
	}
	if !row && !logged {
		return s, "", nil
	}
	mode, perr := ParseSandboxMode(state)
	at, terr := time.Parse(time.RFC3339, setAt)
	if !row || !logged || perr != nil || terr != nil || state != lState || setAt != lAt || setBy != lBy {
		return SandboxSetting{Mode: SandboxOn}, "the sandbox setting in orch.db does not match its change log; orch writes the sandbox rules (on) until the operator sets it", nil
	}
	return SandboxSetting{Mode: mode, SetAt: at, SetBy: setBy}, "", nil
}

// NextSandboxSetting is what setting mode does to cur at now, as set by by:
// the change to record and the new setting. ok is false when mode is the
// setting already (nothing to record).
func NextSandboxSetting(cur SandboxSetting, mode SandboxMode, by string, now time.Time) (SwitchChange, SandboxSetting, bool, error) {
	if _, err := ParseSandboxMode(string(mode)); err != nil {
		return SwitchChange{}, SandboxSetting{}, false, err
	}
	if cur.Mode == "" {
		cur.Mode = SandboxAuto
	}
	if cur.Mode == mode {
		return SwitchChange{}, cur, false, nil
	}
	now = now.UTC().Truncate(time.Second)
	c := SwitchChange{Layer: sandboxSettingKey, State: string(mode), Previous: string(cur.Mode), At: now, By: by}
	return c, SandboxSetting{Mode: mode, SetAt: now, SetBy: by}, true, nil
}

// WriteSandboxSetting stores s and appends c, the change that made it, to
// the change log, in one transaction.
func WriteSandboxSetting(ctx context.Context, db *sql.DB, c SwitchChange, s SandboxSetting) error {
	return WriteSwitch(ctx, db, c, Switch{Layer: sandboxSettingKey, State: string(s.Mode), SetAt: s.SetAt, SetBy: s.SetBy})
}

// DescribeSandboxChange is a setting change in one line, for orchd's log
// and event stream.
func DescribeSandboxChange(c SwitchChange) string {
	return fmt.Sprintf("the sandbox setting changed from %s to %s by %s", c.Previous, c.State, c.By)
}

// SandboxSettingHistory returns the newest limit setting changes, newest
// first.
func SandboxSettingHistory(ctx context.Context, q querier, limit int) ([]SwitchChange, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT at, layer, state, previous, by, until, reverts_to, expired FROM spaces_enforcement_log WHERE layer = ? ORDER BY seq DESC LIMIT ?`,
		sandboxSettingKey, limit)
	if err != nil {
		return nil, fmt.Errorf("read the sandbox setting's log: %w", err)
	}
	return scanChanges(rows)
}
