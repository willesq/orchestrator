package fakeclaude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/safetynet"
)

// TestRunnerSimulatesClaude checks that the simulated commands leave the
// files in the states safetynet.Detect reads, the way the real CLI does.
func TestRunnerSimulatesClaude(t *testing.T) {
	home := t.TempDir()
	loc := safetynet.Locate(home)
	r := New(loc.ClaudeDir)
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if _, err := r.Run(ctx, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	state := func() safetynet.PluginState { return safetynet.Detect(loc).Plugin }

	run("plugin", "install", "safety-net@cc-marketplace", "--scope", "user")
	if state() != safetynet.StateLegacy {
		t.Fatalf("state %s", state())
	}
	run("plugin", "uninstall", "safety-net@cc-marketplace", "--scope", "user")
	run("plugin", "marketplace", "add", loc.MarketplaceDir)
	run("plugin", "install", safetynet.PluginID, "--scope", "user")
	if state() != safetynet.StateOK {
		t.Fatalf("state %s", state())
	}
	run("plugin", "disable", safetynet.PluginID)
	if state() != safetynet.StateDisabled {
		t.Fatalf("state %s", state())
	}
	run("plugin", "enable", safetynet.PluginID)
	run("plugin", "marketplace", "update", safetynet.MarketplaceName)

	old := New(loc.ClaudeDir)
	old.Version, old.SHA = "2.0.0", "abc"
	if _, err := old.Run(ctx, []string{"plugin", "update", safetynet.PluginID}); err != nil {
		t.Fatal(err)
	}
	if state() != safetynet.StateOutdated {
		t.Fatalf("state %s", state())
	}
	run("plugin", "marketplace", "remove", safetynet.MarketplaceName)
	if state() != safetynet.StateMissing {
		t.Fatalf("remove did not uninstall its plugins: %s", state())
	}
	if len(r.Calls()) != 8 {
		t.Errorf("calls %v", r.Calls())
	}

	r.Fail = map[string]error{"plugin install": errors.New("boom")}
	if _, err := r.Run(ctx, []string{"plugin", "install", "x"}); err == nil {
		t.Error("Fail not honored")
	}
	for _, bad := range [][]string{{"plugin"}, {"other", "a", "b"}, {"plugin", "nope", "x"}} {
		if _, err := r.Run(ctx, bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(loc.ClaudeDir, "plugins", "installed_plugins.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(loc.ClaudeDir).Run(ctx, []string{"plugin", "install", "x"}); err == nil {
		t.Error("corrupt file accepted")
	}
	if _, err := New(loc.ClaudeDir).Run(ctx, []string{"plugin", "marketplace", "remove", "x"}); err == nil {
		t.Error("corrupt plugins file accepted on remove")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(blocker, "claude")).Run(ctx, []string{"plugin", "enable", "x"}); err == nil {
		t.Error("write under a file succeeded")
	}
}
