package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/install"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// switchClient records the SetConfig calls `orch spaces on|off` make.
type switchClient struct {
	*fakeClient
	reqs []*orchdv1.SetConfigRequest
}

func (c *switchClient) SetConfig(_ context.Context, req *orchdv1.SetConfigRequest, _ ...grpc.CallOption) (*orchdv1.ConfigState, error) {
	c.reqs = append(c.reqs, req)
	return &orchdv1.ConfigState{}, nil
}

// withCLISpacesOff makes the CLI read memory spaces as off.
func withCLISpacesOff(t *testing.T) {
	t.Helper()
	old := cliSpacesOff
	cliSpacesOff = func() bool { return true }
	t.Cleanup(func() { cliSpacesOff = old })
	t.Cleanup(install.SetSpacesLoader(func() (bool, error) { return false, nil }))
}

// TestSpacesOnOffCmds: on and off send spaces.enabled alone, confirmed; off
// needs --confirm, since it makes quarantined content readable again.
func TestSpacesOnOffCmds(t *testing.T) {
	c := &switchClient{fakeClient: &fakeClient{}}
	out, err := run(factory(c, nil), "", "spaces", "on")
	if err != nil || !strings.Contains(out, "turning on") {
		t.Fatalf("on: %q, %v", out, err)
	}
	if _, err := run(factory(c, nil), "", "spaces", "off"); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("off without --confirm: %v", err)
	}
	out, err = run(factory(c, nil), "", "spaces", "off", "--confirm")
	if err != nil || !strings.Contains(out, "memory spaces are off") {
		t.Fatalf("off: %q, %v", out, err)
	}
	if len(c.reqs) != 2 {
		t.Fatalf("SetConfig calls = %d, want 2", len(c.reqs))
	}
	for i, want := range []string{"true", "false"} {
		r := c.reqs[i]
		if len(r.GetValues()) != 1 || r.GetValues()[config.KeySpacesEnabled] != want || !r.GetConfirmSensitive() {
			t.Fatalf("request %d = %v", i, r)
		}
	}
}

// TestSpacesOffCLIIsPreSpaces is the CLI half of "disabled = pre-spaces
// behaviour": orch status reports no tool hooks, `orch run` and installing
// git hooks are refused, removing them still works, and hooks installed
// earlier let every commit through.
func TestSpacesOffCLIIsPreSpaces(t *testing.T) {
	isolate(t)
	withCLISpacesOff(t)
	for _, info := range harnessInfo() {
		if info.ToolHooks != nil {
			t.Fatalf("%s reports tool hooks with spaces off: %+v", info.Name, info.ToolHooks)
		}
	}
	cf := factory(&fakeClient{}, nil)
	if _, err := run(cf, "", "run", "acme", "--", "claude"); err == nil || !strings.Contains(err.Error(), "memory spaces are off") {
		t.Fatalf("orch run with spaces off: %v", err)
	}
	repo := t.TempDir()
	if _, err := run(cf, "", "guard", "git-hook", "--install", "--hooks-dir", repo); err == nil || !strings.Contains(err.Error(), "memory spaces are off") {
		t.Fatalf("git-hook --install with spaces off: %v", err)
	}
	if _, err := run(cf, "", "guard", "git-hook", "--uninstall", "--hooks-dir", repo); err != nil {
		t.Fatalf("git-hook --uninstall with spaces off: %v", err)
	}
	msg := t.TempDir() + "/COMMIT_EDITMSG"
	if err := os.WriteFile(msg, []byte("anything\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(cf, "", "guard", "git-hook", "run", "commit-msg", msg); err != nil {
		t.Fatalf("git-hook run with spaces off: %v", err)
	}
}

// TestSpacesSwitchedOffReadsTheStore: with no guard snapshot and no orch.db,
// memory spaces read as off, the default.
func TestSpacesSwitchedOffReadsTheStore(t *testing.T) {
	isolate(t)
	if !spacesSwitchedOff() {
		t.Fatal("a fresh install reads spaces as on")
	}
}
