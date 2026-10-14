// Package rag implements retrieval-augmented storage: chunk text, embed it, and
// upsert/search vectors in Qdrant. The embedder is injected so the package stays
// independent of the ONNX runtime and testable in isolation.
package rag

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/willesq/orchestrator/internal/bm25"
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
	// reach, then swaps it in as the active store.
	mu     sync.RWMutex
	store  *QdrantStore
	shadow *QdrantStore
	// touched records sources a live write has fully written to the shadow
	// during a migration; the copy leaves those alone.
	touched map[string]bool
	// dirty records sources a write left inconsistent between the two stores
	// during a migration; the migration copies them again before it switches.
	dirty map[string]bool
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

	fallbackLog rateLog
	// progress is set while a migration copies sources.
	progress atomic.Pointer[MigrationProgress]
	// chunkTokens and overlap size chunks in model tokens.
	chunkTokens int
	overlap     int
	// MarkdownSections chunks markdown by heading section instead of by word
	// window (embedder.markdown_sections).
	MarkdownSections bool
}

// NewService constructs a RAG service. chunkTokens and overlap are in model
// tokens; chunkTokens is capped at what the model embeds.
func NewService(emb Embedder, store *QdrantStore, chunkTokens, overlap int) *Service {
	return &Service{emb: emb, store: store, chunkTokens: chunkTokens, overlap: overlap, MarkdownSections: true,
		BM25: bm25.DefaultParams, SearchMode: ModeHybrid, RRFK: 60, KeywordWeight: 0.5}
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
// switches.
func (s *Service) shadowResult(uri string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.touched == nil {
		return
	}
	if err != nil {
		delete(s.touched, uri)
		s.dirty[uri] = true
		log.Printf("rag: %s reached the active store but not the migration target (%v); it is copied again before the switch", uri, err)
		return
	}
	s.touched[uri] = true
	delete(s.dirty, uri)
}

// markDirty records that a write left a source possibly inconsistent between
// the stores (it failed part-way on the active one) during a migration.
func (s *Service) markDirty(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty != nil {
		delete(s.touched, uri)
		s.dirty[uri] = true
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
func (s *Service) IngestText(ctx context.Context, text string, m Meta) (int, error) {
	points, dim, err := s.prepare(ctx, text, m)
	if err != nil || len(points) == 0 {
		return 0, err
	}
	defer s.lockSource(m.SourceURI)()
	active, shadow := s.stores()
	if err := writePoints(ctx, active, dim, m.SourceURI, points); err != nil {
		if shadow != nil {
			s.markDirty(m.SourceURI)
		}
		return 0, err
	}
	if shadow != nil {
		s.shadowResult(m.SourceURI, writePoints(ctx, shadow, dim, m.SourceURI, points))
	}
	return len(points), nil
}

// prepare chunks, embeds and weights text into points, without writing them.
func (s *Service) prepare(ctx context.Context, text string, m Meta) ([]Point, int, error) {
	// Required here, not just in the callers: the prune below deletes by
	// source_uri, so an empty one would match every URI-less point in the store
	// and destroy it. The layer that performs a delete validates its own input.
	if m.SourceURI == "" {
		return nil, 0, fmt.Errorf("source_uri is required")
	}
	pieces := s.split(text)
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
	for i, c := range chunks {
		payload := map[string]any{
			"text":        c,
			"source_uri":  m.SourceURI,
			"source_type": m.SourceType,
			"scope":       m.Scope,
			"tags":        m.Tags,
			"chunk_index": i,
			"chunk_total": len(chunks),
		}
		if h := pieces[i].heading; h != "" {
			payload["heading"] = h
		}
		points[i] = Point{ID: pointID(m.SourceURI, i), Vector: vecs[i], Sparse: bm25.Document(c, s.BM25), Payload: payload}
	}
	return points, dim, nil
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
}

// ListSourcesOpts filters the inventory.
type ListSourcesOpts struct {
	SourceType string
	Scope      string
	URIPrefix  string
	Limit      int
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
	filter := buildFilter(must, nil)
	fields := []string{"source_uri", "source_type", "scope", "tags"}

	agg := map[string]*SourceSummary{}
	total := 0
	var offset any
	// A server that returns a non-advancing cursor would otherwise spin forever:
	// the per-request timeout bounds each call but not the loop.
	for page := 0; ; page++ {
		if page >= maxScrollPages {
			return nil, 0, 0, fmt.Errorf("list sources: exceeded %d pages; aborting scan", maxScrollPages)
		}
		pts, next, err := s.active().Scroll(ctx, filter, fields, 1000, offset)
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
				}
				agg[uri] = cur
			}
			cur.Chunks++
		}
		if next == nil {
			break
		}
		if offset != nil && fmt.Sprint(next) == fmt.Sprint(offset) {
			return nil, 0, 0, fmt.Errorf("list sources: scroll cursor did not advance")
		}
		offset = next
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

// DeleteSource removes every chunk belonging to one source URI, returning how
// many were removed. A missing source is not an error — it deletes nothing.
func (s *Service) DeleteSource(ctx context.Context, uri string) (int, error) {
	if uri == "" {
		return 0, fmt.Errorf("source_uri is required")
	}
	defer s.lockSource(uri)()
	active, shadow := s.stores()
	n, err := deleteSource(ctx, active, uri)
	if err != nil {
		if shadow != nil {
			s.markDirty(uri)
		}
		return 0, err
	}
	if shadow != nil {
		// The shadow may hold the source even when the active store does not.
		_, err := deleteSource(ctx, shadow, uri)
		s.shadowResult(uri, err)
	}
	return n, nil
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
