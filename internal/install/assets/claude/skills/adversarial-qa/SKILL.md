---
name: adversarial-qa
description: Use when the user wants to find inputs that break their code through adversarial / hostile / fuzz-style input generation — NOT for writing normal tests. Triggers on phrases like "act as adversarial QA", "find inputs that break X", "what edge cases am I missing", "fuzz this function", "try to crash this", "stress test this with weird inputs", "be a hostile QA tester for X", "what unicode/timezone/numeric edge cases break this", or when the user wants someone to play attacker against their own code. Produces a structured findings file (findings.json) listing minimal-reproducer inputs for each failure category (crashes, spec violations, hangs, resource pathologies, type-contract escapes). FIND-ONLY by design — does not propose or apply fixes. The natural companion is a separate fixer skill; the parent session can call both in a loop until findings is empty. SKIP when the user wants: to add new tests proactively (use tiered-harden-testing), to debug a known failing test (use systematic-debugging), to track down specific inputs that trigger an already-suspected bug — i.e., they have a partial diagnosis like "my function returns wrong values, help me find which inputs" (use systematic-debugging; this skill is for *speculative* adversarial sweeps where the user doesn't yet know if bugs exist), to audit security or auth boundaries (different skill — red-team), to write tests for a fresh module (tiered-harden-testing), or knowledge questions about fuzzing.
---
<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->


# Adversarial QA — now backed by the `adversarial-qa` subagent

This skill has been promoted to a subagent. The full workflow now lives at:

- **Subagent:** `~/.claude/agents/adversarial-qa.md`
- **Shared references:** `~/.claude/agents/_shared/adversarial-qa-references/`
  - `finding-schema.md` — `findings.json` schema
  - `attack-patterns.md` — input-domain attack catalog
  - `python.md` — Python/Hypothesis specifics
  - `multi-language.md` — Go, Rust, TS, etc.

**Invoke the subagent** rather than running the workflow inline:

```
Agent(
  subagent_type="adversarial-qa",
  description="Run adversarial QA",
  prompt="<scope + budget + target details, OR pass the user's request through verbatim>"
)
```

The subagent owns the Phase −1 budget gate, target triage, Hypothesis strategy generation, execution + watchdog, and the `findings.json` / `summary.md` outputs. It runs in its own context window, which keeps the parent session lean during long fuzz runs.

## When to use this skill vs. invoke the subagent directly

- **Use this skill (description-triggered)** when the user phrases the request in fuzz-language and you (the parent session) are already engaged. The skill's role is to be discoverable via the standard skill-matching flow — when it matches, hand off to the subagent.
- **Invoke the subagent directly** when you're inside an automation flow (e.g., the global PR-review fan-out rule, the `reviewer` orchestrator subagent's Step 5.5) and don't need skill-matching to find it.

## Why the move

- Long fuzz runs benefit from an isolated context window — parent stays responsive while QA churns.
- The runtime fuzzer pairs with `adversarial-reviewer` (static threat-actor) under the global PR-review fan-out. Both are subagents so the fan-out can dispatch them uniformly via `Agent(...)` calls.
- The shared references survive as files under `~/.claude/agents/_shared/` so both the subagent and any caller that wants the schema can read them without skill-loading machinery.

Everything else — workflow phases, finding categories, loop-mode contract, anti-patterns — is unchanged. The subagent file is the source of truth; read it for the full workflow.
