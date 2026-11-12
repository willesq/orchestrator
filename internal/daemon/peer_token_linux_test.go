//go:build linux

package daemon

import (
	"errors"
	"testing"
)

// TestCheckPIDFD: a pidfd matches only while it names the same PID; -1
// means its process exited.
func TestCheckPIDFD(t *testing.T) {
	if err := checkPIDFD(900, 900); err != nil {
		t.Errorf("same process: %v", err)
	}
	for _, now := range []int{-1, 901} {
		if err := checkPIDFD(now, 900); !errors.Is(err, errPeerReplaced) {
			t.Errorf("pidfd names %d: %v", now, err)
		}
	}
	if _, err := pidfdPID(-1); err == nil {
		t.Error("a pidfd that is not open names a process")
	}
}
