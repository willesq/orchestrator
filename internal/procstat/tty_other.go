//go:build !darwin

package procstat

import (
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// TTY returns the name of pid's controlling terminal under /dev (e.g.
// pts/3), or "" when it has none or it cannot be read.
func TTY(pid int) (string, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return "", err
	}
	t, _ := p.Terminal()
	return strings.TrimPrefix(t, "/"), nil
}
