package daemon

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

const spacesDoc = `{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme"]},{"name":"personal","kind":"private"}]}`

func spacesServer(t *testing.T) *server {
	t.Helper()
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	e, err := spaces.Open(context.Background(), conn, spaces.Env{Home: "/home/u", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	return &server{spaces: e}
}

func callerCtx(a callerAuth) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: a})
}

var operatorCtx = callerCtx(callerAuth{pid: 42})

func draft(t *testing.T, s *server, doc string) *orchdv1.SpacesDraft {
	t.Helper()
	d, err := s.SetSpacesDraft(operatorCtx, &orchdv1.SetSpacesDraftRequest{PolicyJson: doc})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestApplySpacesOperatorOnly is SP-020.2.a1: apply is refused to callers
// descended from a harness or from orchd, and to any caller orchd cannot
// place, and allowed for the operator.
func TestApplySpacesOperatorOnly(t *testing.T) {
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	tests := []struct {
		name string
		ctx  context.Context
		want codes.Code
		says string
	}{
		{name: "an interactive claude session", ctx: callerCtx(callerAuth{pid: 7, harness: "claude"}), want: codes.PermissionDenied, says: "inside a claude session"},
		{name: "an interactive codex session", ctx: callerCtx(callerAuth{pid: 7, harness: "codex"}), want: codes.PermissionDenied, says: "inside a codex session"},
		{name: "a process orchd started", ctx: callerCtx(callerAuth{pid: 7, spawned: true}), want: codes.PermissionDenied, says: "orchd started"},
		{name: "a queued task", ctx: queued, want: codes.PermissionDenied, says: "task task-1"},
		{name: "an unreadable ancestry", ctx: callerCtx(callerAuth{pid: 7, harnessErr: errors.New("gone")}), want: codes.PermissionDenied, says: "cannot read"},
		{name: "an unidentified peer", ctx: callerCtx(callerAuth{err: errPeerUnsupported}), want: codes.PermissionDenied, says: "cannot identify"},
		{name: "no peer at all", ctx: context.Background(), want: codes.PermissionDenied, says: "unidentified"},
		{name: "the operator", ctx: operatorCtx, want: codes.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := spacesServer(t)
			d := draft(t, s, spacesDoc)
			resp, err := s.ApplySpaces(tt.ctx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()})
			if grpcstatus.Code(err) != tt.want {
				t.Fatalf("ApplySpaces = %v, want %v", err, tt.want)
			}
			if tt.want != codes.OK {
				if !strings.Contains(err.Error(), tt.says) {
					t.Errorf("refusal %q does not say %q", err, tt.says)
				}
				if r, _ := s.spaces.Current(); r != nil {
					t.Error("a refused apply changed the policy")
				}
				return
			}
			if r, _ := s.spaces.Current(); resp.GetAppliedRevision() != d.GetRevision() || !r.Configured() {
				t.Fatalf("apply did not take effect: %v", resp)
			}
		})
	}
}

func TestApplySpacesErrors(t *testing.T) {
	s := spacesServer(t)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("no revision: %v", err)
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: 9}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("unsaved revision: %v", err)
	}
	first := draft(t, s, spacesDoc)
	draft(t, s, `{"spaces":[{"name":"globex","kind":"isolated"}]}`)
	// SP-020.2.a2 through the RPC: the reviewed revision is no longer newest.
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: first.GetRevision()}); grpcstatus.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "newer") {
		t.Errorf("stale revision: %v", err)
	}
	bad := draft(t, s, `{"spaces":[{"name":"a","kind":"isolated","reads":["b"]},{"name":"b","kind":"isolated"}]}`)
	if len(bad.GetProblems()) != 1 || bad.GetProblems()[0].GetRule() != string(spaces.RuleIsolatedReadsIsolated) {
		t.Errorf("draft problems = %v", bad.GetProblems())
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: bad.GetRevision()}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("invalid revision: %v", err)
	}
	if r, _ := s.spaces.Current(); r != nil {
		t.Error("a failed apply changed the policy")
	}
}

func TestSetSpacesDraftRejectsBadJSON(t *testing.T) {
	s := spacesServer(t)
	if _, err := s.SetSpacesDraft(operatorCtx, &orchdv1.SetSpacesDraftRequest{PolicyJson: `{"spaces":[{"nmae":"x"}]}`}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("SetSpacesDraft = %v", err)
	}
}

func TestDiffSpacesAndStatus(t *testing.T) {
	s := spacesServer(t)
	d := draft(t, s, spacesDoc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	next := draft(t, s, `{"spaces":[{"name":"acme","kind":"isolated","github_orgs":["acme","acme-labs"]},{"name":"home","kind":"private"}]}`)
	diff, err := s.DiffSpaces(operatorCtx, &orchdv1.DiffSpacesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if diff.GetAppliedRevision() != d.GetRevision() || diff.GetDraftRevision() != next.GetRevision() ||
		diff.GetAppliedJson() == "" || diff.GetDraftJson() == "" || len(diff.GetChanges()) != 3 || len(diff.GetProblems()) != 0 {
		t.Fatalf("DiffSpaces = %v", diff)
	}
	st, err := s.Status(operatorCtx, &orchdv1.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sp := st.GetSpaces()
	if !sp.GetConfigured() || sp.GetAppliedRevision() != d.GetRevision() || sp.GetDraftRevision() != next.GetRevision() ||
		!slices.Equal(sp.GetDrift(), []string{spaces.DriftDraft}) {
		t.Fatalf("Status.spaces = %v", sp)
	}
}

func TestSpacesUnavailable(t *testing.T) {
	s := &server{}
	if _, err := s.SetSpacesDraft(operatorCtx, &orchdv1.SetSpacesDraftRequest{PolicyJson: spacesDoc}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("SetSpacesDraft = %v", err)
	}
	if _, err := s.DiffSpaces(operatorCtx, &orchdv1.DiffSpacesRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("DiffSpaces = %v", err)
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: 1}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("ApplySpaces = %v", err)
	}
	if s.spacesStatus(context.Background()) != nil {
		t.Error("status without spaces is not nil")
	}
}

func TestSpacesStatusReadError(t *testing.T) {
	s := spacesServer(t)
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "other"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	e, err := spaces.Open(context.Background(), conn, spaces.DefaultEnv())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	s.spaces = e
	if st := s.spacesStatus(context.Background()); !strings.Contains(st.GetError(), "could not read") {
		t.Fatalf("status = %v", st)
	}
	if _, err := s.DiffSpaces(operatorCtx, &orchdv1.DiffSpacesRequest{}); grpcstatus.Code(err) != codes.Internal {
		t.Errorf("DiffSpaces = %v", err)
	}
	if _, err := s.SetSpacesDraft(operatorCtx, &orchdv1.SetSpacesDraftRequest{PolicyJson: spacesDoc}); grpcstatus.Code(err) != codes.Internal {
		t.Errorf("SetSpacesDraft = %v", err)
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: 1}); grpcstatus.Code(err) != codes.Internal {
		t.Errorf("ApplySpaces = %v", err)
	}
}

func TestHarnessAncestor(t *testing.T) {
	// 50 is a shell under claude (40) under a terminal (30) under launchd;
	// 61 is a shell under an npm-installed Claude Code (62), which the kernel
	// names node; 63 is under Claude Code's native installer, named for its
	// version; 64 is under Codex from npm.
	progs := map[int]procstat.Program{
		50: {Name: "zsh"},
		40: {Name: "claude"},
		30: {Name: "Terminal"},
		60: {Name: "orch", Path: "/opt/homebrew/bin/orch", Args: []string{"orch", "spaces", "apply", "claude"}},
		70: {Name: "zsh"},
		61: {Name: "bash"},
		62: {Name: "node", Path: "/opt/homebrew/bin/node", Args: []string{"node", "/opt/homebrew/bin/claude"}},
		63: {Name: "2.1.3", Path: "/Users/u/.local/share/claude/versions/2.1.3"},
		64: {Name: "node", Args: []string{"node", "/usr/lib/node_modules/@openai/codex/bin/codex.js"}},
		65: {Name: "node", Args: []string{"/usr/bin/node", "/x/node_modules/@anthropic-ai/claude-code/cli.js"}},
		66: {Name: "node"}, // another user's node: its script cannot be read
		67: {Name: "node", Args: []string{"node", "/x/build.js"}},
	}
	parents := map[int]int{50: 40, 40: 30, 30: 1, 60: 30, 70: 71, 61: 62, 62: 30, 63: 30, 64: 30, 65: 30, 66: 30, 67: 30}
	program := func(pid int) (procstat.Program, error) {
		if p, ok := progs[pid]; ok {
			return p, nil
		}
		return procstat.Program{}, errors.New("no such process")
	}
	parent := func(pid int) (int, error) {
		if p, ok := parents[pid]; ok {
			return p, nil
		}
		return 0, errors.New("no such process")
	}
	tests := []struct {
		pid     int
		want    string
		wantErr bool
	}{
		{pid: 50, want: "claude"},
		{pid: 40, want: "claude"},
		{pid: 60}, // an argument past the script is not a program
		{pid: 61, want: "claude"},
		{pid: 63, want: "claude"},
		{pid: 64, want: "codex"},
		{pid: 65, want: "claude"},
		{pid: 66, wantErr: true},
		{pid: 67},
		{pid: 70, wantErr: true}, // its parent 71 cannot be read
		{pid: 99, wantErr: true},
	}
	for _, tt := range tests {
		got, err := harnessAncestor(tt.pid, parent, program)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("harnessAncestor(%d) = %q, %v", tt.pid, got, err)
		}
	}
	if _, err := harnessAncestor(50, parent, nil); err == nil {
		t.Error("no program lookup did not fail closed")
	}
	loop := func(pid int) (int, error) { return pid + 1, nil }
	if _, err := harnessAncestor(2, loop, func(int) (procstat.Program, error) { return procstat.Program{Name: "x"}, nil }); err == nil {
		t.Error("an endless ancestry did not fail")
	}
	creds := peerCredentials{self: 1000, parent: parent, program: program, peerPID: func(net.Conn) (int, error) { return 61, nil }}
	if a := creds.identify(nil); a.harness != "claude" || a.harnessErr != nil {
		t.Errorf("identify = %+v", a)
	}
	// A failed harness walk is not retried, so it stays a refusal.
	creds.peerPID = func(net.Conn) (int, error) { return 70, nil }
	if a := creds.identify(nil); a.harnessErr == nil {
		t.Errorf("identify = %+v, want a harness error", a)
	}
}

// TestDiffSpacesOperatorOnly: the diff names every job's orgs and roots, so a
// session in one job must not read it.
func TestDiffSpacesOperatorOnly(t *testing.T) {
	s := spacesServer(t)
	draft(t, s, spacesDoc)
	for _, ctx := range []context.Context{
		callerCtx(callerAuth{pid: 7, harness: "claude"}),
		callerCtx(callerAuth{pid: 7, spawned: true}),
	} {
		if _, err := s.DiffSpaces(ctx, &orchdv1.DiffSpacesRequest{}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("DiffSpaces = %v, want PermissionDenied", err)
		}
	}
}

// TestAgentDraftThroughRPC: a session may save a draft, but the operator
// sees it was not theirs and must confirm to apply it.
func TestAgentDraftThroughRPC(t *testing.T) {
	s := spacesServer(t)
	session := callerCtx(callerAuth{pid: 7, harness: "claude"})
	d, err := s.SetSpacesDraft(session, &orchdv1.SetSpacesDraftRequest{PolicyJson: spacesDoc})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := s.DiffSpaces(operatorCtx, &orchdv1.DiffSpacesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if diff.GetDraftByOperator() || diff.GetDraftCreatedBy() != "process 7 in claude" || diff.GetDraftCreatedAt() == 0 ||
		len(diff.GetChanges()) != 2 || len(diff.GetChanges()[0].GetDetails()) == 0 {
		t.Fatalf("DiffSpaces = %v", diff)
	}
	_, err = s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()})
	if grpcstatus.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "accept-agent-draft") {
		t.Fatalf("ApplySpaces(agent draft) = %v", err)
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision(), AcceptAgentDraft: true}); err != nil {
		t.Fatalf("ApplySpaces(agent draft, accepted) = %v", err)
	}
}

func TestCallerLabel(t *testing.T) {
	tests := []struct {
		ctx  context.Context
		want string
	}{
		{context.Background(), "unidentified process"},
		{callerCtx(callerAuth{err: errors.New("x")}), "unidentified process"},
		{callerCtx(callerAuth{pid: 3, spawned: true}), "process 3 started by orchd"},
		{callerCtx(callerAuth{pid: 4, harness: "codex"}), "process 4 in codex"},
		{operatorCtx, "operator (process 42)"},
	}
	for _, tt := range tests {
		if got := callerLabel(tt.ctx); got != tt.want {
			t.Errorf("callerLabel = %q, want %q", got, tt.want)
		}
	}
}
