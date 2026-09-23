// Package config loads orchestrator settings with Viper (defaults < file < env)
// and persists CLI-driven changes through a canonical writer.
//
// We deliberately do NOT use viper.WriteConfig for persistence: it drops comments
// and key ordering. Instead WriteCanonical marshals the resolved settings to YAML
// deterministically, which keeps the on-disk file stable across writes.
package config

import (
	"errors"
	"os"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"

	"github.com/willesq/orchestrator/internal/paths"
)

// Settings is the typed view of configuration. It grows per phase.
type Settings struct {
	Embedder     EmbedderSettings     `mapstructure:"embedder"`
	RAG          RAGSettings          `mapstructure:"rag"`
	Hooks        HooksSettings        `mapstructure:"hooks"`
	Sources      SourcesSettings      `mapstructure:"sources"`
	Docs         DocsSettings         `mapstructure:"docs"`
	PRBabysitter PRBabysitterSettings `mapstructure:"pr_babysitter"`
}

// DocsSettings controls the GitHub code-docs subsystem (orch docs).
type DocsSettings struct {
	ScanInterval    string `mapstructure:"scan_interval"`    // how often to scan for due owners
	RepoCache       string `mapstructure:"repo_cache"`       // clone cache dir (empty → <data>/repos)
	DefaultRefresh  string `mapstructure:"default_refresh"`  // default per-owner cadence
	IncludeArchived bool   `mapstructure:"include_archived"` // ingest archived repos
	IncludeForks    bool   `mapstructure:"include_forks"`    // ingest forks
}

// PRBabysitterSettings controls the PR-polling daemon.
type PRBabysitterSettings struct {
	Enabled      bool   `mapstructure:"enabled"`
	PollInterval string `mapstructure:"poll_interval"`
}

// SourcesSettings controls the source registry + freshness scheduler.
type SourcesSettings struct {
	KnownRoots   []string `mapstructure:"known_roots"`   // dirs holding owner/repo trees
	ScanInterval string   `mapstructure:"scan_interval"` // how often to scan for due sources
}

// HooksSettings controls lifecycle-hook behavior.
type HooksSettings struct {
	AutoRAG AutoRAGSettings `mapstructure:"auto_rag"`
}

// AutoRAGSettings controls UserPromptSubmit auto-injection of RAG context.
type AutoRAGSettings struct {
	Enabled        bool    `mapstructure:"enabled"`
	TopK           int     `mapstructure:"top_k"`
	MinScore       float64 `mapstructure:"min_score"` // Cosine similarity floor (higher = stricter)
	MaxChars       int     `mapstructure:"max_chars"`
	MinPromptChars int     `mapstructure:"min_prompt_chars"`
}

// EmbedderSettings controls the in-orchd embedding model lifecycle.
type EmbedderSettings struct {
	ModelDir     string `mapstructure:"model_dir"`
	OnnxLibDir   string `mapstructure:"onnx_library_dir"`
	IdleUnload   string `mapstructure:"idle_unload"`
	ChunkSize    int    `mapstructure:"chunk_size"`
	ChunkOverlap int    `mapstructure:"chunk_overlap"`
}

// RAGSettings controls the vector store.
type RAGSettings struct {
	Qdrant QdrantSettings `mapstructure:"qdrant"`
}

// QdrantSettings selects how the Qdrant vector store is provided and, for
// binary mode, how the native engine is provisioned.
type QdrantSettings struct {
	Mode       string `mapstructure:"mode"` // docker | binary | external
	URL        string `mapstructure:"url"`
	Collection string `mapstructure:"collection"`
	APIKeyEnv  string `mapstructure:"api_key_env"`
	// binary mode: the native engine.
	Version  string `mapstructure:"version"`   // pinned qdrant release, e.g. v1.17.1
	Storage  string `mapstructure:"storage"`   // storage dir (empty → <data>/qdrant)
	HTTPPort int    `mapstructure:"http_port"` // REST port
	GRPCPort int    `mapstructure:"grpc_port"` // gRPC port
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("embedder.model_dir", "")        // resolved to <data>/models/minilm if empty
	v.SetDefault("embedder.onnx_library_dir", "") // resolved via brew if empty
	v.SetDefault("embedder.idle_unload", "10m")
	v.SetDefault("embedder.chunk_size", 400)
	v.SetDefault("embedder.chunk_overlap", 60)
	// Docker-free by default: orchd runs the native qdrant release binary.
	v.SetDefault("rag.qdrant.mode", "binary")
	v.SetDefault("rag.qdrant.url", "http://127.0.0.1:6333")
	// The canonical collection served by orchd's native engine (cutover complete;
	// the earlier "orchestrator_go" parallel-dev name is retired).
	v.SetDefault("rag.qdrant.collection", "orchestrator")
	v.SetDefault("rag.qdrant.api_key_env", "QDRANT_API_KEY")
	// Pinned to match the Python system's engine for storage compatibility at cutover.
	v.SetDefault("rag.qdrant.version", "v1.17.1")
	v.SetDefault("rag.qdrant.storage", "") // resolved to <data>/qdrant if empty
	v.SetDefault("rag.qdrant.http_port", 6333)
	v.SetDefault("rag.qdrant.grpc_port", 6334)
	// Auto-RAG injection on question-shaped prompts (the UserPromptSubmit hook).
	v.SetDefault("hooks.auto_rag.enabled", true)
	v.SetDefault("hooks.auto_rag.top_k", 3)
	v.SetDefault("hooks.auto_rag.min_score", 0.25)
	v.SetDefault("hooks.auto_rag.max_chars", 2000)
	v.SetDefault("hooks.auto_rag.min_prompt_chars", 12)
	// Source registry + freshness scheduler.
	v.SetDefault("sources.known_roots", []string{"~/code"})
	v.SetDefault("sources.scan_interval", "5m")
	// docs: the GitHub code-docs subsystem (orch docs). Short scan tick so a
	// manual `orch docs sync` (which marks an owner due) runs near-immediately.
	v.SetDefault("docs.scan_interval", "1m")
	v.SetDefault("docs.repo_cache", "") // resolved to <data>/repos if empty
	v.SetDefault("docs.default_refresh", "6h")
	v.SetDefault("docs.include_archived", false)
	v.SetDefault("docs.include_forks", false)
	// PR babysitter: poll open PRs via gh and cache their state.
	v.SetDefault("pr_babysitter.enabled", true)
	v.SetDefault("pr_babysitter.poll_interval", "10m")
}

// Load builds a Viper with precedence defaults < config file < env (ORCH_*).
// A missing config file is not an error.
func Load() (*viper.Viper, error) {
	v := viper.New()
	setDefaults(v)
	v.SetConfigFile(paths.ConfigFile())
	v.SetConfigType("yaml")
	v.SetEnvPrefix("ORCH")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return v, nil
}

// Unmarshal decodes the resolved config into the typed Settings.
func Unmarshal(v *viper.Viper) (Settings, error) {
	var s Settings
	if err := v.Unmarshal(&s); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// Get returns a single dotted key's value (nil if unset).
func Get(v *viper.Viper, key string) any { return v.Get(key) }

// List returns all resolved settings as a nested map.
func List(v *viper.Viper) map[string]any { return v.AllSettings() }

// Set assigns a dotted key and persists the whole config canonically.
func Set(v *viper.Viper, key string, value any) error {
	v.Set(key, value)
	return WriteCanonical(v)
}

// WriteCanonical marshals all resolved settings to YAML and writes config.yaml.
func WriteCanonical(v *viper.Viper) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	out, err := yaml.Marshal(v.AllSettings())
	if err != nil {
		return err
	}
	return os.WriteFile(paths.ConfigFile(), out, 0o644)
}
