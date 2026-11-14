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

// Document rebuilds a source from its chunks: ordered by chunk index,
// without their section context lines, with the overlap between neighbouring
// chunks trimmed. It is read from the first collection holding it, the base
// first and then each space's.
func (s *Service) Document(ctx context.Context, uri string) (Document, error) {
	stores, err := s.allStores(ctx, s.active(), false)
	if err != nil {
		return Document{}, err
	}
	for _, ss := range stores {
		sr, err := s.readSource(ctx, ss.st, uri)
		if err != nil {
			return Document{}, err
		}
		if sr.chunks > 0 {
			return Document{Meta: sr.meta, Text: sr.text, Chunks: sr.chunks, Malformed: sr.malformed, Oversize: sr.oversize}, nil
		}
	}
	return Document{}, ErrSourceNotFound
}
