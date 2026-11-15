package mcpserver

import (
	"context"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func (f *fakeClient) SearchDirectMessages(context.Context, *orchdv1.SearchDirectMessagesRequest, ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	return &orchdv1.SearchResponse{}, nil
}
