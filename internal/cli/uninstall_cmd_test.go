package cli

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/install"
)

// setupHome is a temporary home with Claude Code and Codex present, holding
// the user's own text in their entry files, plus fakes for everything that
// would reach the real machine.
func setupHome(t *testing.T) (home, binDir string, calls *[]string) {
	t.Helper()
	home, binDir = t.TempDir(), t.TempDir()
	for rel, body := range map[string]string{
		".claude/CLAUDE.md": "# Mine\n",
		".codex/AGENTS.md":  "codex rules\n",
		".zprofile":         "export A=1\n",
		// Uninstall keeps these deny rules, so the round trip starts with them.
		".claude/settings.json": "{\n  \"permissions\": {\n    \"deny\": [\n      \"Read(~/.config/gh/**)\",\n      \"Read(~/.netrc)\",\n      \"Read(~/.docker/config.json)\"\n    ]\n  }\n}\n",
	} {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var recorded []string
	calls = &recorded
	prevRun, prevUp, prevModel, prevTTY := setupRunner, setupDaemonUp, setupModel, stdinIsTerminal
	setupRunner = func(_ context.Context, name string, args ...string) (string, error) {
		recorded = append(recorded, name+" "+strings.Join(args, " "))
		return "", nil
	}
	setupDaemonUp = func() bool { return false }
	present := false
	setupModel = func(dataDir string, _ bool) (string, func() bool, func(context.Context) error) {
		dir := filepath.Join(dataDir, "models", "minilm")
		return dir, func() bool { return present }, func(context.Context) error {
			present = true
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("m"), 0o644)
		}
	}
	stdinIsTerminal = func(io.Reader) bool { return false }
	t.Cleanup(func() { setupRunner, setupDaemonUp, setupModel, stdinIsTerminal = prevRun, prevUp, prevModel, prevTTY })
	return home, binDir, calls
}

// homeTree maps every file under home, less Claude Code's plugin records
// and orch's backups, to its content.
func homeTree(t *testing.T, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	skip := map[string]bool{
		filepath.Join(home, ".claude", "plugins"):                        true,
		filepath.Join(home, ".local", "share", "orch", "backups"):        true,
		filepath.Join(home, ".local", "share", "orch", "models"):         true,
		filepath.Join(home, ".local", "share", "orch", "git-hooks.json"): true,
	}
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip[p] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			out[strings.TrimPrefix(p, home)] = string(b)
		}
		return nil
	})
	return out
}

func sameTree(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %q, got %q", k, v, got[k])
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: left behind", k)
		}
	}
}

func TestInstallOneCommandAndUninstallMirror(t *testing.T) {
	home, binDir, calls := setupHome(t)
	cf := factory(&fakeClient{}, nil)
	before := homeTree(t, home)

	out, err := run(cf, "", "install", "--home", home, "--bin-dir", binDir)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{`DRY RUN — target "claude-code+codex"`, ".codex/hooks.json", ".claude/settings.json",
		"download the embedding model", "Manual steps", "gh auth login"} {
		if !strings.Contains(out, want) {
			t.Errorf("install plan lacks %q", want)
		}
	}
	sameTree(t, before, homeTree(t, home))

	if out, err = run(cf, "", "install", "--home", home, "--bin-dir", binDir, "--yes"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "orch", "models", "minilm", "model.onnx")); err != nil {
		t.Error("install did not fetch the model")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Error("install wired gemini, which is not present")
	}
	if out, err = run(cf, "", "install", "--home", home, "--bin-dir", binDir, "--apply"); err != nil || !strings.Contains(out, "already fully installed") {
		t.Fatalf("second install: %v\n%s", err, out)
	}

	installed := homeTree(t, home)
	if out, err = run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir); err != nil || !strings.Contains(out, "uninstall") {
		t.Fatalf("uninstall dry run: %v\n%s", err, out)
	}
	sameTree(t, installed, homeTree(t, home))

	if out, err = run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--apply"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	sameTree(t, before, homeTree(t, home))
	if !strings.Contains(out, "Left for you") || !strings.Contains(out, "--purge-data") {
		t.Errorf("uninstall output lacks what it leaves:\n%s", out)
	}
	if out, err = run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--apply"); err != nil || !strings.Contains(out, "nothing changed") {
		t.Fatalf("second uninstall: %v\n%s", err, out)
	}
	if len(*calls) != 0 {
		t.Errorf("a relocated home ran launchctl or brew: %v", *calls)
	}
}

func TestInstallConfirmation(t *testing.T) {
	home, binDir, _ := setupHome(t)
	stdinIsTerminal = func(io.Reader) bool { return true }
	cf := factory(&fakeClient{}, nil)
	before := homeTree(t, home)
	out, err := run(cf, "n\n", "install", "--home", home, "--bin-dir", binDir, "--no-model")
	if err != nil || !strings.Contains(out, "Nothing changed") {
		t.Fatalf("declined install: %v\n%s", err, out)
	}
	sameTree(t, before, homeTree(t, home))
	if out, err = run(cf, "y\n", "install", "--home", home, "--bin-dir", binDir, "--no-model"); err != nil {
		t.Fatalf("confirmed install: %v\n%s", err, out)
	}
	if _, err := os.Stat(install.SharedPolicyPath(home)); err != nil {
		t.Error("confirmed install wrote nothing")
	}
	if out, err = run(cf, "n\n", "uninstall", "--home", home, "--bin-dir", binDir); err != nil || !strings.Contains(out, "Nothing changed") {
		t.Fatalf("declined uninstall: %v\n%s", err, out)
	}
	if out, err = run(cf, "y\n", "uninstall", "--home", home, "--bin-dir", binDir); err != nil {
		t.Fatalf("confirmed uninstall: %v\n%s", err, out)
	}
	sameTree(t, before, homeTree(t, home))
}

func TestUninstallPurgeData(t *testing.T) {
	home, binDir, _ := setupHome(t)
	cf := factory(&fakeClient{}, nil)
	data := filepath.Join(home, ".local", "share", "orch")
	outside := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(outside, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"orch.db": "db", "qdrant/x": "v", "backups/old": "b"} {
		p := filepath.Join(data, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A link inside the data dir is removed as a link, never followed.
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	out, err := run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--purge-data")
	if err != nil || !strings.Contains(out, "data: delete "+filepath.Join(data, "orch.db")) {
		t.Fatalf("purge dry run: %v\n%s", err, out)
	}
	if _, err = run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--purge-data", "--apply"); err == nil {
		t.Fatal("purge ran without confirmation")
	}
	if _, err := os.Stat(filepath.Join(data, "orch.db")); err != nil {
		t.Fatal("refused purge deleted data")
	}
	if _, err = run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--purge-data", "--target", "codex"); err == nil {
		t.Fatal("purge with a single target accepted")
	}
	stdinIsTerminal = func(io.Reader) bool { return true }
	if out, err = run(cf, "no\n", "uninstall", "--home", home, "--bin-dir", binDir, "--purge-data", "--apply"); err != nil || !strings.Contains(out, "Nothing changed") {
		t.Fatalf("declined purge: %v\n%s", err, out)
	}
	if out, err = run(cf, "delete\n", "uninstall", "--home", home, "--bin-dir", binDir, "--purge-data", "--apply"); err != nil {
		t.Fatalf("purge: %v\n%s", err, out)
	}
	entries, _ := os.ReadDir(data)
	if len(entries) != 1 || entries[0].Name() != "backups" {
		t.Errorf("data dir after purge holds %v", entries)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "mine" {
		t.Error("purge followed a link out of the data dir")
	}
}

func TestUninstallOneTarget(t *testing.T) {
	home, binDir, _ := setupHome(t)
	cf := factory(&fakeClient{}, nil)
	if out, err := run(cf, "", "install", "--home", home, "--bin-dir", binDir, "--yes", "--no-model"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if out, err := run(cf, "", "uninstall", "--home", home, "--bin-dir", binDir, "--target", "codex", "--yes"); err != nil {
		t.Fatalf("uninstall codex: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md"))
	if string(b) != "codex rules\n" {
		t.Errorf("codex AGENTS.md = %q", b)
	}
	if _, err := os.Stat(install.SharedPolicyPath(home)); err != nil {
		t.Error("single-harness uninstall removed the shared policy")
	}
	if _, err := run(cf, "", "uninstall", "--home", home, "--target", "emacs"); err == nil {
		t.Error("unknown target accepted")
	}
}

func TestInstallSingleTargetKeepsPerHarnessPlan(t *testing.T) {
	home, binDir, _ := setupHome(t)
	out, err := run(factory(&fakeClient{}, nil), "", "install", "--home", home, "--bin-dir", binDir, "--target", "codex")
	if err != nil || !strings.Contains(out, `DRY RUN — target "codex"`) || strings.Contains(out, "embedding model") {
		t.Fatalf("single target plan: %v\n%s", err, out)
	}
}

func TestDetectAdapters(t *testing.T) {
	home := t.TempDir()
	if got := adapterNames(detectAdapters(home)); got != "claude-code" {
		t.Errorf("empty home: %s", got)
	}
	for _, d := range []string{".gemini", ".codex"} {
		if err := os.Mkdir(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := adapterNames(detectAdapters(home)); got != "codex+gemini" {
		t.Errorf("codex and gemini: %s", got)
	}
}

func TestSetupDefaults(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ORCH_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	if stdinIsTerminal(strings.NewReader("")) {
		t.Error("a reader is a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if stdinIsTerminal(f) {
		t.Error("a regular file is a terminal")
	}
	if out, err := setupRunner(context.Background(), "echo", "hi"); err != nil || out != "hi" {
		t.Errorf("runner: %q %v", out, err)
	}
	if setupDaemonUp() {
		t.Error("orchd answered on a socket that does not exist")
	}
	data := t.TempDir()
	dir, present, fetch := setupModel(data, true)
	if dir != filepath.Join(data, "models", "minilm") || present() || fetch == nil {
		t.Errorf("relocated model dir %s", dir)
	}
	if dir, _, _ = setupModel(data, false); dir == "" {
		t.Error("no model dir")
	}
	if home, relocated, err := resolveHome(""); err != nil || relocated || home == "" {
		t.Errorf("default home: %s %v %v", home, relocated, err)
	}
	real, _ := os.UserHomeDir()
	if _, relocated, _ := resolveHome(real + "/"); relocated {
		t.Error("the real home spelled with a slash counts as relocated")
	}
	if contextOf(&cobra.Command{}) == nil {
		t.Error("no context")
	}
}

func TestMachineForRealHome(t *testing.T) {
	_, _, _ = setupHome(t)
	real, _ := os.UserHomeDir()
	m := machineFor(setupFlags{}, real, false, install.Bins{Orchd: "/x/orchd"})
	if m.Relocated || len(m.AppPaths) != 2 || m.DataDir == "" {
		t.Errorf("machine for the real home: %+v", m)
	}
}
