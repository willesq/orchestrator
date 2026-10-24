# Daemon, RPC and state

`orchd` is the only long-lived process. Everything stateful lives in it; `orch`, `orch-hook` and
the MCP servers are clients that dial it per call.

## Process model

`cmd/orchd` cancels a context on SIGINT/SIGTERM and runs `daemon.Run`, which starts, in order:

1. Create the config and data directories (0755). Append Homebrew and `~/.local/bin` to `PATH`,
   because launchd's minimal `PATH` hides `gh` and `claude`; an explicit `PATH` keeps precedence.
2. Open `orch.db` (SQLite, pure Go, WAL, `busy_timeout=5000`, foreign keys on) and apply the
   embedded migrations.
3. Load config.
4. Resolve the Qdrant API key: the env var named by `rag.qdrant.api_key_env` if set, else
   `<data>/qdrant-api-key`, else generate 32 random bytes, hex-encode, write the file 0600.
5. Download the embedding model if missing.
6. Build the embedder (lazy-loaded, 384 dims) and the RAG service over the Qdrant store.
7. Open the durable workflow backend (`<data>/workflows.db`) and wire the task ledger, completion
   notifier and dispatcher.
8. Register the supervised subsystems below and start them, failing fast.
9. Listen on the Unix socket (remove a stale one first, then `chmod 0600`). The socket is the only
   trust boundary: the gRPC server has no auth.
10. Serve gRPC until the context ends.

Shutdown runs in reverse: graceful gRPC stop, remove the socket, stop subsystems in reverse order,
close the workflow backend, the embedder, then the database. Self-update exits through the same
path by cancelling the run context.

Clients dial `unix://<socket>` lazily (failure surfaces on the first call) and set their own
deadlines: hooks 0.5 s for writes and 3 s for prompt search; `orch status` 2–3 s; long CLI
commands up to 2 hours (migration).

## gRPC API

Service `orchd.v1.Orchd` (`internal/rpc/orchdv1/orchd.proto`; regenerate with `make proto`).
Times are Unix seconds with 0 meaning unknown or open.

| RPC | Request → response | Server rules |
|---|---|---|
| `Ping` | → version, uptime | — |
| `Ingest` | text, source_uri, source_type, scope, tags, started_at, ended_at, timeline → chunks | type defaults to `manual` |
| `Search` | query, top_k, scope, source_type, tags (any), mode, since, until → hits, fallback | top_k 0–100; mode `hybrid`/`dense`/empty; since ≤ until, neither negative; explicit hybrid that cannot run → `FailedPrecondition` |
| `ListSources` | source_type, scope, uri_prefix, limit, since, until → sources, totals | same range rules; limit 0 = all |
| `DeleteSource` | source_uri, confirm → deleted | `confirm` required server-side (`FailedPrecondition`), so no client can skip it; logged before and after |
| `Dispatch` | agent, body, cwd, task_kind, model → id, worker_enabled, route | agent default `dev`; body ≤ 64 KiB; cwd absolute; routing below; queue full → `ResourceExhausted` |
| `ListTasks` / `GetTask` | filters / id → tasks / task | limit default 50, max 500; missing task → `found=false` |
| `Status` | → version, uptime, subsystems, migration progress | — |
| `Restart` | name (empty = all) → restarted | restarts subsystems in place, never the process; unknown → `NotFound` |
| `SummarizeSession` | session_id, transcript_path, tags → accepted | ID `^[A-Za-z0-9_-]{1,128}$`; path absolute, `.jsonl`, regular file; enqueue only (backlog 32) |
| `RecommendModel` | task_kind → route, from_table | see [tasks.md](tasks.md) |
| `RateTask` | id, rating (`good`/`rework`/`bad`) → — | sets the task's rating; with the PR-outcome sweep, the only ledger writes outside the queue |
| `MigrateHybrid` | target, apply → report, error | one at a time (`Aborted` if busy); see [memory.md](memory.md) |
| `DropBackup` | apply → collection, points, dropped | collection name only from config, never the caller; shares the migration lock |
| `GetGitHubIdentity` | refresh → login, orgs, teams | through `gh`; cached 10 minutes unless `refresh`; GitHub unreachable → `Unavailable` |
| `ListGitHubScopes` | refresh → scopes, warning | creates a scope for each newly discovered account (the user's own enabled, orgs disabled) and never overwrites one; GitHub unreachable → the stored scopes plus a `warning` |
| `SetGitHubScope` | scope → scope | only a discovered scope (`NotFound` otherwise); its kind cannot change; enabled, items (`involving`/`all`), repo mode (`all`/`include`/`exclude`) and repo names are validated (`InvalidArgument`) |
| `ListGitHubRepos` | owner → repos (name, private, archived) | the user's own account through `/user/repos?affiliation=owner`, an org through `/orgs/<org>/repos`; login validated |
| `ListGitHubItems` | kind, owner → items (PRs and issues with kind, number, author, reasons, attention), updated_at | the watcher's cache only, never `gh`; kind `pr`/`issue` or empty (anything else → `InvalidArgument`); owner matched case-insensitively |
| `ListPRs` | needs_me → PRs (with reasons and attention), updated_at | reads the PR babysitter's cache only, never `gh`; most urgent first, then repo and title; `updated_at` is the cache file's mtime, 0 before the first sweep; an unparseable cache → `Unavailable` |

Dispatch inserts the ledger row before creating the workflow (a row without a workflow is
diagnosable; the reverse is invisible), redacts the body, assigns `task-<uuid>`, and snapshots git
HEADs of the task's repository for later verification.

## Supervised subsystems

A unit is either `Run(ctx, beat)` (returns promptly, works in its own goroutine) or `Tick` +
`Interval` (the supervisor ticks immediately on start, then every interval). Each gets a child
context. Stop cancels it and waits up to 15 s; a unit that does not stop is marked `failed`, so
two copies never run. Restart = stop + start; every operation is idempotent. Lock order:
`lifecycle` (serializes start/stop/restart), then `mu` (state); user code never runs under `mu`.
Unit names are a public contract (`orch restart <name>`).

| Unit | Shape | Cadence | Enabled by | Work |
|---|---|---|---|---|
| `qdrant` | Run + Stop | — | `rag.qdrant.mode: binary` | run the native engine from `<data>/bin/qdrant` |
| `workflow-worker` | Run + Stop | — | `queue.worker.enabled` | durable task worker, `max_concurrent` parallel |
| `sources-scheduler` | Tick | `sources.scan_interval` (5m) | always | refresh due web/repo/owner sources |
| `docs-scheduler` | Tick | `docs.scan_interval` (1m) | always | sync GitHub owners' repo markdown |
| `pr-babysitter` | Tick | 15 s check; sweep every `poll_interval` (10m) or on request | `pr_babysitter.enabled` | refresh the cache of PRs and issues in enabled GitHub scopes, label task outcomes |
| `routing-evaluator` | Tick | hourly check; run every `routing.evaluate.interval` (168h) | `routing.evaluate.enabled` | discover models, propose routes |
| `handoff` | Run | 10 s; entries settle 5 s | always | store work hooks handed off |
| `session-summarizer` | Run | queue-driven | `sessions.summarize.enabled` | model-written session summaries |
| `self-update` | Tick | 1m | orchd can locate its own binary | exit when a new build is installed (only logs when not supervised) |

**Self-update** never downloads. It watches the binary orchd was launched from (resolved path,
size, mtime). When it changes and orchd's parent is launchd (ppid 1), orchd exits and launchd's
KeepAlive starts the new build; unsupervised, it only logs. A briefly missing binary (an upgrade
unlinks before relinking) is not a change. Durable tasks resume after the restart.

## On-disk state

Config dir: `$ORCH_CONFIG_DIR`, else `$XDG_CONFIG_HOME/orch`, else `~/.config/orch`.
Data dir: `$ORCH_DATA_DIR`, else `$XDG_DATA_HOME/orch`, else `~/.local/share/orch`.
Socket: `$ORCH_SOCKET`, else `<data>/orchd.sock` (macOS limits socket paths to 104 bytes).

| Path | Contents |
|---|---|
| `<config>/config.yaml` | settings ([config.md](config.md)) |
| `<config>/AGENTS.md`, `policy/`, `rag-eval.yaml` | shared agent policy; retrieval golden set |
| `<data>/orch.db` | SQLite: tasks, sources, docs, models, routing |
| `<data>/workflows.db` | durable workflow state |
| `<data>/qdrant/`, `bin/qdrant`, `qdrant-api-key` (0600) | vector store, engine binary, API key |
| `<data>/models/minilm/` | embedding model |
| `<data>/sessions/` | session records and checkpoints (YAML), per-session hook state |
| `<data>/handoff/` (0700) | work hooks left for orchd; `failed/` after 8 attempts; `pr-sweep` request |
| `<data>/events/` | JSONL event streams (`queue`, `routing`, `pr-babysitter`, per session) |
| `<data>/pr_babysitter_state.json`, `routing_evaluator_state.json` | subsystem state |
| `<data>/repos/`, `tasks/`, `codex/`, `migrations/`, `eval/`, `backups/` | repo clones, task scratch, Codex state, migration markers, eval runs, install backups (0600 files) |

### `orch.db` schema

| Table | Columns (abridged) |
|---|---|
| `meta` | key, value |
| `tasks` | id, agent, body, result, status, updated_at, created_at, cost_usd, duration_ms, turns, session_id, models, prs, verification, task_kind, provider, model, effort, rating, rated_at, pr_outcome, pr_checked_at |
| `sources` | id (`<kind>\|<uri>`), kind (web/repo/owner), uri, scope, tags, owner, refresh_secs (86400), last_fetched, status |
| `docs_owners`, `docs_repos`, `doc_hashes` | GitHub owners and cadence; per-repo HEAD; content hash per `repo://` URI |
| `providers`, `models` | provider state; model registry (tier, efforts, visibility, state) |
| `routing`, `routing_history` | current table (kind, rank, provider, model, effort, rationale, evidence_n, pinned); prior generations |

## Invariants

- Clients stay thin; every long-lived component is a named supervised unit, not a bare goroutine.
- The socket (0600) is the only trust boundary; destructive operations gate server-side.
- Bad requests fail at the RPC; nothing invalid becomes durable.
- Task-completion events carry no result text (results may be steered by an untrusted repo).
- Credentials orchd holds (the Qdrant key) are removed from child agent environments.
- Counts saturate rather than wrap.
