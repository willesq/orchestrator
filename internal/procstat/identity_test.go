package procstat

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func nativeIdentity() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }

// startSleep starts /bin/sleep in dir with os.StartProcess (this package's
// tests may not import os/exec) and kills it when the test ends.
func startSleep(t *testing.T, dir string) *os.Process {
	t.Helper()
	p, err := os.StartProcess("/bin/sleep", []string{"sleep", "60"}, &os.ProcAttr{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Kill()
		_, _ = p.Wait()
	})
	return p
}

func TestStartTimeAndCwd(t *testing.T) {
	if !nativeIdentity() {
		t.Skip("covered by TestIdentityUnsupported")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	child := startSleep(t, dir)
	after := time.Now()

	// Linux dates a start from the boot time, which /proc/stat gives in whole
	// seconds, so allow that much drift from the wall clock.
	const slack = 2 * time.Second
	tests := []struct {
		name             string
		pid              int
		cwd              string
		earliest, latest time.Time
	}{
		{name: "this process", pid: os.Getpid(), cwd: wd, latest: before.Add(slack)},
		{name: "a child in a temp dir", pid: child.Pid, cwd: dir, earliest: before.Add(-slack), latest: after.Add(slack)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StartTime(tt.pid)
			if err != nil {
				t.Fatalf("StartTime: %v", err)
			}
			if got.Before(tt.earliest) || got.After(tt.latest) || got.IsZero() {
				t.Errorf("StartTime = %v, want within [%v, %v]", got, tt.earliest, tt.latest)
			}
			// A pin keys on the start time, so it must read the same every time.
			if again, _ := StartTime(tt.pid); !again.Equal(got) {
				t.Errorf("StartTime changed between reads: %v then %v", got, again)
			}
			cwd, err := Cwd(tt.pid)
			if err != nil {
				t.Fatalf("Cwd: %v", err)
			}
			if cwd != tt.cwd {
				t.Errorf("Cwd = %q, want %q", cwd, tt.cwd)
			}
		})
	}

	self, _ := StartTime(os.Getpid())
	if kid, _ := StartTime(child.Pid); kid.Before(self) {
		t.Errorf("child started %v, before its parent %v", kid, self)
	}
}

func TestIdentityMissingProcess(t *testing.T) {
	if !nativeIdentity() {
		t.Skip("covered by TestIdentityUnsupported")
	}
	if got, err := StartTime(1 << 30); err == nil {
		t.Errorf("StartTime of a missing process = %v", got)
	}
	if got, err := Cwd(1 << 30); err == nil {
		t.Errorf("Cwd of a missing process = %q", got)
	}
}

// Identity lookups are on the path of every orchd call that needs a space, so
// they must stay native.
func TestIdentityWithoutPATH(t *testing.T) {
	if !nativeIdentity() {
		t.Skip("covered by TestIdentityUnsupported")
	}
	t.Setenv("PATH", "")
	if _, err := StartTime(os.Getpid()); err != nil {
		t.Errorf("StartTime with no PATH: %v", err)
	}
	if _, err := Cwd(os.Getpid()); err != nil {
		t.Errorf("Cwd with no PATH: %v", err)
	}
}

func TestIdentityUnsupported(t *testing.T) {
	if nativeIdentity() {
		t.Skip("native lookups exist on " + runtime.GOOS)
	}
	if _, err := StartTime(os.Getpid()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("StartTime err = %v, want ErrUnsupported", err)
	}
	if _, err := Cwd(os.Getpid()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Cwd err = %v, want ErrUnsupported", err)
	}
	if _, found, err := NearestAncestor(os.Getpid(), func(Identity) bool { return true }); found || err != nil {
		t.Errorf("NearestAncestor = found %v, err %v; want no identity", found, err)
	}
}

func TestNearestAncestor(t *testing.T) {
	if !nativeIdentity() {
		t.Skip("covered by TestIdentityUnsupported")
	}
	child := startSleep(t, t.TempDir())
	self, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	selfID := Identity{PID: os.Getpid(), Started: self}

	t.Run("finds the registered ancestor", func(t *testing.T) {
		got, found, err := NearestAncestor(child.Pid, func(id Identity) bool { return id.Same(selfID) })
		if err != nil || !found || !got.Same(selfID) {
			t.Errorf("= %+v, %v, %v; want %+v", got, found, err, selfID)
		}
	})
	t.Run("a reused PID with another start time does not match", func(t *testing.T) {
		stale := Identity{PID: selfID.PID, Started: self.Add(-time.Hour)}
		if _, found, err := NearestAncestor(child.Pid, func(id Identity) bool { return id.Same(stale) }); found || err != nil {
			t.Errorf("found %v, err %v", found, err)
		}
	})
	t.Run("nothing registered reaches the root", func(t *testing.T) {
		if _, found, err := NearestAncestor(child.Pid, func(Identity) bool { return false }); found || err != nil {
			t.Errorf("found %v, err %v", found, err)
		}
	})
	t.Run("a missing process is an error", func(t *testing.T) {
		if _, found, err := NearestAncestor(1<<30, func(Identity) bool { return true }); found || err == nil {
			t.Errorf("found %v, err %v", found, err)
		}
	})
}

// fakeTree serves process snapshots from a map of pid to parent; each pid
// started at Unix second pid unless starts overrides it.
type fakeTree struct {
	parents map[int]int
	starts  map[int]int64
}

func (f fakeTree) read(pid int) (proc, error) {
	p, ok := f.parents[pid]
	if !ok {
		return proc{}, errors.New("no such process")
	}
	sec, ok := f.starts[pid]
	if !ok {
		sec = int64(pid)
	}
	return proc{started: time.Unix(sec, 0), parent: p}, nil
}

func TestNearestAncestorWalk(t *testing.T) {
	tree := fakeTree{parents: map[int]int{40: 30, 30: 20, 20: 10, 10: 1, 1: 0}}
	unsupported := func(int) (proc, error) { return proc{}, ErrUnsupported }
	tests := []struct {
		name       string
		pid        int
		registered map[int]bool
		tree       *fakeTree
		read       func(int) (proc, error)
		want       int
		wantErr    bool
	}{
		{name: "the pid itself", pid: 40, registered: map[int]bool{40: true, 20: true}, want: 40},
		{name: "nearest wins over farther", pid: 40, registered: map[int]bool{30: true, 20: true}, want: 30},
		{name: "the root", pid: 40, registered: map[int]bool{1: true}, want: 1},
		{name: "none registered", pid: 40, registered: map[int]bool{}},
		{name: "pid 0 is no process", pid: 0, registered: map[int]bool{0: true}},
		{name: "a broken link is an error", pid: 40, registered: map[int]bool{10: true}, tree: &fakeTree{parents: map[int]int{40: 30}}, wantErr: true},
		{name: "unsupported is no identity", pid: 40, registered: map[int]bool{40: true}, read: unsupported},
		{name: "a pid that is its own parent stops", pid: 7, registered: map[int]bool{}, tree: &fakeTree{parents: map[int]int{7: 7}}},
		{
			name: "a parent that started after its child was replaced", pid: 40, registered: map[int]bool{30: true},
			tree: &fakeTree{parents: map[int]int{40: 30, 30: 1, 1: 0}, starts: map[int]int64{30: 50}}, wantErr: true,
		},
		{
			name: "a parent started in the same tick as its child", pid: 40, registered: map[int]bool{30: true},
			tree: &fakeTree{parents: map[int]int{40: 30, 30: 1, 1: 0}, starts: map[int]int64{30: 40}}, want: 30,
		},
		{
			name: "a cycle hits the walk limit", pid: 5, registered: map[int]bool{},
			tree: &fakeTree{parents: map[int]int{5: 6, 6: 5}, starts: map[int]int64{5: 1, 6: 1}}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := tree.read
			if tt.tree != nil {
				read = tt.tree.read
			}
			if tt.read != nil {
				read = tt.read
			}
			got, found, err := nearestAncestor(tt.pid, func(id Identity) bool { return tt.registered[id.PID] }, read)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if found != (tt.want != 0) || got.PID != tt.want {
				t.Errorf("= %+v, found %v; want pid %d", got, found, tt.want)
			}
		})
	}
}

func TestIdentitySame(t *testing.T) {
	at := time.Unix(1700000000, 123000)
	a := Identity{PID: 9, Started: at}
	tests := []struct {
		name string
		b    Identity
		want bool
	}{
		{name: "identical", b: a, want: true},
		{name: "reloaded in another zone", b: Identity{PID: 9, Started: at.UTC()}, want: true},
		{name: "another start", b: Identity{PID: 9, Started: at.Add(time.Microsecond)}},
		{name: "another pid", b: Identity{PID: 10, Started: at}},
	}
	for _, tt := range tests {
		if got := a.Same(tt.b); got != tt.want {
			t.Errorf("%s: Same = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1, 1 << 32} {
		if err := checkPID(pid); err == nil {
			t.Errorf("checkPID(%d) accepted", pid)
		}
		if !nativeIdentity() {
			continue
		}
		if _, err := StartTime(pid); err == nil {
			t.Errorf("StartTime(%d) accepted", pid)
		}
		if _, err := Cwd(pid); err == nil {
			t.Errorf("Cwd(%d) accepted", pid)
		}
	}
}

func TestParseStat(t *testing.T) {
	// Fields 3 to 22 of a real line; the name holds a space and a ')'.
	tail := " S 812 900 900 0 -1 4194560 120 0 0 0 3 1 0 0 20 0 1 0 98765 1234 56"
	tests := []struct {
		name       string
		line       string
		wantParent int
		wantTicks  uint64
		wantErr    bool
	}{
		{name: "plain name", line: "4242 (sleep)" + tail, wantParent: 812, wantTicks: 98765},
		{name: "name with space and paren", line: "4242 (a b) c)" + tail, wantParent: 812, wantTicks: 98765},
		{name: "no name", line: "4242 sleep S 1", wantErr: true},
		{name: "truncated", line: "4242 (sleep) S 812 900", wantErr: true},
		{name: "bad parent", line: "4242 (sleep) S x 900 900 0 -1 4194560 120 0 0 0 3 1 0 0 20 0 1 0 98765", wantErr: true},
		{name: "bad start", line: "4242 (sleep) S 812 900 900 0 -1 4194560 120 0 0 0 3 1 0 0 20 0 1 0 -5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, ticks, err := parseStat([]byte(tt.line))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (parent != tt.wantParent || ticks != tt.wantTicks) {
				t.Errorf("= %d, %d; want %d, %d", parent, ticks, tt.wantParent, tt.wantTicks)
			}
		})
	}
}

func TestParseBootTime(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{name: "present", in: "cpu  1 2 3\nintr 5\nbtime 1700000000\nprocesses 9\n", want: 1700000000},
		{name: "missing", in: "cpu  1 2 3\n", wantErr: true},
		{name: "malformed", in: "btime soon\n", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseBootTime([]byte(tt.in))
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
		if !tt.wantErr && got.Unix() != tt.want {
			t.Errorf("%s: = %v, want %d", tt.name, got, tt.want)
		}
	}
	if got := ticksToDuration(250); got != 2500*time.Millisecond {
		t.Errorf("ticksToDuration(250) = %v", got)
	}
}

func BenchmarkStartTime(b *testing.B) {
	if !nativeIdentity() {
		b.Skip("no native lookup")
	}
	pid := os.Getpid()
	for b.Loop() {
		if _, err := StartTime(pid); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCwd(b *testing.B) {
	if !nativeIdentity() {
		b.Skip("no native lookup")
	}
	pid := os.Getpid()
	for b.Loop() {
		if _, err := Cwd(pid); err != nil {
			b.Fatal(err)
		}
	}
}

// The whole walk from this process to the root, the worst case for a caller
// with no registered ancestor.
func BenchmarkNearestAncestor(b *testing.B) {
	if !nativeIdentity() {
		b.Skip("no native lookup")
	}
	pid := os.Getpid()
	for b.Loop() {
		if _, _, err := NearestAncestor(pid, func(Identity) bool { return false }); err != nil {
			b.Fatal(err)
		}
	}
}
