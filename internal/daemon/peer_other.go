//go:build !darwin && !linux

package daemon

// socketPeerPID has no implementation here; callers are then identified only
// by what their client reports.
func socketPeerPID(int) (int, error) { return 0, errPeerUnsupported }
