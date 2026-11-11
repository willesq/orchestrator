package spaces

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
)

var t0 = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func load(t *testing.T, q querier) Enforcement {
	t.Helper()
	e, err := LoadEnforcement(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestEnforcementDefaults is the default half of SP-070.1.a1: memory on,
// guard block-other-jobs, terms on, sandbox on, and the tool hooks on.
func TestEnforcementDefaults(t *testing.T) {
	want := map[Layer]string{LayerMemory: "on", LayerGuard: "block-other-jobs", LayerTerms: "on", LayerSandbox: "on", LayerHooks: "on"}
	for _, e := range []Enforcement{DefaultEnforcement(), load(t, openStore(t)), {}} {
		for l, st := range want {
			if e.State(l) != st || l.Default() != st {
				t.Errorf("%s = %s (default %s), want %s", l, e.State(l), l.Default(), st)
			}
		}
		if len(e.Relaxed()) != 0 {
			t.Errorf("defaults relaxed: %v", e.Relaxed())
		}
	}
	if Layer("nope").Default() != "" {
		t.Error("an unknown layer has a default")
	}
}

// TestSetSwitchEveryLayerState is SP-070.1.a1: each of the layers can be
// set to each of its states, and each set is logged with time and caller.
func TestSetSwitchEveryLayerState(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	n := 0
	for _, l := range Layers {
		for _, st := range append(l.States()[1:], l.Default()) {
			c, changed, err := setSwitch(ctx, conn, l, st, "operator (process 1)", 0, t0.Add(time.Duration(n)*time.Second))
			if err != nil || !changed {
				t.Fatalf("%s -> %s: %v %v", l, st, changed, err)
			}
			sw := load(t, conn)[l]
			if sw.State != st || sw.SetBy != "operator (process 1)" || !sw.SetAt.Equal(c.At) || !sw.Until.IsZero() {
				t.Errorf("%s -> %s stored %+v", l, st, sw)
			}
			if sw.Relaxed() != (st == SwitchOff || st == SwitchLog) {
				t.Errorf("%s %s relaxed = %v", l, st, sw.Relaxed())
			}
			n++
		}
	}
	if n != 12 {
		t.Errorf("%d layer x state cases, want 12", n)
	}
	hist, err := SwitchHistory(ctx, conn, 100)
	if err != nil || len(hist) != n {
		t.Fatalf("history: %d entries, %v; want %d", len(hist), err, n)
	}
	if h := hist[0]; h.Layer != LayerHooks || h.State != "on" || h.Previous != "off" || h.By != "operator (process 1)" {
		t.Errorf("newest change = %+v", h)
	}
	if _, changed, err := setSwitch(ctx, conn, LayerHooks, "on", "x", 0, t0); err != nil || changed {
		t.Errorf("a no-op set: %v %v", changed, err)
	}
	bad := []struct {
		l     Layer
		state string
		dur   time.Duration
		want  error
	}{
		{"firewall", "off", 0, ErrUnknownLayer},
		{LayerMemory, "strict", 0, ErrBadState},
		{LayerGuard, "on", 0, ErrBadState},
		{LayerGuard, "off", -time.Second, ErrBadDuration},
		{LayerGuard, "off", MaxSwitchFor + time.Second, ErrBadDuration},
	}
	for _, b := range bad {
		if _, _, err := setSwitch(ctx, conn, b.l, b.state, "x", b.dur, t0); !errors.Is(err, b.want) {
			t.Errorf("setSwitch(%s, %s, %v) = %v, want %v", b.l, b.state, b.dur, err, b.want)
		}
	}
}

// TestTimedSwitchExpires is SP-070.1.a3 in the store: at expiry a timed
// switch reverts to the state before it, and the revert is logged.
func TestTimedSwitchExpires(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	if _, _, err := setSwitch(ctx, conn, LayerGuard, SwitchLog, "op", 0, t0); err != nil {
		t.Fatal(err)
	}
	c, _, err := setSwitch(ctx, conn, LayerGuard, SwitchOff, "op", time.Hour, t0)
	if err != nil || !c.Until.Equal(t0.Add(time.Hour)) || !strings.Contains(c.Describe(), "until 2026-11-11T11:00:00Z") {
		t.Fatalf("timed switch = %+v, %v", c, err)
	}
	if sw := load(t, conn)[LayerGuard]; sw.Previous != SwitchLog || !sw.Until.Equal(t0.Add(time.Hour)) {
		t.Fatalf("stored = %+v", sw)
	}
	if cs, err := expireSwitches(ctx, conn, t0.Add(time.Hour-time.Second)); err != nil || len(cs) != 0 {
		t.Fatalf("early expiry: %v %v", cs, err)
	}
	cs, err := expireSwitches(ctx, conn, t0.Add(time.Hour))
	if err != nil || len(cs) != 1 {
		t.Fatalf("expiry: %v %v", cs, err)
	}
	if c := cs[0]; c.State != SwitchLog || c.Previous != SwitchOff || !c.Expired || c.By != "timer" ||
		!strings.Contains(c.Describe(), "its timed switch expired") {
		t.Errorf("expiry change = %+v (%s)", c, c.Describe())
	}
	if sw := load(t, conn)[LayerGuard]; sw.State != SwitchLog || !sw.Until.IsZero() {
		t.Errorf("after expiry = %+v", sw)
	}
	hist, _ := SwitchHistory(ctx, conn, 1)
	if len(hist) != 1 || !hist[0].Expired {
		t.Errorf("expiry not logged: %v", hist)
	}
	// A timed switch over a timed one keeps the first revert state.
	_, _, _ = setSwitch(ctx, conn, LayerMemory, SwitchOff, "op", time.Hour, t0)
	_, _, _ = setSwitch(ctx, conn, LayerMemory, SwitchOff, "op", 2*time.Hour, t0)
	if sw := load(t, conn)[LayerMemory]; sw.Previous != SwitchOn {
		t.Errorf("chained timed switch reverts to %q", sw.Previous)
	}
}

// TestLoadEnforcementIgnoresForeignRows: a row edited outside orchd with a
// state its layer does not have, or an unknown layer, is not taken as off.
func TestLoadEnforcementIgnoresForeignRows(t *testing.T) {
	conn := openStore(t)
	for _, q := range []string{
		`INSERT INTO spaces_enforcement (layer, state, set_at, set_by) VALUES ('guard', 'disabled', '2026-11-11T10:00:00Z', 'x')`,
		`INSERT INTO spaces_enforcement (layer, state, set_at, set_by) VALUES ('firewall', 'off', '2026-11-11T10:00:00Z', 'x')`,
		`INSERT INTO spaces_enforcement (layer, state, set_at, set_by, until, previous) VALUES ('memory', 'off', '2026-11-11T10:00:00Z', 'x', '2026-11-11T11:00:00Z', 'bogus')`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e := load(t, conn)
	if e.State(LayerGuard) != SwitchBlockOtherJobs || len(e) != len(Layers) {
		t.Errorf("foreign rows read as %v", e)
	}
	if sw := e[LayerMemory]; sw.Previous != SwitchOn {
		t.Errorf("a timed switch with a bogus revert state reverts to %q", sw.Previous)
	}
	cs, err := expireSwitches(context.Background(), conn, t0.Add(2*time.Hour))
	if err != nil || len(cs) != 1 || load(t, conn).State(LayerMemory) != SwitchOn {
		t.Errorf("expiry of the bogus row: %v %v", cs, err)
	}
}

// TestReadEnforcement: install reads the switches read-only, whether or not
// orch.db or its tables exist.
func TestReadEnforcement(t *testing.T) {
	ctx := context.Background()
	if e, err := ReadEnforcement(ctx, filepath.Join(t.TempDir(), "none.db")); err != nil || e.State(LayerHooks) != SwitchOn {
		t.Errorf("missing store: %v %v", e, err)
	}
	conn := openStore(t)
	if _, _, err := setSwitch(ctx, conn, LayerHooks, SwitchOff, "op", 0, t0); err != nil {
		t.Fatal(err)
	}
	e, err := ReadEnforcement(ctx, paths.DBFile())
	if err != nil || e.State(LayerHooks) != SwitchOff {
		t.Errorf("hooks switch read as %v, %v", e, err)
	}
	empty := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if e, err := ReadEnforcement(ctx, empty); err != nil || e.State(LayerHooks) != SwitchOn {
		t.Errorf("store without the tables: %v %v", e, err)
	}
	garbage := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnforcement(ctx, garbage); err == nil {
		t.Error("an unreadable store read as defaults")
	}
}

func TestParseLayer(t *testing.T) {
	for _, s := range []string{"memory", " Guard ", "TERMS", "sandbox", "hooks"} {
		if _, err := ParseLayer(s); err != nil {
			t.Errorf("ParseLayer(%q) = %v", s, err)
		}
	}
	if _, err := ParseLayer("all"); !errors.Is(err, ErrUnknownLayer) || !strings.Contains(err.Error(), "memory, guard, terms, sandbox, hooks") {
		t.Errorf("ParseLayer(all) = %v", err)
	}
	if LayerGuard.Title() != "tool guard" || !LayerGuard.Relaxed(SwitchLog) || LayerMemory.Relaxed(SwitchLog) || LayerGuard.Relaxed(SwitchStrict) {
		t.Error("guard titles or relaxed states are wrong")
	}
}

// TestEnforcementAt: a timed switch whose time is up reads as reverted,
// without writing; one still running reads as set.
func TestEnforcementAt(t *testing.T) {
	e := DefaultEnforcement()
	e[LayerGuard] = Switch{Layer: LayerGuard, State: SwitchOff, Until: t0.Add(time.Minute), Previous: SwitchStrict, SetBy: "op"}
	e[LayerMemory] = Switch{Layer: LayerMemory, State: SwitchOff}
	if got := e.At(t0); got.State(LayerGuard) != SwitchOff || len(got.Relaxed()) != 2 {
		t.Errorf("before expiry: %v", got)
	}
	got := e.At(t0.Add(time.Minute))
	if sw := got[LayerGuard]; sw.State != SwitchStrict || !sw.Until.IsZero() || sw.SetBy != "timer" || !sw.SetAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("after expiry: %+v", sw)
	}
	if e.State(LayerGuard) != SwitchOff || got.State(LayerMemory) != SwitchOff {
		t.Error("At changed its receiver or an untimed switch")
	}
}

// setSwitch is a switch as orchd makes it: from the stored switches,
// written with its change-log entry.
func setSwitch(ctx context.Context, conn *sql.DB, l Layer, state, by string, forDur time.Duration, now time.Time) (SwitchChange, bool, error) {
	cur, err := LoadEnforcement(ctx, conn)
	if err != nil {
		return SwitchChange{}, false, err
	}
	c, sw, changed, err := NextSwitch(cur, l, state, by, forDur, now)
	if err != nil || !changed {
		return c, changed, err
	}
	return c, true, WriteSwitch(ctx, conn, c, sw)
}

// expireSwitches reverts the stored timed switches whose time is up.
func expireSwitches(ctx context.Context, conn *sql.DB, now time.Time) ([]SwitchChange, error) {
	cur, err := LoadEnforcement(ctx, conn)
	if err != nil {
		return nil, err
	}
	cs, sws := Expired(cur, now)
	for i := range cs {
		if err := WriteSwitch(ctx, conn, cs[i], sws[i]); err != nil {
			return nil, err
		}
	}
	return cs, nil
}

// TestLoadVerified: at startup a row must match its newest change-log
// entry; a row written, or an entry appended, outside orchd leaves its
// layer at the default and is named.
func TestLoadVerified(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	if e, problems, err := LoadVerified(ctx, conn); err != nil || len(problems) != 0 || len(e.Relaxed()) != 0 {
		t.Fatalf("empty store: %v %v %v", e, problems, err)
	}
	for _, l := range []Layer{LayerGuard, LayerMemory, LayerTerms} {
		if _, _, err := setSwitch(ctx, conn, l, SwitchOff, "op", time.Hour, t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := expireSwitches(ctx, conn, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := setSwitch(ctx, conn, LayerGuard, SwitchLog, "op", 0, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := setSwitch(ctx, conn, LayerMemory, SwitchOff, "op", 0, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	e, problems, err := LoadVerified(ctx, conn)
	if err != nil || len(problems) != 0 || e.State(LayerGuard) != SwitchLog || e.State(LayerTerms) != SwitchOn || e.State(LayerMemory) != SwitchOff {
		t.Fatalf("orchd's own writes: %v %v %v", e, problems, err)
	}
	for _, q := range []string{
		// A row written alone: the sandbox switched off with no log entry.
		`INSERT INTO spaces_enforcement (layer, state, set_at, set_by) VALUES ('sandbox', 'off', '2026-11-11T13:00:00Z', 'operator (process 1)')`,
		// A row edited: the guard's caller forged.
		`UPDATE spaces_enforcement SET state = 'off', set_by = 'operator (process 2)' WHERE layer = 'guard'`,
		// An entry appended alone: memory's newest entry no longer matches its row.
		`INSERT INTO spaces_enforcement_log (at, layer, state, previous, by) VALUES ('2026-11-11T14:00:00Z', 'memory', 'off', 'on', 'x')`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e, problems, err = LoadVerified(ctx, conn)
	if err != nil || len(problems) != 3 {
		t.Fatalf("problems = %v, %v", problems, err)
	}
	for _, l := range []Layer{LayerSandbox, LayerGuard, LayerMemory} {
		if e.State(l) != l.Default() {
			t.Errorf("%s = %s after an edit outside orchd, want its default", l, e.State(l))
		}
	}
	if e.State(LayerTerms) != SwitchOn || !strings.Contains(problems[0], "memory switch in orch.db does not match") {
		t.Errorf("terms %s, problems %v", e.State(LayerTerms), problems)
	}
	if _, err := ReadEnforcement(ctx, paths.DBFile()); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("ReadEnforcement of an edited store = %v", err)
	}
}

func TestSameSwitches(t *testing.T) {
	a, b := DefaultEnforcement(), DefaultEnforcement()
	if !SameSwitches(a, b) {
		t.Error("defaults differ")
	}
	b[LayerGuard] = Switch{Layer: LayerGuard, State: SwitchOff, SetAt: t0, SetBy: "op"}
	if SameSwitches(a, b) {
		t.Error("a switched guard reads the same")
	}
	a[LayerGuard] = b[LayerGuard]
	a[LayerMemory] = Switch{Layer: LayerMemory, State: SwitchOn, Until: t0, Previous: SwitchOff}
	b[LayerMemory] = Switch{Layer: LayerMemory, State: SwitchOn, Until: t0, Previous: SwitchOn}
	if SameSwitches(a, b) {
		t.Error("timed switches with different revert states read the same")
	}
}

// TestUnreadableTimerExpires: a timer that cannot be read does not make a
// relaxed switch permanent; it reads as already expired.
func TestUnreadableTimerExpires(t *testing.T) {
	conn := openStore(t)
	if _, err := conn.Exec(`INSERT INTO spaces_enforcement (layer, state, set_at, set_by, until, previous)
		VALUES ('guard', 'off', '2026-11-11T10:00:00Z', 'x', 'next tuesday', 'strict')`); err != nil {
		t.Fatal(err)
	}
	if got := load(t, conn).At(t0).State(LayerGuard); got != SwitchStrict {
		t.Errorf("guard with an unreadable timer = %s, want its revert state", got)
	}
}

// TestLoadVerifiedSingleTableEdits: a row with no readable time and no log
// entry, and a timed row whose revert state was edited, are not taken.
func TestLoadVerifiedSingleTableEdits(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	if _, _, err := setSwitch(ctx, conn, LayerMemory, SwitchOff, "op", time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := setSwitch(ctx, conn, LayerTerms, SwitchOff, "op", time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	if e, problems, err := LoadVerified(ctx, conn); err != nil || len(problems) != 0 || e[LayerMemory].Previous != SwitchOn {
		t.Fatalf("orchd's timed switches: %v %v %v", e, problems, err)
	}
	for _, q := range []string{
		`INSERT INTO spaces_enforcement (layer, state, set_at, set_by) VALUES ('guard', 'off', 'x', 'me')`,
		`UPDATE spaces_enforcement SET previous = 'off' WHERE layer = 'memory'`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e, problems, err := LoadVerified(ctx, conn)
	if err != nil || len(problems) != 2 || e.State(LayerGuard) != SwitchBlockOtherJobs || e.State(LayerMemory) != SwitchOn ||
		e.State(LayerTerms) != SwitchOff {
		t.Errorf("after single-table edits: %v %v %v", e, problems, err)
	}
	hist, err := SwitchHistory(ctx, conn, 1)
	if err != nil || hist[0].RevertsTo != SwitchOn {
		t.Errorf("history = %+v %v", hist, err)
	}
}
