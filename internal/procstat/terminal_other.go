//go:build !darwin && !linux

package procstat

// ReadTerminal has no native implementation here.
func ReadTerminal(int) (Terminal, error) { return Terminal{}, ErrUnsupported }
