// Package rag implements retrieval-augmented storage: chunk text, embed it, and
// upsert/search vectors in Qdrant. The embedder is injected so the package stays
// independent of the ONNX runtime and testable in isolation.
package rag

import (
	"context"
	"fmt"

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
	emb       Embedder
	store     *QdrantStore
	chunkSize int
	overlap   int
}

// NewService constructs a RAG service.
func NewService(emb Embedder, store *QdrantStore, chunkSize, overlap int) *Service {
	return &Service{emb: emb, store: store, chunkSize: chunkSize, overlap: overlap}
}

// IngestText chunks, embeds, and upserts text. Chunks for the same source_uri
// are replaced via deterministic point IDs (uri#index).
func (s *Service) IngestText(ctx context.Context, text string, m Meta) (int, error) {
	chunks := chunkText(text, s.chunkSize, s.overlap)
	if len(chunks) == 0 {
		return 0, nil
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
		points[i] = Point{
			ID:     pointID(m.SourceURI, i),
			Vector: vecs[i],
			Payload: map[string]any{
				"text":        c,
				"source_uri":  m.SourceURI,
				"source_type": m.SourceType,
				"scope":       m.Scope,
				"tags":        m.Tags,
				"chunk_index": i,
				"chunk_total": len(chunks),
			},
		}
	}
	if err := s.store.Upsert(ctx, points); err != nil {
		return 0, err
	}
	return len(chunks), nil
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
