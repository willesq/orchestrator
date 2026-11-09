//go:build darwin

package procstat

import (
	"bytes"
	"os"
	"time"

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

// Processes returns the current user's processes with their kernel names,
// from one sysctl. Their start times match StartTime's, so an entry's
// Identity can be compared with one stored earlier.
func Processes() ([]Process, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Getuid())
	out := make([]Process, 0, len(procs))
	for _, k := range procs {
		if k.Eproc.Ucred.Uid != uid || k.Proc.P_pid <= 0 {
			continue
		}
		tv := k.Proc.P_starttime
		out = append(out, Process{
			Identity: Identity{PID: int(k.Proc.P_pid), Started: time.Unix(tv.Sec, int64(tv.Usec)*int64(time.Microsecond))},
			Name:     commName(k.Proc.P_comm[:]),
		})
	}
	return out, nil
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
