<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# Python Reference

Default tooling for Python adversarial QA. Adapt to the project's existing setup; don't introduce a parallel toolchain.

## Default stack

| Concern | Tool | Why |
|---|---|---|
| Property / fuzz generation | `hypothesis` | Strategies, shrinking, example DB for loop replay |
| Test runner | `pytest` | Standard; integrates with hypothesis cleanly |
| C-extension fuzzing | `atheris` | If the target wraps C code, atheris finds memory bugs hypothesis can't |
| Resource monitoring | `tracemalloc` (stdlib) + `psutil` | Memory peak + FD/CPU watch |
| Timeouts | `signal.SIGALRM` (Unix) or `threading.Timer` (cross-platform) | Watchdog for hang detection |
| Output parsing for spec checks | `pydantic` if project uses it | For checking output shape against declared models |

## pyproject.toml additions

```toml
[project.optional-dependencies]
adversarial-qa = [
    "hypothesis>=6.100",
    "psutil>=5.9",
    # "atheris>=2.3.0",  # only if you're fuzzing C extensions
]
```

`atheris` requires Clang; only add if the target package has C extensions worth fuzzing. For pure-Python, hypothesis is sufficient.

## Hypothesis settings for adversarial work

```python
from hypothesis import settings, Verbosity, HealthCheck, Phase

# Profile for adversarial-qa runs: more examples, smaller deadline,
# all phases enabled (especially shrink), suppress noisy health checks.
settings.register_profile(
    "adversarial",
    max_examples=500,
    deadline=1000,                    # ms; raise if shrinking is slow
    derandomize=False,                # we WANT randomness across runs
    print_blob=True,                  # makes shrunk examples easy to copy
    verbosity=Verbosity.normal,
    phases=[Phase.explicit, Phase.reuse, Phase.generate, Phase.target, Phase.shrink],
    suppress_health_check=[HealthCheck.too_slow, HealthCheck.data_too_large],
)
```

Activate with `HYPOTHESIS_PROFILE=adversarial pytest path/to/fuzz_tests.py`.

The `Phase.reuse` phase replays the failing examples cached in `.hypothesis/examples/` from prior runs — this is what makes loop-mode converge.

## Skeleton: function-level adversarial test

Place adversarial fuzz tests separate from regular tests, e.g., `tests/fuzz/`. They are NOT run in normal CI by default; the parent loop or a dedicated CI job invokes them.

```python
# tests/fuzz/test_calculate_total_adversarial.py
import time
import tracemalloc
from decimal import Decimal
from hypothesis import given, strategies as st, settings, target
import pytest

from myproject.pricing.calculator import calculate_total, InvalidLineItem


# Strategy built from the TYPE SYSTEM, not the parser.
# Layer in attack patterns: negative qty, NaN/inf prices, huge lists,
# decimals at precision boundary, mixed-sign discounts.
line_item = st.fixed_dictionaries({
    "qty": st.integers(min_value=-(2**63), max_value=2**63 - 1),
    "price": st.one_of(
        st.decimals(allow_nan=True, allow_infinity=True),
        st.just(Decimal("0")),
        st.just(Decimal("-0")),
    ),
})


@settings(max_examples=500, deadline=1000)
@given(st.lists(line_item, max_size=10_000))
def test_calculate_total_adversarial(items):
    """
    Spec invariants (from docstring):
      - Returns Decimal
      - Result is >= 0
      - Raises InvalidLineItem on malformed input (negative qty, NaN price)
    """
    tracemalloc.start()
    start = time.perf_counter()
    try:
        result = calculate_total(items)
    except InvalidLineItem:
        # Documented contract behavior — not a finding.
        return
    elapsed_ms = (time.perf_counter() - start) * 1000
    peak_kb = tracemalloc.get_traced_memory()[1] / 1024
    tracemalloc.stop()

    # Hang detection (medium severity)
    assert elapsed_ms < 1000, (
        f"hang: {elapsed_ms:.0f}ms on {len(items)} items"
    )

    # Resource pathology (low severity unless extreme)
    if len(items) > 0 and peak_kb / max(len(items), 1) > 100:
        pytest.fail(f"resource_pathology: {peak_kb:.0f}KB for {len(items)} items")

    # Spec invariants
    assert isinstance(result, Decimal), f"contract_escape: expected Decimal, got {type(result)}"
    assert result >= 0, f"spec_violation: result {result} < 0 for input {items!r}"
```

The Hypothesis assertion failures shrink automatically — you'll see the minimal failing input in the test output.

## Wrapping it as the skill workflow

The skill's runtime is a Python script that:

1. **Discovers targets** — for function scope, the user names the target. For file/package scope, walk the AST for public functions/methods and apply the core/peripheral classifier.
2. **Builds strategies** — for each target, generate a Hypothesis strategy from the type hints, then layer attacks from `attack-patterns.md`. If the function takes a custom type, ask the user for a strategy or sample data.
3. **Runs the fuzz tests** — invoke pytest on a temp file containing the generated test, captures the output and the shrunk failures.
4. **Parses Hypothesis output** — Hypothesis emits a `Falsifying example:` block with the shrunk input. Capture that.
5. **Writes findings.json** — schema in `finding-schema.md`. One entry per shrunk failing input, classified by category.

## Tips for fuzzing in practice

- **Pin random seeds for reproducibility.** Hypothesis's seed is in the output; capture it in `metadata.hypothesis_seed` so the user can re-run the exact same run.
- **Don't `assume(False)` to skip cases.** That hides bugs. Filter strategies up front instead.
- **Use `target()` to guide search.** If you suspect performance pathology grows with input size, `target(len(items))` makes Hypothesis bias toward larger inputs.
- **Custom strategies for domain types.** Build a `valid_user()` strategy for your `User` class, then mutate it (`.map(corrupt_one_field)`) to generate "almost valid" data — that's where contract escapes hide.
- **Don't fuzz with the project's own validation library.** If your function calls `pydantic.BaseModel.parse_obj`, the Pydantic strategy hypothesis-jsonschema generates only valid inputs. You want invalid ones — generate raw dicts and feed them in.

## Atheris (only for C extensions)

If the target package wraps C code (compiled extensions, FFI), `atheris` does coverage-guided fuzzing similar to libFuzzer:

```python
import atheris, sys
import myproject.parser

def TestOneInput(data):
    try:
        myproject.parser.parse(data)
    except myproject.parser.ParseError:
        return  # documented behavior
    except UnicodeDecodeError:
        return
    # any other exception is a crash finding

atheris.Setup(sys.argv, TestOneInput)
atheris.Fuzz()
```

Atheris reports crashes with full inputs in `crash-*` files. Translate those into `findings.json` entries with `category=crash`.
