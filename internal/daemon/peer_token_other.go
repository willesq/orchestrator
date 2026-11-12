//go:build !darwin && !linux

package daemon

// socketSamePeer has no kernel token to check here. It passes: socket
// peers cannot be read on this platform at all (socketPeerPID returns
// errPeerUnsupported), so no caller reaches this check.
func socketSamePeer(int, int) error { return nil }
