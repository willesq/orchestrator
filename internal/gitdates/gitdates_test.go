package gitdates

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}, env...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commitAt(t *testing.T, dir string, when time.Time, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, nil, "add", "-A")
	d := when.Format(time.RFC3339)
	git(t, dir, []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}, "commit", "-q", "--no-gpg-sign", "-m", "c")
}

func TestRead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q")
	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	commitAt(t, dir, t1, map[string]string{"README.md": "a", "docs/Plan ü.MD": "b", "main.go": "x"})
	commitAt(t, dir, t2, map[string]string{"README.md": "a2"})
	commitAt(t, dir, t3, map[string]string{"main.go": "y"}) // HEAD touches no markdown
	d := Read(context.Background(), dir)
	if !d.Head.Equal(t3) {
		t.Fatalf("head = %v", d.Head)
	}
	if got := d.For("README.md"); !got.Equal(t2) {
		t.Errorf("README.md = %v, want %v", got, t2)
	}
	if got := d.For(filepath.Join("docs", "Plan ü.MD")); !got.Equal(t1) {
		t.Errorf("non-ASCII, upper-case .MD = %v, want %v (files %v)", got, t1, d.Files)
	}
	if _, ok := d.Files["main.go"]; ok {
		t.Error("non-markdown file read")
	}
	if got := d.For("untracked.md"); !got.Equal(t3) {
		t.Errorf("unknown file falls back to HEAD: %v", got)
	}
	// A subdirectory reads paths relative to itself.
	if got := Read(context.Background(), filepath.Join(dir, "docs")).For("Plan ü.MD"); !got.Equal(t1) {
		t.Errorf("subdir read = %v", got)
	}
}

func TestReadOutsideARepo(t *testing.T) {
	d := Read(context.Background(), t.TempDir())
	if !d.Head.IsZero() || !d.For("a.md").IsZero() {
		t.Fatalf("non-repo = %+v", d)
	}
}

func TestParse(t *testing.T) {
	out := []byte("\x001790000000\n\na.md\nb.md\n\x00garbage\nc.md\n\x001780000000\n\na.md\nd.md\n")
	f := parse(out)
	if f["a.md"].Unix() != 1790000000 || f["d.md"].Unix() != 1780000000 || len(f) != 3 {
		t.Fatalf("parse = %v", f)
	}
	if !parseUnix("12").IsZero() || !parseUnix("x").IsZero() {
		t.Fatal("implausible times accepted")
	}
}

// A local repo's config is untrusted. A partial clone missing an object would
// lazily fetch it through its remote, and an ext:: remote runs a program; Read
// must never let it.
func TestReadNeverRunsTheRepoConfigsPrograms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q")
	commitAt(t, dir, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), map[string]string{"docs/a.md": "a"})
	commitAt(t, dir, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), map[string]string{"docs/b.md": "b"})
	// Remove the first commit's docs tree, so listing its files needs a fetch.
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD~1:docs").Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(string(out))
	if err := os.Remove(filepath.Join(dir, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"extensions.partialClone", "origin"}, {"remote.origin.promisor", "true"},
		{"remote.origin.url", "ext::" + script}, {"protocol.ext.allow", "always"},
		{"core.sshCommand", script}, {"core.pager", script}, {"core.fsmonitor", script},
	} {
		git(t, dir, nil, "config", kv[0], kv[1])
	}
	_ = Read(context.Background(), dir)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("reading dates ran a program the repository's config named")
	}
}

func TestFutureCommitsAreDatedNow(t *testing.T) {
	future := time.Now().Add(1000 * time.Hour).Unix()
	if got := parseUnix(strconv.FormatInt(future, 10)); got.After(time.Now()) {
		t.Fatalf("future commit dated %v", got)
	}
}
