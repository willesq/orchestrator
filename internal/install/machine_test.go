package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/settings/spacesettings"
)

func TestGitHookRegistry(t *testing.T) {
	data := t.TempDir()
	if dirs, err := RegisteredGitHookDirs(data); err != nil || len(dirs) != 0 {
		t.Fatalf("missing registry: %v %v", dirs, err)
	}
	for _, d := range []string{"/b/hooks", "/a/hooks", "/b/hooks"} {
		if err := RecordGitHookDir(data, d, true); err != nil {
			t.Fatal(err)
		}
	}
	dirs, _ := RegisteredGitHookDirs(data)
	if !slices.Equal(dirs, []string{"/a/hooks", "/b/hooks"}) {
		t.Errorf("dirs = %v", dirs)
	}
	info, err := os.Stat(GitHookRegistryPath(data))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("registry mode: %v %v", info, err)
	}
	if err := RecordGitHookDir(data, "/a/hooks", false); err != nil {
		t.Fatal(err)
	}
	if dirs, _ = RegisteredGitHookDirs(data); !slices.Equal(dirs, []string{"/b/hooks"}) {
		t.Errorf("after removal: %v", dirs)
	}
	// A relative entry written by hand is never used.
	if err := os.WriteFile(GitHookRegistryPath(data), []byte(`{"hooks_dirs":["rel/hooks","/c/hooks"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if dirs, _ = RegisteredGitHookDirs(data); !slices.Equal(dirs, []string{"/c/hooks"}) {
		t.Errorf("relative entry kept: %v", dirs)
	}
	if err := os.WriteFile(GitHookRegistryPath(data), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisteredGitHookDirs(data); err == nil {
		t.Error("broken registry read")
	}
	if err := RecordGitHookDir(data, "/x", true); err == nil {
		t.Error("broken registry overwritten")
	}
}

func TestMachineGitHooksRoundTrip(t *testing.T) {
	h := newTestHome(t)
	hooks := hooksWithUserHook(t)
	before := hookTree(t, hooks)
	applyGitHooks(t, hooks, testBins().Orch, false, t.TempDir())
	if err := RecordGitHookDir(h.data, hooks, true); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "deleted-repo", "hooks")
	if err := RecordGitHookDir(h.data, gone, true); err != nil {
		t.Fatal(err)
	}
	// A recorded directory holding none of orch's hooks gets none.
	plain := hooksWithUserHook(t)
	plainBefore := hookTree(t, plain)
	if err := RecordGitHookDir(h.data, plain, true); err != nil {
		t.Fatal(err)
	}
	m := h.machine(&fakeRun{})

	// Install repairs a hook that lost its execute bit.
	if err := os.Chmod(filepath.Join(hooks, "pre-push"), 0o644); err != nil {
		t.Fatal(err)
	}
	units := FullUnits(m, nil)
	applyPlan(t, InstallPlan("all", units), h)
	if info, _ := os.Stat(filepath.Join(hooks, "pre-push")); info.Mode().Perm()&0o100 == 0 {
		t.Error("install did not repair the hook")
	}
	if n, err := InstallPlan("all", units).Pending(); err != nil || n != 0 {
		t.Errorf("install left %d pending, %v", n, err)
	}
	if !sameTree(plainBefore, hookTree(t, plain)) {
		t.Error("install wrote hooks into a recorded directory that had none of orch's")
	}

	// Uninstall puts the user's hook back and forgets both directories.
	applyPlan(t, UninstallPlan("all", units, true), h)
	if !sameTree(before, hookTree(t, hooks)) {
		t.Errorf("hooks not restored: %v", treeNames(hookTree(t, hooks)))
	}
	if dirs, _ := RegisteredGitHookDirs(h.data); len(dirs) != 0 {
		t.Errorf("registry still lists %v", dirs)
	}
	if n, err := UninstallPlan("all", units, true).Pending(); err != nil || n != 0 {
		t.Errorf("second uninstall has %d pending, %v", n, err)
	}

	// --no-git-hooks plans nothing either way.
	m.NoGitHooks = true
	if len(m.gitHooksUnit().Steps())+len(m.gitHooksUnit().UndoSteps()) != 0 {
		t.Error("--no-git-hooks planned hook steps")
	}
}

func TestMachineGitHooksBadRegistry(t *testing.T) {
	h := newTestHome(t)
	writeFile(t, GitHookRegistryPath(h.data), "{")
	steps := h.machine(&fakeRun{}).gitHooksUnit().UndoSteps()
	if len(steps) != 1 {
		t.Fatalf("steps = %d", len(steps))
	}
	if _, err := steps[0].Run(context.Background()); err == nil {
		t.Error("an unreadable registry passed")
	}
}

func TestMachineCheckoutSettings(t *testing.T) {
	h := newTestHome(t)
	checkout := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	userLocal := mustJSON(t, map[string]any{"env": map[string]any{"MINE": "1"}})
	local := filepath.Join(checkout, ".claude", "settings.local.json")
	writeFile(t, local, userLocal)
	want := spacesettings.Fragment{
		Scalars: map[string]any{"sandbox.enabled": true},
		Lists:   map[string][]string{"permissions.deny": {"Read(//other/**)"}},
	}
	m, err := spacesettings.Merge([]byte(userLocal), true, want, spacesettings.Owned{})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, local, string(m.Data))
	layout := spacesettings.Layout{Dir: filepath.Join(h.data, "spaces")}
	state, _ := json.Marshal(map[string]any{"checkouts": map[string]any{checkout: map[string]any{"owned": m.Owned, "backed_up": true}}})
	writeFile(t, layout.StateFile(), string(state))

	units := FullUnits(h.machine(&fakeRun{}), nil)
	var out strings.Builder
	if err := UninstallPlan("all", units, true).Print(&out); err != nil || !strings.Contains(out.String(), "1 checkout settings file") {
		t.Fatalf("plan: %v\n%s", err, out.String())
	}
	applyPlan(t, UninstallPlan("all", units, true), h)
	var got, wantRoot map[string]any
	_ = json.Unmarshal([]byte(readFile(t, local)), &got)
	_ = json.Unmarshal([]byte(userLocal), &wantRoot)
	if g, w := mustJSON(t, got), mustJSON(t, wantRoot); g != w {
		t.Errorf("settings.local.json = %s, want %s", g, w)
	}
	if n, err := UninstallPlan("all", units, true).Pending(); err != nil || n != 0 {
		t.Errorf("second uninstall has %d pending, %v", n, err)
	}
}

func TestPurgeTargetsAndSafety(t *testing.T) {
	h := newTestHome(t)
	for _, rel := range []string{"orch.db", "qdrant/seg", "backups/x", "events/e.jsonl"} {
		writeFile(t, filepath.Join(h.data, rel), "x")
	}
	writeFile(t, filepath.Join(h.home, "Library", "Logs", "orchestrator", "orchd.log"), "log")
	writeFile(t, filepath.Join(h.home, ".config", "orch", "config.yaml"), "legacy")
	m := h.machine(&fakeRun{})
	m.PurgeData = true
	targets, err := m.PurgeTargets()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(h.data, "orch.db"), filepath.Join(h.data, "qdrant"),
		filepath.Join(h.home, "Library", "Logs", "orchestrator"), filepath.Join(h.home, ".config", "orch", "config.yaml")} {
		if !slices.Contains(targets, want) {
			t.Errorf("targets lack %s: %v", want, targets)
		}
	}
	if slices.Contains(targets, filepath.Join(h.data, "backups")) {
		t.Error("backups would be purged")
	}
	applyPlan(t, UninstallPlan("all", FullUnits(m, nil), true), h)
	for _, p := range targets {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s survived the purge", p)
		}
	}
	if _, err := os.Stat(filepath.Join(h.data, "backups", "x")); err != nil {
		t.Error("purge removed backups")
	}

	refused := []struct{ name, dir string }{
		{"relative", "orch"},
		{"root", "/"},
		{"home", h.home},
		{"holds home", filepath.Dir(h.home)},
	}
	for _, tt := range refused {
		if err := checkDataDir(tt.dir, h.home); err == nil {
			t.Errorf("%s data dir accepted", tt.name)
		}
	}
	other := filepath.Join(t.TempDir(), "notes")
	writeFile(t, filepath.Join(other, "mine.txt"), "x")
	if err := checkDataDir(other, h.home); err == nil {
		t.Error("a folder neither named orch nor holding orch.db accepted")
	}
	link := filepath.Join(t.TempDir(), "orch")
	symlink(t, other, link)
	if err := checkDataDir(link, h.home); err == nil {
		t.Error("a symlinked data dir accepted")
	}
	m.DataDir = other
	steps := m.purgeSteps()
	if len(steps) != 1 {
		t.Fatalf("refused purge planned %d steps", len(steps))
	}
	if _, err := steps[0].Run(context.Background()); err == nil {
		t.Error("refused purge ran")
	}
	if _, err := os.Stat(filepath.Join(other, "mine.txt")); err != nil {
		t.Error("refused purge deleted a file")
	}
	if err := removeBeneath("/"); err == nil {
		t.Error("removeBeneath(/) accepted")
	}
	if err := removeBeneath(filepath.Join(t.TempDir(), "missing", "x")); err != nil {
		t.Errorf("missing parent: %v", err)
	}
}

func TestCheckoutSettingsProblemFailsStep(t *testing.T) {
	h := newTestHome(t)
	layout := spacesettings.Layout{Dir: filepath.Join(h.data, "spaces")}
	writeFile(t, layout.StateFile(), "{")
	if n := recordedCheckouts(layout.StateFile()); n != 0 {
		t.Errorf("broken record counted %d", n)
	}
	// A record naming a checkout whose file is unreadable reports it.
	checkout := filepath.Join(t.TempDir(), "repo")
	writeFile(t, filepath.Join(checkout, ".claude", "settings.local.json"), "not json")
	state, _ := json.Marshal(map[string]any{"checkouts": map[string]any{checkout: map[string]any{
		"owned": spacesettings.Owned{Lists: map[string][]string{"permissions.deny": {"x"}}}}}})
	writeFile(t, layout.StateFile(), string(state))
	steps := h.machine(&fakeRun{}).checkoutSettingsUnit().UndoSteps()
	if len(steps) != 1 {
		t.Fatalf("steps = %d", len(steps))
	}
	if _, err := steps[0].Run(context.Background()); err == nil {
		t.Error("a checkout orch could not clear passed")
	}
}
