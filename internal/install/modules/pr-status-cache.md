---
title: "PR status from the pr-babysitter cache"
group: Pull requests
order: 140
description: "Answer PR status questions from orch's cache; gh search field limits."
---

### 🐙 GitHub — PR status queries

- **Prefer the pr-babysitter cache over live `gh` for status questions.** The daemon runs
  `gh search prs author:@me is:open` every 10 minutes and caches it. For "how are my PRs",
  use `orch prs` / the `orch-pr-status` skill. Use `gh` directly only for details the cache
  doesn't carry (diff, comments, CI logs, files).
- **`gh search prs` has a narrower JSON field set** than `gh pr list` / `gh pr view`. For
  `reviewDecision` / `statusCheckRollup` / `mergeable`, discover via `gh search prs` then
  enrich per-PR with `gh pr view <url> --json reviewDecision,statusCheckRollup,mergeable`.
