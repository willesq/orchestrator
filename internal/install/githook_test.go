package install

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// hookTree records every entry of a hooks directory: name, mode and
// content (a link's target for a symlink).
func hookTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			out[e.Name()] = "link " + target
			continue
		}
		b, _ := os.ReadFile(p)
		out[e.Name()] = fi.Mode().String() + " " + string(b)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func treeNames(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

const userHook = "#!/bin/sh\necho user hook\n"

// hooksWithUserHook is a hooks directory holding an executable commit-msg
// hook of the user's.
func hooksWithUserHook(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "commit-msg"), []byte(userHook), 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

func applyGitHooks(t *testing.T, dir, orch string, uninstall bool, backups string) string {
	t.Helper()
	p, err := PlanGitHooks(dir, orch, uninstall)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := p.Apply(&out, backups); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestGitHookInstallProperties is SP-045.1 a1: install is dry-run by
// default, idempotent, backs up and chains an existing hook rather than
// replacing it, and uninstalls cleanly.
func TestGitHookInstallProperties(t *testing.T) {
	dir := hooksWithUserHook(t)
	backups := filepath.Join(t.TempDir(), "backups")
	orch := "/opt/orch/bin/orch"
	before := hookTree(t, dir)

	// 1. Dry run: the plan is printed and nothing is written.
	p, err := PlanGitHooks(dir, orch, false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p.Print(&out)
	if !sameTree(before, hookTree(t, dir)) {
		t.Fatalf("dry run wrote files: %v", treeNames(hookTree(t, dir)))
	}
	if _, err := os.Stat(backups); !os.IsNotExist(err) {
		t.Fatalf("dry run made backups: %v", err)
	}
	if p.Changes() != 2 || !strings.Contains(out.String(), "DRY RUN") || !strings.Contains(out.String(), "commit-msg.orch-prev") {
		t.Fatalf("dry run output (changes %d):\n%s", p.Changes(), out.String())
	}

	// 2 and 3. Apply backs up the user's hook and chains it.
	applyGitHooks(t, dir, orch, false, backups)
	after := hookTree(t, dir)
	if got := after["commit-msg.orch-prev"]; got != "-rwxr-x--- "+userHook {
		t.Fatalf("chained hook = %q", got)
	}
	for _, name := range GitHookNames {
		got := after[name]
		if !strings.HasPrefix(got, "-rwxr-xr-x ") || !IsOrchGitHook([]byte(got)) || !strings.Contains(got, shQuote(orch)) {
			t.Fatalf("%s = %q", name, got)
		}
	}
	found := false
	_ = filepath.Walk(backups, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			found = found || string(b) == userHook
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("backup %s mode %v", p, fi.Mode())
			}
		}
		return nil
	})
	if !found {
		t.Fatal("the user's hook was not backed up")
	}

	// 4. Idempotent: a second install plans and changes nothing.
	p, err = PlanGitHooks(dir, orch, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Changes() != 0 {
		t.Fatalf("second install plans %d changes", p.Changes())
	}
	if msg := applyGitHooks(t, dir, orch, false, backups); !strings.Contains(msg, "nothing changed") || !sameTree(after, hookTree(t, dir)) {
		t.Fatalf("second apply changed something: %s", msg)
	}

	// 5. Uninstall puts the directory back as it was, and again is a no-op.
	applyGitHooks(t, dir, "", true, backups)
	if got := hookTree(t, dir); !sameTree(before, got) {
		t.Fatalf("after uninstall: %v, want %v", got, before)
	}
	p, _ = PlanGitHooks(dir, "", true)
	if p.Changes() != 0 {
		t.Fatalf("second uninstall plans %d changes", p.Changes())
	}
}

func TestGitHookInstallEmptyAndUpdate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared-hooks") // does not exist yet
	backups := t.TempDir()
	applyGitHooks(t, dir, "/a/orch", false, backups)
	if got := hookTree(t, dir); len(got) != 2 {
		t.Fatalf("hooks = %v", treeNames(got))
	}
	// A new orch path updates orch's own hooks in place, chaining nothing.
	p, err := PlanGitHooks(dir, "/b/orch", false)
	if err != nil || p.Changes() != 2 {
		t.Fatalf("update plan: %d %v", p.Changes(), err)
	}
	var out bytes.Buffer
	p.Print(&out)
	if !strings.Contains(out.String(), "update orch's commit-msg") {
		t.Fatalf("print: %s", out.String())
	}
	applyGitHooks(t, dir, "/b/orch", false, backups)
	got := hookTree(t, dir)
	if len(got) != 2 || !strings.Contains(got["pre-push"], "'/b/orch'") {
		t.Fatalf("after update: %v", got)
	}
	// A hook of orch's that lost its execute bit is repaired.
	if err := os.Chmod(filepath.Join(dir, "pre-push"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, _ := PlanGitHooks(dir, "/b/orch", false); p.Changes() != 1 {
		t.Fatalf("non-executable hook not repaired: %d", p.Changes())
	}
	applyGitHooks(t, dir, "", true, backups)
	if got := hookTree(t, dir); len(got) != 0 {
		t.Fatalf("after uninstall: %v", treeNames(got))
	}
}

func TestGitHookInstallRefusals(t *testing.T) {
	dir := hooksWithUserHook(t)
	if err := os.WriteFile(filepath.Join(dir, "commit-msg.orch-prev"), []byte("older"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanGitHooks(dir, "/a/orch", false); err == nil || !strings.Contains(err.Error(), "move one of them away") {
		t.Fatalf("hook and chained hook both present: %v", err)
	}
	if _, err := PlanGitHooks("relative/hooks", "/a/orch", false); err == nil {
		t.Fatal("relative hooks dir accepted")
	}
	if _, err := PlanGitHooks(dir, "orch", false); err == nil {
		t.Fatal("relative orch accepted")
	}
	sub := filepath.Join(t.TempDir(), "h")
	if err := os.MkdirAll(filepath.Join(sub, "pre-push"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanGitHooks(sub, "/a/orch", false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory hook: %v", err)
	}

	// A hook changed between plan and apply is not touched.
	d2 := hooksWithUserHook(t)
	p, err := PlanGitHooks(d2, "/a/orch", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d2, "commit-msg"), []byte("#!/bin/sh\nchanged\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(&bytes.Buffer{}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("apply over a changed hook: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(d2, "commit-msg")); string(b) != "#!/bin/sh\nchanged\n" {
		t.Fatalf("changed hook overwritten: %q", b)
	}
}

func TestGitHookUninstallLeavesOthersAlone(t *testing.T) {
	dir := hooksWithUserHook(t)
	p, err := PlanGitHooks(dir, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p.Print(&out)
	if p.Changes() != 0 || !strings.Contains(out.String(), "not orch's; left alone") || !strings.Contains(out.String(), "no pre-push hook") {
		t.Fatalf("uninstall of foreign hooks: %d\n%s", p.Changes(), out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "commit-msg")); string(b) != userHook {
		t.Fatal("foreign hook changed")
	}
}

func TestGitHookChainsSymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "shared-pre-push")
	if err := os.WriteFile(target, []byte(userHook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "pre-push")); err != nil {
		t.Fatal(err)
	}
	before := hookTree(t, dir)
	backups := t.TempDir()
	applyGitHooks(t, dir, "/a/orch", false, backups)
	if got := hookTree(t, dir)["pre-push.orch-prev"]; got != "link "+target {
		t.Fatalf("chained link = %q", got)
	}
	if b, _ := os.ReadFile(target); string(b) != userHook {
		t.Fatal("link target changed")
	}
	applyGitHooks(t, dir, "", true, backups)
	if got := hookTree(t, dir); !sameTree(before, got) {
		t.Fatalf("after uninstall: %v", got)
	}
}

func TestGitHookScript(t *testing.T) {
	s := string(GitHookScript("pre-push", "/it's/orch"))
	for _, want := range []string{"#!/bin/sh\n", gitHookMarker, `orch='/it'\''s/orch'`, `prev="$0.orch-prev"`,
		`guard git-hook run pre-push -- "$@"`, "input=$(cat)"} {
		if !strings.Contains(s, want) {
			t.Errorf("pre-push script lacks %q:\n%s", want, s)
		}
	}
	c := string(GitHookScript("commit-msg", "/a/orch"))
	run, prev := strings.Index(c, `exec "$orch" guard git-hook run commit-msg -- "$@"`), strings.Index(c, `"$prev" "$@" || exit $?`)
	if run < 0 || prev < 0 || prev > run {
		t.Errorf("commit-msg script:\n%s", c)
	}
}

// TestGitHookInstallReview holds the SP-045 security review's install
// cases: a chained hook left behind is named in the plan; a FIFO or an
// oversized hook is refused; a chained hook that appears after the plan
// is never replaced.
func TestGitHookInstallReview(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pre-push.orch-prev"), []byte(userHook), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := PlanGitHooks(dir, "/a/orch", false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p.Print(&out)
	if !strings.Contains(out.String(), "also run the pre-push.orch-prev already there") {
		t.Errorf("leftover chained hook not named:\n%s", out.String())
	}

	if err := syscall.Mkfifo(filepath.Join(dir, "commit-msg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanGitHooks(dir, "/a/orch", false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("FIFO hook: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "commit-msg"))
	if err := os.WriteFile(filepath.Join(dir, "commit-msg"), bytes.Repeat([]byte("x"), maxHookFile+1), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanGitHooks(dir, "/a/orch", false); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("oversized hook: %v", err)
	}

	d2 := hooksWithUserHook(t)
	p, err = PlanGitHooks(d2, "/a/orch", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d2, "commit-msg.orch-prev"), []byte("late"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(&bytes.Buffer{}, t.TempDir()); err == nil {
		t.Fatal("chained over a hook that appeared after the plan")
	}
	if b, _ := os.ReadFile(filepath.Join(d2, "commit-msg.orch-prev")); string(b) != "late" {
		t.Fatalf("late chained hook replaced: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(d2, "commit-msg")); string(b) != userHook {
		t.Fatalf("user's hook lost: %q", b)
	}
}
