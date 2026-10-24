package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The app and launchd run orch with launchd's minimal PATH; every command must
// still find gh and brew, as orchd does.
func TestCommandsWidenAMinimalPATH(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	if _, err := run(factory(&fakeClient{}, nil), "", "version"); err != nil {
		t.Fatalf("version: %v", err)
	}
	got := filepath.SplitList(os.Getenv("PATH"))
	if got[0] != "/usr/bin" {
		t.Errorf("PATH = %v, want the caller's entries kept first", got)
	}
	if !strings.Contains(os.Getenv("PATH"), bin) {
		t.Errorf("PATH = %q, want %s appended", os.Getenv("PATH"), bin)
	}
}
