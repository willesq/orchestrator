package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
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
func ingestHash(text string, m Meta) string {
	tags := append([]string(nil), m.Tags...)
	sort.Strings(tags)
	key := strings.Join([]string{text, m.SourceType, m.Scope, strings.Join(tags, "\x01"), m.Writer,
		fmt.Sprint(m.StartedAt.Unix(), m.EndedAt.Unix(), m.Timeline, m.KeepUndated)}, "\x00")
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

// SourcesWrittenBy lists the source URIs under prefix that writer stored.
// The writer is matched in the store, so the scan returns only its chunks.
func (s *Service) SourcesWrittenBy(ctx context.Context, prefix, writer string) ([]string, error) {
	if writer == "" {
		return nil, fmt.Errorf("writer is required")
	}
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

// unchanged reports whether the source already holds exactly this write.
func (s *Service) unchanged(ctx context.Context, st *QdrantStore, uri, hash string) (int, bool) {
	pts, err := st.Retrieve(ctx, []string{pointID(uri, 0)}, []string{fieldIngestHash, "chunk_total", "source_uri"})
	if err != nil || len(pts) == 0 {
		return 0, false
	}
	p := pts[0].Payload
	total, _ := p["chunk_total"].(float64)
	if str(p["source_uri"]) != uri || str(p[fieldIngestHash]) != hash || total < 1 {
		return 0, false
	}
	return int(total), true
}
