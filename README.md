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
`docs-scheduler`, and `pr-babysitter`. Every restart is idempotent: a component that is
already stopped is simply started.

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
