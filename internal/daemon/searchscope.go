package daemon

import (
	"context"
	"log"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// searchCollector names the collections a search reads (*rag.Service).
type searchCollector interface {
	SearchCollections(ctx context.Context, spaces []string) ([]string, error)
}

// describeSearch adds to out what scope let ctx's caller search (SP-031), so
// 'orch rag why' can show the boundary: the caller's space, whether the
// search was limited to its readable spaces, and the collections read. A
// filtered caller's collections are only its readable spaces' own, which it
// may know of; the list is left out when it cannot be read.
//
// An unscoped search under a policy (memory filtering switched off) lists
// collections only for Orch.app: every collection's name holds a space's,
// and space names are what spaces keep apart.
func (s *server) describeSearch(ctx context.Context, scope readScope, out *orchdv1.SearchResponse) {
	c := callerOf(ctx)
	out.CallerSpace, out.Scoped = callerSpace(c), scope.filtered()
	sc, ok := s.rag.(searchCollector)
	if !ok || sc == nil {
		return
	}
	if !scope.filtered() && !(c.kind == callerOperator && c.operatorBy != "") && s.spacesConfigured() {
		return
	}
	cols, err := sc.SearchCollections(ctx, scope.spaces)
	if err != nil {
		log.Printf("orchd: Search: naming the collections searched: %v", err)
		return
	}
	out.Collections = cols
}

// callerSpace is the space c's memory reads come from: its session's pin or
// its queued task's recorded space; "" for any other caller.
func callerSpace(c resolvedCaller) string {
	switch {
	case c.kind == callerSession:
		return c.pin.Space
	case c.kind == callerTask && c.task.TaskID != "":
		return c.task.Space
	}
	return ""
}
