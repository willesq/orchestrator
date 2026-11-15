package daemon

import (
	"context"
	"log"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// SearchDirectMessages is Search with chat direct messages kept in (SP-060):
// the explicit search through which the user finds DMs. Every other read,
// auto-RAG's Search included, leaves them out. It is the operator's alone
// (refuseNonOperator: Orch.app under a policy, with none a terminal outside
// any agent session), so neither a queued task (D-19) nor an agent in an
// interactive session can pull DMs into its context, where text recalled
// from a chat could ask it to. The caller's readable spaces still decide
// what is searched (SP-025).
func (s *server) SearchDirectMessages(ctx context.Context, req *orchdv1.SearchDirectMessagesRequest) (*orchdv1.SearchResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_SearchDirectMessages_FullMethodName, "search direct messages"); err != nil {
		return nil, err
	}
	// Reason: refuseNonOperator judges the connection; a caller orchd
	// resolved as a task or session is refused here too, whatever its
	// connection shows.
	if c := callerOf(ctx); c.kind == callerTask || c.kind == callerSession {
		log.Printf("orchd: refused SearchDirectMessages from %s", readerLabel(c))
		return nil, grpcstatus.Error(codes.PermissionDenied, "only the operator can search direct messages")
	}
	if req.GetSearch() == nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, "name the search")
	}
	return s.search(ctx, req.GetSearch(), orchdv1.Orchd_SearchDirectMessages_FullMethodName, true)
}
