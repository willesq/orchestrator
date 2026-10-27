//go:build linux

package daemon

import "golang.org/x/sys/unix"

// socketPeerPID reads the PID of the process that connected a Unix socket.
func socketPeerPID(fd int) (int, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Pid), nil
}
