//go:build darwin

package procstat

import (
	"bytes"
	"os"

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
		if commName(k.Proc.P_comm[:]) != name || k.Eproc.Ucred.Uid != uid {
			continue
		}
		if started := k.Proc.P_starttime.Sec; pid == 0 || started > newest {
			pid, newest = int(k.Proc.P_pid), started
		}
	}
	return pid, nil
}

// commName reads the kernel's NUL-terminated command name. The buffer can hold
// leftovers of an earlier, longer name after the terminator, so it stops at
// the first NUL rather than trimming trailing ones.
func commName(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
