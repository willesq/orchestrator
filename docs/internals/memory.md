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

plus keyword payload indexes on `source_uri`, `writer`, `space`, `tags` and `source_type`. A collection made
before the last two is asked for them at the first search that filters on them, once per process:
this changes no stored data, and searches carry on, slower, until Qdrant has built them. A 409 "already exists" (a concurrent first write) is
treated as success. A legacy collection with one unnamed dense vector is the `dense` layout.

**A collection per space (SP-024, D-10).** The configured collection (`rag.qdrant.collection`)
is the *base*. Content placed in a space lives in its own collection, `<base>__<space>`
(`rag.SpaceCollection`), Quarantine included as `<base>__quarantine`; unplaced content and
`shared` (no policy applied) stay in the base itself, so with no policy orch uses one collection
exactly as before (INV-10). Space names (`[a-z][a-z0-9_-]{0,31}`) never start with `_`, and a
migration target containing `__` is refused; a base that itself contains `__` (set by hand) can
still read as another base's space, so a backup drop refuses a name under which the active
collection would read as a space. The space collections are found by listing Qdrant's collections
(`GET /collections`) and keeping the base's; the list is cached for a minute per base, and every
listing also includes the collections this orchd created (kept until the base is dropped), so a
listing answered while a write created one never hides it. Physical separation means a
read that forgets a filter still cannot reach another space, and each space's BM25 IDF comes from
its own chunks.

- **Writes** go to the collection of the space the write was placed in, and nowhere else: the
  collection is created there, the points are written there, and the older version's surplus
  chunks are pruned there. A source lives in one space, so before writing, a write placed in a
  space deletes the same source URI from every other collection of the base (one count by
  `source_uri` each, and a delete where it is found); if that fails, nothing is written and the
  next write tries again. A Shared or unplaced write (no policy, or a policy removed) does the
  same for space collections, but a failed listing does not fail it, and a store that never had
  a policy has none to look in, so it makes the requests it always made plus the cached listing.
  A client's write never moves a source out of another space: the Ingest RPC refuses to replace
  a source another space holds before it gets here ([Placement of writes](#placement-of-writes)),
  and should that check fail to read the store, the write itself is refused
  (`ErrStoredInAnotherSpace`) rather than delete the other space's copy, reading the collections
  afresh: a copy in another space's collection, or a base chunk whose payload names another
  space. A client may still replace base content no space claims. So moves are orchd's own
  writers' (a re-placed doc, a repo whose visibility changed).
- **Reads of one source** (`Copy`, `Document`, `HasSource`) look in the base and then each space
  collection (should a source be in several, `Copy` reports the copy that refuses most: a sync
  writer's, then one placed in a space, then the base's, and logs it), and `ListSources`, `SourcesWrittenBy` and `DeleteSource` cover every collection;
  a forget lists the collections afresh. A failed listing fails these (and a write to a space),
  rather than act on part of the store; a collection Qdrant reports missing holds nothing.
- **Searches** read the collections of `SearchOpts.Spaces` ([Search](#search)).

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
own text. That refusal matches the exact URI only: a client can still store the same text under a
different spelling of a URI (another case, a trailing slash), which is a separate `client` source
and labelled as one, so the refusal is advisory, not what makes a tier. Hits return `trust`, and `GetDocument` returns
`writer`, `content_hash` and `trust`. Chunks stored before this have none, and readers treat them
as unverified. A `client`, `file` or `session` chunk whose type or URI says it is a web page or tool
output (an `http(s)`/`ftp` scheme, `//` or `www.`, after NFKC folding; type `web` or `tool_output`
and the like) is read as `web` or `tool`: the type and URI can lower a tier, never raise it, so
`untrusted: exclude` still drops a page a client stored. Only `repo` is shown without a "data" caution, and even it is fenced in full-text
output (see [hooks.md](hooks.md#prompt-context)). Session content is fenced because transcripts
carry the tool output and web pages the agent read. Before the scheme is read, the URI loses its
whitespace and format characters, and `ws`, `wss`, `data`, `javascript`, `smb` and `gopher` schemes
and `\\host` UNC paths also read as `web`.

What a tier does and does not promise:

- **`repo` means where, not who.** It says the text came from a path you registered (or a GitHub
  owner you added): from a checkout that appeared to be on its default branch (see the default-branch
  gate, which is protection against indexing a feature or PR branch by accident, not proof of where
  the files came from), or from a directory that is not a git checkout at all, which is indexed
  without any gate. It does not say you wrote it. A dependency's README, a
  vendored file or a teammate's commit in a registered repo is `repo` too. The tier is for telling
  your registered repos apart from pages, tool output and clients' notes, not for vouching for
  every line in them.
- **`untrusted: exclude` only drops what is labelled.** It removes `web` and `tool` chunks, and a
  client's write whose type or URI says web; a client that stores a page as an ordinary note is
  `client`, shown with a "data" caution, not excluded.
- **A prune confirmation is the operator's.** `confirm_prune` is accepted only from a caller that is
  not a queued task (by process lineage, like the other operator-only RPCs), so an agent in a queued
  task cannot approve deleting a repo's docs.

### Placement of writes

Every write also records its **origin** and is **placed** by orchd (SP-023, INV-02, INV-03). The
writer reports facts as a `rag.Origin`, and `rag.Service.Place`, set by orchd before any writer
starts (`internal/daemon/placing.go`), runs the classifier ([Placement](#placement)) under the
applied policy. Each chunk then stores `origin` (the facts below, only those reported), `space` (the
answer) and `labels_v` (the applied policy revision it was placed under; 0 with none). Whatever a
caller put in `Meta.Space` is discarded before placing, and with a placer set a write that reports
no origin, or that the placer leaves without a space, stores nothing.

| Writer | Origin it reports | Placed by |
|---|---|---|
| docs sync | `github`: owner, repo, visibility (`private`/`public`; unknown when GitHub listed no owner node ID) and the owner's node ID, from the `gh api graphql` repo listing (`isPrivate`, `owner{id}`) | the owner's binding; public only when GitHub said the repo is public |
| sources refresh | `fs` with the file's path (repo and owner sources); `web` with the URL the page was fetched from, after redirects | the deepest root; a checkout's remote can only keep it out, never place it (D-65), and nothing under a public root is public; the bound web prefix of the URL the content came from |
| session hand-off, summaries | `session` with its first directory, from its record, and the session ID when the transcript is that session's, where its harness keeps it | the session's pin only; quarantine with no pin or no tied session |
| edit hand-off | `fs` with the resolved file path | the deepest root and the checkout's remote; never the remote alone; never public (goes to `public_root_sessions` instead) |
| Ingest RPC | `client` with the caller kind (`session`, `task`, `operator`, `none`), the space the caller's pin writes to, and the URL when the URI is one | the caller's pinned space; quarantine for any other caller |
| migration copy (`rag`) | carried over from the stored chunk | not placed again: `origin`, `space` and `labels_v` are copied as stored |

- **Clients cannot choose a space.** No RPC field reaches `Origin` or `Space`: tags, scope and type
  that name a space change nothing. A client note goes to the space of the caller's pin as orchd
  resolved it from the caller's process (SP-011). A queued task, the operator at a terminal and an
  unidentified caller have no space here, so their notes are quarantined until the operator retags
  them, and so is a note from a session pinned to no space, to a public one or to one the policy
  no longer has. A page a client stores is placed by its writer, never by the bound prefix its URL
  falls under.
- **Sessions are placed only by their pin.** A hand-off entry, a session record and a
  `SummarizeSession` request are files or fields any process of the user can write, so neither the
  session ID nor the directory they name is believed on its own: a session's records go to the
  space of the pin orchd made at its start, and only when the transcript is that session's own: a
  regular file (not a link) at `<claude>/projects/<project>/<id>.jsonl` or
  `<codex>/sessions/…/rollout-<time>-<id>.jsonl`, where `<claude>` and `<codex>` are `~/.claude`
  and `~/.codex`, or `CLAUDE_CONFIG_DIR` and `CODEX_HOME` when orchd's environment sets them. A
  `SummarizeSession` request from a session about another session is not tied either. Anything
  else, and a session orchd never pinned, goes to quarantine. So a session can neither pull another
  space's transcript into its own space nor push a file it named after another session into that
  session's space, short of writing into the harness's own transcript folders, which the OS
  sandbox can deny (SP-046). Recording the transcript path at the pin would close that too.
- **A local file is not placed by its remote.** A remote that would place a file in a stricter
  space than its root (`remote_stricter`) quarantines it for every writer (SP-030, D-65): a session
  can write a `.git` naming another job's org beside files it registers as a source. Sessions are
  still pinned by a stricter remote (SP-022).
- **A local file is not placed by its remote alone.** An agent can write a `.git` naming any org
  beside a file it edits, or register such a folder as a repo source (any session may), so a file
  under no root that only its checkout's remote would place goes to quarantine. An edited file is
  also quarantined when its remote is stricter than the root it is under, until hand-offs carry the
  editing session's space (SP-029), and since SP-030 so is a registered source there (above).
  Edit hand-offs use cached repo lookups only, so files an agent picks spend none of GitHub's
  budget.
- **The public-space write gate (SP-030, SP-076, D-64, D-65).** Only the docs sync and the sources
  refresh may place content in a public space, and only by what verifies it public: a repo GitHub
  reports public (docs sync), a bound web prefix, or a folder the operator listed in the applied
  policy's `public_paths` ([config.md](config.md#spaces-policy)). Other local content under a
  public root such as `~/code` is quarantined, whatever its remote ([Placement](#placement)),
  because any job's session can write there, create a `.git` there or register a folder there as
  a source. A session can still write into a listed folder and have the sources refresh index it
  into public (RC-404, accepted risk). Session
  records, summaries, edit hand-offs and client notes never land in public: sessions and client
  notes from a session pinned to a public space go to quarantine, and an edit hand-off that would
  be public (a file in a verified public repo under a public root) goes to `public_root_sessions`,
  where sessions under a public root write (D-22). Only an operator retag (SP-071) moves other
  content into public.
- **Content that is no longer public leaves first.** Visibility is re-checked on every docs sync
  from the repo listing. Before any repo is cloned or written, the sync asks the placer about each
  stored doc of every listed repo that GitHub does not report public (private, or listed without
  its owner's node ID, which counts as private): a doc stored in a public space that would not be
  public if written now is deleted, its hash and the repo's HEAD are forgotten, and the repo is
  synced first, so its docs are written again where they belong now in the same sync. A clone
  that fails leaves them out of public and the repo is tried again next sync. A listing GitHub refuses
  (the owner is gone, or this login lost access) takes every recorded repo's public docs out; a
  listing that fails for another reason (network, credentials) keeps them until one succeeds. A repo or owner
  source does the same for its stored docs before it walks anything, so a checkout left as last
  indexed (off its default branch, or its org binding not checking out) does not keep a public
  copy. A doc whose placement cannot be decided now (a conflict, a repo GitHub says it cannot
  find) is not proven public, so it leaves too. Since no local content is public now, a public
  copy of a checkout leaves whatever GitHub answers. These deletions are not capped by the prune
  guard: a cap would keep content that is not public readable by every session. With no policy, or one that does not verify,
  nothing is moved.
  The docs sync does the same for recorded repos missing from a successful listing (deleted,
  transferred out, no longer visible) and for every recorded repo of an owner whose org binding
  no longer checks out. Once an hour a sources scan also checks every doc the sources refresh ever
  stored, registered or not, so a removed source or a deleted folder keeps no public copy.
  `TestRepoTurnedPrivateLeavesPublicInTheSameSync`, `TestCheckoutNoLongerPublicLeavesFirst`,
  `TestUnlistedReposLeavePublic`, `TestScanSweepsOrphansOutOfPublic`.
- **A client cannot replace another space's source.** A write replaces and prunes every chunk under
  its URI, so the Ingest RPC refuses (`FailedPrecondition`, naming no space) a write over a source
  placed in another space than the one the write would go to. Sources stored before placement are
  not checked. Two unplaced callers' notes share `quarantine`, so one can replace the other's.
- **A conflict is not indexed.** An origin two equally strict spaces claim (a checkout under one
  isolated root whose remote another binds) is refused with an error and nothing is stored; the
  docs sync does not record the file's hash, so it is tried again after the policy changes.
- **Fail closed.** With an applied revision that no longer verifies, every write goes to
  `quarantine`. With no policy, everything is `shared` and `labels_v` is the applied revision
  number (0 when nothing was ever applied), so behaviour is as before spaces (INV-10).
- **A placement change is a new write.** The write hash includes the origin, space and
  `labels_v`, so the same text placed elsewhere or under a new revision is stored again rather
  than skipped as unchanged. Writes made without a placer (tools, tests) keep the hash they had.
- **What is not done yet.** Each space's chunks go to its own collection (SP-024, see
  [Storage](#storage-qdrant)), but no read path passes the caller's spaces or filters by
  `labels_v` yet (SP-025): a search without `SearchOpts.Spaces` reads every collection. Chunks stored before SP-023 have
  no placement, and chunks stored before SP-024 sit in the base collection whatever their space;
  a [relabel](#relabel) (SP-027) places and moves them. A docs file whose content and repo HEAD
  did not change is not stored again, so it keeps its earlier placement until relabelled, except
  that a public copy of a repo no longer public leaves (above). When the repo listing fails
  other than by GitHub refusing the owner, every repo keeps its docs as last indexed, public ones
  included, until a listing succeeds.

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

0. **Collections.** `SearchOpts.Spaces` names the spaces whose collections are read (SP-024). Each
   named space is read only from `<base>__<space>`, and, as the second layer, every request to
   it also filters on payload `space` = that space, so a chunk misfiled in another space's
   collection is still not returned. `shared` reads the base with no `space` filter: it is the
   store as it was before spaces, whose older chunks carry no space. A name that is not a space
   name is skipped; an empty, non-nil list reads nothing and embeds nothing. `nil` reads every
   collection of the base (the base and each space's), as search did before SP-024; deciding
   each caller's readable spaces is SP-025. If the listing fails, a `nil` search reads the base
   alone. Collections not created yet are left out when there are several; hybrid search runs
   only when every remaining one is hybrid.
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
   dense and 0.5 for keyword (`rag.search.keyword_weight`). Across several collections each side
   runs once per collection, in parallel, and each side's lists are first merged into one by
   score (cut to `n` and `2n`): cosine means the same in every collection, and each keyword score
   is ranked by its own space's statistics, as the plan intends. Hits are told apart by
   collection and point ID, since two spaces may hold the same URI's points for a moment. With
   one collection nothing is merged, so the search is exactly the one it was before spaces.
   Across several collections the keyword requests return no vectors (most of their cost,
   SP-002); after weighting, the dense vectors of keyword-only hits are fetched, one request per
   collection, only for those whose cosine matters: hits whose weighted score ties another's
   (the cosine orders ties) and the hits returned (the cosine is their score). Every other
   candidate's place is fixed by its score alone, so the result equals a search with every
   vector read (`TestSearchAcrossCollectionsMatchesOneCollection`). If that fetch fails, the
   search answers by meaning, as when keyword search fails (step 7).
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

## Transcripts and harness text

`session.ParseTranscript` keeps what was said. Claude Code also logs what it sent the model, as
`api-request-blob` lines holding the whole context with the role `user` (instruction files, skill and
tool lists, MCP instructions, git status, earlier hook output); these were once stored as turns, more
than half of a transcript's text, repeating across sessions and matching each other. Now those lines
are skipped, and `<system-reminder>`, `<task-notification>`, `<agent-message>` and
`<local-command-*>` blocks and the background-task notice are cut from a user's turn (an assistant may
be discussing them). The session record, its summary input
and the stored transcript all come from this parse.

`orch rag backfill-sessions --all` (RPC `BackfillSessions`, operator-only, so orchd itself refuses a
queued task) hands every stored transcript whose file is still on disk to orchd to store again (plans
unless `--apply`). Transcripts only: records and summaries stay unless `--summaries` asks for them to
be written again from the clean transcript, at one model call per session. A transcript forgotten on
purpose is not brought back (plain backfill still restores one with a record left), a session already
waiting on orchd is left to it, and a record naming a file other than `<id>.jsonl` counts as no
transcript. Session records and hand-offs are files, so a process running as the user can forge them:
the operator-only check covers the RPC, not those files. Repeating it is safe, since a
transcript that comes out the same is skipped as an unchanged write, and a session whose file is gone
keeps what it has, with the prompt gate ([hooks.md](hooks.md#prompt-context)) keeping its harness
text out of prompts. It rewrites chunks in
place, so to be able to go back, copy `<data>/qdrant` first with orchd stopped. On a copy of a real
store (50,047 chunks, 104 sessions, 52 with their file) it took about four minutes: transcript chunks
went from 21,446 to 12,861, those with harness markers from 4,365 to 11, and chunks repeated verbatim
across sources from 7,922 to 622.

## Redaction

`redact.Redact` replaces matches with `[REDACTED:<label>]`: PEM private keys, AWS access keys,
GitHub tokens (`gh?_`, `github_pat_`), Slack tokens, Anthropic and OpenAI keys, and JWTs. Hooks and
schedulers redact before storing; a file with more than 25 matches is skipped as a likely keystore.
Text sent with `orch ingest` or `rag_ingest_text` is redacted by the Ingest RPC, which logs the count
and never the text, and refuses a URI, scope, type or tag holding a secret, since those are shown as they are. Memory stored before this keeps what it was given until it is stored again.
Redaction is pattern-based and so best-effort: a credential in a form no rule knows is stored as
written. It is not the boundary between jobs; the OS sandbox and per-job GitHub accounts are
(D-47).

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
  running git. The default is, in order: `refs/remotes/origin/HEAD`; `init.defaultBranch` from the
  repo's config when such a branch exists locally; `main` or `master` when one exists. A lone local
  branch is not taken as the default, so a single-branch clone of a PR or feature branch is skipped
  (`default branch is unknown`) rather than indexed. When the repo has an origin, the checked-out
  branch named like the default must also be origin's: an upstream, if set, must be
  `origin/<default>`, else it is skipped with the reason. Tracking config alone is not enough, since `git checkout -B main
  FETCH_HEAD` or a reset onto a PR keeps it, so with or without an upstream the branch must be at
  `refs/remotes/origin/<default>` as last fetched. So a PR checked out as a local `main` is not
  indexed. Limits: a default branch behind or ahead of origin (fetched but not pulled, or commits not
  yet pushed) is skipped, left as last indexed, until it is level again; a repo with no origin is
  taken as its own default; and every signal is read from files the checkout's owner can write,
  so the gate stops accidents, not someone forging a `.git`. On another branch, a detached HEAD, an unknown default, an unreadable HEAD (a reftable
  repository) or a `.git` that does not follow git's layout, the repo is left as last indexed,
  neither stored nor pruned, and the source's status says so (`skipped: on branch X, not the
  default branch Y`; an owner source lists them after `ok; skipped N:`). A plain directory is
  indexed as it is. A `.git` file is believed only when it points at `<common>/worktrees/<name>`
  whose `gitdir` file points back at it, or at a git directory whose `core.worktree` names this
  directory, and only when that git directory is outside the checkout (git keeps it there; one
  inside came with the files, from an archive or a copy; inside is decided on resolved paths at a
  separator boundary). A sibling git directory with `core.worktree` pointing back is git's own
  layout and is believed. Relative pointers (`git worktree add
  --relative-paths`) are resolved against the directory holding them. A `.git` file that points anywhere else, as `git init --separate-git-dir`
  leaves it, is skipped with that reason and how to index it (set `core.worktree` there). Directory
  names and reasons in a status have control and format characters replaced and are cut to 120
  characters, since a repo's name is whatever its owner chose.
- **Symlinks:** a `*.md` symlink is read only when it resolves to a regular `*.md` file inside the
  repo root that the walk would read itself: not under `.git`, an excluded directory (compared
  case-insensitively, for case-insensitive volumes) or, for repo sources, another checkout. A
  committed `docs/x.md -> ../.env` or a link to `~/.ssh/config.md` is never stored.
- **Removed files:** after a complete walk, the sources this refresh wrote under the repo's
  `checkout://` prefix (listed once per refresh by `writer = sources`, which has a payload index)
  that the walk did not find are deleted. An agent's note under the prefix is never a candidate. A
  walk that found no markdown or could not read a directory deletes nothing, and a failed listing
  disables the step.
- **Prune cap** (`internal/prunecap`, tables `prune_state` and `prune_confirm`, migration 0015):
  - **Baseline:** per repo prefix, what was stored at its last confirmed prune, or at a clean walk
    (nothing gone) with no unconfirmed prune still counted; what is stored now before the first one,
    and raised to what is stored when the repo grows (growing back to it also clears the count).
    The clean walk right after a prune finds nothing more gone, so it does not reset what was
    removed; only a confirmed prune lowers the baseline. There is no time window.
  - **Cap:** prunes without a confirmation may remove, added up since the baseline, at most half
    (at least 2) of it, and one prune at most half of what is stored now. So repeated half-prunes
    cannot walk a repo down (1000 docs stop at 500 until a confirmation or a clean walk), and a
    legitimate gradual shrink asks for one confirmation now and then.
  - **Refusal:** past the cap nothing is deleted, the refusal is recorded for the prefix, and the
    status turns `attention: ...; the operator can confirm it with orch source refresh <id>
    --confirm-prune` (counted in `orch status`, red in the app). Each complete refresh of a source
    (sync of an owner) clears that key's refusals it did not refuse again, or for docs did not skip
    as unchanged, so a refusal for a repo since moved, removed, skipped or recovered is not granted.
  - **Grant:** the confirmation (RPC `RefreshSource.confirm_prune`, operator-only) grants each
    current refusal of that source, for that refusal's size; it fails with `FailedPrecondition`
    when none is refused. A grant is used by that exact prefix's next prune past the cap if that
    prune is no larger, and expires after an hour (one dated in the future is ignored). Any prune
    past the cap ends it, used or not, so a larger refusal recorded later needs its own
    confirmation; a prune within the cap or a clean walk drops it too.
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

**Opting in and out** (app: Settings → Docs; CLI: `orch docs enable|disable <owner>`,
`orch docs repos <owner> --all|--only a,b|--except a,b`, `orch docs purge <owner> [repo...] --confirm`): an owner
can be off, and can cover every repo, only some, or all but some (`docs_owners.enabled`, `repo_mode`, `repos`,
migration `0021`). Names match case-insensitively. Opting out stops the sync and keeps what is indexed: the repo's
row is marked `excluded`, which clears any failure it showed, so it stays in the catalog and search. Deleting those
docs is a separate, confirmed step (`PurgeDocs`), refused for a repo still synced, since the next sync would only
restore it. Opting a repo back in forgets its HEAD, so the next sync walks it and its status follows that sync;
files whose hash is unchanged are not embedded again. Widening or resuming makes the owner due. Changes are refused
while a sync runs, because the sync would overwrite them from what it read at its start.

**Docs registry** (`orch docs add <owner>`, every 6 h): list the owner's repos with
`gh api graphql` (skipping archived and forks by default), skip repos whose default-branch HEAD is
unchanged, `gh repo clone --depth 1` into `<data>/repos`, and store each markdown file whose
SHA-256 changed (hash saved only after a successful store), dated by the clone's commit. A file is
skipped as unchanged only while its stored copy is still this sync's (writer `docs`, same content
hash), and an unchanged repo is synced again when another writer has replaced one of its docs, so
a client's overwrite is restored. Docs with a hash under the repo's `repo://` prefix that the clone
no longer has are deleted, with their hashes (not when the clone has no markdown or a directory
could not be read), under the same prune cap: a refused prune leaves the repo `error` with the
reason and `orch docs sync <owner> --confirm-prune` (RPC `SyncDocs.confirm_prune`), which grants
every current refusal of that owner. The refused repo records its new HEAD, so it is not cloned again
on every sync while it waits; confirming forgets that HEAD, so the confirmed sync walks it again. Before each repo after the first that it clones, the sync re-checks the owner's org binding and stops if it no longer holds (D-52, [config.md](config.md#spaces-policy)). Commit dates are read only when a doc changed. Clones use
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
copies into a new one. Space collections are named after the base, so they move with it: each
`<base>__<space>` is copied to `<target>__<space>` in the same run (re-chunked and re-embedded like
the base, though space collections are hybrid already), under the one marker, the
shadow writes reach the same space under the target, and the switch moves them together.
Rollback switches them back with the base. Each target collection is checked like the base's
(not dense; points without a marker refuse). The report sums every collection.

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
   switch. Rollback swaps them back; `--drop-backup` deletes the backup recorded in config and every
   `<backup>__<space>` collection with it (space collections first, so a failed drop can be run
   again), never one named by the caller, and refuses the collection in use, any space collection
   of the active base, or one with an unfinished marker. Its point count sums them.

A source written during the copy is tracked per space (`touched` and `dirty` hold the space and
URI), and a write into a space that had no collection when the copy started reaches that space
under both bases, so it is not lost at the switch (`TestMigrateHybridLiveWriteToANewSpace`).

Lock order: write gate (shared for writes) → source stripe → state mutex.

## Keyword re-index

`orch rag reindex-keywords` (RPC `ReindexKeywords`, operator-only, under the migration lock)
brings keyword vectors up to the current `TokenizerVersion` in place, without re-embedding:

1. Refuse unless the base and every space collection are hybrid and no migration shadow is
   installed. Count all chunks and the stale ones, in every collection of the base, summed in the
   report (`must_not bm25_v = TokenizerVersion`, which also matches chunks with no
   stamp). Without `--apply`, stop there.
2. Scroll stale chunks 256 at a time (payload `source_uri` only; the offset is a point id, so the
   shrinking filter does not skip any). Group the page by source; for each, under that source's
   write lock, re-read the chunks' text and stamp (`POST /points`), skip any a live write already
   stamped, recompute BM25, `PUT /points/vectors` (the `bm25` vector only) and
   `POST /points/payload` with `bm25_v`.
3. Every write stamps `bm25_v`, so a crashed or cancelled run resumes with what is left. Memory
   stays at one page of text; on the 45,563-chunk store it took 27 s with orchd at about 250 MB.

Until it runs, queries tokenized with v2 still match v1 vectors on every term the two share.

**Connections (issue #98).** A long run on macOS once stopped every ~11k chunks with `connect:
can't assign requested address`, which means the machine ran out of local ports. The cause was
not reproduced; measured, Go reuses a connection whose small unread remainder it drains on close,
and breaks reuse only past about 256 KiB left unread. The store now defends anyway:

- **Drains bodies:** every response body is read to EOF (up to 4 MiB) before it is closed.
- **Larger idle pool:** the client keeps up to 16 idle connections to Qdrant, not Go's default
  of 2, so concurrent ingests reuse theirs.
- **Retries port exhaustion:** a dial failing with `EADDRNOTAVAIL` is retried with backoff
  (250 ms doubling to 10 s, eight times). That is longer than macOS's ~30 s `TIME_WAIT`, so one
  run finishes instead of stopping. Nothing reached Qdrant, so this is safe for writes.
- **Counts connections:** the progress line every 10 s reports how many Qdrant connections the
  store has opened. A few means reuse; thousands is the #98 symptom, and the next place to look.

## Relabel

A relabel (SP-027, INV-08, INV-03) brings every stored chunk up to the applied spaces policy:
`rag.Service.Relabel` in `internal/rag/relabel.go`, run by orchd with its relabel placer
(`internal/daemon/relabel.go`) under the migration lock. A chunk is **stale** when its `labels_v`
is not the applied revision, which includes chunks stored before placement (no `labels_v`) and
every chunk after the operator applies a new revision.

1. Refuse with no applied revision, with an applied revision that no longer verifies (everything
   would be quarantined; content stays stale and unreadable until a revision is applied again),
   while a migration shadow is installed, or while a migration, backup drop or keyword re-index
   holds the lock.
2. Count every chunk of the base and each space collection, and list each collection's stale
   chunks by source (`must_not labels_v = <rev>`, payload `source_uri` only). Every collection is
   listed before any is changed, so a chunk moved into a collection not yet read is counted once.
3. For each stale source, under its write lock, re-read its chunks in that collection and place
   it again from its first chunk's stored origin, type, tags and writer. A source a live write
   replaced since it was listed is current and skipped.
4. Where the new space's collection is the one it is in, stamp the stale chunks' `space` and
   `labels_v` (`POST /points/payload`). Otherwise move the whole source: read its points with
   their vectors, upsert them into the new collection (created hybrid when missing; a dense
   chunk gets its keyword vector from its stored text, nothing is re-embedded) with the new
   `space`, `labels_v` and any reconstructed origin, prune surplus chunks of an older, longer copy
   there, check the new collection holds at least as many chunks, then delete the source from the
   old collection. A newer copy already in the new collection (a live write placed it there under
   the applied revision, with another write hash) is kept, and the stale copy is only deleted.
   Either way the source is then deleted from every other collection, so a copy an earlier,
   stopped run left where the source no longer belongs never stays readable there.
5. Without apply, steps 3 and 4 place and count only. The report gives totals, chunks changed and
   moved, chunks left stale, and counts per transition (`unplaced` for chunks stored with no
   space): counts and space names only, never a source, its text or a collection name.

**Resumable.** A source is copied before its old copy is deleted, and point IDs are
deterministic, so a run stopped between the two leaves a stale copy that the next run copies
again over the same points and deletes: the chunk count is unchanged and no source ends in two
collections, even when the next run places the source elsewhere. A source that cannot be placed
now (the policy changed mid-run, or GitHub could not be asked about a docs repo) or whose chunks
cannot be moved as they are (no dense vector) stays stale, is counted as left stale, and is retried
by the next run; the run goes on with the other sources. A failed store request stops the run with
a generic error, since Qdrant's errors name collections and so spaces (RC-350); the detail goes
to orchd's log.

**Placing stored content.** A chunk with a stored origin is placed from it, exactly as a write
would be ([Placement of writes](#placement-of-writes)). A chunk stored before SP-023 has none, so
its origin is reconstructed from the writer orchd stored with it and the URI that writer makes;
the reconstructed origin is stored with the chunks. A URI is read only for its own writer, so a
client's note named like another space's source is not placed by its name:

| Writer | URI | Origin |
|---|---|---|
| docs sync | `repo://<owner>/<repo>/<path>` | `github`, with the repo's visibility asked of GitHub again; when it cannot be asked (budget, error), the source stays stale |
| sources refresh | `checkout://<absolute path>`; a web URL | `fs` with the path; `web` with the URL |
| edit hand-off | `file://<absolute path>`; `claude://<path>` | `fs` with the path (under `~/.claude` for `claude://`), only while it resolves to itself: the live write placed what the path resolved to then, so a link, or a file that is gone, gets none |
| session hand-off, summaries | `session://<id>/record\|transcript` (a valid session ID) | `session`, tied to the session only when its record names a transcript that is that session's own, where its harness keeps it (the live write's check); placed by its pin |
| Ingest RPC | any | `client` with no writer space: quarantined |
| none recorded (before writers were, 2026-10-28) | any | none: quarantined |

Content that cannot be placed (no origin, a conflict between two equally strict spaces) goes to
`quarantine`, never to public or another space (INV-03); the operator can retag it (SP-071).
Accepted risk: a hand-off chunk stored without an origin (written between 2026-10-20, when
hand-offs began resolving links, and SP-023) is placed by the path it was edited at. A process
that can write there could replace a link with a regular file before the relabel, so content read
through the link is placed by the link's folder. It needs write access at that path and timing.

**Running it.** `orch rag relabel [--dry-run]` calls the `Relabel` RPC, which only the operator
may call (refused to queued tasks and harness sessions, since the report names spaces). A run
that stops part-way still reports what it did, with a generic error.

**Not done yet.** Reads do not filter on `labels_v` yet (SP-025): a
stale chunk left in its own space's collection by a revision change is readable there until
relabelled; one stored in the base or another space's collection is not read by a search of the
spaces it belongs to. A relabelled source's write hash still names its old placement, so its next
write is stored again (embedded once) rather than skipped as unchanged.

**Cost.** On the SP-002 corpus (40,000 chunks of 384 dimensions stored unplaced in one base
collection, every chunk moved to one of four space collections, Qdrant 1.17.1 on this Mac), a
relabel took 35.7 s (`TestIntegrationRelabel40k`, `ORCH_QDRANT_BENCH=1`).

## Retag

A retag (SP-071, D-14, INV-15) moves one source to the space the operator names:
`rag.Service.Retag` in `internal/rag/retag.go`. It reuses the relabel's move, so nothing is
re-embedded and the source's chunk count is unchanged.

1. Refuse with no source URI, no applied revision, or a name that cannot be a space (the space
   name pattern, which keeps it out of the collection path). orchd checks the name against the
   applied policy.
2. Under the source's write lock, refuse while a migration shadow is installed (checked under
   the lock, so a migration cannot start mid-retag and miss the move), then count the source in
   the base and every space collection.
3. Several collections holding it must hold the same write (the ingest hash of chunk 0), as a
   retag or relabel stopped between its copy and its delete leaves it; copies of different writes
   are refused, since the store cannot say which is current.
4. If only the target collection holds it, stamp its `space` and `labels_v`. Otherwise read its
   points with their vectors from another holder, upsert them into the target (created hybrid when
   missing) with the new `space` and the applied `labels_v`, prune surplus chunks of an older,
   longer copy there, check the target holds at least as many chunks, and only then delete the
   source from every other collection.

The report gives the spaces that held it (`unplaced` for the base), the chunks in the target and
whether they moved; a failed store request returns a generic error, with the detail in orchd's
log, as a relabel does. **Resumable:** point IDs are kept, so a retag stopped between its copy and
its delete leaves the same write in both collections, and running it again copies over the same
points and deletes the old copy. **Cost:** a 2,000-chunk source moved in 2.6 s on Qdrant 1.17.1
(`TestIntegrationRetag2000`, `ORCH_QDRANT_BENCH=1`).

A rag-level `Check` hook runs under the source's lock after the holders are counted and before
anything changes; orchd uses it for the checks below, so they and the move see the same store.

**In orchd** (`internal/daemon/retag.go`). `RetagSource` (`orch spaces retag`) is operator-only
(`refuseNonOperator`, and on the operator-only list for queued tasks). It needs an applied policy
that verifies, a space of that policy or `quarantine`, and the migration lock (shared with
migrations, keyword re-indexes and relabels). Under the source's lock it refuses, changing nothing:

- without `confirm`, a move into a public space, out of an isolated space, or out of quarantine or
  the unplaced base into a space (content of unknown origin may be another job's);
- with `expect_from`, a source not stored in that space now (any other holder but the target
  refuses too), so a caller such as `orch quarantine assign` gets an atomic answer.

- a policy revision applied since the request was read.

It then records the retag in orch.db (`spaces_retags`, migration 0031) as `moving`, with whether
it was confirmed, before anything moves (a store that cannot record it refuses the retag), moves
the source, and marks it `done`. A retag left `moving` (orchd exited mid-move) is run again 30 s
after the next start, under the migration lock and the current policy: a space the policy no
longer has sends the source to quarantine, and a move that now needs a confirmation it was not
given stays pending, with a log line, until the operator runs it again. A source no longer stored
closes it. A source not stored at all is `NotFound`.

A write placed before a retag of its source finished (it was being embedded meanwhile) is refused
under the source's lock (`rag.ErrRetaggedMidWrite`) instead of moving the source back; written
again, it is placed with the retag.

**The sticky override.** `writePlacer.place` reads the source's retag (`withRetag`) and applies it
with `spaces.ApplyOverride` (a space the policy no longer has quarantines), as follows. An
unreadable retag refuses the write rather than placing it without it.

| Writer | Honors the retag |
|---|---|
| docs sync, sources refresh | always: they place by what GitHub, a checkout or a bound prefix says |
| session records and summaries, edited-file hand-offs | only when placed without it (not quarantined): a session ID or a path any process can name must not borrow the retag's space. An edited file under a root that is retagged does follow it, so sessions that edit it write there |
| client notes (`Ingest`) | only from the operator or a session whose space is the target (RC-850, option (a)); other writers are placed by their own space and `mayReplace` refuses them |
| relabel | for a source stored in the retag's space; otherwise by its writer, as above |

The public gate still applies after the override: only the docs sync and the sources refresh write
into a public space, so another writer's write of a source retagged to public goes to
`public_root_sessions` (D-64).

**Not done yet.** Orch.app has no retag control; a retag row outlives its source's deletion; a retag cannot be cleared (retag the source to
where you want it instead). Like a relabelled source, a retagged source's write hash still names
its old placement, so its next write is embedded once (RC-752).

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

It also lists `repo://local/<repo>/<path>` and `repo://<owner>/<repo>/<path>` docs an earlier
sources refresh stored (no writer, no docs-sync hash, the owner not tracked by the docs sync), but
only once their replacement exists: the `checkout://` URI the refresh now gives that file (under the
registered repo source with that directory name, or `<known_root>/<owner>/<repo>` for an owner
source), stored by the sources refresh. A repo whose refresh is skipped, moved or unregistered keeps
its old copy, since it may be the only one, and so does one whose old URI two registered repos
share (same directory name), or whose path has a dot segment.

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

## Spaces

Memory spaces separate what each job's sessions can read ([the spaces plan](../spaces/spaces-features.json)).
The policy that defines them, the operator-only apply, and drift are described in
[config.md](config.md#spaces-policy). `spaces.Resolved.CanRead(reader, target)` is the rule read
paths will use: a space reads itself, every public space and the private spaces it lists, and
never another isolated space or quarantine. Every write is stamped with its origin, space and
`labels_v` ([Placement of writes](#placement-of-writes)) and stored in its space's own collection
([Storage](#storage-qdrant)); search reads the collections of the spaces it is given. Which spaces
each caller is given is SP-025; until then searches read every collection. Until a policy with spaces is applied, everything is in the one implicit `shared` space
and search behaves as it always has.

## Placement

`spaces.Classify(origin, resolved, options)` (`internal/spaces/classify.go`) decides which space
content goes to. It is a pure function: writers report facts about where content came from as a
`spaces.Origin` (system, source URI, local path, GitHub repo with its current owner and
visibility, URL, chat tenant, and the space of the pinned session that wrote it) and never choose
a space themselves. orchd's write placer calls it for every write
([Placement of writes](#placement-of-writes)). The result is a
space, `quarantine`, or a conflict (also `quarantine`), with the deciding rule, a reason, every
matching root and the remote's effect.

- **No policy:** everything is `shared`, as before spaces (`no_policy`).
- **Checkouts, docs syncs and sessions** combine two candidates. The root candidate is the deepest
  bound root holding the path, matched case-insensitively at path boundaries, like a CODEOWNERS
  file (`~/code/company10` is not under `~/code/company1`). The remote candidate is the space of
  the repo's owner. A fork's parent is ignored, and a transferred repo counts under its current
  owner. When both exist and differ, the stricter kind wins (`root_stricter`, `remote_stricter`).
  Two different spaces of the same kind are a `conflict`: two isolated spaces, as the plan
  requires, and also two private or two public ones, since neither is stricter. With neither
  candidate the origin is `unmatched`. A docs sync has no path, so only its remote counts.
- **Public needs proof.** A result that would be public is quarantined (`not_verified_public`)
  whenever the checkout has a remote that GitHub has not confirmed is public, whoever owns it. That
  includes the times GitHub cannot be reached. A public root is never proof for local content
  (D-50, D-65, SP-030): nothing on disk, not even a public root bound exactly at the folder,
  declares a folder public, since a session can create any of it. D-50's "declares that folder
  public" is now the policy's `public_paths` (SP-076, D-65; forms in
  [config.md](config.md#spaces-policy)): a file whose deepest root is public and that would be
  placed public is public (`public_path`) only when a `public_paths` pattern of that root's space
  matches it. Otherwise it is quarantined (`no_public_proof`), whatever its checkout or remote
  (GitHub, another host, none, an anonymous or symlinked `.git`). A matched file still stays out
  when its repo is not verified public (`not_verified_public`) or its remote names a stricter
  space's org (`no_public_proof`: a `.git` a session writes never moves content). A stricter root
  nested in the public one, or inside a `**` pattern, still decides first, and so does a deeper
  root of another public space. Public repos reach public through the docs sync, and an operator
  retag wins over everything. Sessions keep D-22 (below), an edit hand-off of such a file goes to
  `public_root_sessions` unless its repo is not verified public, and a checkout's harness
  settings still follow its root.
- **A listed folder survives a GitHub blip** (RC-402). When the visibility check of a listed
  folder's checkout gets no answer from GitHub (it cannot be reached, or the lookup budget of 30
  a minute is spent), its stored public copy is not taken out; new writes from it stay out of
  public until GitHub answers. The grace lasts 6 hours from when GitHub last said the repo is
  public, as orchd remembers since it started, because any caller can spend the shared budget;
  after that, or after a restart, an unanswered repo leaves. GitHub answering that the repo is
  private, or refusing to show it (403 or 404), takes the copy out as before.
- **Org bindings must hold.** A remote whose owner's binding is suspended or unconfirmed
  (SP-021) sends the content to quarantine (`org_unconfirmed`). `Engine.Place` reads the binding
  from `orch.db`. A nil `Options.Confirmed` confirms nothing.
- **Sessions** are placed like their first cwd. A session under a public root reads as that public
  space, so it reads public only, and its content goes to `public_root_sessions` (D-22). A session
  never writes to a public space. In a conflict or unmatched folder it has no space.
- **Web pages** go to the longest bound prefix they fall under, at a path boundary. URLs and
  prefixes are compared after normalization: http and https treated as one site, the host
  lowercased, a default port and
  trailing dots on the host dropped, the port read as a number, dot segments resolved, the path
  re-escaped and its trailing slash ignored. So `https://a.com/x/../private`,
  `https://a.com/%70rivate` and `https://a.com/private` all fall under `https://a.com/private/`,
  and `https://wiki.corp.com` falls under `https://wiki.corp.com/`. Paths stay case-sensitive. A
  URL that cannot be normalized, such as one with a backslash in its path (some servers read it as
  a slash) or a bad escape, is quarantined even when a session ingested it, because it cannot be
  checked against stricter prefixes. A page a pinned
  session ingested takes the session's space (D-14). When the prefix's space differs from the
  session's, the stricter wins, and two equally strict spaces are a conflict.
- **Client notes** take the writing session's space. Without a pinned session they are
  unmatched. A pin naming a space the policy no longer has, or a public space, is not trusted
  (`writer_unknown`).
- **Chat** goes to the space bound to `<provider>:<tenant>`.
- **A sticky retag override** (`Options.Override`, recorded by SP-071) wins over every rule. One
  naming a removed space quarantines.

An unmatched or conflicting origin is never placed in public (`TestClassifyNeverPublicWhenUnplaced`).
Paths are compared as given. Symlinks are not resolved, so a writer passes the path it actually
used.

**Explaining a placement.** `ExplainPlacement` (`orch spaces why <target>`) builds the origin
itself. A path's remote comes from the checkout's `.git` files, read without running git
(`handoff.FindCheckout`). An anonymous checkout, whose `.git` file does not point back, has no
remote. A repo's current owner and visibility come from `gh api repos/<owner>/<repo>`, cached for
10 minutes, with at most 512 entries and 30 uncached lookups a minute across all callers. Without
an answer the repo keeps the owner its remote names and an unknown visibility, which is never
public. For a path, the operator also sees where a session started there would write and which
space it would read as. Until sessions are pinned (SP-010, SP-011), orchd cannot tell an agent's
own space. So any caller other than the operator gets an answer only when every space it names is
public and any remote's owner is bound to a public space. Otherwise, and on any error, it gets one
fixed refusal that does not say why. The remote rule matters because an agent can write a `.git`
naming any org under a public root. If an unbound owner were answered, the refusal would show
which orgs the other spaces bind. One thing still shows: an agent can learn that a folder it
names lies under a non-public root. Folder names are already visible to it on disk. Queued tasks
may call it on the same terms.

## Cost of a collection per space

Memory spaces (D-10) plan one Qdrant collection per space, with the payload filter kept as a
second layer. `TestIntegrationSpacesCost` (`internal/rag/spaces_bench_it_test.go`) measures what
that costs against one collection filtered by payload. It is gated: set `ORCH_QDRANT_BENCH=1`
(and `ORCH_QDRANT_BIN_DIR` to reuse a downloaded engine, `ORCH_QDRANT_BENCH_SEGMENTS` to set
the segments per collection). It runs its own engine on 127.0.0.1:6421 with temporary storage.

**Setup:** 40,000 synthetic chunks in 4 spaces: 384-dimension dense vectors clustered around 256
centres, BM25 vectors from the real tokenizer over a Zipf vocabulary with code identifiers. The
same chunks are loaded once into one collection and once split into 4 space collections. Each
search goes through `Service.Search` (hybrid, weighted, TopK 10) for 200 queries after 20
warm-up queries. The baseline searches the one collection filtered to 2 spaces
(`tags` any). The split layout searches the 2 allowed space collections in parallel and fuses the
two lists by reciprocal rank (k = 60).

**Results** (qdrant v1.17.1, Apple M3 Max, 2026-11-08):

| | default segments (one per CPU) | 2 segments |
|---|---|---|
| baseline p50 / p95 | 16.5 / 17.1 ms | 11.2 / 11.8 ms |
| 2 space collections p50 / p95 | 31.1 / 32.9 ms | 16.4 / 17.5 ms |
| p95 ratio | 1.93x | 1.49x |
| relabel 40,000 chunks, one filtered `set_payload` | 0.33 s | 10.3 s |
| relabel across 4 space collections | 0.40 s | 0.44 s |

Both layouts return the same top 10 for 85.5% of hits.

**Where the time goes.** Collection size barely matters. A dense request takes 4.1–4.3 ms on
either 40,000 or 10,000 chunks with default segments, and 1.4 ms with 2 segments. A keyword
request takes 8.2–9.0 ms in every case, and 1.8–2.0 ms of that without its dense vectors. Search
returns those vectors so it can compute cosine for keyword-only hits. The engine did not overlap
concurrent requests: 4 raw requests in parallel took about their sum (13–30 ms). So each extra
space collection adds a whole hybrid search, and the split layout costs 1.5–1.9x at p95. That is
over the plan's 1.3x threshold. The layout decision (D-10 in
[the spaces plan](../spaces/spaces-features.json)) was revisited on 2026-11-08 and kept: the
measured cost is accepted (D-24). Changes that would reduce it: fewer segments for small space
collections, keyword requests that don't return vectors, or one request per collection using
Qdrant's server-side fusion. None of these was measured as a full layout.

Relabelling is cheap either way: at most 10.3 s for 40,000 chunks, well under the 120 s threshold.

**As shipped (SP-024.3).** `TestIntegrationSpaceCollectionsSearch`
(`internal/rag/spacestore_bench_it_test.go`, gated like the above; its own engine on
127.0.0.1:6431, collections `sp-024-*`) loads the same corpus into one collection and into
`sp-024-bench__s0..s3`, and times 200 queries (after 20 warm-up) three ways, interleaved: the
SP-002 baseline (`Service.Search` on the one collection filtered to 2 spaces, as it runs today),
the shipped search (`Service.Search` with `Spaces: [s0 s1]`), and the one collection searched
with the shipped search's request pattern (keyword requests without vectors, vectors fetched
afterwards), which separates the layout's cost from the request's. Qdrant v1.17.1, Apple M3 Max,
2026-11-14:

| | default segments | 2 segments |
|---|---|---|
| SP-002 baseline p50 / p95 | 16.9–17.1 / 18.0–18.1 ms | 12.0 / 12.6–12.7 ms |
| 2 space collections p50 / p95 | 17.0–18.7 / 19.5–20.2 ms | 4.8–5.0 / 5.6–5.7 ms |
| p95 ratio to the baseline | 1.08–1.12x | 0.44–0.45x |
| one collection, same requests, p95 | 8.3–8.4 ms | 2.7–2.8 ms |
| space collections vs that, p95 | 2.34–2.40x | 2.01–2.07x |

A third default-segment run on a loaded machine measured 0.80x (baseline p95 32.6 ms, spaces
26.2 ms). The shipped search returns the baseline's top 10 for 99.7% of hits (85.5% with SP-002's
fusion of final lists): lists are merged by score before fusion.

So against the search users had, reading two space collections is within the plan's 1.3x, but
only because the multi-collection path asks for fewer vectors; the layout itself still costs
about 2x what one collection costs with the same requests, as SP-002 found and D-24 accepted.
A search of one collection keeps the request it always made (INV-10); giving it the same request
pattern would halve its latency (8.4 against 18.0 ms at p95) and is left as a follow-up.

## Caches keyed by space (INV-11)

Every cache in `internal/rag` that holds content, embeddings or what decides whether content is
stored is keyed by space:

- **The unchanged-write check** (`ingest_hash` on a source's first and last chunks) is read in the
  write's own space's collection only, and the hash includes the space, so a write found stored
  for one space is never skipped for another (`TestWriteCacheIsKeyedBySpace`).
- **Store objects and their cached layout** are kept per collection name, so one space's known
  layout is never used for another's (`TestStoreCachesAreKeyedByCollection`).
- **The list of space collections** is cached per base collection.

There is no query, embedding or result cache in `internal/rag`. Caches elsewhere in orch that hold
content are not keyed by space yet: the PR detail cache in orchd (keyed by PR URL, whose owner
decides the space) and the docs sync's per-file hash cache (keyed by URI; RC-124 covers its
placement). The space a read may see there is SP-025's to enforce.
