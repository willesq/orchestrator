package spaces

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/paths"
)

func setSandbox(t *testing.T, ctx context.Context, q querier, write func(SwitchChange, SandboxSetting) error, mode SandboxMode, by string, at time.Time) {
	t.Helper()
	cur, _, err := LoadSandboxSetting(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	c, next, changed, err := NextSandboxSetting(cur, mode, by, at)
	if err != nil || !changed {
		t.Fatalf("set %s: %v %v", mode, changed, err)
	}
	if err := write(c, next); err != nil {
		t.Fatal(err)
	}
}

// TestSandboxSettingStore is the store half of SP-046.4.a1: auto until set,
// each value stored and read back, every change logged with its time and
// caller, and the same value again records nothing.
func TestSandboxSettingStore(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	write := func(c SwitchChange, s SandboxSetting) error { return WriteSandboxSetting(ctx, conn, c, s) }
	if s, p, err := LoadSandboxSetting(ctx, conn); err != nil || p != "" || s != DefaultSandboxSetting() {
		t.Fatalf("default: %+v %q %v", s, p, err)
	}
	for i, m := range []SandboxMode{SandboxOff, SandboxOn, SandboxAuto} {
		at := t0.Add(time.Duration(i) * time.Minute)
		setSandbox(t, ctx, conn, write, m, "operator (process 1)", at)
		s, p, err := LoadSandboxSetting(ctx, conn)
		if err != nil || p != "" || s.Mode != m || s.SetBy != "operator (process 1)" || !s.SetAt.Equal(at) {
			t.Errorf("after %s: %+v %q %v", m, s, p, err)
		}
	}
	hist, err := SandboxSettingHistory(ctx, conn, 10)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history: %+v %v", hist, err)
	}
	if h := hist[0]; h.State != "auto" || h.Previous != "on" || h.By != "operator (process 1)" || !h.At.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("newest: %+v", h)
	}
	if !strings.Contains(DescribeSandboxChange(hist[0]), "from on to auto by operator (process 1)") {
		t.Errorf("describe: %s", DescribeSandboxChange(hist[0]))
	}
	cur, _, _ := LoadSandboxSetting(ctx, conn)
	if _, _, changed, err := NextSandboxSetting(cur, SandboxAuto, "x", t0); changed || err != nil {
		t.Errorf("same value: %v %v", changed, err)
	}
	if _, _, _, err := NextSandboxSetting(cur, "sometimes", "x", t0); !errors.Is(err, ErrBadSandboxMode) {
		t.Errorf("bad value: %v", err)
	}
	if _, _, changed, _ := NextSandboxSetting(SandboxSetting{}, SandboxAuto, "x", t0); changed {
		t.Error("an empty setting is not auto")
	}
	for in, want := range map[string]SandboxMode{" On ": SandboxOn, "OFF": SandboxOff, "auto": SandboxAuto} {
		if m, err := ParseSandboxMode(in); err != nil || m != want {
			t.Errorf("parse %q = %q %v", in, m, err)
		}
	}
}

// TestSandboxSettingIsNotASwitch is the digest half of SP-046.4.a2: the
// setting off is not a relaxed layer, for orchd's switches or for the
// hooks reading orch.db without orchd (INV-16 as amended by D-61).
func TestSandboxSettingIsNotASwitch(t *testing.T) {
	conn := openStore(t)
	ctx := context.Background()
	setSandbox(t, ctx, conn, func(c SwitchChange, s SandboxSetting) error { return WriteSandboxSetting(ctx, conn, c, s) },
		SandboxOff, "operator (process 1)", t0)
	e, problems, err := LoadVerified(ctx, conn)
	if err != nil || len(problems) != 0 || len(e.Relaxed()) != 0 || e.State(LayerSandbox) != SwitchOn {
		t.Errorf("switches with the setting off: %+v %v %v", e, problems, err)
	}
	read, err := ReadEnforcement(ctx, paths.DBFile())
	if err != nil || len(read.Relaxed()) != 0 {
		t.Errorf("the hooks' read with the setting off: %+v %v", read, err)
	}
	// A timed sandbox switch still is one.
	c, sw, _, err := NextSwitch(e, LayerSandbox, SwitchOff, "operator (process 1)", time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSwitch(ctx, conn, c, sw); err != nil {
		t.Fatal(err)
	}
	if read, err := ReadEnforcement(ctx, paths.DBFile()); err != nil || len(read.At(t0).Relaxed()) != 1 {
		t.Errorf("timed switch: %+v %v", read, err)
	}
}

// TestSandboxSettingTampered: a row that does not match its change log, a
// log entry without a row, or an unknown value makes the setting on, the
// strictest value, and says why.
func TestSandboxSettingTampered(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]string{
		"row edited":    `UPDATE spaces_enforcement SET state = 'off' WHERE layer = 'sandbox-setting'`,
		"row deleted":   `DELETE FROM spaces_enforcement WHERE layer = 'sandbox-setting'`,
		"bad value":     `UPDATE spaces_enforcement SET state = 'maybe' WHERE layer = 'sandbox-setting'`,
		"log rewritten": `UPDATE spaces_enforcement_log SET by = 'someone' WHERE layer = 'sandbox-setting'`,
	} {
		t.Run(name, func(t *testing.T) {
			conn := openStore(t)
			setSandbox(t, ctx, conn, func(c SwitchChange, s SandboxSetting) error { return WriteSandboxSetting(ctx, conn, c, s) },
				SandboxOn, "operator (process 1)", t0)
			if _, err := conn.ExecContext(ctx, edit); err != nil {
				t.Fatal(err)
			}
			s, p, err := LoadSandboxSetting(ctx, conn)
			if err != nil || s.Mode != SandboxOn || !strings.Contains(p, "does not match") {
				t.Errorf("%+v %q %v", s, p, err)
			}
		})
	}
	// A store without the tables cannot be read.
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn := openStore(t)
	if _, err := conn.ExecContext(ctx, `DROP TABLE spaces_enforcement`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSandboxSetting(ctx, conn); err == nil {
		t.Error("read a store without the table")
	}
	if _, err := SandboxSettingHistory(ctx, conn, 1); err != nil {
		t.Errorf("history without the switch table: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE spaces_enforcement_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := SandboxSettingHistory(ctx, conn, 1); err == nil {
		t.Error("history without the log table")
	}
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data2"))
	conn = openStore(t)
	if _, err := conn.ExecContext(ctx, `DROP TABLE spaces_enforcement_log`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSandboxSetting(ctx, conn); err == nil {
		t.Error("read a store without the log table")
	}
}

// TestReadSandboxSetting (D-68): the hooks read the setting from orch.db
// read-only when orchd does not answer; a missing store or one without the
// tables is the default, and one that is not a database is an error.
func TestReadSandboxSetting(t *testing.T) {
	ctx := context.Background()
	if s, p, err := ReadSandboxSetting(ctx, filepath.Join(t.TempDir(), "none.db")); err != nil || p != "" || s != DefaultSandboxSetting() {
		t.Errorf("missing store: %+v %q %v", s, p, err)
	}
	conn := openStore(t)
	setSandbox(t, ctx, conn, func(c SwitchChange, s SandboxSetting) error { return WriteSandboxSetting(ctx, conn, c, s) }, SandboxOff, "operator (process 1)", t0)
	if s, p, err := ReadSandboxSetting(ctx, paths.DBFile()); err != nil || p != "" || s.Mode != SandboxOff || !s.SetAt.Equal(t0) {
		t.Errorf("off: %+v %q %v", s, p, err)
	}
	empty := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, _, err := ReadSandboxSetting(ctx, empty); err != nil || s != DefaultSandboxSetting() {
		t.Errorf("store without the tables: %+v %v", s, err)
	}
	garbage := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSandboxSetting(ctx, garbage); err == nil {
		t.Error("an unreadable store read as the default")
	}
}
