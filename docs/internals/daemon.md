# Daemon, RPC and state

`orchd` is the only long-lived process. Everything stateful lives in it; `orch`, `orch-hook` and
the MCP servers are clients that dial it per call.

## Process model

`cmd/orchd` cancels a context on SIGINT/SIGTERM and runs `daemon.Run`, which starts, in order:

1. Create the config and data directories (0755). Append Homebrew and `~/.local/bin` to `PATH`,
   because launchd's minimal `PATH` hides `gh` and `claude`; an explicit `PATH` keeps precedence.
2. Open `orch.db` (SQLite, pure Go, WAL, `busy_timeout=5000`, foreign keys on) and apply the
   embedded migrations.
3. Import an older release's `config.yaml` once ([config.md](config.md#the-old-configyaml)), then
   load the settings from `orch.db`.
4. Resolve the Qdrant API key: the env var named by `rag.qdrant.api_key_env` if set, else
   `<data>/qdrant-api-key`, else generate 32 random bytes, hex-encode, write the file 0600.
5. Download the embedding model if missing.
6. Build the embedder (lazy-loaded, 384 dims) and the RAG service over the Qdrant store.
7. Open the durable workflow backend (`<data>/workflows.db`) and wire the task ledger, completion
   notifier and dispatcher.
8. Register the supervised subsystems below and start them, failing fast. The exception is a
   unit that fails *degraded* (today only `qdrant` with a rejected API key): it is left `failed`
   and the rest start, so orchd stays up.
9. Listen on the Unix socket (remove a stale one first, then `chmod 0600`). The socket is the
   trust boundary: anything running as the user can connect. orchd listens on nothing else; the
   only TCP listener it starts is the Qdrant engine, on 127.0.0.1.
10. Serve gRPC until the context ends.

If any of steps 4 to 8 fails, orchd stops what it started and serves only the settings (`Ping`,
`GetConfig`, `SetConfig`, `RemoveLegacyConfig`, `Restart`) on the same socket, with the failure
in `GetConfig`'s `startup_error`, so the setting at fault can be fixed in Orch.app
([config.md](config.md#when-a-setting-stops-orchd)). `Restart` there exits for the service manager
to start orchd again, as it does by itself after 10 minutes.

Before step 2, `log` output is also teed into an in-memory ring of the last 5000 lines
(`internal/logs`), which `TailLogs` serves. Each line is tagged with a source from its prefix:
a subsystem name (`pr-babysitter: ...`), `qdrant` for the engine's stdout and stderr, `database`
for migration output, `rag` for the embedder, and `orchd` otherwise. Control and ANSI
characters are stripped and lines are capped at 2000 characters.

Shutdown runs in reverse: graceful gRPC stop, remove the socket, stop subsystems in reverse order,
close the workflow backend, the embedder, then the database. Self-update exits through the same
path by cancelling the run context.

### Queued callers

Queued task sessions run as the user too, so orchd attributes each connection to the process that
opened it (`internal/daemon/callers.go`) rather than trusting clients to check `ORCH_TASK_ID`:

- **Peer:** at connect time the server's transport credentials read the peer PID
  (`LOCAL_PEERPID` on macOS, `SO_PEERCRED` on Linux) and walk its parents (`procstat.Parent`, at
  most 256 steps). A process descended from orchd is one orchd started: a task session, its MCP
  servers, hooks and shell commands, or a verification run. orchd itself is not its own
  descendant. A walk that fails is retried once; a peer that still cannot be placed, or a call
  without orchd's peer information, is treated as a task. On platforms without peer lookup, and
  for a Linux peer in a PID namespace orchd cannot see (PID 0), only the next signal applies.
- **Identity (for spaces):** `internal/procstat` also reads a process's start time
  (`StartTime`: sysctl `kern.proc.pid` on macOS, to the microsecond; on Linux the start ticks in
  `/proc/<pid>/stat` added to the boot time, which is read once per process so a clock step
  cannot shift it within that process) and working directory (`Cwd`: libproc `proc_pidinfo`
  on macOS; `/proc/<pid>/cwd` on Linux), never through a command. PID plus start time
  (`procstat.Identity`, compared with `Same`) names one process for its life, so a reused PID
  never matches an earlier record. `procstat.NearestAncestor` walks from a PID up through its
  parents (at most 256 steps) and returns the first one a registry callback accepts. Each step
  reads start time and parent in one kernel read, and a parent that started after its child
  (its PID was reused mid-walk) ends the walk with an error. The starting PID is taken on trust:
  a caller with a socket peer PID must confirm that process is still the one that connected.
  Other platforms return `ErrUnsupported`, which the walk reports as not found with no error:
  such a caller has no identity and so gets no space. On an M3 Max one lookup costs about 15 µs
  (start time) or 1.4 µs (cwd), and a full walk to the root about 0.12 ms.
- **Harness:** at connect time orchd also walks the peer's ancestry and records the nearest
  agent harness, its PID and its start time (read at accept, while the caller still runs under
  it; an unreadable start time is a harness error), then walks on and records every harness
  above that one. `procstat.ReadProgram` reads each process's kernel name and, for the user's own
  processes, its executable and arguments: sysctl `kern.proc.pid` and `kern.procargs2` on macOS,
  `/proc/<pid>/comm`, `exe` and `cmdline` on Linux. A process is a harness when its name, its
  executable or one of its first two arguments is `claude` or `codex`, or is under
  `@anthropic-ai/claude-code`, `@openai/codex` or `claude/versions/`. The kernel name alone is
  not enough: npm installs run as `node`, and Claude Code's native installer names its binary
  after the version. The walk is not retried: a session exiting mid-walk would leave a second
  walk with no harness to find. `DiffSpaces` and `ApplySpaces` use it (`refuseNonOperator`).
  They are refused to queued tasks, to any caller inside an agent session, and to a caller whose
  ancestry orchd cannot read, so a session can neither read nor apply the spaces policy it runs
  under. The limits below apply here too: a process that leaves the session's tree is not caught,
  and a harness installed some other way and renamed is not recognised.
- **Report:** `orchdclient` sends the `orch-task-id` metadata when `ORCH_TASK_ID` is set, which
  names the task in the refusal and covers callers outside orchd's process tree.

Unary and stream interceptors allow a queued caller only the RPCs that read state: `Ping`,
`Search`, `ListSources`, `ListTasks`, `GetTask`, `Status`, `RecommendModel`, `ListPRs`, the
GitHub reads (`GetGitHubIdentity`, `ListGitHubScopes`, `ListGitHubRepos`, `ListGitHubItems`),
`TailLogs`, `GetSystemMetrics`, `ListDocs`, `GetDocument`, `GetDocsSync` and
`ListRegisteredSources`. Every other RPC (`Dispatch`, `RateTask`, `RetryTask`, `CancelTask`, `Ingest`, `DeleteSource`,
`SummarizeSession`, the migration calls (`MigrateHybrid`, `DropBackup`, `ReindexKeywords`), the source, docs-sync, GitHub-scope, `RequestGitHubSweep` and `Restart` calls, and any
RPC added later) is refused with `PermissionDenied`, and the refusal is logged. A test fails
when an RPC is in neither list. A queued session's own transcript is still
stored: its session-end hook's `Ingest` and `SummarizeSession` are refused, so the hand-off unit
stores the record and queues the summary from the entry the hook left.

**Limits.** This raises the bar from "unset a variable" to "leave orchd's process tree"; it is
advisory, not a sandbox, because a task runs as the user. `orch config reset` and the `orch docs` commands that
change what is synced refuse inside a task and are denied to its Claude Code sessions, though a task could still
write the settings in `orch.db` itself (as it could edit `config.yaml` before), and can read them. A task can leave the tree in one
command: `launchctl submit`, `open` (a `.command` file or an app), `osascript`, `at` or cron, or
`systemd-run --user` all start a process launchd or systemd owns, and a process whose parent
exits before orchd accepts its connection is reparented the same way. A task can also write the
files orchd reads directly: hand-off entries, `workflows.db` (a retry or reconcile replays a task's input from its history), `orch.db` (the CLI writes routing and the model
registry there, not over the socket) and the Qdrant API key file. The harness sandbox (Codex's,
or Claude Code's permission rules) is the boundary for those. A PID can also be reused between
connect and accept; audit tokens (`LOCAL_PEERTOKEN`) or pidfds would close that.

### Session pins

Each interactive session is pinned once, at SessionStart (SP-010, INV-05). The pin is a row in
`session_pins`: session ID, harness (`claude`/`codex`), the harness process's PID and start
time, the first directory, its main checkout (from the git layout, not a command), the space a
session started there writes to, the applied spaces revision, `pinned_at`, `ended_at` and
`inherited_from`. Nothing but `ended_at` changes after the insert, and a pin stays after its
session ends so the command log's retention can count from `ended_at` (SP-015).

`PinSession` takes only the session ID, the hook's `cwd` and `parent_session_id`. The harness
and its PID come from the connection's process tree (above), so a caller cannot name another
process; a caller outside a harness, one orchd could not walk, or one whose harness has exited
since it connected (its PID now has another start time) is refused (`PermissionDenied`).
orchd decides, in order, holding one lock so two hooks cannot both create a pin:

1. The session already has a pin: it is returned unchanged. Only the harness process it names
   gets it; any other caller is refused, which also covers a session resumed in a new process
   until SP-017 rebinds pins on resume.
2. `parent_session_id` is set (a subagent): it gets a copy of the parent's pin, and its own
   `cwd` is ignored. The parent must be pinned (`FailedPrecondition`) and pinned to the
   caller's harness process (`PermissionDenied`).
3. The harness process already holds a pin, ended or not: the new session copies its earliest
   one. Claude Code's `/clear` starts a new session ID in the same process, so it keeps the
   process's space.
4. The caller's harness runs inside another harness (an agent started `claude` in another
   directory, or ran any program named `claude`): it copies the nearest outer harness's pin,
   ignoring its own `cwd`, and gets none (`FailedPrecondition`) when no outer harness is pinned.
   Only a harness with no harness above it takes a new pin.
5. Otherwise a new pin, refused (`FailedPrecondition`) to a harness whose parent is launchd or
   init: one command can detach a process named `claude` from the session (a backgrounded
   subshell that exits), leaving it with no harness above it. A new pin is also only taken within
   5 minutes of the harness process starting
   (`FailedPrecondition` after), since SessionStart fires as it starts and an agent might later
   move the harness's own working directory. `cwd` must be absolute and, with symlinks resolved, equal the harness
   process's own working directory as the kernel reports it (`FailedPrecondition` otherwise),
   so a process inside a harness that started without a pin (hooks not yet installed, orchd
   down at SessionStart) cannot choose its space. The space is the `SystemSession` placement of
   that directory under the applied policy: `""` with no policy (INV-10), `quarantine` when the
   applied revision does not verify. Placement runs to its own 10 s deadline, so a hook that
   gives up waiting does not cost the session its pin.

`EndSession` sets `ended_at` once (a repeat returns the first time); only the pinned harness
process may call it, and an unknown session is `NotFound`. Session IDs must match
`^[A-Za-z0-9_-]{1,128}$` (`InvalidArgument`). Both RPCs are refused to queued tasks, whose
identity comes from their task record (SP-011).

Refusals name no process to the caller; orchd logs why.

**Limits.** Harnesses are recognised by name, executable or first arguments, all of which a
process can choose (`exec -a claude`). The orphan check looks only at the harness's own parent,
so a single command in a session can still take a new pin in another directory: a backgrounded
subshell that exits leaves a shell that stays alive under launchd, and that shell starts a
process named `claude` there (`( cd /other && sh -c 'exec -a claude …; true' & )`). Opening a
`.command` file in Terminal, `osascript` or `launchctl submit` do the same, and on Linux so does
an orphan adopted by a subreaper rather than init. Ancestry alone cannot tell these from a real
session (tmux's server also runs under launchd). Pins grant nothing yet; nothing that reads
memory by a pin's space (SP-011, SP-025) may rely on them until the pre-tool guard (SP-041)
stops a session from starting such a process. Any process in a session can
end that session's pin early; retention (SP-015) still keeps a live process's log. Codex is not pinned yet: orch's Codex install registers no SessionStart or
SessionEnd hook. A session resumed in a new process keeps its pin but cannot end it, and two
sessions in one process can hold pins with different spaces only when one was pinned in
another process first (an in-process resume); SP-011 (below) and SP-017 resolve callers and
liveness from these rows.

### Caller sessions

Every RPC learns whom it serves before its handler runs (SP-011, INV-02, INV-05;
`internal/daemon/caller_session.go`). A unary and a stream interceptor, after the queued-caller
guard, attach one of four results to the call's context:

- **task:** a process orchd started, or a client that reports `orch-task-id`. Its record (the
  live run: task ID, directory, session) comes only from the process tree: orchd's own child on
  the caller's path, with the PID and start time read at accept, must be a running task's agent
  CLI, whose start time the runner reads when the CLI starts. Matching the start time matters
  because a process that escaped a finished task can keep its connection until a later task's
  CLI reuses the PID; an unreadable start time matches no run. A reported task ID only marks the
  caller as a task, which narrows it; it never selects a record.
  A spawned process that is no task's agent (a verification run) is a task with no record.
- **operator:** identified, not started by orchd, and with no agent harness above it.
- **session:** the nearest harness at or above the caller, among the ones recorded at accept
  (the caller's harness, then each harness above it), that holds a pin, matched by PID and the
  start time read at accept. The pin is that process's current one: its latest pin not ended,
  else its latest. All pins of one process share a space (the process took its first pin and
  copied it for every later session, subagents and `/clear` included), so the resolved session
  ID means "a session of this harness process", not necessarily the caller's own; nothing may key
  per-session data on it without telling those apart. A harness an agent started without a pin
  resolves to the pinned session above it.
- **none:** everything else: no peer information, an ancestry orchd could not read, a platform
  without peer lookup or start times, a harness no session has pinned (also when its PID was
  reused by a later process), or a pin store that cannot be read (logged). Once spaces are
  enforced such a caller reads only public content.

Nothing in a request takes part, so an agent that runs `cd elsewhere && orch search` (or sends
another `cwd`) resolves to its own session. Each connection caches its task once found, and a pin
once found on its nearest harness. A pin found on an outer harness, and a miss, are looked up
again on the next call, since an MCP server can connect before its session's SessionStart hook
pins it, and a nested harness may pin itself after the connection opened. Lookups run outside
the connection's lock. Resolving a new connection, including
orchd's walk of the real process table at accept, measured p50 0.57 ms and p99 0.79 ms on an M3
Max (`TestResolveCost`, 1000 connections, 300 other pins in the store); the pin lookup alone is
p99 about 0.05 ms, so the walk dominates, and a cached call costs about 0.1 µs. A shared Linux CI
runner testing other packages in parallel measured p99 6.5 ms, so the test holds the 5 ms bound
only on macOS. The recovery server resolves nothing, so every call there is `none`.

Nothing grants access by the result yet. Memory reads by a session's space (SP-025) wait until
the pre-tool guard (SP-041) closes two gaps: the detached-harness case described under
[session pins](#session-pins), and a process that leaves the session's tree before orchd accepts
its connection (a double fork, `launchctl submit`, `open`, `osascript`; see
[queued callers](#queued-callers)), which has no harness above it and so resolves to the
operator, the widest result. The connection's peer PID is still taken on trust at accept
(see [queued callers](#queued-callers)): a peer that hands its socket to a child and exits leaves
its PID free for reuse.

Clients dial `unix://<socket>` lazily (failure surfaces on the first call) and set their own
deadlines: hooks 0.5 s for writes and 3 s for prompt search; `orch status` 2–3 s; long CLI
commands up to 2 hours (migration).

## gRPC API

Service `orchd.v1.Orchd` (`internal/rpc/orchdv1/orchd.proto`; regenerate with `make proto`).
Times are Unix seconds with 0 meaning unknown or open.

| RPC | Request → response | Server rules |
|---|---|---|
| `Ping` | → version, uptime | — |
| `Ingest` | text, source_uri, source_type, scope, tags, started_at, ended_at, timeline → chunks | type defaults to `manual`; always written as `client`, whatever type or URI is declared; refused (`FailedPrecondition`) for a source the docs or sources sync stored, matched on the exact URI, so it is advisory (see [memory.md](memory.md#provenance)) |
| `Search` | query, top_k, scope, source_type, tags (any), mode, since, until, per_source, ranking → hits (with heading and the writer-assigned trust), fallback | top_k 0–100; mode `hybrid`/`dense`/empty; ranking `weighted`/`plain`/empty; per_source at most 100 (0 = `rag.search.per_source`, negative = no cap); since ≤ until, neither negative; explicit hybrid that cannot run → `FailedPrecondition` |
| `ListSources` | source_type, scope, uri_prefix, limit, since, until → sources, totals | same range rules; limit 0 = all |
| `DeleteSource` | source_uri, confirm → deleted | `confirm` required server-side (`FailedPrecondition`), so no client can skip it; logged before and after |
| `Dispatch` | agent, body, cwd, task_kind, model → id, worker_enabled, route | agent default `dev`; body ≤ 64 KiB; cwd absolute; routing below; queue full → `ResourceExhausted` |
| `ListTasks` / `GetTask` | filters / id → tasks / task | limit default 50, max 500; missing task → `found=false` |
| `Status` | → version, uptime, subsystems, migration progress | — |
| `Restart` | name (empty = all) → restarted | restarts subsystems in place, never the process; unknown → `NotFound` |
| `PinSession` | session_id, cwd, parent_session_id → the pin | see [session pins](#session-pins); refused to queued tasks |
| `EndSession` | session_id → ended_at | sets `ended_at` once; only the pinned harness process; refused to queued tasks |
| `RecordToolEvent` | event (`pre-tool-use`/`cwd-changed`), session_id, cwd, tool_name, tool_input (≤ 1 MiB), tool_input_size, tool_use_id, agent_id, agent_type, old_cwd, new_cwd, at → — | takes a [tool hook](hooks.md#tool-hooks) event and answers at once; attributed to the [caller orchd resolved](#caller-sessions), never to session_id; open to queued tasks. Until the command log store (spaces plan SP-014) exists, events are counted by caller kind and dropped: nothing is stored, logged or indexed |
| `SummarizeSession` | session_id, transcript_path, tags → accepted | ID `^[A-Za-z0-9_-]{1,128}$`; path absolute, `.jsonl`, regular file; enqueue only (backlog 32) |
| `RecommendModel` | task_kind → route, from_table | see [tasks.md](tasks.md) |
| `RateTask` | id, rating (`good`/`rework`/`bad`) → — | sets the task's rating; with the PR-outcome sweep, the only ledger writes outside the queue |
| `RetryTask` | id → run_id | runs the task again from its recorded input, resuming its session; the newest run still active → `FailedPrecondition`, unknown → `NotFound` |
| `WatchTasks` | status filter, limit → stream of `TaskEvent` | operator-only (refused to queued tasks); snapshot, then changed tasks, then a 15 s tick (docs/internals/tasks.md) |
| `CancelTask` | id → outcome | stops a running session, cancels a queued run, or marks a stranded task `cancelled`; already ended → `FailedPrecondition` |
| `MigrateHybrid` | target, apply → report, error | one at a time (`Aborted` if busy); see [memory.md](memory.md) |
| `DropBackup` | apply → collection, points, dropped | collection name only from config, never the caller; shares the migration lock |
| `ReindexKeywords` | apply → collection, tokenizer_version, total, stale, updated, applied, error | shares the migration lock (`Aborted` if busy); a refused plan → `FailedPrecondition`; an applied run that stops reports what it did in `error`; see [memory.md](memory.md#keyword-re-index) |
| `GetGitHubIdentity` | refresh → login, orgs, teams | through `gh`; cached 10 minutes unless `refresh`; GitHub unreachable → `Unavailable` |
| `ListGitHubScopes` | refresh → scopes, warning | creates a scope for each newly discovered account (the user's own enabled, orgs disabled) and never overwrites one; GitHub unreachable → the stored scopes plus a `warning` |
| `SetGitHubScope` | scope → scope | only a discovered scope (`NotFound` otherwise); its kind cannot change; enabled, items (`involving`/`all`), repo mode (`all`/`include`/`exclude`) and repo names, including `everything_repos`, are validated (`InvalidArgument`) |
| `ListGitHubRepos` | owner → repos (name, private, archived) | the user's own account through `/user/repos?affiliation=owner`, an org through `/orgs/<org>/repos`; login validated |
| `ListGitHubItems` | kind, owner → items (PRs and issues with kind, number, author, reasons, attention), updated_at | the watcher's cache only, never `gh`; kind `pr`/`issue` or empty (anything else → `InvalidArgument`); owner matched case-insensitively |
| `RequestGitHubSweep` | → nothing | asks the watcher to sweep soon, at most once a minute with other requests; returns before the sweep, whose result shows as `ListGitHubItems`' `updated_at` advancing; refused to queued tasks |
| `TailLogs` | after_seq, source, limit → entries (seq, time, source, message), last_seq, oldest_seq, sources | in-memory lines only; limit default 500, max 5000, and at most 3 MiB, newest kept when more match; `oldest_seq` past the caller's `after_seq` means lines rolled out |
| `GetSystemMetrics` | → processes (orchd, qdrant: pid, RSS, CPU %, uptime), goroutines, heap, data sizes, task counts by status | read natively through `internal/procstat`, never a command; CPU since the previous call; data sizes cached 30 s; a failed task count leaves `tasks` empty |
| `Search` filters | source_uri, all_tags | `source_uri` keeps one source; `all_tags` requires every tag (e.g. `owner:o`, `repo:r` for one repo's docs), alongside `tags` (any); each tag list at most 32 tags of 256 bytes |
| `ListDocs` | → owners (repos, indexed commit), files (URI, path, chunks, GitHub URL), truncated | files are docs both the store holds and the registry recorded; GitHub URLs pinned to the indexed commit; owner and repo names validated before a URL is built; at most 10,000 files or 3 MiB |
| `SyncDocs` | owner (empty = all), force, confirm_prune (needs owner: grants each of its repos with a current refused prune, for one prune no larger within the hour, and forgets their HEAD so this sync walks them; `FailedPrecondition` when none is refused) → sync state | refused with `Aborted` while a sync runs, which is never interrupted (nothing is queued); owner validated and must be registered (`NotFound`); with the scheduler's run lock held, force clears the owner's caches and the owner is marked due; the `docs-scheduler` tick (every 2 s) then scans |
| `ListDocsOwners` | → owners (enabled, repo mode, selected repos, each repo's files and status) | operator-only |
| `ListDocsOwnerRepos` | owner → repos (name, archived, fork) | asks GitHub; the owner need not be registered; operator-only |
| `SaveDocsOwner` | owner, enabled, repo_mode, repos, refresh_secs → owner | registers the owner if new; `Aborted` while a docs sync runs |
| `RemoveDocsOwner` | owner, purge, confirm → purged counts | purge needs `confirm`; a failed purge leaves the owner; `Aborted` while a docs sync runs |
| `PurgeDocs` | owner, repos (empty = every repo not covered), confirm → purged counts | needs `confirm`; a repo still synced → `FailedPrecondition`; `Aborted` while a docs sync runs |
| `GetConfig` | → fields (each with `overridden`), restart_required, legacy_file, startup_error | every setting, from `orch.db` ([config.md](config.md#schema)); operator-only |
| `SetConfig` | values (key → JSON), confirm_sensitive → same as `GetConfig` | all or none; a default value deletes the key's row; invalid → `InvalidArgument`; unconfirmed sensitive → `FailedPrecondition`; logs key names, never values; operator-only |
| `RemoveLegacyConfig` | → same as `GetConfig` | moves `<config>/config.yaml` to `<data>/backups/`, never a path from the request; before the import ran → `FailedPrecondition`; operator-only |
| `GetDocsSync` | → running, owner, started/finished, done of total, last repo | the docs scheduler's progress |
| `GetDocument` | source_uri → text, chunks, malformed, oversize, web_url, writer, content_hash, trust | rebuilt from chunks (see [memory.md](memory.md)); over 3 MiB → `oversize`, no text; nothing indexed → `NotFound`; URI ≤ 4096 bytes |
| `ListRegisteredSources` | → sources (with how to search within each) | the `sources` registry |
| `SaveSource` | source, replace_id → source | `sources.Validate` (`InvalidArgument`); upsert by kind and URI; `replace_id` (an edit that changed kind or URI) is removed after the new entry is stored, and must exist (`NotFound`) |
| `RemoveSource` | id, purge, confirm → purged counts | unknown → `NotFound`; `purge` needs `confirm` (`FailedPrecondition`); content is purged before the source is unregistered, and docs the docs registry owns are kept |
| `RefreshSource` | id (empty = all), confirm_prune (needs id: grants each of its prefixes with a current refused prune, for one prune no larger within the hour; `FailedPrecondition` when none is refused) → marked | marks due, then restarts `sources-scheduler`, which refreshes on start; at most one restart per 10 s, so repeated calls cannot keep cutting a refresh short |
| `GetSafetyNet` | → state (`missing`/`legacy`/`outdated`/`disabled`/`ok`), ok, pinned version, commit and release date, installed version and commit, competing copies, rules (rule.json state, every listed rulebook and its rules, problems), unreadable files, action | read from Claude Code's plugin files and the cc-safety-net rules on every call; runs no command |
| `InstallSafetyNet` | rules_only → the same state | one action at a time: a request while one runs is refused with `Aborted`, never queued. Writes the marketplace manifest, the `orch-guards` rulebook and the rule.json merge (backed up to `<data>/backups/`), then runs the planned `claude plugin` commands (see [install.md](install.md#safety-net)) with the configured `queue.worker.claude_bin`, or the one found on `PATH`. The action is detached from the request with its own 10-minute deadline, so a client that gives up mid-migration cannot leave Claude Code without the plugin; `GetSafetyNet` reports how it ended. Unlike `orch install`, it re-enables a disabled plugin. `rules_only` writes only the rule files. A failed action is returned as the state with the action's error and log, not as an RPC error. The log keeps the last 4 KiB of each command's output and 16 KiB in all, with URL credentials and secret-shaped strings masked. Logged before and after |
| `ListSettings` | provider, kind (empty = all) → providers (kinds and add-form fields, files, error, last look), items (state, origin, status, group, summary, payload), revision | syncs the files first; task-callable. See [settings.md](settings.md) |
| `SetSettingEnabled` | provider, kind, identity, enabled → item | writes the file at once (backed up before the first change, atomic, refused with `Aborted` if the file changed underneath); unknown → `NotFound`; a plugin or rule gone from its file → `FailedPrecondition`; operator-only |
| `AddSetting` | provider, kind, fields → item | hooks and permission rules only; validated (`InvalidArgument`); already listed → `AlreadyExists`; operator-only |
| `RemoveSetting` | provider, kind, identity → — | items added by you or found; orch's → `FailedPrecondition`; operator-only |
| `ListPRReviews` | → reviews (mode, state, heads, verdict, reason, comment, instructions, for_user, report, posted, note, times, last and next check) | `orch.db` only, never `gh`; operator-only |
| `WatchPRReview` | url, mode, set_comment, comment, set_instructions, instructions → review | URL exactly `https://github.com/<owner>/<repo>/pull/<n>`, mode `verdict`/`approve`/`feedback`/`auto`/`comment`, instructions ≤ 4000 bytes (`InvalidArgument`); `review_anyway` lets one review of an untrusted author's PR run; restarts a stopped, closed or failed review; a post in flight for the PR → `Aborted`; operator-only ([tasks.md](tasks.md#pr-review-watch)) |
| `StopPRReview` | url, forget → — | unwatched → `NotFound`; a post in flight → `Aborted`; `forget` deletes the row; operator-only |
| `GetTaskActivity` | id → available, steps (time, tool, title, commands, risk, failed, output, duration, lines), total, counts of tools, commands, files and failures | from the task's transcript; none → not available; operator-only ([tasks.md](tasks.md#session-activity)) |
| `GetPRLanding` | task_id → state, problem, path, branch, base, commit, commits, files, diff, can_land, blocked, can_undo, revert, decided_at | the proposal checked against the PR's head; unknown → `NotFound`; GitHub failure → `Unavailable`; operator-only ([tasks.md](tasks.md#pr-landings)) |
| `ListPRLandings` | → each decision's task, URL and state | local only, for polling; operator-only |
| `LandPR` | task_id, sha → the landing | pushes that commit, fast-forward only; anything no longer true → `FailedPrecondition`; a failed push → `Unavailable`; operator-only |
| `UndoPRLanding` | task_id → the landing | pushes a commit restoring the earlier tree, while the landed commit is the head; else `FailedPrecondition`; operator-only |
| `DiscardPRLanding` | task_id, restore → the landing | records or forgets the decision; operator-only |
| `GetPRDetail` | url, refresh → title, body (≤ 8000 bytes), author, branches, head, sizes, CI, checks, labels, reviewers, reviews, files (≤ 100), fetched_at | one `gh pr view`, cached a minute per PR; GitHub failure → `Unavailable`; operator-only |
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
| `workflow-worker` | Run + Stop | — | `queue.worker.enabled` and `max_concurrent` above 0 | durable task worker, `max_concurrent` parallel |
| `sources-scheduler` | Tick | `sources.scan_interval` (5m) | always | refresh due web/repo/owner sources |
| `docs-scheduler` | Tick | `docs.scan_interval` (1m) | always | sync GitHub owners' repo markdown |
| `pr-reviews` | Tick | 15 s; each watched PR read from GitHub every 2 minutes or when changed | always (needs `orch.db`) | review watched PRs once ready and post what the user opted in to ([tasks.md](tasks.md#pr-review-watch)) |
| `pr-babysitter` | Tick | 15 s check; sweep every `poll_interval` (10m) or on request | `pr_babysitter.enabled` | refresh the cache of PRs and issues in enabled GitHub scopes, label task outcomes |
| `routing-evaluator` | Tick | hourly check; run every `routing.evaluate.interval` (168h) | `routing.evaluate.enabled` | discover models, propose routes |
| `handoff` | Run | 10 s; entries settle 5 s | always | store work hooks handed off |
| `session-summarizer` | Run | queue-driven | `sessions.summarize.enabled` | model-written session summaries |
| `settings-watcher` | Tick | 2 s | the user's home resolves | import edits made outside orch to `~/.claude/settings.json` and cc-safety-net's rule files ([settings.md](settings.md)); never writes |
| `self-update` | Tick | 1m | orchd can locate its own binary | exit when a new build is installed (only logs when not supervised) |

**Self-update** never downloads. It watches the binary orchd was launched from (resolved path,
size, mtime). When it changes and orchd's parent is launchd (ppid 1), orchd exits and launchd's
KeepAlive starts the new build; unsupervised, it only logs. A briefly missing binary (an upgrade
unlinks before relinking) is not a change. Durable tasks resume after the restart.

**qdrant** reuses an engine that already answers `/readyz` on `http_port` (such as an orphan from
a hard kill) instead of spawning a second one, but only after one keyed request (`GET
/collections` with orchd's API key) succeeds. `/readyz` needs no key, so a different Qdrant on the
same port would otherwise pass and turn every request into a 401. The same check runs after
spawning, in case another engine took the port first. The check catches a misconfiguration, not an
impostor: a keyless engine still passes.

- **Key rejected (401/403):** the unit is marked `failed` with an error naming the port, and orchd
  keeps running; only memory is unavailable. Exiting instead would take the socket, hooks and task
  queue down and crash-loop under launchd's `KeepAlive`. Another engine may hold the port, or an
  orphan was started with an older key. To recover, either:
  - stop that engine, then `orch restart qdrant`; or
  - move orchd with `rag.qdrant.http_port` and `grpc_port`, then a full `orch restart`. orchd
    reads the ports only at startup, so `orch restart qdrant` would retry the old port.
- **Any other failure** (5xx, timeout): retried up to 3 times, 1 s apart, then logged, and the
  engine is used anyway, since it already answered `/readyz`.

`orch status` shows the failed unit under subsystems. Its vector-store line runs the same check
with the key it resolves itself (the env override, else `<data>/qdrant-api-key`; it never
generates one) and reports `rejects the resolved API key`. With no key it skips the check. Status
can't be certain it resolved the key orchd uses, for example when the env differs from orchd's
launchd environment.

## On-disk state

Config dir: `$ORCH_CONFIG_DIR`, else `$XDG_CONFIG_HOME/orch`, else `~/.config/orch`.
Data dir: `$ORCH_DATA_DIR`, else `$XDG_DATA_HOME/orch`, else `~/.local/share/orch`.
Socket: `$ORCH_SOCKET`, else `<data>/orchd.sock` (macOS limits socket paths to 104 bytes).

| Path | Contents |
|---|---|
| `<config>/config.yaml` | settings of older releases: imported into `orch.db` once, then never read ([config.md](config.md#the-old-configyaml)) |
| `<config>/AGENTS.md`, `policy/`, `rag-eval.yaml` | shared agent policy; retrieval golden set |
| `<data>/landings/<task>.git` | a proposal's commit, in a bare repository with no hooks; pruned after 30 days |
| `<data>/orch.db` | SQLite: tasks, sources, docs, models, routing, managed settings |
| `<data>/workflows.db` | durable workflow state |
| `<data>/qdrant/`, `bin/qdrant`, `qdrant-api-key` (0600) | vector store, engine binary, API key |
| `<data>/models/minilm/` | embedding model |
| `<data>/sessions/` | session records and checkpoints (YAML), per-session hook state |
| `<data>/handoff/` (0700) | work hooks left for orchd; `failed/` after 8 attempts; `pr-sweep` request |
| `<data>/events/` | JSONL event streams (`queue`, `routing`, `pr-babysitter`, per session) |
| `<data>/pr_babysitter_state.json`, `routing_evaluator_state.json` | subsystem state |
| `<data>/marketplaces/orch-pinned/` | the orch-owned Claude Code marketplace pinning cc-safety-net |
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
| `config_values`, `config_import` | orch's settings that differ from a default (key, JSON value, updated_at, updated_by); whether the old config.yaml was imported ([config.md](config.md)) |
| `session_pins` | session_id (key), harness, harness_pid, harness_started (unix ns), first_cwd, main_checkout, space, policy_revision, pinned_at, ended_at, inherited_from ([session pins](#session-pins)) |
| `settings_items` | provider, kind, identity (key), enabled, origin (`orch`/`you`/`found`), payload (JSON), present, removed_at, created_at, updated_at ([settings.md](settings.md)) |

## Invariants

- Clients stay thin; every long-lived component is a named supervised unit, not a bare goroutine.
- The socket (0600) is the trust boundary; destructive operations gate server-side, and
  operator-only RPCs refuse queued callers server-side.
- Bad requests fail at the RPC; nothing invalid becomes durable.
- Task-completion events carry no result text (results may be steered by an untrusted repo).
- Credentials orchd holds (the Qdrant key) are removed from child agent environments.
- Counts saturate rather than wrap.
