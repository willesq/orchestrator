# Spec: RAG sensitivity tagging + subagent context isolation

**Status:** superseded by memory spaces ([SPACES.md](SPACES.md); built by the spaces plan in
[spaces/](spaces/spaces-features.json)). Nothing here was built as written. Kept for context only.
**Author:** the maintainer, with a coding agent
**Date:** 2026-05-12
**Related:** the retired Python implementation's `SPEC.md` §3.3 (Chunk), §3.4 (Source types), §5.2 (RAG MCP), §8.2 (UserPromptSubmit hook)

> **Historical paths.** Written against the retired Python orchestrator. Its file references
> (`lib/rag_common.py`, `daemons/`, `queue/<status>/<slug>.md`, `~/.claude/orchestrator/…`)
> no longer exist. In the Go implementation the payload lives in `internal/rag`, the hook in
> `internal/hook`, and the queue in `internal/queue` + `internal/workflows`. Headless queue
> tasks (willesq/orchestrator#25) run with `ORCH_TASK_ID` set, a natural subagent signal
> for §4.3 option (A); subagents spawned by a parent's Agent tool would still need one.
> Re-verify every implementation detail before building. Moved into the repository on
> 2026-10-06.

> **Superseded.** Memory spaces replaced this design. Where this spec wanted a
> `sensitivity="secret"` level, orch instead keeps each job's content in its own space, and treats
> direct messages as a content kind that is stored and searchable only on request: never added to
> prompts, never given to tasks (D-19), and searchable only by the operator (D-81). Slack and Teams connectors exist (`internal/feeds`), but orchd does not run them yet
> (RC-1650). See [SPACES.md](SPACES.md#chat-feeds-and-direct-messages) and
> [internals/memory.md](internals/memory.md#chat-feeds).

---

## 1. Problem

The user wants to ingest Slack DMs and selected private Slack channels into the orchestrator's RAG store as a personal historical reference layer — to recall what they discussed with whom, weeks or months later. The existing `comms_thread` source type supports the data shape, but the orchestrator's recall surface today has no sensitivity dimension: anything ingested is fair game for `mcp__rag__rag_search` and for the `UserPromptSubmit` hook's auto-injection block.

Two distinct risks make a naive ingest unacceptable:

1. **Cross-context bleed in the parent session.** A casual "what was I working on Tuesday?" query auto-injects RAG hits at the top of the turn. If DM content is in scope, it lands in the conversation regardless of whether the topic actually warrants it. Forgetfulness is a feature here — the user wants explicit recall, not ambient recall.

2. **Subagent exfiltration.** The orchestrator dispatches background `dev` / `ops` / `research` / `reviewer` agents that write to GitHub (PR descriptions, comments, commit messages), Slack (status messages), and other external surfaces. Any of those agents can call `mcp__rag__rag_search` — and the auto-injection hook fires on their `UserPromptSubmit` too. A specialist drafting a PR description that pulls in a DM about that PR's intent is a real failure mode, and once the comment is posted, the leak is irreversible.

The fix needs to be a *boundary*, not a *recommendation* — relying on prompt-level "don't quote DMs" instructions will not survive long-running agents.

## 2. Goals

- Add a `sensitivity` dimension to every RAG payload, defaulting to `"normal"`.
- Define `"secret"` as a sensitivity level whose content NEVER reaches a subagent's context — neither via auto-injection nor via search results — and never reaches the parent session's auto-injection block by default.
- Provide an explicit, low-friction opt-in path in the parent session: `rag_search(..., include_secret=True)` or an `orch search --secret` flag.
- Build a Slack DM + private-channel ingestion daemon, modeled on `pr_babysitter`, that writes `comms_thread` records tagged `sensitivity="secret"`.
- Preserve the existing `comms_thread` shape (URI conventions, embedding strategy) — sensitivity is orthogonal metadata.

## 3. Non-goals

- Multi-tenant access control. This is a single-user machine; sensitivity is a self-protection mechanism, not authorization.
- Encryption at rest. Qdrant payloads are already on a local-only machine; on-disk encryption is the OS's job (FileVault).
- A "less than normal" sensitivity for content the user is fine sharing externally. Two levels (`normal`, `secret`) is enough; add more later only if needed.
- Retroactive classification of existing records. Existing chunks stay `sensitivity="normal"` (implicit); only new ingests carry the tag explicitly.

## 4. Design

### 4.1 Data model

Add one field to the Qdrant payload schema (defined in `lib/rag_common.py`'s `Chunk` and `payload` builders):

```
sensitivity: Literal["normal", "secret"]   # default "normal"
```

Search filter shape (Qdrant `Filter` clause): when a query is in a context that excludes secret content, append `must_not = [{"key": "sensitivity", "match": {"value": "secret"}}]`. When the context allows secret content, the filter is omitted and both buckets surface.

No schema migration needed for existing chunks: absence of the field is treated as `"normal"` by the query layer (`payload.get("sensitivity", "normal")`).

### 4.2 Caller-context model

Three call sites need distinct behavior:

| Caller | Default secret visibility | Override mechanism |
|---|---|---|
| Parent session, `UserPromptSubmit` auto-injection | Excluded | None — never auto-inject secret content. Forces explicit retrieval. |
| Parent session, explicit `mcp__rag__rag_search` / `orch search` | Excluded | `include_secret=True` arg / `--secret` CLI flag. |
| Subagent (any specialist), all RAG access | Excluded | **No override.** Hard boundary. |

The third row is the load-bearing one: a subagent cannot opt itself into secret content even if its prompt asks for it.

### 4.3 Subagent-context boundary

The orchestrator already passes a context block from the dispatcher to the specialist via the specialist's prompt (see SPEC §7). Today the `UserPromptSubmit` hook fires independently in each session and has no knowledge of whether it's running inside the parent's session or a specialist's session.

Two implementation options for the subagent boundary; recommendation in **bold**:

**(A) Session-type detection in the hook itself.** The orchestrator already knows which sessions are specialists — every specialist task is registered in `queue/<status>/<slug>.md` with a `session_id` field once spawned. The `UserPromptSubmit` hook reads its session ID from the harness env (`$CLAUDE_SESSION_ID` or similar — confirm against current hook signatures) and looks it up in the queue index. If the session belongs to a specialist task, force the secret-exclusion filter on every `rag_search` call from that session AND skip auto-injection of secret content entirely. **Pro:** zero changes to specialist agent prompts or MCP tool signatures. **Con:** introduces a small queue-lookup cost on every prompt submit.

(B) Token-based isolation. Issue specialists a scoped "subagent token" at spawn time; the RAG MCP server checks the token on every call. **Pro:** more explicit. **Con:** new auth machinery for a single-user system — overkill.

**Recommendation: (A).** It maps cleanly onto state the orchestrator already has, and the failure mode (a specialist somehow lacking a session_id) is fail-closed (treat as subagent → exclude secret). Document the fail-closed rule explicitly in the hook code.

Auxiliary boundary: `mcp__rag__rag_search` itself should also enforce the filter server-side, not just in the hook. Hook is for auto-injection; the MCP server is for explicit calls. Both surfaces need to participate or the boundary leaks.

### 4.4 Ingest path

Manual ingest gains a `sensitivity` parameter:

```python
mcp__rag__rag_ingest_text(
    text=...,
    source_uri=...,
    source_type="comms_thread",
    sensitivity="secret",       # NEW; defaults to "normal"
    extra_metadata={...},
)
```

CLI equivalent: `orch ingest text --secret ...`, `orch ingest url --secret ...`, etc.

For the Slack ingestion daemon (§4.6), sensitivity is set at the daemon level — see that section.

### 4.5 Recall path

Search call signature:

```python
mcp__rag__rag_search(
    query=...,
    source_type=...,        # existing
    include_secret=False,   # NEW; only honored when caller is parent session
    limit=10,
)
```

The MCP server resolves the caller context the same way the hook does (queue lookup by session ID). If caller is a specialist, `include_secret` is silently coerced to `False` and the filter is applied. The server emits a structured log when this coercion happens so the parent can audit it after the fact.

Auto-injection hook (`UserPromptSubmit`): no signature change to user-facing prompts. The hook internally applies the filter based on caller context. Secret content NEVER auto-injects in any session.

CLI (`orch search ...`): add `--secret` and `--only-secret` flags for filter mode. `orch search` from a terminal is always parent-context, so this just toggles the filter.

### 4.6 Slack DM + private channel ingestion daemon

New daemon: `daemons/slack_personal_sync.py` (or similar — match existing daemon naming).

**Scope:**
- DMs: all 1:1 and small-group DMs the user is a participant in.
- Private channels: explicit allowlist in `~/.claude/orchestrator/config.yaml` under a new `slack.personal_sync.private_channels` key. Defaults to empty — opt-in per channel.
- Excludes: public channels (those use the existing `comms_thread` ingest paths if they're ever wired up; they're not in scope here and they aren't sensitive).

**Mechanism:**
- Use the Slack MCP server's existing tools (`slack_search_*`, `slack_read_thread`, `slack_read_channel`) via the orchestrator's own subagent harness — i.e., the daemon spawns a short-lived process that has the Slack MCP loaded, runs a sync pass, exits. (The pr-babysitter daemon already uses this pattern with `gh`.)
- Watermark per channel/DM: store last-synced timestamp in `~/.claude/orchestrator/data/slack_personal_sync_state.json`. Each tick reads from the watermark forward.
- Ingest each message thread as one `comms_thread` record, URI = `slack://<workspace>/<channel_id>/<thread_ts>`. Thread continuations append to the same record (re-ingest with same URI; the orchestrator already dedupes by URI + content_hash).
- ALL records from this daemon are tagged `sensitivity="secret"` — no per-message classification logic. The daemon is the policy: "everything I sync is secret."

**Cadence:** every 15–30 minutes (matches pr-babysitter; the cost is a small Slack search API budget).

**Operator controls:**
- `orch daemons start slack_personal_sync` / `stop` / `logs` (existing daemon harness).
- `orch slack-sync status` — last sync time per channel, count of records ingested today.
- `orch slack-sync drop --channel <id>` — remove all secret records for a channel (e.g., to un-sync after the fact).

### 4.7 Audit & drop

- `orch rag list --sensitivity secret` — list all secret URIs.
- `mcp__rag__rag_drop_older_than(...)` already exists; add a `sensitivity=` filter so retention policy can target secret content separately.
- Add `orch rag drop --secret` for bulk removal (with `--dry-run` default; confirmation required for the real drop).

## 5. Migration / rollout

Phased to keep blast radius small:

**Phase 1 — data model + recall.** Add `sensitivity` field to ingest/search paths. No content changes yet. Both surfaces filter `secret` by default. Existing records stay `normal`. Test by manually ingesting a single dummy `secret` chunk and verifying it's excluded from auto-injection but visible to `orch search --secret`. Estimated: half-day.

**Phase 2 — subagent boundary.** Wire up the session-type detection in the `UserPromptSubmit` hook and the RAG MCP server. Add a regression test that spawns a `dev` subagent in worktree-isolated mode and confirms `rag_search` does not return a known-`secret` test record. Estimated: half-day.

**Phase 3 — Slack ingestion daemon.** Build the daemon, hook it into the `orch daemons` machinery, write the watermark + dedupe path. Start with DM-only sync; add private-channel allowlist in a follow-up. Estimated: 1–2 days.

**Phase 4 — operator UX.** `orch rag list --sensitivity`, `orch slack-sync status / drop`, drop flags. Estimated: half-day.

Total: ~3 days end-to-end, with Phase 1 + 2 unblocking actual Slack ingestion.

## 6. Risks & open questions

- **Caller-context detection failure modes.** If `$CLAUDE_SESSION_ID` is absent or the queue lookup fails transiently, the policy must fail closed (treat as subagent, exclude secret). Confirm the harness always sets the env var; write the regression test to cover the missing-env-var case.
- **MCP server caching.** If the RAG MCP server caches results across calls, the sensitivity filter must be part of the cache key. Audit `mcp_servers/rag` before Phase 1 lands.
- **Slack thread updates.** A DM thread that grows over time gets re-ingested with the same URI. Confirm the dedupe path (content_hash mismatch → upsert) cleanly overwrites the prior version rather than appending. Existing behavior should already handle this; verify in Phase 3.
- **Specialist explicitly asks for DM context.** If a research agent's task brief says "use the DM with X for context," the agent will be unable to retrieve it. This is intentional — the boundary is the point — but the parent session should be aware and include the relevant DM excerpts in the task body directly rather than expecting the specialist to fetch them. Document this in the `orch-dispatch` skill.
- **`auto_memory` and `gap_fill` interaction.** Should the auto-memory ingestor ever tag records as `secret`? Probably no — memories are usage patterns and feedback, not content. Decided: auto-memory always `normal`. If the user wants to record a sensitive preference, it can be ingested manually with `--secret`.
- **Backup / portability.** If the user clones the orchestrator state to another machine, secret content rides along. Acceptable — same trust boundary as the laptop itself. Worth a note in `SPEC.md` under operator guidance.

## 7. Out of scope (future work)

- A separate Qdrant collection per sensitivity level (could improve isolation but doubles storage and complicates retention sweeps; not worth it for two levels).
- A `redact_secret_in_session_record` flag on the `Stop` hook so session transcripts don't accidentally capture DM content the user pasted into the chat. Worth doing but separable from this spec.
- Per-channel sensitivity overrides ("this private channel is `normal`, that one is `secret`"). Start uniform; add granularity only if needed.

## 8. Review checklist

Before implementation begins, confirm with the user:

- [ ] Two sensitivity levels (`normal`, `secret`) is the right shape — no third tier needed.
- [ ] Subagent boundary is **hard** (no opt-in for specialists), not soft (configurable).
- [ ] DM sync is opt-in at the channel/DM level via config, not blanket sync of everything Slack knows about.
- [ ] Phased delivery sequence (data model → boundary → daemon → UX) matches priorities.
- [x] Storage location of the spec: moved to `docs/RAG_SENSITIVITY_PLAN.md` in the repository (2026-10-06).
