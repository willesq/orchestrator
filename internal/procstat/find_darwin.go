//go:build darwin

package procstat

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// FindByName returns the newest process named name (the kernel's short
// command name) owned by the current user, or 0 when there is none. It reads
// the process table with sysctl.
func FindByName(name string) (int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return 0, err
	}
	uid := uint32(os.Getuid())
	var pid int
	var newest int64
	for _, k := range procs {
		comm := strings.TrimRight(string(k.Proc.P_comm[:]), "\x00")
		if comm != name || k.Eproc.Ucred.Uid != uid {
			continue
		}
		if started := k.Proc.P_starttime.Sec; pid == 0 || started > newest {
			pid, newest = int(k.Proc.P_pid), started
		}
	}
	return pid, nil
}
