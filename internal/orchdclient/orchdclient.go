// Package orchdclient dials the long-lived orchd daemon over its Unix-domain
// socket. Shared by the CLI, hooks, and MCP servers so the transport is defined once.
package orchdclient

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Dial returns a lazy orchd client plus a cleanup func. The connection is not
// established until the first RPC, so callers that can't reach orchd fail on use,
// not on dial.
func Dial() (orchdv1.OrchdClient, func(), error) {
	conn, err := grpc.NewClient(
		"unix://"+paths.SocketPath(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, err
	}
	return orchdv1.NewOrchdClient(conn), func() { _ = conn.Close() }, nil
}
