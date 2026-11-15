package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// quarantineClient is orchd's quarantine as a test fake: it holds sources
// by URI, refuses non-operators like orchd, and records purges.
type quarantineClient struct {
	orchdv1.OrchdClient
	held      map[string]int32
	refuse    bool // answer as orchd answers a non-operator
	purgeReqs []*orchdv1.PurgeQuarantineRequest
}

var errNotOperator = grpcstatus.Error(codes.PermissionDenied, "only the operator can list quarantine")

func (q *quarantineClient) ListQuarantine(context.Context, *orchdv1.ListQuarantineRequest, ...grpc.CallOption) (*orchdv1.ListQuarantineResponse, error) {
	if q.refuse {
		return nil, errNotOperator
	}
	resp := &orchdv1.ListQuarantineResponse{}
	for uri, n := range q.held {
		resp.Sources = append(resp.Sources, &orchdv1.QuarantineSource{SourceUri: uri, SourceType: "file", Chunks: n})
		resp.TotalSources++
		resp.TotalChunks += n
	}
	return resp, nil
}

func (q *quarantineClient) PurgeQuarantine(_ context.Context, req *orchdv1.PurgeQuarantineRequest, _ ...grpc.CallOption) (*orchdv1.PurgeQuarantineResponse, error) {
	q.purgeReqs = append(q.purgeReqs, req)
	if q.refuse {
		return nil, grpcstatus.Error(codes.PermissionDenied, "only the operator can purge quarantine")
	}
	resp := &orchdv1.PurgeQuarantineResponse{Purged: req.GetConfirm()}
	uris := req.GetSourceUris()
	if req.GetAll() {
		uris = nil
		for u := range q.held {
			uris = append(uris, u)
		}
	}
	for _, u := range uris {
		n, ok := q.held[u]
		if !ok {
			return nil, grpcstatus.Errorf(codes.FailedPrecondition, "%q: not in quarantine", u)
		}
		resp.Sources++
		resp.Chunks += n
	}
	if req.GetConfirm() {
		for _, u := range uris {
			delete(q.held, u)
		}
	}
	return resp, nil
}

// fakeRetag stands in for SP-071's RetagSource: it moves the source out of
// the client's quarantine and records the sticky override.
type fakeRetag struct {
	q         *quarantineClient
	reqs      []RetagRequest
	overrides map[string]string // source_uri -> space
	err       error
}

func (f *fakeRetag) Retag(_ context.Context, req RetagRequest) (RetagResult, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return RetagResult{}, f.err
	}
	n := f.q.held[req.SourceURI]
	delete(f.q.held, req.SourceURI)
	if f.overrides == nil {
		f.overrides = map[string]string{}
	}
	f.overrides[req.SourceURI] = req.Space
	return RetagResult{Chunks: int(n)}, nil
}

// runQuarantine runs 'orch quarantine args...' against q, with retag as the
// Retagger (nil: the build's default).
func runQuarantine(q *quarantineClient, retag Retagger, args ...string) (string, error) {
	var rf retaggerFactory
	if retag != nil {
		rf = func(orchdv1.OrchdClient) Retagger { return retag }
	}
	root := &cobra.Command{Use: "orch", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newQuarantineCmd(factory(q, nil), rf))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"quarantine"}, args...))
	err := root.Execute()
	return out.String(), err
}

// TestQuarantineListShowsSourcesAndCounts: SP-028.1.a1 (list).
func TestQuarantineListShowsSourcesAndCounts(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	q := &quarantineClient{held: map[string]int32{"file:///q1": 2}}
	out, err := runQuarantine(q, nil, "list")
	if err != nil || !strings.Contains(out, "file:///q1") || !strings.Contains(out, "1 source(s), 2 chunk(s) in quarantine") {
		t.Fatalf("list = %q, %v", out, err)
	}
	empty, err := runQuarantine(&quarantineClient{}, nil, "list")
	if err != nil || !strings.Contains(empty, "quarantine is empty") {
		t.Fatalf("empty = %q, %v", empty, err)
	}
}

func TestWriteQuarantineListMarksACutList(t *testing.T) {
	var b bytes.Buffer
	writeQuarantineList(&b, &orchdv1.ListQuarantineResponse{TotalSources: 9, TotalChunks: 20, Sources: []*orchdv1.QuarantineSource{
		{SourceUri: "file:///a\x1b[2J", SourceType: "file", Chunks: 3, IngestedAt: 1_700_000_000}}})
	out := b.String()
	if !strings.Contains(out, "showing the first 1") || strings.ContainsRune(out, 0x1b) {
		t.Fatalf("out = %q", out)
	}
}

// TestQuarantineAssignMovesAndRecordsOverride: SP-028.1.a1 (assign), against
// a fake of SP-071's retag: assign asks for a retag from quarantine, the
// source leaves quarantine, and an override rule names its space.
func TestQuarantineAssignMovesAndRecordsOverride(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	q := &quarantineClient{held: map[string]int32{"file:///q1": 2, "file:///q2": 1}}
	retag := &fakeRetag{q: q}
	out, err := runQuarantine(q, retag, "assign", "file:///q1", "personal")
	if err != nil || !strings.Contains(out, "moved 2 chunk(s) of file:///q1 to personal") {
		t.Fatalf("assign = %q, %v", out, err)
	}
	want := RetagRequest{SourceURI: "file:///q1", Space: "personal", ExpectFrom: "quarantine"}
	if len(retag.reqs) != 1 || retag.reqs[0] != want {
		t.Fatalf("retag requests = %+v", retag.reqs)
	}
	if retag.overrides["file:///q1"] != "personal" {
		t.Fatalf("overrides = %v", retag.overrides)
	}
	if _, ok := q.held["file:///q1"]; ok {
		t.Fatal("source still in quarantine")
	}
	// --confirm is passed through, for a move that needs it (into public).
	if _, err := runQuarantine(q, retag, "assign", "--confirm", "file:///q2", "public"); err != nil || !retag.reqs[1].Confirm {
		t.Fatalf("confirm = %+v, %v", retag.reqs, err)
	}
}

func TestQuarantineAssignRefusals(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	q := &quarantineClient{held: map[string]int32{"file:///q1": 2}}
	retag := &fakeRetag{q: q}
	if _, err := runQuarantine(q, retag, "assign", "file:///elsewhere", "personal"); err == nil || !strings.Contains(err.Error(), "not in quarantine") {
		t.Fatalf("not quarantined: %v", err)
	}
	if _, err := runQuarantine(q, retag, "assign", "file:///q1", "quarantine"); err == nil {
		t.Fatal("assign into quarantine accepted")
	}
	if len(retag.reqs) != 0 {
		t.Fatalf("refused assigns retagged: %+v", retag.reqs)
	}
	retag.err = errors.New("retag refused\x1b[2J")
	if _, err := runQuarantine(q, retag, "assign", "file:///q1", "personal"); err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("retag error = %q", err)
	}
	// Until SP-071 lands, the build's assign says so and moves nothing.
	if _, err := runQuarantine(q, nil, "assign", "file:///q1", "personal"); !errors.Is(err, errRetagUnavailable) && (err == nil || err.Error() != errRetagUnavailable.Error()) {
		t.Fatalf("default retagger: %v", err)
	}
	if q.held["file:///q1"] != 2 {
		t.Fatal("a failed assign moved the source")
	}
}

// TestQuarantinePurgeIsDryRunUnlessConfirmed: SP-028.1.a1 (purge).
func TestQuarantinePurgeIsDryRunUnlessConfirmed(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	q := &quarantineClient{held: map[string]int32{"file:///q1": 2, "file:///q2": 1}}
	out, err := runQuarantine(q, nil, "purge", "--all")
	if err != nil || !strings.Contains(out, "would purge 2 source(s), 3 chunk(s); run again with --confirm") || len(q.held) != 2 {
		t.Fatalf("dry run = %q, %v, held %v", out, err, q.held)
	}
	out, err = runQuarantine(q, nil, "purge", "--confirm", "file:///q1")
	if err != nil || !strings.Contains(out, "purged 1 source(s), 2 chunk(s)") || len(q.held) != 1 {
		t.Fatalf("named = %q, %v, held %v", out, err, q.held)
	}
	last := q.purgeReqs[len(q.purgeReqs)-1]
	if !last.GetConfirm() || last.GetAll() || len(last.GetSourceUris()) != 1 {
		t.Fatalf("request = %v", last)
	}
	for _, args := range [][]string{{"purge"}, {"purge", "--all", "file:///q2"}} {
		if _, err := runQuarantine(q, nil, args...); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

// TestQuarantineCommandsRefusedForAgentCallers: SP-028.1.a2 at the command.
// A queued task is refused before orchd is asked; an agent session gets
// orchd's refusal, and assign asks for no retag.
func TestQuarantineCommandsRefusedForAgentCallers(t *testing.T) {
	q := &quarantineClient{held: map[string]int32{"file:///q1": 2}}
	retag := &fakeRetag{q: q}
	cmds := [][]string{{"list"}, {"assign", "file:///q1", "personal"}, {"purge", "--all", "--confirm"}}
	t.Setenv(agentrun.TaskIDEnv, "task-1")
	for _, args := range cmds {
		if _, err := runQuarantine(q, retag, args...); err == nil || !strings.Contains(err.Error(), "queue tasks cannot") {
			t.Fatalf("task %v: %v", args, err)
		}
	}
	t.Setenv(agentrun.TaskIDEnv, "")
	q.refuse = true
	for _, args := range cmds {
		if _, err := runQuarantine(q, retag, args...); err == nil || !strings.Contains(err.Error(), "only the operator") {
			t.Fatalf("agent %v: %v", args, err)
		}
	}
	if len(retag.reqs) != 0 || q.held["file:///q1"] != 2 {
		t.Fatalf("refused callers changed quarantine: retags %v, held %v", retag.reqs, q.held)
	}
}

func TestQuarantineDialFailure(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	root := &cobra.Command{Use: "orch", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newQuarantineCmd(factory(nil, errors.New("no orchd")), nil))
	root.SetArgs([]string{"quarantine", "list"})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "no orchd") {
		t.Fatalf("err = %v", err)
	}
}

func TestRootHasQuarantine(t *testing.T) {
	cmd, _, err := NewRootCmd(factory(&quarantineClient{}, nil)).Find([]string{"quarantine", "purge"})
	if err != nil || cmd.Name() != "purge" {
		t.Fatalf("find = %v, %v", cmd, err)
	}
}
