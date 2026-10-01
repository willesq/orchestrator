# Capability-Aware Model Routing — Design

**Status:** proposal. Nothing here is built.
**Drafted:** 2026-09-30. Every claim in §2 was verified by inspection on the author's
machine; re-verify before relying on one, since vendor layouts move.

---

## 1. The problem

Agent definitions hard-code a model: `dev=opus`, `reviewer=sonnet`, `dispatcher=haiku`.
Three things are wrong with that.

**It is a tier guess, not a capability match.** The tiering assumes "harder work → bigger
model," but the useful question is *which model is good at this kind of work*. A
documentation pass and a refactor are both "dev work" and have no reason to route the same.

**It cannot evolve.** Models ship continuously. A hard-coded `model:` field is correct on
the day it is written and decays silently thereafter. Nothing re-examines it.

**It is single-vendor.** The machine has working `claude` and `codex` CLIs. Only one is
reachable from a routing decision, so a task that another vendor handles better simply
never goes there.

### Goals

1. Route by **task kind**, not by agent name.
2. **Discover** available models automatically — ideally with no user input and no credentials.
3. Let the routing table **evolve**, on a manual trigger and on a cadence.
4. Support **multiple vendors**, gated by explicit registration and enablement.
5. Never let a model start receiving work because a vendor shipped it.

### The constraint that shapes everything

**orchd cannot choose the model at runtime for a Claude Code subagent.** The `model:`
frontmatter is read by Claude Code, not by us; orchd is not in that path. Routing
therefore has to be *materialized* into something the harness reads, or applied at spawn
time by the parent session. See §8.

---

## 2. Discovery — what is actually available

Verified 2026-09-30.

| Source | Result |
|---|---|
| `claude --help`, `codex --help` | Neither CLI has a model-list subcommand. `claude --model` accepts aliases or full names but will not enumerate them. |
| `GET /v1/models` (Anthropic, OpenAI) | Alive, `401`. Google `v1beta/models`: `403`. |
| API credentials on this machine | **None.** No `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`. `codex login status` → "Logged in using ChatGPT" (subscription OAuth). |
| `ant` CLI | Not installed; `anthropics/tap` not tapped. |
| **`~/.codex/models_cache.json`** | **Rich and free.** 9 models with slug, display name, description, `default_reasoning_level`, `supported_reasoning_levels`, `visibility`, service tiers. Carries `etag` + `fetched_at`; the Codex CLI keeps it current. |
| **`~/.claude.json` → `additionalModelOptionsCache`** | **Thin but authoritative.** Model options beyond the built-in aliases, each with `value`, `label`, `description`, and a `disabled` flag (observed: *"Opus 5.5 (disabled) — Update to 2.1.280+"*). |
| `~/.gemini/` | Config and settings only, no model cache. `gemini` is not on PATH. |

### Two conclusions

**Harness-local caches beat both the API and the docs.** They are structured, free,
require no credentials, are refreshed by the vendor's own CLI, and — decisively — they
describe *what that CLI will actually accept*, which is the only set routing cares about.

**`/v1/models` answers a different question.** It lists models available to an **API
org**, which is not the set Claude Code can run as. Using it to populate the Claude
candidate list would be wrong. It is useful only for *metadata enrichment*
(`max_input_tokens`, `max_tokens`, capabilities), and only if the user mints a key.

### Discovery tier order

1. **Harness-local cache** — codex, claude-code. Primary.
2. **Built-in aliases** — `opus`, `sonnet`, `haiku`, `fable`. Self-resolve to latest, so
   Claude tracks new releases with no discovery at all. Prefer aliases over pinned IDs
   for Claude routing.
3. **Provider API** — optional, behind a registered key. Enrichment only.
4. **Bundled catalog** — compiled into the binary, refreshed per release. Last resort.

Every registry row records which tier answered and when, so `orch model list` can show
provenance and age rather than implying uniform confidence.

---

## 3. The effort lever, and why it is asymmetric

Codex exposes reasoning effort as a first-class, per-model, enumerable property:

```
gpt-6-astra        default=low     low,medium,high,xhigh,max,ultra
gpt-6-sol          default=medium  low,medium,high,xhigh,max,ultra
gpt-6-luna         default=medium  low,medium,high,xhigh,max
gpt-5.5            default=medium  low,medium,high,xhigh
```

**The valid set varies by model.** `gpt-5.5` has no `max` or `ultra`; the `luna` variants
have no `ultra`. Effort cannot be a global enum — it must be validated per model.

Claude Code exposes **no per-agent effort knob**. The `Agent` tool takes `model` and
nothing else, and no agent frontmatter carries an effort field. The only related control
is a global `alwaysThinkingEnabled` boolean in `settings.json`.

**Consequences:**

- The routing unit is `{provider, model, effort?}`, with `effort` nullable and validated
  against that model's declared set. Claude rows carry none.
- The standing advice to *measure one model at varying effort before building a
  multi-model cascade* is **only testable on the Codex side today**. "Cheaper model at
  `max` vs. frontier model at `low`" is a real, cheap experiment there and unavailable
  on the Claude path.
- This is an argument *for* multi-vendor support, not a complication of it: Codex offers
  an axis Claude subagents do not have.

---

## 4. Provider support — three tiers

The question "which providers do we support?" should not be binary.

**Tier 1 — built-in adapter.** `claude-code` and `codex`. The adapter knows where the
local cache lives, how to parse it, and how to invoke the CLI. Zero configuration;
discovery is automatic. Adding a provider here requires a release.

These two are the defaults precisely because they are the two with a local,
credential-free cache. That is an empirical criterion, not a preference.

**Tier 2 — generic API adapter.** For any vendor exposing an OpenAI-compatible
`/v1/models`:

```
orch provider add together --models-endpoint https://api.together.xyz/v1/models \
                           --auth-env TOGETHER_API_KEY --invoke '<cmd template>'
```

No code change, no release. This is what keeps "anything else" from meaning "wait for us."

**Tier 3 — static manual list.** No discovery; the user maintains the list.

```
orch provider add local-llama --invoke 'llama-run --model {{model}} {{prompt}}' \
                              --models llama-70b,llama-8b
```

Covers local models and anything exotic.

**Unavailable providers are recorded, not errors.** Gemini registers as `unavailable`
with a reason (`CLI not on PATH`), so `orch provider list` explains why it is not
routable instead of silently omitting it.

---

## 5. Schema

New tables in `orch.db`, following the existing `sources` / `docs_owners` registry pattern.
Policy knobs live in `~/.config/orch/config.yaml`, matching how `sources.scan_interval`
already works.

```sql
providers(
  name           TEXT PRIMARY KEY,   -- claude-code | codex | ...
  kind           TEXT NOT NULL,      -- builtin | api | manual
  invoke         TEXT,               -- command template (tiers 2-3)
  models_endpoint TEXT,              -- tier 2
  auth_env       TEXT,               -- env var holding the key, never the key
  state          TEXT NOT NULL,      -- enabled | disabled | unavailable
  state_reason   TEXT,
  last_refreshed INTEGER
);

models(
  provider          TEXT NOT NULL,
  slug              TEXT NOT NULL,   -- alias or pinned id
  display_name      TEXT,
  description       TEXT,
  tier              TEXT,            -- frontier | balanced | fast
  default_effort    TEXT,            -- NULL where unsupported
  supported_efforts TEXT,            -- comma-separated; NULL where unsupported
  visibility        TEXT,            -- list | hide (vendor's own signal)
  source            TEXT NOT NULL,   -- cache | alias | api | docs | bundled
  last_verified     INTEGER,
  state             TEXT NOT NULL,   -- discovered | enabled | disabled
  PRIMARY KEY (provider, slug)
);

routing(
  task_kind  TEXT NOT NULL,
  rank       INTEGER NOT NULL,
  provider   TEXT NOT NULL,
  model      TEXT NOT NULL,
  effort     TEXT,
  rationale  TEXT,
  evidence_n INTEGER NOT NULL DEFAULT 0,
  decided_at INTEGER NOT NULL,
  PRIMARY KEY (task_kind, rank)
);
```

The existing `tasks` table gains the outcome columns (see §7):
`provider`, `model`, `effort`, `task_kind`, `turns`, `duration_ms`, `outcome`.

`visibility = 'hide'` is the vendor saying a model is not for general selection
(observed on `gpt-reserve`, `codex-auto-review`). Discovered, never a routing candidate.

---

## 6. Task kinds

A small closed vocabulary. Larger invites miscategorisation.

```
code · review · docs · research · ops · classify · test · adversarial
```

Orthogonal to agent identity. The dispatcher already picks `dev|ops|research|reviewer`;
kind is finer, and `dev` writing documentation should not route like `dev` writing a
parser.

Kind is inferred by the dispatcher from the task body and the chosen agent, and is
recorded on the task so outcomes can be attributed back to it.

---

## 7. Evidence

Two inputs, deliberately weighted differently.

**Declared capability** — from discovery. Model descriptions, tiers, effort ranges,
context windows. Available at n=0.

> Declared capability is vendor copy. It is a reasonable prior and a poor verdict. Its
> weight should decay as observed outcomes accumulate.

**Observed outcome** — from the task ledger. Needs an outcome label, which is the hard
part. Three sources, cheapest first:

1. **PR babysitter, already running.** It polls open PRs every 10 minutes and caches CI
   and review state. Whether a PR merged clean or needed follow-up commits is a real
   quality signal for `code` and `review`, obtained for free.
2. **Explicit rating** — `orch task <id> --rate good|rework|bad`. Accurate, requires effort.
3. **Turns and duration** — free, but measures speed, not correctness. Must not dominate
   the ranking, or every kind converges on the fastest model.

**Existing usage data:** `~/.claude.json` records `lastModelUsage` per project. Not a
quality signal, but a free prior on what the user actually reaches for.

### Honest state at launch

The ledger holds zero real tasks, and the dispatch worker is a stub (§8c). Every ranking
on day one is declared-capability opinion. `model_recommend` must return
`confidence: low, evidence_n: 0` and mean it. The system is only as good as outcome
labeling, and there is no free lunch there.

---

## 8. Materialization — how a decision takes effect

**(a) Per-spawn override — works today, build first.** The `Agent` tool accepts a `model`
parameter that overrides frontmatter. The dispatcher already returns a routing JSON; it
gains `provider`, `model`, and `effort`. No file writes, no new machinery.

**(b) Frontmatter sync.** `orch routing apply --write-agents` rewrites `model:` in
`~/.claude/agents/*.md` for invocations that bypass the dispatcher. Backs up first, and
honours a `# routing: pinned` marker so hand-edits survive.

**(c) Cross-harness execution.** Routing to Codex cannot use the `Agent` tool; it means
invoking that CLI. **This is the honest job for the workflow worker**, which today sleeps
three seconds and writes `"processed: <body>"` without running anything. It is already
durable and restart-surviving; it just has no real work. Cross-harness dispatch is that
work, and it is the point at which the task ledger stops being a ledger of nothing.

**Cost:** a Codex run has no `subagent-stop` hook, no shared context, and no streaming.
Exit code and stdout are all that come back. Worth it for genuine capability wins; not
worth it to chase a marginal benchmark.

---

## 9. Re-evaluation

Manual and autonomous share one code path.

```
orch model refresh                 # discovery only; never enables
orch routing evaluate              # proposes; shows the diff and evidence per row
orch routing evaluate --explain code
orch routing apply                 # writes, retaining the prior table
orch routing rollback
```

Autonomous is a **seventh supervised subsystem**, `routing-evaluator`, alongside the six
orchd already runs (`qdrant`, `workflow-worker`, `sources-scheduler`, `docs-scheduler`,
`pr-babysitter`, `self-update`). Cadence from config, default weekly.

**It proposes; it does not apply.** The default is to write a proposal and emit an event.
That event surfaces in the next session's startup digest — the same channel that already
reports PR state — as *"1 new model discovered, 2 routing changes proposed."*
`routing.auto_apply: true` opts into unattended changes.

**Discovery never enables.** A newly discovered model lands in state `discovered` and is
inert. `orch model enable <provider>/<slug>` is the gate. A vendor shipping a model can
never silently redirect the user's work — which matters precisely because a daemon is in
the loop.

---

## 10. CLI surface

```
orch provider add|list|enable|disable|remove
orch provider auth set <name>        # stdin → Keychain; never echoed
orch provider auth status            # set / not set, never the value

orch model refresh [--provider <p>]  # discover; report new as `discovered`
orch model list [--task-kind <k>] [--provider <p>] [--state <s>]
orch model enable|disable <provider>/<slug>

orch routing evaluate|apply|rollback|show
orch task <id> --rate good|rework|bad
```

One new MCP tool:

```
model_recommend(task_kind, constraints?)
  → { provider, model, effort?, rationale, confidence, evidence_n }
```

So any session can ask what to use and get the evidence count attached.

---

## 11. Phased build

**Phase 1 — registry and discovery.** `providers` / `models` tables, the two built-in
adapters reading local caches, `orch provider`, `orch model`, `orch model refresh`.
Self-contained; useful immediately as an inventory even before anything routes.

**Phase 2 — routing and recommendation.** `routing` table, task kinds, a declared-capability
policy, `orch routing evaluate|apply`, the `model_recommend` MCP tool, and per-spawn
override in the dispatcher (§8a). End of this phase, routing works for Claude Code.

**Phase 3 — evidence.** Ledger outcome columns, PR-babysitter outcome attribution,
`orch task --rate`, and evidence-weighted ranking that overrides declared priors once
`evidence_n` crosses a threshold.

**Phase 4 — autonomy.** The `routing-evaluator` subsystem, proposal events in the digest,
`auto_apply`, rollback.

**Phase 5 — cross-harness execution.** Real work in the workflow worker: invoke Codex,
capture outcomes into the ledger. Largest and riskiest; deliberately last, because
Phases 1–4 make it measurable before it is built.

Phases 1–2 deliver a working single-vendor system. Phase 5 is what makes multi-vendor
real, and by then there is data to justify it.

---

## 12. Open questions

1. **Outcome labeling** is the load-bearing unknown. If the PR-babysitter signal proves
   too coarse and explicit rating proves too tedious, evidence-weighted ranking degrades
   to latency-ranking, which is worse than declared priors. Validate in Phase 3 before
   building Phase 4 on top of it.
2. **Alias vs. pinned IDs for Claude.** Aliases self-update, which is a feature for
   tracking releases and a hazard for reproducibility — the model behind `opus` changes
   under a fixed routing table. Probably: route by alias, record the resolved ID on the
   task so outcomes stay attributable.
3. **Effort for Claude** may appear later. The schema already allows it; nothing else
   should assume it is absent.
4. **Cost.** No pricing source is credential-free today. Until a key is registered,
   "cost per completed task" is not computable, so Phase 3 ranks on quality and latency
   only.
