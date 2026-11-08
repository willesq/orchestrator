//go:build !darwin && !linux

package procstat

// ReadProgram has no native implementation here.
func ReadProgram(int) (Program, error) { return Program{}, ErrUnsupported }
