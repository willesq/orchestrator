package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for read-only opens
)

// Toggles is the set of items turned off through orch, read by orch install
// so it does not put them back and orch status does not report them as drift.
type Toggles struct {
	disabled map[string]bool
	count    map[string]int
	// enabled is each listed item's stored on/off choice, for providers whose
	// items install writes only when on (instruction modules).
	enabled map[string]bool
}

func toggleKey(provider, kind, identity string) string {
	return provider + "\x00" + kind + "\x00" + identity
}

// NewToggles builds a set from rows; only rows turned off through orch count
// (not ones removed from their file outside it).
func NewToggles(rows []Row) Toggles {
	t := Toggles{disabled: map[string]bool{}, count: map[string]int{}, enabled: map[string]bool{}}
	for _, r := range rows {
		if r.RemovedAt.IsZero() {
			t.enabled[toggleKey(r.Provider, r.Kind, r.Identity)] = r.Enabled
		}
		if r.Status() == StatusDisabled {
			t.disabled[toggleKey(r.Provider, r.Kind, r.Identity)] = true
			t.count[r.Provider]++
		}
	}
	return t
}

// Disabled reports whether an item was turned off through orch.
func (t Toggles) Disabled(provider, kind, identity string) bool {
	return t.disabled[toggleKey(provider, kind, identity)]
}

// State returns an item's stored choice, and whether one is stored.
func (t Toggles) State(provider, kind, identity string) (enabled, known bool) {
	enabled, known = t.enabled[toggleKey(provider, kind, identity)]
	return enabled, known
}

// Count is how many of a provider's items are turned off.
func (t Toggles) Count(provider string) int { return t.count[provider] }

// LoadToggles reads the disabled items from orch.db at dbPath, read-only and
// without migrating it, so it works whether or not orchd is running.
//
// Reason, fail safe: a missing store, or one that predates the settings
// table, cannot hold a toggle, so it yields none. Any other failure (a locked
// or corrupt store) is returned, and install refuses to write rather than
// guessing — guessing "none" would silently re-enable everything the user
// turned off.
func LoadToggles(dbPath string) (Toggles, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return NewToggles(nil), nil
	}
	// Reason: built as a URL, so a data directory with ?, # or % in its path
	// cannot turn into connection options (and undo mode=ro).
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return Toggles{}, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := (&Store{db: conn, now: time.Now}).List(ctx, "", "")
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return NewToggles(nil), nil
		}
		return Toggles{}, fmt.Errorf("read settings toggles from %s: %w", dbPath, err)
	}
	return NewToggles(rows), nil
}
