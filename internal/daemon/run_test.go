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
			dir := t.TempDir()
			t.Setenv("ORCH_DATA_DIR", filepath.Join(dir, "data"))
			t.Setenv("ORCH_CONFIG_DIR", filepath.Join(dir, "cfg"))
			// external mode: don't download/spawn a native qdrant in the test.
			t.Setenv("ORCH_RAG_QDRANT_MODE", "external")
			// Unix socket paths are capped near 104 chars on macOS; t.TempDir()
			// plus the test name blows past that, so use a short explicit path.
			t.Setenv("ORCH_SOCKET", filepath.Join(os.TempDir(), "orchd-daemontest.sock"))

			// Pre-seed dummy model files so the first-run fetch is a no-op (the
			// embedder is lazy and this test only pings).
			modelDir := filepath.Join(dir, "data", "models", "minilm")
			if err := os.MkdirAll(modelDir, 0o755); err != nil {
				t.Fatalf("mkdir model: %v", err)
			}
			for _, f := range []string{"model.onnx", "tokenizer.json"} {
				if err := os.WriteFile(filepath.Join(modelDir, f), []byte("x"), 0o644); err != nil {
					t.Fatalf("seed model: %v", err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- Run(ctx) }()

			sock := paths.SocketPath()
			if !waitForFile(sock, 3*time.Second) {
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
			case <-time.After(3 * time.Second):
				t.Errorf("Run did not shut down after cancel")
			}
		})
	}
}

func TestRunSupervisesAndRestartsSubsystems(t *testing.T) {
	tests := []struct {
		name     string
		wantUnit []string
	}{
		{
			name:     "live orchd reports and restarts its subsystems",
			wantUnit: []string{UnitWorker, UnitSources, UnitDocs},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ORCH_DATA_DIR", filepath.Join(dir, "data"))
			t.Setenv("ORCH_CONFIG_DIR", filepath.Join(dir, "cfg"))
			t.Setenv("ORCH_RAG_QDRANT_MODE", "external") // no native engine in tests
			t.Setenv("ORCH_PR_BABYSITTER_ENABLED", "false")
			t.Setenv("ORCH_SOCKET", filepath.Join(os.TempDir(), "orchd-supervisortest.sock"))

			modelDir := filepath.Join(dir, "data", "models", "minilm")
			if err := os.MkdirAll(modelDir, 0o755); err != nil {
				t.Fatalf("mkdir model: %v", err)
			}
			for _, f := range []string{"model.onnx", "tokenizer.json"} {
				if err := os.WriteFile(filepath.Join(modelDir, f), []byte("x"), 0o644); err != nil {
					t.Fatalf("seed model: %v", err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-errCh:
				case <-time.After(3 * time.Second):
					t.Errorf("Run did not shut down after cancel")
				}
			}()

			sock := paths.SocketPath()
			if !waitForFile(sock, 5*time.Second) {
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
			t.Setenv("ORCH_SOCKET", filepath.Join(os.TempDir(), "orchd-permtest.sock"))
			seedModel(t, filepath.Join(dir, "data"))

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- Run(ctx) }()
			defer func() {
				cancel()
				<-errCh
			}()

			sock := paths.SocketPath()
			if !waitForFile(sock, 5*time.Second) {
				t.Fatalf("socket %s never appeared", sock)
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
