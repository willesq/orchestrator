# orch — Claude Orchestrator (Go)

A local-first orchestration layer for agentic coding CLIs (Claude Code, Codex, Gemini).
One supervised daemon (`orchd`) owns a warm embedding model, a native Docker-free Qdrant,
a durable workflow engine, a source-freshness scheduler, and a PR babysitter. The CLI,
hooks, and MCP servers are thin clients that dial `orchd` over a Unix-domain socket.

macOS, single-user. Ships three binaries: `orch` (CLI), `orchd` (daemon), `orch-hook`
(harness lifecycle hooks).

## Install (Homebrew)

The tap and this repo are private, so tap over **SSH** (HTTPS tapping fails to
authenticate). The formula then authenticates the asset download with the gh CLI,
so an authenticated `gh` (with read access to `willesq/orchestrator`) is all you
need — no manual token export:

```sh
brew install gh && gh auth login   # once, if you haven't already
brew tap willesq/tap git@github.com:willesq/homebrew-tap.git   # SSH — the tap is private
brew install willesq/tap/orch     # pulls the prebuilt binaries + gh + onnxruntime
orch model fetch                  # download the embedding model (~90MB, once)
orch install --apply              # wire hooks + MCP + alias + launchd (backs up first)
brew services start orch          # start orchd (+ native qdrant, workflows, scheduler)
orch check                        # verify preconditions are green
```

`orch install` is **dry-run by default** — run `orch install` (no `--apply`) to see exactly
what it would change. It is surgical (preserves other tools' hooks) and idempotent.

To wire every supported coding agent, use `orch install --target all`, then
`orch install --target all --apply`. The default target remains `claude-code`;
`--target codex` and `--target gemini` install those harnesses individually.
There is no separate setup command: `install` handles harness configuration.

All targets share `~/.config/orch/AGENTS.md`. The first install seeds it from
the existing global `~/.claude/CLAUDE.md`, preserves personal instructions, and
replaces the old Orchestrator-managed section with an agent-neutral memory policy.
Subsequent installs preserve shared personal rules and update only that section.
Edit the shared file for rules that should apply across agents.

Claude's global `~/.claude/CLAUDE.md` and Gemini's `~/.gemini/GEMINI.md` import
the shared file. Codex's global `~/.codex/AGENTS.md` explicitly directs the agent
to read it at session start; Codex does not expand Claude-style `@` imports.
An existing nonempty `AGENTS.override.md` receives the directive instead because
Codex loads it preferentially. Existing harness-specific rules are preserved;
Claude's legacy rules move into the shared file when safe to do so. Backups are
created before modifying existing files. `orch status` includes instruction drift.

These paths assume the standard harness homes; `--home` relocates the whole
install for testing. Custom `CODEX_HOME` locations are not currently resolved.
The legacy rules are carried over verbatim: review any Claude-specific tool names
or relative imports when adapting personal instructions for other agents.
Codex hooks must be reviewed and trusted in Codex before they execute. Memory
search and ingest through MCP work independently of automatic hook execution.

## Status and restart

`orch status` is the whole-system sitrep: the `orchd` service, every supervised subsystem
(with its last tick and restart count), the vector store, the on-disk stores and their
freshness, harness wiring drift, and the daemon-independent preconditions. It stays useful
with `orchd` down — which is when you usually run it — and always exits zero. Use
`orch check` when you want a pass/fail gate with a non-zero exit, and `--json` when you
want to script it.

`orch restart` restarts the whole system by bouncing the `orchd` process through its
service manager, then waits for it to answer again. Naming a component restarts just that
one **in place**, without dropping the socket, the warm embedder, or the other subsystems:

```sh
orch status                      # full report (add --json for scripts)
orch restart                     # everything: bounce orchd (== orch restart orchd)
orch restart --list              # what can be restarted, per the running daemon
orch restart sources-scheduler   # one subsystem, in place
orch restart subsystems          # every subsystem, in place (no process bounce)
```

Subsystems are `qdrant` (binary mode only), `workflow-worker`, `sources-scheduler`,
`docs-scheduler`, `pr-babysitter`, and `self-update`. Every restart is idempotent: a
component that is already stopped is simply started.

### Picking up a new build

Upgrading `orch` replaces the binaries on disk but leaves the running `orchd` serving the
old code. The `self-update` subsystem closes that gap by itself: it watches the path the
service manager launches `orchd` from and, within a minute of a new build appearing there,
exits — and the KeepAlive supervisor behind `orchd` (`brew services`, or the launchd agent
`orch install` writes) starts the new binary. Nothing in flight is lost: the shutdown
drains the workflow worker, and agent tasks are durable, so they resume under the new
build.

`orch status` reports the `build` line as `drifted` when `orchd` is nonetheless still
running an older version than the installed `orch` — which means the self-restart did not
happen, typically because `orchd` was started by hand with nothing supervising it. `orch
restart` is the manual way to close it.

## Build from source

The ORT embedder is cgo: it links a prebuilt `libtokenizers.a` at build time and loads
`libonnxruntime.dylib` at runtime.

```sh
brew install onnxruntime
# stage the tokenizers static lib for your arch (daulet/tokenizers release):
#   build/lib/darwin-arm64/libtokenizers.a
make build      # -> build/bin/{orch,orchd,orch-hook}
make test       # pure-Go tests (no ORT tag)
make cover      # per-file coverage gate (>= 80%)
make proto      # regenerate the orchd gRPC stubs after editing the .proto
```

## Architecture

See [`docs/GO_REWRITE_PLAN.md`](docs/GO_REWRITE_PLAN.md) for the full design: the `orchd`
process model, the embedder parity, durable workflows (go-workflows), the source registry,
the multi-harness install adapters, and the packaging story.

## Command surface

`orch check | config | daemon | dispatch | ingest | install | mcp | model | ping | prs |
qdrant | repo | restart | search | source | status | version`
