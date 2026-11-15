package cli

import (
	"context"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// lastDMSearch is the request a fakeClient's SearchDirectMessages last
// received.
var lastDMSearch *orchdv1.SearchDirectMessagesRequest

// SearchDirectMessages records the request and answers as Search does.
func (f *fakeClient) SearchDirectMessages(_ context.Context, req *orchdv1.SearchDirectMessagesRequest, _ ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	f.searchMu.Lock()
	defer f.searchMu.Unlock()
	lastDMSearch = req
	return f.search, f.err
}
