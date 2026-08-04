package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeRag struct {
	chunks int
	hits   []rag.Hit
	err    error
}

func (f fakeRag) IngestText(context.Context, string, rag.Meta) (int, error) {
	return f.chunks, f.err
}

func (f fakeRag) Search(context.Context, string, rag.SearchOpts) ([]rag.Hit, error) {
	return f.hits, f.err
}

type fakeDispatcher struct {
	id  string
	err error
}

func (f fakeDispatcher) Dispatch(context.Context, string, string) (string, error) {
	return f.id, f.err
}

func TestServerIngest(t *testing.T) {
	tests := []struct {
		name       string
		rag        fakeRag
		wantChunks int32
		wantErr    bool
	}{
		{name: "success", rag: fakeRag{chunks: 3}, wantChunks: 3},
		{name: "error propagates", rag: fakeRag{err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: tt.rag}
			resp, err := s.Ingest(context.Background(), &orchdv1.IngestRequest{Text: "x", SourceUri: "u"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && resp.GetChunks() != tt.wantChunks {
				t.Errorf("chunks = %d, want %d", resp.GetChunks(), tt.wantChunks)
			}
		})
	}
}

func TestServerSearch(t *testing.T) {
	tests := []struct {
		name     string
		rag      fakeRag
		wantHits int
		wantErr  bool
	}{
		{name: "maps hits", rag: fakeRag{hits: []rag.Hit{{Text: "t", SourceURI: "u", Tags: []string{"go"}}}}, wantHits: 1},
		{name: "empty", rag: fakeRag{}, wantHits: 0},
		{name: "error", rag: fakeRag{err: errors.New("x")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: tt.rag}
			resp, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(resp.GetHits()) != tt.wantHits {
				t.Errorf("hits = %d, want %d", len(resp.GetHits()), tt.wantHits)
			}
		})
	}
}

func TestServerDispatch(t *testing.T) {
	tests := []struct {
		name    string
		disp    fakeDispatcher
		wantID  string
		wantErr bool
	}{
		{name: "success", disp: fakeDispatcher{id: "task-1"}, wantID: "task-1"},
		{name: "error propagates", disp: fakeDispatcher{err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: fakeRag{}, disp: tt.disp}
			resp, err := s.Dispatch(context.Background(), &orchdv1.DispatchRequest{Body: "x"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && resp.GetInstanceId() != tt.wantID {
				t.Errorf("id = %q, want %q", resp.GetInstanceId(), tt.wantID)
			}
		})
	}
}

func TestServerPing(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "reports version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: fakeRag{}}
			resp, err := s.Ping(context.Background(), &orchdv1.PingRequest{})
			if err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if resp.GetVersion() == "" {
				t.Errorf("empty version")
			}
		})
	}
}

func TestStartQdrant(t *testing.T) {
	// A stub already serving /readyz lets binary mode reuse it (no download/spawn).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	tests := []struct {
		name    string
		mode    string
		wantURL string
	}{
		{name: "external returns configured url", mode: "external", wantURL: "http://example:6333"},
		{name: "binary reuses ready engine", mode: "binary", wantURL: "http://127.0.0.1:" + portStr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := config.Settings{}
			s.RAG.Qdrant.Mode = tt.mode
			s.RAG.Qdrant.URL = "http://example:6333"
			s.RAG.Qdrant.HTTPPort = port
			s.RAG.Qdrant.Version = "v1.17.1"

			stop, url, err := startQdrant(context.Background(), s, "")
			if err != nil {
				t.Fatalf("startQdrant: %v", err)
			}
			defer stop()
			if url != tt.wantURL {
				t.Errorf("url = %q, want %q", url, tt.wantURL)
			}
		})
	}
}

func TestHelpers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		def  string
		want string
	}{
		{name: "empty uses default", in: "", def: "d", want: "d"},
		{name: "value wins", in: "v", def: "d", want: "v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orDefault(tt.in, tt.def); got != tt.want {
				t.Errorf("orDefault(%q,%q) = %q, want %q", tt.in, tt.def, got, tt.want)
			}
		})
	}
	// resolveOnnxLibDir returns a non-empty path (brew or fallback).
	if resolveOnnxLibDir() == "" {
		t.Errorf("resolveOnnxLibDir returned empty")
	}
}

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

func TestEnsureQdrantAPIKey(t *testing.T) {
	tests := []struct {
		name     string
		preseed  string
		wantSame bool
	}{
		{name: "generates and persists when absent", preseed: "", wantSame: false},
		{name: "reuses an existing key", preseed: "existing-key", wantSame: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "qdrant-api-key")
			if tt.preseed != "" {
				if err := os.WriteFile(path, []byte(tt.preseed+"\n"), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			k1, err := ensureQdrantAPIKey(path)
			if err != nil {
				t.Fatalf("ensureQdrantAPIKey: %v", err)
			}
			if k1 == "" {
				t.Fatal("empty key")
			}
			if tt.wantSame && k1 != tt.preseed {
				t.Errorf("key = %q, want preseeded %q", k1, tt.preseed)
			}
			k2, err := ensureQdrantAPIKey(path) // must be stable across calls
			if err != nil {
				t.Fatalf("second ensureQdrantAPIKey: %v", err)
			}
			if k2 != k1 {
				t.Errorf("key not stable: %q != %q", k1, k2)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
			}
		})
	}
}

func TestResolveQdrantAPIKeyEnvOverride(t *testing.T) {
	tests := []struct {
		name   string
		envVal string
		want   string
	}{
		{name: "env override wins over persisted", envVal: "from-env", want: "from-env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			t.Setenv("QDRANT_API_KEY", tt.envVal)
			s := config.Settings{}
			s.RAG.Qdrant.APIKeyEnv = "QDRANT_API_KEY"
			got, err := resolveQdrantAPIKey(s)
			if err != nil {
				t.Fatalf("resolveQdrantAPIKey: %v", err)
			}
			if got != tt.want {
				t.Errorf("key = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDirs(t *testing.T) {
	tests := []struct {
		name               string
		storage            string
		modelDir           string
		wantStorageDefault bool
		wantModelDefault   bool
	}{
		{name: "defaults resolve under data dir", wantStorageDefault: true, wantModelDefault: true},
		{name: "config overrides win", storage: "/tmp/store", modelDir: "/tmp/model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := config.Settings{}
			s.RAG.Qdrant.Storage = tt.storage
			s.Embedder.ModelDir = tt.modelDir
			gotStorage, gotModel := resolveStorageDir(s), resolveModelDir(s)
			if tt.wantStorageDefault {
				if want := filepath.Join(paths.DataDir(), "qdrant"); gotStorage != want {
					t.Errorf("storage = %q, want %q", gotStorage, want)
				}
			} else if gotStorage != tt.storage {
				t.Errorf("storage = %q, want %q", gotStorage, tt.storage)
			}
			if tt.wantModelDefault {
				if want := filepath.Join(paths.DataDir(), "models", "minilm"); gotModel != want {
					t.Errorf("model = %q, want %q", gotModel, want)
				}
			} else if gotModel != tt.modelDir {
				t.Errorf("model = %q, want %q", gotModel, tt.modelDir)
			}
		})
	}
}
