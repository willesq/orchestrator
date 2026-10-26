//go:build darwin

package daemon

import "golang.org/x/sys/unix"

// socketPeerPID reads the PID of the process that connected a Unix socket.
func socketPeerPID(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}
