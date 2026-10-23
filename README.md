# orch

Local-first memory and task orchestration for coding agents (Claude Code, Codex, Gemini).

Coding agents forget everything between sessions. orch gives them a shared, durable memory on
your machine: it indexes your docs and past sessions, adds relevant context to each prompt, and
runs background agent tasks that survive restarts.

## How it works

- **`orchd`**, a daemon, owns everything long-lived: the embedding model, a native Qdrant vector
  store, a durable task queue, and schedulers that keep indexed sources fresh.
- **`orch`** (the CLI) and **`orch-hook`** (harness hooks) are thin clients of `orchd`, over a
  Unix socket. MCP servers expose search and the task queue to agents.
- **`Orch.app`**, a macOS menu bar app, shows orchd's health and the PRs that need you, starts or
  restarts orchd, and has a window for its subsystems and for searching memory. Like
  the CLI, it talks to `orchd` and never opens the databases directly.
- **`orch install`** wires the hooks, MCP servers and a shared agent policy into each harness.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full picture.

## Quick start

macOS only. The repo and Homebrew tap are private; `gh` must be authenticated with read access.

```sh
brew tap willesq/tap git@github.com:willesq/homebrew-tap.git
brew install willesq/tap/orch
orch model fetch            # embedding model, ~90 MB
orch install                # preview changes; add --apply to make them
brew services start orch    # start orchd
orch check                  # verify preconditions
```

Then use your coding agent as usual. `orch search "<query>"` searches memory directly;
`orch status` reports on the whole system.

## Docs

| Doc | Covers |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Components, data flow, where state lives, package map |
| [INSTALL.md](docs/INSTALL.md) | Install, harness wiring, shared agent policy, building from source |
| [MEMORY.md](docs/MEMORY.md) | Session memory, chunking, hybrid search, retrieval evals |
| [TASKS.md](docs/TASKS.md) | Background tasks, model registry, routing |
| [OPERATIONS.md](docs/OPERATIONS.md) | Status, restarts, upgrades |
| [internals/](docs/internals/README.md) | Implementation reference: APIs, formats, algorithms, building your own |

## Contributing

Follow [AGENTS.md](AGENTS.md). Changes to `main` need a pull request and a code-owner review.

```sh
make build    # build/bin/{orch,orchd,orch-hook}; needs the native libraries in INSTALL.md
make test     # pure-Go tests
make cover    # per-file coverage gate (>= 80%)
make app      # build/app/Orch.app, the menu bar app (needs Xcode 26+)
make app-install  # install it in ~/Applications and launch it
```
