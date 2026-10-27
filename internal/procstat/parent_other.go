//go:build !darwin

package procstat

import "github.com/shirou/gopsutil/v4/process"

// Parent returns pid's parent process ID. On Linux gopsutil reads it from
// /proc without running a command.
func Parent(pid int) (int, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	ppid, err := p.Ppid()
	return int(ppid), err
}
