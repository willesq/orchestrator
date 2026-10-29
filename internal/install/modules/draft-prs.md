---
title: "Draft PRs need no confirmation"
group: Git & GitHub
order: 130
description: "gh pr create --draft is always OK; later PR changes still need confirmation."
---

### 🐙 GitHub — draft pull requests

- **Draft PR creation is always OK.** `gh pr create --draft` needs no per-instance
  confirmation — no reviewers tagged, not merged, easy to close; treat it like a local commit.
  Confirmation still applies to `gh pr ready`, `gh pr edit` (after open), `gh pr comment`, and
  any merge action.
