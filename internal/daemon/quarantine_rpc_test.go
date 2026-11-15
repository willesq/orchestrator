package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// seedIn stores chunks of uri in collection, as a placed write left them.
func (q *memQdrant) seedIn(collection, uri string, chunks int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.collections[collection] == nil {
		q.collections[collection] = map[string]map[string]any{}
		q.vectors[collection] = map[string]any{}
	}
	for i := 0; i < chunks; i++ {
		id := fmt.Sprintf("%s#%d", uri, i)
		q.collections[collection][id] = map[string]any{"source_uri": uri, "source_type": "note",
			"chunk_index": float64(i), "chunk_total": float64(chunks), "text": "t"}
		q.vectors[collection][id] = map[string]any{"dense": []any{1.0, 0.0}}
	}
}

// quarantineEnv is the plan's example policy over a memQdrant whose
// quarantine holds two sources (3 chunks) and whose acme space holds one.
func quarantineEnv(t *testing.T) writesEnv {
	t.Helper()
	e := newWritesEnv(t, orgNodes{})
	e.q.seedIn("c__quarantine", "file:///q1", 2)
	e.q.seedIn("c__quarantine", "https://q2.example/x", 1)
	e.q.seedIn("c__acme", "repo://acme/app/README.md", 1)
	return e
}

// agentCallers are callers that are not the operator: an interactive
// harness session (resolved and by process tree), a queued task, and a
// process orchd started.
func agentCallers(e writesEnv) map[string]context.Context {
	return map[string]context.Context{
		"session":        sessionCtx(context.Background(), sessionPin{SessionID: "s-q", Space: "acme", Revision: e.rev}),
		"task":           taskCtx("t-q"),
		"harness":        callerCtx(callerAuth{pid: 950, harness: "claude"}),
		"orchd-launched": callerCtx(callerAuth{pid: 951, spawned: true}),
	}
}

// TestQuarantineRPCsRefuseAgentCallers: SP-028.1.a2 for list and purge.
// Every non-operator caller is refused, and a refused purge with confirm
// deletes nothing.
func TestQuarantineRPCsRefuseAgentCallers(t *testing.T) {
	e := quarantineEnv(t)
	for name, ctx := range agentCallers(e) {
		if _, err := e.s.ListQuarantine(ctx, &orchdv1.ListQuarantineRequest{}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s list: %v", name, err)
		}
		if _, err := e.s.PurgeQuarantine(ctx, &orchdv1.PurgeQuarantineRequest{All: true, Confirm: true}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s purge: %v", name, err)
		}
	}
	if n := len(e.q.storedIn("c__quarantine", "file:///q1")); n != 2 {
		t.Fatalf("refused purges deleted chunks: %d left", n)
	}
}

// TestListQuarantineRPC: SP-028.1.a1 (list). The operator sees each
// quarantined source with its chunks, and only quarantine's.
func TestListQuarantineRPC(t *testing.T) {
	e := quarantineEnv(t)
	resp, err := e.s.ListQuarantine(operatorCtx, &orchdv1.ListQuarantineRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTotalSources() != 2 || resp.GetTotalChunks() != 3 || len(resp.GetSources()) != 2 {
		t.Fatalf("list = %v", resp)
	}
	if s := resp.GetSources()[0]; s.GetSourceUri() != "file:///q1" || s.GetChunks() != 2 || s.GetSourceType() != "note" {
		t.Fatalf("first = %v", s)
	}
	if s := resp.GetSources()[1]; s.GetSourceUri() != "https://q2.example/x" || s.GetChunks() != 1 {
		t.Fatalf("second = %v", s)
	}
}

// TestPurgeQuarantineRPC: SP-028.1.a1 (purge). Without confirm it only
// counts; with confirm it deletes from quarantine and nowhere else.
func TestPurgeQuarantineRPC(t *testing.T) {
	e := quarantineEnv(t)
	dry, err := e.s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{All: true})
	if err != nil || dry.GetPurged() || dry.GetSources() != 2 || dry.GetChunks() != 3 {
		t.Fatalf("dry run = %v, %v", dry, err)
	}
	if n := len(e.q.storedIn("c__quarantine", "file:///q1")); n != 2 {
		t.Fatalf("dry run deleted: %d left", n)
	}
	one, err := e.s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{SourceUris: []string{"file:///q1"}, Confirm: true})
	if err != nil || !one.GetPurged() || one.GetSources() != 1 || one.GetChunks() != 2 {
		t.Fatalf("named purge = %v, %v", one, err)
	}
	if len(e.q.storedIn("c__quarantine", "file:///q1")) != 0 || len(e.q.storedIn("c__quarantine", "https://q2.example/x")) != 1 {
		t.Fatalf("named purge left %v", e.q.uris())
	}
	// A source held by another space is refused like one held nowhere,
	// and stays where it is.
	for _, uri := range []string{"repo://acme/app/README.md", "file:///nowhere"} {
		_, err := e.s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{SourceUris: []string{uri}, Confirm: true})
		if grpcstatus.Code(err) != codes.FailedPrecondition || !strings.HasSuffix(grpcstatus.Convert(err).Message(), "not in quarantine") {
			t.Fatalf("%s: %v", uri, err)
		}
	}
	if len(e.q.storedIn("c__acme", "repo://acme/app/README.md")) != 1 {
		t.Fatal("a refused purge reached another space")
	}
	all, err := e.s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{All: true, Confirm: true})
	if err != nil || !all.GetPurged() || all.GetSources() != 1 || all.GetChunks() != 1 {
		t.Fatalf("purge all = %v, %v", all, err)
	}
	if len(e.q.storedIn("c__acme", "repo://acme/app/README.md")) != 1 {
		t.Fatal("purge all reached another space")
	}
}

func TestPurgeQuarantineRPCArguments(t *testing.T) {
	e := quarantineEnv(t)
	for name, req := range map[string]*orchdv1.PurgeQuarantineRequest{
		"neither":   {Confirm: true},
		"both":      {All: true, SourceUris: []string{"file:///q1"}, Confirm: true},
		"empty uri": {SourceUris: []string{""}, Confirm: true},
	} {
		if _, err := e.s.PurgeQuarantine(operatorCtx, req); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if n := len(e.q.storedIn("c__quarantine", "file:///q1")); n != 2 {
		t.Fatalf("refused requests deleted: %d left", n)
	}
	// A refused URI is echoed without control characters.
	_, err := e.s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{SourceUris: []string{"x\x1b[2J"}})
	if msg := grpcstatus.Convert(err).Message(); strings.ContainsRune(msg, 0x1b) {
		t.Fatalf("message keeps an escape: %q", msg)
	}
}

// failingQuarantine is a quarantine whose store fails.
type failingQuarantine struct {
	ragService
	res rag.PurgeResult
}

func (failingQuarantine) ListQuarantine(context.Context) ([]rag.QuarantinedSource, int, error) {
	return nil, 0, errors.New("dial tcp 127.0.0.1:6333: refused")
}

func (f failingQuarantine) PurgeQuarantine(context.Context, []string, bool, bool) (rag.PurgeResult, error) {
	return f.res, errors.New("dial tcp 127.0.0.1:6333: refused")
}

func TestQuarantineRPCFailures(t *testing.T) {
	s := &server{}
	if _, err := s.ListQuarantine(operatorCtx, &orchdv1.ListQuarantineRequest{}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no store list: %v", err)
	}
	if _, err := s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{All: true}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no store purge: %v", err)
	}
	if s.quarantineStatus(context.Background()) != nil {
		t.Fatal("counts without a store")
	}
	s.rag = failingQuarantine{res: rag.PurgeResult{Sources: 1, Chunks: 4}}
	_, err := s.ListQuarantine(operatorCtx, &orchdv1.ListQuarantineRequest{})
	if grpcstatus.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("failing list: %v", err)
	}
	_, err = s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{All: true, Confirm: true})
	if grpcstatus.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "after 4 chunk") {
		t.Fatalf("failing purge: %v", err)
	}
	if s.quarantineStatus(context.Background()) != nil {
		t.Fatal("counts from a failing store")
	}
	// A failed count is remembered for the TTL, so Status does not scan a
	// failing store on every call; forget (after a purge) clears it.
	if !s.quarantineCounts.failed {
		t.Fatal("failure not cached")
	}
	s.quarantineCounts.forget()
	if s.quarantineCounts.failed {
		t.Fatal("forget kept the failure")
	}
	// A bidi override in a refused URI is replaced too.
	if got := termSafeErr(errors.New("a\u202eb\x07")); got != "a?b?" {
		t.Fatalf("termSafeErr = %q", got)
	}
}

// TestStatusCountsQuarantine: SP-028's "counts in orch status". Status,
// open to every caller, carries counts and never a source; a purge refreshes
// them.
func TestStatusCountsQuarantine(t *testing.T) {
	e := quarantineEnv(t)
	st, err := e.s.Status(sessionCtx(context.Background(), sessionPin{SessionID: "s-q", Space: "acme", Revision: e.rev}), &orchdv1.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	q := st.GetSpaces().GetQuarantine()
	if q.GetSources() != 2 || q.GetChunks() != 3 {
		t.Fatalf("counts = %v", q)
	}
	if strings.Contains(st.String(), "q1") || strings.Contains(st.String(), "q2.example") {
		t.Fatal("status names a quarantined source")
	}
	// Cached: a write behind orchd's back is not seen until the TTL ends...
	e.q.seedIn("c__quarantine", "file:///q3", 1)
	if got := e.s.quarantineStatus(context.Background()); got.GetSources() != 2 {
		t.Fatalf("cache missed: %v", got)
	}
	// ...but a purge through orchd is seen at once.
	if _, err := e.s.PurgeQuarantine(operatorCtx, &orchdv1.PurgeQuarantineRequest{SourceUris: []string{"file:///q1"}, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.s.quarantineStatus(context.Background()); got.GetSources() != 2 || got.GetChunks() != 2 {
		t.Fatalf("after purge = %v", got)
	}
}
