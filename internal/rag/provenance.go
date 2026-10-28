package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/willesq/orchestrator/internal/bm25"
)

// Writers: which part of orchd stored a source. A writer is set by orchd's
// own code, never by a client, so what it implies cannot be claimed by
// whoever chose a source's type or URI.
const (
	// WriterDocs is the GitHub docs sync (registered owners' default branches).
	WriterDocs = "docs"
	// WriterSources is the sources refresh (registered web, repo and owner sources).
	WriterSources = "sources"
	// WriterSession is orchd's own session storage: transcripts, records and
	// summaries it reads from disk.
	WriterSession = "session"
	// WriterFile is the edit hand-off, storing a document an agent edited.
	WriterFile = "file"
	// WriterClient is anything stored through the Ingest RPC (the CLI, hooks,
	// MCP rag_ingest_text), whatever type or URI it declares.
	WriterClient = "client"
)

// Trust tiers stored with each chunk (payload "trust"), from its writer.
const (
	TrustRepo    = "repo"    // a registered repo's docs, from a syncer
	TrustWeb     = "web"     // a registered web page, fetched
	TrustSession = "session" // the user's sessions: they carry tool and web text
	TrustFile    = "file"    // a file an agent edited
	TrustClient  = "client"  // stored by a client
)

// Payload fields of provenance.
const (
	fieldWriter      = "writer"
	fieldTrust       = "trust"
	fieldContentHash = "content_hash"
	fieldIngestHash  = "ingest_hash"
)

// TrustFor is the tier a writer's content gets; "" for an unknown writer,
// which readers treat as unverified.
func TrustFor(writer, sourceType string) string {
	switch writer {
	case WriterDocs:
		return TrustRepo
	case WriterSources:
		if sourceType == "web" {
			return TrustWeb
		}
		return TrustRepo
	case WriterSession:
		return TrustSession
	case WriterFile:
		return TrustFile
	case WriterClient:
		return TrustClient
	}
	return ""
}

// ContentHash identifies a source's text as given to IngestText, so two
// writers storing the same text agree.
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

// ingestHash identifies a whole write: text and everything stored with it. A
// write whose hash matches what is stored changes nothing and is skipped, so
// an unchanged page or doc keeps the date it was first stored.
//
// The chunker's settings and the keyword tokenizer are part of it, so a
// change to either re-chunks or re-indexes a source at its next write.
func (s *Service) ingestHash(text string, m Meta) string {
	tags := append([]string(nil), m.Tags...)
	sort.Strings(tags)
	key := strings.Join([]string{text, m.SourceType, m.Scope, strings.Join(tags, "\x01"), m.Writer,
		fmt.Sprint(m.StartedAt.Unix(), m.EndedAt.Unix(), m.Timeline, m.KeepUndated),
		fmt.Sprint(s.chunkTokens, s.overlap, s.MarkdownSections, bm25.TokenizerVersion, s.BM25)}, "\x00")
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// StoredCopy is what the store holds for one source, read from its first chunk.
type StoredCopy struct {
	Writer, Trust, ContentHash string
}

// Copy returns the provenance of the source stored under uri; ok is false
// when nothing is stored there.
func (s *Service) Copy(ctx context.Context, uri string) (StoredCopy, bool, error) {
	if uri == "" {
		return StoredCopy{}, false, nil
	}
	pts, err := s.active().Retrieve(ctx, []string{pointID(uri, 0)}, []string{fieldWriter, fieldTrust, fieldContentHash, "source_uri"})
	if err != nil || len(pts) == 0 {
		return StoredCopy{}, false, err
	}
	p := pts[0].Payload
	if str(p["source_uri"]) != uri {
		return StoredCopy{}, false, nil
	}
	return StoredCopy{Writer: str(p[fieldWriter]), Trust: str(p[fieldTrust]), ContentHash: str(p[fieldContentHash])}, true, nil
}

// ensureWriterIndex adds the writer payload index until one attempt succeeds.
func (s *Service) ensureWriterIndex(ctx context.Context) {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	if s.writerIndexed {
		return
	}
	if err := s.active().EnsureIndex(ctx, fieldWriter); err != nil {
		log.Printf("rag: indexing %s: %v", fieldWriter, err)
		return
	}
	s.writerIndexed = true
}

// SourcesWrittenBy lists the source URIs under prefix that writer stored.
// The writer is matched in the store, so the scan returns only its chunks.
func (s *Service) SourcesWrittenBy(ctx context.Context, prefix, writer string) ([]string, error) {
	if writer == "" {
		return nil, fmt.Errorf("writer is required")
	}
	// Collections made before writers were recorded lack the writer index;
	// it is added once per process, so the filter is served from it. A failed
	// attempt is retried on the next call rather than remembered.
	s.ensureWriterIndex(ctx)
	filter := EqFilter(fieldWriter, writer)
	seen := map[string]bool{}
	var out []string
	var offset any
	for page := 0; page < maxScrollPages; page++ {
		pts, next, err := s.active().Scroll(ctx, filter, []string{"source_uri"}, 1000, offset)
		if err != nil {
			return nil, err
		}
		for _, p := range pts {
			uri := str(p.Payload["source_uri"])
			if strings.HasPrefix(uri, prefix) && !seen[uri] {
				seen[uri] = true
				out = append(out, uri)
			}
		}
		if next == nil {
			sort.Strings(out)
			return out, nil
		}
		offset = next
	}
	return nil, fmt.Errorf("sources written by %s: exceeded %d pages", writer, maxScrollPages)
}

// unchanged reports whether the source already holds exactly this write,
// whole: its first and last chunks carry the write's hash, and it holds
// exactly the chunk count the write recorded. A write that failed part-way
// (a later batch, or the prune of an older version's surplus chunks) fails
// that check, so the next write of the same text is done again rather than
// skipped.
func (s *Service) unchanged(ctx context.Context, st *QdrantStore, uri, hash string) (int, bool) {
	pts, err := st.Retrieve(ctx, []string{pointID(uri, 0)}, []string{fieldIngestHash, "chunk_total", "source_uri"})
	if err != nil || len(pts) == 0 {
		return 0, false
	}
	p := pts[0].Payload
	t, _ := p["chunk_total"].(float64)
	total := int(t)
	if str(p["source_uri"]) != uri || str(p[fieldIngestHash]) != hash || total < 1 {
		return 0, false
	}
	if total > 1 {
		last, err := st.Retrieve(ctx, []string{pointID(uri, total-1)}, []string{fieldIngestHash, "source_uri"})
		if err != nil || len(last) == 0 || str(last[0].Payload[fieldIngestHash]) != hash || str(last[0].Payload["source_uri"]) != uri {
			return 0, false
		}
	}
	if n, err := st.Count(ctx, EqFilter("source_uri", uri)); err != nil || n != total {
		return 0, false
	}
	return total, true
}
