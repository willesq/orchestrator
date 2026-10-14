// Package rag implements retrieval-augmented storage: chunk text, embed it, and
// upsert/search vectors in Qdrant. The embedder is injected so the package stays
// independent of the ONNX runtime and testable in isolation.
package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
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
	emb   Embedder
	store *QdrantStore
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
	return &Service{emb: emb, store: store, chunkTokens: chunkTokens, overlap: overlap, MarkdownSections: true}
}

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
	// Required here, not just in the callers: the prune below deletes by
	// source_uri, so an empty one would match every URI-less point in the store
	// and destroy it. The layer that performs a delete validates its own input.
	if m.SourceURI == "" {
		return 0, fmt.Errorf("source_uri is required")
	}
	pieces := s.split(text)
	if len(pieces) == 0 {
		return 0, nil
	}
	chunks := make([]string, len(pieces))
	for i, p := range pieces {
		chunks[i] = p.text
	}
	vecs, err := s.emb.Embed(ctx, chunks)
	if err != nil {
		return 0, err
	}
	dim, err := s.emb.Dimension(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.store.EnsureCollection(ctx, dim); err != nil {
		return 0, err
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
		points[i] = Point{ID: pointID(m.SourceURI, i), Vector: vecs[i], Payload: payload}
	}
	if err := s.store.Upsert(ctx, points); err != nil {
		return 0, err
	}
	// Re-ingesting a source that shrank would otherwise leave the previous
	// version's surplus chunks behind: point IDs are uri#index, so indices past
	// the new length are never overwritten and would keep matching searches.
	if err := s.pruneChunksFrom(ctx, m.SourceURI, len(chunks)); err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// pruneChunksFrom deletes chunks of a source at index >= from.
func (s *Service) pruneChunksFrom(ctx context.Context, uri string, from int) error {
	filter := map[string]any{"must": []any{
		map[string]any{"key": "source_uri", "match": map[string]any{"value": uri}},
		map[string]any{"key": "chunk_index", "range": map[string]any{"gte": from}},
	}}
	n, err := s.store.Count(ctx, filter)
	if err != nil || n == 0 {
		return err
	}
	return s.store.DeleteByFilter(ctx, filter)
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
		pts, next, err := s.store.Scroll(ctx, filter, fields, 1000, offset)
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
	filter := EqFilter("source_uri", uri)
	n, err := s.store.Count(ctx, filter)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.store.DeleteByFilter(ctx, filter); err != nil {
		return 0, err
	}
	return n, nil
}

// Hit is a search result surfaced to callers.
type Hit struct {
	Text       string
	SourceURI  string
	SourceType string
	Scope      string
	Tags       []string
	Score      float32
}

// SearchOpts filters a search.
type SearchOpts struct {
	TopK       int
	Scope      string
	SourceType string
	Tags       []string
}

// Search embeds the query and returns the top matching chunks.
func (s *Service) Search(ctx context.Context, query string, opts SearchOpts) ([]Hit, error) {
	if opts.TopK <= 0 {
		opts.TopK = 5
	}
	vecs, err := s.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	must := map[string]string{}
	if opts.Scope != "" {
		must["scope"] = opts.Scope
	}
	if opts.SourceType != "" {
		must["source_type"] = opts.SourceType
	}
	raw, err := s.store.Search(ctx, vecs[0], opts.TopK, must, opts.Tags)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(raw))
	for _, r := range raw {
		hits = append(hits, Hit{
			Text:       str(r.Payload["text"]),
			SourceURI:  str(r.Payload["source_uri"]),
			SourceType: str(r.Payload["source_type"]),
			Scope:      str(r.Payload["scope"]),
			Tags:       strSlice(r.Payload["tags"]),
			Score:      r.Score,
		})
	}
	return hits, nil
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
