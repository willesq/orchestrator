---
name: qa-fixer
description: Use when the user wants to apply fixes for findings produced by the adversarial-qa skill — i.e., consume a findings.json file and turn each finding into a code change. Triggers on phrases like "fix the findings", "process findings.json", "apply fixes from adversarial-qa", "run the qa-fixer", "loop the QA and fixer", "fix the bugs the fuzzer found", "convergence loop on adversarial-qa", or whenever the user references a findings file produced by adversarial-qa. Reads findings-latest.json, classifies each finding's fix strategy (code_fix / validation_fix / spec_fix / defer), applies low-risk fixes automatically, gates medium-risk fixes for confirmation, defers high-risk to the user. Always re-runs the original reproducer AND the project test suite after each apply. Writes a fix-status.json the parent loop can use to decide whether to re-invoke adversarial-qa. SKIP when the user wants: to fix an arbitrary bug not driven by a findings file (use focused-fix or systematic-debugging), to find new bugs (use adversarial-qa), to write tests proactively (use tiered-harden-testing), to refactor or restructure code, or knowledge questions about bug fixing.
---
<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->


# QA Fixer

The companion to `adversarial-qa`. It reads the findings file, classifies each finding by fix strategy, applies the safe ones automatically, gates the risky ones, and records a status file the parent loop uses to decide whether to keep going. Together they form a closed loop: find → fix → re-find → exit when findings is empty.

## Core principles (do not violate these)

1. **Driven only by findings.json.** This skill is not a general bug-fixer. It consumes the structured output of `adversarial-qa` and applies *that specific* set of fixes. If the user wants to fix arbitrary bugs not driven by a findings file, route them to `focused-fix` or `systematic-debugging` instead. The strict input contract is what makes the loop work.

2. **Code-only by default. Specs are sacred.** A "fix" that loosens the docstring or relaxes the type hint to make the failure go away is not a fix — it's hiding the bug under a new spec. By default, this skill modifies code only. Spec edits are gated behind an explicit `--allow-spec-edits` flag, and even then they require per-edit confirmation. The ability to edit a spec to match buggy code is exactly the move that makes adversarial QA meaningless.

3. **Always verify before declaring done.** After every apply: re-run the failing reproducer (must now pass) AND the project test suite (must not regress). If either fails, revert the change and mark the finding `deferred` with the failure reason. Trusting the LLM's "this fixes it" without verification is how silent regressions land.

4. **Risk-classify, then route by class.** Three classes:
   - **Low risk** — adding a precondition validator, narrowing a too-broad `except`, adding a type guard, returning a documented default. Auto-apply in hybrid mode.
   - **Medium risk** — algorithmic change inside the function, control-flow guard, accepting a wider input range. Confirm with user before applying.
   - **High risk** — multi-file refactor, public API change, spec edit, anything touching shared utilities. Always defer to user; never auto-apply.

5. **Defer is a first-class outcome.** Not every finding has a clean fix. If the right answer is "the spec is wrong" or "this is a real architectural problem," mark it `deferred` with a clear reason and move on. The loop terminator works on (applied + deferred) — deferred findings show up in the status file for the user to triage.

## Workflow

Run these phases in order. After each phase, summarize and proceed.

### Phase −1 — Confirmation gate (mandatory)

Before applying anything, read the findings file and present:

- Total findings count, broken down by severity and category.
- Estimated wall-clock budget (rule of thumb: 3–10 min per finding for low/medium risk, plus the project test suite runtime ×N for verification).
- The autonomy mode for *this run* — default is **hybrid** (auto low, confirm medium, defer high). Other modes:
  - `--mode=full-auto` — auto-apply everything that's not high-risk; no confirmations. Loop-mode default for unattended runs.
  - `--mode=propose` — confirm every fix, even low-risk. Use when running interactively for the first time on a new codebase.
- Whether `--allow-spec-edits` is on. Default: off.

Then ask the user to confirm. Wait for explicit approval before reading code or applying fixes. Same rule as the QA skill — the parent session might have invoked this in a loop, but the user should still see the budget when running interactively.

### Phase 0 — Findings intake

1. Read `./.adversarial-qa/findings-latest.json` (or `--findings <path>`).
2. Validate `schema_version` matches what this skill knows (currently `1.0`). Refuse to proceed on a major version mismatch.
3. **Cluster similar findings.** Multiple findings with the same `target` + `category` + `spec_invariant_violated` are usually one root cause; group them so a single fix can address all the clustered findings simultaneously. Each cluster gets a primary representative (the most-shrunk reproducer wins).
4. Sort the cluster list by `severity` (high → medium → low). Within severity, by `category` (crashes first, then spec_violations, then contract_escapes, then resource_pathology, then hang).

### Phase 1 — Per-finding triage

For each cluster:

1. Read the target function's current code, signature, and docstring. Use Serena MCP tools (`mcp__serena__get_symbols_overview`, `mcp__serena__find_symbol`) for code reading where available — token-efficient and structured.
2. **Decide fix strategy** by reading the finding's `category` + `expected.source` + `spec_invariant_violated`. See `references/fix-strategies.md` for the full rubric. Strategies:
   - `code_fix` — modify the implementation to handle the input correctly or reject it explicitly.
   - `validation_fix` — add an explicit precondition check that raises a clear documented error. Often the right move for crashes on invalid input.
   - `spec_fix` — gated behind `--allow-spec-edits`. Update the docstring/type hint when the code is correct and the spec was overspecified. Always defer if the flag is off.
   - `defer` — when the right answer isn't clear, when the fix would be high-risk, or when fixing requires architectural decisions the skill can't make alone.
3. **Risk-classify** the proposed fix. See `references/fix-strategies.md` for the rubric.
4. Route by mode:
   - Hybrid mode: low → apply silently; medium → present diff and confirm; high → mark deferred, write reason.
   - Full-auto: low + medium → apply; high → mark deferred.
   - Propose: any → present diff and confirm.

### Phase 2 — Apply, verify, commit-or-revert

For each fix that survives Phase 1 routing:

1. **Make the code change.** Edit narrowly — one edit per finding when possible, so a verification failure cleanly identifies which fix broke. Use the Edit tool with sufficient context to make the replacement unique.
2. **Run the reproducer.** Construct a Python expression from `reproducer.call_form` and execute it; the failure observed in the original finding must NOT recur. If it still recurs, the fix didn't work — revert and mark `failed`.
3. **Run the project test suite.** See `references/verification.md` for how to detect the project's test command. The pre-fix baseline (recorded in Phase 0) must hold: no test that was green is now red. If a regression appeared, revert and mark `failed`.
4. **Record outcome** in the in-memory status accumulator: `applied | deferred | failed`, with the diff hash, verification timing, and any error messages.

If verification fails twice in a row on the same finding (after revert + retry with a different strategy), stop retrying and mark `deferred` with reason `"could not synthesize a fix that passes verification"`. Don't loop forever.

### Phase 3 — Status report

Write `./.adversarial-qa/fix-status-{ISO-timestamp}.json` plus `./.adversarial-qa/fix-status-latest.json` (symlink) and a human-readable `./.adversarial-qa/fix-status.md`. Schema in `references/status-schema.md`.

The status file includes:
- Per-finding outcome with diff hash, verification result, and elapsed time.
- Aggregate counts: applied, deferred (by reason), failed.
- Pointers to the input findings file (run_id) so the parent loop can correlate.

### Phase 4 — Loop exit signal

Print a parseable final line:

```
QA_FIXER_RESULT applied=<N> deferred=<N> failed=<N> reproducer_pass_rate=<X>%
```

Exit codes:
- `0` — clean run; whatever was applied passed verification, deferred is acceptable.
- `1` — at least one apply attempt verified-failed AND wasn't reverted cleanly. Parent loop should stop and surface to the user.
- `2` — schema mismatch or missing findings file (cannot proceed).

## Loop semantics (with adversarial-qa)

The parent session running the convergence loop:

```
while True:
    invoke adversarial-qa  → produces findings-latest.json
    if findings.count == 0: break
    invoke qa-fixer        → reads findings-latest.json, applies fixes, writes fix-status-latest.json
    if qa-fixer.exit_code != 0: break  # surface to user
    if qa-fixer.applied == 0 and qa-fixer.deferred > 0: break  # nothing more to do automatically
```

Why the second break condition matters: if every finding from this round was *deferred*, looping again will produce the same findings and defer again — infinite loop. The parent must recognize that and surface the deferred set to the user.

## Bootstrap

Required tools:
- A way to run the project test suite (detected via `pyproject.toml`, `package.json`, `Makefile`, etc. — see `references/verification.md`).
- The Edit tool (which Claude Code provides directly).
- Optional: Serena MCP for token-efficient code reading.

This skill doesn't add new dependencies to the user's project. If anything's missing (e.g., no test runner detected), it surfaces the gap and asks how to verify fixes — never silently skips verification.

## Anti-patterns to refuse

- **"Fix this bug for me."** Without a findings file, this skill is the wrong tool. Route to `focused-fix` or `systematic-debugging`. The contract with adversarial-qa is what makes this skill safe and predictable; honor it.
- **"Just relax the docstring so the test passes."** That's the spec-edit anti-pattern. Without `--allow-spec-edits`, refuse; with the flag, still confirm per-edit and document the reason explicitly in the fix-status report.
- **"Skip the test suite, just check the reproducer."** Half-verification masks regressions. The reproducer confirms the specific finding is gone; the test suite confirms you didn't break something else. Both are needed.
- **"Apply all the high-severity fixes silently."** Severity isn't risk. A high-severity crash might need a low-risk validation fix; a low-severity contract escape might need a high-risk algorithmic change. Risk-class drives autonomy, not severity.
- **"Bundle all findings into one big fix."** Tempting but bad — when something breaks in verification, you can't tell which sub-fix caused it. One fix per cluster, one verification per fix.
- **"Loop until the test suite is 100% green."** Loop until *findings* is empty. Test suite greenness is a verification check on each fix, not the convergence condition.
