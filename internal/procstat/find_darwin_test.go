//go:build darwin

package procstat

import (
	"os"
	"testing"
)

func TestCommName(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{name: "stops at the first NUL despite leftovers", in: []byte("qdrant\x00y\x00sk\x00\x00"), want: "qdrant"},
		{name: "a full buffer has no NUL", in: []byte("sixteen-chars-xx"), want: "sixteen-chars-xx"},
		{name: "empty", in: []byte{0, 0}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commName(tt.in); got != tt.want {
				t.Errorf("commName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestProcessesIncludesSelf: the table lists this test process under its
// kernel name, with the start time StartTime reads, so an entry matches a
// stored identity.
func TestProcessesIncludesSelf(t *testing.T) {
	procs, err := Processes()
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	want, err := StartTime(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == self {
			if !p.Same(Identity{PID: self, Started: want}) || p.Name == "" {
				t.Fatalf("self = %+v, want start %v and a name", p, want)
			}
			return
		}
	}
	t.Fatalf("process %d missing from %d processes", self, len(procs))
}
