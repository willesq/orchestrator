---
name: orch-pr-status
description: |
  Use when the user asks about THEIR pull requests across repos. Triggers include:
  "how are my prs", "what's the status of my prs", "any prs blocked", "any prs ready to merge",
  "what prs need review", "any prs failing ci", "what's blocking my prs", "show me my open prs",
  "did <pr> get reviewed", "any new pr activity".
  Activate even when the parent session is NOT inside a git repo — this skill reads from the
  pr-babysitter daemon's cached state (`~/.local/share/orch/pr_babysitter_state.json`),
  not from `gh` directly. Do NOT shell out to `gh pr list` or `gh search prs` — the daemon
  already aggregates cross-repo state every 10 minutes.
allowed-tools:
  - Bash
  - Read
---

<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# orch-pr-status — PR snapshot via the babysitter daemon

The `pr-babysitter` launchd daemon polls `gh search prs author:@me is:open` every 10 minutes
and writes a normalized snapshot to `~/.local/share/orch/pr_babysitter_state.json`.
The `orch prs` CLI wraps that file with filters and rendering.

**Do not** call `gh pr list` / `gh search prs` from this skill — the babysitter has already
done it. Reading the cache costs nothing; an extra `gh` shell-out is wasteful and breaks
when the parent session is in a non-repo cwd (which is common).

## Decision tree

| User intent | Command |
|---|---|
| "How are my PRs?" / "Show me my PRs" | `orch prs` |
| "Any PRs blocked?" / "What's failing?" | `orch prs --blocked` |
| "Any PRs ready to merge?" | `orch prs --ready` |
| "Give me the JSON" / programmatic follow-up | `orch prs --json` |
| "Hide drafts" / "Just my drafts" | No flag for this. Take `orch prs --json` and filter on `isDraft` yourself. |
| "Re-check now, don't trust the cache" | No on-demand sweep exists. Report the cache's age from `orch status` (`pr cache — N PRs, updated Xm ago`) and let the user decide whether to wait for the next 10-minute tick. |

`orch prs` accepts exactly three flags: `--blocked`, `--ready`, `--json`. Anything else
fails. In particular there is no `--drafts` and no `--refresh`.

## Output format

`orch prs` already renders human-friendly. Surface its output verbatim, then add a short
synthesis line above:
- If `--blocked` returned items: `"<N> PR(s) need attention:"`
- If `--ready` returned items: `"<N> PR(s) ready to merge:"`
- Default unfiltered: one-line summary like `"You have <N> open PRs across <M> repos."`

If the cache is empty (`(no PRs in babysitter state)`), diagnose in this order:

1. `orch status` — the `pr-babysitter` line under **subsystems** shows whether it's running
   and when it last ticked; the `pr cache` line under **data** shows the snapshot's age.
2. `tail -30 /opt/homebrew/var/log/orch/orchd.log` — orchd's combined stdout/stderr when
   Homebrew-managed. Look for babysitter errors (usually `gh` auth).
3. `orch check` — confirms `gh auth` is still valid, which is the usual culprit.

## When NOT to use this skill

- The user is asking about a *specific* PR's diff, comments, or files — that's `gh pr view`
  / `gh pr diff` territory, not the babysitter snapshot.
- The user wants to *create*, *merge*, or *close* a PR — those are mutations; use `gh` with
  explicit confirmation, never silently.
- The user is asking about PRs they did NOT author (e.g. team review queue) — the babysitter
  filters to `author:@me`. Fall back to `gh pr list --search "review-requested:@me"`.

## Hard rules

- ALWAYS prefer `orch prs` (reads cache) over `gh search prs` (network call) for status questions.
- ALWAYS cite the URL alongside any specific PR you mention so the user can click through.
- If you mention transitions ("CI just turned red", "review just landed"), pull those from
  `~/.local/share/orch/events/pr-babysitter.jsonl` — that's where the daemon emits diffs.
