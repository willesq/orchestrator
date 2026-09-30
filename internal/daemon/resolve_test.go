package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
)

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

func TestResolveModelDir(t *testing.T) {
	tests := []struct {
		name        string
		modelDir    string
		wantDefault bool
	}{
		{name: "default resolves under the data dir", wantDefault: true},
		{name: "config override wins", modelDir: "/tmp/model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := config.Settings{}
			s.Embedder.ModelDir = tt.modelDir
			got := resolveModelDir(s)
			if tt.wantDefault {
				if want := filepath.Join(paths.DataDir(), "models", "minilm"); got != want {
					t.Errorf("model = %q, want %q", got, want)
				}
			} else if got != tt.modelDir {
				t.Errorf("model = %q, want %q", got, tt.modelDir)
			}
		})
	}
}
