//go:build linux

package procstat

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// readProc reads pid's start time and parent from one read of
// /proc/<pid>/stat.
func readProc(pid int) (proc, error) {
	if err := checkPID(pid); err != nil {
		return proc{}, err
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return proc{}, err
	}
	parent, ticks, err := parseStat(b)
	if err != nil {
		return proc{}, err
	}
	boot, err := bootTime()
	if err != nil {
		return proc{}, err
	}
	return proc{started: boot.Add(ticksToDuration(ticks)), parent: parent}, nil
}

// bootTime is read once per process: the kernel derives btime from the wall
// clock, so a clock step would otherwise shift every start time and break the
// identities already recorded. A failed read is not kept, so the next lookup
// retries.
var (
	bootMu   sync.Mutex
	bootSeen time.Time
)

func bootTime() (time.Time, error) {
	bootMu.Lock()
	defer bootMu.Unlock()
	if !bootSeen.IsZero() {
		return bootSeen, nil
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	t, err := parseBootTime(b)
	if err != nil {
		return time.Time{}, err
	}
	bootSeen = t
	return t, nil
}

// Cwd returns pid's current working directory from /proc/<pid>/cwd.
func Cwd(pid int) (string, error) {
	if err := checkPID(pid); err != nil {
		return "", err
	}
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
}
