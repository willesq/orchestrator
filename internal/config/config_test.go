package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/willesq/orchestrator/internal/mdwalk"
	"github.com/willesq/orchestrator/internal/paths"
)

func TestLoadDefaults(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "embedder idle_unload default", key: "embedder.idle_unload", want: "10m"},
		{name: "qdrant mode default is docker-free", key: "rag.qdrant.mode", want: "binary"},
		{name: "collection default is orchestrator", key: "rag.qdrant.collection", want: "orchestrator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			v, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := v.GetString(tt.key); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestSetPersistsAndReloads(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		value       string
		checkKept   string
		keptDefault string
	}{
		{
			name:        "persisted value survives reload and keeps defaults",
			key:         "rag.qdrant.mode",
			value:       "external",
			checkKept:   "rag.qdrant.version",
			keptDefault: "v1.17.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ORCH_CONFIG_DIR", dir)

			v, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := Set(v, tt.key, tt.value); err != nil {
				t.Fatalf("Set: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
				t.Fatalf("config.yaml not written: %v", err)
			}

			v2, err := Load()
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got := v2.GetString(tt.key); got != tt.value {
				t.Errorf("%s after reload = %q, want %q", tt.key, got, tt.value)
			}
			if got := v2.GetString(tt.checkKept); got != tt.keptDefault {
				t.Errorf("default lost: %s = %q, want %q", tt.checkKept, got, tt.keptDefault)
			}
		})
	}
}

func TestEnvOverridesDefault(t *testing.T) {
	tests := []struct {
		name   string
		envKey string
		envVal string
		key    string
		want   string
	}{
		{name: "env overrides embedder model", envKey: "ORCH_EMBEDDER_MODEL", envVal: "custom/model", key: "embedder.model", want: "custom/model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			t.Setenv(tt.envKey, tt.envVal)
			v, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := v.GetString(tt.key); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestUnmarshalTyped(t *testing.T) {
	tests := []struct {
		name    string
		wantURL string
		wantCol string
	}{
		{name: "defaults decode into typed struct", wantURL: "http://127.0.0.1:6333", wantCol: "orchestrator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			v, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			s, err := Unmarshal(v)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if s.RAG.Qdrant.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", s.RAG.Qdrant.URL, tt.wantURL)
			}
			if s.RAG.Qdrant.Collection != tt.wantCol {
				t.Errorf("Collection = %q, want %q", s.RAG.Qdrant.Collection, tt.wantCol)
			}
		})
	}
}

func TestGetAndList(t *testing.T) {
	tests := []struct {
		name    string
		getKey  string
		wantNil bool
	}{
		{name: "known key returns value", getKey: "rag.qdrant.mode", wantNil: false},
		{name: "unknown key returns nil", getKey: "nope.nope", wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			v, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := Get(v, tt.getKey)
			if (got == nil) != tt.wantNil {
				t.Errorf("Get(%q) nil = %v, want %v", tt.getKey, got == nil, tt.wantNil)
			}
			if all := List(v); len(all) == 0 {
				t.Errorf("List returned no settings")
			}
		})
	}
}

func TestQdrantURL(t *testing.T) {
	tests := []struct {
		name string
		mode string
		port int
		url  string
		want string
	}{
		{name: "binary uses the local port", mode: "binary", port: 6333, url: "http://example:6333", want: "http://127.0.0.1:6333"},
		{name: "external uses the configured url", mode: "external", port: 6333, url: "http://example:6333", want: "http://example:6333"},
		{name: "docker uses the configured url", mode: "docker", port: 6333, url: "http://localhost:6333", want: "http://localhost:6333"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Settings{}
			s.RAG.Qdrant.Mode = tt.mode
			s.RAG.Qdrant.HTTPPort = tt.port
			s.RAG.Qdrant.URL = tt.url
			if got := s.QdrantURL(); got != tt.want {
				t.Errorf("QdrantURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQdrantStorageDir(t *testing.T) {
	tests := []struct {
		name        string
		storage     string
		wantDefault bool
	}{
		{name: "default resolves under the data dir", wantDefault: true},
		{name: "config override wins", storage: "/tmp/qdrant-store"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			s := Settings{}
			s.RAG.Qdrant.Storage = tt.storage
			got := s.QdrantStorageDir()
			if tt.wantDefault {
				if want := filepath.Join(paths.DataDir(), "qdrant"); got != want {
					t.Errorf("QdrantStorageDir() = %q, want %q", got, want)
				}
				return
			}
			if got != tt.storage {
				t.Errorf("QdrantStorageDir() = %q, want %q", got, tt.storage)
			}
		})
	}
}

func TestIngestExcludeDirs(t *testing.T) {
	tests := []struct {
		name string
		set  any // value as `orch config set` passes it; nil keeps the default
		want []string
	}{
		{name: "defaults to the built-in list", set: nil, want: mdwalk.DefaultExcludeDirs},
		{name: "comma-separated CLI value replaces the list", set: "vendor,generated", want: []string{"vendor", "generated"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			v, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.set != nil {
				if err := Set(v, "ingest.exclude_dirs", tt.set); err != nil {
					t.Fatalf("Set: %v", err)
				}
				// Reload so the value comes back through the config file.
				if v, err = Load(); err != nil {
					t.Fatalf("reload: %v", err)
				}
			}
			s, err := Unmarshal(v)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !slices.Equal(s.Ingest.ExcludeDirs, tt.want) {
				t.Errorf("ExcludeDirs = %v, want %v", s.Ingest.ExcludeDirs, tt.want)
			}
		})
	}
}

func TestSessionSummaryDefaults(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	v, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Sessions.Summarize; !got.Enabled || got.Model != "haiku" || got.Timeout != "2m" {
		t.Errorf("summary defaults = %+v", got)
	}
}

// A partial source_weights map replaces the default map (the daemon overlays
// it on the defaults; see rankingParams).
func TestSourceWeightsFromConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCH_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("rag:\n  search:\n    source_weights:\n      web: 0.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	w := s.RAG.Search.SourceWeights
	if w["web"] != 0.5 || s.RAG.Search.Ranking != "weighted" || s.RAG.Search.PerSource != 2 {
		t.Fatalf("weights = %v, search = %+v", w, s.RAG.Search)
	}
}
