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
	return s.DocumentIn(ctx, uri, nil, 0)
}

// DocumentIn is Document read only from the given spaces, as
// SearchOpts.Spaces chooses them (SP-025): nil reads every collection, and
// a space's collection is read only where the payload names that space and,
// when labelsV is set, that revision (SearchOpts.LabelsV). A source held
// only elsewhere is ErrSourceNotFound, as if it did not exist.
func (s *Service) DocumentIn(ctx context.Context, uri string, spaces []string, labelsV int64) (Document, error) {
	stores, err := s.listStores(ctx, spaces)
	if err != nil {
		return Document{}, err
	}
	stores = withLabels(stores, labelsV)
	for _, ss := range stores {
		sr, err := s.readSourceWhere(ctx, ss.st, uri, ss.must(map[string]string{"source_uri": uri}))
		if err != nil {
			return Document{}, err
		}
		if sr.chunks > 0 {
			return Document{Meta: sr.meta, Text: sr.text, Chunks: sr.chunks, Malformed: sr.malformed, Oversize: sr.oversize}, nil
		}
	}
	return Document{}, ErrSourceNotFound
}
