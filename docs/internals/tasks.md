# Tasks, routing and PRs

Packages: `queue`, `workflows`, `agentrun`, `models`, `routing`, `evidence`, `prs`, `taskfmt`.

## Lifecycle

1. **Dispatch** (`orch dispatch`, `queue_dispatch`, RPC `Dispatch`): validate (agent
   `^[a-z0-9][a-z0-9_-]{0,63}$`, body ≤ 64 KiB, absolute cwd), resolve the route, refuse past
   `queue.max_active` pending + running, redact the body, record git HEADs of the task's
   repository, insert the ledger row (`tasks`, status `pending`), then create the durable workflow.
2. **Workflow** (go-workflows on SQLite `workflows.db`): `AgentTask` runs `StartActivity` (status
   `in_progress`) then `RunActivity` with one attempt (a failed run may already have pushed, so it
   is never retried), saves the outcome to the ledger, and appends a `queue` event naming the task
   and status, never its result. Inputs (including the route) are fixed at dispatch, so a resumed
   task keeps its model.
3. **Worker:** runs only with `queue.worker.enabled`; `max_concurrent` parallel activities. Stopping
   cancels sessions; after restart, an interrupted task's lease lapses and it resumes its own
   session.

## Claude Code runner

```
claude -p --output-format json --permission-mode <mode>
  --disallowedTools mcp__queue__queue_dispatch,Bash(orch dispatch:*)
  [--allowedTools=<rules>] --agent <agent> [--model <slug>]
  (--resume <sid> | --session-id <sid>)
```

- **Session ID:** UUIDv5 of the task ID under the namespace
  `6f0d3f52-6c3b-4c1e-9a4e-2d5b9f1c7a10`, so a restarted task resumes (`--resume`) when its
  transcript exists under `~/.claude/projects/*/`.
- **Permission mode:** `auto`, `dontAsk`, `acceptEdits` or `plan` (each decides without a human);
  anything else fails the task.
- **Allowed tools:** `queue.worker.allowed_tools.claude`, joined into one argument; rules with a
  comma or leading `-` are dropped.
- **Prompt** on stdin: the task body with instructions that the run is headless, the final message
  is the result, and a blocked run starts with `BLOCKED:`. Resuming sends a continue prompt.
- **Directory:** the dispatch cwd, or a private scratch directory `<data>/tasks/<id>`.
- **Environment:** orchd's, minus the vector-store key, with `ORCH_TASK_ID` set and `PATH` =
  the CLI's directory, Homebrew, `~/.local/bin`, `~/go/bin`, then orchd's `PATH`.
- **Process:** its own process group; timeout `queue.worker.timeout` (60 min); on cancel SIGTERM the
  group, wait 10 s, SIGKILL. Output is captured to unlinked temp files (≤ 8 MiB).
- **Outcome** from the JSON result: `failed` on `is_error`, a non-zero exit or unreadable output;
  `blocked` on a `BLOCKED:` prefix; else `complete`. Recorded: cost, turns, duration, models used,
  session ID, PR URLs in the result. Results are redacted and capped at 64 KiB.

Before running, the route is checked again: a model no longer routable runs on the agent's own
model, and a Codex route with Codex disabled runs on Claude.

## Codex runner

`codex exec [resume] --json --skip-git-repo-check --ignore-rules -o <last-message>` with
`approval_policy="never"`, `sandbox_mode` `workspace-write` (or `read-only`; full access refused),
network off by default, `notify=[]`, `ORCH_TASK_ID` set in the shell environment, optional model and
reasoning effort. Guards:

- **MCP servers:** every one is disabled except allow-listed names (`orch_rag`) whose command
  resolves to the orch binary installed beside orchd; listing failure fails the run.
- **Writable roots:** granted only for a linked worktree (its git dir and the shared objects,
  refs and logs); a plain clone gets none.
- **Git state:** refs, `gitdir` and worktree config are snapshotted and restored after the run; any
  tampering fails the task.
- **Project config:** a `.codex` directory between cwd and home is refused unless allowed.
- The Codex thread ID is kept in `<data>/codex/<id>.thread` for resuming.

## Verification

For a `complete` task whose repository's main checkout is listed in `queue.worker.verify.repos`:

1. Find checkouts of that repository whose HEAD moved since dispatch (`git worktree list`).
2. Detect a test command: a make `test` target (`GNUmakefile`, `makefile`, `Makefile`, first found),
   else `go test ./...` for `go.mod`, else `cargo test`, else `npm test` if `package.json` has a
   real test script. Python is not guessed. At most 5 checks.
3. Run with a minimal environment (home, user, shell, locale, toolchain caches and paths; no
   tokens or SSH agent), timeout 15 min, own process group.
4. On failure, resume the session once (`verify.retries`) with the output's redacted 4 KiB tail in
   a fence labelled untrusted data; if checks still fail, the task is `failed` with the output.

Recorded as `passed`, `failed` or `skipped: <reason>` (unlisted repo, nothing changed, no command).

## Model registry

- **Providers:** `claude-code` (enabled initially) and `codex` (disabled). A provider whose CLI is
  not on `PATH` is `unavailable` and returns to its prior state when found.
- **Discovery** reads only model fields: Claude Code's aliases `opus` (frontier), `sonnet`
  (balanced), `haiku` (fast), `fable` (untiered), plus `~/.claude.json`
  `additionalModelOptionsCache`; Codex's `~/.codex/models_cache.json` (slug, name, default and
  supported reasoning efforts, visibility).
- **States:** discovered models are inert (`discovered`) until enabled; aliases start enabled.
  Hidden models cannot be enabled. Refresh keeps your state and tier.
- **Routable** = enabled and not hidden.

## Routing

Kinds: `code`, `review`, `docs`, `research`, `ops`, `classify`, `test`, `adversarial`.

**Ranking** for a kind, over routable models of providers listed in `routing.providers`:

- `prior = 1 − |tier − wanted tier| / 3` (frontier 0, balanced 1, fast 2; untiered distance 3).
- Evidence per (kind, model): each task counts once, by your rating, else the fate of the PRs it
  opened, else `bad` if it failed; completed tasks with neither carry no verdict.
  `observed = (good + 0.5·rework) / n`.
- Below `routing.min_evidence` (5) outcomes, `score = prior`; otherwise
  `w = n / (n + min_evidence)`, `score = w·observed + (1 − w)·prior`.
- Ties: provider order, aliases before cached IDs, slug. Keep `routing.ranks` (3). Effort only if
  the model supports it. Confidence: low < 5, medium < 20, high.

**Table** (`routing`): `apply` writes a proposal (kinds with a pinned row are kept), `set` pins a
kind, `clear` drops it, `rollback` restores the previous generation (50 kept in
`routing_history`). Dispatch uses the first table route whose model is still usable, else a live
proposal without evidence, else the agent's own model.

**Evaluator** (weekly): runs discovery, computes a proposal, and reports new models and changes
in the `routing` event stream, once per distinct proposal. It applies only with
`routing.auto_apply`, never when discovery failed or a change removes a route.

**Guards:** routing, model, provider and rating changes refuse inside a queued task
(`ORCH_TASK_ID`), so a session steered by an untrusted repository cannot redirect later work or
grade itself. orchd enforces the RPC side itself (`Dispatch`, `RateTask` and the other
operator-only RPCs; see [daemon.md](daemon.md#queued-callers)). Routing and the model registry
are written to `orch.db` by the CLI, so for those the CLI check is the only one.

## PR outcomes (evidence)

Every sweep, up to 20 completed tasks (≤ 30 days old, rechecked every 6 h, ≤ 50 `gh` calls) have
their PRs checked with `gh pr view --json state,createdAt,author,commits`. A PR counts only if its
author matches your `gh` login (case-insensitive) and it was created after the task was
dispatched. Merged as left → `good`; merged after further commits (other than `Merge …` commits)
→ `rework`; closed → `bad`; any still open → undecided. A task takes its worst PR's outcome; a task
with more than 10 PR URLs stays undecided.

## GitHub scopes

`github_scopes` in `orch.db` (`internal/github`) records what orch monitors on GitHub:

- **Defaults:** the user's own account, enabled by default, and each org they belong to, disabled by
  default.
- **Discovery:** it runs through `gh` (`gh api user`, `user/orgs`, `user/teams`) and is cached for
  10 minutes. It adds rows only for accounts that have none, so the user's choices survive
  rediscovery.
- **Each scope has:**
  - an `items` mode: `involving` (authored, assigned, mentioned, or review requested of the user or
    their team) or `all` (every open PR and issue);
  - a repo mode: `all` (every repo the user can see), `include` (only the listed repos) or
    `exclude` (all but the listed repos).
- **Who can change them:** clients change scopes only through `SetGitHubScope`, which accepts only
  discovered accounts. `orch github` and the menu bar app's GitHub tab both use these RPCs.
- **Guards:**
  - Logins are case-insensitive and at most 39 characters. Repo names exclude `.` and `..`.
  - An org the user has left keeps its row but cannot be turned on.
  - `ListGitHubRepos` serves only the user's account and their orgs, so a client cannot spend the
    user's rate limit on other owners.
  - A forced refresh is honoured at most every 30 s.
  - gh's error text is cut to 512 bytes, with control characters removed.
- **Queued tasks:** `orch github enable|disable` refuses to run inside a queued task, and orchd
  refuses `SetGitHubScope` from one (see [daemon.md](daemon.md#queued-callers) for the limits).

## PR babysitter

The `pr-babysitter` unit watches the user's enabled GitHub scopes for open PRs and issues.

**When it sweeps:**
- Every poll (10 min), and on first start.
- On request: the `handoff/pr-sweep` file, written after `gh pr` mutations and by `SetGitHubScope`.
  A requested sweep runs at most a minute after the previous sweep. An earlier request waits, rather
  than being dropped, so a scope change shows within about a minute.
- After GitHub reports a rate limit, sweeps pause for 10 minutes.

**Searches:**
- Three cover everything involving the user, whatever the number of scopes:
  - `gh search prs --involves=@me`
  - `gh search prs --review-requested=@me`, which includes the user's teams
  - `gh search issues --involves=@me`
- A scope watched for everything adds `--owner=<scope>` searches for its PRs and issues. At most 5
  such scopes are searched (alphabetically); further ones are watched only for items involving the
  user.
- Every search asks for open items, newest first, at most 100 each. That keeps the number of
  searches nearly fixed against GitHub's limit of 30 a minute.
- Hits outside the enabled scopes, or excluded by a scope's repo picks, are dropped. So the tray and
  `orch prs` cover only what the user opted in to.
- Only `https://github.com/` item URLs are kept, and scope logins are checked again when read.

**Each item records:**
- its kind (`pr` or `issue`), number, author and update time;
- its reasons:
  - `authored`: the user is the author;
  - `assigned`: the user is an assignee;
  - `review_requested`: from that search;
  - `involved`: mentioned or commented, otherwise;
  - `all_open`: from an everything search.

CI, review decision and mergeability are fetched with `gh pr view` only for PRs the user authored
or was asked to review, because only those can need the user. If the scopes can't be read or any
search fails, the sweep fails and the cache is kept.

CI rolls up to `none`, `failure`, `pending` or `success`. State is cached in
`<data>/pr_babysitter_state.json`, written atomically (temp file, then rename) and owner-only.

**Events:** new items and changes to the user's own PRs, including a new reason, append
`pr-babysitter` events: `blocked` for CI failure or changes requested, `complete` for green and
approved, else `info`. Other people's PRs and all issues get no events. Their titles are written by
other people, and events reach every agent session through the SessionStart digest.

**Who sees what:**
- `ListPRs` serves the cached PRs; `ListGitHubItems` serves PRs and issues, for the app.
- `orch prs`, which agents read, shows only PRs the user opened or was asked to review
  (`prs.Trusted`). The rest of the cache has titles anyone who can open a PR wrote, so only the app
  shows it. `orch prs` also strips control and bidi characters from titles.
- None of them call gh. Both RPCs answer `Unavailable` when the file exists but cannot be parsed, rather than
an empty list.

**Attention** (`prs.Attention`) says why a PR needs the user (issues never do). It picks the most urgent reason that
applies, in this order:

1. `ci_failed`, `changes_requested` or `conflicts` on the user's own PR.
2. `review_requested` on someone else's PR.
3. `ready_to_merge` on the user's own PR: green, approved, not a draft.

Drafts never need attention. Cache entries written before reasons existed count as the user's
own. `orch prs --needs-me`, `ListPRs` and the menu bar app all use this classification.
