// Package config loads orchestrator settings with Viper (defaults < file < env)
// and persists CLI-driven changes through a canonical writer.
//
// We deliberately do NOT use viper.WriteConfig for persistence: it drops comments
// and key ordering. Instead WriteCanonical marshals the resolved settings to YAML
// deterministically, which keeps the on-disk file stable across writes.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"

	"github.com/willesq/orchestrator/internal/mdwalk"
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
	Ingest       IngestSettings       `mapstructure:"ingest"`
	Queue        QueueSettings        `mapstructure:"queue"`
	Sessions     SessionsSettings     `mapstructure:"sessions"`
	Routing      RoutingSettings      `mapstructure:"routing"`
}

// RoutingSettings is the declared-capability policy model routing ranks the
// registry's enabled models with (orch routing evaluate).
type RoutingSettings struct {
	// Kinds maps each task kind to the tier it prefers: frontier, balanced or fast.
	Kinds map[string]string `mapstructure:"kinds"`
	// Providers orders the providers routing may propose, most preferred first;
	// a provider not listed is never proposed.
	Providers []string `mapstructure:"providers"`
	// Efforts maps a task kind to the reasoning effort to request, applied only
	// to models that declare that level. Empty uses each model's default.
	Efforts map[string]string `mapstructure:"efforts"`
	// Ranks is how many fallback routes each kind keeps.
	Ranks int `mapstructure:"ranks"`
	// MinEvidence is how many observed outcomes (ratings, PR fates, failures) a
	// model needs on a kind before they move its rank.
	MinEvidence int `mapstructure:"min_evidence"`
	// Evaluate controls the routing-evaluator subsystem, which re-runs discovery
	// and evaluation on a cadence and reports to the session digest.
	Evaluate RoutingEvaluateSettings `mapstructure:"evaluate"`
	// AutoApply lets the evaluator apply its proposals unattended. Off, it only
	// proposes; pinned kinds are kept either way.
	AutoApply bool `mapstructure:"auto_apply"`
}

// RoutingEvaluateSettings controls the routing-evaluator subsystem.
type RoutingEvaluateSettings struct {
	Enabled  bool   `mapstructure:"enabled"`
	Interval string `mapstructure:"interval"`
}

// DocsSettings controls the GitHub code-docs subsystem (orch docs).
type DocsSettings struct {
	ScanInterval    string `mapstructure:"scan_interval"`    // how often to scan for due owners
	RepoCache       string `mapstructure:"repo_cache"`       // clone cache dir (empty → <data>/repos)
	DefaultRefresh  string `mapstructure:"default_refresh"`  // default per-owner cadence
	IncludeArchived bool   `mapstructure:"include_archived"` // ingest archived repos
	IncludeForks    bool   `mapstructure:"include_forks"`    // ingest forks
}

// RepoCacheDir is the docs clone cache dir (config override or <data>/repos).
// Shared by orchd's scheduler and the CLI's foreground sync so both clone into
// the same place and keep each other's incremental caches warm.
func (s Settings) RepoCacheDir() string {
	if s.Docs.RepoCache != "" {
		return s.Docs.RepoCache
	}
	return filepath.Join(paths.DataDir(), "repos")
}

// QdrantURL is the base URL of the vector store: the local port orchd's own
// native engine listens on in binary mode, else the configured URL. Shared by
// orchd (which dials it) and `orch status` (which reports it).
func (s Settings) QdrantURL() string {
	if s.RAG.Qdrant.Mode == "binary" {
		return fmt.Sprintf("http://127.0.0.1:%d", s.RAG.Qdrant.HTTPPort)
	}
	return s.RAG.Qdrant.URL
}

// QdrantStorageDir is the native engine's storage directory (config override or
// <data>/qdrant). Only meaningful in binary mode.
func (s Settings) QdrantStorageDir() string {
	if s.RAG.Qdrant.Storage != "" {
		return s.RAG.Qdrant.Storage
	}
	return filepath.Join(paths.DataDir(), "qdrant")
}

// QueueVerifySettings controls the check run against a queued task's work. orchd
// finds the checkouts of the task's repository whose HEAD moved during the task
// and runs each one's detected test command (a make test target, else go test,
// cargo test or the package.json test script).
type QueueVerifySettings struct {
	Enabled bool `mapstructure:"enabled"`
	// Repos lists the repositories (main checkout paths, ~ allowed) whose tests
	// orchd may run. A check executes the repository's own code outside the
	// agent's permission checks, so only listed repositories are verified.
	Repos   []string `mapstructure:"repos"`
	Timeout string   `mapstructure:"timeout"` // per check command
	// Retries is how often a failing check goes back to the agent, resuming its
	// session with the output, before the task fails.
	Retries int `mapstructure:"retries"`
}

// QueueSettings controls the durable dispatch queue.
type QueueSettings struct {
	Worker QueueWorkerSettings `mapstructure:"worker"`
	// MaxActive caps tasks pending or in progress; dispatch fails beyond it.
	MaxActive int `mapstructure:"max_active"`
}

// QueueWorkerSettings controls the worker that runs dispatched tasks as headless
// agent sessions. It is opt-in: with it disabled, tasks are recorded and stay
// pending until a worker is enabled.
type QueueWorkerSettings struct {
	Enabled bool `mapstructure:"enabled"`
	// ClaudeBin is the Claude Code CLI to run; empty resolves it from PATH and the
	// usual install locations, since orchd runs with launchd's minimal PATH.
	ClaudeBin string `mapstructure:"claude_bin"`
	// PermissionMode is passed to --permission-mode. Nobody can answer a prompt
	// in a headless run, so it must be a mode that decides without asking.
	PermissionMode string `mapstructure:"permission_mode"`
	Timeout        string `mapstructure:"timeout"`        // per-task wall-clock cap
	MaxConcurrent  int    `mapstructure:"max_concurrent"` // tasks run at once
	// AllowedTools maps a worker harness ("claude") to the permission rules its
	// headless sessions get without asking. Each harness spells its rules its own
	// way, so lists are kept per harness; only "claude" is used today.
	AllowedTools map[string][]string `mapstructure:"allowed_tools"`
	// Verify checks the agent's work before a task counts as complete.
	Verify QueueVerifySettings `mapstructure:"verify"`
	// Codex lets the worker run tasks routed to Codex models.
	Codex QueueCodexSettings `mapstructure:"codex"`
}

// QueueCodexSettings controls the worker's Codex runs. Off by default: with it
// off, dispatch never routes to Codex.
type QueueCodexSettings struct {
	Enabled bool   `mapstructure:"enabled"`
	Bin     string `mapstructure:"bin"` // empty resolves codex from PATH and the usual locations
	// Sandbox is Codex's sandbox for the session's commands: workspace-write
	// (the default) or read-only. Full access is refused.
	Sandbox string `mapstructure:"sandbox"`
	// Network lets sandboxed commands reach the network, which pushing a
	// branch and opening a PR need. Off by default: the sandbox can read every
	// file the user can, so with network a steered session could send any of
	// them out.
	Network bool `mapstructure:"network"`
	// MCPAllow names the MCP servers a Codex task keeps; every other server
	// configured for Codex is turned off for the run, since MCP servers run
	// outside the sandbox.
	MCPAllow []string `mapstructure:"mcp_allow"`
	// AllowProjectConfig runs Codex tasks in checkouts that carry a .codex
	// directory. Off by default: project config can redirect the model
	// provider, which the worker cannot pin.
	AllowProjectConfig bool `mapstructure:"allow_project_config"`
}

// IngestSettings controls which files the repo walkers (sources and docs) ingest.
type IngestSettings struct {
	// ExcludeDirs names directories never walked for markdown, matched exactly
	// against each directory name. Setting it replaces the default list; .git is
	// always skipped.
	ExcludeDirs []string `mapstructure:"exclude_dirs"`
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
	Enabled  bool    `mapstructure:"enabled"`
	TopK     int     `mapstructure:"top_k"`
	MinScore float64 `mapstructure:"min_score"` // Cosine similarity floor (higher = stricter)
	// MinScoreStatement is the floor for prompts that aren't question-shaped;
	// the stricter of it and MinScore applies.
	MinScoreStatement float64 `mapstructure:"min_score_statement"`
	MaxChars          int     `mapstructure:"max_chars"`
	MinPromptChars    int     `mapstructure:"min_prompt_chars"`
	// Format is "pointers" (a title, date, source and the most relevant line
	// per hit) or "full" (each hit's whole text).
	Format string `mapstructure:"format"`
	// Untrusted is "mark" (inject web pages and tool output labelled as
	// untrusted, their text quoted) or "exclude" (never inject them).
	Untrusted string `mapstructure:"untrusted"`
}

// SessionsSettings controls what orchd does with finished sessions.
type SessionsSettings struct {
	Summarize SessionSummarySettings `mapstructure:"summarize"`
}

// SessionSummarySettings controls the model-written summary of each finished
// session, which replaces the transcript excerpt as its session record.
type SessionSummarySettings struct {
	Enabled bool   `mapstructure:"enabled"`
	Model   string `mapstructure:"model"`   // passed to claude --model
	Timeout string `mapstructure:"timeout"` // per summary
}

// EmbedderSettings controls the in-orchd embedding model lifecycle.
type EmbedderSettings struct {
	ModelDir   string `mapstructure:"model_dir"`
	OnnxLibDir string `mapstructure:"onnx_library_dir"`
	IdleUnload string `mapstructure:"idle_unload"`
	// ChunkTokens sizes chunks in model tokens. The model embeds only the first
	// 128 tokens of its input (126 after its special tokens), so larger values
	// are capped there; a chunk sized in words ran far past it.
	ChunkTokens int `mapstructure:"chunk_tokens"`
	// ChunkOverlapTokens is how many tokens neighbouring windows share.
	ChunkOverlapTokens int `mapstructure:"chunk_overlap_tokens"`
	// MarkdownSections chunks markdown by heading section, keeping its structure,
	// instead of by token window.
	MarkdownSections bool `mapstructure:"markdown_sections"`
}

// RAGSettings controls the vector store.
type RAGSettings struct {
	Qdrant QdrantSettings `mapstructure:"qdrant"`
	Search SearchSettings `mapstructure:"search"`
}

// SearchSettings controls how RAG search ranks results.
type SearchSettings struct {
	// Mode is "hybrid" (keyword and semantic, fused) or "dense" (semantic
	// only). Hybrid needs a hybrid collection; on an older one search is dense.
	Mode string `mapstructure:"mode"`
	// RRFK is reciprocal rank fusion's constant: larger values flatten the
	// advantage of the top ranks of each list.
	RRFK int `mapstructure:"rrf_k"`
	// KeywordWeight scales the keyword list's share of the fusion: below 1
	// favors meaning, above 1 favors exact words. The default, 0.5, kept the
	// identifier wins without costing prose recall on the golden-set eval.
	KeywordWeight float64 `mapstructure:"keyword_weight"`
	// Ranking is "weighted" (scale fused scores by source type and recency)
	// or "plain" (fusion alone).
	Ranking string `mapstructure:"ranking"`
	// RecencyHalfLifeDays is how many days recency takes to fall halfway to
	// RecencyFloor; 0 turns recency off.
	RecencyHalfLifeDays float64 `mapstructure:"recency_half_life_days"`
	// RecencyFloor is the least recency scales a score by (0 to 1), so an old
	// decision that still matches is not buried.
	RecencyFloor float64 `mapstructure:"recency_floor"`
	// SourceWeights scale scores by source type; unlisted types weigh 1.
	SourceWeights map[string]float64 `mapstructure:"source_weights"`
	// PerSource caps the hits one source may return to a search that sets no
	// cap of its own; 0 leaves them uncapped.
	PerSource int `mapstructure:"per_source"`
}

// QdrantSettings selects how the Qdrant vector store is provided and, for
// binary mode, how the native engine is provisioned.
type QdrantSettings struct {
	Mode       string `mapstructure:"mode"` // docker | binary | external
	URL        string `mapstructure:"url"`
	Collection string `mapstructure:"collection"`
	// BackupCollection is the collection a hybrid migration replaced, kept so
	// `orch rag migrate-hybrid --rollback` can switch back to it.
	BackupCollection string `mapstructure:"backup_collection"`
	APIKeyEnv        string `mapstructure:"api_key_env"`
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
	v.SetDefault("embedder.chunk_tokens", 110)
	v.SetDefault("embedder.chunk_overlap_tokens", 16)
	v.SetDefault("embedder.markdown_sections", true)
	// Docker-free by default: orchd runs the native qdrant release binary.
	v.SetDefault("rag.qdrant.mode", "binary")
	v.SetDefault("rag.qdrant.url", "http://127.0.0.1:6333")
	// The canonical collection served by orchd's native engine (cutover complete;
	// the earlier "orchestrator_go" parallel-dev name is retired).
	v.SetDefault("rag.qdrant.collection", "orchestrator")
	v.SetDefault("rag.qdrant.backup_collection", "")
	v.SetDefault("rag.search.mode", "hybrid")
	v.SetDefault("rag.search.rrf_k", 60)
	v.SetDefault("rag.search.keyword_weight", 0.5)
	// Ranking after fusion: weighted by source type and recency, with a floor
	// so an old decision is not buried (tuned on the golden-set eval).
	v.SetDefault("rag.search.ranking", "weighted")
	v.SetDefault("rag.search.recency_half_life_days", 180)
	v.SetDefault("rag.search.recency_floor", 0.7)
	v.SetDefault("rag.search.source_weights", map[string]float64{
		"session_record": 1.0, "decision": 1.0, "session_transcript": 0.7, "web": 0.9,
	})
	v.SetDefault("rag.search.per_source", 2)
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
	v.SetDefault("hooks.auto_rag.min_score_statement", 0.45)
	v.SetDefault("hooks.auto_rag.max_chars", 2000)
	v.SetDefault("hooks.auto_rag.min_prompt_chars", 12)
	v.SetDefault("hooks.auto_rag.format", "pointers")
	v.SetDefault("hooks.auto_rag.untrusted", "mark")
	// Source registry + freshness scheduler.
	v.SetDefault("sources.known_roots", []string{"~/code"})
	// Third-party and generated trees whose READMEs would crowd out real project docs.
	v.SetDefault("ingest.exclude_dirs", mdwalk.DefaultExcludeDirs)
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
	// Session summaries: one small, tool-less model call per finished session.
	v.SetDefault("sessions.summarize.enabled", true)
	v.SetDefault("sessions.summarize.model", "haiku")
	v.SetDefault("sessions.summarize.timeout", "2m")
	// Queue worker: off until explicitly enabled, because it spawns agent sessions.
	v.SetDefault("queue.max_active", 20)
	v.SetDefault("queue.worker.enabled", false)
	v.SetDefault("queue.worker.claude_bin", "")
	v.SetDefault("queue.worker.permission_mode", "auto")
	v.SetDefault("queue.worker.timeout", "60m")
	v.SetDefault("queue.worker.max_concurrent", 1)
	v.SetDefault("queue.worker.verify.enabled", true)
	v.SetDefault("queue.worker.verify.timeout", "15m")
	v.SetDefault("queue.worker.verify.retries", 1)
	v.SetDefault("queue.worker.verify.repos", []string{})
	v.SetDefault("queue.worker.codex.enabled", false)
	v.SetDefault("queue.worker.codex.bin", "")
	v.SetDefault("queue.worker.codex.sandbox", "workspace-write")
	v.SetDefault("queue.worker.codex.network", false)
	v.SetDefault("queue.worker.codex.mcp_allow", []string{"orch_rag"})
	v.SetDefault("queue.worker.codex.allow_project_config", false)
	// Routing: harder or riskier work prefers the frontier tier, bulk and
	// mechanical work a cheaper one.
	for kind, tier := range map[string]string{
		"code": "frontier", "review": "frontier", "adversarial": "frontier",
		"research": "balanced", "docs": "balanced", "ops": "balanced", "test": "balanced",
		"classify": "fast",
	} {
		v.SetDefault("routing.kinds."+kind, tier)
	}
	v.SetDefault("routing.providers", []string{"claude-code", "codex"})
	v.SetDefault("routing.ranks", 3)
	v.SetDefault("routing.min_evidence", 5)
	v.SetDefault("routing.evaluate.enabled", true)
	v.SetDefault("routing.evaluate.interval", "168h")
	v.SetDefault("routing.auto_apply", false)
	// Read-only commands a queued session needs constantly; granting them up front
	// keeps a dontAsk run from failing on its first `git status`.
	v.SetDefault("queue.worker.allowed_tools.claude", []string{
		"Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)",
		"Bash(git branch:*)", "Bash(ls:*)", "Bash(go test:*)", "Bash(go vet:*)",
		"Bash(gh pr view:*)", "Bash(gh pr checks:*)", "Bash(gh pr diff:*)",
		"Bash(orch search:*)", "Bash(orch status:*)",
	})
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

// SetMany assigns several keys and persists them in one write, so settings
// that only make sense together never land half-changed.
func SetMany(v *viper.Viper, kv map[string]any) error {
	for k, val := range kv {
		v.Set(k, val)
	}
	return WriteCanonical(v)
}

// WriteCanonical marshals all resolved settings to YAML and writes config.yaml,
// atomically: a reader, or a crash mid-write, sees the old file or the new one,
// never a truncated mix.
func WriteCanonical(v *viper.Viper) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	out, err := yaml.Marshal(v.AllSettings())
	if err != nil {
		return err
	}
	path := paths.ConfigFile()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
