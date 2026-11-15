---
title: "GitHub hyperlink format"
group: Output
order: 150
description: "Clickable PR, issue, run and commit links, repeated every time."
---

### 🐙 GitHub — hyperlink format

- **Hyperlink format** (the shared policy requires clickable links; these are the shapes):
  - Same-repo PR: `[#5885](https://github.com/acme/infra/pull/5885)`
  - Cross-repo PR: `[acme/infra#5885](…)` — qualify with `org/repo` so the repo is
    knowable without clicking.
  - Issue: `[#1234](https://github.com/<org>/<repo>/issues/1234)`
  - Run / job: `[run #25460472057](https://github.com/<org>/<repo>/actions/runs/<id>)`
  - Commit: `[abc1234](https://github.com/<org>/<repo>/commit/<full-sha>)`; bare `abc1234` is
    fine inside code fences or commit lists.
  - Tables: the cell value is the link, not a separate "see URL" column.
  - Repeat the hyperlink every time, even if the URL was just shown. Don't make me scroll back.
  - Exception: literal terminal output, log lines, and verbatim commit messages keep their
    original formatting.
