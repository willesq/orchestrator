package procstat

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestReadProgramOfSelf(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no native program lookup")
	}
	p, err := ReadProgram(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Base(os.Args[0])
	// The kernel truncates the name, to 15 bytes on Linux and 16 on macOS.
	if p.Name == "" || !strings.HasPrefix(exe, p.Name) {
		t.Errorf("Name = %q, executable %q", p.Name, exe)
	}
	if filepath.Base(p.Path) != exe {
		t.Errorf("Path = %q, executable %q", p.Path, exe)
	}
	if !slices.Equal(p.Args, os.Args) {
		t.Errorf("Args = %q, want %q", p.Args, os.Args)
	}
	if _, err := ReadProgram(-1); err == nil {
		t.Error("ReadProgram(-1) succeeded")
	}
}

func procArgs(argc uint32, body string) []byte {
	b := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint32(b, argc)
	return append(b, body...)
}

func TestParseProcArgs(t *testing.T) {
	path, args, err := parseProcArgs(procArgs(2, "/usr/local/bin/node\x00\x00\x00\x00node\x00/x/claude-code/cli.js\x00HOME=/u\x00"))
	if err != nil || path != "/usr/local/bin/node" || !slices.Equal(args, []string{"node", "/x/claude-code/cli.js"}) {
		t.Errorf("parse = %q, %q, %v", path, args, err)
	}
	if _, args, err := parseProcArgs(procArgs(3, "/bin/a\x00a\x00b")); err != nil || !slices.Equal(args, []string{"a", "b"}) {
		t.Errorf("unterminated last argument: %q, %v", args, err)
	}
	for _, bad := range [][]byte{nil, {1, 0}, procArgs(1, "/no/terminator")} {
		if _, _, err := parseProcArgs(bad); err == nil {
			t.Errorf("parseProcArgs(%q) succeeded", bad)
		}
	}
}

func TestSplitCmdline(t *testing.T) {
	if got := splitCmdline([]byte("node\x00/x/cli.js\x00")); !slices.Equal(got, []string{"node", "/x/cli.js"}) {
		t.Errorf("split = %q", got)
	}
	if got := splitCmdline([]byte("\x00")); got != nil {
		t.Errorf("empty = %q", got)
	}
}
