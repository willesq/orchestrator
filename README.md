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

For Claude Code, install registers the `rag` and `queue` MCP servers in the user-global
`~/.claude/settings.json` (and in `~/.claude.json` when present) and allows their tools in
`permissions.allow` as `mcp__rag__*` and `mcp__queue__*`. Registration alone is not enough:
under the default `ask` permission mode every call waits for interactive confirmation, so
lifecycle hooks and background agents — which have nobody to answer the prompt — lose access
to durable memory. Because the grant is user-global, it applies in every project, and the
dry-run summary says so before you apply it.

Existing rules are scanned before anything is appended, so repeat installs add no duplicates;
`mcp__*`, `mcp__rag` and `mcp__rag__*` all count as already covering a server. Both categories
that outrank `allow` hold the matching rule back entirely and are reported instead: a `deny`
rule, and an `ask` rule — the one you add precisely to keep a destructive tool such as
`rag_delete_source` behind a confirmation. Either is honored whether written as a blanket
`mcp__*`, a bare `mcp__rag`, a wildcard `mcp__rag__*`, or a single tool
`mcp__rag__rag_delete_source`. A `permissions` block, or an `allow`, `deny` or `ask` of an
unexpected JSON type, aborts the install with an error rather than being overwritten, and no
file is written when any edit fails to compute. Codex and Gemini use different permission
models and are unaffected.

To wire every supported coding agent, use `orch install --target all`, then
`orch install --target all --apply`. The default target remains `claude-code`;
`--target codex` and `--target gemini` install those harnesses individually.
There is no separate setup command: `install` handles harness configuration.

The shared policy is version-controlled in
[`internal/install/agent-instructions.md`](internal/install/agent-instructions.md).
It contains agent-neutral coding expectations for exploration, structure,
testing, security, credentials, Git/GitHub workflows, reviews, documentation,
and durable memory. Repository `AGENTS.md` loads that policy and adds this
project's conventions; repository `CLAUDE.md` imports `AGENTS.md`.
It is embedded in the CLI at build time. `orch install` writes that policy to
`~/.config/orch/AGENTS.md`; it never seeds the policy from a user's global files.
Change the repository source, rebuild or upgrade the CLI, and rerun install to
publish policy updates locally. The installed copy is Orchestrator-owned;
`orch status` detects missing or modified content against the policy shipped
with the running CLI, and recommends reinstalling to repair it.

Claude's global `~/.claude/CLAUDE.md` and Gemini's `~/.gemini/GEMINI.md` import
the installed shared file. Codex's global `~/.codex/AGENTS.md` explicitly directs
the agent to read it at session start; Codex does not expand Claude-style `@`
imports. An existing nonempty `AGENTS.override.md` receives the directive instead
because Codex loads it preferentially. Personal instructions remain in each
harness's entry file; only its Orchestrator-managed reference is updated.
Existing files, including the installed shared policy, are backed up before
modification. Backups are written owner-only (`0600`) into owner-only
directories, because harness config can carry OAuth state and MCP server `env`
blocks and these copies are retained indefinitely. Backups written by releases
before this change used `0644` under `0755`, and tightening is not retroactive —
clean up any you still have with
`chmod -R go-rwx ~/.local/share/orch/backups` (or the same path under
`$ORCH_DATA_DIR` / `$XDG_DATA_HOME/orch` if you set either). If upgrading an earlier installation that copied personal rules
into the shared file, recover those rules from its backup into your personal
harness entry files before relying on the replacement policy.

These paths assume the standard harness homes; `--home` relocates the whole
install for testing. Custom `CODEX_HOME` locations are not currently resolved.
Codex hooks must be reviewed and trusted in Codex before they execute. Memory
search and ingest through MCP work independently of automatic hook execution.

When run inside the Orchestrator checkout (including subdirectories and linked
worktrees), `orch status` also reports **repository instructions** in text and
JSON. It checks `AGENTS.md`, `CLAUDE.md`, and the version-controlled shared policy
against the current committed revision and verifies that the committed Claude
entry is the standalone `@AGENTS.md` import. Missing or edited files are reported as drift, with advice to review
local edits before restoring the affected file from `HEAD`, or to commit an
intentional instruction change. A committed broken import needs correction and
a new commit. Status is read-only; `orch install` repairs global wiring, while
Git owns repository guidance. Outside this repository the project check is not
applicable. These checks compare files on disk; they do not verify that a running
agent has loaded the latest instructions.

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
