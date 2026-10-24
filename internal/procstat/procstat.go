// Package procstat reads process statistics natively: sysctl and libproc
// through gopsutil's in-process bindings. It never runs ps, pgrep or any other
// command, so orchd can report on itself and its children without spawning a
// shell. Only the gopsutil calls wrapped here are known not to shell out
// (some of its other functions run ps), so orchd uses this package rather
// than gopsutil directly.
package procstat

import (
	"errors"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Stats is one process's resource use.
type Stats struct {
	PID        int
	RSSBytes   uint64
	CPUSeconds float64 // user + system time used so far
	Started    time.Time
}

// ErrUnsupported is returned where a lookup has no native implementation.
var ErrUnsupported = errors.New("not supported on this platform")

// Read returns pid's current memory, CPU time and start time.
func Read(pid int) (Stats, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return Stats{}, err
	}
	mem, err := p.MemoryInfo()
	if err != nil {
		return Stats{}, err
	}
	times, err := p.Times()
	if err != nil {
		return Stats{}, err
	}
	created, err := p.CreateTime()
	if err != nil {
		return Stats{}, err
	}
	return Stats{
		PID:        pid,
		RSSBytes:   mem.RSS,
		CPUSeconds: times.User + times.System,
		Started:    time.UnixMilli(created),
	}, nil
}

// CPUPercent is the CPU use between two samples of the same process, as a
// percentage of one core (so 200 means two cores busy).
func CPUPercent(before, after Stats, elapsed time.Duration) float64 {
	if elapsed <= 0 || after.CPUSeconds < before.CPUSeconds {
		return 0
	}
	return (after.CPUSeconds - before.CPUSeconds) / elapsed.Seconds() * 100
}
