package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/install"
)

// TestUninstallScanFindsOldGitHooks installs orch's hooks into a fake
// repository the old way (nothing recorded), with a hook of the user's
// chained, and checks `orch uninstall --scan` previews, then removes them
// and puts the user's hook back.
func TestUninstallScanFindsOldGitHooks(t *testing.T) {
	home, binDir, _ := setupHome(t)
	cf := factory(&fakeClient{}, nil)
	scan := t.TempDir()
	hooks := filepath.Join(scan, "acme", "app", ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	user := "#!/bin/sh\necho mine\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(user), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := install.PlanGitHooks(hooks, filepath.Join(binDir, "orch"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(&bytes.Buffer{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	out, err := run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--scan", scan)
	if err != nil || !strings.Contains(out, "from "+hooks) || !strings.Contains(out, "--scan "+scan) {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, "commit-msg")); !install.IsOrchGitHook(b) {
		t.Fatal("the dry run changed the hook")
	}
	if out, err = run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--scan", scan, "--apply"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, "commit-msg")); string(b) != user {
		t.Errorf("commit-msg = %q, want the user's hook back", b)
	}
	for _, gone := range []string{"pre-push", "commit-msg.orch-prev"} {
		if _, err := os.Lstat(filepath.Join(hooks, gone)); err == nil {
			t.Errorf("%s left behind", gone)
		}
	}
}

func TestRecordedPlacesReadsOrchDBReadOnly(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data dir")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	// No orch.db: nothing is created.
	p := recordedPlaces(context.Background(), home, data)
	if _, err := os.Stat(filepath.Join(data, "orch.db")); err == nil {
		t.Fatal("the search created orch.db")
	}
	if !slices.Contains(p.Roots, filepath.Join(home, "code")) {
		t.Errorf("roots = %v", p.Roots)
	}
	conn, err := sql.Open("sqlite", filepath.Join(data, "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`CREATE TABLE sources (id TEXT, kind TEXT, uri TEXT); INSERT INTO sources VALUES ('repo|/w/r', 'repo', '/w/r')`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if p := recordedPlaces(context.Background(), home, data); !slices.Contains(p.Repos, "/w/r") {
		t.Errorf("repos = %v", p.Repos)
	}
}

func TestGitSearchEnv(t *testing.T) {
	env := []string{"PATH=/bin", "HOME=/real", "GIT_DIR=/x", "GIT_CONFIG_GLOBAL=/g", "XDG_CONFIG_HOME=/c"}
	got := gitSearchEnv(env, "/relocated-home", true)
	for _, want := range []string{"PATH=/bin", "HOME=/relocated-home", "XDG_CONFIG_HOME=/relocated-home/.config", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(got, want) {
			t.Errorf("relocated env lacks %s: %v", want, got)
		}
	}
	for _, bad := range []string{"HOME=/real", "GIT_DIR=/x", "GIT_CONFIG_GLOBAL=/g", "XDG_CONFIG_HOME=/c"} {
		if slices.Contains(got, bad) {
			t.Errorf("relocated env keeps %s", bad)
		}
	}
	got = gitSearchEnv(env, "/ignored", false)
	if !slices.Contains(got, "HOME=/real") || slices.Contains(got, "GIT_DIR=/x") {
		t.Errorf("env = %v", got)
	}
	// The real runner runs git as an argument array; asking its version
	// touches no repository.
	if out, err := uninstallGit(t.TempDir(), true)(context.Background(), "--version"); err != nil || !strings.HasPrefix(out, "git version") {
		t.Errorf("git --version: %q %v", out, err)
	}
}
