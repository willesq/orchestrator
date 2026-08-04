package qdrant

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssetName(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		goarch  string
		want    string
		wantErr bool
	}{
		{name: "darwin arm64", goos: "darwin", goarch: "arm64", want: "qdrant-aarch64-apple-darwin.tar.gz"},
		{name: "darwin amd64", goos: "darwin", goarch: "amd64", want: "qdrant-x86_64-apple-darwin.tar.gz"},
		{name: "linux arm64", goos: "linux", goarch: "arm64", want: "qdrant-aarch64-unknown-linux-musl.tar.gz"},
		{name: "linux amd64", goos: "linux", goarch: "amd64", want: "qdrant-x86_64-unknown-linux-musl.tar.gz"},
		{name: "unsupported", goos: "plan9", goarch: "mips", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := assetName(tt.goos, tt.goarch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("assetName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReleaseURL(t *testing.T) {
	tests := []struct {
		name         string
		version      string
		wantContains string
	}{
		{name: "embeds version", version: "v1.17.1", wantContains: "v1.17.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := releaseURL(tt.version)
			if err != nil {
				t.Fatalf("releaseURL: %v", err)
			}
			if !strings.Contains(got, tt.wantContains) || !strings.HasPrefix(got, "https://github.com/qdrant/qdrant/releases/download/") {
				t.Errorf("releaseURL = %q", got)
			}
		})
	}
}

// makeTarGz writes a .tar.gz containing a single file entry.
func makeTarGz(t *testing.T, dir, entry, content string) string {
	t.Helper()
	path := filepath.Join(dir, "archive.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o755, Size: int64(len(content))})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = gz.Close()
	return path
}

func TestExtractBinary(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		want    string
		wantErr bool
	}{
		{name: "found", entry: "qdrant", want: "qdrant", wantErr: false},
		{name: "not found", entry: "other", want: "qdrant", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := makeTarGz(t, dir, tt.entry, "BINARY")
			dest := filepath.Join(dir, "qdrant")
			err := extractBinary(archive, tt.want, dest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			b, _ := os.ReadFile(dest)
			if string(b) != "BINARY" {
				t.Errorf("extracted content = %q", string(b))
			}
		})
	}
}

func TestEnsureBinary(t *testing.T) {
	tests := []struct {
		name       string
		prePresent bool
		wantDLd    bool
	}{
		{name: "already present skips download", prePresent: true, wantDLd: false},
		{name: "missing triggers download+extract", prePresent: false, wantDLd: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := New(Config{Version: "v1.17.1", BinDir: dir})
			if tt.prePresent {
				if err := os.WriteFile(p.BinaryPath(), []byte("x"), 0o755); err != nil {
					t.Fatalf("pre-write: %v", err)
				}
			}
			downloaded := false
			p.download = func(_ context.Context, _, dest string) error {
				downloaded = true
				// Emulate a real release archive.
				src := makeTarGz(t, t.TempDir(), "qdrant", "BINARY")
				b, _ := os.ReadFile(src)
				return os.WriteFile(dest, b, 0o644)
			}
			if err := p.EnsureBinary(context.Background()); err != nil {
				t.Fatalf("EnsureBinary: %v", err)
			}
			if downloaded != tt.wantDLd {
				t.Errorf("downloaded = %v, want %v", downloaded, tt.wantDLd)
			}
			if _, err := os.Stat(p.BinaryPath()); err != nil {
				t.Errorf("binary not present: %v", err)
			}
		})
	}
}

// readyServer starts a stub HTTP server on 127.0.0.1 returning `status` on /readyz.
func readyServer(t *testing.T, status int) (port int, close func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &httptest.Server{Listener: lis, Config: &http.Server{Handler: mux}}
	srv.Start()
	_, portStr, _ := net.SplitHostPort(lis.Addr().String())
	port, _ = strconv.Atoi(portStr)
	return port, srv.Close
}

func TestReady(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "200 is ready", status: http.StatusOK, want: true},
		{name: "503 is not ready", status: http.StatusServiceUnavailable, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, closeSrv := readyServer(t, tt.status)
			defer closeSrv()
			p := New(Config{HTTPPort: port})
			if got := p.Ready(context.Background()); got != tt.want {
				t.Errorf("Ready = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadyNoServer(t *testing.T) {
	tests := []struct{ name string }{{name: "no server is not ready"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(Config{HTTPPort: 1}) // nothing listening on :1
			if p.Ready(context.Background()) {
				t.Errorf("expected not ready with no server")
			}
		})
	}
}

// flipReadyServer serves 503 on /readyz until ready is set true.
func flipReadyServer(t *testing.T, ready *atomic.Bool) (int, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &httptest.Server{Listener: lis, Config: &http.Server{Handler: mux}}
	srv.Start()
	_, portStr, _ := net.SplitHostPort(lis.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return port, srv.Close
}

func TestStartStop(t *testing.T) {
	tests := []struct {
		name    string
		runErr  error
		wantErr bool
	}{
		{name: "spawns then becomes ready", runErr: nil, wantErr: false},
		{name: "run error propagates", runErr: errors.New("spawn failed"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ready atomic.Bool // starts not-ready so Start proceeds to spawn
			port, closeSrv := flipReadyServer(t, &ready)
			defer closeSrv()
			dir := t.TempDir()
			p := New(Config{Version: "v1.17.1", BinDir: dir, Storage: filepath.Join(dir, "store"), HTTPPort: port})
			_ = os.WriteFile(p.BinaryPath(), []byte("x"), 0o755) // EnsureBinary no-op

			stopped := false
			var gotDir string
			p.run = func(_, dir string, _ []string) (stopFunc, error) {
				gotDir = dir
				if tt.runErr != nil {
					return nil, tt.runErr
				}
				ready.Store(true) // engine came up
				return func() error { stopped = true; return nil }, nil
			}

			err := p.Start(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Start err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if err := p.Stop(); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if !stopped {
					t.Errorf("stop handle not invoked")
				}
				if want := filepath.Join(dir, "store"); gotDir != want {
					t.Errorf("run working dir = %q, want storage dir %q", gotDir, want)
				}
			}
		})
	}
}

func TestStartReusesRunningEngine(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "already-ready engine is not re-spawned"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ready atomic.Bool
			ready.Store(true) // already up
			port, closeSrv := flipReadyServer(t, &ready)
			defer closeSrv()
			p := New(Config{HTTPPort: port})
			spawned := false
			p.run = func(string, string, []string) (stopFunc, error) {
				spawned = true
				return func() error { return nil }, nil
			}
			if err := p.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if spawned {
				t.Errorf("re-spawned an already-running engine")
			}
		})
	}
}

func TestEnvHardening(t *testing.T) {
	tests := []struct {
		name   string
		apiKey string
		want   []string
	}{
		{name: "no key", apiKey: "", want: []string{"QDRANT__SERVICE__HOST=127.0.0.1", "QDRANT__STORAGE__STORAGE_PATH=/tmp/s", "QDRANT__STORAGE__SNAPSHOTS_PATH=/tmp/s/snapshots", "QDRANT__TELEMETRY_DISABLED=true", "QDRANT__SERVICE__ENABLE_CORS=false"}},
		{name: "with key", apiKey: "secret", want: []string{"QDRANT__SERVICE__API_KEY=secret"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(Config{HTTPPort: 6333, GRPCPort: 6334, Storage: "/tmp/s", APIKey: tt.apiKey})
			env := strings.Join(p.env(), "\n")
			for _, w := range tt.want {
				if !strings.Contains(env, w) {
					t.Errorf("env missing %q", w)
				}
			}
			if tt.apiKey == "" && strings.Contains(env, "QDRANT__SERVICE__API_KEY") {
				t.Errorf("api key set when it should be empty")
			}
		})
	}
}

func TestHTTPDownload(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "success writes file", status: http.StatusOK, body: "payload", wantErr: false},
		{name: "non-200 errors", status: http.StatusNotFound, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "out")
			err := httpDownload(context.Background(), srv.URL, dest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				b, _ := os.ReadFile(dest)
				if string(b) != tt.body {
					t.Errorf("downloaded %q, want %q", string(b), tt.body)
				}
			}
		})
	}
}

// TestIntegrationRealEngine downloads and runs the REAL native qdrant binary
// (no Docker) on an isolated port + storage. Gated: set ORCH_QDRANT_IT=1.
func TestIntegrationRealEngine(t *testing.T) {
	tests := []struct {
		name     string
		httpPort int
		grpcPort int
	}{
		{name: "download + start + ready + create collection", httpPort: 6399, grpcPort: 6400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if os.Getenv("ORCH_QDRANT_IT") != "1" {
				t.Skip("set ORCH_QDRANT_IT=1 to run the real qdrant download/start integration test")
			}
			dir := t.TempDir()
			p := New(Config{
				Version:  "v1.17.1",
				BinDir:   filepath.Join(dir, "bin"),
				Storage:  filepath.Join(dir, "store"),
				HTTPPort: tt.httpPort,
				GRPCPort: tt.grpcPort,
			})
			ctx := context.Background()
			if err := p.Start(ctx); err != nil {
				t.Fatalf("Start (native, no docker): %v", err)
			}
			defer func() { _ = p.Stop() }()
			if !p.Ready(ctx) {
				t.Fatalf("engine not ready")
			}
			url := fmt.Sprintf("http://127.0.0.1:%d/collections/itest", tt.httpPort)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url,
				strings.NewReader(`{"vectors":{"size":4,"distance":"Cosine"}}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("create collection: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode >= 300 {
				t.Fatalf("create collection status %d", resp.StatusCode)
			}
			t.Logf("native qdrant %s running Docker-free on :%d, collection created", "v1.17.1", tt.httpPort)
		})
	}
}

func TestExecRun(t *testing.T) {
	tests := []struct{ name string }{{name: "starts a process and stops it"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cwdFile := filepath.Join(dir, "cwd.txt")
			script := filepath.Join(dir, "stayalive.sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\npwd -P > \""+cwdFile+"\"\nsleep 30\n"), 0o755); err != nil {
				t.Fatalf("write script: %v", err)
			}
			stop, err := execRun(script, dir, os.Environ())
			if err != nil {
				t.Fatalf("execRun: %v", err)
			}
			// Poll until the script records its working directory (avoids a race).
			var got string
			for range 50 {
				if b, err := os.ReadFile(cwdFile); err == nil && strings.TrimSpace(string(b)) != "" {
					got = strings.TrimSpace(string(b))
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if err := stop(); err != nil {
				t.Errorf("stop: %v", err)
			}
			wantDir, _ := filepath.EvalSymlinks(dir)
			gotDir, _ := filepath.EvalSymlinks(got)
			if gotDir != wantDir {
				t.Errorf("working dir = %q, want %q", gotDir, wantDir)
			}
		})
	}
}
