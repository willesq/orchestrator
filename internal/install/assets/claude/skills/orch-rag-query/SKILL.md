---
name: orch-rag-query
description: |
  Use when the user asks about content stored in the orchestrator's RAG knowledge base, prior session memory, indexed docs, OR anything about past activity / past work / past decisions. Triggers include phrases like:
  "what is in the rag store", "what do we know about X", "have we indexed Y", "do we have docs on Z", "search our knowledge", "what did we decide about <topic>", "find me anything about <subject>", "list rag sources", "what sources are indexed", "show me what's in rag".
  PAST-ACTIVITY triggers (always-RAG-first — never shell out to git log or filesystem first):
  "what did i work on today", "what have i been doing", "what did i do yesterday", "recap this week", "what changed last <day|week>", "what was i working on", "what happened in <session|task>", "what did <agent> do", "remind me what we figured out about X", "summarize last session", "what was the last thing we discussed".
  Also activate when the user asks a substantive question that could benefit from prior session memory before you answer from scratch.
allowed-tools:
  - mcp__rag__rag_search
  - mcp__rag__rag_ingest_text
  - mcp__rag__rag_list_sources
  - Bash
---

<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# orch-rag-query — Retrieve from the orchestrator RAG store

When the user asks something that could be answered (or contextualized) by indexed knowledge, query the RAG store first and cite source URIs.

## Decision tree

| User intent | Tool | What to surface |
|---|---|---|
| "What's in the rag store?" / "What's indexed?" / "List sources" | `mcp__rag__rag_list_sources` | Returns distinct source URIs with chunk counts — the store's actual inventory. Filters: `source_type`, `scope`, `uri_prefix`, `limit`. |
| "What do we know about <topic>?" / "Search rag for X" | `mcp__rag__rag_search` with `query=<topic>`, `top_k=5` | One bullet per hit: source URI, source_type, distance, 1–2 line snippet. |
| "What did I work on <today\|yesterday\|this week>" | `mcp__rag__rag_search` with `query="work activity decisions"`, `top_k=10`, and `since`/`until` for the period (e.g. `since="yesterday", until="yesterday"`, or `since="7d"`); `mcp__rag__rag_list_sources` with `uri_prefix="session://"` and the same range lists every session in it | Dates are UTC: convert the user's "yesterday" to UTC bounds when their time zone matters. Each hit shows when it happened, and transcript turns are marked like `[2026-10-21T14:02Z]`, so a long session's parts are dated separately. Group by date, records first, then transcripts. Cite each `session://<id>/record` URI. Sessions marked "date unknown" predate dating and are left out of ranged searches; mention them only if nothing dated answers. |
| Substantive question where prior context could matter | Search-then-learn, two steps — see below | Lead with the hits when they're useful; otherwise say so and fall back, then backfill. |

## Search-then-learn (the two-step)

There is no single "search or learn" tool. Do it explicitly:

1. `mcp__rag__rag_search` with `query=<the question>`, `top_k=5`.
2. Judge the hits yourself — there is no `gap` flag to read.
   - **Useful hits:** lead with them, cite the URIs, ground your answer in them.
   - **Nothing relevant:** say "Nothing relevant in RAG; answering from general knowledge," then answer.
3. If you learned something durable while answering the gap, backfill it:
   `mcp__rag__rag_ingest_text` with `source_type='gap_fill'` and a stable `source_uri`.
   Inside a queued task (`ORCH_TASK_ID` set) the ingest is refused; put the entry in the
   `## For RAG` section of your final report instead.

Step 3 is the only ingest this skill may perform, and only to close a gap it just
hit. Everything else belongs to `orch-rag-ingest`.

## Output rules

- ALWAYS cite source URIs inline in your answer (`[file://… ]` or `[https://…]`) so the user can trace claims.
- ALWAYS prefer RAG hits over general knowledge when both are available.
- If RAG returns no useful hits AND you don't have a confident answer, say so. Don't fabricate.
- Don't dump full chunks unless the user asks. Snippet first, full content on request.
- For registry output, pass `orch source list` / `orch repo list` through as-is; don't
  reformat it into a chunk-count table the data can't support.

## Inventory vs registry — keep these straight

- `mcp__rag__rag_list_sources` (or `orch rag sources`) — **what is indexed**. Distinct
  source URIs and their chunk counts, read from the vector store itself.
- `orch source list` / `orch repo list` — **what gets refreshed**. The cadence registry.
  A source can be registered but not yet ingested, or ingested but never registered.

Answer "what's in the RAG store?" with the first. Don't present the registry as the
index.

## When NOT to use this skill

- The user explicitly says "skip rag" / "don't search memory" / "answer from scratch".
- The question is about Claude Code itself (model behavior, capabilities) rather than the user's project.
- The user is asking you to *write* code rather than retrieve context (use rag in passing if helpful, but don't make this the focus).

## Hard rules

- NEVER ingest from this skill except the step-3 `gap_fill` backfill above. Bulk or
  user-directed ingestion is `orch-rag-ingest`'s job.
- NEVER present `orch source list` / `orch repo list` as the contents of the store.
  They are the refresh registry; conflating them overstates what's indexed.
- NEVER fabricate source URIs in citations. If you didn't see it in the RAG response, don't cite it.
- NEVER call `rag_search_or_learn`. It does not exist — search, judge the hits, then
  ingest as two explicit steps.
