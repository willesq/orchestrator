package procstat

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Identity names one process for its whole life. The kernel reuses PIDs, but
// not a PID together with its start time, so a record keyed on both is never
// matched by an unrelated later process. Compare identities with Same.
type Identity struct {
	PID     int
	Started time.Time
}

// Same reports whether a and b name the same process. It compares start
// times with Equal, so an identity that was stored and reloaded still matches.
func (a Identity) Same(b Identity) bool {
	return a.PID == b.PID && a.Started.Equal(b.Started)
}

// Process is one entry of the process table: a process and its kernel
// (short) command name.
type Process struct {
	Identity
	Name string
}

// proc is one kernel snapshot of a process: its start time and parent read
// together, so a PID reused between two reads cannot mix two processes.
type proc struct {
	started time.Time
	parent  int
}

// maxAncestors bounds the walk up the process tree, as orchd's caller lookup
// does, so a corrupt table cannot hold a request.
const maxAncestors = 256

// StartTime returns when pid started.
func StartTime(pid int) (time.Time, error) {
	p, err := readProc(pid)
	return p.started, err
}

// NearestAncestor walks from pid (inclusive) up through its parents and
// returns the first process registered reports true for. found is false when
// no process on the path is registered. Where the platform has no native
// lookup the result is also not found with a nil error: such a caller has no
// identity, so it gets no space rather than a guessed one.
//
// pid itself is taken on trust. A caller holding a peer PID from a socket
// must make sure that process is still the one that connected (for example,
// that it started before the connection was accepted); otherwise a peer that
// exits leaves its PID free for another process to take over.
func NearestAncestor(pid int, registered func(Identity) bool) (id Identity, found bool, err error) {
	return nearestAncestor(pid, registered, readProc)
}

func nearestAncestor(pid int, registered func(Identity) bool, read func(int) (proc, error)) (Identity, bool, error) {
	var child time.Time
	for range maxAncestors {
		if pid <= 0 {
			return Identity{}, false, nil
		}
		p, err := read(pid)
		if err != nil {
			return Identity{}, false, noIdentity(err)
		}
		// A parent always starts no later than its child, and so does the
		// launchd, init or subreaper an orphan is moved to. A later start
		// means the parent exited and its PID now names another process.
		if !child.IsZero() && p.started.After(child) {
			return Identity{}, false, fmt.Errorf("process %d was replaced during the walk", pid)
		}
		if id := (Identity{PID: pid, Started: p.started}); registered(id) {
			return id, true, nil
		}
		// PID 1 (launchd, init) is the root; its parent reads as 0.
		if p.parent == pid {
			return Identity{}, false, nil
		}
		child, pid = p.started, p.parent
	}
	return Identity{}, false, errors.New("process tree deeper than the walk limit")
}

// noIdentity drops ErrUnsupported: it is the expected answer on platforms
// without native lookups, not a failure to report.
func noIdentity(err error) error {
	if errors.Is(err, ErrUnsupported) {
		return nil
	}
	return err
}

// checkPID rejects values the kernel never hands out, which would otherwise
// wrap when narrowed to a C pid_t and name a different process.
func checkPID(pid int) error {
	if pid <= 0 || pid > math.MaxInt32 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return nil
}
