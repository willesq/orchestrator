//go:build !darwin

package procstat

// FindByName is only implemented on macOS, where orchd runs.
func FindByName(string) (int, error) { return 0, ErrUnsupported }

// Processes is only implemented on macOS, where orchd runs.
func Processes() ([]Process, error) { return nil, ErrUnsupported }
