# Claude Orchestrator — Go Rewrite & Restructure Plan

**Status:** Design locked, implementation not started.
**Date:** 2026-07-30.
**Scope:** A fresh, ground-up Go rewrite of the orchestrator (currently ~9K LOC Python)
with a fundamental restructure — not a 1:1 port. This document is the authoritative
plan; `docs/SPEC.md` describes the *current Python system* and will be rewritten to
match this design as phases land.

---

## 1. Motivation

Replace the current `uv run python <script>` invocations (used by every launchd daemon
and every Claude Code hook) with **compiled Go binaries that have exact names and scoped
permissions**. Today the executable macOS grants TCC / Full-Disk / network permissions to
is the *Python interpreter*, shared across all 7 daemons and 7 hooks — grant it one
capability and you've granted it to everything. Compiled binaries give a 1:1
Label→executable mapping, so any permission grant or future sandbox profile is scoped to
one binary, not to "python on this machine."

Secondary goals that emerged during design:
- Single supervised service (`orchd`) instead of a fleet of scripts.
- Multi-harness: install into **Claude Code, Codex, and Gemini**.
- Extensible RAG taxonomy (custom source types, scopes, tags) driven by config/CLI.
- Everything configurable and persisted from the CLI — no hunting for YAML.
- Robust, durable queue + sleep-safe cadence.
- Single compiled binary (assets embedded) distributed via Homebrew (packaging deferred).

---

## 2. Locked decisions

| Area | Decision |
|---|---|
| Language / CLI / config | **Go**; **Cobra** CLI; **Viper** config (load/precedence) + a clean canonical writer for CLI-persisted `config set` (Viper `WriteConfig` loses comments/order). |
| Embeddings | **hugot** (`github.com/knights-analytics/hugot`) **ORT backend**, in-process, runs the same **all-MiniLM-L6-v2** (384-dim, mean-pooled, L2-normalized). cgo + `libonnxruntime.dylib` (brew-managed dep). Full record: `project://orchestrator-go-rewrite/decisions/embeddings`. |
| Core process | **`orchd`** — one always-on supervised daemon. Owns the **warm embedder** (only place the ONNX model loads; lazy-load + configurable idle-unload), Qdrant access, go-workflows worker(s), and the source/cadence scheduler. Exposes a **local Unix-domain socket** (gRPC). CLI, hooks, and MCP servers are **thin clients**. |
| Orchestration spine | **`cschleiden/go-workflows`** — embedded durable workflows, **no server**, pure-Go SQLite backend (`modernc.org/sqlite`, no cgo), durable timers that **catch up after downtime/sleep**. Escape hatch to real Temporal later (Temporal-shaped API). Caveat: single-maintainer (mitigated: MIT + portable API). Recurring jobs use `Sleep`+`ContinueAsNew` (no built-in cron). |
| Multi-harness | Install into **Claude Code / Codex / Gemini** via a `HarnessAdapter` per tool. `orch install --target claude-code\|codex\|gemini\|all`. |
| Storage | **SQLite (pure-Go)** family: `workflows.db` (go-workflows) + `orch.db` (settings, sources, projects, source-type registry). **Qdrant** remains the vector store, mode configurable: `rag.qdrant.mode = docker\|binary\|external`. |
| Module path | `github.com/willesq/orchestrator`. |
| Homebrew tap | `willesq/homebrew-tap` — **packaging deferred**, do later. |
| Index migration | Wipe + re-embed fresh (new runtime + new taxonomy require it). |
| RPC transport | gRPC over the Unix socket (typed contract shared by all client binaries). |

---

## 3. Principles

1. **One warm process, many thin clients.** Heavy state (ONNX model, Qdrant conn,
   workflow engine, scheduler) lives only in `orchd`; everything else dials it. Bounded
   memory, exact binary names, scoped permissions.
2. **Embedded over external.** SQLite + go-workflows + in-process embedder. The only
   optional external is Qdrant (mode configurable).
3. **Config-driven, not hardcoded.** Source types, scopes, orgs, cadences, harness
   targets — all data, editable from the CLI.
4. **Harness-agnostic core, adapter edges.** RAG + queue speak MCP everywhere; only the
   install/wiring layer knows Claude vs Codex vs Gemini. Skills authored AI-agnostically.

---

## 4. Process & data-flow model

```
        ┌────────── thin clients (near-zero RAM, dial the socket) ──────────┐
  orch CLI (cobra)   hook binary (per harness)   MCP servers (queue,rag)   check/status
        └──────────────────────────────┬───────────────────────────────────┘
                                        │  Unix-domain socket RPC (gRPC)
                                        ▼
┌───────────────────────────────── orchd (launchd KeepAlive) ─────────────────────────────┐
│  Embedder (hugot/ORT, warm, lazy-load + idle-unload)                                     │
│  go-workflows engine  ── workers ──►  agent tasks, ingestion pipelines (durable)         │
│  Scheduler (go-workflows durable timers, catch-up-on-wake)                               │
│  Source registry walker  ── refresh-or-flag ──►  repos / web / threads                   │
│  RAG service (chunk → embed → Qdrant upsert/search)                                      │
└───────┬───────────────────────────────┬────────────────────────────────────┬───────────┘
        ▼                               ▼                                     ▼
   SQLite family                    Qdrant (mode: docker|binary|external)   filesystem
   (workflows.db, orch.db)          vector store                           (transcripts,
                                                                            backups, logs)
```

---

## 5. Repo layout

```
cmd/                 # THIN entrypoints only (func main); all logic is in internal/
  orch/              # → internal/cli.Execute()
  orchd/             # signal wiring → internal/daemon.Run(ctx)
  orch-hook/         # stdin → internal/hook.Run()
internal/
  version/           # build-stamped version/commit          [done]
  paths/             # config/data/socket path resolution     [done]
  config/            # viper load + canonical writer; typed settings   [done]
  db/                # pure-Go SQLite open + goose migrations  [done]
  rpc/orchdv1/       # generated gRPC contract (Orchd service) [done]
  embed/             # hugot/ORT loader behind an interface;
                     #   embedder_ort.go / embedder_noort.go; idle-unload  [done]
  rag/               # chunking, Qdrant HTTP client, ingest/search service [done]
  cli/               # cobra command tree; injectable orchd client factory [done]
  daemon/            # orchd Run(ctx) + gRPC handlers behind a ragService iface [done]
  hook/              # 7-event dispatch: auto-RAG inject, session capture, digest, events [done]
  session/           # transcript JSONL → record + text for RAG   [done]
  events/            # append-only JSONL event log per stream      [done]
  orchdclient/       # shared orchd Unix-socket dialer             [done]
  qdrant/            # native (Docker-free) qdrant provisioner     [done]
  install/           # HarnessAdapter + dry-run-by-default surgical installer [done: claude-code]
  model/             # embedding model provisioner (download on install)  [done]
  check/             # `orch check` preconditions verifier                 [done]
  supervisor/        # named, independently restartable orchd subsystems  [done]
  status/            # `orch status` whole-system report (human + JSON)    [done]
  sources/           # source registry + freshness scheduler (web/repo/owner) [done]
  prs/               # PR babysitter (gh poll → cache + events)          [done]
  redact/            # secret redaction before ingest                    [done]
  queue/             # go-workflows backend + task store + dispatcher [done]
  workflows/         # durable AgentTask workflow + activity    [done]
  mcpserver/         # RAG + queue MCP servers (stdio) → orchd RPCs [done]
  # --- planned ---
  sources/           # source registry + freshness workflows (repo/web/thread)
  sessions/          # per-harness transcript readers → records → RAG
  enrich/            # RAG enrichment + per-project filter resolution
  harness/           # HarnessAdapter iface + claudecode/ codex/ gemini/ impls
  check/             # precondition verifier
  status/            # liveness/health aggregation
skills/              # canonical SKILL.md (AI-agnostic) — //go:embed, rendered per harness
agents/              # canonical agent defs — //go:embed, installed for Claude+Gemini
build/lib/<platform>/ # staged libtokenizers.a (daulet release asset)
scripts/             # cover.sh + cover_by_file.py (per-file coverage gate)
```

**Core reorg:** the Python `hooks/`, `daemons/`, `mcp_servers/` *directories of scripts*
become **subcommands/goroutines of three thin binaries** (`orch`, `orchd`, `orch-hook`)
whose logic lives entirely in testable `internal/` packages. ~20+ scripts → 3 binaries.

**Testing convention:** every `Test*` is table-driven (a `tests := []struct{…}` slice +
`t.Run` loop, even for a single case). External deps sit behind interfaces (embedder
loader, daemon `ragService`, CLI client factory) so logic is unit-tested without the ONNX
runtime, a live Qdrant, or a running daemon. **Per-file coverage ≥80%** is enforced by
`make cover` (`scripts/cover.sh`); generated `*.pb.go` and the thin `cmd/*/main.go`
entrypoints are the only exclusions.

---

## 6. CLI command tree (Cobra)

```
orch install [--target claude-code|codex|gemini|all]   # prompts if omitted; bootstraps whole system
orch uninstall / check / reload
orch status [--json]                                    # full sitrep, always exits zero
orch restart [orchd|subsystems|<subsystem>] [--list]    # whole system, or one in place
orch config     get|set|list                            # persists (orch.db + YAML)
orch daemon     start|stop|restart|logs|status          # manages orchd via launchd
orch search <q> [--scope --tag --type --repo --owner]
orch ingest     url|path|text  [--scope --tag --type]
orch source     add|list|remove|refresh                 # unified registry
                add web <url> --refresh 24h
                add owner willesq --refresh 24h         # md-ingest, extensible orgs
orch source-type add email --retention 90d --redact pii # extensible taxonomy
orch project    add|list|remove                         # per-project hooks + filter key
orch repo       list|onboard|status
orch prs        [--blocked --ready --refresh --json]
orch qdrant     start|stop|status                       # respects rag.qdrant.mode
orch ui open
orch dispatch <body>                                    # → AgentTask workflow
```

---

## 7. Subsystem designs

### 7.1 Embedder / memory
`orchd` loads the ONNX model **lazily** on first embed request; **unloads** after
`embedder.idle_unload` (config) with no activity. CLI/hooks never load it — they RPC. This
is the whole answer to "don't claim memory per invocation." Parity gate: pin a Python
MiniLM reference vector and assert Go output matches (naive ONNX pooling can drift).

### 7.2 Orchestration (queue + workers)
Agent tasks are **durable workflows** over `workflows.db`: a task survives an `orchd`
restart *mid-run*, gets step-level retries, and the queue is the workflow engine's job
store. Cadence jobs are workflows with catch-up timers — **sleep-safety is inherited**.

### 7.3 Source registry + freshness (unifies several requirements)
Repos, web URLs, comms threads = rows in a `sources` registry, each
`{scope, tags, refresh_interval, last_fetched, status}`. One scheduler walks it:
- **success** → re-chunk/re-embed, replacing that URI's chunks.
- **absence/404/gone** → mark `status = stale|missing`, **retain existing chunks**
  (never delete), surface in `status` + session digest.

Covers: daily `.md` ingest of **willesq** repos, **extensible to other orgs/users**
(ingest `.md` with `repo` + `owner`/`org` metadata); web-source re-ingest cadence.

### 7.4 RAG taxonomy (extensible)
Chunks carry an **open** `source_type` + `scope` (`personal` / `work` / `project:<name>`)
+ arbitrary `tags[]`, all filterable in Qdrant payload. New types added via
`orch source-type add`, not code. Replaces the current hardcoded `VALID_SOURCE_TYPES`
closed set. Future: emails (personal vs work), slack messages, project-scoped items.

### 7.5 Enrichment + per-project
`orch project add` registers a cwd → `{extra_hooks, enrichment_filter}`. The
auto-RAG-enrichment path (on every prompt, on all 3 harnesses) always applies the active
project's filter key → clean, scoped results.

### 7.6 Harness adapters
`HarnessAdapter{ ConfigPaths, RegisterMCP, RegisterHooks(eventMap), InstallAgents,
InstallSkills, WriteMemoryFile, TranscriptReader }`. Findings
(`research://agentic-cli-extension-surfaces/2026-07-30-digest`):

| Surface | Claude Code | Codex | Gemini |
|---|---|---|---|
| MCP servers | `.mcp.json` / `~/.claude.json` | `~/.codex/config.toml` `[mcp_servers]` | `settings.json` `mcpServers` |
| Prompt-injection hook | `UserPromptSubmit` | `UserPromptSubmit` | `BeforeAgent` |
| Subagent defs | `.claude/agents/*.md` | ❌ (→ skills) | `.gemini/agents/*.md` |
| Skills/commands | `SKILL.md` | `SKILL.md` | `*.toml` (transpile) |
| Memory file | `CLAUDE.md` | `AGENTS.md` (native) | `GEMINI.md` (repointable) |
| Transcript capture | `projects/<enc>/<id>.jsonl` | `~/.codex/sessions/.../rollout-*.jsonl` | weak/opt-in |

MCP is the portable core; hooks exist on all three (prompt-injection everywhere);
`AGENTS.md` is the memory pivot. **Build risk:** hook stdin/stdout JSON schemas differ
per tool — the shared `orch-hook` binary needs per-harness payload parsing.

### 7.7 check vs status vs restart  **[done]**
- `orch check` — preconditions: onnxruntime dylib, Qdrant reachable, workflow store, `gh`
  auth, harness wiring, orchd socket. Pass/fail per requirement, **non-zero exit**.
- `orch status` — live health: `orchd` + every supervised subsystem (state, last tick,
  restart count) + the sitrep (Qdrant/registries/queue/harness-drift/preconditions).
  Always exits zero and degrades gracefully with `orchd` down; `--json` for scripts.
- `orch restart [component]` — no component bounces the `orchd` process via its service
  manager (and waits for it to answer again); a component name restarts that **subsystem in
  place** over the `Restart` RPC, keeping the socket and warm embedder. Every operation is
  idempotent. `orch restart --list` names the live components.

`orchd`'s long-lived pieces are `supervisor.Unit`s rather than bare goroutines: each owns a
child context, records a heartbeat per tick, and counts its restarts. That single mechanism
is what makes both the per-subsystem status detail and the per-subsystem restart possible;
it also replaced three hand-rolled ticker loops (sources, docs, PR babysitter).

### 7.8 Packaging & auto-update (DEFERRED — Homebrew work is a later scope)
Target: single binary with `//go:embed`'d assets (skills, agents, plist, migrations,
default config), distributed via `willesq/homebrew-tap`. Formula
`depends_on "onnxruntime"` supplies the one non-embeddable native lib; runtime resolution
via rpath to `/opt/homebrew/opt/onnxruntime/lib`. `brew services` manages the launchd
service. **Auto-update on upgrade** via version-stamped **self-reconciliation**: hooks
auto-update for free (binary invoked fresh per event); `orchd` watchdog re-execs when its
own binary changes; startup reconciliation re-extracts assets + re-syncs harness wiring +
rewrites plist on version-marker mismatch. The reconciliation logic is built in Phase 7;
only the brew-trigger wiring is deferred.

---

## 8. Requirements ledger (all captured)

| # | Requirement | Status / direction |
|---|---|---|
| 1 | Cobra CLI + Viper config | Locked. Canonical writer for persisted `config set`. |
| 2 | LLM must not claim memory per CLI call | `orchd` warm embedder + thin clients + idle-unload. |
| 3 | Extensible RAG key types (emails/slack/personal-vs-work/project) | Config-driven type registry + scope + tags. |
| 4 | Per-project custom hooks + RAG enrichment filter key | `orch project add`. |
| 5 | Everything configurable from CLI, persisted | `orch config` → `orch.db` + YAML. |
| 6 | More robust queue than files | go-workflows over SQLite. |
| 7 | Agents run in daemon mode off the queue | Workers in `orchd`. |
| 8 | willesq `.md` ingest daemon, daily (configurable) | Source registry + scheduler. |
| 9 | Sleep-safe cadence | go-workflows catch-up timers. |
| 10 | Works with Claude Code / Codex / Gemini; install selects target | `HarnessAdapter`. |
| 11 | Correct per-harness symlink paths + names; AI-agnostic skills | Adapter + canonical SKILL.md render. |
| 12 | md daemon extensible to other orgs/users | `source add owner <x>`; `owner`/`org` metadata. |
| 13 | Web sources re-ingest cadence; flag-not-delete on absence | Source registry refresh-or-flag. |
| 14 | `orchd` foundational daemon | Locked, foundational. |
| 15 | Qdrant docker/non-docker configurable | `rag.qdrant.mode`. |
| 16 | `orch check` verifies all required preconditions | Non-zero exit. |
| 17 | `orch status` includes daemon health | **Done.** orchd + per-subsystem liveness (state, last tick, restarts) via the `Status` RPC; `orch restart [component]` bounces the process or one subsystem in place. |
| 18 | `install` spins up the entire system | One-shot idempotent bootstrapper. |
| 19 | Single binary incl. text files | `//go:embed` + extract-on-install. |
| 20 | Homebrew install + management | Tap `willesq/homebrew-tap` — **deferred**. |
| 21 | Daemons/hooks auto-update on brew upgrade | Version-stamped self-reconciliation (logic Phase 7; brew trigger deferred). |

---

## 9. Old → new reorg map

| Python today | Go target |
|---|---|
| 7 hook scripts (`hooks/*.py`) | one `orch-hook` binary, event+harness dispatch |
| 7 daemon scripts + 7 plists | scheduler **jobs inside `orchd`**; one plist for `orchd` |
| 2 FastMCP servers | `orch mcp queue\|rag`, thin clients to `orchd` |
| `lib/*` | `internal/*` packages |
| markdown-file queue | go-workflows over SQLite (durable, leased, `sqlite3`-inspectable) |
| closed `VALID_SOURCE_TYPES` | config-driven type registry |
| hardcoded docker Qdrant | `rag.qdrant.mode` strategy |
| Claude-only wiring | `HarnessAdapter` × {claude-code, codex, gemini} |

---

## 10. Phased build plan

Each phase is independently shippable and testable. The spine is vertical: one real path
end-to-end early, then widen. Tests (table-driven, ≥1 edge + 1 failure per `internal/*`
package) land with each phase.

- **Phase 0 — Skeleton & contracts.** `go mod init`; `cmd/{orch,orchd,orch-hook}` +
  `internal/*` tree; Cobra root; Viper load + canonical writer; `internal/rpc` gRPC
  contract; SQLite bootstrap (`orch.db` migrations via embed). *Deliverable:* `orch --help`,
  `orch config get/set` persist, `orchd` starts and serves an empty socket.
- **Phase 1 — orchd core: embedder + RAG (vertical slice).** hugot/ORT embedder
  (lazy-load + idle-unload); Qdrant client (mode strategy); chunking + taxonomy;
  `Ingest`/`Search` over RPC. **Embedding parity gate** vs a Python reference vector.
  *Deliverable:* `orch ingest` + `orch search` through `orchd`; CLI stays memory-light.
- **Phase 2 — go-workflows spine + queue. [DONE]** Embedded go-workflows over
  `workflows.db` (pure-Go SQLite); `AgentTask` durable workflow + `ProcessActivity`;
  `queue.Dispatcher`; `orch dispatch` enqueues; a worker in `orchd.Run` drains it; results
  mirrored to orch.db `tasks`. *Verified:* dispatch → `kill -9 orchd` mid-flight → restart
  → workflow resumed and completed. (Resume waits out go-workflows' default lease-reclaim
  timeout, ~30s, for the dead worker's held task — tunable via worker/backend options.)
- **Phase 3 — MCP servers as thin clients. [DONE]** `orch mcp rag` (tools: rag_search,
  rag_ingest_text) + `orch mcp queue` (tool: queue_dispatch), each a stdio MCP server
  (mark3labs/mcp-go) that dials orchd and forwards to its gRPC. *Verified:* drove the stdio
  server through `initialize` → `tools/list` → `tools/call queue_dispatch`; the call reached
  orchd and created a durable workflow instance. Cross-harness common denominator.
- **Phase 4a — Hook parity [DONE except post-tool-use].** `orch-hook <event>` dispatches
  all 7 events with the crash-safety contract (never blocks a session). Verified via the
  real binary: user-prompt-submit (auto-RAG inject), session-end (transcript→RAG + record),
  stop/pre-compact (records), subagent-stop (event log), session-start (digest). Only
  post-tool-use is a documented safe no-op — its native-config/repo re-ingest + PR poke
  depend on the Phase 5 source registry / babysitter daemon. **This is the critical
  pre-install gate the user set.** Native-qdrant provisioner (Docker-free) also DONE +
  proven. Install decisions: binary-mode qdrant, backup→stop-docker→start-native cutover on
  install AND on `rag.qdrant.mode` change (#25/#26), Homebrew-first deps + native fallback
  for qdrant (#24), zprofile alias → built binary (#22/#23), surgical hook registration
  preserving Serena/Orca hooks (#28).
- **Phase 4b — `orch install` (Claude Code) [DONE + PROVEN].** `internal/install`:
  `Adapter` interface + `ClaudeCode` impl; `orch install [--apply] [--target] [--home]
  [--bin-dir]`. **Dry-run by default** (writes nothing, prints the plan); `--apply` backs up
  every touched file first. Surgical: registers the 7 `orch-hook <event>` commands + rag/queue
  MCP, replacing only orchestrator-owned entries; writes a managed `.zprofile` alias to the
  BUILT binary; writes the orchd launchd plist. Verified against a COPY of the real
  ~/.claude/settings.json in a temp HOME: 7 orch-hook entries wired, 7 Python entries removed,
  Serena (4) + Orca (10) hooks preserved, alias added w/ existing content kept, backups
  written, real ~/.claude untouched. Remaining before a REAL run: confirm MCP registration
  file (settings.json vs ~/.claude.json where serena lives), model + onnxruntime provisioning,
  `orch check`, qdrant-cutover wiring, codex/gemini adapters.
- **Phase 4c — HarnessAdapter for Codex + Gemini.** `HarnessAdapter` +
  `claudecode` impl; embedded-asset extraction + versioned symlinks; `orch-hook` with the
  `UserPromptSubmit`→auto-RAG-enrichment path; `orch install --target claude-code` wires
  MCP + hooks + agents + skills + memory file. *Then* codex + gemini adapters (SKILL.md→TOML
  transpile, AGENTS.md pivot). *Deliverable:* fresh session gets auto-RAG injection; `/orch-*`
  skills work.
- **Phase 5a — Source registry + freshness scheduler [DONE + PROVEN].** `internal/sources`:
  SQLite `sources` registry (web/repo/owner) + refreshers (web fetch→strip→ingest with
  404→missing/retain; owner/repo walk *.md → ingest with repo+owner tags) + a sleep-safe
  Scheduler (persisted last_fetched catch-up) run inside orchd. `orch source add/list/
  remove/refresh`. Also this batch: `orch daemon start/stop/restart` (launchctl) + `orch
  qdrant status`; **orchd owns the native qdrant** (one service brings up qdrant+embedder+
  workflows+scheduler, Docker-free, proven). Config audit: every config field is now
  consumed by code (`embedder.model` removed). Verified: willesq owner source →
  auto-ingested repo://willesq/plant-aware/README.md @ 0.654.
  Remaining Phase 5: PR babysitter + `orch prs`, PostToolUse full impl, `orch repo`,
  qdrant-cutover wiring.
- **Phase 5b — remaining parity [DONE]. FULL CODEBASE PARITY REACHED.**
  - **PR babysitter** (`internal/prs`): 2-phase gh poll → cache (`pr_babysitter_state.json`)
    + events on CI/review transitions; runs on cadence in orchd. `orch prs [--blocked
    --ready --json]` reads the cache only.
  - **PostToolUse full impl**: `gh pr {create,ready,…}` → babysitter sweep; Edit/Write of
    native config (~/.claude/*) → ingest as claude_config; *.md → repo_doc; all with secret
    **redaction** (`internal/redact`, reused by the source refreshers). All 7 hooks now fully
    functional.
  - **`orch repo add/list`** over the sources registry.
  - **Codex + Gemini adapters** (`internal/install/{codex,gemini}.go`) + `orch install
    --target all`. Codex: config.toml (TOML) MCP + Claude-style hooks.json. Gemini:
    settings.json MCP + Gemini-named hooks. (Hook JSON schemas for codex/gemini are
    best-effort per research — dry-run shows exactly what would be written before --apply.)
  - **`orch qdrant cutover`** (dry-run default): backup → docker stop → repoint Go engine at
    the existing storage/collection.
  Full command surface: check, config, daemon, dispatch, ingest, install, mcp, model, ping,
  prs, qdrant, repo, search, source, version. 21 packages, all files ≥80%, vet clean.
- **Phase 5-legacy — Source registry + freshness scheduler.** `sources` table + walker as
  go-workflows cadence jobs (catch-up timers); repo-md ingest (`source add owner willesq`,
  extensible orgs, `repo`+`owner` metadata); web-source refresh-or-flag (retain-on-404);
  re-chunk-on-refresh. *Deliverable:* web refresh + daily willesq md-ingest run and survive sleep.
- **Phase 6 — Sessions, per-project, taxonomy.** Per-harness transcript readers → session
  records → RAG; `orch project add` (per-project hooks + enrichment filter); `orch
  source-type add`. *Deliverable:* session capture on wired harnesses; in-project RAG scoped.
- **Phase 7 — check / status / self-reconciliation.** `orch check` (preconditions,
  non-zero exit) **[done]**; `orch status` + `orch restart` over supervised subsystems
  **[done]**; version-marker self-reconcile + `orchd` re-exec watchdog *(pending)*.
  *(Homebrew trigger deferred.)*

Cross-cutting: rewrite `docs/SPEC.md` to match as phases land.

---

## 11. Working defaults (override anytime)

- Module path `github.com/willesq/orchestrator`.
- gRPC over the Unix socket.
- `workflows.db` (go-workflows) + `orch.db` (settings + registries).
- Viper YAML for static knobs; `orch.db` for dynamic registries (sources/projects/types).
- Wipe + re-embed the index fresh.

---

## 12. Research references (RAG)

- `project://orchestrator-go-rewrite/decisions/embeddings` — embeddings decision.
- `project://orchestrator-go-rewrite/decisions/architecture` — all locked decisions.
- `research://go-onnx-embeddings-2026-07-30` — hugot / ORT / tokenizers survey.
- `research://go-local-embeddings/2026-07-30-survey` — non-ONNX embedding options.
- `research://go-task-queue-laptop/synthesis` — durable queue options.
- `research://go-durable-workflow-laptop/synthesis` — Temporal vs go-workflows vs DBOS.
- `research://agentic-cli-extension-surfaces/2026-07-30-digest` — Claude/Codex/Gemini surfaces.
