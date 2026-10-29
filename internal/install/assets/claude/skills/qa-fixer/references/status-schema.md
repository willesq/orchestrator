<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# Fix Status Schema

Output contract for the `qa-fixer` skill. Mirror of the `findings.json` schema in adversarial-qa, with per-finding outcomes and aggregate counts. The parent loop reads this to decide whether to re-invoke `adversarial-qa`.

## File location

```
./.adversarial-qa/fix-status-{ISO-8601-timestamp}.json
./.adversarial-qa/fix-status-latest.json   # symlink to newest
./.adversarial-qa/fix-status.md            # human-readable digest
```

## Top-level structure

```json
{
  "schema_version": "1.0",
  "fixer_run_id": "uuid-v4",
  "input_findings_run_id": "uuid-of-findings-file-consumed",
  "input_findings_path": "./.adversarial-qa/findings-2026-05-04T19-30-12Z.json",
  "started_at": "2026-05-04T19:45:00Z",
  "completed_at": "2026-05-04T19:58:31Z",
  "config": {
    "mode": "hybrid | full-auto | propose",
    "allow_spec_edits": false,
    "test_command": "pytest tests/",
    "test_baseline_passed": 412,
    "test_baseline_failed": 3
  },
  "summary": {
    "input_findings_count": 12,
    "clusters": 7,
    "applied": 4,
    "deferred": 2,
    "failed": 1,
    "reproducer_pass_rate": 0.857,
    "test_suite_regressions": 0
  },
  "outcomes": [
    { ... see Outcome schema below ... }
  ]
}
```

## Outcome schema (per cluster)

```json
{
  "cluster_id": "c-0001",
  "finding_ids": ["f-0001", "f-0007"],
  "target": {
    "qualified_name": "myproject.pricing.calculator.calculate_total",
    "file": "src/pricing/calculator.py",
    "line": 42
  },
  "category": "crash",
  "severity": "high",
  "status": "applied | deferred | failed",
  "strategy": "code_fix | validation_fix | spec_fix | defer",
  "risk_class": "low | medium | high",
  "decision": {
    "rationale": "Crash on empty list violates implied precondition; spec was silent. Adding explicit validation with documented error.",
    "auto_applied": true,
    "user_confirmed": false
  },
  "diff": {
    "files_changed": ["src/pricing/calculator.py"],
    "lines_added": 3,
    "lines_removed": 0,
    "diff_hash": "sha256:abc123...",
    "diff_preview": "+    if not items:\n+        raise ValueError(\"items must be non-empty\")\n+    "
  },
  "verification": {
    "reproducer": {
      "ran": true,
      "passed": true,
      "elapsed_ms": 47,
      "output_summary": "ValueError raised as expected; original ZeroDivisionError no longer occurs"
    },
    "test_suite": {
      "ran": true,
      "passed_count": 412,
      "failed_count": 3,
      "elapsed_seconds": 14.2,
      "regression_detected": false,
      "newly_failing_tests": []
    }
  },
  "deferred_reason": null,
  "failed_reason": null,
  "elapsed_ms": 12347
}
```

### Field guidance

- **`status`** — three values:
  - `applied` — fix was made AND verification passed (reproducer + test suite both green or no regressions).
  - `deferred` — skill chose not to apply; `deferred_reason` populated with one of: `high_risk_routing` | `requires_spec_edit` | `requires_user_judgment` | `verification_setup_failed` | `architectural_change` | `conflicting_clusters`.
  - `failed` — skill tried to apply, verification failed, fix was reverted; `failed_reason` populated with `reproducer_still_fails` | `test_suite_regressed` | `apply_error` | `revert_error`.
- **`decision.auto_applied`** — true only if the skill applied without asking. False for any user-confirmed apply (propose mode, or hybrid medium-risk).
- **`diff.diff_hash`** — sha256 of the unified diff. Lets the parent loop verify the fix landed (e.g., compare against `git diff` after invocation).
- **`diff.diff_preview`** — first ~5 lines of the diff for the human-readable digest. Don't try to summarize the whole change; the file path + line count is sufficient.
- **`verification.reproducer.ran`** — false only if the reproducer couldn't be constructed (e.g., complex object reconstruction failed). In that case `passed` is null and `failed_reason` is `reproducer_construction_failed`.
- **`verification.test_suite.regression_detected`** — true if any test that was previously green is now red. Compare `newly_failing_tests` against the baseline from `config.test_baseline_failed`.

## Loop terminator semantics

The parent session uses these fields to decide whether to keep looping:

```python
def should_continue_loop(fix_status):
    if fix_status["summary"]["failed"] > 0:
        return False  # something broke; surface to user
    if fix_status["summary"]["applied"] == 0:
        return False  # nothing changed this round; re-running QA would just re-find the same things
    if fix_status["summary"]["test_suite_regressions"] > 0:
        return False  # someone applied a fix that broke other tests; should not have happened, surface
    return True  # made progress, re-run adversarial-qa to confirm
```

The fixer's job is to make `summary.applied > 0` honestly. If it can't, the loop exits and the user takes over.

## fix-status.md (human digest)

Alongside the JSON, write a markdown digest that's grep-friendly and easy to read in a terminal:

```markdown
# QA Fix Status

**Run:** {fixer_run_id}
**Input:** {input_findings_path} ({input_findings_run_id})
**Mode:** {config.mode} | spec edits: {config.allow_spec_edits}
**Duration:** {duration_seconds}s

## Summary

- {applied} applied
- {deferred} deferred ({reason breakdown})
- {failed} failed
- Reproducer pass rate: {pct}%
- Test suite regressions: {n}

## Per-cluster outcomes

### [✓ APPLIED] f-0001, f-0007: calculate_total — empty list crash
- Risk: low | Strategy: validation_fix
- Diff: src/pricing/calculator.py (+3 lines)
- Verification: reproducer ✓, test suite ✓ (412 passed, 3 unchanged failures)
- Rationale: Crash on empty list violates implied precondition...

### [⊘ DEFERRED] f-0003: parse_date — DST handling
- Risk: high | Reason: requires_user_judgment
- Issue: Fix would require choosing between "raise on ambiguous time" and "always use earlier offset"
- Recommendation: User decision needed on DST policy before fix can be applied.

### [✗ FAILED] f-0005: tokenize — empty string
- Risk: medium | Reason: test_suite_regressed
- Attempted: Added empty-string guard; verification revealed 4 tests in tokenization/ now failing.
- Reverted automatically. Surface for human review.
```

## Versioning

Same semver rules as `findings.json`. `schema_version` follows the same versioning policy. Consumers (just the parent loop, for now) should refuse to parse on a major mismatch.
