# Memory: embedding, storage and search

Packages: `rag` (chunk, store, search, migrate), `embed`, `bm25`, `qdrant`, `redact`, `timeline`,
`sources`, `docs`, `mdwalk`, `model`, `rageval`. Every write goes through `rag.Service.IngestText`.

## Embedding

- **Model:** all-MiniLM-L6-v2, 384 dimensions, mean-pooled, L2-normalized. `model.onnx` from
  `Xenova/all-MiniLM-L6-v2` and `tokenizer.json` from `sentence-transformers/all-MiniLM-L6-v2`
  on Hugging Face, downloaded into `<data>/models/minilm` if missing (`orch model fetch`).
- **Runtime:** hugot feature-extraction pipeline over ONNX Runtime (cgo, build tag `ORT`); a
  build without the tag has a stub that errors. One ORT session per process, never destroyed,
  because re-initializing the ORT environment fails; pipelines are created and closed per load.
- **Lifecycle:** loaded on first use; a reaper (every 30 s) unloads it after
  `embedder.idle_unload` (10 min) idle.
- **Batching:** 32 texts per inference call. A whole large source in one call held over 10 GB;
  the lock is taken per batch so searches interleave. Closed embedders refuse to reload.
- **Token counting:** the model's own tokenizer (`tokenizer.json` with truncation and padding
  disabled, no special tokens).

## Chunking

The model reads 128 tokens, 126 after its special tokens, so chunks are at most
`embedder.chunk_tokens` (110), capped at 126. Text is first cleaned: invalid UTF-8 and control or
format characters removed (except tab and newlines), other Unicode spaces normalized.

**Token measure per whitespace word:** `max(tokenizer count, 1, ceil(runes/20))`; without a
tokenizer, an estimate (each Han, Hiragana, Katakana, punctuation or symbol rune is 1 token; each
letter or digit run of r runes, Hangul included, is `1 + (r−1)/3`).

**Plain text:** greedy windows of whole words, overlapping by `chunk_overlap_tokens` (16, at most
size/4), always advancing. A word too large for a chunk is split into the largest fitting pieces.

**Markdown** (when `embedder.markdown_sections` is on and an ATX heading exists outside a fence):

1. Split into sections at headings outside code fences, tracking each section's heading path.
   Titles are cleaned and capped at 120 runes.
2. Pack consecutive sections into one chunk while they share a subtree and fit the budget: text
   before the first heading stands alone; a top-level section absorbs only its descendants; any
   other section absorbs following sections under the same parent (siblings and their
   descendants). Heading lines are rewritten as `#… <title>`, with `›` and `>` in titles
   replaced by `-`.
3. Prefix each chunk with its ancestor path, `[section: Install › Homebrew]`, capped at half the
   chunk (dropping outer headings first). Budget = `max(size − prefix, size/2, 1)`.
4. A section too large for one chunk repeats its heading (capped at size/4) on every piece and
   splits at blank lines, never inside a fenced block unless the block alone is too large. A
   single block (paragraph, list, fence) too large for one chunk splits at line boundaries, so
   its lines survive, and consecutive pieces share whole trailing lines up to the overlap; only
   one line too large for a chunk falls back to word windows.
5. Blank-line runs collapse to one outside fences; a final pass re-splits anything still too large.

The chunk's `heading` payload is the deepest path all its sections share.

## Storage (Qdrant)

**Engine:** in `binary` mode orchd downloads the pinned release (`v1.17.1`,
`qdrant-{aarch64,x86_64}-apple-darwin` or `-unknown-linux-musl`) to `<data>/bin/qdrant` and runs it
on 127.0.0.1 (HTTP and gRPC) with telemetry and cluster mode off, storage in `<data>/qdrant`, and
the API key (`QDRANT__SERVICE__API_KEY`, sent by the client as the `api-key` header). Inherited
`QDRANT__*` variables are dropped, so orchd's environment cannot reconfigure the engine.
Readiness: `GET /readyz`.

**Collection (hybrid layout):**

```json
PUT /collections/<name>
{"vectors": {"dense": {"size": 384, "distance": "Cosine"}},
 "sparse_vectors": {"bm25": {"modifier": "idf"}}}
```

plus a keyword payload index on `source_uri`. A 409 "already exists" (a concurrent first write) is
treated as success. A legacy collection with one unnamed dense vector is the `dense` layout.

**Points:** ID = UUIDv5(URL namespace, `<source_uri>#<chunk_index>`), so re-ingesting a source
overwrites its chunks; afterwards chunks with `chunk_index >= new count` are deleted. Upserts go 256
points per request (`PUT /points?wait=true`), each point
`{"id", "vector": {"dense": [...], "bm25": {"indices": [...], "values": [...]}}, "payload"}`
(`bm25` omitted when empty; a bare vector in the dense layout). A failure after the first batch is
a partial write and names the source to re-ingest.

**Filters** are `{"must": [...]}` of `{"key", "match": {"value"}}` (scope, source_type),
`{"key": "tags", "match": {"any": [...]}}`, and `{"key", "range": {"gte"|"lte"}}` (dates). Deletes
refuse a filter with no `must` condition, since Qdrant reads an empty filter as "everything".

| Payload field | Value |
|---|---|
| `text` | chunk text, including any `[section: …]` prefix |
| `source_uri`, `source_type`, `scope`, `tags` | provenance; `source_uri` required |
| `chunk_index`, `chunk_total` | position |
| `heading` | shared heading path, if any |
| `started_at`, `ended_at` | Unix seconds the chunk describes (see below) |
| `ingested_at` | Unix seconds stored |
| `bm25_v` | tokenizer version that made the keyword vector |

**Dates.** Every chunk is dated, so date-range searches and recency see all content:

- transcript chunks by their own turns, within the session's span;
- session records by the session's span;
- repo docs (docs and sources syncs) by the file's last commit (`git log --name-only` over the
  repo's markdown, read once per repo; a `--depth 1` clone lists every file under its one commit,
  so docs synced from GitHub carry the default branch's HEAD commit date). A local repo's
  `.git/config` is untrusted, so git runs with every transport disabled (`protocol.*.allow=never`,
  `GIT_NO_LAZY_FETCH=1`, which stops a partial clone's lazy fetch from running an `ext::` remote),
  no fsmonitor, hooks, pager, signature program or `core.sshCommand`, no system config, in its own
  process group, with 16 MiB of output and 30 s at most. Future commit dates count as now;
- anything else (web pages, notes, config files) by when it was stored, so a re-fetched page is
  dated by its fetch.

Only a migration's copy keeps undated content undated (`Meta.KeepUndated`): copying old content is
not new content. Chunks stored before this release stay undated until their source is stored
again.

**Duplicates within a source.** A chunk whose text (SHA-256) repeats an earlier chunk of the same
source is dropped before embedding, and the remaining chunks keep contiguous indices.

**Unchanged writes.** Each chunk stores `ingest_hash`, over the text and everything stored with it
(type, scope, tags, dates, writer) and the settings that shape it (chunk size and overlap, markdown
sections, the BM25 tokenizer version and constants). Under the source's write lock, a write is
skipped before embedding (outside a migration) only when the source is whole: its first and last
chunks carry the write's hash and it holds exactly the recorded chunk count. So a re-fetched page or
re-walked doc that did not change keeps its date and costs nothing, while a write that failed
part-way (a later batch, or the prune of an older version's surplus) is done again next time, and a
chunker change is applied at each source's next write.

### Provenance

Every write names its **writer**, set by orchd's own code, never by a client:

| Writer | Who | Stored `trust` |
|---|---|---|
| `docs` | the GitHub docs sync | `repo` |
| `sources` | the sources refresh | `repo` (repo and owner sources), `web` (web sources) |
| `session` | orchd's session storage (hand-off, summaries) | `session` |
| `file` | the edit hand-off | `file` |
| `client` | the Ingest RPC, so the CLI, hooks and MCP `rag_ingest_text` | `client` |

`writer`, `trust` and `content_hash` (the first 16 bytes of SHA-256 of the text as given, kept
through a migration) are stored on every chunk. A client cannot claim a tier: whatever type or URI
it declares, an Ingest RPC write is `client`, and the Ingest RPC refuses (`FailedPrecondition`) to
replace a source the docs or sources sync stored, so a client cannot hide a synced doc behind its
own text. Hits return `trust`, and `GetDocument` returns
`writer`, `content_hash` and `trust`. Chunks stored before this have none, and readers treat them
as unverified. A `client`, `file` or `session` chunk whose type or URI says it is a web page or tool
output (an `http(s)`/`ftp` scheme, `//` or `www.`, after NFKC folding; type `web` or `tool_output`
and the like) is read as `web` or `tool`: the type and URI can lower a tier, never raise it, so
`untrusted: exclude` still drops a page a client stored. Only `repo` is shown without a "data" caution, and even it is fenced in full-text
output (see [hooks.md](hooks.md#prompt-context)). Session content is fenced because transcripts
carry the tool output and web pages the agent read.

Source URIs in use: `session://<id>/record|transcript`, `repo://<owner>/<repo>/<path>`,
`file://<path>`, `claude://<path under ~/.claude>`, web URLs, and anything given to `orch ingest`.

## BM25 keyword vectors

`TokenizerVersion = 2`; changing tokenization requires bumping it and re-indexing
(`orch rag reindex-keywords`, below).

- **Tokens:** NFC-normalize; match `[\pL\pM\pN_]+(?:[./:\-][\pL\pM\pN_]+)*`; split each match
  into script runs. Han, kana, Hangul and Thai runs become overlapping 2-rune bigrams. Other runs
  split into parts at `. / : - _` and camelCase boundaries (`HTTPServer` → `http`, `server`),
  lowercased; emit the whole identifier (≤ 64 bytes) when it has several parts, and each part of at
  least 2 runes that is not a stopword. Caps: 8192 tokens, 64 parts, 64-byte terms.
- **Version 2 additions:** a capital after a digit starts a part (`OAuth2Token` → `auth2`,
  `token`); each `. / : -` separated piece that is itself compound is emitted whole
  (`rag.search.keyword_weight` → `keyword_weight`; `parse_HTTPRequest` → `httprequest`); and a
  path's last element with a dot (`internal/rag/search.go` → `search.go`). On the golden set this
  measured neutral (MRR 0.666 before and 0.662 after the re-index, 95% CI −0.011 to +0.002); it is kept because the change is within
  noise and the re-index takes 27 seconds.
- **Term id:** FNV-1a 32-bit of the term.
- **Document weight:** per distinct term `tf·(k1+1) / (tf + k1·(1 − b + b·len/avgLen))`, with
  k1 1.2, b 0.75, avgLen 55 (measured: a 110-token chunk is about 50 words), `len ≥ avgLen/4`. At
  most 4096 terms, keeping the heaviest.
- **Query weight:** 1 per distinct term; Qdrant applies IDF.

## Search

1. TopK 1–100 (0 → 5). Embed the query once. Filters: `scope` and `source_type` (exact), `tags`
   (any), and a time range: `ended_at ≥ since` and `started_at ≤ until` (overlap; undated content
   never matches a range).
2. **Hybrid** (default, on a hybrid collection): in parallel, a dense search for
   `n = min(max(4·TopK, 20), 400)` (`POST /points/search` with
   `{"vector": {"name": "dense", "vector": [...]}, "limit", "with_payload": true, "filter"}`; a bare
   vector in the dense layout) and a keyword query (`POST /points/query`, `using: bm25`, limit
   `min(2n, 400)`, returning each hit's dense vector). Keyword hits are capped at 2 per source. A
   query with no BM25 terms (only stopwords or single characters) skips keyword search silently.
3. **Fuse** by reciprocal rank: each hit's score is `Σ weight/(k + rank)` with k = 60, weight 1 for
   dense and 0.5 for keyword (`rag.search.keyword_weight`).
4. **Weight** (ranking `weighted`, the default; `plain` skips it): multiply each candidate's fused
   score by its source type's weight and by recency, `floor + (1 − floor) · 2^(−age / half-life)`.
   Age is from `ended_at`, else `started_at`, else `ingested_at`; undated content counts as one
   half-life old; future dates count as now; a negative score is taken as 0 first. Defaults:
   half-life 180 days, floor 0.85, weights `session_transcript` 0.85, `web` 0.9, all else 1
   (`rag.search.recency_half_life_days`,
   `recency_floor`, `source_weights`). This runs client-side, after fusion, because fusion is
   client-side; Qdrant's formula queries would need fusion moved into the server.
5. **Diversify:** keep at most `per_source` hits from one source (request field, else
   `rag.search.per_source`, default 2; negative lifts it; never applied to a search filtered to one
   source). Prompt context asks for 1. Sort by weighted score, then cosine; cut to TopK. Dense-only
   searches fetch the same candidate depth as hybrid ones when they weight or cap.
6. Every hit reports its **cosine** (computed from the returned vector for keyword-only hits), its
   BM25 score, and `Exact`: whether it contains, as a whole token, a code identifier from the
   query. Identifiers come from the first 8 KiB of the query, at most 16, each 5–128 characters:
   camelCase, snake_case, a dotted name of 2+ parts (each 2+ characters) not ending in a domain or
   file suffix (`com`, `io`, `md`, `json`, `yaml`, …), or a path with three parts or one part of 8+
   characters. Versions, numbers and a stop list (`e.g`, `i.e`, `a.k.a`, `go.mod`, `makefile`, …)
   are excluded.
7. **Fallback:** if the collection has no keyword index or keyword search fails, configured hybrid
   answers by meaning and returns a note; an explicitly requested hybrid fails instead. Fallback
   logs are limited to one a minute.

Scores keep cosine meaning in both modes, so relevance floors (prompt context) work unchanged:
weighting and the cap change the order, never a hit's score. Hits also carry their `heading`.

**Tuning** used the golden set's `tune` split only (70 cases), on a read-only copy of the store,
and the defaults were judged on the `holdout` split (92 cases). Decision cases accept any source
of their session, so favouring records over transcripts cannot score by construction. MRR, tune /
holdout, against fusion alone (0.702 / 0.571):

| half-life | floor | transcript | web | tune | holdout |
|---|---|---|---|---|---|
| off | | 1 | 1 | 0.702 | 0.571 |
| 180 | 0.7 | 1 | 1 | 0.730 | 0.588 |
| off | | 0.7 | 1 | 0.708 | 0.592 |
| 180 | 0.7 | 0.85 | 0.9 | 0.742 | 0.617 |
| 90 / 365 | 0.7 | 0.85 | 0.9 | 0.742 | 0.618 / 0.617 |
| 180 | 0.5 | 0.85 | 0.9 | 0.740 | 0.603 |
| 180 | 0.85 | 0.85 | 0.9 | 0.723 | 0.617 |

The best tune score was floor 0.7. The default is the softer floor 0.85, a choice made after
looking at holdout sections, so the holdout split no longer judges that one choice blind: within 0.02 on tune, equal
on holdout overall, and better on the holdout sections weighting hurts (decisions 0.661 → 0.702,
hand-written 0.721 → 0.720). On holdout the gain comes from repo-doc questions (identifiers 0.492
→ 0.573, headings 0.453 → 0.542 against plain fusion); the hand-written questions are slightly
lower (13 cases, not significant). A per-source cap of 1 or 2 added +0.005 on tune and +0.009 on
holdout.

**Limits of the set.** Heading and identifier cases ask for the heading or identifier verbatim, so
they measure lexical self-retrieval more than understanding; decision cases are cut from the
record's own text. The hand-written sections are small (22 hard, 20 legacy), too small to show a
change of a few points. Treat the numbers as directional, and add hand-written cases (`orch rag eval
add --section hard`) before trusting a small difference.

## Timelines

Transcripts are stored with each turn prefixed `[YYYY-MM-DDTHH:MMZ]` (UTC, one word so chunking
never splits it). For a transcript ingest (`Timeline`), each chunk's span is the earliest and
latest marker in it; a chunk without one inherits the last time seen; a chunk that opens mid-turn
starts at that turn's time. Spans are clamped to the session's start and end. Marker-shaped text
inside messages is rewritten `[..]` → `(..)` before storing, so quoted content cannot date itself.
Date bounds accept `2026-10-21` (UTC day), RFC 3339, `today`, `yesterday`, or ages up to 36500 days
(`7d`, `12h`).

## Redaction

`redact.Redact` replaces matches with `[REDACTED:<label>]`: PEM private keys, AWS access keys,
GitHub tokens (`gh?_`, `github_pat_`), Slack tokens, Anthropic and OpenAI keys, and JWTs. Hooks and
schedulers redact before storing; a file with more than 25 matches is skipped as a likely keystore.
Text sent with `orch ingest` or `rag_ingest_text` is stored as given.

**Queued tasks cannot write.** `rag_ingest_text`, `rag_delete_source`, `orch ingest text` and
`orch rag forget` refuse inside a queued task, and orchd refuses `Ingest`, `DeleteSource` and
`SummarizeSession` from one (see [daemon.md](daemon.md#queued-callers)). A session an untrusted
repository steers could otherwise plant memories later sessions trust, or erase them. The
installed specialist agents (`dev`, `ops`, `research`, `reviewer`) never ingest, queued or not:
they end their report with a `## For RAG` section of proposed entries (`source_uri`,
`source_type`, text), and the parent session reviews them as untrusted and ingests what the user
approves (`orch-dispatch` Phase 4). Its own transcript is still stored through
the hand-off unit, as a `session_record` like any session.

## Sources and docs

**Sources registry** (`sources` table, `orch source|repo`), refreshed when
`now − last_fetched ≥ refresh_secs`:

- `web`: GET (≤ 5 MiB), strip HTML, redact, store under the URL. Errors keep the old chunks.
- `repo` (absolute path) and `owner` (`<known_root>/<owner>/*`): every `*.md` found by `mdwalk`
  (skipping `.git` and `ingest.exclude_dirs`), stored as `checkout://<resolved absolute path>`,
  type `repo_doc`, tagged `repo:<name>` and `owner:<owner>`, dated by its last commit, written as
  `sources`. A local checkout is not the GitHub repo (it may be behind, ahead or on a branch), so it
  never shares the docs sync's `repo://<owner>/<repo>` names, and two checkouts of the same name
  never share one. Earlier versions stored these under `repo://<owner|local>/<repo>/`; `orch rag
  dedupe` lists those leftovers.
- **Other checkouts are not walked:** an owner scan skips `<repo>.worktrees` containers (gittree's
  layout) and linked worktrees, and every walk skips a directory below the repo that has a `.git`
  entry of its own (a nested repository or worktree) or is named `*.worktrees`. Each checkout is
  judged on its own branch.
- **Default branch only:** a working tree is indexed only on its default branch, judged from the
  `.git` at the repo directory itself (never an enclosing repository's), read from files without
  running git. The default is, in order: `refs/remotes/origin/HEAD`; the only local branch;
  `init.defaultBranch` from the repo's config when such a branch exists; `main` or `master` when one
  exists. On another branch, a detached HEAD, an unknown default, an unreadable HEAD (a reftable
  repository) or a `.git` that does not follow git's layout, the repo is left as last indexed,
  neither stored nor pruned, and the source's status says so (`skipped: on branch X, not the
  default branch Y`; an owner source lists them after `ok; skipped N:`). A plain directory is
  indexed as it is. A `.git` file is believed only when it points at `<common>/worktrees/<name>`
  whose `gitdir` file points back at it, or at a git directory whose `core.worktree` names this
  directory.
- **Removed files:** after a complete walk, the sources this refresh wrote under the repo's
  `checkout://` prefix (listed once per refresh by `writer = sources`, which has a payload index)
  that the walk did not find are deleted. An agent's note under the prefix is never a candidate. A
  walk that found no markdown or could not read a directory deletes nothing, and a failed listing
  disables the step.
- **Prune cap** (`internal/prunecap`, table `prune_state`): deletions under one prefix are capped at
  half (at least 2) of what was stored when a 7-day window began, counted across scans, so the cap
  cannot be walked down a half at a time. Past it nothing is deleted and the status turns
  `attention: ...; to remove them run: orch source refresh <id> --confirm-prune` (counted in `orch
  status`, red in the app). The confirmation (RPC `RefreshSource.confirm_prune`, operator-only) lets
  the next prune through and starts a new window.
- **Validation** (`sources.Validate`, used by `orch source add` and the `SaveSource` RPC):
  - a `web` URI is an `http`/`https` URL with a host and no credentials;
  - a `repo` URI is an absolute path, cleaned;
  - an `owner` is one directory name (`[A-Za-z0-9][A-Za-z0-9._-]*`), so it cannot name a path
    outside the known roots; `local` is reserved, because repo sources are stored under
    `repo://local/`;
  - the refresh interval is 1 minute to 365 days;
  - scope and tags are at most 64 characters with no control characters, tags have no commas
    (they are stored comma-joined), tags cannot start with `owner:` or `repo:` (refresh adds those),
    and there are at most 32 tags.
- **Removal** unregisters the source and keeps what it indexed. `RemoveSource` with `purge` (and
  `confirm`) also deletes that content, before unregistering so a failed purge can be retried:
  - `web`: the URL's content.
  - `repo` and `owner`: `checkout://` docs written by the sources refresh that carry the source's
    tag, under the repo's resolved path (a repo source) or in a `/<owner>/<repo>/` segment pair
    matching the doc's repo tag (an owner source); plus `repo_doc` content earlier versions stored
    under the source's `repo://` prefix with its tag. Content of an owner the docs registry tracks,
    or with a row in `doc_hashes`, is kept.
  - It is refused for a repo source that shares its directory name with another (they share
    content), and for a stored row that fails validation (from before it existed).
  - A refresh running during the purge can store a source's docs again; purge again to remove them.

**Docs registry** (`orch docs add <owner>`, every 6 h): list the owner's repos with
`gh api graphql` (skipping archived and forks by default), skip repos whose default-branch HEAD is
unchanged, `gh repo clone --depth 1` into `<data>/repos`, and store each markdown file whose
SHA-256 changed (hash saved only after a successful store), dated by the clone's commit. A file is
skipped as unchanged only while its stored copy is still this sync's (writer `docs`, same content
hash), and an unchanged repo is synced again when another writer has replaced one of its docs, so
a client's overwrite is restored. Docs with a hash under the repo's `repo://` prefix that the clone
no longer has are deleted, with their hashes (not when the clone has no markdown or a directory
could not be read), under the same prune cap: a refused prune leaves the repo `error` with the
reason and `orch docs sync <owner> --confirm-prune` (RPC `SyncDocs.confirm_prune`), and keeps its
previous HEAD so every sync retries it. Commit dates are read only when a doc changed. Clones use
whatever protocol the user's gh is configured for (SSH or HTTPS).

- **One sync at a time:** every docs sync runs in orchd: its scheduled scan, and `orch docs sync`
  and `refresh` through the `SyncDocs` RPC. While one runs, another request is refused, not
  queued.
- **Failures are recorded, not skipped silently:** a repo whose clone fails gets status `error`
  and an `error` reason, and the failure is logged under `docs-scheduler`. Failures are classified
  so the reason says what to fix:
  - `network`: GitHub could not be reached;
  - `authentication`: the SSH agent or gh credentials were refused;
  - `access`: the repo was not found or not readable;
  - `stalled`: the clone wrote nothing for 45 s.

  A successful sync clears a repo's error.
- **Stalls are cut off:** clones run with `--progress` under a watchdog. One that writes nothing
  for 45 s (typically waiting on SSH agent approval, such as an unanswered 1Password prompt, or a
  connection that stopped responding) is stopped with its whole process group, so no `ssh` is
  left behind. A clone also has a 3-minute limit overall.
- **Fail fast:** after 3 network failures or stalls in a row, the owner's sync stops; the repos not
  tried keep their previous state, and the owner records `error` with the reason. Otherwise an
  owner with failed repos is `partial`. A sync stopped by orchd stopping records the reason but
  leaves the owner due, so the rest syncs at the next scan.
- **Safe error text:** recorded and logged reasons have credentials in URLs and token shapes
  redacted, control and format characters removed, and are valid UTF-8, capped on a character
  boundary.
- **Needs attention:** `orch status` (and the app's System report) warns on the docs line with the
  failing count and the reason; the app's Docs tab shows a banner, the failing repos with their
  reasons, and Sync now.

`ListDocs` serves the docs catalog: each owner the registry syncs, its repos with the indexed
default-branch commit, and every doc that both the store holds and the registry recorded, with its
chunk count and GitHub URL, `https://github.com/<owner>/<repo>/blob/<commit>/<path>`. Requiring
both leaves out a doc whose ingest failed or whose content was deleted, and anything another
client stored under a `repo://` URI (only orchd's sync writes the registry), including a local
owner folder that shares a synced owner's name. The reply stops at 10,000 files or 3 MiB.
The URL is pinned to the indexed commit, so the page shows the version that was indexed; with no
commit recorded it uses `HEAD`. `GetDocument` rebuilds any indexed source from its chunks (the
same rebuild the hybrid migration uses). Where a piece starts with the previous piece's last whole
lines (a block split at line boundaries), the rebuild drops them and continues the block on the
next line; a shared line must have content, so two blocks that only both meet at a fence line are
not merged. It links the original only for a `web` source or a synced
GitHub doc, since any client can store text under any URI. A rebuild over 3 MiB is reported as
oversize with no text, so replies stay under clients' 4 MiB limit; `ListDocs` stops at 10,000
files and says it was truncated.

## Migration to the hybrid layout

Qdrant 1.17 cannot add a sparse vector to an existing collection, so `orch rag migrate-hybrid`
copies into a new one:

1. The target defaults to `<collection>-hybrid`. Refuse unless the active collection is dense and
   the target is new or holds an unfinished migration (a hybrid target with points but no marker
   is a finished one). Without `--apply`, only rebuild and re-chunk in memory and report the counts.
2. Write a marker `<data>/migrations/hybrid-migration-<target>.inprogress`; create the target;
   install it as a **shadow** (under an exclusive write gate) so new writes reach both.
3. Copy each source under its per-source lock (64 stripes by FNV-32a): read its chunks, rebuild the
   text (sort by index, strip context prefixes, remove repeated headings and word overlaps of 8+),
   re-chunk and write. Skip sources a live write already put in the target (`touched`); a failed
   shadow write marks the source `dirty`.
4. Remove target sources deleted since; refuse to switch if any source exceeded 16 MiB.
5. Re-copy dirty sources until none remain under the exclusive gate (at most 5 rounds).
6. Remove the marker, record `collection` and `backup_collection` in config in one write, then
   switch. Rollback swaps them back; `--drop-backup` deletes the backup recorded in config, never
   one named by the caller, and refuses the collection in use or with an unfinished marker.

Lock order: write gate (shared for writes) → source stripe → state mutex.

## Keyword re-index

`orch rag reindex-keywords` (RPC `ReindexKeywords`, operator-only, under the migration lock)
brings keyword vectors up to the current `TokenizerVersion` in place, without re-embedding:

1. Refuse unless the collection is hybrid and no migration shadow is installed. Count all chunks
   and the stale ones (`must_not bm25_v = TokenizerVersion`, which also matches chunks with no
   stamp). Without `--apply`, stop there.
2. Scroll stale chunks 256 at a time (payload `source_uri` only; the offset is a point id, so the
   shrinking filter does not skip any). Group the page by source; for each, under that source's
   write lock, re-read the chunks' text and stamp (`POST /points`), skip any a live write already
   stamped, recompute BM25, `PUT /points/vectors` (the `bm25` vector only) and
   `POST /points/payload` with `bm25_v`.
3. Every write stamps `bm25_v`, so a crashed or cancelled run resumes with what is left. Memory
   stays at one page of text; on the 45,563-chunk store it took 27 s with orchd at about 250 MB.

Until it runs, queries tokenized with v2 still match v1 vectors on every term the two share.

## Duplicate clean-up

`orch rag dedupe` (dry-run unless `--apply`; deletes through `DeleteSource` with `confirm`) lists a
`file://` or `claude://` source only when all of these hold:

- its file exists, and its real path (after symlinks) sits in a registered source: a repo or
  owner source, or a checkout of a repo the docs sync tracks (the checkout's own `origin` is
  believed only for a tracked repo);
- that source's own URI for the file (`sources.Canonical`: `checkout://` or `repo://`) is stored,
  written by the docs or sources sync, not by a client;
- the two stored copies are identical (by content hash, else rebuilt text), or the file as it is
  now has the synced copy's content hash (the stored copy is an older version of it).

Text alone never makes a duplicate, so a boilerplate file in another project or `~/.claude`'s own
config is never listed, and a copy that differs from the synced one (an unmerged edit) is kept.
The synced copies need their content hash, which docs stored before this release lack until
`orch docs refresh` re-stores them, so run that first.

## Retrieval evaluation

`internal/rageval` with `orch rag eval`:

- **Set:** `<data>/eval/golden.jsonl`, one case per line,
  `{query, expect: [uri|prefix*], span?, scope?, source_type?, tags?, section?, note?, split?}` (unknown
  fields rejected; `#` lines skipped; at most 1,000 cases, 4 MiB). While it does not exist the
  legacy `<config>/rag-eval.yaml` is used, and the first `orch rag eval seed` imports it as the
  `legacy` section. `--set` takes either format.
- **Seed** (`orch rag eval seed`, no model calls): up to 3 decisions per session record (bullets
  under `## Decisions`, cut at the first clause and to 10 words, asked as "what did we decide about
  ..."), labelled with every source of that session (`session://<id>/*`, so the transcript the
  record was written from answers too); and per `repo://` doc one heading (level 2–3, 3–12 words, not
  generic) and one backticked identifier that no other doc or record names, labelled with that doc.
  Sources are sampled in FNV-64 order of their URI. A reseed replaces only `seed-*` sections.
- **Scoring** (report version 2): a case's rank is the chunk position of the first hit from an
  expected source whose text contains the span, if any (chunk position, not distinct source,
  since the top three chunks are what is injected); the source rank ignores the span. Metrics:
  recall@1/3/5/10, MRR, nDCG@10 (one relevant item: expected URIs are alternatives), source hit@k,
  injected tokens (the pointer, or `--inject-format full`, rendering of the top three distinct
  sources at 2,000 characters, at 4 characters a token), and latency p50/p95, overall and per
  section.
- **Splits:** every case is in `tune` or `holdout` (its `split`, else a hash of its query), so
  settings are chosen on one half and judged on the other; `--split` scores one half, and reports
  give each section's halves.
- **Compare:** `--compare <run>` prints deltas, cases that moved, and a paired bootstrap over the
  shared queries for MRR, recall@3 and nDCG@10 (10,000 resamples, fixed seed): the mean delta, its
  95% interval and the share of resamples where it was not positive. Cases pair by query and
filters, once each; a comparison over fewer than 10 cases is never called significant. Runs of another version are
  not compared. `--mode`, `--ranking` and `--per-source` score other settings on the same daemon.
