package daemon

import (
	"context"
	"errors"
	"log"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Browsing memory: the collections a caller reads, and every chunk matching
// a filter page by page, for clients that list rather than rank (Orch.app's
// Memory view, orch rag collections and orch rag browse). Both read through
// the caller's readable spaces exactly as Search does (SP-025).

// memoryBrowser lists collections and pages through chunks (implemented by
// *rag.Service).
type memoryBrowser interface {
	ListCollections(ctx context.Context, opts rag.ListCollectionsOpts) ([]rag.CollectionInfo, error)
	Browse(ctx context.Context, opts rag.BrowseOpts) ([]rag.Hit, string, int64, error)
}

// maxBrowseLimit bounds one browse page.
const maxBrowseLimit = 200

// errBrowseUnavailable answers when the memory store cannot browse.
var errBrowseUnavailable = grpcstatus.Error(codes.Unavailable, "browsing memory is unavailable")

// backupCollection is the collection a hybrid migration replaced, kept for
// rollback; "" when none is recorded or config cannot be read. A variable
// so tests need not read config.
var backupCollection = func() string {
	v, err := config.Load()
	if err != nil {
		log.Printf("orchd: ListCollections: reading config: %v", err)
		return ""
	}
	return v.GetString("rag.qdrant.backup_collection")
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_ListCollections_FullMethodName,
	spaceDecision{spaceFiltered, "memory collections, listed only from the caller's readable spaces"}))

// ListCollections names the collections the caller reads, with their chunk
// counts. The migration backup is never listed: no read reaches it.
func (s *server) ListCollections(ctx context.Context, _ *orchdv1.ListCollectionsRequest) (*orchdv1.ListCollectionsResponse, error) {
	b, ok := s.rag.(memoryBrowser)
	if !ok {
		return nil, errBrowseUnavailable
	}
	scope := s.readScope(ctx, orchdv1.Orchd_ListCollections_FullMethodName)
	cols, err := b.ListCollections(ctx, rag.ListCollectionsOpts{
		Spaces:  scope.spaces,
		LabelsV: scope.labelsV,
		Hide:    []string{backupCollection()},
	})
	if err != nil {
		return nil, scope.storeError("ListCollections", err)
	}
	// Reason (SP-083): as for a hit, a space is named only while a policy
	// gives spaces a meaning.
	placed := s.spacesConfigured()
	out := &orchdv1.ListCollectionsResponse{
		Collections: make([]*orchdv1.CollectionInfo, 0, len(cols)),
		SpacesOn:    s.spacesSwitch != nil && !s.spacesSwitch.off(),
	}
	for _, c := range cols {
		space := c.Space
		if !placed {
			space = ""
		}
		out.Collections = append(out.Collections, &orchdv1.CollectionInfo{
			Name: c.Name, Space: space, Chunks: c.Chunks, Hybrid: c.Hybrid,
		})
	}
	return out, nil
}

var _ = registerRPCs(taskRPC(orchdv1.Orchd_BrowseChunks_FullMethodName,
	spaceDecision{spaceFiltered, "memory listing, read only from the caller's readable spaces' collections, chat DMs left out"}))

// BrowseChunks returns one page of the chunks matching the request's
// filters, unranked, and the cursor continuing it.
func (s *server) BrowseChunks(ctx context.Context, req *orchdv1.BrowseChunksRequest) (*orchdv1.BrowseChunksResponse, error) {
	b, ok := s.rag.(memoryBrowser)
	if !ok {
		return nil, errBrowseUnavailable
	}
	if n := req.GetLimit(); n < 0 || n > maxBrowseLimit {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "limit %d must be between 0 and %d", n, maxBrowseLimit)
	}
	if err := checkTags(req.GetTags(), req.GetAllTags()); err != nil {
		return nil, err
	}
	if err := checkCollections(req.GetCollections()); err != nil {
		return nil, err
	}
	if len(req.GetCursor()) > maxBrowseCursorLen {
		return nil, grpcstatus.Error(codes.InvalidArgument, rag.ErrBadCursor.Error())
	}
	tr, err := timeRange(req.GetSince(), req.GetUntil())
	if err != nil {
		return nil, err
	}
	// The caller's spaces choose what is read; the request only narrows
	// within them (SP-025, INV-02). Direct messages are never browsed
	// (SP-060, D-19): no request field can keep them in.
	scope := s.readScope(ctx, orchdv1.Orchd_BrowseChunks_FullMethodName)
	hits, next, total, err := b.Browse(ctx, rag.BrowseOpts{
		Spaces:      scope.spaces,
		LabelsV:     scope.labelsV,
		Collections: req.GetCollections(),
		Scope:       req.GetScope(),
		SourceType:  req.GetSourceType(),
		SourceURI:   req.GetSourceUri(),
		Tags:        req.GetTags(),
		AllTags:     req.GetAllTags(),
		Range:       tr,
		Limit:       int(req.GetLimit()),
		Cursor:      req.GetCursor(),
	})
	if errors.Is(err, rag.ErrBadCursor) {
		return nil, grpcstatus.Error(codes.InvalidArgument, rag.ErrBadCursor.Error())
	}
	if err != nil {
		return nil, scope.storeError("BrowseChunks", err)
	}
	out := &orchdv1.BrowseChunksResponse{NextCursor: next, Total: total}
	out.Hits = s.searchHits(hits)
	return out, nil
}
