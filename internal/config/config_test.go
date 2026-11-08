package config

import (
	"database/sql"
	"errors"
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
			v := loadTemp(t)
			if got := v.GetString(tt.key); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestAppliedValueReloadsAndKeepsDefaults(t *testing.T) {
	loadTemp(t)
	if err := applyTemp(t, map[string]any{"rag.qdrant.mode": "external"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.LegacyConfigFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a write created config.yaml: %v", err)
	}
	v := reload(t)
	if got := v.GetString("rag.qdrant.mode"); got != "external" {
		t.Errorf("mode after reload = %q, want external", got)
	}
	if got := v.GetString("rag.qdrant.version"); got != "v1.17.1" {
		t.Errorf("default lost: version = %q", got)
	}
}

func TestLoadIgnoresALegacyFile(t *testing.T) {
	loadTemp(t)
	if err := os.WriteFile(paths.LegacyConfigFile(), []byte("rag:\n  qdrant:\n    mode: external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := reload(t).GetString("rag.qdrant.mode"); got != "binary" {
		t.Errorf("mode = %q: Load read config.yaml", got)
	}
}

func TestLoadWithAStoreOlderThanTheSettingsTable(t *testing.T) {
	loadTemp(t)
	if err := os.MkdirAll(paths.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", "file:"+paths.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`CREATE TABLE meta (k TEXT)`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := reload(t).GetString("rag.qdrant.mode"); got != "binary" {
		t.Errorf("mode = %q, want the default", got)
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
			v := loadTemp(t)
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
			v := loadTemp(t)
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
		{name: "a stored list replaces the built-in one", set: []string{"vendor", "generated"}, want: []string{"vendor", "generated"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := loadTemp(t)
			if tt.set != nil {
				if err := applyTemp(t, map[string]any{"ingest.exclude_dirs": tt.set}, true); err != nil {
					t.Fatalf("apply: %v", err)
				}
				v = reload(t)
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
	v := loadTemp(t)
	var err error
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
	loadTemp(t)
	if err := applyTemp(t, map[string]any{"rag.search.source_weights": map[string]any{"web": 0.5}}, true); err != nil {
		t.Fatal(err)
	}
	s, err := Unmarshal(reload(t))
	if err != nil {
		t.Fatal(err)
	}
	w := s.RAG.Search.SourceWeights
	if w["web"] != 0.5 || s.RAG.Search.Ranking != "weighted" || s.RAG.Search.PerSource != 2 {
		t.Fatalf("weights = %v, search = %+v", w, s.RAG.Search)
	}
}
