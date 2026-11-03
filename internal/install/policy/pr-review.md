# Reviewing pull requests

Read this before reviewing a pull request or giving a review verdict. It extends
the shared coding-agent expectations in `~/.config/orch/AGENTS.md`.

- Check CI, reviews, and unresolved blocking feedback before approving or merging.
  A skipped check counts as passing, unless the PR changes GitHub Actions workflows
  or actions: then verify from the change that each skip is intentional.
  A later commit alone does not prove a finding was fixed: verify the affected
  behavior. Respect human reviewers and bots configured to gate merges.
- Do not approve over unresolved blocking feedback unless the user explicitly
  authorizes the override. Surface ambiguous or outstanding blockers to the user.
  When assessing a verdict, explain which reviewer concerns were addressed.
- When giving a review verdict, use a findings comment for blocking issues and
  approval only for a clean review with no outstanding peer blockers. Do not
  submit a changes-requested verdict on the user's behalf unless requested.
- For substantive PR reviews, obtain independent static adversarial review and
  runtime adversarial QA where the environment provides those capabilities.
  Run them alongside the primary review; keep review workers read-only and
  consolidate their actual findings. Use diff-scoped runtime targets with a
  five-minute cap per target; skip Hypothesis-dependent fuzzing if unavailable.
- Honor explicit review opt-outs and the project's exceptions for bot/dependency
  PRs and quick looks at drafts. Report unavailable review capabilities to the
  user without claiming they ran.
- Keep GitHub-visible review text focused on actionable findings. Do not expose
  internal agent names, session IDs, queue IDs, dispatch mechanics, or tooling
  failures in review comments. Explain those limitations in the user session.
