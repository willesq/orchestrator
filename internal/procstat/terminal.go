package procstat

import (
	"bytes"
	"errors"
	"strconv"
)

// Terminal is a process's controlling terminal, as the kernel records it.
type Terminal struct {
	// Controlling reports that the process has a controlling terminal.
	Controlling bool
	// Group is the process's process group; Foreground is the terminal's
	// foreground process group (0 without a terminal).
	Group, Foreground int
}

// InForeground reports whether the process runs in its terminal's
// foreground process group: a command the user typed at a shell prompt
// does; a job sent to the background, or a process left behind by one, does
// not.
func (t Terminal) InForeground() bool {
	return t.Controlling && t.Group > 0 && t.Group == t.Foreground
}

// parseStatTerminal returns the process group (field 5), controlling
// terminal (field 7) and its foreground process group (field 8) from a
// /proc/<pid>/stat line. Fields are counted from the command name's last
// ')', as in parseStat.
func parseStatTerminal(b []byte) (Terminal, error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return Terminal{}, errors.New("malformed stat: no command name")
	}
	f := bytes.Fields(b[i+1:])
	if len(f) < 6 {
		return Terminal{}, errors.New("malformed stat: too few fields")
	}
	var n [3]int
	for k, idx := range []int{2, 4, 5} {
		v, err := strconv.Atoi(string(f[idx]))
		if err != nil {
			return Terminal{}, err
		}
		n[k] = v
	}
	t := Terminal{Group: n[0], Controlling: n[1] != 0}
	if t.Controlling && n[2] > 0 {
		t.Foreground = n[2]
	}
	return t, nil
}
