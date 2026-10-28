# Memory and retrieval

## What gets remembered

| Source | How it arrives | Stored as |
|---|---|---|
| Your sessions | At session end; `orchd` stores the redacted transcript in the background, tagged with the checkout path | `session_record`, `session_transcript` |
| Registered repos and GitHub owners | Markdown ingested on a schedule (`orch repo add`, `orch source add owner <name>`) | `repo_doc` |
| Web pages | Fetched on a schedule (`orch source add web <url>`) | `web` |
| Anything else | `orch ingest`, or the `rag_ingest_text` MCP tool (not from queued tasks) | your choice |

Every stored chunk records when it was stored. Session transcripts mark each turn with its UTC
time, such as `[2026-10-21T14:02Z] user: …`, and each chunk is dated by the turns it holds, so a
session spanning days is found under the day each part happened. Session records carry the
session's start and end. Search and source listings show these dates, and `--since`/`--until`
(or `since`/`until` on the MCP tools) keep only content dated in that range: a date
(`2026-10-21`), an RFC 3339 time, `today`, `yesterday`, or an age like `7d`. Undated content is
left out of ranged results. `orch rag backfill-sessions --apply` dates sessions stored before
this, from their transcripts still on disk.

Session records start as an excerpt; `orchd` then replaces them with a summary written by a small
model (`sessions.summarize.*`, default `haiku`). Directories in `ingest.exclude_dirs` (vendor,
node_modules, build output and similar) are never walked. Redaction is pattern-based and does
not catch every secret shape.

## What the agent sees

- **Prompt context.** On each prompt, `orch-hook` searches memory and prepends up to three
  pointers: the current project's docs and sessions first, then the whole store, one per
  source. Questions need a score of `hooks.auto_rag.min_score` (0.25); instructions need
  `min_score_statement` (0.45). A chunk is injected at most once per session. Each pointer is a
  title, date, source URI, how far its origin is trusted, and the single most relevant line (for
  a session record, usually the decision itself); the agent reads the rest with `rag_search` and
  that `source_uri`. `hooks.auto_rag.format: full` injects whole chunks instead. Pointers cost
  about a third fewer tokens than whole chunks on the golden set (253 against 383).

  ```
  [1] "session record" · 2026-10-27 · "session://…/record" · own session
      "Author trust comes from GitHub's authorAssociation, not from PR text."
  ```
- **Trust.** Each memory is labelled by which part of orchd stored it: a registered repo's docs
  (`repo doc`), a registered web page (untrusted), your sessions (`own session`), a file an agent
  edited, or something stored through `orch ingest` or the MCP `rag_ingest_text` tool (`stored by
  a client`, whatever type or URI it claimed). Everything retrieved is shown quoted, as data;
  text in any of them that reads like an instruction to the agent ("ignore previous
  instructions", "you must run …") is flagged. `hooks.auto_rag.untrusted: exclude` keeps web pages
  out of prompts. This is a guard, not a wall: the flag is a pattern list, and memory stored before
  this release is labelled `unverified` until it is stored again.
- **Updates.** Each prompt notes, once, which subagents and background tasks finished since the
  last one.
- **Resume brief.** After Claude Code compacts a session, the next session start restores the
  branch, uncommitted files, recent commits, and the last request and reply.

## Chunking

Text is split into chunks of at most `embedder.chunk_tokens` (110) model tokens, because the
embedding model reads only its first 128. Markdown splits at headings and keeps code blocks
whole where they fit; a list, code block or paragraph too large for one chunk splits at line
boundaries, so its lines survive. Each chunk starts with its heading path, such as
`[section: Install › Homebrew]`. Plain text uses windows overlapping by
`embedder.chunk_overlap_tokens` (16).

## Search

Each query runs a semantic search and a BM25 keyword search in parallel and merges them with
reciprocal rank fusion, so exact identifiers like `queue.worker.verify.repos` are found even
when their wording scores low by meaning.

| Setting | Default | Meaning |
|---|---|---|
| `rag.search.mode` | `hybrid` | Or `dense` for meaning only |
| `rag.search.rrf_k` | `60` | Fusion constant; larger flattens the top ranks |
| `rag.search.keyword_weight` | `0.5` | Keyword list's share; 0.5 scored best on the eval |

- **Ranking.** After fusion, scores are weighted by source type and age: session records and
  docs over raw transcripts (`session_transcript` 0.85) and web pages (0.9), and newer over older,
  halving toward a floor of 0.85 every 180 days, so an old decision that matches well is not
  buried. `orch search --ranking plain` turns this off for one search.
- **Diversity.** One source returns at most two hits to any search (`rag.search.per_source`,
  so MCP and `orch search` too); prompt context takes one per source. `--per-source -1` lifts the cap for one search.
- **Dates.** Everything stored is dated: repo docs by their last commit, sessions by when they
  ran, web pages and notes by when they were stored. So `--since` and `--until` find docs and
  notes too.
- Every hit keeps its cosine score, so the prompt-context floors mean the same in both modes.
- A hit containing a code identifier from the prompt needs only half the floor; one per prompt.
- Each source can place at most two chunks in the keyword results.
- If keyword search can't run, search falls back to meaning only and says so; an explicit
  `--mode hybrid` fails instead.

```sh
orch search "where is sync_meta kept"     # hits marked "exact" or "keyword"
orch search --mode dense "where is sync_meta kept"
orch search --require-tag owner:willesq --require-tag repo:orchestrator "install"   # one repo's docs
orch search --uri repo://willesq/orchestrator/README.md "install"                   # one doc
orch search --ranking plain --per-source -1 "install"  # fusion order, no cap
orch rag show repo://willesq/orchestrator/README.md   # the doc as memory holds it
```

`orch rag show` rebuilds a source from its chunks: in order, without their section context
lines, with the overlap between chunks trimmed. It is what retrieval sees, close to but not
exactly the original.

## Measuring retrieval

`orch rag eval` scores search against a golden set of queries and the sources that should
answer them: recall@1/3/5/10, MRR and nDCG@10 by where the first chunk of a right source ranks,
plus the tokens prompt context would inject and search latency, overall and per section. Use it
before and after changing chunking, ranking or the model; `--compare` runs a paired bootstrap, so
you can tell a real gain from noise.

```sh
orch rag eval seed                        # build seed cases from your store, no model calls
orch rag eval add "which PR fixed the hand-off race" "session://<id>/*"   # a hard case
orch rag eval --save before
orch rag eval --compare before            # deltas, moved cases, 95% intervals
orch rag eval --ranking plain --per-source -1 --save fusion   # score other settings
```

The set is `eval/golden.jsonl` in the data directory, one JSON case per line: `query`, `expect`
(source URIs; a trailing `*` matches a prefix), and optionally `span` (text the answering chunk
must hold), filters, `section`, `note` and `split` (`tune` or `holdout`; by default a hash of the
query decides). Choose settings with `--split tune` and judge them with `--split holdout`. Seed cases come from your session records' decisions
and your repo docs' headings and identifiers, so they favour what the system already finds; keep a
`hard` section of questions in your own words beside them. An older `rag-eval.yaml` in the config
directory is used until the JSONL set exists and imported by the first seed.

## Cleaning up and re-indexing

```sh
orch docs refresh               # re-store docs: commit dates, writer and content hash
orch rag dedupe                 # list file:// copies identical to a synced doc
orch rag dedupe --apply         # delete them
orch rag reindex-keywords       # count chunks indexed by an older keyword tokenizer
orch rag reindex-keywords --apply   # rebuild their keyword vectors in place; resumable
```

Editing a doc in a registered repo (a repo or owner source, or a checkout of a repo the docs
sync indexes) stores a `file://` copy only when it differs from the synced copy: an edit on a
branch that is not merged yet stays searchable under its path, and once the synced copy matches
it, the path copy is removed at the next edit. Local repo and owner sources are indexed under
`checkout://<path>`, apart from the GitHub docs, and only while checked out on their default
branch. Docs removed from a repo are deleted at the next sync; docs stored before this release
stay undated (and unverified) until they are stored again.

## Migrating an older store

Stores created before hybrid search have no keyword index. Migration copies them into a new
collection, re-chunked and re-embedded, then switches to it and keeps the old one as a backup.

```sh
orch rag migrate-hybrid                        # plan only
orch rag migrate-hybrid --apply                # copy and switch
orch rag migrate-hybrid --rollback             # back to the backup; then orch restart
orch rag migrate-hybrid --drop-backup --apply  # delete the backup once you trust the new one
```

New ingests reach both collections while the copy runs, and an interrupted run can be repeated.
`orch status` shows progress. Writes after the switch exist only in the new collection, so a
rollback loses them. `--drop-backup` deletes only the backup recorded in config and refuses one
`orchd` is still using.
