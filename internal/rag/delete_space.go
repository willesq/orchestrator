package rag

import (
	"context"
	"errors"
	"fmt"
)

// DeleteSourceIn removes the chunks of one source URI from space's
// collection only, returning how many were removed (D-77). A copy of the
// URI in another space, or in the base collection, is left alone, so the
// count is the same whether or not another space holds the URI. A space
// with no collection yet holds nothing.
func (s *Service) DeleteSourceIn(ctx context.Context, uri, space string) (int, error) {
	if uri == "" || space == "" {
		return 0, fmt.Errorf("source_uri and space are required")
	}
	defer s.lockSource(uri)()
	active, shadow := s.storeFor(space)
	key := sourceKey(space, uri)
	n, err := deleteSource(ctx, active, uri)
	switch {
	case errors.Is(err, errCollectionMissing):
		n, err = 0, nil
	case err != nil:
		if shadow != nil {
			s.markDirty(key)
		}
		return 0, err
	}
	if shadow != nil {
		// The shadow may hold the source even when the active store does not.
		_, serr := deleteSource(ctx, shadow, uri)
		if errors.Is(serr, errCollectionMissing) {
			serr = nil
		}
		s.shadowResult(key, serr)
	}
	return n, nil
}
