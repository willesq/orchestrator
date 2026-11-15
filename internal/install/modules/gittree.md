---
title: "Branch creation via gittree"
group: Git & GitHub
order: 120
description: "Start every branch with the gittree worktree script."
---

### 🔀 Branch creation — always via `gittree`

- **Always start new branches via the `gittree` alias** — never `git checkout -b` or
  `git worktree add` directly. `gittree` is an explicit shell alias (NOT shorthand for the
  built-in `git worktree`) pointing at `~/.claude/orchestrator/scripts/git_worktree_create.sh`.
  (Note: no `gittree` alias is currently defined in any shell rc on this machine — invoke the
  script by path until one is added.) Invoke from the canonical (non-worktree) repo root with
  one argument, the branch name. Behavior:
  - Worktree lands at `<repo-parent>/<repo-name>.worktrees/<branch-name>` (a `/` in the branch
    name becomes a nested directory — that's expected).
  - Default branch (`main`/`master`) is auto-detected and fetched.
  - If the branch already exists locally or on the remote, `gittree` prompts
    `Use existing branch ... [y/N]`. For greenfield work, answer `N` and pick another name.
  - Example: `cd ~/code/acme/infra && gittree feat/my-feature` →
    `~/code/acme/infra.worktrees/feat/my-feature` off `origin/main`.
  - **Never substitute `git worktree add` or `git checkout -b`** even when directness tempts
    you — this alias is used consistently so worktrees land in a predictable location and the
    default branch always resolves correctly.
