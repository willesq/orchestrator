---
title: "PR Watch registration"
group: Personal workflow
order: 220
description: "Register every new PR in ~/.pr-watch.json."
---

- If the user's PR Watch registry is configured, register newly created PRs once
  in `~/.pr-watch.json`, preserving existing entries and their timestamps. Do not
  create duplicate entries or remove entries owned by the watcher.

### 👀 PR Watch integration — register new PRs with the menu bar app

After creating a PR in **any** repo, append it to `~/.pr-watch.json` so the PR Watch menu bar app
picks it up. Applies to every PR, including drafts.

```json
{
  "prs": [
    { "url": "https://github.com/owner/repo/pull/123", "added_at": "2026-03-11T16:30:00Z" }
  ]
}
```

1. Read `~/.pr-watch.json`; if missing, create it as `{"prs": []}`.
2. If an entry with the same `url` exists, **stop** — never duplicate.
3. Otherwise append the full PR `url` and `added_at` as the current ISO8601 UTC timestamp.
4. Write back as pretty-printed JSON, preserving all existing entries.

**Never remove existing entries** — the app owns cleanup of merged/closed PRs.
