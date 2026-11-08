package procstat

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// Program is what a process runs: the kernel's short command name, and, where
// the kernel lets this user read them, its executable path and arguments.
// The short name alone can mislead: a Node program that sets process.title
// keeps the kernel name "node", so callers matching programs need all three.
type Program struct {
	Name string
	Path string
	Args []string
}

// parseProcArgs decodes the KERN_PROCARGS2 layout macOS returns: a native
// 32-bit argc, the executable path, NUL padding, then argc NUL-terminated
// arguments (the environment follows and is ignored).
func parseProcArgs(b []byte) (path string, args []string, err error) {
	if len(b) < 4 {
		return "", nil, errors.New("procargs too short")
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	rest := b[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", nil, errors.New("procargs: unterminated executable path")
	}
	path, rest = string(rest[:end]), rest[end:]
	rest = bytes.TrimLeft(rest, "\x00")
	for len(args) < argc && len(rest) > 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			args = append(args, string(rest))
			break
		}
		args = append(args, string(rest[:end]))
		rest = rest[end+1:]
	}
	return path, args, nil
}

// splitCmdline decodes /proc/<pid>/cmdline: NUL-separated arguments.
func splitCmdline(b []byte) []string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}
