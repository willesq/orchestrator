//go:build linux

package procstat

import (
	"os"
	"strconv"
)

// ReadTerminal returns pid's controlling terminal, read from
// /proc/<pid>/stat.
func ReadTerminal(pid int) (Terminal, error) {
	if err := checkPID(pid); err != nil {
		return Terminal{}, err
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Terminal{}, err
	}
	return parseStatTerminal(b)
}
