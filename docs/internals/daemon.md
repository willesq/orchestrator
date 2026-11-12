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
  most 256 steps). After the walk orchd checks the PID against the peer's kernel token (D-44,
  `internal/daemon/peer_token*.go`): on macOS the `LOCAL_PEERTOKEN` audit token's PID and PID
  version must match the process now at that PID (`proc_pidinfo` `PROC_PIDUNIQIDENTIFIERINFO`); on
  Linux the `SO_PEERPIDFD` pidfd (kernel 6.5) must still name that PID. A peer that handed its
  socket on and exited, its PID then reused by another process, or whose token cannot be read,
  is not identified: it resolves to no session and no space, and gets a task's limits. A Linux
  kernel before 6.5 has no `SO_PEERPIDFD`, so there no peer is identified and, while spaces are
  configured, every guarded call is refused for want of a pin (fails closed; orch targets macOS,
  D-49). A process descended from orchd is one orchd started: a task session, its MCP
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
`inherited_from`, plus `resumed_at`. The space, first directory, main checkout and revision
never change after the insert; a resume moves the pin to its new harness process (below), and
`ended_at` records the end. A pin stays after its session ends so the command log's retention
can count from `ended_at` (SP-015).

`PinSession` takes only the session ID, the hook's `cwd`, `parent_session_id` and the
SessionStart `source` (`startup`, `resume`, `clear`, `compact`). The harness
and its PID come from the connection's process tree (above), so a caller cannot name another
process; a caller outside a harness, one orchd could not walk, or one whose harness has exited
since it connected (its PID now has another start time) is refused (`PermissionDenied`).
orchd decides, in order, holding one lock so two hooks cannot both create a pin:

1. The session already has a pin: it is returned unchanged. Only the harness process it names
   gets it; any other caller is refused (`PermissionDenied`), unless `source` is `resume`
   (SP-017). A resume in a new harness process moves the pin there: `harness_pid` and
   `harness_started` become the new process's, `ended_at` is cleared and `resumed_at` set, and
   the space, first directory and revision stay (INV-05). It is refused while the old process
   still runs the session (not ended and alive, `PermissionDenied`), from another harness
   program (`PermissionDenied`), and to any harness that could not take a new pin of its own
   (`FailedPrecondition`): one that already holds a pin or runs inside another session, an
   orphan, or one older than 5 minutes. The new process's own working directory must then place
   into the pin's space (`FailedPrecondition` otherwise), so a resume never widens what a process
   reads. orchd cannot tell a real in-process `/resume` from an agent calling `PinSession` itself
   to take a dead session of its space, so a harness holding a pin cannot resume another
   process's session; its events stay in its own pin's chain. A resume in the pin's own process
   only clears `ended_at`.
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
memory by a pin's space (SP-011, SP-025) may rely on them until the pre-tool guard stops a
session from starting such a process. The enforcing guard (SP-041) refuses such a launch only
when its command names another job's folder; one in a folder of no space, or hidden in an opaque
command under `block-other-jobs`, still runs (RC-101). Any process in a session can
end that session's pin early; retention (SP-015) still keeps a live process's log, and a
resume in the same process clears the end. The detached fake harness above can also resume a
dead or ended session of any space whose directory it starts in, and then write that session's
command log, unless the guard refuses its launch (above). Codex is not pinned yet: orch's Codex install registers no SessionStart or
SessionEnd hook. SP-011 (below) resolves callers from these rows, and
[session liveness](#session-liveness) follows their processes.

### Session liveness

Whether a pinned session still runs comes from the kernel, never from hook events (SP-017;
`internal/daemon/liveness.go`): its harness PID must exist with the start time the pin
recorded. A PID the kernel gave to another process has another start time, so a pin never
follows it. A sweep runs when orchd starts and every 60 s (and before each `ListSessions`); it
sets `ended_at` to the time it noticed on every pin not yet ended whose process is gone or
unreadable, and writes the session's [command log](#command-log) end record as SessionEnd
would. It also sets `exited_at` (migration `0027`), the time it noticed the process gone, on those
pins and on pins SessionEnd ended earlier whose process has since exited; retention counts from
it. A resume clears it with `ended_at`. The sessions of one harness process are read once per sweep; a sweep of 50 pins takes
about 1 ms.

A session's state, computed when listed:

| State | Meaning |
|---|---|
| `active` | the harness process runs and the session's last activity was under 30 minutes ago |
| `idle` | the process runs with no activity for 30 minutes or more, however long (a session left open for a week stays idle) |
| `ended` | SessionEnd came, or the process is gone or its PID reused |
| `unpinned` | a running `claude`, `codex` or `gemini` process holding no pin not yet ended, e.g. started before the hooks were installed or whose `PinSession` failed |

Last activity is the latest of `pinned_at`, `resumed_at` and the session's last tool event in
the command log (the hook events orchd records per session); with the log off it is the pin's
own times. Unpinned processes come from the process table (`procstat.Processes`, one sysctl;
none outside macOS), matched by kernel name as `procstat.FindByName` does. A process named
`node`, `bun`, `deno` or for a version number (Claude Code's native binary) is matched by its
program, as [caller resolution](#queued-callers) does. An unpinned process's row carries its
[command log](#command-log) chain's tool-event count, refusals and last event (SP-073, D-36);
with `all` the operator also sees the chains of unpinned processes that have since exited
(`ended`) or been pinned (`unpinned`, the events from before the pin). Read one with
`orch spaces log process:<pid>`. The liveness sweep also ends and anchors the chain of every
unpinned process that has exited.

`ListSessions` (all → sessions) returns, per row, the harness, session ID, PID, space, pinned
directory, state, last activity, tool-event count, refusals (events whose decision is
`refused`, which only the enforcing guard SP-041 writes) and `ended_at`, newest activity first.
Ended sessions appear only with `all`. The operator sees every pin and every unpinned process;
a session's caller sees only its own row (the pin caller resolution names, RC-27); any other
caller gets an empty list, and queued tasks are refused. A process an agent detaches from its
session resolves as the operator (RC-26) and so sees every space and directory here, as with
other operator reads. Pins are not capped per process or pruned, so the sweep and the list read
every pin (RC-50); [retention](#command-log) deletes command-log records but keeps the pins, whose
`ended_at` it counts from.

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
operator, the widest result. Since SP-041 the connection's peer PID is checked against its
kernel token (see [queued callers](#queued-callers)), so a peer that hands its socket to a child
and exits cannot be resolved to whatever process reuses its PID; it gets no space.

### Command log

orchd stores every event the [tool hooks](hooks.md#tool-hooks) hand to `RecordToolEvent`
(spaces SP-014, D-04, D-17; `internal/toollog`, `internal/daemon/toollog.go`). It is never indexed
into RAG (INV-12): the log package imports only the standard library, SQLite and `redact`, and
`TestCommandLogNeverReachesRAG` type-checks the repository and fails if any function reachable
by calls from the code that takes, stores or reads tool events (`internal/toollog`,
`internal/daemon/toollog.go`, `toollog_read.go`, `toollog_anchors.go`, `toollog_chains.go`,
`toolevents.go`) calls `IngestText`, `Upsert` or `Ingest`, or if
any function both touches the log and calls one. A call through an interface may reach any
method of that name; calls through function values are not followed.

- **Where:** `toollog.db` in the data directory, a SQLite file of its own apart from `orch.db`
  (D-29), created mode 0600; SQLite gives its `-wal` and `-shm` files the same mode, and an older
  wider file is tightened at open. A symlink or anything but a regular file at that path is
  refused, and the log is then off (orchd logs why and counts and drops events as before).
- **Whose:** each record belongs to one chain, keyed by the [caller orchd
  resolved](#caller-sessions): a session's pin, a queued task's ID (`task:<id>`, D-31), or a
  harness process that holds no pin (`process:<pid>-<start>`, D-36). The
  `session_id` a hook sends is kept as `claimed_session`; it chooses the chain only when that
  session is pinned to the same harness process as the caller's pin (an earlier session of the
  process, before `/clear`), so a hook cannot write into another process's chain. That check
  reads `orch.db`, so it runs on the log's writer, never on the hook's RPC; dropped events are
  resolved the same way, so their gap lands in the chain that lost them. A hook whose nearest
  harness process holds no pin (started before the hooks were installed, or one whose
  SessionStart pin failed) logs into a chain keyed by that process: its PID and its start time in
  unix nanoseconds, so a reused PID starts a new chain (SP-073, D-36). Its events share that
  process's quota, its `claimed_session` is kept as evidence only, and a session pinned in the
  process later writes and reads its own chain, never the process's. At most 8 unpinned processes
  have a chain open at once (`maxProcessChains`, counted again from the file at open): a running
  chain is never expired nor cut for the total cap, and any process named like a harness counts,
  so without the bound a local process could grow the log without limit. Events of further
  processes are dropped and counted (logged by the next liveness sweep) until one exits or is
  pinned: the liveness sweep also ends the chain of a process that has since been pinned, whose
  sessions log in their own chains from then on. Events from the operator,
  from a task orchd cannot find, or from a caller under no harness are counted by caller kind
  and not stored (RC-29): they have no owner, and storing them would let any local process
  grow the log.
- **What:** per record `seq`, `ts` (when orchd took it) and the hook's `hook_at`, `kind`
  (`event`, `gap` or `end`), harness, event, tool, tool_use_id, agent_id, agent_type, cwd,
  old/new cwd, `targets` (the JSON of [`spaces.Targets`](hooks.md#tool-call-targets)), the
  command, the input size, `decision`, `rule`, `policy_rev` (the applied spaces revision) and
  the hashes. The command is a shell tool's command (an argv list joined with spaces) or any
  other tool's input, credential-redacted with `redact.RedactCommand` (the rules RAG ingestion
  uses plus command-log-only ones that prefer losing text to keeping a secret: any `login -p`
  or `--password` value, quoted or not, an unquoted YAML `password:` ending its line, a private
  key with no END line; pattern-based, so best-effort) and then capped at 2 KB with a
  `…[truncated N bytes]` marker. Every other text field is redacted and capped at 4 KiB; targets
  over 16 KiB are stored as opaque. Redaction reads at most twice the cap plus 4 KiB of a field,
  so a secret the cut passes through is still found and a 1 MiB input costs little. Text is made
  valid UTF-8 (invalid bytes become U+FFFD) before it is stored and hashed. The decision is the
  one the hook's [enforcing guard](hooks.md#enforcing-guard) reports (SP-041): `refused` or
  `allowed` with its rule id when it enforced; otherwise `logged`, with `rule` holding the
  verdict it would have given (`would refuse (<rule>)`, `would allow (<rule>)`, RC-86). While
  the guard's [enforcement switch](#enforcement-switches) is `off` the decision is `not
  enforced`. An event from a hook that sent no decision stays `logged`. The decision is the
  hook's report: a caller can only write to the chain orchd resolved for it, and a rule that is
  not a rule id is stored as `unknown`. Logging never stops: every switch state stores every event. `RecordToolEvent` refuses an event whose fields
  other than `tool_input` exceed 4 KiB, or that carries protobuf fields orchd does not know.
- **Off the hook's path:** `RecordToolEvent` only queues the event. The queue holds at most
  4096 events and 64 MiB (each event counted at its encoded request size plus 512 bytes), and one
  harness process (all its sessions together) or one task may hold at most a quarter of either,
  so a session flooding the log, or pinning more sessions, drops its own events, not others'. One writer goroutine reads each call's targets (up to `Targets`' 100 ms
  budget), builds the record, releases the event's share and stores up to 256 at a time in one
  transaction, so each chain keeps the order events arrived. Events dropped because the queue,
  the budget or the owner's share is full, or because a batch could not be stored, are counted
  per chain; a `gap` record with the count is written just before the chain's next queued record
  (an end record included), or after the batch, within a second, when none follows (D-30). A
  session can still hide what it ran by flooding its own share; the gap shows that it did.
  Refusals are not pushed out that way (D-37): a call the guard refused (enforced) for a pinned
  session or a task that finds its owner's share or the queue full goes into room kept apart
  for guard decisions (a quarter of the queue's length, 1024 events, and 16 MiB, with the same
  quarter-per-owner shares), so it is stored with its decision while the ordinary events past
  the share drop with a gap record. Unpinned harness processes' chains get no reserved room, so a
  process cannot fill it by starting programs it names claude; the decision is the hook's report
  (RC-104). Events lost before they reach orchd (orchd down, or slower than the
  hook's deadline) leave no trace; the guard has already decided the call.
- **Integrity:** each record's hash is SHA-256 over its JSON with the previous record's hash, so
  records chain per owner. When a chain's owner ends orchd writes an `end` record after its
  queued events and anchors its hash in the `anchors` table, unless the chain is empty or already
  ends in an end record: a repeated end adds nothing, and a resumed session or a retried task,
  which continues the same chain, is anchored again at its next end. A session ends at
  SessionEnd (or when the liveness sweep finds its process gone); a queued task when its ledger
  status becomes terminal (`complete`, `failed`, which includes a timed-out run, `blocked`,
  `cancelled` or `interrupted`) and no run of it is live: orchd follows the task ledger's
  changes (one of the task hub's 16 watcher slots), and every retention sweep, and the
  follower when it starts or falls behind, also anchors any task chain left open whose task is
  terminal; an unpinned harness process's chain when the liveness sweep finds the process gone
  (SP-073, D-35, D-36). One end per chain is in flight at a time; an end that arrives meanwhile
  runs one more after it.
- **Copies in orch.db** (SP-073, D-35; `internal/daemon/toollog_anchors.go`, tables
  `toollog_anchors` and `toollog_cuts` in `orch.db`): the writer hands every anchor it committed,
  and retention every cut it committed (its pruned head), to orchd, which copies them into
  `orch.db` on a goroutine of their own, so a slow `orch.db` never stalls the writer. An anchor
  already copied is never replaced (a disagreeing one is logged); a copied pruned head only moves
  forward. Copies `orch.db` refuses wait in memory (at most 10,000 of each; past that the oldest
  are dropped and logged) and are written by the next hand-over, retention sweep or verify; one
  handed over after the copier stopped (a sweep outliving shutdown) is written at once. A pruned
  head whose copy is lost anyway (a crash between the cut and its copy) is reported by verify for
  that chain until a later cut moves the copy past it. When
  orchd opens the log it copies every anchor `toollog.db` holds that `orch.db` lacks, so a crash
  between the two writes is repaired (an anchor added to the file by hand only constrains the
  chain further). Pruned heads are copied at open only the first time (`toollog_copy_started`),
  for a log that predates SP-073: copying them later would let a pruned head written into the file
  by hand excuse the records it claims retention deleted. What was copied that way is only as
  trustworthy as the file was then.
  `orch spaces log verify <session|task:<id>|process:<pid>>` (`VerifyToolLog`,
  operator-only) recomputes the chain and reports each record whose content no longer matches its
  hash, that no longer links to the one before it, that is missing from the sequence, and each
  anchored head that is gone or changed. It then compares the file with `orch.db`'s copies: an
  anchor in only one store, a pair that disagrees, a copy whose record is gone or has another
  hash, and a pruned head that differs from the copied one are reported, and the chain is then
  checked against the copied pruned head, so records deleted under one written into the file by
  hand show as missing. That catches what the file alone cannot: records removed after the last
  anchor the file still holds (with the later anchors), a chain rewritten from its first record
  with the same code (`TestAnchorCopiesCatchWhatTheFileCannot`), and a chain deleted under a
  forged pruned head (`TestForgedPrunedHeadReported`). Records written after a chain's last
  anchor in both stores are still unprotected until its next end. Both files belong to the same
  user, so this is defense in depth against edits to `toollog.db`, not against a process that
  can write both: it is tamper-evidence, not a defense. Verify works one chain at a time, so a
  chain deleted whole shows only when that owner is verified. An anchor only in the file is
  reported until orchd next opens the log, which copies it.
  Up to a chain's pruned head (retention, below) a missing record is no problem and a kept record
  links only to one kept just before it; retention keeps only refusals there, so any other record
  at or before the head is reported, as is a kept head record whose hash is not the head's. The
  first record after the head must link to the head's hash, and every kept record must still
  match its own hash. An anchor at or before the head may be gone with its record; the `anchors`
  rows themselves are never deleted. `VerifyToolLog` returns the head as `pruned` and the CLI
  prints it.
  Mode 0600 keeps out other users only: an agent running as the user can read the file, so
  "operator only" holds at orchd's RPCs, and the OS sandbox (SP-046) is what keeps sessions out of
  the file. `VerifyToolLog` waits at most 10 s for queued events to be stored first.
- **Cost:** `TestToolLogThroughput` (RecordToolEvent through stored, targets read for each
  `cd … && git -C … && cat … | grep` command, four sessions) measured 3,500–3,650 events/s on an
  M3 Max over five runs; the store alone (`TestAppendThroughput`) 37,000–40,000. The gate is 1,000 events/s (SP-014.2.a2).
- **Retention** (SP-015, D-05; `toollog.Store.Sweep`, `server.sweepToolLog`): orchd sweeps the
  log when it starts and every 10 minutes. A chain is *ended* when its owner no longer runs: a
  session once its pin has `ended_at` (SessionEnd, or the [liveness sweep](#session-liveness))
  and the liveness sweep has seen its harness process gone (`exited_at`); a queued task once the
  runner no longer runs it. A session ended early while its process runs (any process in it can
  call `EndSession`, and Claude Code's `/clear` ends one) is still running here, so the log of a
  process that keeps working is never expired, and an agent cannot start the clock on its own
  log. The end time is the latest of `ended_at`, `exited_at` and the chain's last record: for a
  session that ended normally that is within a minute of SessionEnd, and one ended early gets its
  full retention after its process exits. A task and a session chain with no pin ended at their
  last record (a task's end record once it is terminal). An unpinned harness process's chain runs
  while that PID runs with the same start time and ended at its last record once it exits.
  - An ended chain loses every record except refusals `sessions.command_log.retention` after it
    ended (`3d`, `7d` or `30d`; default `7d`, [config.md](config.md)). A refusal (decision
    `refused`, written by the guard, SP-041) goes once it is also 90 days old.
  - A running chain is never expired, however long it is idle. Over
    `sessions.command_log.max_session_mb` of records (default 50, 10 to 1000; the stored text plus
    64 bytes a record) it loses its oldest records except refusals until it fits.
  - Then, while the file's pages in use exceed `sessions.command_log.max_total_mb` (default 500),
    the chains that ended first lose, early, what an expired chain loses: everything but
    refusals under 90 days old. A running chain is never cut for it, and refusals under 90 days
    are never deleted, so the file can stay over the cap; orchd then logs that it is.
  - Each cut moves the chain's *pruned head* (table `pruned`: the last record the cut reached, its
    hash and kind) forward, so `verify` still checks what is left (above), shows how far records
    were cut, and a resumed session's next record continues the chain from it. Anchored heads are
    kept. orchd logs every cut (reason, owner, records deleted, the pruned head after it and its
    hash) to its own log, a record outside `toollog.db` to compare a pruned head with: the latest
    line for an owner names its head. `verify` reads the records, the head and the anchors in one
    read transaction, so a cut committed meanwhile is seen whole or not at all.
  - A sweep never deletes past the records a chain had when it read the owner's state, so a
    session resumed meanwhile keeps what it writes, and a second sweep at the same time deletes
    nothing. The writer and the sweep take turns for each write transaction, and a sweep stops
    waiting when orchd stops (SQLite's busy wait lets one that commits continuously starve the
    other), and each transaction still
    takes the write lock with its first statement, since in WAL mode a transaction that reads first
    cannot wait for another writer (it fails at once with `SQLITE_BUSY_SNAPSHOT`). A chain whose
    owner's state cannot be read, or that fails to cut, is logged and left for the next sweep; the
    rest are swept.
  - Freed pages go back to the file system (`auto_vacuum` incremental, then `incremental_vacuum`
    in steps of 2,048 pages, each taking its turn with the writer, after a sweep that deleted
    anything) and `journal_size_limit` keeps the `-wal` file to 64 MiB once checkpointed.
    `auto_vacuum` is set once, on the connection that creates the tables, never in the
    connection string, where every new connection would take the write lock to set it and a read
    would wait behind any write. A log created before SP-015 is converted with one `VACUUM` at
    open (schema version 2), before orchd serves; when that fails (no room for a copy, the file
    held open) orchd logs it, the log works on without shrinking, and the next start tries again.
  - **Limits.** The total cap is the one place retention deletes records before their time, and
    anything that writes the log fills it: ten sessions busy at once, or an agent flooding events
    push the ended sessions' records other than refusals out early. Each chain of a running
    process may keep the per-session cap (50 MB by default), and a process keeps a chain for every session it ended early
    (each `/clear`), so one long-lived process can hold several. A pruned head written into the
    file by hand is reported by `verify` against `orch.db`'s copy (above); without `orch.db` it
    still excuses any records deleted before it, refusals included, when nothing other than
    refusals is kept there and the next record links to it, and orchd's log has no line for it. A large delete holds the write lock for its whole statement; the writer waits
    for it, and events that overflow the queue meanwhile are dropped and recorded as a gap. Pins are not pruned here (RC-50), and the policy is read when orchd starts.
- **Reading** (SP-016, INV-12; `internal/daemon/toollog_read.go`, `toollog.Store.Page`,
  `toollog.Store.ActivitySince`): `ListToolEvents` returns one chain's records in sequence order
  after an `after_seq`, with the chain's pruned head: up to 1000 per call, and fewer once a page
  holds about 3 MiB for the operator or 512 KiB for an agent (a record can hold about 55 KiB;
  always at least one), so a reply stays under gRPC's 4 MiB limit. An agent's handler reads one
  page of a chain at a time; a call while one runs is refused (`ResourceExhausted`), never
  queued, so agents cannot pile up reads or delay the operator's. That bounds work, not memory: Before reading it flushes the writer, best effort:
  concurrent readers share one flush, so reading cannot pile control jobs on the writer's queue,
  a reader joining a running flush may miss events taken just before it called, and a flush that
  fails or times out is logged and what is stored is served anyway. gRPC keeps a reply until its
  client reads it, after the handler returns, so orchd caps every connection at 128 open calls
  (`maxStreamsPerConn`), at most 64 MiB of agent pages per connection; a client that opens many
  connections and reads no replies can still make orchd hold them (RC-63). The operator reads any chain and must name it. A pinned session reads only
  its own: the current session of the harness process its [resolved pin](#caller-sessions) names
  (RC-27), and only while that is still the connection's pin, so a pin a resume moved to another
  process (RC-50), or a session `/clear` (or a later subagent's pin) has since replaced, is no
  longer readable from a connection that resolved it earlier; a new connection reads the current
  session. A session `/clear` replaced is readable only by the operator. A queued task reads only `task:<its id>` (`ListToolEvents` is in
  `taskCallable`; the handler narrows it). Any other caller (an unpinned harness, a task orchd
  cannot find), and any request naming another chain, gets the same `PermissionDenied` whether or
  not that chain exists; that includes an unpinned process's chain, which only the operator reads
  (`process:<pid>` names the latest chain of that PID, `process:<pid>-<start>` one chain;
  `NotFound` when the PID has none). `SummarizeToolLog` (operator-only) sums every chain's `event` records
  taken in a window (7 days unless `since` names an earlier time) per space: sessions and tasks
  with events, tool calls (`pre-tool-use`), refusals, directory moves (`cwd-changed`) and moves
  into another space, which are moves whose new directory a session would be pinned to a
  different space for under the policy applied now (`placeSession`, with GitHub lookups answered
  from the cache alone: the directories are whatever sessions reported, so placing them spends
  none of the lookups pinning relies on, and an uncached repo keeps the owner its remote names).
  Placing stops after 20 seconds. A session counts in the
  space it is pinned to; tasks, which have no space yet, sessions pinned with no policy applied,
  chains with no pin and unpinned processes are under `""`, and their moves are not compared. One summary places at
  most 500 distinct directories; moves past that or the time limit are counted as unplaced. Moves a Bash `cd` makes without a `cwd-changed` event (Codex
  sends none) are not counted. CLI: `orch spaces log [<session>|task:<id>|process:<pid>]`,
  `orch spaces log summary [--since 168h]`.

Clients dial `unix://<socket>` lazily (failure surfaces on the first call) and set their own
deadlines: hooks 0.5 s for writes and 3 s for prompt search; `orch status` 2–3 s; long CLI
commands up to 2 hours (migration).

### Enforcement switches

Each layer of spaces enforcement has an operator-only switch (spaces SP-070, D-11, D-12, D-40,
INV-16; `internal/spaces/enforce.go`, `enforce_store.go`, `internal/daemon/enforcement.go`):

| Layer | States (default first) | What it switches |
|---|---|---|
| `memory` | `on`, `off` | memory filtering by space (SP-025) |
| `guard` | `block-other-jobs`, `off`, `log`, `strict` | the pre-tool-use guard's mode (SP-040, SP-041) |
| `terms` | `on`, `off` | outbound term checks (SP-044, SP-045) |
| `sandbox` | `on`, `off` | generated sandbox deny rules (SP-046) |
| `hooks` | `on`, `off` | the tool hooks themselves (pre-tool-use, cwd-changed) |

- **Stored** in `orch.db` (migration `0029`): `spaces_enforcement` holds one row per switched
  layer (state, set_at, set_by, and for a timed switch `until` and the state it reverts to); a
  layer with no row is at its default. `spaces_enforcement_log` appends every change with its
  time, caller (`callerLabel`, or `timer` for a revert) and whether a timer expired, in the same
  transaction as the row.
- **orchd's copy wins:** orchd loads the switches once at startup (`spaces.LoadVerified`) and
  afterwards keeps them in memory, writing each change to `orch.db` before it takes effect. At
  startup a layer whose row does not match its newest log entry (state, time, caller, timer and,
  for a timed switch, the state it reverts to; a row written, or an entry appended, outside
  orchd), whose state it does not have, or whose time cannot be read starts at its default, which
  enforces; a timer that cannot be read reads as expired. While orchd runs, an edit to `orch.db` changes
  nothing; `GetEnforcement` and `orch status` report it as drift until orchd rewrites the layer.
  Switches that cannot be read at startup are the defaults.
- **Set** by `SetEnforcement` (`orch spaces enforce <layer> <state> [--for 15m]`). It is
  operator-only: refused to queued tasks and to any caller with a harness or orchd among its
  ancestors, or whose ancestry orchd cannot read (`refuseNonOperator`). `--for` (more than 0, at
  most 24 h) makes a timed switch, which reverts to the state before it; a timed switch set over
  another keeps the first one's revert state, and an untimed set replaces any timer. Setting the
  state a layer already has changes nothing.
- **Timers:** every read (`GetEnforcement`, `Status`) reverts expired switches first, and orchd
  checks every 5 s so the revert's event arrives on time. The command log's writer only reads
  (`enforcement.state`): it sees an expired switch as reverted without writing.
- **Announced:** every change is logged by orchd and appended to the `spaces` event stream
  (`[info] enforcement: <layer> switched from X to Y by <caller> [until T]`, plus what a hooks
  switch could not do to Claude Code's settings). Sessions do not read that stream:
  `GetEnforcement` is open to every caller, queued tasks included, and the SessionStart digest of
  every session names each layer that is off, or the guard in `log`, and since when, and any
  drift ([hooks.md](hooks.md#events)). When orchd does not answer, the digest reads `orch.db`'s
  switches read-only (`spaces.ReadEnforcement`), so stopping orchd does not hide a switched-off
  layer. `orch status` names them in its `spaces` section, with who set them for the operator.
- **The hooks switch** (D-40): orch settings cannot turn the Claude Code tool hooks on or off
  (`SetSettingEnabled` refuses them with `FailedPrecondition`), and `orch install` writes them
  whatever a settings toggle says ([install.md](install.md#claude-code)). Switching `hooks` off
  turns Claude Code's `PreToolUse` and `CwdChanged` orch-hook entries off through the settings
  service, and on again when switched back or when the timer reverts it; install then leaves them
  out while the switch is off. Codex's `hooks.json` is not managed by orch settings, so its
  `pre-tool-use` hook stays and its events are still logged. While the switch is on, a tool hook
  orch installed that is now turned off, or missing or removed from `settings.json` (by hand,
  outside orch), makes the hooks layer read as not enforcing, with the reason (`problem`), in the
  digest and status, as does a settings listing or sync orchd cannot read. With a spaces policy
  applied and orch's session-start hook (which prints the digest) in Claude Code's settings, a
  tool hook with no settings row at all counts as not installed, so deleting its row from
  `orch.db` does not hide it; the check goes by the items' identities, which each sync derives
  from the file, never by their stored origin. With every orch hook gone from Claude Code's
  settings, no digest runs and the layer reads as enforcing (`orch status` lists the tool hooks as
  missing under harness wiring). The check reads Claude
  Code's settings through the settings service, under its own 5 s deadline, and is reused for 5 s. `orch spaces enforce hooks on`
  puts them back even though the switch is already on.
- **Limits:** operator-only rests on the [caller checks](#queued-callers), so a process an agent
  detaches from its session (whose parent exits, so it is reparented to launchd) passes as the
  operator, as for `ApplySpaces`. A process running as the user can also write both tables
  consistently while orchd is stopped, which the next start takes. Either way the switch shows in
  every digest and in `orch status`. Self-protection paths (SP-042) are the guard's answer to
  both. A `"disableAllHooks": true` in a project's Claude Code settings turns off every hook,
  the digest included, without any switch; the hooks check reads only the user's
  `~/.claude/settings.json`. When orchd does not answer, the digest's fallback reads only the
  switches in `orch.db`, not whether the tool hooks are still in `settings.json`.
- **Consumers:** the guard switch sets the enforcing guard's mode: orchd writes it into the
  guard's snapshot after every change and timed revert, so the next tool call of every running
  session uses it ([hooks.md](hooks.md#enforcing-guard), SP-041). Memory filtering, terms and
  sandbox rules read `enforcement.state` when SP-025, SP-044 and SP-046 land.

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
| `PinSession` | session_id, cwd, parent_session_id, source → the pin | see [session pins](#session-pins); `source` `resume` moves a pin to a new harness process; refused to queued tasks |
| `EndSession` | session_id → ended_at | sets `ended_at` once; only the pinned harness process; refused to queued tasks; writes the session's [command log](#command-log) end record and anchors its head in the background |
| `RecordToolEvent` | event (`pre-tool-use`/`cwd-changed`), session_id, cwd, tool_name, tool_input (≤ 1 MiB), tool_input_size, tool_use_id, agent_id, agent_type, old_cwd, new_cwd, at → — | takes a [tool hook](hooks.md#tool-hooks) event and answers at once; attributed to the [caller orchd resolved](#caller-sessions), never to session_id; open to queued tasks. Counted by caller kind and stored in the [command log](#command-log) when its caller is a pinned session, a running task or a harness process holding no pin; never indexed |
| `ListSessions` | all → sessions (harness, session_id, harness_pid, space, first_cwd, state, last_activity, tool_events, refusals, ended_at) | runs a [liveness](#session-liveness) sweep first; the operator sees every session and unpinned harness process (with its command log chain's counts and last event), a session only its own row, anyone else none; refused to queued tasks; ended sessions, and the chains of unpinned processes that exited or were pinned since, only with `all` |
| `GetEnforcement` | → switches (layer, title, state, default, states, relaxed, set_at, set_by, until, reverts_to), error | every [enforcement switch](#enforcement-switches); open to every caller, queued tasks included; `set_by` only for the operator; switches that cannot be read are returned as the defaults with `error` |
| `SetEnforcement` | layer, state, for_seconds → current switch, changed, notes | operator-only (refused to callers descended from a harness or orchd); unknown layer or state, or `for_seconds` outside 0–86400 → `InvalidArgument`; logged with time and caller and emitted on the `spaces` stream; `notes` say what the hooks switch did to Claude Code's settings |
| `VerifyToolLog` | owner (session ID, `task:<id>` or `process:<pid>[-<start>]`) → records, anchors, ends, gaps, dropped, problems | recomputes the owner's [command log](#command-log) chain after storing what is queued, and compares its anchors with orch.db's copies; operator-only |
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
| `<data>/toollog.db` | SQLite, mode 0600: the [command log](#command-log); never indexed; trimmed by retention |
| `<data>/guard/` (0700) | the [enforcing guard](hooks.md#enforcing-guard)'s inputs, 0600: `policy.json` (applied policy, guard switch) and `pins/<session>.json` |
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
| `session_pins` | session_id (key), harness, harness_pid, harness_started (unix ns), first_cwd, main_checkout, space, policy_revision, pinned_at, ended_at, inherited_from, resumed_at, exited_at ([session pins](#session-pins)) |
| `spaces_enforcement`, `spaces_enforcement_log` | layer (key), state, set_at, set_by, until, previous; every change with at, layer, state, previous, by, until, expired ([enforcement switches](#enforcement-switches)) |
| `settings_items` | provider, kind, identity (key), enabled, origin (`orch`/`you`/`found`), payload (JSON), present, removed_at, created_at, updated_at ([settings.md](settings.md)) |

## Invariants

- Clients stay thin; every long-lived component is a named supervised unit, not a bare goroutine.
- The socket (0600) is the trust boundary; destructive operations gate server-side, and
  operator-only RPCs refuse queued callers server-side.
- Bad requests fail at the RPC; nothing invalid becomes durable.
- Task-completion events carry no result text (results may be steered by an untrusted repo).
- Credentials orchd holds (the Qdrant key) are removed from child agent environments.
- Counts saturate rather than wrap.
