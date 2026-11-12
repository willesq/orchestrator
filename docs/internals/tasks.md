# Tasks, routing and PRs

Packages: `queue`, `workflows`, `agentrun`, `models`, `routing`, `evidence`, `prs`, `reviews`, `taskfmt`.

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
   - **Interrupted runs:** go-workflows records any error an activity returns, even one from a
     stopping worker, as that activity's failure. So `AgentTask` recognizes the runner's
     interruption (`workflows.InterruptedMarker`, matched by text since the error arrives
     serialized) and runs `RunActivity` again, up to 20 times. The next worker resumes the
     session. Before this, the workflow failed and the ledger kept the task `in_progress` with
     nothing running it (issue #100).
3. **Worker:** runs only with `queue.worker.enabled` (default `true`) and `max_concurrent` above 0;
   `max_concurrent` parallel activities, at most 50 ([config.md](config.md#queue-limits)). Stopping cancels
   sessions. Tasks given the same working directory are not isolated from each other: running several at
   once in one checkout can interleave their changes, so give each its own worktree.
4. **Reconcile on start:** each time the worker starts, before it takes any work,
   `queue.Control.Reconcile` checks every `pending` or `in_progress` task. Three guards apply:
   - **New tasks:** a task dispatched under a minute ago is skipped, since its row is written
     before its workflow.
   - **Stale tasks:** a `pending` task over a day old whose run never started (dispatched while no
     worker ran, say) is cancelled and marked `interrupted`, not run against today's repos.
   - **Retry cap:** after 3 automatic retries an interrupted task is left `interrupted` for the
     user, so a worker that keeps crashing cannot rerun it without end. A task whose newest run is still active is left alone. The rest
   are settled by how their run ended:

   | Run ended | Result |
   |---|---|
   | With the interruption error (a task stranded by an older orch) | Run again from its recorded input, as a retry |
   | Failed with any other error | `failed`, with that error |
   | Finished without recording a result | `interrupted`, with the retry command |
   | No run at all | `interrupted`, with the retry command |
5. **Runs:** a task's first run is the workflow instance named after it; a retry is
   `<id>#retry-<8 hex>`. `queue.Runs` finds a task's runs in the backend's own `instances` table
   (read-only), since every backend call needs an execution ID and there is no lookup by name;
   tests run that query against the real backend of the pinned go-workflows version. A task's
   input comes from the newest run whose history records its start (a run no worker has picked
   up yet has none).
6. **Control** (operator-only RPCs, refused to queued tasks):
   - `RetryTask` starts another run from the recorded input, unless the newest run is active. It
     counts against `queue.max_active` like a dispatch.
   - `CancelTask` stops a session running here (the runner records the task `cancelled`). A run
     still waiting for a worker is cancelled in the backend, and a task with no active run is
     marked `cancelled`. Its result is `cancelled by the user`; orchd's own cancels
     (`Control.CancelFor`, such as a PR review whose PR moved on) record their reason instead.
     A cancelled session's scratch directory (`<data>/tasks/<id>`) is removed once the session
     has exited, with whatever it cloned or downloaded there; a dispatch cwd is never removed.
   - **Ordering:** `Reconcile`, `Retry` and `Cancel` run one at a time, so two can never start
     two runs of one session.
   - **Cancelled is final:** the ledger never moves a `cancelled` task to another status. A run
     that races the cancel checks the ledger before it starts, and the worker refuses a second
     live session for a task.
7. **Liveness:** `GetTask`/`ListTasks` attach a `TaskRun` to pending and in-progress tasks:

   | State | Meaning | Also shows |
   |---|---|---|
   | `running` | a session is live in this worker | its PID and start time |
   | `waiting` | its run is queued for a worker | |
   | `stalled` | nothing owns it | |

   Every state also carries the session ID, the working directory, the time the session
   transcript last changed, and a `claude --resume` command to open the session yourself.
   - **Safe to paste:** the command includes `cd '<dir>'` only when the path is plainly safe
     (`/` followed by letters, digits, spaces and `._/+@:,=~-`). Any other directory is left out,
     since fish reads `\'` inside single quotes and csh expands `!` there. The directory is shown
     on its own line either way. The CLI strips control characters from what it
     prints.
8. **Live updates (`WatchTasks`):**
   - **Sources:** the ledger (`SQLTaskStore.OnChange`, after any write that changed a row:
     dispatch, status, result, end) and the runner (`Runner.OnLive`, when a session starts, its
     process starts or stops, or it ends) publish the task's ID to orchd's `taskHub`. So does a
     user's rating. The PR-outcome sweep does not, so its label shows on the next snapshot or
     refresh.
   - **Stream:** a snapshot, the same as `ListTasks`, is sent first. Each later message is either
     the changed tasks (IDs arriving within 100 ms are batched) or a `tick` every 15 s. A tick
     carries only each pending and in-progress task's ID, status, `updated_at` and run, and no
     text, so it stays small however many tasks are open. Clients merge it into rows they hold.
   - **Slow clients:** each stream buffers 256 IDs. One that falls further behind gets a fresh
     snapshot rather than silently missing changes.
   - **Limits:** at most 16 streams are open (`ResourceExhausted` beyond). Streams end with
     `Unavailable` when orchd begins shutting down, since gRPC's graceful stop waits for every open
     RPC and an app holding a stream would otherwise keep orchd from exiting. Shutdown also forces
     a stop of anything still open after 5 s. A client that goes away ends its stream cleanly
     (no error), even when it leaves while a snapshot, change or tick is being read; a ledger
     failure while the client is still connected ends the stream with that error.
   - **Access:** operator-only. Queued tasks are refused it and use `ListTasks` and `GetTask`.

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

## PR landings

Package `landing`. A hand-off that may not push ends with a proposal rather than a command for the
user to paste. Its brief asks for a commit on the PR's current head, left checked out in a worktree
under the home directory, and for the result's last non-empty line to be
`PROPOSED: <absolute worktree path> <40-hex commit>`. Nothing else about a task changes: it finishes
`complete` like any other, and a result without the line proposes nothing.

1. **Reading (`GetPRLanding`):** needs a `complete` task with a `PR: <url>` line and a proposal. orchd
   copies the commit once into `<data>/landings/<task>.git`, a bare repository with no hooks and none
   of the worktree's config, by fetching the worktree's `HEAD` and checking it is the reported commit.
   Later commands run there, with replacement refs off. An agent runs as the same user and could write
   there too, so a repository with any other config, or with a planted `.git`, hooks, replacement refs,
   grafts or a commit graph, is thrown away and rebuilt on reading, and refused on pushing. Pushing also
   runs a full `git fsck` first, so a replaced object cannot show one change and send another. Commands
   name the repository with `--git-dir`. The fetch needs git 2.45.1 or newer,
   whose `upload-pack` refuses a lazy fetch that a hostile source could use to run a command. orchd
   then reads the PR with `gh pr view` and compares:

   | State | Meaning |
   |---|---|
   | `proposed` | descends from the PR's head; carries the commits (≤ 50), files (≤ 300), diff (≤ 256 KiB), and counts of all files, lines and binary files |
   | `stale` | the head moved, so it would not fast-forward |
   | `invalid` | path not a clean directory under home, worktree gone before the first read, or `HEAD` moved |
   | `on_pr` | it already is the PR's head: it reached GitHub some other way |
   | `landed`, `undone`, `discarded` | what the user decided (`pr_landings`) |

2. **Pushing (`LandPR`):** takes the commit the user reviewed and repeats every check, so a head that
   moved meanwhile refuses. It also needs an open PR the user wrote, not from a fork, on a head other
   than the base, `main`, `master`, `develop` or `trunk`. The push names
   `https://github.com/<owner>/<repo>.git` and `gh auth git-credential` itself, with a plain refspec,
   so GitHub refuses anything but a fast-forward. One action runs at a time.
3. **Undo (`UndoPRLanding`):** only while the landed commit is still the branch's head. It pushes one
   new commit on top with the earlier head's tree, signed if `commit.gpgsign` is on (`commit-tree`, so
   it works for a merge and needs no working tree). History is never rewritten, so nothing a push
   brought is lost: the landed commit stays in the branch's history.
4. **Discard (`DiscardPRLanding`):** records the decision, and `restore` forgets it. Neither touches
   GitHub or the worktree.

Bare repositories untouched for 30 days are pruned when orchd starts. Decisions stay; they only lose
their commit list. These RPCs are operator-only, so an agent cannot land its own proposal.

The app keeps Push disabled until the user has opened the diff, and says when a diff was cut off or
a binary file changed.

**Limits:**
- The rule against pushing is an instruction, not a sandbox. An agent that pushes anyway is caught by
  the activity feed and the `on_pr` state, not prevented. The feed is evidence, not proof: a same-user
  agent can flood or forge its own transcript.
- `fsck` runs before each push, so a slow repository takes longer to push, and a change made between
  the check and the push is not caught. A worktree's `.git` file may point outside home; only the
  commit is copied from it.
- A process that outlives its task's process tree counts as the operator, as for every operator-only
  RPC ([daemon.md](daemon.md)), so it could call `LandPR`. A same-user agent could equally run `git
  push` itself: landing is a review gate for honest and prompt-injected runs, not a containment
  boundary. The user's global git config is still read, since commit signing needs it.
- Protected heads are matched by name (the base, `main`, `master`, `develop`, `trunk`), not by GitHub's
  branch protection.
- A proposal on a PR that has moved must be sent back; orch does not rebase it.
- A commit needs signing to work without anyone there. With 1Password approving each signature, a
  background run can stop `BLOCKED` at `git commit -S`; the card shows why.

## Session activity

`GetTaskActivity` reads a task's tool calls and their results from the last 4 MiB of its Claude Code
transcript (`agentrun.ReadActivity`): the newest 200 steps, plus the newest 200 risky ones before them.
Counts of tools, commands, files, failures and risky steps, and whether it pushed, cover every step in
that tail. Nothing is stored: each request reads the file again. A transcript is written by an agent, so
times must parse and tool names are capped.

- **Each step:** the tool, a one-line title, the full command or pattern (≤ 2 KiB, keeping its lines),
  and for a shell step its commands (≤ 8), split at `;`, `&&`, `||` and newlines outside quotes,
  parentheses and here-documents. The title is the first command that does something, skipping `cd`,
  `echo`, assignments and the like. This is for reading only, and is not a shell parser.
- **Results:** matched by tool-use ID, giving whether it failed, how long it took, and the end of what
  it returned (≤ 1.5 KiB). Only the newest 100 steps and failures carry output, to bound a poll.
- **Edits:** lines written and replaced, counted from the tool's input, not from git. An edit that
  replaces a line counts it both ways, so this is a sense of size, not a diff.
- **Redaction:** commands and output lose control characters, then pass through `redact`, before they
  are split or capped. Redaction matches known secret shapes, so a secret in any other form (a truncated
  private key, an `Authorization` header) is shown. Operator-only for that reason.
- **Copy** takes what the step shows, without a here-document's body, so pasting cannot run lines nobody read.
- **Risky** means, where a shell command starts: `git push` (and force), `git reset --hard`,
  `gh pr merge|close|comment|review|edit|ready|create`, `gh issue comment|edit`, a writing `gh api`, or
  a recursive force `rm`. A command hidden in a script or `sh -c` is not seen, so this reports what
  happened and prevents nothing.

Codex runs have no such transcript, and show as unavailable.

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
    `exclude` (all but the listed repos);
  - a list of repos watched for everything (`everything_repos`), on top of what involves the user.
    A listed repo counts whatever the repo mode says. The list is kept but ignored while `items` is
    `all`.
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
- On request: the `handoff/pr-sweep` file, written after `gh pr` mutations, by `SetGitHubScope` and
  by `RequestGitHubSweep` (the app's Refresh).
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
- Repos watched for everything add `--repo=<owner>/<repo>` searches for their PRs and issues, batched
  to stay under GitHub's 256-character query limit. At most 5 batches are searched (alphabetically);
  further repos are watched only for items involving the user. Each search still returns at most 100
  items, so a busy repo can push a quieter one in its batch past the limit. GitHub ignores a missing
  repo in a batch, and a batch with only missing repos is skipped, so a deleted repo cannot stop the
  sweep.
- Every search asks for open items, newest first, at most 100 each. That keeps the number of
  searches nearly fixed against GitHub's limit of 30 a minute.
- Hits outside the enabled scopes, or excluded by a scope's repo picks, are dropped. So the tray and
  `orch prs` cover only what the user opted in to.
- Only `https://github.com/` item URLs are kept, and scope logins are checked again when read.
- With spaces applied, the searches above run once as the default login and once as each logged-in
  space gh profile (SP-063, [config.md](config.md#spaces-policy)). An owner's items are kept only
  from the account its space routes it to, and are enriched as that account. An owner bound to an
  isolated space without a working profile is dropped. Everything searches and repo batches go
  only to the owner's account, and the limits of 5 apply per account. As before, a hit counts only
  for a scope the user enabled.

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

CI rolls up to `none`, `failure`, `pending` or `success`, from the newest run of each check (by
workflow and name, or context): `statusCheckRollup` also returns runs a re-run or re-triggered
workflow replaced, which GitHub's checks box leaves out. A run still queued counts as the newest, and
a skip never replaces a failure. Unlike `gh pr checks`, a job run by two events counts as one check,
because `gh pr view` does not export the event. Those names can also be copied by a workflow the PR
adds, so an earlier failure blocks auto-approval for an untrusted author, and the review is told about
it. Only a check not known to have ended is `pending`, which includes `stale`, as in `gh`.
A skipped or neutral check counts as passing, and `action_required` and `startup_failure` as failing. `orch prs`, the app and the review agent's prompt also show the counts, such as `3 passed,
1 skipped, 0 failed`: every check skipping can be the intent, so it is shown rather than blocked, for
any author. The review judges skips, and looks at each one when the PR changes GitHub Actions. State
is cached in
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

## PR review watch

`internal/reviews` reviews the PRs the user picks (from the app's PRs pane) once they are ready,
and posts to GitHub only what the user opted each PR in to. Rows live in `pr_reviews` in `orch.db`.

**Modes** (what orch may post once there is a verdict). The app shows the standing ones as two
permissions, "approve" and "comment its suggestions":
- `verdict`: nothing. The user decides on each verdict.
- `approve`: approve when the verdict is `approve`, with the drafted comment as the body.
- `feedback`: when the verdict is not `approve`, post the drafted comment so the author can fix
  what it found, once per reviewed commit. The next push is reviewed again. It never approves.
- `auto`: `approve` and `feedback` together.
- `comment`: the user's own "Post comment": post the current draft once, whatever the verdict,
  then fall back to `verdict`. It is for the verdict the user read: new notes drop the request,
  as they redo the verdict.

Nothing is posted for a commit the PR has moved past, in any mode: findings the new commits may
already fix would only confuse the author, and could contradict a review of the new head. A
verdict that a mode would have posted is kept for the user, marked `posted = superseded` with a
note naming the new head (`superseded by <sha>`), and the new head is reviewed as usual.

**Update in place (`update_in_place`, per PR, off by default, `SetPRReviewUpdateInPlace` changes it
and nothing else):** orch's comment always starts with
the hidden `<!-- orch:pr-review -->` marker, and its text names the reviewed commit. Off, each comment
is added as a new one. On, a later comment edits the newest `COMMENTED` review with that marker
written by the signed-in user, or adds one if there is none, and an approving verdict updates that
comment under `feedback` or `auto`. If orch cannot read the PR's reviews, it fails the post rather
than risk a duplicate. An edited review keeps the commit it was first pinned to on GitHub. An approval
is always a new review, and `approve` alone leaves an earlier comment as it is.

Every change of mode is logged (`pr-reviews: <url> may now post as …`), so what orch was allowed
to post, and when that changed, can be traced.

Orch never requests changes.

**Notes:** the user can give each PR notes for its reviews (`WatchPRReview` `instructions`,
≤ 4000 bytes, control characters dropped). They are added to the task prompt, marked as the
user's own instructions, unlike anything in the PR. The agent answers them in the verdict's
`for_user` field, never in the comment that may be posted. orchd keeps that answer and the full
review text before the verdict block (`report`, ≤ 60000 bytes); neither is ever posted. New notes on a PR that already has a verdict
or a failed review review its current commit again.

**States:** `waiting` → (`held`) → `reviewing` → `verdict`, or `failed`; `closed` once the PR is merged or
closed; `stopped` when the user stops watching (in the app, or `orch prs stop`), or when the watch
reaches its limit. A PR moving between draft and open stays watched. `closed` and `stopped` rows are no
longer read; a closed PR that is reopened waits for the user to watch it again.

**Watch limit:** a watch stops 14 days after it began (`watched_at`), with a note saying so and asking
the user to watch the PR again, which starts a new 14 days. Changing the mode or notes of a running
watch does not extend it, unless it is already past the limit and not yet stopped: that counts as
watching again. The limit is enforced when the PR is read, so within one poll, and also when
the read fails. A review under way finishes first: its verdict is kept and shown, and is posted only if
the user watches the PR again in a mode that posts and the commit is unchanged. A PR already merged or
closed by then reads as `closed`. A go-ahead for an untrusted author does not carry into a new watch.

`StopPRReview` with `forget` deletes the row. Stopping does not cancel a review task already running; that task is cancelled on its own, and its verdict is not collected.

**The `pr-reviews` unit** ticks every 15 s:
- A `reviewing` row reads its task from the ledger, so a limit reached during a review is acted
  on once the review ends, before anything is posted for it. Every poll
  (`pr_reviews.poll_interval`, 2 minutes by default, at least the 15 s tick; read when orchd
  starts) it also reads the PR (`gh pr view`), and cancels the review task (`CancelFor`, the
  result saying why) once it no longer matters, so no tokens go on it:
  - A PR merged or closed: the row becomes `closed`, ending the watch, with a note.
  - New commits past the one under review: the row goes back to `waiting` with a note naming
    both commits, and the new head is reviewed once it is ready.

  The row keeps the cancelled `task_id`. A task that ended before the cancel is collected as
  usual; the check that follows closes the watch or holds its verdict back. A failed read leaves
  the review running. Each review gets a random token
  (`nonce`, stored on the row and given only in the task prompt). The result must end with exactly
  one `ORCH-VERDICT-BEGIN <token> … ORCH-VERDICT-END <token>` block holding JSON
  `{verdict, reason, comment, for_user}`; a fence around it is allowed, nothing after it. A block quoted from
  the PR cannot carry the token, and a result the runner cut at 64 KiB no longer ends with the
  block, so neither counts. The verdict must be `approve`, `wait` or `changes` with a reason. Reason (≤ 2000 bytes) and comment (≤ 60000) have control and bidi
  characters dropped. A failed, missing or unparseable review settles that head as `failed`: it is
  not rerun on its own. A new commit, or the user changing the watch, retries it.
- Other active rows read the PR with one `gh pr view` every poll, or right after the user
  changes the watch. `ListPRReviews` reports when each was last read (since orchd started) and
  when it is read next, for the app's progress row. When no CI ran, one
  `gh api repos/<repo>/pulls/<n>` call adds the author's association, which `gh pr view` does
  not offer. **Ready** means open, not a draft, CI green or absent, and no merge conflicts.
  A review runs the PR's code (the runtime fuzz) with the user's credentials, so only PRs whose
  author is the repo's owner, a member or a collaborator are reviewed unattended. Anyone else's
  ready PR is `held` until the user gives the go-ahead (`WatchPRReview` `review_anyway`), which
  covers one review: a later push is held again.
  A ready PR whose head differs from the reviewed one is dispatched to the `pr-verdict` agent,
  unrouted so it runs on Claude Code, which it needs for its subagents. A queue that is full leaves
  it waiting, with the reason in `note`.
- **Posting** happens only for a `verdict` row whose head is still the PR's head, and at most once
  per head; a failed post is recorded and not retried until the user changes the watch.
  - The PR's head is read again right before each post. A head that moved marks the verdict
    `superseded` and posts nothing; a head that cannot be read posts nothing, with a note, and
    the next check tries again. GitHub has no compare-and-set for reviews, so a push in the moment
    between that read and the post can still miss it; the review stays pinned to the reviewed
    commit, and branch protection's "dismiss stale approvals" drops an approval it made stale.
  - The review goes through `gh api POST repos/<repo>/pulls/<n>/reviews` with `commit_id` set to
    the reviewed commit, so it cannot land on later commits. The body goes on stdin.
  - Approving also requires the PR to be ready at that moment, and CI that actually ran and passed
    unless the author is the repo's owner, a member or a collaborator: a fork PR whose workflows
    wait for a maintainer shows no checks at all. It is skipped, with a note, on the user's own
    PR, which GitHub refuses.
  - Approve mode stays on across new commits: each new head is reviewed again, and approved if
    that review says `approve`. That is the point of "approve when ready", and also its risk: the
    verdict comes from a model reading the author's code. The user chooses it per PR.
  - The comment is read when the post is made, not when it was planned. While a post is in
    flight, `WatchPRReview` and `StopPRReview` for that PR answer `Aborted`, and the outcome is
    recorded even if the row changed meanwhile, so the app never shows less than was posted.

**The agent is read-only.** `pr-verdict` (shipped in `internal/install/assets`) runs the primary
review plus the adversarial fan-out (`adversarial-reviewer`, `adversarial-qa`, then
`exploit-finder` when findings could chain). Its prompt holds only orch-built values (URL, repo,
number, head); it reads the PR's text from GitHub as untrusted data. The runner denies it every `gh`
command that changes GitHub, `gh api`, `gh auth`, `git push`, `git -C`, `curl`, `wget`, `sqlite3`
and WebFetch (`agentrun.readOnlyAgents`). Permission patterns match what the agent types, not what
the programs it starts do. The runtime fuzz runs the PR's code with the user's credentials, and
that code could reach GitHub or orch.db itself. So the patterns are a guard, not a sandbox, and
untrusted authors' PRs are held for the user. Running reviews under a separate read-only GitHub
credential is a follow-up. A verdict can also be steered by content in the PR itself, which
is why approval is opt-in per PR, pinned to the reviewed commit, and gated on CI.

**Who can call it:** `ListPRReviews`, `WatchPRReview`, `StopPRReview` and `GetPRDetail` are refused
to queued tasks. `GetPRDetail` reads one PR's metadata (one `gh pr view`), cached a minute per PR
(a forced refresh at most every 10 s, 64 PRs kept).
