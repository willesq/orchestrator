//go:build linux

package daemon

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// soPeerPIDFD is SO_PEERPIDFD (Linux 6.5): a pidfd of the process that
// connected, which keeps naming that process after its PID is reused.
const soPeerPIDFD = 77

// socketSamePeer checks that fd's peer pidfd still names process pid.
func socketSamePeer(fd, pid int) error {
	pidfd, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, soPeerPIDFD)
	if err != nil {
		return fmt.Errorf("read the peer's pidfd: %w", err)
	}
	defer unix.Close(pidfd)
	now, err := pidfdPID(pidfd)
	if err != nil {
		return err
	}
	return checkPIDFD(now, pid)
}

// checkPIDFD reports whether the PID a pidfd names now, -1 once its
// process exited, is pid.
func checkPIDFD(now, pid int) error {
	if now != pid {
		return errPeerReplaced
	}
	return nil
}

// pidfdPID is the PID a pidfd names now, -1 once its process has exited.
func pidfdPID(pidfd int) (int, error) {
	f, err := os.Open(fmt.Sprintf("/proc/self/fdinfo/%d", pidfd))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Pid:"); ok {
			return strconv.Atoi(strings.TrimSpace(v))
		}
	}
	return 0, fmt.Errorf("pidfd %d names no process", pidfd)
}
