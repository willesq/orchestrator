---
title: "PR verdict: enumerate other reviewers"
group: Pull requests
order: 170
description: "Commands and rules for not approving over unresolved blocking feedback."
---

### 🤝 PR verdict — how to enumerate other reviewers

The shared policy forbids approving over unresolved blocking feedback. These are the commands
that produce the evidence:

1. **Enumerate input:** `gh pr view <n> --json reviews,comments,reviewDecision` plus
   `gh api repos/<org>/<repo>/pulls/<n>/comments` for inline review comments. Don't rely on the
   UI summary — inline comments and thread resolution state matter.
2. **Classify blocking vs non-blocking.** Blocking: state `CHANGES_REQUESTED`; "blocking" /
   "required" / "must fix" / "needs to change before merge" phrasing; any unresolved thread on
   a flagged-as-required finding. Ambiguous phrasing ("consider X" vs "this must change") →
   default to blocking and ask. Silently downgrading a peer's concern is not your call.
3. **Check post-feedback commits:** compare each blocking comment's `createdAt`/`submittedAt`
   against `gh pr view <n> --json commits`, then **walk the diff** of later commits and verify
   the specific concern was addressed. "A commit landed after the comment" is NOT sufficient.
4. **Name the reviewers you considered** in your verdict text. "Approved over @alice's blocking
   note on auth handling — confirmed addressed by commit abc1234" is honest; a bare "Approved"
   that omits unresolved peer feedback is dishonest by omission.

**Exceptions:** merge-gating bots (cubic-dev-ai, semgrep gates) are treated as humans;
cosmetic/auto-fixing bots don't gate. A resolved thread counts as addressed only if the
resolver was the reviewer who raised it — the PR author resolving their own flagged thread does
not count, re-verify. If the user says "approve it, I've talked to alice," proceed and document
the override.
