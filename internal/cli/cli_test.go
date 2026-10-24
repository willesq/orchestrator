package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// togglingClient is an orchd client whose reachability a test flips, so the
// lifecycle commands can be driven through the real "down → supervisor acts → up"
// sequence instead of a daemon that is conveniently always reachable.
type togglingClient struct {
	*fakeClient
	up *bool
}

func (t togglingClient) Ping(context.Context, *orchdv1.PingRequest, ...grpc.CallOption) (*orchdv1.PingResponse, error) {
	if !*t.up {
		return nil, errors.New("connect: connection refused")
	}
	return &orchdv1.PingResponse{Version: "9.9.9"}, nil
}

// toggling wires a client that starts at `up` and is flipped to `then` the first
// time the supervisor is invoked, plus the supervisor spy itself.
func toggling(t *testing.T, up, then bool) (togglingClient, *[]string) {
	t.Helper()
	state := up
	var gotArgs []string
	saved := runCommand
	runCommand = func(name string, args ...string) error {
		gotArgs = append([]string{name}, args...)
		state = then
		return nil
	}
	t.Cleanup(func() { runCommand = saved })
	return togglingClient{fakeClient: &fakeClient{}, up: &state}, &gotArgs
}

// fastDaemonWaits collapses the lifecycle poll so the timeout paths are covered
// without burning the real 20s budget.
func fastDaemonWaits(t *testing.T) {
	t.Helper()
	wait, poll, probe := daemonWait, daemonPoll, daemonProbeTimeout
	daemonWait, daemonPoll, daemonProbeTimeout = 40*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { daemonWait, daemonPoll, daemonProbeTimeout = wait, poll, probe })
}

// isolateSocket points the socket path somewhere disposable so a test never
// reads (or reports) the developer's live orchd socket.
func isolateSocket(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "orchd.sock")
	t.Setenv("ORCH_SOCKET", sock)
	return sock
}

func TestDaemonStartWithoutPlist(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "start errors without plist", args: []string{"daemon", "start"}},
		{name: "stop errors without plist", args: []string{"daemon", "stop"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Point HOME at an empty dir so the plist is absent.
			t.Setenv("HOME", t.TempDir())
			isolateSocket(t)
			fastDaemonWaits(t)
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return false }
			t.Cleanup(func() { daemonIsHomebrew = savedHB })
			// A daemon that is already up would short-circuit `start`, so keep it down.
			_, err := run(factory(nil, errors.New("no socket")), "", tt.args...)
			if err == nil {
				t.Errorf("expected error when orchd plist is absent")
			}
		})
	}
}

func TestDaemonStartStopSuccess(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		up      bool // reachable before the command runs
		then    bool // reachable after the supervisor acts
		wantHas []string
	}{
		{
			name: "start loads plist and waits for the socket",
			args: []string{"daemon", "start"}, up: false, then: true,
			wantHas: []string{"started", "is answering", "9.9.9"},
		},
		{
			name: "stop unloads plist and waits for the socket to go quiet",
			args: []string{"daemon", "stop"}, up: true, then: false,
			wantHas: []string{"stopped"},
		},
		{
			name: "restart waits for the daemon to come back",
			args: []string{"daemon", "restart"}, up: true, then: true,
			wantHas: []string{"started", "is answering"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			isolateSocket(t)
			fastDaemonWaits(t)
			plist := filepath.Join(home, "Library", "LaunchAgents", "com.orchestrator.orchd.plist")
			_ = os.MkdirAll(filepath.Dir(plist), 0o755)
			_ = os.WriteFile(plist, []byte("<plist/>"), 0o644)

			client, _ := toggling(t, tt.up, tt.then)
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return false } // exercise the launchd path
			t.Cleanup(func() { daemonIsHomebrew = savedHB })

			out, err := run(factory(client, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			for _, want := range tt.wantHas {
				if !strings.Contains(out, want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
		})
	}
}

func TestDaemonControlHomebrew(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		up      bool
		then    bool
		wantOp  string
		wantOut string
	}{
		{
			name: "start via brew services", args: []string{"daemon", "start"},
			up: false, then: true, wantOp: "start", wantOut: "started",
		},
		{
			name: "stop via brew services", args: []string{"daemon", "stop"},
			up: true, then: false, wantOp: "stop", wantOut: "stopped",
		},
		{
			name: "restart via brew services", args: []string{"daemon", "restart"},
			up: true, then: true, wantOp: "restart", wantOut: "restarted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateSocket(t)
			fastDaemonWaits(t)
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return true }
			t.Cleanup(func() { daemonIsHomebrew = savedHB })

			client, gotArgs := toggling(t, tt.up, tt.then)
			out, err := run(factory(client, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			want := strings.Join([]string{"brew", "services", tt.wantOp, "orch"}, " ")
			if got := strings.Join(*gotArgs, " "); got != want {
				t.Errorf("ran %q, want %q", got, want)
			}
			if !strings.Contains(out, tt.wantOut) {
				t.Errorf("output %q missing %q", out, tt.wantOut)
			}
		})
	}
}

// A supervisor that accepts the op is not evidence the daemon reached the state:
// `brew services start` exits zero for a job it already considers started, even
// when the daemon behind it is wedged. These cases are the ones that used to
// report a cheerful success while `orch status` kept saying the daemon was down.
func TestDaemonControlReportsWhatActuallyHappened(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		up      bool
		then    bool
		wantErr bool
		wantHas string
		wantRan bool
	}{
		{
			name: "start on a wedged daemon fails instead of claiming success",
			args: []string{"daemon", "start"}, up: false, then: false,
			wantErr: true, wantHas: "orch daemon restart", wantRan: true,
		},
		{
			name: "start on a healthy daemon is a no-op, not a supervisor call",
			args: []string{"daemon", "start"}, up: true, then: true,
			wantHas: "already running", wantRan: false,
		},
		{
			name: "restart that never comes back fails",
			args: []string{"daemon", "restart"}, up: true, then: false,
			wantErr: true, wantHas: "did not answer", wantRan: true,
		},
		{
			name: "stop that leaves the daemon answering fails",
			args: []string{"daemon", "stop"}, up: true, then: true,
			wantErr: true, wantHas: "still answering", wantRan: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateSocket(t)
			fastDaemonWaits(t)
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return true }
			t.Cleanup(func() { daemonIsHomebrew = savedHB })

			client, gotArgs := toggling(t, tt.up, tt.then)
			out, err := run(factory(client, nil), "", tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			got := out
			if err != nil {
				got += err.Error()
			}
			if !strings.Contains(got, tt.wantHas) {
				t.Errorf("result %q missing %q", got, tt.wantHas)
			}
			if ran := len(*gotArgs) > 0; ran != tt.wantRan {
				t.Errorf("supervisor invoked = %v, want %v (%v)", ran, tt.wantRan, *gotArgs)
			}
		})
	}
}

func TestQdrantStatus(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantSub string
	}{
		{name: "external mode reports url", mode: "external", wantSub: "mode=external"},
		{name: "binary mode not running", mode: "binary", wantSub: "not running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			t.Setenv("ORCH_RAG_QDRANT_MODE", tt.mode)
			t.Setenv("ORCH_RAG_QDRANT_HTTP_PORT", "1") // nothing listening
			out, err := run(factory(&fakeClient{}, nil), "", "qdrant", "status")
			if err != nil {
				t.Fatalf("qdrant status: %v", err)
			}
			if !strings.Contains(out, tt.wantSub) {
				t.Errorf("output %q missing %q", out, tt.wantSub)
			}
		})
	}
}

func TestQdrantMigrateDryRun(t *testing.T) {
	tests := []struct{ name string }{{name: "prints plan, changes nothing"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			out, err := run(factory(&fakeClient{}, nil), "", "qdrant", "migrate", "/new/path")
			if err != nil {
				t.Fatalf("migrate dry-run: %v", err)
			}
			if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "/new/path") {
				t.Errorf("unexpected dry-run output: %q", out)
			}
		})
	}
}

func TestQdrantMigrateApplyErrors(t *testing.T) {
	tests := []struct {
		name    string
		storage string
		target  string
	}{
		{name: "same path is rejected", storage: "/data/qdrant", target: "/data/qdrant"},
		{name: "missing plist is rejected", storage: "/data/qdrant", target: "/data/qdrant-new"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			t.Setenv("ORCH_RAG_QDRANT_STORAGE", tt.storage)
			t.Setenv("HOME", t.TempDir()) // no orchd plist
			_, err := run(factory(&fakeClient{}, nil), "", "qdrant", "migrate", tt.target, "--apply")
			if err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
		})
	}
}

func TestRealMigrateDeps(t *testing.T) {
	tests := []struct{ name string }{{name: "builds usable closures"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // no plist → stop/start return errors, but must not panic
			d := realMigrateDeps(context.Background(), 1, "coll")
			if d.stop == nil || d.start == nil || d.ready == nil || d.present == nil {
				t.Fatalf("nil closure in deps")
			}
			// Exercise them (ready/present false with nothing listening; stop/start error).
			_ = d.ready()
			_ = d.present()
			if d.stop() == nil || d.start() == nil {
				t.Errorf("expected stop/start to error without a plist")
			}
		})
	}
}

func TestCollectionPresent(t *testing.T) {
	tests := []struct {
		name   string
		status int
		serve  bool
		want   bool
	}{
		{name: "present", status: http.StatusOK, serve: true, want: true},
		{name: "not found", status: http.StatusNotFound, serve: true, want: false},
		{name: "no server", serve: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := 1 // nothing listening
			if tt.serve {
				lis, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
				})}
				go func() { _ = srv.Serve(lis) }()
				defer func() { _ = srv.Close() }()
				_, portStr, _ := net.SplitHostPort(lis.Addr().String())
				port, _ = strconv.Atoi(portStr)
			}
			if got := collectionPresent(port, "coll"); got != tt.want {
				t.Errorf("collectionPresent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunMigrate(t *testing.T) {
	tests := []struct {
		name       string
		stopReady  bool // does the engine actually stop?
		startReady bool // does it come back up?
		present    bool
		wantErr    bool
		wantSub    string
	}{
		{name: "happy path", stopReady: true, startReady: true, present: true, wantSub: "migration complete"},
		{name: "engine won't stop aborts before copy", stopReady: false, wantErr: true},
		{name: "engine won't restart", stopReady: true, startReady: false, wantErr: true},
		{name: "up but collection missing warns", stopReady: true, startReady: true, present: false, wantSub: "WARNING"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			old := filepath.Join(t.TempDir(), "old")
			_ = os.MkdirAll(old, 0o755)
			_ = os.WriteFile(filepath.Join(old, "meta"), []byte("x"), 0o644)
			newPath := filepath.Join(t.TempDir(), "new")

			v, _ := config.Load()
			started := false
			deps := migrateDeps{
				stop:  func() error { return nil },
				start: func() error { started = true; return nil },
				ready: func() bool {
					if !started {
						return !tt.stopReady // waitFor(ready,false) succeeds when the engine is down
					}
					return tt.startReady
				},
				present:     func() bool { return tt.present },
				downTimeout: 200 * time.Millisecond,
				upTimeout:   200 * time.Millisecond,
			}

			var out bytes.Buffer
			err := runMigrate(&out, old, newPath, v, deps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v (out=%q)", err, tt.wantErr, out.String())
			}
			if tt.wantSub != "" && !strings.Contains(out.String(), tt.wantSub) {
				t.Errorf("output %q missing %q", out.String(), tt.wantSub)
			}
			if !tt.wantErr {
				if _, err := os.Stat(filepath.Join(newPath, "meta")); err != nil {
					t.Errorf("data not copied to new path")
				}
			}
		})
	}
}

var startedYet bool

func TestCopyDir(t *testing.T) {
	tests := []struct {
		name    string
		setup   bool
		wantErr bool
	}{
		{name: "copies a tree", setup: true, wantErr: false},
		{name: "missing source errors", setup: false, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src")
			if tt.setup {
				_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
				_ = os.WriteFile(filepath.Join(src, "sub", "f"), []byte("data"), 0o644)
			}
			dst := filepath.Join(t.TempDir(), "dst")
			err := copyDir(src, dst)
			if (err != nil) != tt.wantErr {
				t.Fatalf("copyDir err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if b, _ := os.ReadFile(filepath.Join(dst, "sub", "f")); string(b) != "data" {
					t.Errorf("file not copied")
				}
			}
		})
	}
}

func TestQdrantCutover(t *testing.T) {
	tests := []struct {
		name  string
		apply bool
	}{
		{name: "dry-run prints plan, changes nothing", apply: false},
		{name: "apply backs up, stops container, reconfigures", apply: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			storage := filepath.Join(home, "qstore")
			_ = os.MkdirAll(filepath.Join(storage, "collections"), 0o755)
			_ = os.WriteFile(filepath.Join(storage, "collections", "x"), []byte("data"), 0o644)
			t.Setenv("ORCH_CONFIG_DIR", filepath.Join(home, "cfg"))
			t.Setenv("ORCH_DATA_DIR", filepath.Join(home, "data"))

			saved := runCommand
			stopped := false
			runCommand = func(_ string, args ...string) error {
				if len(args) >= 1 && args[0] == "stop" {
					stopped = true
				}
				return nil
			}
			t.Cleanup(func() { runCommand = saved })

			args := []string{"qdrant", "cutover", "--storage", storage}
			if tt.apply {
				args = append(args, "--apply")
			}
			out, err := run(factory(&fakeClient{}, nil), "", args...)
			if err != nil {
				t.Fatalf("cutover: %v", err)
			}
			if tt.apply {
				if !stopped {
					t.Errorf("container was not stopped")
				}
				if !strings.Contains(out, "reconfigured") {
					t.Errorf("did not reconfigure: %q", out)
				}
			} else {
				if !strings.Contains(out, "DRY RUN") {
					t.Errorf("dry-run banner missing")
				}
				if stopped {
					t.Errorf("dry-run stopped the container")
				}
			}
		})
	}
}

func TestRepoCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		wantSub string
	}{
		{name: "add repo", args: []string{"repo", "add", "."}, wantSub: "registered repo"},
		{name: "invalid refresh", args: []string{"repo", "add", ".", "--refresh", "nope"}, wantErr: true},
		{name: "list empty", args: []string{"repo", "list"}, wantSub: "no repos"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantSub != "" && !strings.Contains(out, tt.wantSub) {
				t.Errorf("output %q missing %q", out, tt.wantSub)
			}
		})
	}
}

func TestRepoAddThenList(t *testing.T) {
	tests := []struct{ name string }{{name: "add then list shows the repo"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			cf := factory(&fakeClient{}, nil)
			if _, err := run(cf, "", "repo", "add", ".", "--refresh", "12h"); err != nil {
				t.Fatalf("add: %v", err)
			}
			out, err := run(cf, "", "repo", "list")
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if strings.Contains(out, "no repos") {
				t.Errorf("list should show the added repo: %q", out)
			}
		})
	}
}

func TestPRsCommand(t *testing.T) {
	tests := []struct {
		name    string
		seed    bool
		args    []string
		wantSub string
	}{
		{name: "empty cache", seed: false, args: []string{"prs"}, wantSub: "no PRs"},
		{name: "shows cached pr", seed: true, args: []string{"prs"}, wantSub: "o/r: fix"},
		{name: "json output", seed: true, args: []string{"prs", "--json"}, wantSub: "https://x/1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			if tt.seed {
				_ = prs.SaveState(paths.PRStateFile(), prs.State{PRs: map[string]prs.PR{
					"https://x/1": {URL: "https://x/1", Title: "fix", Repo: "o/r", CIState: "success"},
				}})
			}
			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("prs: %v", err)
			}
			if !strings.Contains(out, tt.wantSub) {
				t.Errorf("output %q missing %q", out, tt.wantSub)
			}
		})
	}
}

func TestSourceCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		wantSub string
	}{
		{name: "add owner", args: []string{"source", "add", "owner", "willesq", "--refresh", "24h"}, wantSub: "added owner"},
		{name: "add web", args: []string{"source", "add", "web", "https://x", "--refresh", "6h"}, wantSub: "added web"},
		{name: "invalid kind errors", args: []string{"source", "add", "bogus", "x"}, wantErr: true},
		{name: "owner outside the roots errors", args: []string{"source", "add", "owner", "../../etc"}, wantErr: true},
		{name: "relative repo path errors", args: []string{"source", "add", "repo", "code/app"}, wantErr: true},
		{name: "invalid refresh errors", args: []string{"source", "add", "web", "https://x", "--refresh", "nope"}, wantErr: true},
		{name: "list empty", args: []string{"source", "list"}, wantSub: "no sources"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantSub != "" && !strings.Contains(out, tt.wantSub) {
				t.Errorf("output %q missing %q", out, tt.wantSub)
			}
		})
	}
}

func TestSourceLifecycle(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "add, list, refresh, remove"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			cf := factory(&fakeClient{}, nil)
			if _, err := run(cf, "", "source", "add", "web", "https://x", "--refresh", "1h"); err != nil {
				t.Fatalf("add: %v", err)
			}
			out, _ := run(cf, "", "source", "list")
			if !strings.Contains(out, "https://x") {
				t.Errorf("list missing source: %q", out)
			}
			if _, err := run(cf, "", "source", "refresh"); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if _, err := run(cf, "", "source", "remove", "web|https://x"); err != nil {
				t.Fatalf("remove: %v", err)
			}
			out, _ = run(cf, "", "source", "list")
			if !strings.Contains(out, "no sources") {
				t.Errorf("expected empty after remove: %q", out)
			}
		})
	}
}

func TestCheckCommand(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "reports failures and exits non-zero when preconditions missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			t.Setenv("ORCH_DATA_DIR", t.TempDir())
			t.Setenv("ORCH_SOCKET", filepath.Join(t.TempDir(), "nope.sock"))
			out, err := run(factory(&fakeClient{}, nil), "", "check")
			if err == nil {
				t.Errorf("expected non-zero exit when preconditions fail")
			}
			if !strings.Contains(out, "orchd") {
				t.Errorf("output missing check lines: %q", out)
			}
		})
	}
}

func TestModelFetchAlreadyPresent(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "present model is a no-op"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			_ = os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("m"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte("t"), 0o644)
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			t.Setenv("ORCH_EMBEDDER_MODEL_DIR", dir)
			out, err := run(factory(&fakeClient{}, nil), "", "model", "fetch")
			if err != nil {
				t.Fatalf("model fetch: %v", err)
			}
			if !strings.Contains(out, "already present") {
				t.Errorf("output %q missing 'already present'", out)
			}
		})
	}
}

// fakeClient implements orchdv1.OrchdClient with canned responses.
type fakeClient struct {
	searchReq    *orchdv1.SearchRequest
	listReq      *orchdv1.ListSourcesRequest
	ping         *orchdv1.PingResponse
	ingest       *orchdv1.IngestResponse
	search       *orchdv1.SearchResponse
	dispatch     *orchdv1.DispatchResponse
	dispatchReq  *orchdv1.DispatchRequest
	listSources  *orchdv1.ListSourcesResponse
	deleteSource *orchdv1.DeleteSourceResponse
	listTasks    *orchdv1.ListTasksResponse
	getTask      *orchdv1.GetTaskResponse
	status       *orchdv1.StatusResponse
	restart      *orchdv1.RestartResponse
	// statusErr and restartErr override err for those two calls, so a test can
	// have a reachable daemon that still refuses one specific request.
	statusErr    error
	restartErr   error
	restartNames []string
	// tails are TailLogs's replies in order; once used up, TailLogs fails with
	// tailErr, which ends a --follow loop.
	tails    []*orchdv1.TailLogsResponse
	tailReqs []*orchdv1.TailLogsRequest
	tailErr  error
	// document answers GetDocument; docReq records the request.
	document *orchdv1.Document
	docReq   *orchdv1.GetDocumentRequest
	err      error
}

func (f *fakeClient) Ping(context.Context, *orchdv1.PingRequest, ...grpc.CallOption) (*orchdv1.PingResponse, error) {
	return f.ping, f.err
}

func (f *fakeClient) Ingest(context.Context, *orchdv1.IngestRequest, ...grpc.CallOption) (*orchdv1.IngestResponse, error) {
	return f.ingest, f.err
}

func (f *fakeClient) Search(_ context.Context, req *orchdv1.SearchRequest, _ ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	f.searchReq = req
	return f.search, f.err
}

func (f *fakeClient) Dispatch(_ context.Context, req *orchdv1.DispatchRequest, _ ...grpc.CallOption) (*orchdv1.DispatchResponse, error) {
	f.dispatchReq = req
	return f.dispatch, f.err
}

func (f *fakeClient) ListSources(_ context.Context, req *orchdv1.ListSourcesRequest, _ ...grpc.CallOption) (*orchdv1.ListSourcesResponse, error) {
	f.listReq = req
	return f.listSources, f.err
}

func (f *fakeClient) DeleteSource(context.Context, *orchdv1.DeleteSourceRequest, ...grpc.CallOption) (*orchdv1.DeleteSourceResponse, error) {
	return f.deleteSource, f.err
}

func (f *fakeClient) ListTasks(context.Context, *orchdv1.ListTasksRequest, ...grpc.CallOption) (*orchdv1.ListTasksResponse, error) {
	return f.listTasks, f.err
}

func (f *fakeClient) GetTask(context.Context, *orchdv1.GetTaskRequest, ...grpc.CallOption) (*orchdv1.GetTaskResponse, error) {
	return f.getTask, f.err
}

func (f *fakeClient) Status(context.Context, *orchdv1.StatusRequest, ...grpc.CallOption) (*orchdv1.StatusResponse, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.status, f.err
}

func (f *fakeClient) Restart(_ context.Context, req *orchdv1.RestartRequest, _ ...grpc.CallOption) (*orchdv1.RestartResponse, error) {
	f.restartNames = append(f.restartNames, req.GetName())
	if f.restartErr != nil {
		return nil, f.restartErr
	}
	return f.restart, f.err
}

func factory(c orchdv1.OrchdClient, dialErr error) clientFactory {
	return func() (orchdv1.OrchdClient, func(), error) {
		if dialErr != nil {
			return nil, nil, dialErr
		}
		return c, func() {}, nil
	}
}

func run(cf clientFactory, stdin string, args ...string) (string, error) {
	root := NewRootCmd(cf)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestDialOrchd(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "returns a lazy client and cleanup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_SOCKET", "/tmp/orch-nonexistent.sock")
			client, cleanup, err := dialOrchd()
			if err != nil {
				t.Fatalf("dialOrchd: %v", err)
			}
			if client == nil {
				t.Fatalf("nil client")
			}
			cleanup()
		})
	}
}

func TestExecute(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "version via Execute reads os.Args", args: []string{"orch", "version"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := os.Args
			t.Cleanup(func() { os.Args = saved })
			os.Args = tt.args
			if err := Execute(); err != nil {
				t.Errorf("Execute: %v", err)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	tests := []struct {
		name         string
		wantContains string
	}{
		{name: "prints version", wantContains: "orch "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(&fakeClient{}, nil), "", "version")
			if err != nil {
				t.Fatalf("version: %v", err)
			}
			if !strings.Contains(out, tt.wantContains) {
				t.Errorf("output %q missing %q", out, tt.wantContains)
			}
		})
	}
}

func TestConfigCommand(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantErr      bool
		wantContains string
	}{
		{name: "get default", args: []string{"config", "get", "rag.qdrant.mode"}, wantContains: "binary"},
		{name: "get unknown key errors", args: []string{"config", "get", "nope.nope"}, wantErr: true},
		{name: "set persists", args: []string{"config", "set", "rag.qdrant.mode", "external"}, wantContains: "external"},
		{name: "list shows settings", args: []string{"config", "list"}, wantContains: "qdrant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !strings.Contains(out, tt.wantContains) {
				t.Errorf("output %q missing %q", out, tt.wantContains)
			}
		})
	}
}

func TestPingAndDaemonStatus(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		client       *fakeClient
		dialErr      error
		socket       bool // seed a leftover socket file
		wantErr      bool
		wantContains string
	}{
		{
			name:         "ping success",
			args:         []string{"ping"},
			client:       &fakeClient{ping: &orchdv1.PingResponse{Version: "1.2.3", UptimeSeconds: 5}},
			wantContains: "1.2.3",
		},
		{
			name:    "ping dial error",
			args:    []string{"ping"},
			dialErr: errors.New("no socket"),
			wantErr: true,
		},
		{
			name:         "daemon status running",
			args:         []string{"daemon", "status"},
			client:       &fakeClient{ping: &orchdv1.PingResponse{Version: "1.2.3"}},
			wantContains: "running",
		},
		{
			name:         "daemon status not running",
			args:         []string{"daemon", "status"},
			client:       &fakeClient{err: errors.New("down")},
			wantContains: "not running",
		},
		{
			// A leftover socket means orchd is up but no longer accepting, which
			// `orch daemon start` cannot fix — say so and point at the restart.
			name:         "daemon status unreachable with a leftover socket",
			args:         []string{"daemon", "status"},
			client:       &fakeClient{err: errors.New("connection refused")},
			socket:       true,
			wantContains: "unreachable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock := isolateSocket(t)
			if tt.socket {
				if err := os.WriteFile(sock, nil, 0o600); err != nil {
					t.Fatalf("seed socket: %v", err)
				}
			}
			out, err := run(factory(tt.client, tt.dialErr), "", tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantContains != "" && !strings.Contains(out, tt.wantContains) {
				t.Errorf("output %q missing %q", out, tt.wantContains)
			}
		})
	}
}

func TestIngestCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		stdin   string
		client  *fakeClient
		dialErr error
		wantErr bool
		wantOut string
	}{
		{
			name:    "happy path",
			args:    []string{"ingest", "text", "--source-uri", "manual://x"},
			stdin:   "hello world",
			client:  &fakeClient{ingest: &orchdv1.IngestResponse{Chunks: 2}},
			wantOut: "ingested 2 chunk(s)",
		},
		{
			name:    "missing source-uri",
			args:    []string{"ingest", "text"},
			stdin:   "hello",
			client:  &fakeClient{},
			wantErr: true,
		},
		{
			name:    "empty stdin",
			args:    []string{"ingest", "text", "--source-uri", "manual://x"},
			stdin:   "",
			client:  &fakeClient{},
			wantErr: true,
		},
		{
			name:    "server error",
			args:    []string{"ingest", "text", "--source-uri", "manual://x"},
			stdin:   "hello",
			client:  &fakeClient{err: errors.New("boom")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, tt.dialErr), tt.stdin, tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v (out=%q)", err, tt.wantErr, out)
			}
			if tt.wantOut != "" && !strings.Contains(out, tt.wantOut) {
				t.Errorf("output %q missing %q", out, tt.wantOut)
			}
		})
	}
}

func TestInstallCommand(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
		wantOut string
	}{
		{name: "dry-run prints plan, writes nothing", target: "claude-code", wantOut: "DRY RUN"},
		{name: "unknown target errors", target: "emacs", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			binDir := t.TempDir()
			out, err := run(factory(&fakeClient{}, nil), "",
				"install", "--target", tt.target, "--home", home, "--bin-dir", binDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantOut != "" && !strings.Contains(out, tt.wantOut) {
				t.Errorf("output %q missing %q", out, tt.wantOut)
			}
			// dry-run must never write into the temp home.
			if _, err := os.Stat(home + "/.claude/settings.json"); !os.IsNotExist(err) {
				t.Errorf("dry-run wrote settings.json")
			}
		})
	}
}

func TestDispatchCommand(t *testing.T) {
	wd, _ := os.Getwd()
	tests := []struct {
		name    string
		args    []string
		client  *fakeClient
		wantOut []string
		wantNot string
		wantCwd string
		taskID  string
		wantErr bool
	}{
		{
			name:    "runs in the current directory by default",
			args:    []string{"dispatch", "fix the bug"},
			client:  &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-abc", WorkerEnabled: true}},
			wantOut: []string{"task-abc", "runs in " + wd},
			wantNot: "disabled",
			wantCwd: wd,
		},
		{
			name:    "relative --cwd is made absolute",
			args:    []string{"dispatch", "--cwd", "sub", "fix the bug"},
			client:  &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-abc", WorkerEnabled: true}},
			wantCwd: filepath.Join(wd, "sub"),
		},
		{
			name:    "disabled worker is called out",
			args:    []string{"dispatch", "fix the bug"},
			client:  &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-abc"}},
			wantOut: []string{"task-abc", "worker is disabled", "queue.worker.enabled"},
		},
		{
			name:    "refused inside a queued task",
			args:    []string{"dispatch", "fix the bug"},
			taskID:  "task-parent",
			client:  &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-abc"}},
			wantErr: true,
		},
		{
			name:    "server error",
			args:    []string{"dispatch", "fix the bug"},
			client:  &fakeClient{err: errors.New("boom")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(agentrun.TaskIDEnv, tt.taskID)
			out, err := run(factory(tt.client, nil), "", tt.args...)
			if tt.taskID != "" && tt.client.dispatchReq != nil {
				t.Errorf("orchd was called from inside a task")
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			for _, w := range tt.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output %q missing %q", out, w)
				}
			}
			if tt.wantNot != "" && strings.Contains(out, tt.wantNot) {
				t.Errorf("output %q unexpectedly contains %q", out, tt.wantNot)
			}
			if tt.wantCwd != "" && tt.client.dispatchReq.GetCwd() != tt.wantCwd {
				t.Errorf("cwd = %q, want %q", tt.client.dispatchReq.GetCwd(), tt.wantCwd)
			}
		})
	}
}

func TestSearchCommand(t *testing.T) {
	tests := []struct {
		name    string
		client  *fakeClient
		wantOut string
		wantErr bool
	}{
		{
			name: "returns hits",
			client: &fakeClient{search: &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
				{Text: "hello", SourceUri: "manual://x", SourceType: "manual", Score: 0.9},
			}}},
			wantOut: "manual://x",
		},
		{
			name:    "no results",
			client:  &fakeClient{search: &orchdv1.SearchResponse{}},
			wantOut: "no results",
		},
		{
			name:    "server error",
			client:  &fakeClient{err: errors.New("boom")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, nil), "", "search", "some query")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantOut != "" && !strings.Contains(out, tt.wantOut) {
				t.Errorf("output %q missing %q", out, tt.wantOut)
			}
		})
	}
}

func TestSearchNarrowsToOneSource(t *testing.T) {
	fc := &fakeClient{search: &orchdv1.SearchResponse{}}
	if _, err := run(factory(fc, nil), "", "search", "install", "--uri", "repo://o/r/README.md",
		"--require-tag", "owner:o", "--require-tag", "repo:r"); err != nil {
		t.Fatal(err)
	}
	if fc.searchReq.GetSourceUri() != "repo://o/r/README.md" || strings.Join(fc.searchReq.GetAllTags(), ",") != "owner:o,repo:r" {
		t.Errorf("request = %v", fc.searchReq)
	}
}

func (f *fakeClient) SummarizeSession(context.Context, *orchdv1.SummarizeSessionRequest, ...grpc.CallOption) (*orchdv1.SummarizeSessionResponse, error) {
	return &orchdv1.SummarizeSessionResponse{}, f.err
}
