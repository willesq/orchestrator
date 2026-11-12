package daemon

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// D-44 (SP-041.1.a5): before the guard enforces, a connection's peer is
// identified by a kernel token, not its PID alone. LOCAL_PEERPID and
// SO_PEERCRED report the PID that connected; if that process hands its
// socket to a child and exits before orchd reads its ancestry, the PID can
// be reused by a harness in another space, and the walk would resolve the
// connection to that harness's session. The token (LOCAL_PEERTOKEN's audit
// token on macOS, SO_PEERPIDFD on Linux) names the exact process, so orchd
// checks, after its walk, that the PID still names the process that
// connected. A peer that fails the check cannot be identified and gets no
// space (callerNone), never a default.

// errPeerReplaced reports a connection whose peer PID now names another
// process than the one that connected.
var errPeerReplaced = errors.New("the process that connected has exited and its PID names another process")

// identify is walk, checked against the peer's kernel token.
func (p peerCredentials) identify(c net.Conn) callerAuth {
	a := p.walk(c)
	if a.err != nil || a.pid <= 0 {
		return a
	}
	if err := p.checkPeer(c, a.pid); err != nil {
		return callerAuth{pid: a.pid, err: err}
	}
	return a
}

// checkPeer reports whether pid still names the process at the other end
// of c. Tests that supply peer PIDs without a socket skip it unless they
// supply samePeer too.
func (p peerCredentials) checkPeer(c net.Conn, pid int) error {
	switch {
	case p.samePeer != nil:
		return p.samePeer(c, pid)
	case p.peerPID != nil:
		return nil
	}
	return connSamePeer(c, pid)
}

// connSamePeer checks pid against c's kernel token.
func connSamePeer(c net.Conn, pid int) error {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return fmt.Errorf("%T has no socket to inspect", c)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var perr error
	if err := raw.Control(func(fd uintptr) { perr = socketSamePeer(int(fd), pid) }); err != nil {
		return err
	}
	return perr
}
