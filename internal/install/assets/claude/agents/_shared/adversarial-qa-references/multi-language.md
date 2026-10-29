<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# Multi-Language Reference

Sketches for adversarial QA in non-Python ecosystems. Adapt to the project's tools; same workflow (Phase −1 budget → 0 triage → 1 strategy → 2 execute → 3 report → 4 exit signal), different libraries.

## Go

| Concern | Tool | Notes |
|---|---|---|
| Property | `gopter` or `testing/quick` | `gopter` more featureful and Hypothesis-like |
| Native fuzz | `go test -fuzz` (Go 1.18+) | First-class; coverage-guided; corpus persisted in `testdata/fuzz/` |
| Resource monitoring | `runtime.MemStats`, `time.Now()` | Stdlib |
| Timeouts | `context.WithTimeout` | Idiomatic Go |
| Crash detection | `defer recover()` | Distinguishes panics from documented errors |

```go
func FuzzCalculateTotal(f *testing.F) {
    f.Add([]byte(`[{"qty":1,"price":"10.00"}]`))
    f.Fuzz(func(t *testing.T, data []byte) {
        var items []LineItem
        if err := json.Unmarshal(data, &items); err != nil {
            return // documented input format violation
        }
        defer func() {
            if r := recover(); r != nil {
                t.Errorf("crash: panic on %q: %v", data, r)
            }
        }()
        result, err := CalculateTotal(items)
        if err != nil {
            return // documented error
        }
        if result.Sign() < 0 {
            t.Errorf("spec_violation: negative result %s for input %s", result, data)
        }
    })
}
```

Go's native fuzzer persists failing corpus automatically — that's your loop-mode replay equivalent.

## TypeScript / JavaScript

| Concern | Tool | Notes |
|---|---|---|
| Property | `fast-check` | Direct equivalent of Hypothesis; excellent shrinking |
| Native fuzz | `jest-fuzz` or `fast-check`'s arbitrary system | fast-check is the modern default |
| Resource monitoring | `process.memoryUsage()`, `process.hrtime.bigint()` | Stdlib |
| Timeouts | `Promise.race(p, timeout)` | Idiomatic Node |
| Crash detection | try/catch + `process.on('uncaughtException')` | Async crashes need the latter |

```ts
import fc from 'fast-check';

test('calculateTotal: adversarial', () => {
  fc.assert(fc.property(
    fc.array(fc.record({
      qty: fc.integer(),
      price: fc.oneof(fc.float({ noNaN: false }), fc.constant(-0)),
    }), { maxLength: 10_000 }),
    (items) => {
      try {
        const result = calculateTotal(items);
        if (result < 0) throw new Error(`spec_violation: ${result}`);
      } catch (e) {
        if (e.name === 'InvalidLineItem') return; // documented
        throw e; // crash finding
      }
    }
  ), { numRuns: 500 });
});
```

## Rust

| Concern | Tool | Notes |
|---|---|---|
| Property | `proptest` | Closer to Hypothesis than `quickcheck`; better shrinking |
| Native fuzz | `cargo-fuzz` (libFuzzer backend) | The standard; needs nightly |
| Coverage-guided alternative | `afl.rs` (AFL++) | Mature, slower setup |
| Resource monitoring | `std::time::Instant`, custom allocator wrappers | Stdlib gives time; memory needs work |
| Timeouts | `std::thread::Builder` + channel | No native timeout primitive |
| Crash detection | Compiler ensures memory safety; logic crashes via `panic!` | `#[cfg(test)] catch_unwind` for fuzz harness |

```rust
use proptest::prelude::*;
use rust_decimal::Decimal;

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 500, .. ProptestConfig::default()
    })]
    #[test]
    fn calculate_total_adversarial(items in prop::collection::vec(
        (any::<i64>(), any::<f64>()), 0..10_000
    )) {
        let line_items: Vec<LineItem> = items.iter()
            .map(|(qty, price)| LineItem { qty: *qty, price: Decimal::try_from(*price).ok() })
            .collect();
        match calculate_total(&line_items) {
            Ok(total) => prop_assert!(total >= Decimal::ZERO,
                "spec_violation: {} < 0", total),
            Err(InvalidLineItem(_)) => {} // documented
            Err(e) => panic!("crash: unexpected error {:?}", e),
        }
    }
}
```

Note: Rust's strong type system already eliminates large classes of inputs that Python/JS need to fuzz — focus adversarial-qa effort on *domain* invariants rather than type-system escapes.

## Stub status

These sections are sketches. When this skill picks up real usage in an ecosystem, expand the relevant section with actual workflow code and update the SKILL.md anti-patterns / bootstrap notes accordingly.
