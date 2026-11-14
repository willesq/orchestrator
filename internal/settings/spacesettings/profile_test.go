package spacesettings

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeLaunch (SP-046.2.a1, D-59): 'orch run' gives Claude Code the
// launcher's rules as one compact --settings JSON, the space's environment,
// and CLAUDE_CONFIG_DIR only when the space names its own folder; with the
// sandbox layer off (an empty fragment) there is no --settings.
func TestClaudeLaunch(t *testing.T) {
	w := newWorld(t)
	a := For(w.res, "joba", w.layout)
	args, env, err := ClaudeLaunch(a, LauncherFragment(a))
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "--settings" || strings.Contains(args[1], "\n") {
		t.Fatalf("args = %q", args)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(args[1]), &got); err != nil {
		t.Fatal(err)
	}
	if at(got, "sandbox", "enabled") != true || at(got, "sandbox", "allowUnsandboxedCommands") != false {
		t.Errorf("settings = %v", got)
	}
	if !strings.Contains(args[1], "Read(/"+filepath.Join(w.code, "b")+"/**)") {
		t.Errorf("settings do not deny job b: %s", args[1])
	}
	if env[envClaudeConfigDir] != filepath.Join(w.home, "claude", "a") || env[envGHConfigDir] != filepath.Join(w.home, "gh", "a") || env[envTmpDir] != a.TmpDir {
		t.Errorf("joba env = %v", env)
	}
	b := For(w.res, "jobb", w.layout)
	args, env, err = ClaudeLaunch(b, Fragment{})
	if err != nil || args != nil {
		t.Errorf("off: %q %v", args, err)
	}
	if _, ok := env[envClaudeConfigDir]; ok {
		t.Errorf("jobb env names a Claude folder: %v", env)
	}
	if _, ok := env[envGHConfigDir]; ok || env[envTmpDir] != b.TmpDir {
		t.Errorf("jobb env = %v", env)
	}
	if _, _, err := ClaudeLaunch(b, Fragment{Scalars: map[string]any{"a": 1, "a.b": 2}}); err == nil {
		t.Error("an unmergeable fragment was accepted")
	}
}

// TestCodexArgs: the -c overrides name a profile per space that denies
// every denied folder, with paths quoted for TOML.
func TestCodexArgs(t *testing.T) {
	args := CodexArgs(Rules{Space: "job-a", Deny: []string{`/w/b "x"`, "/w/c"}})
	want := []string{"-c", `default_permissions="orch_job_a"`, "-c", `permissions.orch_job_a={extends=":workspace",filesystem={"/w/b \"x\""="deny","/w/c"="deny"}}`}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Errorf("args = %q", args)
	}
}

// TestCodexSandboxPerSpaceTmp (SP-046.1.a6) runs the real Codex sandbox
// (no model, a temp CODEX_HOME) when codex is installed: a session of job A
// writes its own TMPDIR but can neither read a file job B's session wrote
// under job B's TMPDIR nor write there, nor read job B's root.
func TestCodexSandboxPerSpaceTmp(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	w := newWorld(t)
	// The temp folders sit under /tmp, as with DefaultLayout.
	w.layout.Tmp = filepath.Join(shortTmp(t), "r")
	a, b := For(w.res, "joba", w.layout), For(w.res, "jobb", w.layout)
	for _, n := range []string{"joba", "jobb"} {
		if err := w.layout.EnsureTmpDir(n); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(b.TmpDir, "b-wrote.txt"), "B-SECRET")
	writeFile(t, filepath.Join(w.code, "b", "repo", "f.txt"), "B-ROOT")
	work := filepath.Join(w.code, "a", "repo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `echo mine > "$TMPDIR/a.txt" && cat "$TMPDIR/a.txt"; ` +
		`cat "` + filepath.Join(b.TmpDir, "b-wrote.txt") + `"; ` +
		`echo x > "` + filepath.Join(b.TmpDir, "a-wrote.txt") + `"; ` +
		`cat "` + filepath.Join(w.code, "b", "repo", "f.txt") + `"; true`
	args := append([]string{"sandbox"}, CodexArgs(a)...)
	args = append(args, "--", "sh", "-c", script)
	cmd := exec.Command(codex, args...)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "CODEX_HOME="+filepath.Join(w.home, "codex-home"))
	for k, v := range a.Env() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := os.MkdirAll(filepath.Join(w.home, "codex-home"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("codex sandbox: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "mine") {
		t.Errorf("own TMPDIR not writable:\n%s", s)
	}
	if strings.Contains(s, "B-SECRET") || strings.Contains(s, "B-ROOT") {
		t.Errorf("read job B's files:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(b.TmpDir, "a-wrote.txt")); !os.IsNotExist(err) {
		t.Errorf("wrote into job B's TMPDIR: %v", err)
	}
	t.Logf("codex sandbox output:\n%s", s)
}
