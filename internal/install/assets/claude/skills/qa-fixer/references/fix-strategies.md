<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# Fix Strategies

How to translate a finding from `findings.json` into a fix decision. Use this rubric in Phase 1 of the workflow. Two outputs per finding: a **strategy** (what kind of fix) and a **risk class** (how to route by autonomy mode).

## Strategy by category

The finding's `category` is the strongest signal for which strategy fits.

### `crash`

The function raised an unhandled exception on input the spec didn't say it would reject.

**Default strategy:** `validation_fix` if the input violates a *spec-implied* precondition (e.g., the docstring says "non-empty list" and the input was empty), `code_fix` if the spec didn't constrain the input but the code blew up anyway.

- **Validation-fix example:** Function says it returns the average of a list. Input was `[]`. The docstring doesn't say what happens on empty. Add `if not items: raise ValueError("items must be non-empty")` and update the docstring to document it. *Risk: low.*
- **Code-fix example:** Function says it parses a date string in ISO format. Input was a valid ISO string with timezone offset `+25:00`. The function should reject it explicitly, not crash with a cryptic `ValueError` from `int()`. Catch the underlying error, raise the documented one. *Risk: low.*
- **Defer when:** The crash is in code outside the target function (the bug is upstream/downstream); or the fix needs an architectural decision.

### `spec_violation`

The function returned *something*, but the output violates a documented invariant.

**Default strategy:** `code_fix` — the spec is the contract; the implementation didn't honor it.

- **Code-fix example:** Spec says "result is sorted." Function returned `[3, 1, 2]` for some input. Fix the sort. *Risk depends on cause.* If a missing call to `sorted()`, low. If the algorithm is fundamentally wrong, medium-to-high.
- **Spec-fix candidate (gated):** When the spec was written aspirationally and the code never matched it, AND the user has flagged that the spec was wrong (rare; usually requires user input). Still defer by default; only consider `spec_fix` with `--allow-spec-edits` and per-edit confirmation.
- **Defer when:** The fix would change the function's primary behavior, not just patch a corner case.

### `hang`

Function exceeded the wall-clock watchdog.

**Default strategy:** `code_fix` — add an early-return for the pathological input, switch to a more efficient algorithm, or document an input-size limit.

- **Code-fix examples:**
  - Catastrophic regex backtracking on adversarial input → use `regex` module's atomic groups, or pre-validate input length.
  - Quadratic loop on large input → switch to a linear or `O(n log n)` approach.
  - Unbounded recursion → add a depth check that raises a clear error.
- *Risk: medium-to-high.* Algorithmic changes are not low-risk by default — they can have subtle correctness implications that the test suite might not catch. Always prefer "reject pathological input explicitly" over "make the algorithm faster" unless the user has explicitly asked for the latter.

### `resource_pathology`

Memory or CPU disproportionate to input size.

**Default strategy:** `code_fix` if the algorithm has an obvious fix; `validation_fix` if the right answer is "this function is documented to handle inputs up to N elements, reject larger." Often the latter — pathological inputs frequently aren't real production data.

- **Validation-fix example:** Resource pathology on a 1MB string when the API documents itself as "for short identifiers." Add a length check. Risk: low.
- **Code-fix example:** Memory leak from accumulating intermediate state in a loop. Use a generator. Risk: medium.
- **Defer when:** The pathology requires switching to a streaming model or fundamental refactor.

### `contract_escape`

Return type/shape doesn't match the signature/docstring.

**Default strategy:** Almost always `code_fix` — fix the implementation to honor the typed contract.

- **Code-fix example:** Signature says `-> List[int]`; function returned `List[Optional[int]]` for some inputs. Filter out `None`s before returning, or raise an explicit error. Risk: low.
- **Spec-fix candidate (gated):** When the type hint was wrong from the start and the code's behavior is intentional (e.g., signature should always have been `Optional[List[int]]`). Defer by default.
- **Defer when:** Fixing the type would require changing the public interface.

## Risk classification

Independent of strategy. Determines how the fix is routed by autonomy mode.

### Low risk

Fixes that *narrow* what the function does, don't change correct behavior, and are obviously safe to apply.

- Adding an explicit precondition validator that raises a clear error (`if x < 0: raise ValueError("x must be non-negative")`).
- Narrowing a too-broad `except` clause (`except:` → `except SpecificError:`).
- Adding a type guard at the top of the function.
- Returning a documented default value when the docstring already specifies one.
- Removing a `# TODO: handle X` comment by adding the handler the comment described.

In hybrid and full-auto modes: apply silently after verification.

### Medium risk

Fixes that change *behavior* on inputs the function previously accepted, even if the change matches the spec.

- Changing an algorithm inside the function (sort method, hashing strategy, traversal order).
- Adding a guard that changes control flow significantly (early return that skips later side effects).
- Accepting a wider input range than before (loosening internal validation, with corresponding spec update).
- Re-ordering operations.

In hybrid mode: present the diff and confirm. In full-auto: apply.

### High risk

Fixes that touch shared code, change the public surface, or relax the spec.

- Multi-file refactor.
- Changing a public function's signature, even subtly (return type narrowing, parameter rename).
- Editing a spec/docstring/type hint to match buggy behavior (only with `--allow-spec-edits`, and even then defer to user per-edit).
- Changes to utility code used elsewhere in the codebase.
- Replacing one library with another.

In hybrid mode: defer to user with reason. In full-auto: still defer.

## How to choose

For each cluster of findings:

1. Read the finding's `category` → start strategy decision there.
2. Read the target's current code and docstring.
3. Look at `expected.source` — `docstring` (strong) vs `type_hint` (weaker) vs `user_provided` (weakest). If `expected.source == "type_hint"` and the expected behavior conflicts with the docstring, the docstring wins; surface that conflict to the user.
4. Sketch the smallest-possible fix that:
   - Makes the reproducer pass.
   - Doesn't change behavior on inputs not exercised by the finding.
5. Risk-classify by looking at *what the fix actually changes*, not *which category the finding was in*. A high-severity crash often gets a low-risk validation fix; a low-severity contract escape might need a high-risk algorithm rewrite.
6. Route by autonomy mode.

## When to defer (always)

- The right fix conflicts with the spec, the spec is right, and the code is intentionally wrong (i.e., this is a real bug that needs a design conversation).
- The fix would require modifying tests that exist for *current* behavior, beyond the regression test for this finding.
- Two clusters have related findings whose fixes might conflict — defer one of them and let the user decide on order.
- The finding's category is a category combination this skill doesn't have a clean strategy for (e.g., a crash that's also a contract escape because the function was supposed to raise a specific subclass).
- Verification couldn't be set up reliably (no test runner detected, project too large to run full suite).

Defer is honest. Forcing a fix the skill can't reason about confidently is how you get bad commits.
