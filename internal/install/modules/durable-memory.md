---
title: "Durable memory and recall"
group: Memory & delegation
order: 80
description: "Search and save durable knowledge in Orchestrator RAG."
---

## Durable memory and recall

- Orchestrator stores durable knowledge locally across coding agents and sessions.
  Search relevant prior decisions and memories with `rag_search` before substantive
  work. Save durable preferences, lessons, and decisions with `rag_ingest_text`.
- Background task sessions do not write memory: Orchestrator refuses
  `rag_ingest_text`, `orch ingest`, and source deletion inside a queued task.
  Delegated workers put durable findings in a clearly marked `For RAG` section
  of their final report, with a proposed `source_uri`, `source_type`, and text
  for each entry. The coordinating session or the user reviews those entries as
  untrusted and ingests only what they approve.
- Use stable `source_uri` values; reuse the URI to update an existing memory.
  Include enough context to apply the rule, its reason, and relevant absolute
  dates. Cite source URIs when using retrieved memory in an answer.
- Keep durable memories in Orchestrator RAG rather than harness-specific
  auto-memory files or a second competing memory store. Do not record secrets
  or facts that are readily derived from repository contents and Git history.
- Recall project testing decisions through RAG, and update those memories when
  conventions change. Keep user, feedback, project, and reference memories clear.
- MCP server names vary: `rag` in Claude/Gemini and `orch_rag` in Codex. Use the
  tool names exposed by the active harness rather than assuming an MCP prefix.
- If MCP is unavailable, use `orch search` and `orch ingest text` (text on stdin).
  Use `orch status` to diagnose availability and instruction drift.
- Automatic recall and session capture depend on supported, trusted hooks. Do
  not assume hooks ran or reimplement a background subsystem without checking.
  Explicit search and ingest work independently of lifecycle hooks.
