//go:build darwin

package procstat

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Parent returns pid's parent process ID, read with sysctl.
func Parent(pid int) (int, error) {
	k, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return int(k.Eproc.Ppid), nil
}

// kinfo reads pid's kernel process record with sysctl.
func kinfo(pid int) (*unix.KinfoProc, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	// A pid with no process comes back as a zeroed record rather than an error.
	if int(k.Proc.P_pid) != pid {
		return nil, fmt.Errorf("process %d not found", pid)
	}
	return k, nil
}
