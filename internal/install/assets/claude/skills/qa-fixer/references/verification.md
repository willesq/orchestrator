<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# Verification

Per-fix verification has two parts that both must pass before a fix is marked `applied`:

1. **Reproducer** — the original failing input no longer triggers the documented failure.
2. **Test suite** — the project's existing tests still pass at the pre-fix baseline (no regressions).

This file covers how to detect, run, and interpret each.

## Detecting the project test command

Order of preference:

1. **`pyproject.toml` script.** Look for `[tool.pytest.ini_options]`, `[tool.tox]`, or a `[project.scripts]` entry that runs tests.
2. **`Makefile` target.** Look for `test:`, `tests:`, `check:` targets.
3. **`package.json` script** (for JS/TS projects).
4. **`tox.ini` / `noxfile.py`** for Python multi-env setups.
5. **CI config** (`.github/workflows/*.yml`, `.gitlab-ci.yml`) — read what CI runs as the canonical answer.
6. **`pytest`** with no args, run from the project root, if a `tests/` or `test/` directory exists.

If none of these resolve, **stop and ask the user**. Don't silently skip verification — that's exactly the failure mode this skill exists to prevent.

For Python projects, the most common command resolutions:

```bash
# From pyproject.toml or default
pytest                              # full suite
pytest -x                           # stop at first failure (faster signal)
pytest tests/ -m "not slow"         # exclude slow markers in fix loop

# From Makefile
make test

# From tox
tox -e py311

# Project-specific (always check)
uv run pytest
poetry run pytest
```

When you're not sure, run the project test command in a dry mode first (e.g. `pytest --collect-only`) to confirm it works.

## Establishing the baseline

Before applying any fixes, run the test suite once and record:
- **Total tests run.**
- **Number passing.**
- **Number failing** (some projects have known-failing tests; we tolerate them as long as the count doesn't grow).
- **Names of failing tests** (so we can detect newly failing ones).
- **Wall-clock time** (so we can budget verification across N fixes).

Save this in the in-memory accumulator as `baseline`. Each post-fix run is compared against it.

If the baseline run itself errors out (collection error, missing dependency, syntax error elsewhere in the codebase), don't proceed. Surface the issue to the user — fixing baseline-broken tests is out of scope for this skill.

## Running the reproducer

Each finding has a `reproducer.call_form` field — a Python expression like:

```python
calculate_total([{"qty": 0, "price": Decimal("-1")}])
```

To run it:

1. **Construct the call** using `reproducer.args` and `reproducer.kwargs` if `call_form` is unavailable or fails to import.
2. **Execute in a subprocess** to isolate from the fixer's runtime — use `python -c "import myproject; ..."` or a dedicated test file.
3. **Compare to `observed`** — if the original was an exception, the new run must NOT raise that exception. If the original was a wrong value, the new return value must match `expected`.
4. **Capture both stdout and stderr.** Some failures only show up in stderr (e.g., warnings that should now be errors).

If the reproducer can't be constructed at all (e.g., the original input contained an opaque object the skill can't reconstruct), record `verification.reproducer.ran=false` and `failed_reason=reproducer_construction_failed`. The fix can't be applied without verification — defer instead.

## Running the test suite after each fix

Don't run the full test suite if the project has marker support — narrow it to:
- Tests in the same module as the fix (`pytest tests/test_<module>.py`).
- Plus a quick smoke test of unrelated areas (e.g., `pytest tests/ -k "smoke" --maxfail=3`).

For the final fix in a batch, run the full suite. This balances thoroughness with verification time.

If the project is small (test suite < 30s), just run the full thing every time. The simplicity is worth the marginal time.

## Interpreting test suite results

Compare to baseline:

- **Same count passing, same count failing, same failing names** → green light. Fix is verified.
- **More passing, no new failures** → green light, and you accidentally fixed something else. Note in the rationale.
- **Same passing, fewer failing** → same as above; a previously-failing test now passes. Note it.
- **Fewer passing, new failures** → REGRESSION. Revert the fix and mark `failed`.
- **Failing tests changed identity** (same count but different names) → REGRESSION; one test silently started passing while another silently started failing. Treat as failure.

## Rolling back a failed fix

When verification fails:

1. Use `git diff` (read-only) to confirm the change you made.
2. Use `git checkout -- <file>` or revert the Edit to restore.
3. Re-run the test suite to confirm the baseline is restored.
4. If the rollback fails (e.g., the file was in a weird state to begin with), STOP and surface the rollback failure to the user. Don't keep trying to fix forward.

## Edge cases

- **Tests that depend on randomness or time.** If the baseline includes flaky tests, treat them as known-flaky and exclude them from regression comparison. Add a note in the status report.
- **Tests that take >60s individually.** Skip during fix verification; run only at end-of-batch. Use `pytest -m "not slow"` if the project marks them.
- **No test suite at all.** Refuse to enter `full-auto` mode. In `hybrid` and `propose` modes, surface the gap and ask the user whether they want to verify by reproducer alone (high risk, document explicitly).
- **Test suite that mutates the database / filesystem.** Run inside the project's documented test fixture; if no isolation is documented, defer hard — don't apply fixes that might leak state into the user's environment.

## Time budget

Default rule of thumb:
- Reproducer: ~1s per finding.
- Narrow test suite (one module): ~5–15s.
- Full test suite: project-dependent; record the baseline runtime in Phase 0 and budget `2 × baseline_runtime × N_fixes` as the worst case.

If the budget would exceed 30 min, stop, report the situation, ask the user whether to continue or batch into multiple runs.
