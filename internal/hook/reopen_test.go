package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/settings/spacesettings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// reopenWorld is a job's checkout at <tmp>/code/job/app and another job's
// root at <tmp>/code/other, with orchd's record saying the checkout's file
// denies the other root (deny), and the guard in mode.
func reopenWorld(t *testing.T, mode string, deny bool) (checkout, other string) {
	t.Helper()
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobRoot, other := filepath.Join(base, "code", "job"), filepath.Join(base, "code", "other")
	checkout = filepath.Join(jobRoot, "app")
	if err := os.MkdirAll(filepath.Join(checkout, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	guardSetup(t, mode, jobRoot, other, "sess")
	fs := map[string]any{"space": "job", "owned": map[string]any{}}
	if deny {
		fs["deny"] = []string{other}
	}
	writeRecord(t, map[string]any{"checkouts": map[string]any{checkout: fs}})
	return checkout, other
}

// writeRecord writes orchd's record of the checkout files where the hook
// reads it.
func writeRecord(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, spacesettings.DefaultLayout().StateFile(), b)
}

func writeRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// reopenCall runs the guard on a Bash call in cwd and returns the deny
// reason (ok false when it printed none) and the rule handed to orchd.
func reopenCall(t *testing.T, cwd string) (string, bool, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"session_id": "sess", "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]string{"command": "ls"}})
	c := &toolEventClient{fakeClient: &fakeClient{}}
	var out bytes.Buffer
	if err := run([]string{"pre-tool-use"}, bytes.NewReader(payload), &out, factory(c, nil)); err != nil {
		t.Fatal(err)
	}
	reason, ok := denyOf(t, out.Bytes())
	rule := ""
	if len(c.events) == 1 {
		rule = c.events[0].GuardRule
	}
	return reason, ok, rule
}

// TestGuardRefusesReopeningCheckout is D-67 (RC-215): in a checkout whose
// file holds orch's sandbox denies, a tool call is refused while the
// checkout's own .claude/settings.json, or a user entry in its
// settings.local.json, or a .claude folder in a subfolder up to it, allows
// a denied folder, or holds a file orch cannot check; in the guard's
// enforcing modes only. Calls elsewhere, in a checkout orch wrote no
// denies into, and with the entry gone, are allowed.
func TestGuardRefusesReopeningCheckout(t *testing.T) {
	allow := func(dir string) string {
		return `{"sandbox":{"filesystem":{"allowRead":["` + dir + `"]}}}`
	}
	tests := []struct {
		name    string
		deny    bool
		file    string // relative to the checkout
		content func(other string) string
		want    bool
	}{
		{"tracked settings.json", true, ".claude/settings.json", allow, true},
		{"user entry in settings.local.json", true, ".claude/settings.local.json", allow, true},
		{"subfolder's settings.json", true, "sub/.claude/settings.json", allow, true},
		{"allowWrite under the denied root", true, ".claude/settings.json", func(o string) string {
			return `{"sandbox":{"filesystem":{"allowWrite":["` + o + `/x"]}}}`
		}, true},
		{"file orch cannot check", true, ".claude/settings.json", func(string) string { return "not json" }, true},
		{"unrelated entry", true, ".claude/settings.json", func(string) string { return allow("/opt/tools") }, false},
		{"no denies written (auto, nothing to deny)", false, ".claude/settings.json", allow, false},
	}
	for _, tt := range tests {
		for _, mode := range []string{spaces.SwitchBlockOtherJobs, spaces.SwitchStrict, spaces.SwitchLog, spaces.SwitchOff} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				checkout, other := reopenWorld(t, mode, tt.deny)
				writeRaw(t, filepath.Join(checkout, tt.file), []byte(tt.content(other)))
				reason, refused, rule := reopenCall(t, filepath.Join(checkout, "sub"))
				enforcing := spaces.GuardMode(mode).Enforcing()
				if refused != (tt.want && enforcing) {
					t.Fatalf("refused = %v, want %v (%s)", refused, tt.want && enforcing, reason)
				}
				if tt.want && rule != string(spaces.RuleSettingsReopen) {
					t.Errorf("rule handed to orchd = %q, want %s", rule, spaces.RuleSettingsReopen)
				}
				if refused && (!strings.Contains(reason, "rule settings_reopen") || strings.Contains(reason, other)) {
					t.Errorf("reason %q does not name the rule, or names the path", reason)
				}
			})
		}
	}

	// The same entry, then removed; and the session's project folder counts
	// though the call runs elsewhere.
	checkout, other := reopenWorld(t, spaces.SwitchBlockOtherJobs, true)
	set := filepath.Join(checkout, ".claude", "settings.json")
	writeRaw(t, set, []byte(allow(other)))
	outside := filepath.Join(filepath.Dir(checkout), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, refused, _ := reopenCall(t, outside); refused {
		t.Error("a call outside the checkout was refused")
	}
	t.Setenv("CLAUDE_PROJECT_DIR", checkout)
	if _, refused, _ := reopenCall(t, outside); !refused {
		t.Error("a call of a session started in the checkout was allowed")
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if err := os.Remove(set); err != nil {
		t.Fatal(err)
	}
	if reason, refused, _ := reopenCall(t, checkout); refused {
		t.Errorf("refused after the entry went: %s", reason)
	}
}

// TestGuardReopenRecordUnreadable: an orchd record that cannot be read
// fails closed while the guard enforces (INV-13); a missing one is nothing
// to check; with no spaces configured nothing is refused (INV-10).
func TestGuardReopenRecordUnreadable(t *testing.T) {
	checkout, _ := reopenWorld(t, spaces.SwitchBlockOtherJobs, true)
	writeRaw(t, spacesettings.DefaultLayout().StateFile(), []byte("{"))
	if _, refused, rule := reopenCall(t, checkout); !refused || rule != string(spaces.RuleSettingsReopen) {
		t.Errorf("damaged record: refused = %v, rule %q", refused, rule)
	}
	if err := os.Remove(spacesettings.DefaultLayout().StateFile()); err != nil {
		t.Fatal(err)
	}
	if reason, refused, _ := reopenCall(t, checkout); refused {
		t.Errorf("missing record: refused: %s", reason)
	}
	// No policy: the guard allows with no_policy and the record is not read.
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	writeRaw(t, spacesettings.DefaultLayout().StateFile(), []byte("{"))
	if reason, refused, _ := reopenCall(t, checkout); refused {
		t.Errorf("no policy: refused: %s", reason)
	}
}
