package rag

import (
	"context"
	"errors"
)

// ErrSourceNotFound means no chunks are stored under a source URI.
var ErrSourceNotFound = errors.New("source not found")

// Document is one source rebuilt from its stored chunks: what retrieval sees,
// close to but not exactly the original text.
type Document struct {
	Meta   Meta
	Text   string
	Chunks int
	// Malformed means chunk indices were missing or repeated, so the order
	// may be off and overlap was not trimmed.
	Malformed bool
	// Oversize means the text passed the size limit and is not returned.
	Oversize bool
}

// Document rebuilds a source from the active collection's chunks: ordered by
// chunk index, without their section context lines, with the overlap between
// neighbouring chunks trimmed.
func (s *Service) Document(ctx context.Context, uri string) (Document, error) {
	sr, err := s.readSource(ctx, s.active(), uri)
	if err != nil {
		return Document{}, err
	}
	if sr.chunks == 0 {
		return Document{}, ErrSourceNotFound
	}
	return Document{Meta: sr.meta, Text: sr.text, Chunks: sr.chunks, Malformed: sr.malformed, Oversize: sr.oversize}, nil
}
