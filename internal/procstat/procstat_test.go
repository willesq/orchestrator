package procstat

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestReadSelf(t *testing.T) {
	s, err := Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if s.PID != os.Getpid() || s.RSSBytes == 0 || s.Started.IsZero() || s.Started.After(time.Now()) {
		t.Errorf("stats = %+v", s)
	}
}

func TestReadMissingProcess(t *testing.T) {
	if _, err := Read(1 << 30); err == nil {
		t.Error("read a process that does not exist")
	}
}

func TestCPUPercent(t *testing.T) {
	a := Stats{CPUSeconds: 10}
	tests := []struct {
		after   float64
		elapsed time.Duration
		want    float64
	}{
		{11, 2 * time.Second, 50},
		{14, 2 * time.Second, 200},
		{10, time.Second, 0},
		{9, time.Second, 0},
		{12, 0, 0},
	}
	for _, tt := range tests {
		if got := CPUPercent(a, Stats{CPUSeconds: tt.after}, tt.elapsed); got != tt.want {
			t.Errorf("CPUPercent(10→%v over %v) = %v, want %v", tt.after, tt.elapsed, got, tt.want)
		}
	}
}

func TestFindByName(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if _, err := FindByName("x"); err != ErrUnsupported {
			t.Errorf("err = %v, want ErrUnsupported", err)
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(self)
	if len(name) > 16 {
		name = name[:16]
	}
	pid, err := FindByName(name)
	if err != nil || pid == 0 {
		t.Errorf("FindByName(%q) = %d, %v", name, pid, err)
	}
	if pid, err := FindByName("no-such-process-" + strconv.Itoa(os.Getpid())); err != nil || pid != 0 {
		t.Errorf("missing = %d, %v", pid, err)
	}
}

// The package exists so orchd never spawns a command to inspect processes.
func TestNoCommands(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"os/exec"` {
				t.Errorf("%s imports os/exec", f)
			}
		}
	}
}

// gopsutil finds ps through PATH; with PATH empty, any method that shells out
// fails, so a successful read proves Read stays native.
func TestReadWithoutPATH(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("procstat reads processes natively only on darwin")
	}
	t.Setenv("PATH", "")
	if _, err := Read(os.Getpid()); err != nil {
		t.Fatalf("Read with no PATH: %v", err)
	}
}

// TestProcessesUnsupported: off macOS the process table cannot be read, so
// callers list no processes (TestProcessesIncludesSelf covers macOS).
func TestProcessesUnsupported(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("covered by TestProcessesIncludesSelf")
	}
	if procs, err := Processes(); err != ErrUnsupported || procs != nil {
		t.Errorf("Processes() = %v, %v; want nil, ErrUnsupported", procs, err)
	}
}
