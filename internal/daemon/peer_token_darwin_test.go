//go:build darwin

package daemon

import (
	"errors"
	"os"
	"testing"
)

// TestCheckToken: the audit token matches only the process at its PID with
// its PID version; a reused PID has a new version.
func TestCheckToken(t *testing.T) {
	tok := auditToken{5: 900, 7: 41}
	version := func(v uint32, err error) func(int) (uint32, error) {
		return func(int) (uint32, error) { return v, err }
	}
	if err := checkToken(tok, 900, version(41, nil)); err != nil {
		t.Errorf("same process: %v", err)
	}
	if err := checkToken(tok, 900, version(42, nil)); !errors.Is(err, errPeerReplaced) {
		t.Errorf("reused PID: %v", err)
	}
	if err := checkToken(tok, 901, version(41, nil)); !errors.Is(err, errPeerReplaced) {
		t.Errorf("other PID: %v", err)
	}
	if err := checkToken(tok, 900, version(0, errors.New("gone"))); err == nil {
		t.Error("an unreadable version passed")
	}
	if v, err := pidVersion(os.Getpid()); err != nil || v == 0 {
		t.Errorf("own PID version = %d, %v", v, err)
	}
	if _, err := pidVersion(-5); err == nil {
		t.Error("a PID that cannot exist has a version")
	}
}
