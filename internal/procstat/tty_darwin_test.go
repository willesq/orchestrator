//go:build darwin

package procstat

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTTYName finds a terminal by its device number among /dev's ttys, and
// reports none for NODEV, a device no tty has, or a folder it cannot read.
func TestTTYName(t *testing.T) {
	if got, err := ttyName(-1, "/dev"); got != "" || err != nil {
		t.Fatalf("NODEV = %q, %v", got, err)
	}
	// /dev/tty, the controlling terminal's alias, is always there.
	var st unix.Stat_t
	if err := unix.Stat("/dev/tty", &st); err != nil {
		t.Fatal(err)
	}
	if got, err := ttyName(st.Rdev, "/dev"); got != "tty" || err != nil {
		t.Fatalf("/dev/tty's device = %q, %v", got, err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ttyfake"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ttyName(12345, dir); got != "" || err != nil {
		t.Fatalf("no such device = %q, %v", got, err)
	}
	if _, err := ttyName(12345, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("an unreadable folder is no error")
	}
}
