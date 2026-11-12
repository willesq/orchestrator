package spacesettings

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeProfile: the profile's settings are the user's with the space's
// rules merged in, rewritten at each launch; the user's instructions,
// agents and the rest are linked, and a profile's own file is kept.
func TestClaudeProfile(t *testing.T) {
	w := newWorld(t)
	user := filepath.Join(w.home, "dot-claude")
	writeFile(t, filepath.Join(user, "settings.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/bin/orch-hook session-start"}]}]},"model":"opus"}`)
	writeFile(t, filepath.Join(user, "CLAUDE.md"), "# mine")
	if err := os.MkdirAll(filepath.Join(user, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	frag := ClaudeFragment(For(w.res, "joba", w.layout))
	dir, err := WriteClaudeProfile(w.layout, "joba", user, frag)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "settings.json"))), &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "opus" || got["hooks"] == nil || at(got, "sandbox", "enabled") != true {
		t.Errorf("profile settings = %v", got)
	}
	if info, _ := os.Stat(filepath.Join(dir, "settings.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("settings mode %v", info.Mode())
	}
	if info, _ := os.Stat(w.layout.SpaceDir("joba")); info.Mode().Perm() != 0o700 {
		t.Errorf("space dir mode %v", info.Mode())
	}
	if l, err := os.Readlink(filepath.Join(dir, "CLAUDE.md")); err != nil || l != filepath.Join(user, "CLAUDE.md") {
		t.Errorf("CLAUDE.md link %q %v", l, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "agents")); !os.IsNotExist(err) {
		t.Errorf("agents linked though the user has none: %v", err)
	}

	// The user's settings change; the profile follows. A link the user's
	// folder no longer backs is removed; the profile's own file stays.
	writeFile(t, filepath.Join(user, "settings.json"), `{"model":"sonnet"}`)
	if err := os.Remove(filepath.Join(user, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "commands"), "profile's own")
	writeFile(t, filepath.Join(user, "commands", "x.md"), "x")
	if err := os.Remove(filepath.Join(dir, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere", filepath.Join(dir, "skills")); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteClaudeProfile(w.layout, "joba", user, Fragment{}); err != nil {
		t.Fatal(err)
	}
	if s := readFile(t, filepath.Join(dir, "settings.json")); !strings.Contains(s, "sonnet") || strings.Contains(s, "sandbox") {
		t.Errorf("rewritten settings = %s", s)
	}
	if _, err := os.Lstat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("stale link kept: %v", err)
	}
	if readFile(t, filepath.Join(dir, "commands")) != "profile's own" {
		t.Error("profile's own file replaced")
	}
	if l, _ := os.Readlink(filepath.Join(dir, "skills")); l != filepath.Join(user, "skills") {
		t.Errorf("skills link = %q", l)
	}

	// No user settings at all; then settings the profile cannot merge into.
	empty := filepath.Join(w.home, "none")
	if _, err := WriteClaudeProfile(w.layout, "jobb", empty, Fragment{}); err != nil {
		t.Fatal(err)
	}
	if s := readFile(t, filepath.Join(w.layout.ClaudeDir("jobb"), "settings.json")); s != "{}\n" {
		t.Errorf("empty profile settings = %q", s)
	}
	writeFile(t, filepath.Join(user, "settings.json"), `{"sandbox":1}`)
	if _, err := WriteClaudeProfile(w.layout, "joba", user, frag); err == nil {
		t.Error("unmergeable user settings accepted")
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

// TestClaudeProfileErrors: a space folder that is a file, a profile folder
// that is a file, and user settings that cannot be read are errors.
func TestClaudeProfileErrors(t *testing.T) {
	w := newWorld(t)
	user := filepath.Join(w.home, "dot-claude")
	writeFile(t, w.layout.SpaceDir("joba"), "file")
	if _, err := WriteClaudeProfile(w.layout, "joba", user, Fragment{}); err == nil {
		t.Error("space folder is a file")
	}
	writeFile(t, w.layout.ClaudeDir("jobb"), "file")
	if _, err := WriteClaudeProfile(w.layout, "jobb", user, Fragment{}); err == nil {
		t.Error("profile folder is a file")
	}
	if err := os.MkdirAll(filepath.Join(user, "settings.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteClaudeProfile(w.layout, "jobc", user, Fragment{}); err == nil {
		t.Error("user settings a folder")
	}
	if err := replaceFile(filepath.Join(w.home, "absent", "x"), nil); err == nil {
		t.Error("replace in a missing folder")
	}
	unreadable := filepath.Join(w.home, "locked")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(unreadable, "x"), filepath.Join(unreadable, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(unreadable, "loop"), filepath.Join(unreadable, "x")); err != nil {
		t.Fatal(err)
	}
	if err := linkShared(filepath.Join(unreadable, "x"), filepath.Join(w.home, "at")); err == nil {
		t.Error("link to a looping target")
	}
}
