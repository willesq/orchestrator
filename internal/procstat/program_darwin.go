//go:build darwin

package procstat

import "golang.org/x/sys/unix"

// ReadProgram returns pid's program. The name comes from sysctl
// kern.proc.pid; the path and arguments from kern.procargs2, which the kernel
// refuses for another user's process, leaving them empty.
func ReadProgram(pid int) (Program, error) {
	k, err := kinfo(pid)
	if err != nil {
		return Program{}, err
	}
	p := Program{Name: commName(k.Proc.P_comm[:])}
	if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil {
		p.Path, p.Args, _ = parseProcArgs(raw)
	}
	return p, nil
}
