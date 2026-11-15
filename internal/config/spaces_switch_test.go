package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/db"
)

// TestSpacesSwitchField: spaces are off by default, and the switch is a
// sensitive setting orchd applies at once.
func TestSpacesSwitchField(t *testing.T) {
	f, ok := FieldFor(KeySpacesEnabled)
	if !ok {
		t.Fatal("spaces.enabled is not a setting")
	}
	if f.Type != TypeBool || !f.Sensitive || !f.Live || f.Hidden || f.Section != SectionSpaces {
		t.Fatalf("field = %+v", f)
	}
	if Defaults().GetBool(KeySpacesEnabled) {
		t.Fatal("spaces are on by default")
	}
	s, err := Unmarshal(Defaults())
	if err != nil || s.Spaces.Enabled {
		t.Fatalf("Settings.Spaces = %+v, %v", s.Spaces, err)
	}
}

// TestSpacesEnabled reads the switch without orchd: off with no store, then
// what orch.db holds.
func TestSpacesEnabled(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	if on, err := SpacesEnabled(); err != nil || on {
		t.Fatalf("with no store: %v, %v", on, err)
	}
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if on, err := SpacesEnabled(); err != nil || on {
		t.Fatalf("with an empty store: %v, %v", on, err)
	}
	if err := ApplyStore(context.Background(), conn, map[string]any{KeySpacesEnabled: true}, false, "test"); err == nil {
		t.Fatal("the switch changed without confirmation")
	}
	if err := ApplyStore(context.Background(), conn, map[string]any{KeySpacesEnabled: true}, true, "test"); err != nil {
		t.Fatal(err)
	}
	if on, err := SpacesEnabled(); err != nil || !on {
		t.Fatalf("after turning on: %v, %v", on, err)
	}
}

// TestSpacesEnabledWithoutAStoredValue: a store that never stored the
// switch reads as on exactly when it applied a policy (an upgrade from
// before the switch, or a row removed outside orch), so enforcement never
// stops silently; the operator's explicit "off" is kept as a row and wins.
func TestSpacesEnabledWithoutAStoredValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		applied bool
	}{{"never applied", false}, {"applied", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			conn, err := db.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if tc.applied {
				for _, q := range []string{
					`INSERT INTO spaces_revisions (doc, sha256, created_at, created_by) VALUES ('{}', 'x', 'now', 'test')`,
					`INSERT INTO spaces_applied (revision, sha256, applied_at, applied_by) VALUES (1, 'x', 'now', 'test')`,
				} {
					if _, err := conn.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
			}
			on, stored, err := SpacesEnabledIn(context.Background(), conn)
			if err != nil || on != tc.applied || stored {
				t.Fatalf("SpacesEnabledIn = %v, %v, %v; want %v, unstored", on, stored, err, tc.applied)
			}
			if got, err := SpacesEnabled(); err != nil || got != tc.applied {
				t.Fatalf("SpacesEnabled = %v, %v", got, err)
			}
			// The operator's off is stored even though it is the default.
			if err := ApplyStore(context.Background(), conn, map[string]any{KeySpacesEnabled: false}, true, "operator"); err != nil {
				t.Fatal(err)
			}
			if on, stored, err := SpacesEnabledIn(context.Background(), conn); err != nil || on || !stored {
				t.Fatalf("after the operator's off: %v, %v, %v", on, stored, err)
			}
		})
	}
}

// TestSpacesSwitchCannotBeReset: a reset would turn spaces off behind the
// operator's back, so it is refused; a reset of everything removes the row,
// which reads as on wherever a policy was applied.
func TestSpacesSwitchCannotBeReset(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	err = ResetStore(context.Background(), conn, []string{KeySpacesEnabled}, true, "cli:reset")
	if err == nil || !strings.Contains(err.Error(), "orch spaces on|off") {
		t.Fatalf("reset = %v", err)
	}
	if got := keepDefaultHint("other"); !strings.Contains(got, "Orch.app") {
		t.Fatalf("hint = %q", got)
	}
}

// TestFirstStringOnAnOlderStore: a table an older store lacks reads as no
// row.
func TestFirstStringOnAnOlderStore(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, found, err := firstString(context.Background(), conn, `SELECT x FROM no_such_table`); err != nil || found {
		t.Fatalf("firstString = %v, %v", found, err)
	}
	if _, _, err := firstString(context.Background(), conn, `SELEKT`); err == nil {
		t.Fatal("a bad query was not reported")
	}
}
