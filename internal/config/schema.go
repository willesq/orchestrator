package config

// Limits on the task queue. They are absolute: Apply refuses anything past
// them and Unmarshal clamps a hand-edited file or environment variable, so no
// configuration can start more than this many agent sessions at once.
const (
	// MaxParallelTasks caps queue.worker.max_concurrent. 0 turns the worker off.
	MaxParallelTasks = 50
	// MaxActiveTasks caps queue.max_active, tasks running plus waiting.
	MaxActiveTasks = 50
)

const (
	keyWorkerEnabled = "queue.worker.enabled"
	keyMaxConcurrent = "queue.worker.max_concurrent"
	keyMaxActive     = "queue.max_active"
)

// FieldType says how a setting's value is shaped, validated and edited.
type FieldType string

// The field types.
const (
	TypeBool     FieldType = "bool"
	TypeInt      FieldType = "int"
	TypeFloat    FieldType = "float"
	TypeString   FieldType = "string"
	TypeDuration FieldType = "duration"
	TypeEnum     FieldType = "enum"
	TypePath     FieldType = "path" // absolute, or empty for the built-in default
	TypeURL      FieldType = "url"  // http or https, without credentials
	TypeVersion  FieldType = "version"
	TypeList     FieldType = "list"
	TypeMap      FieldType = "map" // JSON object; MapValues says what it holds
)

// Field describes one setting. Fields is the single list every client's view
// of config.yaml comes from; a test fails when a default has no field.
type Field struct {
	Key     string
	Section string
	Label   string
	Help    string
	Type    FieldType
	Options []string // TypeEnum
	// HasRange turns on Min and Max, for TypeInt and TypeFloat.
	HasRange bool
	Min, Max float64
	// MapValues is what a TypeMap holds: TypeString, TypeFloat or TypeList.
	MapValues FieldType
	// Sensitive settings change what agents may run or where orch keeps its
	// data, so a client must confirm them explicitly.
	Sensitive bool
	// Live settings are read fresh by each short-lived process; every other
	// setting takes effect when orchd restarts.
	Live bool
	// Hidden settings keep working but are not offered to clients:
	// queue.worker.enabled is folded into max_concurrent's 0.
	Hidden bool
}

// The sections, in the order clients show them.
const (
	SectionTasks     = "Tasks"
	SectionVerify    = "Task verification"
	SectionCodex     = "Codex"
	SectionRouting   = "Model routing"
	SectionDocs      = "Docs"
	SectionSources   = "Sources"
	SectionGitHub    = "GitHub"
	SectionContext   = "Prompt context"
	SectionSummaries = "Session summaries"
	SectionSearch    = "Search"
	SectionEmbedding = "Embedding"
	SectionStore     = "Vector store"
)

// Fields lists every setting in config.yaml.
var Fields = buildFields()

func buildFields() []Field {
	ranged := func(f Field, lo, hi float64) Field {
		f.HasRange, f.Min, f.Max = true, lo, hi
		return f
	}
	return []Field{
		// Tasks.
		ranged(Field{Key: keyMaxConcurrent, Section: SectionTasks, Label: "Parallel tasks", Type: TypeInt,
			Help: "Tasks that run at the same time, each a full agent session, so more means more usage and load. 0 turns the worker off, and tasks wait until it is on again."}, 0, MaxParallelTasks),
		ranged(Field{Key: keyMaxActive, Section: SectionTasks, Label: "Queue limit", Type: TypeInt,
			Help: "Tasks running plus waiting. A new task is refused past this. Never lower than parallel tasks."}, 1, MaxActiveTasks),
		{Key: keyWorkerEnabled, Section: SectionTasks, Label: "Worker enabled", Type: TypeBool, Hidden: true},
		{Key: "queue.worker.timeout", Section: SectionTasks, Label: "Task timeout", Type: TypeDuration,
			Help: "Longest one task may run."},
		{Key: "queue.worker.permission_mode", Section: SectionTasks, Label: "Permission mode", Type: TypeEnum, Sensitive: true,
			Options: []string{"auto", "dontAsk", "acceptEdits", "plan"},
			Help:    "How headless Claude Code sessions decide on tool use. Nobody can answer a prompt, and bypassPermissions is refused."},
		{Key: "queue.worker.allowed_tools", Section: SectionTasks, Label: "Allowed tools", Type: TypeMap, MapValues: TypeList, Sensitive: true,
			Help: "Permission rules each harness's sessions get without asking."},
		{Key: "queue.worker.claude_bin", Section: SectionTasks, Label: "Claude Code binary", Type: TypePath, Sensitive: true,
			Help: "The program run for each task. Empty finds it automatically."},

		// Verification.
		{Key: "queue.worker.verify.enabled", Section: SectionVerify, Label: "Test a task's changes", Type: TypeBool,
			Help: "Run the repository's tests before a task counts as complete."},
		{Key: "queue.worker.verify.timeout", Section: SectionVerify, Label: "Check timeout", Type: TypeDuration,
			Help: "Longest one check may run."},
		ranged(Field{Key: "queue.worker.verify.retries", Section: SectionVerify, Label: "Retries", Type: TypeInt,
			Help: "How often a failing check goes back to the agent."}, 0, 10),
		{Key: "queue.worker.verify.repos", Section: SectionVerify, Label: "Trusted repositories", Type: TypeList, Sensitive: true,
			Help: "Checkouts whose tests orchd may run, outside the agent's permission checks."},

		// Codex.
		{Key: "queue.worker.codex.enabled", Section: SectionCodex, Label: "Run tasks on Codex", Type: TypeBool, Sensitive: true,
			Help: "Off, dispatch never routes to Codex."},
		{Key: "queue.worker.codex.bin", Section: SectionCodex, Label: "Codex binary", Type: TypePath, Sensitive: true,
			Help: "Empty finds it automatically."},
		{Key: "queue.worker.codex.sandbox", Section: SectionCodex, Label: "Sandbox", Type: TypeEnum, Sensitive: true,
			Options: []string{"workspace-write", "read-only"},
			Help:    "The sandbox for a session's commands. Full access is refused."},
		{Key: "queue.worker.codex.network", Section: SectionCodex, Label: "Network in the sandbox", Type: TypeBool, Sensitive: true,
			Help: "The sandbox can read every file you can, so a steered session with network could send any of them out."},
		{Key: "queue.worker.codex.mcp_allow", Section: SectionCodex, Label: "MCP servers kept", Type: TypeList, Sensitive: true,
			Help: "MCP servers run outside the sandbox; every other one is turned off for the run."},
		{Key: "queue.worker.codex.allow_project_config", Section: SectionCodex, Label: "Honor a repo's .codex config", Type: TypeBool, Sensitive: true,
			Help: "Project config can redirect the model provider, which the worker cannot pin."},

		// Routing.
		{Key: "routing.kinds", Section: SectionRouting, Label: "Tier per task kind", Type: TypeMap, MapValues: TypeString,
			Help: "frontier, balanced or fast for each kind of task."},
		{Key: "routing.efforts", Section: SectionRouting, Label: "Effort per task kind", Type: TypeMap, MapValues: TypeString,
			Help: "Reasoning effort to request, for models that declare it."},
		{Key: "routing.providers", Section: SectionRouting, Label: "Providers", Type: TypeList,
			Help: "Providers that may be proposed, most preferred first."},
		ranged(Field{Key: "routing.ranks", Section: SectionRouting, Label: "Routes per kind", Type: TypeInt,
			Help: "Fallback routes each kind keeps."}, 1, 20),
		ranged(Field{Key: "routing.min_evidence", Section: SectionRouting, Label: "Outcomes before evidence counts", Type: TypeInt,
			Help: "Outcomes a model needs on a kind before they move its rank."}, 0, 1000),
		{Key: "routing.evaluate.enabled", Section: SectionRouting, Label: "Evaluate routes on a schedule", Type: TypeBool},
		{Key: "routing.evaluate.interval", Section: SectionRouting, Label: "Evaluation interval", Type: TypeDuration},
		{Key: "routing.auto_apply", Section: SectionRouting, Label: "Apply proposals unattended", Type: TypeBool, Sensitive: true,
			Help: "Off, the evaluator only proposes. Pinned kinds are kept either way."},

		// Docs.
		{Key: "docs.default_refresh", Section: SectionDocs, Label: "Default refresh", Type: TypeDuration,
			Help: "How often a newly added owner is synced."},
		{Key: "docs.scan_interval", Section: SectionDocs, Label: "Scan interval", Type: TypeDuration,
			Help: "How often orchd looks for owners that are due."},
		{Key: "docs.include_archived", Section: SectionDocs, Label: "Include archived repos", Type: TypeBool},
		{Key: "docs.include_forks", Section: SectionDocs, Label: "Include forks", Type: TypeBool},
		{Key: "docs.repo_cache", Section: SectionDocs, Label: "Clone cache", Type: TypePath, Sensitive: true,
			Help: "Where owners' repos are cloned. Empty uses orch's data folder."},

		// Sources.
		{Key: "sources.known_roots", Section: SectionSources, Label: "Known roots", Type: TypeList,
			Help: "Folders that hold <owner>/<repo> checkouts."},
		{Key: "sources.scan_interval", Section: SectionSources, Label: "Scan interval", Type: TypeDuration},
		{Key: "ingest.exclude_dirs", Section: SectionSources, Label: "Excluded folders", Type: TypeList,
			Help: "Folder names never walked for markdown. .git is always skipped."},

		// GitHub.
		{Key: "pr_babysitter.enabled", Section: SectionGitHub, Label: "Watch GitHub", Type: TypeBool,
			Help: "Keep the cache of PRs and issues in your enabled scopes fresh."},
		{Key: "pr_babysitter.poll_interval", Section: SectionGitHub, Label: "Sweep interval", Type: TypeDuration},

		// Prompt context, read by orch-hook on every prompt.
		{Key: "hooks.auto_rag.enabled", Section: SectionContext, Label: "Add memory to prompts", Type: TypeBool, Live: true},
		ranged(Field{Key: "hooks.auto_rag.top_k", Section: SectionContext, Label: "Hits added", Type: TypeInt, Live: true}, 1, 50),
		ranged(Field{Key: "hooks.auto_rag.min_score", Section: SectionContext, Label: "Score floor for questions", Type: TypeFloat, Live: true}, 0, 1),
		ranged(Field{Key: "hooks.auto_rag.min_score_statement", Section: SectionContext, Label: "Score floor for other prompts", Type: TypeFloat, Live: true}, 0, 1),
		ranged(Field{Key: "hooks.auto_rag.max_chars", Section: SectionContext, Label: "Size limit (characters)", Type: TypeInt, Live: true}, 100, 100000),
		ranged(Field{Key: "hooks.auto_rag.min_prompt_chars", Section: SectionContext, Label: "Shortest prompt (characters)", Type: TypeInt, Live: true}, 0, 10000),
		{Key: "hooks.auto_rag.gate", Section: SectionContext, Label: "Evidence gate", Type: TypeEnum, Live: true, Options: []string{"evidence", "off"},
			Help: "A prompt must name a topic and a hit must show evidence for it."},
		{Key: "hooks.auto_rag.format", Section: SectionContext, Label: "Format", Type: TypeEnum, Live: true, Options: []string{"pointers", "full"}},
		{Key: "hooks.auto_rag.untrusted", Section: SectionContext, Label: "Web pages and tool output", Type: TypeEnum, Live: true, Sensitive: true,
			Options: []string{"mark", "exclude"},
			Help:    "Add them labelled as untrusted, or leave them out."},

		// Session summaries.
		{Key: "sessions.summarize.enabled", Section: SectionSummaries, Label: "Summarize finished sessions", Type: TypeBool},
		{Key: "sessions.summarize.model", Section: SectionSummaries, Label: "Model", Type: TypeString},
		{Key: "sessions.summarize.timeout", Section: SectionSummaries, Label: "Timeout", Type: TypeDuration},

		// Search.
		{Key: "rag.search.mode", Section: SectionSearch, Label: "Mode", Type: TypeEnum, Options: []string{"hybrid", "dense"}},
		ranged(Field{Key: "rag.search.rrf_k", Section: SectionSearch, Label: "Fusion constant", Type: TypeInt}, 1, 1000),
		ranged(Field{Key: "rag.search.keyword_weight", Section: SectionSearch, Label: "Keyword weight", Type: TypeFloat}, 0, 10),
		{Key: "rag.search.ranking", Section: SectionSearch, Label: "Ranking", Type: TypeEnum, Options: []string{"weighted", "plain"}},
		ranged(Field{Key: "rag.search.recency_half_life_days", Section: SectionSearch, Label: "Recency half-life (days)", Type: TypeFloat,
			Help: "0 turns recency off."}, 0, 36500),
		ranged(Field{Key: "rag.search.recency_floor", Section: SectionSearch, Label: "Recency floor", Type: TypeFloat}, 0, 1),
		{Key: "rag.search.source_weights", Section: SectionSearch, Label: "Source weights", Type: TypeMap, MapValues: TypeFloat,
			Help: "Score scale per source type, above 0 and up to 10. Other types weigh 1."},
		ranged(Field{Key: "rag.search.per_source", Section: SectionSearch, Label: "Hits per source", Type: TypeInt,
			Help: "0 means no limit."}, 0, 1000),

		// Embedding.
		{Key: "embedder.idle_unload", Section: SectionEmbedding, Label: "Unload after idle", Type: TypeDuration},
		ranged(Field{Key: "embedder.chunk_tokens", Section: SectionEmbedding, Label: "Chunk size (tokens)", Type: TypeInt,
			Help: "Applies to what is ingested from now on."}, 1, 126),
		ranged(Field{Key: "embedder.chunk_overlap_tokens", Section: SectionEmbedding, Label: "Chunk overlap (tokens)", Type: TypeInt}, 0, 125),
		{Key: "embedder.markdown_sections", Section: SectionEmbedding, Label: "Chunk markdown by heading", Type: TypeBool},
		{Key: "embedder.model_dir", Section: SectionEmbedding, Label: "Model folder", Type: TypePath, Sensitive: true,
			Help: "Empty uses orch's data folder."},
		{Key: "embedder.onnx_library_dir", Section: SectionEmbedding, Label: "ONNX Runtime folder", Type: TypePath, Sensitive: true,
			Help: "Empty finds it through Homebrew."},

		// Vector store. A wrong value here stops memory from working.
		{Key: "rag.qdrant.mode", Section: SectionStore, Label: "Mode", Type: TypeEnum, Sensitive: true, Options: []string{"binary", "docker", "external"}},
		{Key: "rag.qdrant.url", Section: SectionStore, Label: "URL", Type: TypeURL, Sensitive: true, Help: "Used unless the mode is binary."},
		{Key: "rag.qdrant.collection", Section: SectionStore, Label: "Collection", Type: TypeString, Sensitive: true},
		{Key: "rag.qdrant.backup_collection", Section: SectionStore, Label: "Backup collection", Type: TypeString, Sensitive: true,
			Help: "Set by a migration, for rolling it back."},
		{Key: "rag.qdrant.api_key_env", Section: SectionStore, Label: "API key variable", Type: TypeString, Sensitive: true,
			Help: "The name of the environment variable, never the key."},
		{Key: "rag.qdrant.version", Section: SectionStore, Label: "Engine version", Type: TypeVersion, Sensitive: true},
		{Key: "rag.qdrant.storage", Section: SectionStore, Label: "Storage folder", Type: TypePath, Sensitive: true,
			Help: "Empty uses orch's data folder."},
		ranged(Field{Key: "rag.qdrant.http_port", Section: SectionStore, Label: "HTTP port", Type: TypeInt, Sensitive: true}, 1, 65535),
		ranged(Field{Key: "rag.qdrant.grpc_port", Section: SectionStore, Label: "gRPC port", Type: TypeInt, Sensitive: true}, 1, 65535),
	}
}

// FieldFor returns the field for an exact key.
func FieldFor(key string) (Field, bool) {
	for _, f := range Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}
