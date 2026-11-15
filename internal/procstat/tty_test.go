package procstat

import (
	"os"
	"strings"
	"testing"
)

// TestTTY: a process has a terminal or none, named without /dev; a process
// that does not exist is an error. Under go test the terminal depends on
// how the tests run, so only its form is checked.
func TestTTY(t *testing.T) {
	name, err := TTY(os.Getpid())
	if err != nil {
		t.Fatalf("TTY(self) = %v", err)
	}
	if strings.HasPrefix(name, "/") || strings.Contains(name, "dev/") {
		t.Fatalf("TTY(self) = %q, want a name under /dev", name)
	}
	if _, err := TTY(1 << 30); err == nil {
		t.Fatal("a process that does not exist has a terminal")
	}
}
