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

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

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
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return false }
			t.Cleanup(func() { daemonIsHomebrew = savedHB })
			_, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if err == nil {
				t.Errorf("expected error when orchd plist is absent")
			}
		})
	}
}

func TestDaemonStartStopSuccess(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "start loads plist", args: []string{"daemon", "start"}, want: "started"},
		{name: "stop unloads plist", args: []string{"daemon", "stop"}, want: "stopped"},
		{name: "restart", args: []string{"daemon", "restart"}, want: "started"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			plist := filepath.Join(home, "Library", "LaunchAgents", "com.orchestrator.orchd.plist")
			_ = os.MkdirAll(filepath.Dir(plist), 0o755)
			_ = os.WriteFile(plist, []byte("<plist/>"), 0o644)

			saved := runCommand
			runCommand = func(string, ...string) error { return nil }
			t.Cleanup(func() { runCommand = saved })
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return false } // exercise the launchd path
			t.Cleanup(func() { daemonIsHomebrew = savedHB })

			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output %q missing %q", out, tt.want)
			}
		})
	}
}

func TestDaemonControlHomebrew(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantOp  string
		wantOut string
	}{
		{name: "start via brew services", args: []string{"daemon", "start"}, wantOp: "start", wantOut: "started"},
		{name: "stop via brew services", args: []string{"daemon", "stop"}, wantOp: "stop", wantOut: "stopped"},
		{name: "restart via brew services", args: []string{"daemon", "restart"}, wantOp: "restart", wantOut: "restarted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			savedHB := daemonIsHomebrew
			daemonIsHomebrew = func() bool { return true }
			t.Cleanup(func() { daemonIsHomebrew = savedHB })

			var gotArgs []string
			saved := runCommand
			runCommand = func(name string, args ...string) error {
				gotArgs = append([]string{name}, args...)
				return nil
			}
			t.Cleanup(func() { runCommand = saved })

			out, err := run(factory(&fakeClient{}, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			want := strings.Join([]string{"brew", "services", tt.wantOp, "orch"}, " ")
			if got := strings.Join(gotArgs, " "); got != want {
				t.Errorf("ran %q, want %q", got, want)
			}
			if !strings.Contains(out, tt.wantOut) {
				t.Errorf("output %q missing %q", out, tt.wantOut)
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
	ping     *orchdv1.PingResponse
	ingest   *orchdv1.IngestResponse
	search   *orchdv1.SearchResponse
	dispatch *orchdv1.DispatchResponse
	err      error
}

func (f *fakeClient) Ping(context.Context, *orchdv1.PingRequest, ...grpc.CallOption) (*orchdv1.PingResponse, error) {
	return f.ping, f.err
}

func (f *fakeClient) Ingest(context.Context, *orchdv1.IngestRequest, ...grpc.CallOption) (*orchdv1.IngestResponse, error) {
	return f.ingest, f.err
}

func (f *fakeClient) Search(context.Context, *orchdv1.SearchRequest, ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	return f.search, f.err
}

func (f *fakeClient) Dispatch(context.Context, *orchdv1.DispatchRequest, ...grpc.CallOption) (*orchdv1.DispatchResponse, error) {
	return f.dispatch, f.err
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	tests := []struct {
		name    string
		client  *fakeClient
		wantOut string
		wantErr bool
	}{
		{
			name:    "success prints instance id",
			client:  &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-abc"}},
			wantOut: "task-abc",
		},
		{
			name:    "server error",
			client:  &fakeClient{err: errors.New("boom")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, nil), "", "dispatch", "fix the bug")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantOut != "" && !strings.Contains(out, tt.wantOut) {
				t.Errorf("output %q missing %q", out, tt.wantOut)
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
