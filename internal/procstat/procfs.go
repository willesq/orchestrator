package procstat

import (
	"bytes"
	"errors"
	"strconv"
	"time"
)

// Parsers for Linux procfs. They build everywhere so the formats are tested
// on every platform, though only identity_linux.go uses them.

// userHZ is the unit of /proc tick counts, fixed at 100 by the kernel ABI
// whatever the internal tick rate.
const userHZ = 100

// parseStat returns the parent PID (field 4) and start time in ticks since
// boot (field 22) from a /proc/<pid>/stat line. The command name (field 2)
// may hold spaces and parentheses, so fields are counted from its last ')'.
func parseStat(b []byte) (parent int, startTicks uint64, err error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, errors.New("malformed stat: no command name")
	}
	f := bytes.Fields(b[i+1:])
	// f[0] is field 3 (state), so field n is f[n-3].
	if len(f) < 20 {
		return 0, 0, errors.New("malformed stat: too few fields")
	}
	parent, err = strconv.Atoi(string(f[1]))
	if err != nil {
		return 0, 0, err
	}
	startTicks, err = strconv.ParseUint(string(f[19]), 10, 64)
	return parent, startTicks, err
}

// parseBootTime reads the btime line of /proc/stat.
func parseBootTime(b []byte) (time.Time, error) {
	for line := range bytes.Lines(b) {
		if rest, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			sec, err := strconv.ParseInt(string(bytes.TrimSpace(rest)), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}

func ticksToDuration(ticks uint64) time.Duration {
	return time.Duration(ticks) * (time.Second / userHZ)
}
