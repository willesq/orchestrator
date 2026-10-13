---
name: orch-task-status
description: |
  Use when the user asks about the orchestrator's background work — subagents that have finished, what a past session did, or the state of the dispatch queue. Triggers include:
  "what's running", "what's in flight", "what finished", "any updates", "anything new", "did <task> finish", "show me the queue", "task status", "what subagents ran", "what did dev do recently", "show me recent completions".
  Activate also when a subagent completion lands in `~/.local/share/orch/events/<parent_session_id>.jsonl` and the user asks about it indirectly ("anything new?", "any updates?").
allowed-tools:
  - mcp__queue__queue_list
  - mcp__queue__queue_read
  - Bash
  - Read
---

<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# orch-task-status — What the orchestrator has been doing

Read-only. Never mutates anything.

## Read this before answering: the queue does not execute tasks

`queue_dispatch` (and `orch dispatch`) record a task and run a durable workflow that
**sleeps three seconds and writes the literal string `"processed: <body>"`** as the
result — see `internal/workflows/workflows.go`. No agent is spawned. No work is done.

Task *status* is real: a dispatched task is recorded `pending`, moves to `in_progress`
when the worker picks it up, and ends `complete`. What is not real is the work — the
result is always the placeholder. A dispatched task is a row in a ledger, not a
running job.

Never tell the user a dispatched task is "running," "in progress," or "being worked
on." If they dispatched something and are waiting on it, say plainly that the queue
worker is a stub and the task will only ever come back as `processed: …`.

Background work that *does* really happen goes through the **Agent tool** in the
parent session — those are the completions in the event log below.

## What is actually observable

| User intent | How to answer |
|---|---|
| "Anything new?" / "What finished?" / "Did the subagent come back?" | Read `~/.local/share/orch/events/<parent_session_id>.jsonl` — one JSON object per line. This is the real record of completed subagents. |
| "What's the queue look like?" / "Any tasks?" | `mcp__queue__queue_list` (filters: `status`, `agent`, `limit`), or `orch tasks` from a shell. |
| "Status of a specific task" | `mcp__queue__queue_read` with the task ID, or `orch task <id>`. |
| "How many tasks overall?" | `orch status` — the **data** section's `tasks` line gives per-status counts. |
| "What did I work on earlier?" / "What happened last session?" | Not this skill — that's `orch-rag-query` over `session_record` / `session_transcript`. Hand off. |
| "Is the orchestrator healthy?" | `orch status` (full report) or `orch check` (pass/fail gate). |

## The event log

```bash
cat ~/.local/share/orch/events/<parent_session_id>.jsonl
```

Each line: `{"kind":…, "source":…, "summary":…, "timestamp":…}`.

- `source` is the subagent type (`dev`, `reviewer`, …) or a daemon name (`pr-babysitter`).
- In practice the `subagent-stop` hook only ever emits `kind: "complete"`; the
  `pr-babysitter` stream emits `info` and `blocked`. Don't promise `progress` or
  `failed` events — nothing writes them.
- `timestamp` is frequently an empty string. Fall back to line order, which is
  append-only and therefore chronological. Never invent a time.
- The file only exists once a subagent has finished under this parent session. A
  missing file means "nothing has completed yet," not an error.

There is also a shared `pr-babysitter.jsonl` stream. Route PR questions to
`orch-pr-status` instead of reading it here.

## Output format

Lead with completions, because that's the only real signal:

```
## Orchestrator

### Completed this session (2)
- reviewer — "Reviewed willesq/orchestrator#16; two blocking findings on the …"
- dev — "Opened PR #17 with the drift fixes; tests green."

### Dispatch ledger
- 0 tasks recorded. (Note: the queue worker is a stub — dispatched tasks are not executed.)
```

If both are empty: "No subagent completions recorded this session, and the dispatch
ledger is empty."

## When NOT to use this skill

- External trackers (Linear, JIRA, GitHub issues) — not ours.
- "What did I work on this week?" — that's `orch-rag-query`.
- PR state — that's `orch-pr-status`.
- Cancelling or modifying a task — there is no mutation surface at all. Tell the user
  that rather than reaching into the DB to change rows.

## Hard rules

- NEVER call `queue_update` or `queue_archive`. There is no mutation surface: task state
  belongs to the workflow engine. `queue_dispatch`, `queue_list` and `queue_read` are the
  only queue tools.
- NEVER describe a dispatched task as executing. See the stub warning above.
- NEVER write to `orch.db` or reach into it directly. `queue_list` / `queue_read` are
  the supported reads.
- ALWAYS keep output to one screen; one line per completion.
- ALWAYS say "nothing recorded" rather than guessing when the event log is absent.
