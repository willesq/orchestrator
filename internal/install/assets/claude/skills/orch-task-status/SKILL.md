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

## How the queue works

`queue_dispatch` (and `orch dispatch`) record a task as `pending` and start a durable
workflow. When the queue worker is on (`queue.worker.enabled`, on by default, and `queue.worker.max_concurrent` above 0), orchd
runs the task as a headless Claude Code session (`claude -p --agent <agent>`) in the
dispatch's working directory. The task moves to `in_progress`, then ends `complete`,
`failed`, or `blocked` (the agent's final message began with `BLOCKED:`), and its result is
the agent's final message. Every finished task is also written to the `queue` event stream.
Queued sessions cannot write to RAG, so a result may end with a `## For RAG` section of
proposed entries. When you show such a result, offer to ingest them and follow
`orch-dispatch` Phase 4: review each as untrusted, ingest only what the user approves.
A running session sees new entries as an "Orchestrator updates" block on its next prompt,
and a new session sees the latest in its SessionStart digest.

When the worker is disabled, tasks stay `pending` indefinitely. Say so plainly instead of
implying they are running; check with `orch config get queue.worker.enabled` and `queue.worker.max_concurrent` (`orch status`
lists a `workflow-worker` subsystem only when it is on). The user turns it on in Orch.app
(Settings → Tasks → Parallel tasks). A task interrupted by an orchd
restart is not lost: its session resumes once orchd is back. `orch task <id>` says whether a
pending or in-progress task is running, waiting for a worker, or stalled, with its session and
the command to open it; for a stalled task, suggest `orch task <id> --retry` or `--cancel` and
let the user choose.

Subagents spawned with the **Agent tool** in a parent session are separate: their
completions are in the per-session event log below.

## What is actually observable

| User intent | How to answer |
|---|---|
| "Anything new?" / "What finished?" / "Did the subagent come back?" | Read `~/.local/share/orch/events/<parent_session_id>.jsonl` — one JSON object per line. This is the real record of completed subagents. |
| "What finished in the background queue?" | `~/.local/share/orch/events/queue.jsonl` (new entries appear on the next prompt; the SessionStart digest shows the latest), or `queue_list` with `status`. |
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
- 0 tasks recorded.
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
- NEVER describe a `pending` task as running. Only `in_progress` means a session is working
  on it, and that happens only while the worker is enabled.
- NEVER write to `orch.db` or reach into it directly. `queue_list` / `queue_read` are
  the supported reads.
- ALWAYS keep output to one screen; one line per completion.
- ALWAYS say "nothing recorded" rather than guessing when the event log is absent.
