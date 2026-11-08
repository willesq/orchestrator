# Configuration

`internal/config` loads settings with Viper: compiled defaults, then `<config>/config.yaml`, then
environment variables `ORCH_<KEY>` with dots as underscores (`ORCH_QUEUE_WORKER_ENABLED`). A
missing file is fine.

Writes (`orch config set`, `SetMany`) set keys and rewrite the whole resolved view (`WriteCanonical`):
marshal to YAML, write a temp file in the same directory, fsync, `chmod 0644`, rename. Related keys
change together in one write. `SetConfig` and `orch config set` on a key the schema knows go through
`config.Apply`, which validates first; other keys (one entry of a map, say) are written as given. orchd itself writes config only to record a migration switch or drop.

| Key | Default | Meaning |
|---|---|---|
| `embedder.model_dir` | `<data>/models/minilm` | embedding model directory |
| `embedder.onnx_library_dir` | via `brew --prefix onnxruntime` | ONNX Runtime dylib directory |
| `embedder.idle_unload` | `10m` | unload the model after this idle time |
| `embedder.chunk_tokens` | `110` | chunk size in model tokens (capped at 126, the model's window) |
| `embedder.chunk_overlap_tokens` | `16` | overlap between plain-text windows |
| `embedder.markdown_sections` | `true` | chunk markdown by heading |
| `rag.qdrant.mode` | `binary` | `binary` (orchd runs it), `docker`, or `external` |
| `rag.qdrant.url` | `http://127.0.0.1:6333` | used unless binary mode (then `127.0.0.1:<http_port>`) |
| `rag.qdrant.collection` | `orchestrator` | active collection |
| `rag.qdrant.backup_collection` | empty | set by migration for rollback |
| `rag.qdrant.api_key_env` | `QDRANT_API_KEY` | env var that overrides the generated key |
| `rag.qdrant.version` | `v1.17.1` | pinned engine release |
| `rag.qdrant.storage` | `<data>/qdrant` | engine storage |
| `rag.qdrant.http_port`, `grpc_port` | `6333`, `6334` | engine ports |
| `rag.search.mode` | `hybrid` | or `dense` |
| `rag.search.rrf_k` | `60` | reciprocal rank fusion constant |
| `rag.search.keyword_weight` | `0.5` | keyword list's share of the fusion |
| `rag.search.ranking` | `weighted` | scale fused scores by source type and recency, or `plain` |
| `rag.search.recency_half_life_days` | `180` | days for recency to fall halfway to the floor; 0 turns recency off |
| `rag.search.recency_floor` | `0.85` | least recency scales a score by (0–1) |
| `rag.search.source_weights` | `session_transcript: 0.85`, `web: 0.9`, `session_record`/`decision: 1` | per source type; set types override these defaults, other types weigh 1; values outside (0, 10] are ignored |
| `rag.search.per_source` | `2` | most hits one source returns to a search that sets no cap (every search: MCP, CLI and prompt context, which asks for 1); 0 = no cap |
| `hooks.auto_rag.enabled` | `true` | inject memory into prompts |
| `hooks.auto_rag.top_k` | `3` | hits injected |
| `hooks.auto_rag.min_score` | `0.25` | cosine floor for questions |
| `hooks.auto_rag.min_score_statement` | `0.45` | cosine floor for other prompts |
| `hooks.auto_rag.max_chars` | `2000` | injection size cap |
| `hooks.auto_rag.min_prompt_chars` | `12` | shorter prompts get nothing |
| `hooks.auto_rag.gate` | `evidence` | a prompt must name a topic and a hit must show evidence for it ([hooks.md](hooks.md#prompt-context)); `off` leaves the score floors alone; any other value means on, so a typo cannot quietly turn it off |
| `hooks.auto_rag.format` | `pointers` | one line per hit with its source, or `full` text |
| `hooks.auto_rag.untrusted` | `mark` | inject web pages and tool output labelled untrusted, or `exclude` them |
| `ingest.exclude_dirs` | vendor, node_modules, bower_components, third_party, Pods, .venv, venv, \_\_pycache\_\_, .tox, .terraform, .next, dist, build, target | never walked; `.git` always skipped |
| `sources.known_roots` | `[~/code]` | where `<owner>/<repo>` checkouts live |
| `sources.scan_interval` | `5m` | sources scheduler tick |
| `docs.scan_interval` | `1m` | docs scheduler tick |
| `docs.repo_cache` | `<data>/repos` | clones of GitHub owners' repos |
| `docs.default_refresh` | `6h` | per-owner refresh cadence |
| `docs.include_archived`, `include_forks` | `false` | repo filters |
| `pr_babysitter.enabled` | `true` | PR cache refresh |
| `pr_babysitter.poll_interval` | `10m` | sweep cadence |
| `sessions.summarize.enabled` | `true` | model-written session summaries |
| `sessions.summarize.model` | `haiku` | model for summaries |
| `sessions.summarize.timeout` | `2m` | per summary |
| `queue.max_active` | `20` | pending + running tasks; dispatch fails beyond. 1 to 50, never below `max_concurrent` |
| `queue.worker.enabled` | `true` | run dispatched tasks (never with `bypassPermissions`; sessions cannot dispatch more) |
| `queue.worker.claude_bin` | found automatically | Claude Code binary |
| `queue.worker.permission_mode` | `auto` | `auto`, `dontAsk`, `acceptEdits` or `plan` |
| `queue.worker.timeout` | `60m` | per task |
| `queue.worker.max_concurrent` | `1` | parallel tasks, 0 to 50; 0 turns the worker off |
| `queue.worker.allowed_tools.claude` | read-only git/ls/go test/go vet/gh pr view/orch search set | rules granted without asking |
| `queue.worker.verify.enabled` | `true` | test a task's changes before completing it |
| `queue.worker.verify.timeout` | `15m` | per check |
| `queue.worker.verify.retries` | `1` | resumes with the failure output |
| `queue.worker.verify.repos` | `[]` | repositories trusted to run their own tests |
| `queue.worker.codex.enabled` | `false` | run tasks routed to Codex |
| `queue.worker.codex.bin` | found automatically | Codex binary |
| `queue.worker.codex.sandbox` | `workspace-write` | or `read-only`; full access refused |
| `queue.worker.codex.network` | `false` | network inside the sandbox |
| `queue.worker.codex.mcp_allow` | `[orch_rag]` | MCP servers kept for Codex runs |
| `queue.worker.codex.allow_project_config` | `false` | honor a repo's `.codex` config |
| `routing.kinds.<kind>` | frontier: code, review, adversarial; balanced: research, docs, ops, test; fast: classify | preferred tier |
| `routing.providers` | `[claude-code, codex]` | providers that may be proposed, in order |
| `routing.efforts.<kind>` | none | effort level to request |
| `routing.ranks` | `3` | routes kept per kind |
| `routing.min_evidence` | `5` | outcomes before evidence moves a rank |
| `routing.evaluate.enabled`, `interval` | `true`, `168h` | evaluator subsystem |
| `routing.auto_apply` | `false` | evaluator only proposes |

## Database store (not yet read)

Settings are moving from `config.yaml` into `orch.db` ([plan](../CONFIG_IN_DB_PLAN.md)). Migration
`0022` adds `config_values` (key, JSON value, `updated_at`, `updated_by`) and `config_import`.
Only values that differ from the compiled default are stored: `ApplyStore` validates as `Apply` does
and writes in one transaction, deleting a key's row when its value equals the default, and
`ResetStore` puts keys back to their defaults with the same sensitive-key check. `LoadStore` layers
the rows over the defaults; a row naming no setting, or no longer valid, is skipped and logged by key.
`ImportLegacy` copies a `config.yaml` into the store once: values that differ from today's default
are imported as they are (the queue limits as the old loader clamped them), invalid and unknown keys
are reported by name, a `config_import` row stops it from running again, and the file is left in
place. Nothing calls these yet; orchd, orch-hook and the CLI still read the file.

## Schema

`config.Fields` (`internal/config/schema.go`) describes every key: its section, label, help, type, range or
options, and two flags. It is what `GetConfig` serves and the app's Settings pane is built from. A test fails
when a default has no field.

- **Sensitive:** a key that changes what agents may run or where orch keeps its data. `SetConfig` refuses it
  without `confirm_sensitive`, and the app asks first. `orch config set` does not ask, because the operator
  typed the key.
- **Live:** a key each short-lived process reads for itself (`hooks.auto_rag.*`, read by orch-hook on every
  prompt). Every other key takes effect when orchd restarts: `Restart` restarts subsystems in place, without
  reading config again.

`GetConfig` compares each key against a snapshot from orchd's start, so it reports what is waiting for a
restart however the value was changed, and a value set back stops waiting. A key an environment variable
overrides is reported as such, and writing it is refused, since the write would have no effect.

## Queue limits

`max_concurrent` and `max_active` are held to `config.MaxParallelTasks` and `config.MaxActiveTasks` (both 50)
on every route: `Apply` refuses more, and `Unmarshal` clamps a hand-edited file or environment variable and
logs it, raising `max_active` to at least `max_concurrent`. `max_active: 0` used to mean no cap and now means
50. The worker runs only when `queue.worker.enabled` is true and `max_concurrent` is above 0
(`QueueWorkerSettings.Runs`); `max_concurrent: 0` used to mean 1 and now means off. Setting it above 0 through
`Apply` also turns `enabled` on and raises `max_active` if needed, so the app offers the one number.
