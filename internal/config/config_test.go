package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "embedder idle_unload default", key: "embedder.idle_unload", want: "10m"},
		{name: "qdrant mode default is docker-free", key: "rag.qdrant.mode", want: "binary"},
		{name: "collection is isolated from python", key: "rag.qdrant.collection", want: "orchestrator_go"},
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
		{name: "defaults decode into typed struct", wantURL: "http://127.0.0.1:6333", wantCol: "orchestrator_go"},
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
