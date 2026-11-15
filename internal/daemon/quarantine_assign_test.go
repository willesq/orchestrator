package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/cli"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// serveAs serves s on a fresh socket, every call made as caller, and points
// the CLI at it.
func serveAs(t *testing.T, s *server, caller callerAuth) {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "orch-sp028-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Reason: this test may itself run under an agent harness, which orchd
	// would refuse; the interceptor decides who the CLI is.
	as := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(peer.NewContext(ctx, &peer.Peer{AuthInfo: caller}), req)
	}
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(as))
	orchdv1.RegisterOrchdServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	t.Setenv("ORCH_SOCKET", sock)
}

// orch runs the real CLI against the socket serveAs set.
func orch(args ...string) (string, error) {
	root := cli.NewRootCmd(orchdclient.Dial)
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestQuarantineAssignThroughOrchd: SP-028.1.a1 (assign) end to end. The
// operator's 'orch quarantine assign' moves a quarantined source to a space
// through orchd's RetagSource, which records the override: a later write of
// the same source lands in that space, not in quarantine.
func TestQuarantineAssignThroughOrchd(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	e := retagEnv(t)
	operator := context.WithValue(context.Background(), resolvedKey{}, resolvedCaller{kind: callerOperator})
	if err := e.ingestAs(operator, "manual://note", "the operator's note"); err != nil {
		t.Fatal(err)
	}
	e.placedAs(t, "manual://note", spaces.Quarantine, rag.OriginClient)
	serveAs(t, e.s, callerAuth{pid: 42})

	out, err := orch("quarantine", "list")
	if err != nil || !strings.Contains(out, "manual://note") || !strings.Contains(out, "1 source(s)") {
		t.Fatalf("list = %q, %v", out, err)
	}
	// Leaving quarantine needs --confirm; without it nothing moves.
	if _, err := orch("quarantine", "assign", "manual://note", "personal"); err == nil {
		t.Fatal("assign without --confirm moved the source")
	}
	e.placedAs(t, "manual://note", spaces.Quarantine, rag.OriginClient)

	out, err = orch("quarantine", "assign", "--confirm", "manual://note", "personal")
	if err != nil || !strings.Contains(out, "to personal") {
		t.Fatalf("assign = %q, %v", out, err)
	}
	e.placedAs(t, "manual://note", "personal", rag.OriginClient)
	if n := len(e.q.storedIn("c__quarantine", "manual://note")); n != 0 {
		t.Fatalf("quarantine still holds %d chunk(s)", n)
	}
	if out, err := orch("quarantine", "list"); err != nil || !strings.Contains(out, "quarantine is empty") {
		t.Fatalf("list after = %q, %v", out, err)
	}
	// The override rule: the next write of the source stays in personal.
	if err := e.ingestAs(operator, "manual://note", "the operator's note, edited"); err != nil {
		t.Fatal(err)
	}
	e.placedAs(t, "manual://note", "personal", rag.OriginClient)
	// A source that is not in quarantine is refused, wherever it is.
	if _, err := orch("quarantine", "assign", "--confirm", "manual://note", "acme"); err == nil || !strings.Contains(err.Error(), "not in quarantine") {
		t.Fatalf("assign of a placed source: %v", err)
	}
	e.placedAs(t, "manual://note", "personal", rag.OriginClient)
}

// TestQuarantineCommandsRefusedThroughOrchd: SP-028.1.a2 end to end. From a
// harness process, list, assign and purge are refused by orchd and nothing
// leaves quarantine.
func TestQuarantineCommandsRefusedThroughOrchd(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	e := retagEnv(t)
	operator := context.WithValue(context.Background(), resolvedKey{}, resolvedCaller{kind: callerOperator})
	if err := e.ingestAs(operator, "manual://note", "the operator's note"); err != nil {
		t.Fatal(err)
	}
	serveAs(t, e.s, callerAuth{pid: 950, harness: "claude"})
	for _, args := range [][]string{
		{"quarantine", "list"},
		{"quarantine", "assign", "--confirm", "manual://note", "personal"},
		{"quarantine", "purge", "--all", "--confirm"},
	} {
		if _, err := orch(args...); err == nil || !strings.Contains(err.Error(), "only the operator") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	e.placedAs(t, "manual://note", spaces.Quarantine, rag.OriginClient)
}
