# Configuration

orch's settings live in `orch.db` and are changed in Orch.app's Settings pane, through orchd's
`GetConfig` and `SetConfig`. There is no settings file and no environment override: `ORCH_<KEY>`
variables are not read. The location variables (`ORCH_DATA_DIR`, `ORCH_CONFIG_DIR`, `ORCH_SOCKET`)
still say where orch keeps its files, `orch.db` among them.

`internal/config` resolves the compiled defaults, then the rows of `config_values` (migration
`0022`: key, JSON value, `updated_at`, `updated_by`). Only values that differ from a default are
stored, so a default changed in a later release reaches everyone who never set it, and setting a
value back to its default deletes its row. Every write validates first (`ApplyStore`, the checks
below) and lands in one transaction: related keys change together, all or nothing. A row that names
no setting, or no longer passes its checks (only possible by editing `orch.db` by hand), is skipped
and logged by key, never by value, and the default applies.

- **Readers.** `config.Load` opens `orch.db` read-only and never migrates it, so orch-hook (on every
  prompt) and the CLI's read commands can call it; with no store yet, or one older than migration
  `0022`, the defaults apply. orchd reads through its own connection (`LoadStore`).
- **Writers.** Orch.app through `SetConfig` (`updated_by` `app`). orch's own maintenance writes
  record what they switched through `config.Save`: a hybrid migration's collection switch and backup
  drop (`orchd:migrate`), `orch qdrant migrate` and `cutover` (`cli:<command>`). `orch config reset`
  (`cli:reset`) is the way out when orchd cannot run at all.
- **CLI.** `orch config get <key>` and `orch config list` read; `orch config reset <key>...` or
  `--all` puts settings back to their defaults and names the keys it reset, refused inside a queued
  task. There is no `orch config set`.
- **Accepted risk.** `orch config reset` writes `orch.db` itself, so orchd's process-ancestry guard
  (daemon.md, "Queued callers") does not cover it: only `ORCH_TASK_ID` and the Claude Code deny rule
  stop a queued task, and a task can clear the variable. Resetting a sensitive key can widen what
  agents may do. This is no wider than before (`orch config set` had the same gate, and a task can
  write `orch.db` or could edit `config.yaml` directly); the harness sandbox is the boundary.

### The old config.yaml

Releases before this kept settings in `<config>/config.yaml`. At startup orchd imports it once
(`ImportLegacy`): every value that differs from today's default is stored as it is, with
`updated_by` `import`, even one an older orch wrote only because it saved every key, since dropping
it would change what runs (a worker that was off would turn on). The queue limits are imported as
the old loader clamped them (`max_active: 0` ran as 50). Invalid and unknown keys keep their
defaults and are logged by name. A `config_import` row records that the import ran, so it never runs
twice; a first start without the file records it too, so a config.yaml written later (by an older
orch, say) is never imported over settings confirmed in the app. A file that does not parse is not
imported, and orchd tries again at the next start.

The file is left in place and never read again. `GetConfig` reports it in `legacy_file` while it
exists, and `RemoveLegacyConfig` removes it: always that one fixed path, and only once the import
ran. It first copies the file (owner-only) to `<data>/backups/config.yaml.<UTC time>`, since its
comments and any keys the import could not keep live only there; a symlink is removed as a link,
its target untouched. Orch.app offers this as a quiet row under Settings → Overview.

### When a setting stops orchd

Settings can only be changed through orchd, so a setting that stops orchd's services from starting
(a model folder it cannot create, a port in use, a storage path) must not take orchd down with it.
If anything after the config is read fails (the Qdrant key, the embedding model, the workflow
store, adding or starting subsystems), orchd stops what started and serves only `Ping`,
`GetConfig`, `SetConfig`, `RemoveLegacyConfig` and `Restart`, with the failure in `GetConfig`'s
`startup_error`; every other RPC is unimplemented. Orch.app shows the error at the top of Settings.
Once the setting is fixed, restarting orchd (the app's restart, or `Restart`, which exits for the
service manager to start orchd again) tries the full start. orchd also exits to try again after
10 minutes, so a failure that clears by itself (the network down during the first model download)
does not leave its services down until someone notices. When orchd cannot run at all,
`orch config reset` reaches the store directly.

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

## Schema

`config.Fields` (`internal/config/schema.go`) describes every key: its section, label, help, type, range or
options, and two flags. It is what `GetConfig` serves and the app's Settings pane is built from. A test fails
when a default has no field.

- **Sensitive:** a key that changes what agents may run or where orch keeps its data. `SetConfig` refuses it
  without `confirm_sensitive`, and the app asks first. orch's own maintenance writes and `orch config reset`
  do not ask: the operator ran the command.
- **Live:** a key each short-lived process reads for itself (`hooks.auto_rag.*`, read by orch-hook on every
  prompt). Every other key takes effect when orchd restarts: `Restart` restarts subsystems in place, without
  reading config again.

`GetConfig` compares each key against a snapshot from orchd's start, so it reports what is waiting for a
restart however the value was changed, and a value set back stops waiting. Each field also says whether it is
`overridden` (set away from its default).

## Queue limits

`max_concurrent` and `max_active` are held to `config.MaxParallelTasks` and `config.MaxActiveTasks` (both 50)
on every route: `ApplyStore` refuses more, and `Unmarshal` clamps an imported or hand-edited value and logs it, raising `max_active` to at least `max_concurrent`. `max_active: 0` used to mean no cap and now means
50. The worker runs only when `queue.worker.enabled` is true and `max_concurrent` is above 0
(`QueueWorkerSettings.Runs`); `max_concurrent: 0` used to mean 1 and now means off. Setting it above 0 through
`ApplyStore` also turns `enabled` on and raises `max_active` if needed, so the app offers the one number.
