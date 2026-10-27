# Background tasks and model routing

## Background tasks

`orch dispatch "<task>"` or the `queue_dispatch` MCP tool records a durable task. The worker
runs it as a headless session (`claude -p --agent <agent>`, default `dev`) in the directory it
was dispatched from. The agent's final message is the result: `complete`, `failed`, or `blocked`
if it starts with `BLOCKED:`. Read results with `orch tasks`, `orch task <id>`, or `queue_read`;
`orch tasks --stats` summarizes cost, run time and success per agent.

The worker is off by default, since it runs agents unattended:

```sh
orch config set queue.worker.enabled true
orch restart
```

| Setting | Default | Meaning |
|---|---|---|
| `queue.worker.permission_mode` | `auto` | `auto`, `dontAsk`, `acceptEdits` or `plan`; anything else, including `bypassPermissions`, fails the task |
| `queue.worker.timeout` | `60m` | Per task |
| `queue.worker.max_concurrent` | `1` | Tasks running at once |
| `queue.max_active` | `20` | Pending plus running; dispatch fails beyond it |
| `queue.worker.allowed_tools.claude` | read-only set | Permission rules granted without asking |
| `queue.worker.codex.enabled` | `false` | Also run tasks routed to Codex |

How tasks behave:

- **Verification.** For repos listed in `queue.worker.verify.repos`, each checkout the task
  changed is tested with its own command (make `test`, `go test ./...`, `cargo test`, or the
  `package.json` script) in a minimal environment. On failure the session is resumed once to fix
  it (`queue.worker.verify.retries`); if it still fails, so does the task.
- **No retries.** A failed task may already have pushed or opened PRs, so it is not rerun.
- **Restarts resume.** Interrupted tasks resume their own session once `orchd` is back.
- **Bounded fan-out.** Queued sessions can't dispatch more tasks; `queue.max_active` is the
  hard limit.
- **Isolation.** Sessions don't receive the vector store's API key. Task bodies and results are
  redacted and capped at 64 KiB, and the session digest shows a task's status, never its result
  text.

## Model registry

```sh
orch model refresh                       # discover from each CLI's local cache
orch model list [--provider codex] [--state enabled]
orch model enable codex/gpt-6.1-sol
orch model tier codex/gpt-6.1-sol fast   # frontier | balanced | fast
orch provider enable codex
```

Discovery reads only model fields from each CLI's local cache, never account data. A discovered
model stays inert until you enable it, so a vendor release can't redirect your work. Your
enable/disable and tier choices survive refreshes. Claude Code is enabled by default; Codex is
not.

## Routing

A task can name a kind: `code`, `review`, `docs`, `research`, `ops`, `classify`, `test`,
`adversarial`. The routing table ranks enabled models per kind.

```sh
orch routing evaluate [--explain code]    # propose a table; writes nothing
orch routing apply | rollback | show
orch routing set docs claude-code/sonnet  # pin a kind
orch dispatch --kind code "<task>"
orch dispatch --model claude-code/opus "<task>"
```

- **Policy is config.** `routing.kinds.<kind>` sets each kind's preferred tier (frontier for
  code, review and adversarial; balanced for research, docs, ops, test; fast for classify).
  `routing.providers` orders and limits providers.
- **Evidence moves ranks.** A task's outcome comes from your rating
  (`orch task <id> --rate good|rework|bad`), else from what happened to the PRs it opened, else
  `bad` if it failed. After `routing.min_evidence` outcomes (default 5), a model's success rate
  is blended into its rank. Speed never counts.
- **Proposals, not surprises.** The `routing-evaluator` runs weekly and reports new models and
  proposed changes in the session digest. It applies nothing unless `routing.auto_apply` is on,
  and `orch routing rollback` undoes any change.
- **Fixed at dispatch.** The model is chosen when a task is queued and kept if it resumes.
- **Only you change it.** Routing, model and rating commands refuse inside a queued task, and
  orchd refuses ratings, further dispatches and RAG writes from task sessions. A task's report
  ends with a `## For RAG` section of proposed memories; the session that reads the result
  reviews them and ingests what you approve.
- **Agents can ask.** The `model_recommend` MCP tool returns the model for a kind and why.
