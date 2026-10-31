package mcpserver

import (
	"context"
	"errors"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// RetryTask and CancelTask complete the client interface; these tests never
// call them.
func (f *fakeClient) RetryTask(context.Context, *orchdv1.RetryTaskRequest, ...grpc.CallOption) (*orchdv1.RetryTaskResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeClient) CancelTask(context.Context, *orchdv1.CancelTaskRequest, ...grpc.CallOption) (*orchdv1.CancelTaskResponse, error) {
	return nil, errors.New("not used")
}

// WatchTasks completes the client interface; these tests never stream.
func (f *fakeClient) WatchTasks(context.Context, *orchdv1.WatchTasksRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[orchdv1.TaskEvent], error) {
	return nil, errors.New("not used")
}
