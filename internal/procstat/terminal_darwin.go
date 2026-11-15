//go:build darwin

package procstat

// noDev is the kernel's NODEV: a process with no controlling terminal.
const noDev = -1

// ReadTerminal returns pid's controlling terminal, read with sysctl.
func ReadTerminal(pid int) (Terminal, error) {
	k, err := kinfo(pid)
	if err != nil {
		return Terminal{}, err
	}
	t := Terminal{Group: int(k.Eproc.Pgid), Controlling: k.Eproc.Tdev != noDev}
	if t.Controlling && k.Eproc.Tpgid > 0 {
		t.Foreground = int(k.Eproc.Tpgid)
	}
	return t, nil
}
