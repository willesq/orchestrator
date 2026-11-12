//go:build darwin

package daemon

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// auditToken is macOS's audit_token_t: val[5] is the PID and val[7] the
// PID version, which changes when the PID is reused.
type auditToken [8]uint32

// procUniqIdentifierInfo is struct proc_uniqidentifierinfo, the
// PROC_PIDUNIQIDENTIFIERINFO flavor of proc_pidinfo.
type procUniqIdentifierInfo struct {
	UUID            [16]byte
	UniqueID        uint64
	PUniqueID       uint64
	IDVersion       int32
	OrigPPIDVersion int32
	Reserve2        uint64
	Reserve3        uint64
}

const (
	procInfoCallPIDInfo       = 2
	procPIDUniqIdentifierInfo = 17
)

// socketSamePeer compares the PID version in fd's peer audit token with
// the version of the process now at pid.
func socketSamePeer(fd, pid int) error {
	var tok auditToken
	n := uint32(unsafe.Sizeof(tok))
	if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.SOL_LOCAL, unix.LOCAL_PEERTOKEN,
		uintptr(unsafe.Pointer(&tok)), uintptr(unsafe.Pointer(&n)), 0); e != 0 {
		return fmt.Errorf("read the peer's audit token: %w", e)
	}
	if n != uint32(unsafe.Sizeof(tok)) {
		return errPeerReplaced
	}
	return checkToken(tok, pid, pidVersion)
}

// checkToken reports whether tok names the process now at pid: the same
// PID and the same PID version.
func checkToken(tok auditToken, pid int, version func(int) (uint32, error)) error {
	if int(tok[5]) != pid {
		return errPeerReplaced
	}
	v, err := version(pid)
	if err != nil {
		return fmt.Errorf("read the PID version of process %d: %w", pid, err)
	}
	if v != tok[7] {
		return errPeerReplaced
	}
	return nil
}

// pidVersion is the PID version of the process now at pid.
func pidVersion(pid int) (uint32, error) {
	var info procUniqIdentifierInfo
	r, _, e := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDUniqIdentifierInfo, 0,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	switch {
	case e != 0:
		return 0, e
	case r != unsafe.Sizeof(info):
		return 0, fmt.Errorf("proc_pidinfo returned %d bytes, want %d", r, unsafe.Sizeof(info))
	}
	return uint32(info.IDVersion), nil
}
