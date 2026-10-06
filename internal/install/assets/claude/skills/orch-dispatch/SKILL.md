---
name: orch-dispatch
description: |
  Use when the user asks to delegate a substantive piece of work to a specialist subagent — typically a multi-step task that you (the parent session) shouldn't tackle inline. Triggers include phrases like:
  "delegate this", "kick off a job to <X>", "spawn a <dev|ops|research|reviewer>", "run this in the background", "have <agent> handle this", "dispatch X to a subagent", "go fix X in <repo>", "open a PR for X", "investigate why <service> is failing", "research X and put it in RAG", "review PR <url>", "/orch-dispatch <task>".
  Activate also when the user describes work that obviously fits one specialist (e.g., a bug fix in a known repo → `dev`; a doc-crawl-into-RAG → `research`; an ops investigation → `ops`; a PR review → `reviewer`) and the request implies "do this without me watching every step".
allowed-tools:
  - Agent
  - Bash
---

<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# orch-dispatch — delegate substantive work to a specialist subagent

This is the auto-trigger equivalent of the `/orch-dispatch` slash command. Same flow, no slash needed: when the user describes work that should be handed off, invoke this and they get a one-line confirmation while a specialist runs in the background.

## Flow at a glance

0. **Worktree prep** (when applicable) — create a `gittree` worktree so `dev` specialists work in isolation.
1. **Dispatcher** (foreground) — Haiku-backed classifier writes the queue entry and returns routing JSON.
2. **Specialist** (background, parent-owned) — parent spawns the `dev`/`ops`/`research`/`reviewer` agent with `run_in_background: true`.
3. **Confirm** — one short sentence to the user.

The split between Phase 1 and Phase 2 exists because sub-subagents calling `Agent(run_in_background=true)` don't reliably survive their caller's exit. Spawning at the **parent** session level always survives.

## Phase 0 — Compose worktree info for `dev` dispatches

`dev` specialists ALWAYS work in a fresh `gittree`-managed worktree (per user's standing convention `feedback_always_use_gittree`). The `dev` agent itself runs `gittree` as its first step — you do NOT run the script from the parent. Your job here is to inject the branch name and canonical repo path into the task body so the dev agent has the values it needs.

### Decide whether to run Phase 0

| Specialist | Phase 0? |
|---|---|
| `dev` | **YES — always.** The dev agent's own Phase 0 will block-and-exit if the prompt omits the worktree section. |
| `ops` / `research` / `reviewer` | NO — read-only specialists don't write code. |

There is no opt-out for `dev`. If the user says "no worktree" for dev work, ask whether to (a) reroute to `general-purpose` instead of `dev`, or (b) provide a branch name and proceed normally.

### How to compose the worktree section

1. **Decide the branch name.**
   - Honor user-provided names verbatim.
   - Otherwise propose one in the user's semantic-commit style: `feat/<slug>`, `fix/<slug>`, `test/<slug>`, `chore/<slug>`, `docs/<slug>`, `refactor/<slug>`, `perf/<slug>`, `style/<slug>`. Make the slug mirror the dispatch slug.
   - When you proposed the name, surface it briefly so the user can redirect.

2. **Decide the canonical repo path.** In this order:
   - User explicitly stated it (a path, or a repo name like "some-repo" → `~/code/some-org/some-repo`).
   - Parent CWD is itself a canonical clone — confirm with `git rev-parse --show-toplevel` and check the path is not under `*.worktrees/`.
   - Inferred from task content + the standard layout `~/code/<org>/<repo>`.
   - Still unknown? STOP and ask. Don't guess paths.

3. **Compute the expected worktree path** without running anything: `<canonical-repo-parent>/<canonical-repo-name>.worktrees/<branch>`. The dev agent's own Phase 0 will create this path via `gittree`.

4. **Append a `## Worktree` section to the task body** before Phase 1:

   ```
   ## Worktree
   - Path: <worktree-path>
   - Branch: <branch>
   - Base: main
   - Canonical repo: <canonical-repo>

   The dev agent will run `gittree <branch>` from `<canonical-repo>` as its first step (Phase 0 of its own lifecycle). It must `cd` into `<Path>` for all subsequent commands.
   ```

You do NOT run `gittree` from the parent session. You do NOT need the parent CWD to be inside a git repo. The dev agent owns worktree creation entirely — your sole responsibility is to provide it the branch name and canonical repo via the prompt.

## Phase 1 — Run the dispatcher (foreground)

Spawn the `dispatcher` subagent (Haiku) to classify the task and enrich it with prior
RAG context. Wait for its return.

The dispatcher does **not** create a durable queue entry — `queue_dispatch` exists but
its worker is a stub that sleeps and writes `"processed: <body>"` without running
anything (`internal/workflows/workflows.go`). Treat the dispatcher purely as a
classifier and context-gatherer; the real handoff is the Phase 2 prompt.

```
Agent({
  description: "Dispatch task",
  subagent_type: "dispatcher",
  prompt: "Task body:\n<task body, with Phase 0 worktree section appended if applicable>"
})
```

Dispatcher returns a JSON block:

```json
{
  "slug": "<...>",
  "agent": "dev|ops|research|reviewer",
  "context_block": "...",
  "source_uris": [...],
  "summary": "<short>"
}
```

If dispatcher reports `status: blocked` (e.g., MCP not loaded, empty body), surface the reason and stop.

## Phase 2 — Spawn the specialist (background, parent-owned)

Parse the JSON. Then YOU (the parent) call:

```
Agent({
  description: "<summary from JSON>",
  subagent_type: "<agent from JSON>",
  run_in_background: true,
  prompt: "Task ID: <slug>\n\n<the FULL task body, including the Phase 0 ## Worktree section if any>\n\n## Prior context\n<context_block from JSON>\n\n## Source URIs\n<source_uris from JSON>"
})
```

This survives reliably as a session-level background agent.

**The prompt is the only channel.** There is no queue entry for the specialist to
read — the queue worker is a stub and a queue entry carries only the body, so a task ID
alone tells the specialist nothing. Everything it needs to do the work must be
inlined here. Don't pass a slug and expect it to go look the task up; it can't.

Likewise, results come back in the agent's **return message**, not via a queue
update. When the specialist finishes, the `subagent-stop` hook appends a
`complete` event to `~/.local/share/orch/events/<your session id>.jsonl` with a
200-character summary — that and the return message are the whole reporting path.

## Phase 3 — Confirm to the user

One short sentence. With Phase 0:

> Dispatched: `<slug>` → `<agent>` (background) on branch `<branch>` at `<worktree-path>`. I'll surface its report when it finishes. Submit more tasks anytime.

Without Phase 0:

> Dispatched: `<slug>` → `<agent>` (background). I'll surface its report when it finishes. Submit more tasks anytime.

## When NOT to use this skill

- The user is asking a **question** (use `orch-rag-query` instead) — not asking you to do work.
- The user's task is small enough to do inline in the parent session in <2 minutes (e.g., "show me file X", "run pytest", "explain this function"). Dispatching adds overhead; only worth it for substantive tasks.
- The user explicitly says "do it yourself" / "don't delegate" / "stay in this session".
- The work depends on iterative back-and-forth with the user — specialists run autonomously.

## Hard rules

- ALWAYS run Phase 0 before Phase 1 when dispatching `dev` work. There is no opt-out — if the user wants no worktree, reroute to `general-purpose` instead of `dev`.
- ALWAYS run Phase 1 before Phase 2. Don't skip the dispatcher and try to classify yourself.
- ALWAYS spawn the specialist with `run_in_background: true` in Phase 2. Never foreground.
- ALWAYS inline the complete task body into the specialist prompt. The slug is a label
  for your own tracking, not a lookup key — nothing can retrieve a task by ID.
- NEVER run `gittree` from the parent session. The dev agent owns worktree creation; your job is only to compose the `## Worktree` section.
- NEVER do the work yourself in the parent session once dispatched. You're the orchestrator.
- NEVER call `mcp__queue__*` or `mcp__rag__*` tools from this skill — those belong to the dispatcher (Phase 1) and the specialist (after Phase 2).
- NEVER guess the canonical repo path. If unsure, ask.
- If the dispatcher reports `MCP servers not loaded`, tell the user to restart Claude Code; don't fall back.
