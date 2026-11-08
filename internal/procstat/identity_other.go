//go:build !darwin && !linux

package procstat

// readProc has no native implementation here; callers treat that as no
// identity.
func readProc(int) (proc, error) { return proc{}, ErrUnsupported }

// Cwd has no native implementation here.
func Cwd(int) (string, error) { return "", ErrUnsupported }
