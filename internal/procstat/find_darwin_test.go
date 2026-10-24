//go:build darwin

package procstat

import "testing"

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
