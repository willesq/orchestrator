---
title: "Adversarial review fan-out"
group: Pull requests
order: 160
description: "Run adversarial-reviewer and adversarial-qa alongside every substantive PR review."
---

### 🛡️ Adversarial review fan-out — dispatch mechanics

The shared policy requires independent static + runtime adversarial review on substantive PR
reviews. This is how to actually dispatch it here.

Triggered by any PR review path — a skill (`/review`, `pr-review-toolkit:review-pr`,
`code-review:code-review`), the `reviewer` orchestrator subagent, or ad-hoc "review PR #X".

1. **`adversarial-reviewer`** — static threat-actor diff review, fires in parallel with the
   primary review. Read-only. Outputs a markdown findings file + ≤100-word summary.
2. **`adversarial-qa`** — runtime fuzzer. **Auto-confirm the Phase −1 budget gate with PR-mode
   defaults — never ask.** Defaults: diff-scoped, 5-min per-target cap, skip-if-no-hypothesis.
   Pass `auto_confirm: true` (or state in the prompt body: "PR-mode budget gate pre-approved;
   proceed without user confirmation") so it skips the interactive
   `[confirm]/[shrink]/[skip]/[override]` prompt. Fan-out is hands-off by default.

**Mechanics:**
- **Both are SUBAGENTS, not skills.** Use the `Agent` tool with
  `subagent_type="adversarial-reviewer"` / `"adversarial-qa"`. Do NOT use the `Skill` tool —
  `adversarial-reviewer` isn't registered as a skill, and `adversarial-qa`'s skill is just a
  redirect to the subagent.
- If any caller (parent OR subagent) reasons "the Agent tool isn't available to me," that
  reasoning is wrong — re-check the tool inventory (parent always has it; the orch `reviewer`
  lists it in frontmatter) and proceed.
- Issue both Agent calls in a **single message with multiple Agent blocks** so they run
  concurrently.
- From the parent session: `run_in_background: true` is fine and recommended for long fuzz runs.
- From inside a subagent (e.g. the orch `reviewer`'s Step 5.5): use **foreground** calls in a
  single message — backgrounding from a sub-subagent is unreliable. Single-message parallelism
  still gives concurrency.

```
Agent({ description: "Threat-actor review of <repo>#<n>", subagent_type: "adversarial-reviewer", prompt: "<PR url + output path>" })
Agent({ description: "Runtime fuzz of <repo>#<n>",        subagent_type: "adversarial-qa",       prompt: "<PR url + worktree + PR-mode budget defaults>" })
```

**Consolidated output:** fold both into the unified review under labelled sections
(`### Threat-actor review (adversarial-reviewer)`, `### Runtime fuzz (adversarial-qa)`). Read
the full output files — don't rely on the short agent-result summaries.

**Opt-outs:** user says "primary review only" / "skip adversarial" / "just a quick look"; the
PR is a bot/dependency PR (renovate, dependabot — they have their own auto-merge flows); the PR
is draft AND the user asked for a quick look.

**Never leak internal scaffolding into PR-visible text.** GitHub-bound review bodies are
strictly findings-content. "The adversarial-reviewer skill was not accessible", "fan-out
failed", "subagent timed out", queue task IDs, session IDs, agent names, skill names, harness
state — never acceptable in GitHub text; the PR author can't act on any of it. Surface failures
and skip-reasons in parent-session channels (queue `result`, agent return message, terminal).
If an adversarial section has no real content because the subagent didn't run, **omit the
section entirely** — no placeholder explaining why.
