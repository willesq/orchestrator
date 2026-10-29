<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# Finding Schema

This is the contract between `adversarial-qa` (producer) and any downstream consumer (the future fixer skill, regression-test generators in `tiered-harden-testing`, the user reading the report). Stable schema — change carefully.

## File location

```
./.adversarial-qa/findings-{ISO-8601-timestamp}.json
./.adversarial-qa/findings-latest.json   # symlink to the newest above
./.adversarial-qa/summary.md              # human-readable digest
```

The `findings-latest.json` symlink is what loop-mode consumers read — it's stable across runs.

## Top-level structure

```json
{
  "schema_version": "1.0",
  "run_id": "uuid-v4",
  "started_at": "2026-05-04T19:30:12Z",
  "completed_at": "2026-05-04T19:35:47Z",
  "scope": {
    "type": "function | file | package | repo",
    "targets": ["pkg.module.func", "pkg.module.OtherClass.method"]
  },
  "config": {
    "per_target_budget_seconds": 300,
    "hang_threshold_ms": 1000,
    "categories_enabled": ["crash", "spec_violation", "hang", "resource_pathology", "contract_escape"]
  },
  "summary": {
    "total_findings": 7,
    "by_category": {"crash": 2, "spec_violation": 3, "hang": 0, "resource_pathology": 1, "contract_escape": 1},
    "by_severity": {"high": 4, "medium": 2, "low": 1}
  },
  "findings": [
    { ... see Finding schema below ... }
  ]
}
```

## Finding schema

```json
{
  "id": "f-0001",
  "category": "crash | spec_violation | hang | resource_pathology | contract_escape",
  "severity": "high | medium | low",
  "target": {
    "qualified_name": "myproject.pricing.calculator.calculate_total",
    "file": "src/pricing/calculator.py",
    "line": 42
  },
  "reproducer": {
    "args": [...],
    "kwargs": {...},
    "call_form": "calculate_total([{'qty': 0, 'price': Decimal('-1')}])"
  },
  "observed": {
    "type": "exception | wrong_value | timeout | resource_pathology",
    "exception_type": "ZeroDivisionError",
    "exception_message": "division by zero",
    "traceback_summary": "calculate_total -> _apply_discount -> divide_safely",
    "actual_value": null,
    "actual_value_repr": null,
    "wall_clock_ms": 1023,
    "memory_peak_bytes": null
  },
  "expected": {
    "source": "docstring | type_hint | user_provided",
    "description": "Should return non-negative Decimal sum of line items, or raise InvalidLineItem on malformed input"
  },
  "spec_invariant_violated": "result must be non-negative",
  "suggested_test_name": "test_calculate_total_negative_price_rejected",
  "shrinking": {
    "original_input_size": 47,
    "shrunk_input_size": 1,
    "shrinking_iterations": 12
  },
  "metadata": {
    "first_seen_at": "2026-05-04T19:31:55Z",
    "hypothesis_seed": "abc123...",
    "deduplicated_with": []
  }
}
```

### Field guidance

- **`id`** — stable within a run. If the same finding appears across runs (replayed from Hypothesis DB), keep its semantic identity by `target` + `spec_invariant_violated` + `category`, even if the `id` rotates.
- **`category`** — one of the fixed five. Don't invent new categories without updating this schema; the fixer skill switches on it.
- **`severity`** — see the rubric in SKILL.md. High = crash on plausible production input. Medium = hang or contract escape. Low = pathological resource usage on weird input that probably won't hit prod.
- **`reproducer.call_form`** — a Python expression a developer can paste into a REPL to reproduce. Should be self-contained (no fixtures, no globals). For complex objects, prefer `cls(**kwargs)` reconstruction over opaque serialized blobs — readability matters here, the consumer is a human or another LLM.
- **`observed`** — one of `exception`, `wrong_value`, `timeout`, `resource_pathology` populates the relevant subfields and leaves others null.
- **`expected.source`** — where the expectation came from. `docstring` is strongest; `user_provided` (i.e., the user wrote a quick spec line for a function with no docstring) is acceptable; `type_hint` is weakest and should rarely be the only source.
- **`spec_invariant_violated`** — a one-line statement of the contract that broke. Used by the fixer to decide what to repair.
- **`suggested_test_name`** — snake_case, descriptive. `tiered-harden-testing` can lift this directly when generating regression tests.

## Category definitions

| Category | What qualifies |
|---|---|
| `crash` | Unhandled exception escapes to the caller, OR process aborts (segfault, panic, OOM kill). |
| `spec_violation` | Function returns *some* value but it violates a documented invariant (sortedness, non-negativity, length constraint, idempotence, roundtrip). The output looks plausible; that's why these are dangerous. |
| `hang` | Function doesn't return within `hang_threshold_ms`. Triggered by exceeding a watchdog, not by deadlock detection — we conservatively call any over-threshold a hang. |
| `resource_pathology` | Wall-clock or memory grows disproportionately to input size. Default threshold: ≥10× the median for the population of inputs at this size. Configurable. |
| `contract_escape` | Return value's type, shape, or null-ness doesn't match the function's signature / type hints / docstring contract. E.g., signature says `-> List[int]` but returns `List[Optional[int]]` for some input; or returns `None` when the docstring says it always returns a value. |

## Severity rubric

- **`high`** — production data path will hit this. Examples: any crash on a string the user might type; spec violation on a list with one element; contract escape that propagates a `None` to a typed caller.
- **`medium`** — possible in production but unlikely. Examples: hang on a 10K-element list when typical inputs are <100; spec violation only at integer-overflow boundaries.
- **`low`** — pathological input that's almost certainly not real. Examples: resource pathology on a 1MB string when the API is documented to accept "short identifiers"; contract escape only with NaN inputs to a function that doesn't claim to handle floats.

If you're unsure, mark it medium and let the fixer / human triage.

## Versioning

`schema_version` follows semver. Adding optional fields → minor bump. Renaming or removing required fields → major bump. Consumers should refuse to parse files with a major version they don't understand.

## Why this schema matters

The fixer skill's job is to take a finding and decide:
- Is the *code* wrong (fix the implementation)?
- Is the *spec* wrong (update the docstring/contract)?
- Is this a precondition violation that should be enforced explicitly (raise a clear `ValueError`)?

That decision depends on `category` (crash → usually code), `severity` (low → maybe document, don't fix), `expected.source` (type_hint only → spec is weak, may need user input), and `spec_invariant_violated` (the actual rule that broke). Without all those fields populated cleanly, the fixer skill can't make good decisions.
