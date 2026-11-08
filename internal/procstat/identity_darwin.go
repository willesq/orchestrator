//go:build darwin

package procstat

import (
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// readProc reads pid's start time, to the microsecond, and parent from one
// sysctl. The kernel stamps the start with the wall clock, so after the clock
// steps back a child can read as older than its parent; the walk then fails
// closed rather than tolerating it, which would weaken the reuse check.
func readProc(pid int) (proc, error) {
	if err := checkPID(pid); err != nil {
		return proc{}, err
	}
	k, err := kinfo(pid)
	if err != nil {
		return proc{}, err
	}
	tv := k.Proc.P_starttime
	return proc{
		started: time.Unix(tv.Sec, int64(tv.Usec)*int64(time.Microsecond)),
		parent:  int(k.Eproc.Ppid),
	}, nil
}

// Cwd returns pid's current working directory. gopsutil reads it with
// libproc's proc_pidinfo, loaded in-process, not through a command.
func Cwd(pid int) (string, error) {
	if err := checkPID(pid); err != nil {
		return "", err
	}
	// A bare Process skips NewProcess's existence check; proc_pidinfo fails
	// for a missing pid on its own.
	return (&process.Process{Pid: int32(pid)}).Cwd()
}
