---
name: orch-repo-onboard
description: |
  Use when the user wants to register a repo with the orchestrator so its markdown docs are ingested into RAG on a cadence, OR when you encounter a repo that hasn't been registered yet. Triggers include:
  "onboard this repo", "index this repo", "add this repo to rag", "register this repo", "ingest this repo's docs", "import <repo> into the orchestrator", "set up <repo> for the orchestrator", "make the orchestrator aware of <repo>".
  ALSO activate proactively when:
  - The user mentions a local repo path (e.g. `~/code/<org>/<name>`) AND that path isn't in `orch repo list` — check first, offer to register.
  - The user asks a docs-shaped question about a repo and `orch search "<q>" --tag repo:<name>` returns nothing — it probably isn't registered.
  - You enter a new working directory that's a git repo for the first time in this session and don't see it in `orch repo list`.
  Don't register silently — always confirm first, since it walks the filesystem and writes to RAG.
allowed-tools:
  - Bash
  - Read
  - mcp__rag__rag_search
---

<!-- managed-by: orch — installed by `orch install` from internal/install/assets in the orchestrator repo. Local edits are overwritten on the next install; change the repo copy instead. -->

# orch-repo-onboard — Register a repo for cadence doc ingestion

## What registration actually does

1. Adds a row to the `docs_repos` / `sources` tables in `~/.local/share/orch/orch.db`
   (path, refresh interval, scope, tags, `last_synced`, status).
2. On the sources scheduler's next scan (every 5m by default), walks the repo for
   **`*.md` files only**, skipping `.git/` and `node_modules/`.
3. Ingests each as `source_type=repo_doc` under
   `repo://<owner|local>/<repo-name>/<relative-path>`, tagged `repo:<repo-name>` (and
   `owner:<owner>` when known).

Registration is **not** an immediate ingest. `orch repo add` returns instantly; the
content lands on the next scan. Never tell the user their docs are searchable the
moment the command returns.

## Commands (these are the real ones)

| Situation | Command |
|---|---|
| Register a repo | `orch repo add <path> --refresh 24h` (optional `--scope`, `--tag`) |
| Is it registered? | `orch repo list` |
| Register a non-repo web/owner source instead | `orch source add <web\|repo\|owner> <uri>` |
| Force an earlier re-scan | `orch source refresh` — marks sources due on the next scan |
| Stop refreshing it | `orch source remove <kind>\|<uri>` — de-registers; **does not** delete indexed chunks |

There is no `orch repo onboard`, no `orch repo status`, no `--nick`, and no
`orch ingest path`. The repo's name is always `basename <path>`.

## Searching a registered repo

Repo association is a **tag**, not a payload field:

```
orch search "<your query>" --tag repo:<repo-name>
```

The MCP `rag_search` tool takes only `query`, `top_k`, `scope`, and `source_type` — it
has **no tag filter**, so it cannot scope to one repo. For repo-scoped search, shell out
to `orch search --tag`. Use `source_type=repo_doc` via MCP to at least narrow to docs.

To confirm a repo's docs were actually ingested, use
`mcp__rag__rag_list_sources` with `uri_prefix="repo://"` (or
`orch rag sources --prefix repo://`) — that reads the index, not the registry.

## Output format

After `orch repo add`, surface:
- The repo name it registered under (`basename` of the path)
- The refresh interval
- That ingestion happens on the next scheduler scan, not now
- The follow-up search command: `orch search "<query>" --tag repo:<name>`

Then confirm on a later turn with `orch status` — the **data** section's `docs owners` /
`sources` lines show registration counts and last-sync age.

## Secret redaction (verified, still active)

Before any file is ingested, `redact.Redact` replaces secret-shaped substrings inline
with `[REDACTED:<label>]`: RSA/PGP/OpenSSH private keys, AWS `AKIA…`/`ASIA…` access
keys, `aws_secret_access_key=…` values, GitHub `ghp_/gho_/ghu_/ghs_` tokens, Slack
`xox[baprs]-…` tokens, OpenAI/Anthropic `sk-…` keys, and JWTs. Surrounding doc context
survives; the key never reaches Qdrant.

Files exceeding `redact.Threshold` (**25** redactions) are skipped entirely — treated as
keystore dumps rather than docs. This is silent: nothing reports which files were
dropped. If the user cares, have them grep the repo themselves.

## When NOT to use this skill

- The path isn't a directory — `refreshRepo` marks the source `missing` and ingests
  nothing. Check first.
- The repo has no `.md` files. Only markdown is ingested — no `README` without an
  extension, no `.rst`, no `.txt`, no code.
- The user wants to ingest *code*. The orchestrator deliberately doesn't index active
  code. For a single file, pipe it: `cat <file> | orch ingest text --source-uri file://<abs> --type file`.
- The user wants to register a *remote* repo without cloning. `orch source add owner <login>`
  covers GitHub org/user markdown via the docs scheduler; otherwise clone first.

## Hard rules

- ALWAYS confirm before registering — it writes to the registry and, on the next scan, to RAG.
- ALWAYS set expectations that ingestion is deferred to the next scan.
- NEVER claim a file count after `orch repo add`. It hasn't walked anything yet, and the
  scheduler doesn't report per-file results back to you.
- NEVER confuse `rag_list_sources` (what is indexed) with `orch repo list` (what is
  registered for refresh). Use the former to check whether a repo's docs actually landed.
- NEVER promise that removing a source deletes its indexed content. It doesn't.
