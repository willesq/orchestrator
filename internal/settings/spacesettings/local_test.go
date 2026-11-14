package spacesettings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/willesq/orchestrator/internal/settings"
)

// TestLocalFileConflicts: a write or removal computed from content the file
// no longer holds is refused and changes nothing.
func TestLocalFileConflicts(t *testing.T) {
	dir := realDir(t)
	if b, ok, err := ReadLocal(dir); b != nil || ok || err != nil {
		t.Fatalf("no .claude: %q %v %v", b, ok, err)
	}
	if err := WriteLocal(dir, nil, false, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(dir, nil, false, []byte("{\"x\":1}\n")); !errors.Is(err, ErrConflict) {
		t.Errorf("write over an unexpected file: %v", err)
	}
	if err := WriteLocal(dir, []byte("{\"y\":1}"), true, []byte("{}")); !errors.Is(err, ErrConflict) {
		t.Errorf("write over changed content: %v", err)
	}
	if err := RemoveLocal(dir, []byte("other")); !errors.Is(err, ErrConflict) {
		t.Errorf("remove changed content: %v", err)
	}
	if b, _, _ := ReadLocal(dir); string(b) != "{}\n" {
		t.Errorf("file = %q", b)
	}
	if err := os.Chmod(localOf(dir), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(dir, []byte("{}\n"), true, []byte("{\"z\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(localOf(dir)); info.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", info.Mode())
	}
	if err := RemoveLocal(dir, []byte("{\"z\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLocal(dir, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("remove of a missing file expected as present: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, ".claude")); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
	if err := RemoveLocal(filepath.Join(dir, "absent"), nil); err == nil {
		t.Error("remove in a missing checkout")
	}
}

// TestLocalFileRefusals: a missing checkout, a settings path that is not a
// regular file and an oversized file are refused.
func TestLocalFileRefusals(t *testing.T) {
	dir := realDir(t)
	if _, _, err := ReadLocal(filepath.Join(dir, "absent")); err == nil {
		t.Error("read in a missing checkout")
	}
	if err := WriteLocal(filepath.Join(dir, "absent"), nil, false, nil); err == nil {
		t.Error("write in a missing checkout")
	}
	notDir := realDir(t)
	writeFile(t, filepath.Join(notDir, ".claude"), "file")
	if _, _, err := ReadLocal(notDir); err == nil || !strings.Contains(err.Error(), "link or not a folder") {
		t.Errorf(".claude a file: %v", err)
	}
	asDir := realDir(t)
	if err := os.MkdirAll(localOf(asDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadLocal(asDir); err == nil {
		t.Error("settings path a folder")
	}
	fifo := realDir(t)
	if err := os.MkdirAll(filepath.Join(fifo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(localOf(fifo), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadLocal(fifo); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("fifo: %v", err)
	}
	big := realDir(t)
	writeFile(t, localOf(big), strings.Repeat(" ", settings.MaxFileSize+1))
	if _, _, err := ReadLocal(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big file: %v", err)
	}
	locked := realDir(t)
	if err := os.MkdirAll(filepath.Join(locked, ".claude"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(locked, ".claude"), 0o755) })
	if os.Geteuid() != 0 {
		if err := WriteLocal(locked, nil, false, []byte("{}")); err == nil {
			t.Error("write into a read-only .claude")
		}
	}
}

// TestGitErrors: git failing, or a folder that is not a checkout, is an
// error, never a guess that the file is ignored.
func TestGitErrors(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", realDir(t))
	notRepo := realDir(t)
	g := Git{}
	if _, err := g.Tracked(context.Background(), notRepo); err == nil {
		t.Error("Tracked outside a checkout")
	}
	if _, err := g.Ignored(context.Background(), notRepo); err == nil {
		t.Error("Ignored outside a checkout")
	}
	if _, err := g.EnsureIgnored(context.Background(), notRepo); err == nil {
		t.Error("EnsureIgnored outside a checkout")
	}
	missing := Git{Bin: filepath.Join(notRepo, "no-git")}
	if _, err := missing.Tracked(context.Background(), notRepo); err == nil {
		t.Error("missing git binary")
	}
	if _, err := missing.Ignored(context.Background(), notRepo); err == nil {
		t.Error("missing git binary")
	}
	if _, err := missing.EnsureIgnored(context.Background(), notRepo); err == nil {
		t.Error("missing git binary")
	}
	// info/exclude cannot be written: the line is not added.
	repo := gitRepo(t, filepath.Join(realDir(t), "r"))
	info := filepath.Join(repo, ".git", "info")
	if err := os.RemoveAll(info); err != nil {
		t.Fatal(err)
	}
	writeFile(t, info, "not a folder")
	if _, err := g.EnsureIgnored(context.Background(), repo); err == nil {
		t.Error("exclude under a file")
	}
}

// TestAppendLine: a line is added after existing text that lacks a final
// newline; only <git dir>/info/exclude is written, never through a link.
func TestAppendLine(t *testing.T) {
	gitDir := filepath.Join(realDir(t), "g")
	writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
	p := filepath.Join(gitDir, "info", "exclude")
	if err := appendLine(p, "a"); err != nil || readFile(t, p) != "a\n" {
		t.Fatalf("%v", err)
	}
	writeFile(t, p, "x")
	if err := appendLine(p, "a"); err != nil || readFile(t, p) != "x\na\n" {
		t.Fatalf("%q %v", readFile(t, p), err)
	}
	other := realDir(t)
	for name, path := range map[string]string{
		"not exclude":    filepath.Join(gitDir, "info", "other"),
		"not info":       filepath.Join(gitDir, "x", "exclude"),
		"no HEAD":        filepath.Join(other, "info", "exclude"),
		"missing folder": filepath.Join(other, "absent", "info", "exclude"),
	} {
		if err := appendLine(path, "b"); err == nil {
			t.Errorf("%s: written", name)
		}
	}
	// info a link to another folder; exclude a link.
	linked := filepath.Join(realDir(t), "g")
	writeFile(t, filepath.Join(linked, "HEAD"), "x")
	if err := os.Symlink(other, filepath.Join(linked, "info")); err != nil {
		t.Fatal(err)
	}
	if err := appendLine(filepath.Join(linked, "info", "exclude"), "b"); err == nil {
		t.Error("written through an info link")
	}
	if _, err := os.Stat(filepath.Join(other, "exclude")); !os.IsNotExist(err) {
		t.Errorf("file created through the link: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "target"), p); err != nil {
		t.Fatal(err)
	}
	if err := appendLine(p, "b"); err == nil {
		t.Error("appended through an exclude link")
	}
	// info is a file.
	fileInfo := filepath.Join(realDir(t), "g")
	writeFile(t, filepath.Join(fileInfo, "HEAD"), "x")
	writeFile(t, filepath.Join(fileInfo, "info"), "x")
	if err := appendLine(filepath.Join(fileInfo, "info", "exclude"), "b"); err == nil {
		t.Error("written under a file")
	}
}

// TestEnsurePrivateDir: the folder ends up owner-only; a file in its place
// is refused.
func TestEnsurePrivateDir(t *testing.T) {
	dir := filepath.Join(realDir(t), "t")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", info.Mode())
	}
	file := filepath.Join(realDir(t), "f")
	writeFile(t, file, "x")
	if err := EnsurePrivateDir(file); err == nil {
		t.Error("file accepted")
	}
	link := filepath.Join(realDir(t), "l")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(link); err == nil {
		t.Error("link accepted")
	}
}

// TestDiscoverBounds: hidden and dependency folders, folders too deep and
// links below a root are skipped; a root that is a link is followed.
func TestDiscoverBounds(t *testing.T) {
	root := realDir(t)
	for _, d := range []string{"a/.git", "a/sub/.git", ".hidden/x/.git", "node_modules/y/.git", "1/2/3/4/5/6/7/.git", "1/2/3/4/5/ok/.git"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "wt", ".git"), "gitdir: /x\n")
	other := realDir(t)
	if err := os.MkdirAll(filepath.Join(other, "linked", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "linked"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, truncated := Discover([]string{root, root + "/a"})
	// root/a is also a root, and a root that is a checkout is walked.
	want := []string{filepath.Join(root, "1/2/3/4/5/ok"), filepath.Join(root, "a"), filepath.Join(root, "a/sub"), filepath.Join(root, "wt")}
	if truncated || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Discover = %v, want %v", got, want)
	}
	rootLink := filepath.Join(realDir(t), "code")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	if got, _ := Discover([]string{rootLink}); len(got) != 3 || !strings.HasPrefix(got[0], rootLink+"/") {
		t.Errorf("through a root link: %v", got)
	}
	if got, _ := Discover([]string{filepath.Join(root, "absent")}); len(got) != 0 {
		t.Errorf("missing root: %v", got)
	}
	// A root that is itself a checkout is still walked.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := Discover([]string{root}); len(got) != 4 || got[0] != root || slices.Contains(got, filepath.Join(root, "a/sub")) {
		t.Errorf("root checkout: %v", got)
	}
}
