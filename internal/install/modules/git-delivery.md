---
title: "Git and GitHub delivery"
group: Git & GitHub
order: 60
description: "Conventional Commits, branches, staging, gh usage, and authorization for external changes."
---

## Git and GitHub delivery

- Use Conventional Commit messages and PR titles, such as `feat:`, `fix:`,
  `docs:`, `test:`, `refactor:`, `chore:`, `style:`, and `perf:`.
- Work on the intended feature branch or managed worktree rather than committing
  directly to the default branch. Use the user's configured worktree workflow
  when available, and preserve existing worktree ownership and branch state.
- Stage the intended changes only. Keep local planning/spec artifacts, including
  `docs/superpowers/`, credentials, and personal configuration out of commits.
  Move durable project decisions into proper documentation or a PR description.
- Use the `gh` CLI for GitHub PRs, reviews, issues, checks, releases, and workflows.
  Prefer structured JSON for inspection and body files for multiline mutations.
  Do not scrape GitHub pages or bypass authenticated tooling to work around auth.
- Respect authorization already given in the session. Obtain approval for external
  mutations when not already authorized; do not repeatedly ask for an action the
  user has requested. Never force-push, rewrite published history, merge, or
  release without authorization for that action.
- Check CI and unresolved blocking review feedback before merging. Reviewing a
  pull request or giving a verdict follows `policy/pr-review.md`.
- Use clickable links whenever referring to PRs, issues, commits, or workflow runs
  in user-facing reports. Describe resulting behavior, evidence, and limitations.
