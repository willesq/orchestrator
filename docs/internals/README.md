# orch internals

Implementation reference: enough to change orch, or to build your own. Start with
[ARCHITECTURE.md](../ARCHITECTURE.md) for the overview; the user guides (`docs/*.md`) cover how to
use it. The code is the source of truth; these pages say what it does and why.

| Page | Covers |
|---|---|
| [daemon.md](daemon.md) | process model, gRPC API, supervised subsystems, on-disk state, schema |
| [memory.md](memory.md) | embedding, chunking, Qdrant layout and payload, BM25, hybrid search, timelines, redaction, sources, migration, evals |
| [hooks.md](hooks.md) | hook events, prompt context, session records, hand-offs, summaries, events |
| [tasks.md](tasks.md) | task queue, Claude Code and Codex runners, verification, model registry, routing, PR outcomes |
| [install.md](install.md) | harness adapters, files written, shared policy, safety net, drift |
| [settings.md](settings.md) | managed settings: Claude Code hooks, permissions and plugins, cc-safety-net rules, the reconciler, adding a provider |
| [config.md](config.md) | every setting and its default |
| [cli.md](cli.md) | command tree and MCP tools |
| [app.md](app.md) | macOS menu bar app: connection, health, daemon control, signing and release |

## Building your own

A workable order, each step usable on its own:

1. **Store and search.** An embedder (any sentence model; keep chunks within its token window), a
   vector store with a payload per chunk, deterministic point IDs per `(source, chunk index)`, and
   delete-the-tail on re-ingest. Add BM25 sparse vectors and rank fusion once dense search works.
2. **Daemon.** One process owns the model, store and schedulers, behind a local socket readable
   only by you. Clients (CLI, hooks, MCP) stay thin and stateless.
3. **Hooks.** Prompt context (project first, cosine floors) and session capture. Keep hooks fast
   and infallible; hand anything slow to the daemon through files it processes later.
4. **MCP tools** so agents can search, ingest and list memory themselves.
5. **Install.** Per-harness adapters that preview, back up, merge rather than overwrite, and stay
   idempotent.
6. **Tasks.** A durable queue that runs headless agent sessions, records outcomes, and optionally
   verifies them; then model routing learned from those outcomes.

## Security model

- **Trust boundary:** the daemon socket, mode 0600. No network listener; the vector store binds to
  127.0.0.1 with a generated API key that child agents never receive.
- **Untrusted content:** indexed pages, repos and transcripts may carry instructions. They are
  stored and returned as data: secrets are redacted on capture, injected context is labelled,
  task results never appear in digests, quoted timeline markers are neutralized, and failing test
  output is fenced as untrusted when fed back to an agent.
- **Agents act as you,** so guards inside queued tasks (no dispatch, no rating, routing or
  GitHub changes, no RAG writes) are first lines of defense, not boundaries. orchd enforces the
  RPC side by refusing operator-only calls from processes it started
  ([daemon.md](daemon.md#queued-callers)), but a same-user process can still leave its tree or
  write orch's files directly; the hard limits are `queue.max_active` and the permission mode.
- **Destructive operations** gate server-side: `DeleteSource` needs `confirm`; collection drops
  take the name from config only; installs back up first and refuse to overwrite files orch does
  not own.
- **Credentials:** never written to new places. Child environments drop the vector-store key;
  verification runs with no tokens or SSH agent.

## Conventions

Go, `gofmt`, packages under `internal/`. `make test` (pure Go), `make cover` (≥ 80 % per file),
`make build` (native, needs ONNX Runtime and `libtokenizers.a`), `make proto` after editing
`orchd.proto`. See [AGENTS.md](../../AGENTS.md).
