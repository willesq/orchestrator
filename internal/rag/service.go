// Package rag implements retrieval-augmented storage: chunk text, embed it, and
// upsert/search vectors in Qdrant. The embedder is injected so the package stays
// independent of the ONNX runtime and testable in isolation.
package rag

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/willesq/orchestrator/internal/bm25"
	"github.com/willesq/orchestrator/internal/timeline"
)

// Embedder embeds texts into vectors (implemented by internal/embed.Embedder).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimension(ctx context.Context) (int, error)
}

// Meta describes the provenance and scoping of ingested content.
type Meta struct {
	SourceURI  string
	SourceType string
	Scope      string
	Tags       []string
	// StartedAt and EndedAt date the whole source, such as a session record.
	StartedAt, EndedAt time.Time
	// Timeline dates each chunk by the timeline markers in its own text, as a
	// transcript carries them. StartedAt and EndedAt, when set, then bound
	// those times, so no chunk is dated outside its session.
	Timeline bool
	// KeepUndated stores content that has no date of its own undated, instead
	// of dating it by when it is stored: a migration's copy is not new content.
	KeepUndated bool
	// Writer names the part of orchd storing the source (WriterDocs, ...). It
	// decides the trust tier stored with every chunk; only orchd's own code
	// sets it, so a client cannot claim a tier by its choice of type or URI.
	Writer string
	// ContentHash identifies the original text. IngestText computes it; a
	// migration's copy passes the original's along, since its rebuilt text
	// can differ in whitespace.
	ContentHash string
	// Origin is the facts the writer reports about the content (SP-023),
	// stored as payload "origin". With a placer set, a write without one is
	// refused.
	Origin Origin
	// Space and LabelsV are where the content was placed and the applied
	// spaces policy revision that placed it (payload "space" and
	// "labels_v"). With a placer set (Service.Place) only the placer's
	// answer is stored, whatever a caller put here; without one they are
	// stored as given, and an empty Space stores neither.
	Space   string
	LabelsV int64
}

// Service ties the embedder and vector store together.
type Service struct {
	emb Embedder
	// gate is held shared by every write for its whole duration. A migration
	// takes it exclusively, briefly, to add or remove its shadow store, so no
	// write is ever half on one side of that change.
	gate sync.RWMutex
	// stripes serialize writes to one source: a live write and the migration's
	// copy of the same source never interleave, while other sources proceed.
	stripes [64]sync.Mutex
	// mu guards the fields below: a migration adds a shadow that writes also
	// reach, then swaps it in as the active store. Both are base collections:
	// each space's collection is named after them (SpaceCollection).
	mu     sync.RWMutex
	store  *QdrantStore
	shadow *QdrantStore
	// touched records sources a live write has fully written to the shadow
	// during a migration; the copy leaves those alone. Keyed by sourceKey:
	// a source is copied per space collection.
	touched map[string]bool
	// dirty records sources a write left inconsistent between the two stores
	// during a migration; the migration copies them again before it switches.
	dirty map[string]bool
	// derivedMu guards derived, the stores of space collections by name,
	// lists, each base's known space collections, and noted.
	derivedMu sync.Mutex
	derived   map[string]*QdrantStore
	lists     map[string]spaceList
	// noted holds, by base, the space collections this process created.
	noted map[string]map[string]bool
	// StateDir holds migration markers, so an interrupted migration can be
	// told apart from a finished one.
	StateDir string
	// BM25 weights the sparse vectors a hybrid collection stores.
	BM25 bm25.Params
	// SearchMode is ModeHybrid or ModeDense (rag.search.mode).
	SearchMode string
	// RRFK is reciprocal rank fusion's constant (rag.search.rrf_k).
	RRFK int
	// KeywordWeight scales the keyword list's share of the fusion
	// (rag.search.keyword_weight); 1 weighs both lists alike, the default 0.5
	// gives meaning the larger share.
	KeywordWeight float64
	// Ranking weights fused candidates by source type and recency
	// (rag.search.ranking and its weights).
	Ranking RankingParams
	// PerSource caps the hits one source may return to a search that does not
	// set its own cap (rag.search.per_source); 0 leaves them uncapped.
	PerSource int
	// now is the clock ranking measures age by; nil is time.Now.
	now func() time.Time
	// writerIndexed records that the writer payload index was added to an
	// older collection; writerMu guards it so a failure is retried.
	writerMu      sync.Mutex
	writerIndexed bool
	// searchIndexed is set while, or once, the search filter indexes are being
	// added to a collection made before them.
	searchIndexed atomic.Bool

	fallbackLog rateLog
	// retagMu guards retagGens, how many retags each source has had (in
	// this process), so a write placed before a retag is not stored after it.
	retagMu   sync.Mutex
	retagGens map[string]uint64
	// progress is set while a migration copies sources.
	progress atomic.Pointer[MigrationProgress]
	// chunkTokens and overlap size chunks in model tokens.
	chunkTokens int
	overlap     int
	// MarkdownSections chunks markdown by heading section instead of by word
	// window (embedder.markdown_sections).
	MarkdownSections bool
	// lazyVectors makes a search of one collection fetch keyword hits'
	// vectors afterwards, as a search of several does; the SP-024.3
	// benchmark sets it to compare the layouts like with like.
	lazyVectors bool
	// Place decides every write's space from its origin (SP-023). Set once,
	// before the service is shared; nil stores writes unplaced, as tools and
	// tests do.
	Place PlaceFunc
}

// NewService constructs a RAG service. chunkTokens and overlap are in model
// tokens; chunkTokens is capped at what the model embeds.
func NewService(emb Embedder, store *QdrantStore, chunkTokens, overlap int) *Service {
	return &Service{emb: emb, store: store, chunkTokens: chunkTokens, overlap: overlap, MarkdownSections: true,
		BM25: bm25.DefaultParams, SearchMode: ModeHybrid, RRFK: 60, KeywordWeight: 0.5, Ranking: DefaultRanking()}
}

// active returns the store reads go to.
func (s *Service) active() *QdrantStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store
}

// stores returns the active store and, during a migration, its shadow.
func (s *Service) stores() (active, shadow *QdrantStore) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store, s.shadow
}

func (s *Service) stripe(uri string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(uri))
	return &s.stripes[h.Sum32()%uint32(len(s.stripes))]
}

// lockSource takes the write gate (shared) and the source's stripe; the
// returned function releases both.
func (s *Service) lockSource(uri string) func() {
	s.gate.RLock()
	mu := s.stripe(uri)
	mu.Lock()
	return func() { mu.Unlock(); s.gate.RUnlock() }
}

// shadowResult records how a write fared against the shadow: only a source
// fully written there is left alone by the copy, and one that is not is marked
// dirty, so the migration copies it again from the active store before it
// switches. key is the source in its space (sourceKey).
func (s *Service) shadowResult(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.touched == nil {
		return
	}
	if err != nil {
		delete(s.touched, key)
		s.dirty[key] = true
		_, uri := splitKey(key)
		log.Printf("rag: %s reached the active store but not the migration target (%v); it is copied again before the switch", uri, err)
		return
	}
	s.touched[key] = true
	delete(s.dirty, key)
}

// markDirty records that a write left a source (key, see sourceKey) possibly
// inconsistent between the stores (it failed part-way on the active one)
// during a migration.
func (s *Service) markDirty(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty != nil {
		delete(s.touched, key)
		s.dirty[key] = true
	}
}

// takeDirty returns and clears the sources needing another copy.
func (s *Service) takeDirty() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.dirty))
	for u := range s.dirty {
		out = append(out, u)
	}
	for _, u := range out {
		delete(s.dirty, u)
	}
	sort.Strings(out)
	return out
}

func (s *Service) dirtyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.dirty)
}

// ActiveCollection names the collection searches use.
func (s *Service) ActiveCollection() string { return s.active().Collection() }

// split chunks text for ingestion, counting with the embedder's own tokenizer
// when it has one.
func (s *Service) split(text string) []chunk {
	tc, _ := s.emb.(TokenCounter)
	m := newMeasure(tc)
	text = cleanText(text)
	var pieces []chunk
	if s.MarkdownSections && isMarkdown(text) {
		pieces = markdownChunks(text, s.chunkTokens, s.overlap, m)
	} else {
		for _, c := range chunkText(text, s.chunkTokens, s.overlap, m) {
			pieces = append(pieces, chunk{text: c})
		}
	}
	// Whatever the chunkers did, nothing leaves here larger than the model
	// embeds: an oversized chunk is re-split into plain windows.
	limit := s.chunkTokens
	if limit <= 0 || limit > maxChunkTokens {
		limit = maxChunkTokens
	}
	var out []chunk
	for _, c := range pieces {
		if tokens(m, c.text) <= limit {
			out = append(out, c)
			continue
		}
		for _, w := range chunkText(c.text, limit, 0, m) {
			out = append(out, chunk{text: w, heading: c.heading})
		}
	}
	return out
}

// IngestText chunks, embeds, and upserts text. Chunks for the same source_uri
// are replaced via deterministic point IDs (uri#index).
//
// With a placer set (Place), the write is placed first and stamped with its
// origin, space and labels_v; a write the placer refuses stores nothing.
//
// The write goes to its space's collection only (SpaceCollection). A source
// lives in one space, so a copy another collection holds (the source was
// placed elsewhere before) is deleted first; if that fails nothing is
// written, and the next write tries again.
func (s *Service) IngestText(ctx context.Context, text string, m Meta) (int, error) {
	gen := s.retagGeneration(m.SourceURI)
	if err := s.place(ctx, &m); err != nil {
		return 0, err
	}
	// An unchanged write is skipped before embedding, so re-fetching an
	// unchanged page neither costs an embed nor makes it look new. During a
	// migration every write goes through, so the shadow gets it too.
	if m.SourceURI != "" {
		if n, same := s.skipUnchanged(ctx, text, m); same {
			return n, nil
		}
	}
	points, dim, err := s.prepare(ctx, text, m)
	if err != nil || len(points) == 0 {
		return 0, err
	}
	defer s.lockSource(m.SourceURI)()
	if s.retagGeneration(m.SourceURI) != gen {
		// Reason: it was placed before a retag of the source finished, so
		// storing it would move the source back where the retag took it
		// from (SP-071).
		return 0, ErrRetaggedMidWrite
	}
	if err := s.removeElsewhere(ctx, m); err != nil {
		return 0, err
	}
	active, shadow := s.storeFor(m.Space)
	key := sourceKey(m.Space, m.SourceURI)
	// Noted before the write: a write that fails part-way may still have
	// made the collection and left points in it.
	s.noteCollection(s.active(), m.Space)
	if err := writePoints(ctx, active, dim, m.SourceURI, points); err != nil {
		if shadow != nil {
			s.markDirty(key)
		}
		return 0, err
	}
	if shadow != nil {
		s.shadowResult(key, writePoints(ctx, shadow, dim, m.SourceURI, points))
	}
	return len(points), nil
}

// skipUnchanged checks, under the source's write lock so no write is half
// done meanwhile, whether the store already holds this exact write.
func (s *Service) skipUnchanged(ctx context.Context, text string, m Meta) (int, bool) {
	defer s.lockSource(m.SourceURI)()
	active, shadow := s.storeFor(m.Space)
	if shadow != nil {
		return 0, false
	}
	// Reason: the stored copy is looked for in the write's own space only,
	// so a write cached as unchanged in one space is never skipped for
	// another (INV-11).
	return s.unchanged(ctx, active, m.SourceURI, s.ingestHash(text, m))
}

// prepare chunks, embeds and weights text into points, without writing them.
func (s *Service) prepare(ctx context.Context, text string, m Meta) ([]Point, int, error) {
	// Required here, not just in the callers: the prune below deletes by
	// source_uri, so an empty one would match every URI-less point in the store
	// and destroy it. The layer that performs a delete validates its own input.
	if m.SourceURI == "" {
		return nil, 0, fmt.Errorf("source_uri is required")
	}
	pieces := dedupeChunks(s.split(text))
	if len(pieces) == 0 {
		return nil, 0, nil
	}
	chunks := make([]string, len(pieces))
	for i, p := range pieces {
		chunks[i] = p.text
	}
	vecs, err := s.emb.Embed(ctx, chunks)
	if err != nil {
		return nil, 0, err
	}
	dim, err := s.emb.Dimension(ctx)
	if err != nil {
		return nil, 0, err
	}
	points := make([]Point, len(chunks))
	ingested := time.Now()
	contentHash, writeHash, trust := ContentHash(text), s.ingestHash(text, m), TrustFor(m.Writer, m.SourceType)
	if m.ContentHash != "" {
		contentHash = m.ContentHash
	}
	// A transcript chunk cut from the middle of a long turn has no marker of its
	// own; it belongs to the last turn dated before it.
	var carried time.Time
	for i, c := range chunks {
		payload := map[string]any{
			"text":        c,
			"source_uri":  m.SourceURI,
			"source_type": m.SourceType,
			"scope":       m.Scope,
			"tags":        m.Tags,
			"chunk_index": i,
			"chunk_total": len(chunks),
			// Which tokenizer made the keyword vector, for ReindexKeywords.
			fieldBM25Version: bm25.TokenizerVersion,
			fieldContentHash: contentHash,
			fieldIngestHash:  writeHash,
		}
		if m.Writer != "" {
			payload[fieldWriter] = m.Writer
			payload[fieldTrust] = trust
		}
		if o := m.Origin.payload(); len(o) > 0 {
			payload[fieldOrigin] = o
		}
		if m.Space != "" {
			payload[fieldSpace] = m.Space
			payload[fieldLabelsV] = m.LabelsV
		}
		if h := pieces[i].heading; h != "" {
			payload["heading"] = h
		}
		start, end := m.StartedAt, m.EndedAt
		if m.Timeline {
			start, end = chunkSpan(c, &carried)
			start, end = clamp(start, end, m.StartedAt, m.EndedAt)
		}
		if start.IsZero() && end.IsZero() {
			// Every chunk is dated, so date-range searches find it: by its
			// source's own span when it has one (a commit date, a session),
			// otherwise by when it was stored (a fetch, a note).
			start, end = m.StartedAt, m.EndedAt
			if start.IsZero() && end.IsZero() && !m.KeepUndated {
				start, end = ingested, ingested
			}
		}
		setTime(payload, fieldStartedAt, start)
		setTime(payload, fieldEndedAt, end)
		setTime(payload, fieldIngestedAt, ingested)
		points[i] = Point{ID: pointID(m.SourceURI, i), Vector: vecs[i], Sparse: bm25.Document(c, s.BM25), Payload: payload}
	}
	return points, dim, nil
}

// dedupeChunks drops a chunk whose text repeats an earlier chunk of the same
// source: a repeated block (boilerplate, a pasted log) would otherwise be
// stored, and returned, twice.
func dedupeChunks(pieces []chunk) []chunk {
	seen := make(map[[sha256.Size]byte]bool, len(pieces))
	out := pieces[:0:0]
	for _, p := range pieces {
		h := sha256.Sum256([]byte(p.text))
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, p)
	}
	return out
}

// writePoints stores one source's chunks in st and prunes what is left of an
// older, longer version of it.
func writePoints(ctx context.Context, st *QdrantStore, dim int, uri string, points []Point) error {
	if err := st.EnsureCollection(ctx, dim); err != nil {
		return err
	}
	if err := st.Upsert(ctx, points); err != nil {
		// A source too large for one request can fail between batches, leaving
		// it part new, part old. Say so where it will be seen: hooks discard
		// ingest errors, so the log is the only trace.
		var partial *PartialWriteError
		if errors.As(err, &partial) {
			log.Printf("rag: %s in %s is part old, part new (%v); re-ingest it", uri, st.Collection(), partial)
			return fmt.Errorf("%s was only partly rewritten; re-ingest it: %w", uri, err)
		}
		return err
	}
	// Re-ingesting a source that shrank would otherwise leave the previous
	// version's surplus chunks behind: point IDs are uri#index, so indices past
	// the new length are never overwritten and would keep matching searches.
	return pruneChunksFrom(ctx, st, uri, len(points))
}

// pruneChunksFrom deletes chunks of a source at index >= from.
func pruneChunksFrom(ctx context.Context, st *QdrantStore, uri string, from int) error {
	filter := map[string]any{"must": []any{
		map[string]any{"key": "source_uri", "match": map[string]any{"value": uri}},
		map[string]any{"key": "chunk_index", "range": map[string]any{"gte": from}},
	}}
	n, err := st.Count(ctx, filter)
	if err != nil || n == 0 {
		return err
	}
	return st.DeleteByFilter(ctx, filter)
}

// maxScrollPages bounds a full-collection scan at 10M points (1000 per page).
// Reaching it means the store outgrew this aggregation strategy or the cursor is
// misbehaving; either way, failing beats looping.
const maxScrollPages = 10_000

// SourceSummary is one distinct source held in the store.
type SourceSummary struct {
	SourceURI  string
	SourceType string
	Scope      string
	Tags       []string
	Chunks     int
	// Writer is the part of orchd that stored it ("" before writers were recorded).
	Writer string
	// StartedAt and EndedAt span the source's dated chunks; IngestedAt is the
	// latest store of any of them. Zero when unknown.
	StartedAt, EndedAt, IngestedAt time.Time
}

// ListSourcesOpts filters the inventory.
type ListSourcesOpts struct {
	SourceType string
	Scope      string
	URIPrefix  string
	Limit      int
	// Range keeps only sources with content dated within it.
	Range TimeRange
	// Spaces are the spaces listed, as SearchOpts.Spaces: nil lists every
	// collection, an empty, non-nil list lists nothing (SP-025).
	Spaces []string
	// LabelsV is as SearchOpts.LabelsV.
	LabelsV int64
}

// ListSources enumerates distinct sources in the store with their chunk counts.
//
// This scans the collection rather than consulting an index, because nothing
// maintains a source-level table — the vector store is the only record of what
// has been ingested. Cost is linear in points, which is acceptable at this
// store's scale and honest about where the data lives.
// Returns the (possibly limited) summaries, the distinct source count before
// the limit, and the total chunks across them.
func (s *Service) ListSources(ctx context.Context, opts ListSourcesOpts) ([]SourceSummary, int, int, error) {
	must := map[string]string{}
	if opts.Scope != "" {
		must["scope"] = opts.Scope
	}
	if opts.SourceType != "" {
		must["source_type"] = opts.SourceType
	}
	fields := []string{"source_uri", "source_type", "scope", "tags", fieldStartedAt, fieldEndedAt, fieldIngestedAt, fieldWriter}

	agg := map[string]*SourceSummary{}
	total := 0
	// The caller's spaces (SP-025) choose the collections, each filtered on
	// its space as the second layer, as a search is.
	stores, err := s.listStores(ctx, opts.Spaces)
	if err != nil {
		return nil, 0, 0, err
	}
	stores = withLabels(stores, opts.LabelsV)
	for _, ss := range stores {
		st := ss.st
		filter := buildFilter(ss.must(must), nil, nil, opts.Range)
		var offset any
		// A server that returns a non-advancing cursor would otherwise spin forever:
		// the per-request timeout bounds each call but not the loop.
		for page := 0; ; page++ {
			if page >= maxScrollPages {
				return nil, 0, 0, fmt.Errorf("list sources: exceeded %d pages; aborting scan", maxScrollPages)
			}
			pts, next, err := st.Scroll(ctx, filter, fields, 1000, offset)
			if err != nil {
				return nil, 0, 0, err
			}
			for _, p := range pts {
				uri := str(p.Payload["source_uri"])
				if opts.URIPrefix != "" && !strings.HasPrefix(uri, opts.URIPrefix) {
					continue
				}
				total++
				cur, ok := agg[uri]
				if !ok {
					cur = &SourceSummary{
						SourceURI:  uri,
						SourceType: str(p.Payload["source_type"]),
						Scope:      str(p.Payload["scope"]),
						Tags:       strSlice(p.Payload["tags"]),
						Writer:     str(p.Payload[fieldWriter]),
					}
					agg[uri] = cur
				}
				cur.Chunks++
				if t := unixTime(p.Payload[fieldStartedAt]); !t.IsZero() && (cur.StartedAt.IsZero() || t.Before(cur.StartedAt)) {
					cur.StartedAt = t
				}
				if t := unixTime(p.Payload[fieldEndedAt]); t.After(cur.EndedAt) {
					cur.EndedAt = t
				}
				if t := unixTime(p.Payload[fieldIngestedAt]); t.After(cur.IngestedAt) {
					cur.IngestedAt = t
				}
			}
			if next == nil {
				break
			}
			if offset != nil && fmt.Sprint(next) == fmt.Sprint(offset) {
				return nil, 0, 0, fmt.Errorf("list sources: scroll cursor did not advance")
			}
			offset = next
		}
	}

	out := make([]SourceSummary, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	// Chunk count descending, then URI, so the ordering is stable across calls.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chunks != out[j].Chunks {
			return out[i].Chunks > out[j].Chunks
		}
		return out[i].SourceURI < out[j].SourceURI
	})
	distinct := len(out)
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, distinct, total, nil
}

// DeleteSource removes every chunk belonging to one source URI, from every
// space's collection, returning how many were removed. A missing source is
// not an error — it deletes nothing.
func (s *Service) DeleteSource(ctx context.Context, uri string) (int, error) {
	if uri == "" {
		return 0, fmt.Errorf("source_uri is required")
	}
	defer s.lockSource(uri)()
	active, shadow := s.stores()
	// Reason: a forget must reach every space, so the list is read afresh
	// rather than trusted from the cache.
	stores, err := s.allStores(ctx, active, true)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, ss := range stores {
		sp, _ := spaceOf(active.Collection(), ss.st.Collection())
		key := sourceKey(sp, uri)
		n, err := deleteSource(ctx, ss.st, uri)
		if err != nil {
			if shadow != nil {
				s.markDirty(key)
			}
			return 0, err
		}
		total += n
		if shadow != nil {
			// The shadow may hold the source even when the active store does not.
			_, err := deleteSource(ctx, s.derive(shadow, sp), uri)
			s.shadowResult(key, err)
		}
	}
	return total, nil
}

// HasSource reports whether any chunk is stored under uri, in any space.
func (s *Service) HasSource(ctx context.Context, uri string) (bool, error) {
	if uri == "" {
		return false, nil
	}
	stores, err := s.allStores(ctx, s.active(), false)
	if err != nil {
		return false, err
	}
	for _, ss := range stores {
		n, err := ss.st.Count(ctx, EqFilter("source_uri", uri))
		if err != nil || n > 0 {
			return n > 0, err
		}
	}
	return false, nil
}

// pointID is a deterministic UUIDv5 so re-ingesting a source overwrites prior chunks.
func pointID(uri string, index int) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s#%d", uri, index))).String()
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// deleteSource removes a source's chunks from st, returning how many there were.
func deleteSource(ctx context.Context, st *QdrantStore, uri string) (int, error) {
	filter := EqFilter("source_uri", uri)
	n, err := st.Count(ctx, filter)
	if err != nil || n == 0 {
		return 0, err
	}
	return n, st.DeleteByFilter(ctx, filter)
}

// chunkSpan dates a chunk by the timeline markers in it. A chunk with none
// takes the time carried from the chunk before; the latest time seen is
// carried on.
func chunkSpan(text string, carried *time.Time) (start, end time.Time) {
	first, last, ok := timeline.Span(text)
	if !ok {
		return *carried, *carried
	}
	if !carried.IsZero() && first.After(*carried) && !strings.HasPrefix(strings.TrimLeft(text, " \n"), timeline.Marker(first)) {
		// The chunk opens mid-turn: that turn began at the carried time.
		first = *carried
	}
	*carried = last
	return first, last
}

// clamp bounds a chunk's span to [lo, hi]; a zero bound leaves that side open.
func clamp(start, end, lo, hi time.Time) (time.Time, time.Time) {
	limit := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		if !lo.IsZero() && t.Before(lo) {
			return lo
		}
		if !hi.IsZero() && t.After(hi) {
			return hi
		}
		return t
	}
	return limit(start), limit(end)
}

// setTime stores t in payload as Unix seconds, leaving unknown times out.
func setTime(payload map[string]any, key string, t time.Time) {
	if !t.IsZero() {
		payload[key] = t.Unix()
	}
}
