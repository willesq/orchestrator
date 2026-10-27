// Package orchdclient dials the long-lived orchd daemon over its Unix-domain
// socket. Shared by the CLI, hooks, and MCP servers so the transport is defined once.
package orchdclient

import (
	"context"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TaskMetadataKey carries the queued task a client runs in. orchd refuses
// operator-only calls that carry it. It is a courtesy that names the task:
// orchd also identifies task processes itself, since a session can unset the
// environment variable this comes from.
const TaskMetadataKey = "orch-task-id"

// Dial returns a lazy orchd client plus a cleanup func. The connection is not
// established until the first RPC, so callers that can't reach orchd fail on use,
// not on dial.
func Dial() (orchdv1.OrchdClient, func(), error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if id := os.Getenv(agentrun.TaskIDEnv); id != "" {
		opts = append(opts, grpc.WithUnaryInterceptor(withTaskID(id)))
	}
	conn, err := grpc.NewClient("unix://"+paths.SocketPath(), opts...)
	if err != nil {
		return nil, nil, err
	}
	return orchdv1.NewOrchdClient(conn), func() { _ = conn.Close() }, nil
}

// withTaskID tags every call with the task the client runs in.
func withTaskID(id string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, TaskMetadataKey, id)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
