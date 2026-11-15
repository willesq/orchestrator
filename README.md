<img src="macos/Resources/Brand/icon.svg" alt="Orch logo: three rings overlapping on a shared core" width="96">

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
  restarts orchd, and has a window for its subsystems, searching memory, the task queue, the
  GitHub accounts and orgs it watches, the safety net, and a Settings pane, the one place to configure orch, that also turns Claude Code
  hooks, permissions, plugins and cc-safety-net rules on or off (per machine, kept in `orch.db`,
  honored by `orch install`). Like
  the CLI, it talks to `orchd` and never opens the databases directly.
- **Memory** is hybrid (meaning plus keyword) search over your sessions, repo docs, notes and
  fetched pages, ranked by source type and recency. Each prompt gets a few dated pointers, one per
  source and labelled by how far their origin is trusted, that the agent can follow with
  `rag_search`; `orch rag eval` measures retrieval on a golden set
  ([docs/MEMORY.md](docs/MEMORY.md)).
- **Memory spaces** (off by default) keep work for different jobs apart on one machine: each
  job's memory, tasks and PRs stay in its own space, and a guard before each tool call keeps a
  session out of another job's folders and repos ([docs/SPACES.md](docs/SPACES.md)).
- **`orch install`** wires the hooks, MCP servers and a shared agent policy into each harness,
  and for Claude Code installs [cc-safety-net](docs/INSTALL.md#safety-net) pinned to a reviewed
  release with orch's guard rules.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full picture.

## Quick start

macOS only. The Homebrew tap is private: tap it over SSH and authenticate `gh`, which the formula
uses to download the release.

```sh
brew tap willesq/tap git@github.com:willesq/homebrew-tap.git
brew install willesq/tap/orch
orch install                # one plan of every change; confirm to apply (or --yes)
orch check                  # verify preconditions
```

`orch install` wires every harness it finds, downloads the embedding model and starts orchd.
`orch uninstall` reverses it ([docs/INSTALL.md](docs/INSTALL.md#uninstalling)).

Then use your coding agent as usual. `orch search "<query>"` searches memory directly;
`orch status` reports on the whole system.

## Docs

| Doc | Covers |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Components, data flow, where state lives, package map |
| [INSTALL.md](docs/INSTALL.md) | Install, harness wiring, shared agent policy, building from source |
| [MEMORY.md](docs/MEMORY.md) | Session memory, chunking, hybrid search, retrieval evals |
| [SPACES.md](docs/SPACES.md) | Memory spaces: keeping jobs apart, the operator, enforcement, harness coverage, limits |
| [TASKS.md](docs/TASKS.md) | Background tasks, model registry, routing |
| [OPERATIONS.md](docs/OPERATIONS.md) | Status, logs, restarts, upgrades, Claude settings |
| [internals/](docs/internals/README.md) | Implementation reference: APIs, formats, algorithms, building your own |

## Contributing

Follow [AGENTS.md](AGENTS.md). Changes to `main` need a pull request and a code-owner review.

```sh
make build    # build/bin/{orch,orchd,orch-hook}; needs the native libraries in INSTALL.md
make test     # pure-Go tests
make cover    # per-file coverage gate (>= 80%)
make app      # build/app.noindex/Orch.app, the menu bar app (needs Xcode 26+)
make app-install  # install it in /Applications and launch it
```
