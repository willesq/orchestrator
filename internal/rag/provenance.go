package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/bm25"
	"github.com/willesq/orchestrator/internal/logs"
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
	case WriterChat:
		return TrustChat
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
	// Reason: a write placed elsewhere, or under another revision, is not
	// the write that is stored, so it is not skipped. Unplaced writes keep
	// the hash they had before placement existed.
	if m.Space != "" || m.Origin.System != "" {
		key += "\x00" + strings.Join([]string{m.Origin.key(), m.Space, fmt.Sprint(m.LabelsV)}, "\x00")
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// StoredCopy is what the store holds for one source, read from its first chunk.
type StoredCopy struct {
	Writer, Trust, ContentHash string
	// Space is the space the source was placed in; "" when it was stored
	// unplaced (before SP-023, or without a placer).
	Space string
}

// Copy returns the provenance of the source stored under uri; ok is false
// when nothing is stored there. A source lives in one space, but should it be
// found in several collections (a move part done), the copy that refuses most
// is returned: a sync writer's first, then one placed in a space, then the
// base's. Callers decide from it whether a write may replace the source.
func (s *Service) Copy(ctx context.Context, uri string) (StoredCopy, bool, error) {
	if uri == "" {
		return StoredCopy{}, false, nil
	}
	stores, err := s.allStores(ctx, s.active(), false)
	if err != nil {
		return StoredCopy{}, false, err
	}
	var best StoredCopy
	found, rank := false, -1
	for _, ss := range stores {
		pts, err := ss.st.Retrieve(ctx, []string{pointID(uri, 0)}, []string{fieldWriter, fieldTrust, fieldContentHash, fieldSpace, "source_uri"})
		if errors.Is(err, errCollectionMissing) && len(stores) > 1 {
			// A collection not made yet holds nothing; a lone one keeps the
			// error it gave before spaces.
			continue
		}
		if err != nil {
			return StoredCopy{}, false, err
		}
		if len(pts) == 0 || str(pts[0].Payload["source_uri"]) != uri {
			continue
		}
		p := pts[0].Payload
		c := StoredCopy{Writer: str(p[fieldWriter]), Trust: str(p[fieldTrust]), ContentHash: str(p[fieldContentHash]), Space: str(p[fieldSpace])}
		r := 0
		switch {
		case IsSyncWriter(c.Writer):
			r = 2
		case c.Space != "":
			r = 1
		}
		if found {
			log.Printf("rag: %s is stored in more than one collection", logs.Clean(uri))
		}
		if r > rank {
			best, rank = c, r
		}
		found = true
	}
	return best, found, nil
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

// searchIndexTimeout bounds asking for the filter indexes; Qdrant builds them after.
const searchIndexTimeout = 30 * time.Second

// ensureSearchIndexes adds the payload indexes filtered searches need, once per
// process. Without one Qdrant reads every chunk's payload: on a 40,000-chunk store
// a search for one project's tags took 740 ms against 35 ms unfiltered, on every
// prompt. Building one takes seconds on a store that size, longer than a prompt
// waits, so it is only asked for while searches carry on unindexed. Adding an index
// changes no stored data and repeats harmlessly; a failure is retried next time.
func (s *Service) ensureSearchIndexes() {
	if !s.searchIndexed.CompareAndSwap(false, true) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), searchIndexTimeout)
		defer cancel()
		for _, field := range searchIndexFields {
			if err := s.active().startIndex(ctx, field); err != nil {
				// A store with nothing in it yet gets them with its collection.
				if !errors.Is(err, errCollectionMissing) {
					log.Printf("rag: indexing %s: %v", field, err)
				}
				s.searchIndexed.Store(false)
				return
			}
		}
	}()
}

// searchIndexFields are the payload fields filtered searches read.
var searchIndexFields = []string{"tags", "source_type"}

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
	// Space collections are made with the writer index, so only the base
	// may lack it.
	stores, err := s.allStores(ctx, s.active(), false)
	if err != nil {
		return nil, err
	}
	for _, ss := range stores {
		if err := scrollURIs(ctx, ss.st, filter, func(uri string) {
			if strings.HasPrefix(uri, prefix) && !seen[uri] {
				seen[uri] = true
				out = append(out, uri)
			}
		}); err != nil {
			return nil, fmt.Errorf("sources written by %s: %w", writer, err)
		}
	}
	sort.Strings(out)
	return out, nil
}

// scrollURIs calls each with the source URI of every chunk in st that
// matches filter.
func scrollURIs(ctx context.Context, st *QdrantStore, filter map[string]any, each func(string)) error {
	var offset any
	for page := 0; page < maxScrollPages; page++ {
		pts, next, err := st.Scroll(ctx, filter, []string{"source_uri"}, 1000, offset)
		if err != nil {
			return err
		}
		for _, p := range pts {
			each(str(p.Payload["source_uri"]))
		}
		if next == nil {
			return nil
		}
		offset = next
	}
	return fmt.Errorf("exceeded %d pages", maxScrollPages)
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
