//go:build linux

package procstat

import (
	"os"
	"strconv"
	"strings"
)

// ReadProgram returns pid's program from /proc. The executable link and
// command line of another user's process may be unreadable, leaving them
// empty.
func ReadProgram(pid int) (Program, error) {
	if err := checkPID(pid); err != nil {
		return Program{}, err
	}
	dir := "/proc/" + strconv.Itoa(pid)
	comm, err := os.ReadFile(dir + "/comm")
	if err != nil {
		return Program{}, err
	}
	p := Program{Name: strings.TrimSuffix(string(comm), "\n")}
	p.Path, _ = os.Readlink(dir + "/exe")
	if b, err := os.ReadFile(dir + "/cmdline"); err == nil {
		p.Args = splitCmdline(b)
	}
	return p, nil
}
