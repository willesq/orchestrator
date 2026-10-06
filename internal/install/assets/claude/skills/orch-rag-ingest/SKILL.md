---
name: orch-rag-ingest
description: |
  Use when the user asks to add content to the orchestrator's RAG knowledge base. Triggers include:
  "ingest this <url|file|path>", "add this to rag", "add this to memory", "remember this", "index this", "save this for later", "drop this in the knowledge base", "onboard <url> into rag", "load these docs into our knowledge base".
  Use also when the user pastes a URL or file path AND asks you to retain it for future reference.
allowed-tools:
  - mcp__rag__rag_ingest_text
  - mcp__rag__rag_list_sources
  - mcp__rag__rag_delete_source
  - Bash
  - Read
---

<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# orch-rag-ingest — Add content to the orchestrator RAG store

Used for *explicit* user-driven ingestion. Background schedulers handle registered
sources and session transcripts automatically — this skill is for "I want this
specific thing in the index now".

## The MCP surface

- `mcp__rag__rag_ingest_text(text, source_uri, source_type, scope)` — the only ingest tool.
- `mcp__rag__rag_list_sources(source_type, scope, uri_prefix, limit)` — what is indexed.
- `mcp__rag__rag_delete_source(source_uri)` — permanently removes a source's chunks.

`rag_ingest_url` and `rag_ingest_path` **do not exist**; a call fails with
tool-not-found. Fetching and reading are yours to do — then ingest the text.

## Decision tree

| User intent | How | Notes |
|---|---|---|
| "Remember this: <pasted text>" / "Index this snippet" | `mcp__rag__rag_ingest_text(text=…, source_uri=…, source_type='manual')` | Generate a stable URI like `note://<YYYY-MM-DD>-<topic>` so future searches and citations work. Tell the user the URI you used. |
| "Ingest this URL: <u>" | `orch source add web <u> --refresh 24h` | Registers the page; the sources scheduler fetches it, strips HTML, and ingests on its next scan (every 5m). Not instant — say so. One URL per call; it does **not** crawl links. |
| "Ingest this file: <p>" | Read the file, then pipe it: `cat <p> \| orch ingest text --source-uri file://<abs-path> --type file` | `orch ingest text` reads **stdin only**. There is no path-ingest command. Absolute paths always. |
| "Ingest this repo's docs" | `orch repo add <path> --refresh 24h` | Markdown only, on a cadence. Hand off to `orch-repo-onboard` for the full flow. |
| "Forget about <uri>" / "Remove <source> from rag" | `mcp__rag__rag_delete_source(source_uri=<uri>)`, or `orch rag forget <uri> --yes` | Permanent, with no undo. Confirm the exact URI first with `rag_list_sources` — a typo silently deletes nothing, and the right URI deletes everything under it. Note this does **not** de-register a cadence source; pair it with `orch source remove` or the scheduler re-ingests. |

## Workflow

1. **Identify the source.** URL → use directly. Pasted text → derive a short slug from
   the first heading or the user's stated topic.
2. **Pick the route** from the table. Prefer `rag_ingest_text` when you already have the
   content in hand — it's synchronous and you can report the chunk count immediately.
3. **Report what happened**: chunks ingested + the source URI, e.g.
   `Ingested 8 chunks under note://2026-09-30-qdrant-tuning`.
   For `orch source add`, report that it's *registered* and will be picked up on the
   next scan — do not claim it's indexed yet.
4. If multiple sources were requested in one message, summarize as a list at the end.

## Re-ingesting the same URI

Safe and idempotent. Point IDs are a deterministic UUIDv5 of
`<source_uri>#<chunk_index>` and writes are create-or-replace, so chunks overwrite in
place. If the new version is shorter, the surplus trailing chunks are pruned
automatically — a shrinking rewrite no longer leaves stale text behind.

## Remaining gap

**No one-shot URL or path ingest.** `rag_ingest_url` / `rag_ingest_path` don't exist;
use the workarounds in the table above. That's a feature request against the Go MCP
server, not something to improvise around.

## When NOT to use this skill

- User wants to *crawl* a site or recursively pull linked pages → that's the `research`
  subagent. Recommend `/orch-dispatch ingest <url> and follow related links`.
- User wants a permanent watch on a directory → register it with `orch source add` or
  `orch repo add`. (The old `ingestion.watch_dirs` config key is gone along with the
  Python-era config file; the live config has no such key.)
- The session-end hook captures the transcript automatically — never ingest
  "this conversation".

## Hard rules

- ALWAYS use absolute paths. If the user gives a relative path, resolve it against their
  `cwd` and confirm before ingesting.
- ALWAYS report the source URI so the user can find it later, and so a future
  `rag_delete_source` has something exact to target.
- NEVER ingest content you didn't see ("ingest the AWS docs" without a URL → ask which).
- NEVER delete without the user asking for that specific URI. Deletion is permanent,
  there is no undo, and `rag_delete_source` removes every chunk under the URI at once.
- The live config is `~/.config/orch/config.yaml`, managed with `orch config get|set|list`;
  edit it through those commands rather than by hand.
- NEVER set `source_type` outside the closed set (`web`, `file`, `manual`, `gap_fill`,
  `comms_thread`, `repo_doc`, `session_record`, `session_transcript`, `auto_memory`).
