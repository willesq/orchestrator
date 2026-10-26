package procstat

import (
	"os"
	"testing"
)

func TestParent(t *testing.T) {
	tests := []struct {
		name    string
		pid     int
		want    int
		wantErr bool
	}{
		{name: "this process's parent", pid: os.Getpid(), want: os.Getppid()},
		{name: "a process that does not exist", pid: 1 << 30, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parent(tt.pid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parent(%d) err = %v, wantErr %v", tt.pid, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("Parent(%d) = %d, want %d", tt.pid, got, tt.want)
			}
		})
	}
}

// Parent is on the path of every orchd call it vets, so it must stay native.
func TestParentWithoutPATH(t *testing.T) {
	t.Setenv("PATH", "")
	if got, err := Parent(os.Getpid()); err != nil || got != os.Getppid() {
		t.Fatalf("Parent with no PATH = %d, %v", got, err)
	}
}
