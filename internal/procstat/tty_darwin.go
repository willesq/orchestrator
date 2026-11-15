//go:build darwin

package procstat

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// TTY returns the name of pid's controlling terminal under /dev (e.g.
// ttys003), or "" when it has none.
func TTY(pid int) (string, error) {
	k, err := kinfo(pid)
	if err != nil {
		return "", err
	}
	return ttyName(k.Eproc.Tdev, "/dev")
}

// ttyName finds the terminal device numbered dev in dir; NODEV (-1) is
// no terminal.
func ttyName(dev int32, dir string) (string, error) {
	if dev == -1 {
		return "", nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "tty") && name != "console" {
			continue
		}
		var st unix.Stat_t
		if unix.Stat(filepath.Join(dir, name), &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFCHR && st.Rdev == dev {
			return name, nil
		}
	}
	return "", nil
}
