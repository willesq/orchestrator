package procstat

import (
	"os"
	"testing"
)

func TestParseStatTerminal(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Terminal
		fg   bool
		bad  bool
	}{
		{name: "foreground at a terminal", line: "42 (orch) S 40 42 40 34817 42 4194304 0 0",
			want: Terminal{Controlling: true, Group: 42, Foreground: 42}, fg: true},
		{name: "background job", line: "43 (orch) S 40 43 40 34817 42 4194304",
			want: Terminal{Controlling: true, Group: 43, Foreground: 42}},
		{name: "no terminal", line: "44 (orch) S 1 44 44 0 -1 4194304",
			want: Terminal{Group: 44}},
		{name: "name with parentheses", line: "45 (a) (b)) S 40 45 40 34817 45 0",
			want: Terminal{Controlling: true, Group: 45, Foreground: 45}, fg: true},
		{name: "no name", line: "45 S 40", bad: true},
		{name: "too short", line: "45 (x) S 40 45", bad: true},
		{name: "not a number", line: "45 (x) S 40 x 40 1 1", bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatTerminal([]byte(tt.line))
			if tt.bad {
				if err == nil {
					t.Fatalf("parsed %+v, want an error", got)
				}
				return
			}
			if err != nil || got != tt.want || got.InForeground() != tt.fg {
				t.Fatalf("got %+v (fg %v), %v; want %+v (fg %v)", got, got.InForeground(), err, tt.want, tt.fg)
			}
		})
	}
}

func TestReadTerminalOfSelf(t *testing.T) {
	got, err := ReadTerminal(os.Getpid())
	if err == ErrUnsupported {
		t.Skip("not supported here")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.Group <= 0 {
		t.Fatalf("process group %d, want > 0", got.Group)
	}
	if _, err := ReadTerminal(-1); err == nil {
		t.Fatal("read a terminal for PID -1")
	}
}
