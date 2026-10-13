package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/selfupdate"
	"github.com/willesq/orchestrator/internal/supervisor"
)

func TestRunServesAndShutsDown(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "serves ping then shuts down on ctx cancel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seedRunEnv(t, "orchd-daemontest.sock")

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- Run(ctx) }()

			sock := paths.SocketPath()
			if !waitForFile(sock, waitLimit) {
				cancel()
				t.Fatalf("socket %s never appeared", sock)
			}

			conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			resp, err := orchdv1.NewOrchdClient(conn).Ping(context.Background(), &orchdv1.PingRequest{})
			if err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if resp.GetVersion() == "" {
				t.Errorf("empty version from live server")
			}
			_ = conn.Close()

			cancel()
			select {
			case err := <-errCh:
				if err != nil {
					t.Errorf("Run returned error: %v", err)
				}
			case <-time.After(waitLimit):
				t.Errorf("Run did not shut down after cancel")
			}
		})
	}
}

// TestRunExitsWhenANewBuildIsInstalled is the whole point of the self-update
// unit: a new binary at the launch path has to end this process, because the
// KeepAlive service manager behind it is what starts the new one. Exiting is
// the upgrade, so a Run that keeps serving is the bug.
func TestRunExitsWhenANewBuildIsInstalled(t *testing.T) {
	tests := []struct {
		name       string
		supervised bool
		wantExit   bool
	}{
		{name: "a supervised daemon steps aside for the new build", supervised: true, wantExit: true},
		{name: "an unsupervised daemon keeps serving the old one", supervised: false, wantExit: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := seedRunEnv(t, "orchd-selfupdatetest.sock")

			// Stand in for the binary the service manager launches orchd from.
			bin := filepath.Join(dir, "orchd")
			if err := os.WriteFile(bin, []byte("old build"), 0o755); err != nil {
				t.Fatalf("write binary: %v", err)
			}
			w, err := selfupdate.NewWatcher(bin)
			if err != nil {
				t.Fatalf("NewWatcher: %v", err)
			}
			swap(t, &selfUpdateWatch, func() (*selfupdate.Watcher, bool) { return w, tt.supervised })
			swap(t, &selfUpdateCheckInterval, 10*time.Millisecond)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- Run(ctx) }()
			if !waitForFile(paths.SocketPath(), waitLimit) {
				t.Fatalf("socket %s never appeared", paths.SocketPath())
			}

			if err := os.WriteFile(bin, []byte("a visibly longer new build"), 0o755); err != nil {
				t.Fatalf("install new build: %v", err)
			}

			select {
			case err := <-errCh:
				if !tt.wantExit {
					t.Fatalf("Run exited (%v) with nothing to restart it", err)
				}
				if err != nil {
					t.Errorf("Run returned error: %v", err)
				}
			case <-time.After(waitLimit):
				if tt.wantExit {
					t.Fatal("Run kept serving the old build after a new one was installed")
				}
			}
		})
	}
}

// swap sets *p to v for the duration of the test and restores it afterwards.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	t.Cleanup(func() { *p = old })
	*p = v
}

func TestRunSupervisesAndRestartsSubsystems(t *testing.T) {
	tests := []struct {
		name     string
		wantUnit []string
	}{
		{
			// self-update is always present: Run watches whatever binary the
			// process was launched from, which under `go test` is the test binary.
			name:     "live orchd reports and restarts its subsystems",
			wantUnit: []string{UnitWorker, UnitSources, UnitDocs, UnitSelfUpdate},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_PR_BABYSITTER_ENABLED", "false")
			t.Setenv("ORCH_QUEUE_WORKER_ENABLED", "true")
			seedRunEnv(t, "orchd-supervisortest.sock")

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-errCh:
				case <-time.After(waitLimit):
					t.Errorf("Run did not shut down after cancel")
				}
			}()

			sock := paths.SocketPath()
			if !waitForFile(sock, waitLimit) {
				t.Fatalf("socket %s never appeared", sock)
			}
			conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = conn.Close() }()
			client := orchdv1.NewOrchdClient(conn)

			st, err := client.Status(context.Background(), &orchdv1.StatusRequest{})
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			var names []string
			for _, s := range st.GetSubsystems() {
				names = append(names, s.GetName())
				if s.GetState() != supervisor.StateRunning {
					t.Errorf("%s state = %q, want running", s.GetName(), s.GetState())
				}
			}
			if strings.Join(names, ",") != strings.Join(tt.wantUnit, ",") {
				t.Fatalf("subsystems = %v, want %v", names, tt.wantUnit)
			}

			// Restarting one subsystem must bump only its counter and leave the
			// daemon (and every other subsystem) serving.
			if _, err := client.Restart(context.Background(), &orchdv1.RestartRequest{Name: UnitSources}); err != nil {
				t.Fatalf("Restart %s: %v", UnitSources, err)
			}
			st, err = client.Status(context.Background(), &orchdv1.StatusRequest{})
			if err != nil {
				t.Fatalf("Status after restart: %v", err)
			}
			for _, s := range st.GetSubsystems() {
				wantRestarts := int32(0)
				if s.GetName() == UnitSources {
					wantRestarts = 1
				}
				if s.GetRestarts() != wantRestarts {
					t.Errorf("%s restarts = %d, want %d", s.GetName(), s.GetRestarts(), wantRestarts)
				}
				if s.GetState() != supervisor.StateRunning {
					t.Errorf("%s state = %q after restart, want running", s.GetName(), s.GetState())
				}
			}

			// An unknown name is a NOT_FOUND, not a crash.
			if _, err := client.Restart(context.Background(), &orchdv1.RestartRequest{Name: "nope"}); err == nil {
				t.Errorf("expected an error for an unknown subsystem")
			}

			// An empty name restarts every subsystem.
			all, err := client.Restart(context.Background(), &orchdv1.RestartRequest{})
			if err != nil {
				t.Fatalf("Restart all: %v", err)
			}
			if len(all.GetRestarted()) != len(tt.wantUnit) {
				t.Errorf("restarted %v, want %d subsystems", all.GetRestarted(), len(tt.wantUnit))
			}
		})
	}
}

// seedRunEnv points Run at a throwaway data/config dir and pre-seeds enough of
// it that the daemon comes up without touching the network: external qdrant so
// no native engine is downloaded or spawned, and dummy model files so the
// first-run fetch is a no-op (the embedder is lazy; these tests never embed).
// It returns the temp dir so a test can place files of its own beside it.
func seedRunEnv(t *testing.T, sockName string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("ORCH_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("ORCH_RAG_QDRANT_MODE", "external")
	// Unix socket paths are capped near 104 chars on macOS; t.TempDir() plus the
	// test name blows past that, so use a short directory of the test's own. A
	// fixed name in the shared temp dir would let overlapping test runs replace
	// each other's socket.
	sockDir, err := os.MkdirTemp("", "orchd")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	t.Setenv("ORCH_SOCKET", filepath.Join(sockDir, sockName))

	modelDir := filepath.Join(dir, "data", "models", "minilm")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir model: %v", err)
	}
	for _, f := range []string{"model.onnx", "tokenizer.json"} {
		if err := os.WriteFile(filepath.Join(modelDir, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed model: %v", err)
		}
	}
	return dir
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestRunSocketIsOwnerOnly(t *testing.T) {
	tests := []struct {
		name     string
		wantMode os.FileMode
	}{
		{name: "socket is not readable or writable by others", wantMode: 0o600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ORCH_DATA_DIR", filepath.Join(dir, "data"))
			t.Setenv("ORCH_CONFIG_DIR", filepath.Join(dir, "cfg"))
			t.Setenv("ORCH_RAG_QDRANT_MODE", "external")
			t.Setenv("ORCH_PR_BABYSITTER_ENABLED", "false")
			// Keep the path short enough for macOS's Unix socket limit and unique
			// across simultaneous test processes.
			socketDir, err := os.MkdirTemp(os.TempDir(), "orch-perm-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
			t.Setenv("ORCH_SOCKET", filepath.Join(socketDir, "d.sock"))
			seedModel(t, filepath.Join(dir, "data"))

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- Run(ctx) }()
			defer func() {
				cancel()
				<-errCh
			}()

			sock := paths.SocketPath()
			if !waitForFile(sock, waitLimit) {
				t.Fatalf("socket %s never appeared", sock)
			}
			// File creation precedes chmod during startup. Wait for an actual RPC
			// response, which can only happen after permissions have been set;
			// waiting for mode 0600 itself would conceal a permission regression.
			conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("dial socket: %v", err)
			}
			defer conn.Close()
			readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
			defer readyCancel()
			if _, err := orchdv1.NewOrchdClient(conn).Ping(readyCtx, &orchdv1.PingRequest{}, grpc.WaitForReady(true)); err != nil {
				t.Fatalf("wait for daemon readiness: %v", err)
			}
			fi, err := os.Stat(sock)
			if err != nil {
				t.Fatalf("stat socket: %v", err)
			}
			// The trust boundary is the socket: anything that can connect gets
			// Dispatch and Restart, so group/other must have no access at all.
			if got := fi.Mode().Perm(); got != tt.wantMode {
				t.Errorf("socket mode = %#o, want %#o", got, tt.wantMode)
			}
		})
	}
}

// seedModel writes dummy model files so first-run provisioning is a no-op.
func seedModel(t *testing.T, dataDir string) {
	t.Helper()
	modelDir := filepath.Join(dataDir, "models", "minilm")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir model: %v", err)
	}
	for _, f := range []string{"model.onnx", "tokenizer.json"} {
		if err := os.WriteFile(filepath.Join(modelDir, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed model: %v", err)
		}
	}
}

// waitLimit bounds how long a test waits for something it expects to happen.
// The waits return as soon as the condition holds, so the limit costs nothing
// on a healthy run and is deliberately generous: only a real hang reaches it,
// never a slow or loaded machine.
const waitLimit = 30 * time.Second
