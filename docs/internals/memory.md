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
   splits at blank lines, never inside a fenced block unless the block alone is too large.
5. Blank-line runs collapse to one outside fences; a final pass re-splits anything still too large.

The chunk's `heading` payload is the deepest path all its sections share.

## Storage (Qdrant)

**Engine:** in `binary` mode orchd downloads the pinned release (`v1.17.1`,
`qdrant-{aarch64,x86_64}-apple-darwin` or `-unknown-linux-musl`) to `<data>/bin/qdrant` and runs it
on 127.0.0.1 with telemetry off, storage in `<data>/qdrant`, and the API key
(`QDRANT__SERVICE__API_KEY`, sent by the client as the `api-key` header). Readiness: `GET /readyz`.

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
| `started_at`, `ended_at` | Unix seconds the chunk describes, if known |
| `ingested_at` | Unix seconds stored |

Source URIs in use: `session://<id>/record|transcript`, `repo://<owner>/<repo>/<path>`,
`file://<path>`, `claude://<path under ~/.claude>`, web URLs, and anything given to `orch ingest`.

## BM25 keyword vectors

`TokenizerVersion = 1`; changing tokenization requires bumping it and re-indexing.

- **Tokens:** NFC-normalize; match `[\pL\pM\pN_]+(?:[./:\-][\pL\pM\pN_]+)*`; split each match
  into script runs. Han, kana, Hangul and Thai runs become overlapping 2-rune bigrams. Other runs
  split into parts at `. / : - _` and camelCase boundaries (`HTTPServer` → `http`, `server`),
  lowercased; emit the whole identifier (≤ 64 bytes) when it has several parts, and each part of at
  least 2 runes that is not a stopword. Caps: 8192 tokens, 64 parts, 64-byte terms.
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
   dense and 0.5 for keyword (`rag.search.keyword_weight`). Sort by fused score, then cosine.
4. Every hit reports its **cosine** (computed from the returned vector for keyword-only hits), its
   BM25 score, and `Exact`: whether it contains, as a whole token, a code identifier from the
   query. Identifiers come from the first 8 KiB of the query, at most 16, each 5–128 characters:
   camelCase, snake_case, a dotted name of 2+ parts (each 2+ characters) not ending in a domain or
   file suffix (`com`, `io`, `md`, `json`, `yaml`, …), or a path with three parts or one part of 8+
   characters. Versions, numbers and a stop list (`e.g`, `i.e`, `a.k.a`, `go.mod`, `makefile`, …)
   are excluded.
5. **Fallback:** if the collection has no keyword index or keyword search fails, configured hybrid
   answers by meaning and returns a note; an explicitly requested hybrid fails instead. Fallback
   logs are limited to one a minute.

Scores keep cosine meaning in both modes, so relevance floors (prompt context) work unchanged.

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

## Sources and docs

**Sources registry** (`sources` table, `orch source|repo`), refreshed when
`now − last_fetched ≥ refresh_secs`:

- `web`: GET (≤ 5 MiB), strip HTML, redact, store under the URL. Errors keep the old chunks.
- `repo` (absolute path) and `owner` (`<known_root>/<owner>/*`): every `*.md` found by `mdwalk`
  (skipping `.git` and `ingest.exclude_dirs`), stored as `repo://<owner|local>/<repo>/<path>`,
  type `repo_doc`, tagged `repo:<name>` and `owner:<owner>`.
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
  - `repo` and `owner`: `repo_doc` content under the source's `repo://` prefix that carries its
    `repo:<name>` or `owner:<name>` tag. Content of an owner the docs registry tracks, or with a
    row in `doc_hashes`, is kept.
  - It is refused for a repo source that shares its directory name with another (they share
    content), and for a stored row that fails validation (from before it existed).
  - A refresh running during the purge can store a source's docs again; purge again to remove them.

**Docs registry** (`orch docs add <owner>`, every 6 h): list the owner's repos with
`gh api graphql` (skipping archived and forks by default), skip repos whose default-branch HEAD is
unchanged, `gh repo clone --depth 1` into `<data>/repos`, and store each markdown file whose
SHA-256 changed (hash saved only after a successful store).

`ListDocs` serves the docs registry: each owner, its repos with the indexed default-branch commit,
and every indexed file with its GitHub URL, `https://github.com/<owner>/<repo>/blob/<commit>/<path>`.
The URL is pinned to the indexed commit, so the page shows the version that was indexed; with no
commit recorded it uses `HEAD`. `GetDocument` rebuilds any indexed source from its chunks (the
same rebuild the hybrid migration uses). It links the original only for a `web` source or a synced
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

## Retrieval evaluation

`orch rag eval` runs each case of `<config>/rag-eval.yaml` (`cases: [{query, expect: [uri|prefix*],
scope?, source_type?, tags?}]`) through normal search, counting hits by distinct source. Metrics:
hit@1, recall@k (`--top-k`, default 10), MRR. Runs are saved as JSON under `<data>/eval/` and can
be compared case by case.
