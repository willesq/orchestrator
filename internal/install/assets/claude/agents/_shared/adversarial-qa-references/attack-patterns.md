<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# Attack-Pattern Catalog

Per-type input attacks. For each input type your target function accepts, layer in the relevant patterns when building the Hypothesis strategy. The goal is to generate inputs that *the type system allows* but *the function probably doesn't expect*.

## Strings

| Pattern | Why | Hypothesis hint |
|---|---|---|
| Empty string | Often forgotten in length-based logic | `st.just("")` |
| Single char | Boundary on substring/slicing logic | `st.text(min_size=1, max_size=1)` |
| Very long (10K, 1M chars) | Buffer sizing, regex catastrophic backtracking | `st.text(min_size=100_000)` |
| Null byte `\x00` | C-API truncation, path-handling surprises | `st.text(alphabet="\x00abc")` |
| Mixed-script Unicode | Combining chars, RTL, ZWJ | `st.text(alphabet=st.characters())` (default is wide) |
| Combining chars / NFC vs NFD | `len("é") != len("é")` after normalization | `st.text().map(unicodedata.normalize, 'NFD')` vs `'NFC'` |
| Surrogate pairs | Emoji like 👨‍👩‍👧‍👦 are multiple code points | Default `st.text()` covers this |
| Whitespace-only | Often slips past `if s:` but fails downstream parsers | `st.text(alphabet=" \t\n\r")` |
| Leading/trailing whitespace | `strip()` not called, comparisons fail silently | `st.text().map(lambda s: " " + s + " ")` |
| Case variants | Email/URL handlers that compare without normalization | `.map(str.upper)`, `.map(str.title)` |
| Lookalikes / homoglyphs | `Ω` (U+03A9) vs `Ω` (U+2126) | Worth mentioning in spec model |

## Numerics (int / float / Decimal)

| Pattern | Why | Hypothesis hint |
|---|---|---|
| Zero | Division-by-zero, identity-element edge | `st.just(0)` |
| Negative zero (float) | `-0.0 == 0.0` but `1/-0.0 == -inf` | `st.just(-0.0)` |
| One | Identity for multiplication, often cancels logic | `st.just(1)` |
| Negative numbers | Functions assuming non-negative often break | `st.integers(max_value=-1)` |
| Very large positive | Integer overflow in C extensions, memory blowup | `st.integers(min_value=2**63)` |
| Very large negative | Same, reversed | `st.integers(max_value=-(2**63))` |
| `float('nan')` | `nan != nan` breaks all comparisons | `st.just(float('nan'))` |
| `float('inf')` / `-inf` | Range checks pass, math breaks | `st.just(float('inf'))` |
| Subnormal floats | Precision loss, `x + 1 == x` for very small | `st.floats(allow_subnormal=True)` |
| Decimal precision boundary | Money math: `Decimal("0.1") + Decimal("0.2")` | `st.decimals(places=N)` |

## Collections (list / tuple / set / dict)

| Pattern | Why | Hypothesis hint |
|---|---|---|
| Empty | Off-by-one in `for x in xs` + reduction | `st.just([])` |
| Single element | Edge between empty-handling and bulk-handling | `st.lists(..., min_size=1, max_size=1)` |
| Very large (10K, 1M elements) | Quadratic loops, pagination assumptions | `st.lists(..., min_size=10_000)` |
| All-same elements | Hash collision, set deduplication assumptions | `st.lists(st.just(x))` |
| Pre-sorted vs reverse-sorted | Sort algorithms with bad worst case | `.map(sorted)`, `.map(lambda xs: sorted(xs, reverse=True))` |
| Duplicates | Set-vs-list confusion, `dict[k]=v` overwrite vs append | natural under `st.lists` |
| Nested deeply (>1000 levels) | Recursion limit, stack overflow | `st.recursive(...)` |
| Mixed types | If function claims homogeneous, often doesn't enforce | `st.lists(st.one_of(st.integers(), st.text()))` |
| Mutated during iteration | Functions that yield from a collection then mutate it | manually inject |
| Dict with `None`/empty-string keys | Often treated as "missing" by accident | `st.fixed_dictionaries({"": ...})` |

## Dates / datetimes / timestamps

| Pattern | Why | Hypothesis hint |
|---|---|---|
| Naive vs aware | Mixing naive and tz-aware datetimes raises | `st.datetimes(timezones=st.one_of(st.none(), st.timezones()))` |
| DST transition (spring forward / fall back) | A wall-clock minute that doesn't exist or exists twice | manually inject `datetime(2024, 3, 10, 2, 30)` |
| Leap day (Feb 29) | Year-arithmetic that does `+= 365 days` | `st.dates().filter(lambda d: d.month == 2 and d.day == 29)` |
| Year 0 / negative years | Some libs reject, some don't | manually inject |
| Year 9999 | Often the upper bound for date types; +1 day overflows | manually inject |
| Epoch boundaries (1970, 2038) | Y2K38 in 32-bit time_t | manually inject |
| Microsecond precision | Round-trip through Postgres TIMESTAMP loses precision | `st.datetimes()` includes microseconds by default |
| Same instant in different timezones | Equality, sorting, deduplication | `dt.astimezone(...)` |

## Bytes / binary data

| Pattern | Why | Hypothesis hint |
|---|---|---|
| Empty bytes | Length checks, header parsing | `st.just(b"")` |
| Invalid UTF-8 | `.decode("utf-8")` raises, often unhandled | `st.binary().filter(lambda b: not is_valid_utf8(b))` |
| BOMs (`\xef\xbb\xbf`) | Many parsers don't strip, produce ghost chars | manually inject |
| Truncated structured formats | First N bytes of valid PNG/PDF/protobuf | manually inject |
| Bytes with embedded nulls | C-API truncation again | `st.binary().filter(lambda b: b'\x00' in b)` |

## File paths

| Pattern | Why | Hypothesis hint |
|---|---|---|
| Relative paths with `..` | Path traversal even in non-security contexts | manually inject `"../../../etc/passwd"` |
| Symlinks | Different result on resolve vs raw | filesystem fixture |
| Spaces, quotes, shell metachars | Shell-out without proper quoting | `st.text().filter(lambda s: any(c in s for c in " '\"$`"))` |
| Very long paths (>4096 chars) | OS-level limit varies | `st.text(min_size=4097)` |
| Reserved Windows names (`CON`, `PRN`, `NUL`) | Even on macOS/Linux some libs refuse | manually inject |
| Trailing slash | `/path/` vs `/path` semantically different in some APIs | `.map(lambda s: s + "/")` |
| Empty / `.` / `..` | Each does something surprising | manually inject |

## Tips

- **Layer attacks.** Combine: a long Unicode string in a list of dates with NaN microseconds. Real bugs often need multi-axis weirdness.
- **Use `assume()` sparingly.** If you're filtering out 90% of generated inputs, the strategy is too broad — narrow the type, don't filter post-hoc.
- **Add `@example(...)` for known edge cases.** Hypothesis lets you mix property-based with explicit examples; pin the ones from this catalog you care about.
- **Don't generate inputs the spec explicitly forbids.** If the function's docstring says "raises ValueError on negative input," don't generate negatives and call the resulting raise a "finding" — that's the spec working. Use it as a positive contract test instead.
