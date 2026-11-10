package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/cli"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// logFixture stores a record in o's chain the way the hooks' intake would,
// taken at time at.
func logFixture(t *testing.T, s *server, o toollog.Owner, at time.Time, r toollog.Record) {
	t.Helper()
	r.TS = at.UnixNano()
	if r.Decision == "" {
		r.Decision = decisionLogged
	}
	if !s.toolLog.writer.Enqueue(toollog.Key{Quota: o.String(), Owner: o}, 100, func() toollog.Record { return r }) {
		t.Fatal("fixture event dropped")
	}
}

func callFixture(cmd string) toollog.Record {
	return toollog.Record{Event: toollog.EventPreToolUse, Tool: "Bash", Command: cmd}
}

func moveFixture(from, to string) toollog.Record {
	return toollog.Record{Event: toollog.EventCwdChanged, OldCwd: from, NewCwd: to}
}

func taskOwner(id string) toollog.Owner { return toollog.Owner{Kind: toollog.OwnerTask, ID: id} }

func taskCtx(id string) context.Context {
	return context.WithValue(context.Background(), resolvedKey{},
		resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: id}})
}

func listEvents(s *server, ctx context.Context, owner string) ([]string, error) {
	rep, err := s.ListToolEvents(ctx, &orchdv1.ListToolEventsRequest{Owner: owner})
	if err != nil {
		return nil, err
	}
	var cmds []string
	for _, e := range rep.GetEvents() {
		cmds = append(cmds, e.GetCommand())
	}
	return cmds, nil
}

// TestListToolEventsCallerScope is SP-016.1.a1: an agent session listing
// events gets only its own session's; asking for another session, or any
// other chain, is refused. The operator reads any chain; a queued task
// only its own.
func TestListToolEventsCallerScope(t *testing.T) {
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-a", e.dirs[0])
	e.pin(t, e.hook("codex", 410), "sess-b", e.dirs[1])
	withToolLog(t, e.s)
	now := time.Now()
	for i := range 3 {
		logFixture(t, e.s, sessionOwner("sess-a"), now, callFixture(fmt.Sprint("a-", i)))
		logFixture(t, e.s, sessionOwner("sess-b"), now, callFixture(fmt.Sprint("b-", i)))
	}
	logFixture(t, e.s, taskOwner("t-1"), now, callFixture("t-0"))

	inA := e.session("claude", 400, "sess-a")
	for _, owner := range []string{"", "sess-a", "session:sess-a"} {
		cmds, err := listEvents(e.s, inA, owner)
		if err != nil || strings.Join(cmds, ",") != "a-0,a-1,a-2" {
			t.Fatalf("sess-a reading %q got %v, %v; want only its own events", owner, cmds, err)
		}
	}
	for _, owner := range []string{"sess-b", "session:sess-b", "task:t-1", "sess-unknown"} {
		if cmds, err := listEvents(e.s, inA, owner); grpcstatus.Code(err) != codes.PermissionDenied || cmds != nil {
			t.Errorf("sess-a reading %q = %v, %v; want PermissionDenied", owner, cmds, err)
		}
	}
	if _, err := listEvents(e.s, inA, "task:../x"); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad owner from a session = %v, want InvalidArgument", err)
	}
	// A refusal never says whether the other session exists.
	_, errKnown := listEvents(e.s, inA, "sess-b")
	_, errUnknown := listEvents(e.s, inA, "sess-nobody")
	if errKnown.Error() != errUnknown.Error() {
		t.Errorf("refusals differ: %v / %v", errKnown, errUnknown)
	}

	// A pin a resume moved to another process is no longer the caller's.
	stale := sessionCtx(e.hook("claude", 400), sessionPin{SessionID: "sess-b", Harness: "claude",
		Process: procstat.Identity{PID: 400, Started: e.tbl.started[400]}})
	if _, err := listEvents(e.s, stale, ""); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("a pin of another process = %v, want PermissionDenied", err)
	}

	// Callers orchd cannot tie to a session read nothing.
	unpinned := hookIn("claude", procstat.Identity{PID: 600, Started: e.base})
	for name, ctx := range map[string]context.Context{"unpinned harness": unpinned, "task without a record": taskCtx("")} {
		if _, err := listEvents(e.s, ctx, ""); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("%s = %v, want PermissionDenied", name, err)
		}
	}

	// A queued task reads its own task's chain only.
	if cmds, err := listEvents(e.s, taskCtx("t-1"), ""); err != nil || strings.Join(cmds, ",") != "t-0" {
		t.Errorf("task t-1 got %v, %v", cmds, err)
	}
	if _, err := listEvents(e.s, taskCtx("t-1"), "sess-a"); grpcstatus.Code(err) != codes.PermissionDenied {
		t.Errorf("task reading a session = %v, want PermissionDenied", err)
	}
	if !taskCallable[orchdv1.Orchd_ListToolEvents_FullMethodName] {
		t.Error("queued tasks cannot reach ListToolEvents")
	}

	// The operator reads any chain, and must name one.
	for owner, want := range map[string]string{"sess-b": "b-0,b-1,b-2", "task:t-1": "t-0"} {
		if cmds, err := listEvents(e.s, operatorCtx, owner); err != nil || strings.Join(cmds, ",") != want {
			t.Errorf("operator reading %s got %v, %v", owner, cmds, err)
		}
	}
	for _, owner := range []string{"", "task:../x"} {
		if _, err := listEvents(e.s, operatorCtx, owner); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("operator reading %q = %v, want InvalidArgument", owner, err)
		}
	}
}

// TestListToolEventsPages: records come a page at a time after a sequence
// number, with the retention cut; without a log the RPC says so.
func TestListToolEventsPages(t *testing.T) {
	s, _ := pinsServer(t)
	if _, err := s.ListToolEvents(operatorCtx, &orchdv1.ListToolEventsRequest{Owner: "s"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("without a command log = %v, want Unavailable", err)
	}
	path := withToolLog(t, s)
	o := sessionOwner("sess-p")
	for i := range 5 {
		logFixture(t, s, o, time.Now(), callFixture(fmt.Sprint("c", i)))
	}
	logFixture(t, s, o, time.Now(), moveFixture("/a", "/b"))
	rep, err := s.ListToolEvents(operatorCtx, &orchdv1.ListToolEventsRequest{Owner: "sess-p", Limit: 2})
	if err != nil || len(rep.GetEvents()) != 2 || !rep.GetMore() || rep.GetOwner() != "sess-p" {
		t.Fatalf("first page = %v, %v", rep, err)
	}
	rep, err = s.ListToolEvents(operatorCtx, &orchdv1.ListToolEventsRequest{Owner: "sess-p", AfterSeq: 4, Limit: 5000})
	if err != nil || len(rep.GetEvents()) != 2 || rep.GetMore() {
		t.Fatalf("last page = %v, %v", rep, err)
	}
	if m := rep.GetEvents()[1]; m.GetEvent() != toollog.EventCwdChanged || m.GetNewCwd() != "/b" || m.GetSeq() != 6 || m.GetKind() != toollog.KindEvent {
		t.Errorf("move record = %v", m)
	}
	if err := tamperFile(path, `INSERT INTO pruned VALUES ('session', 'sess-p', 3, 'h', 'event')`); err != nil {
		t.Fatal(err)
	}
	if rep, err = s.ListToolEvents(operatorCtx, &orchdv1.ListToolEventsRequest{Owner: "sess-p"}); err != nil || rep.GetPruned() != 3 {
		t.Errorf("pruned = %v, %v", rep, err)
	}
}

// summaryEnv is the placement layout (oss public at code, personal at
// code/willesq, acme isolated at code/company1) with sess-acme pinned in
// acme's checkout, sess-me in personal, and a command log.
func summaryEnv(t *testing.T) (*server, string) {
	t.Helper()
	s, code, _ := placementServer(t)
	app := code + "/company1/app"
	if err := os.MkdirAll(code+"/willesq", 0o755); err != nil {
		t.Fatal(err)
	}
	withPins(t, s, app, code+"/willesq")
	for ctx, id := range map[context.Context]string{inClaude: "sess-acme", inCodex: "sess-me"} {
		if _, err := s.PinSession(ctx, pinReq(id, map[string]string{"sess-acme": app, "sess-me": code + "/willesq"}[id], "")); err != nil {
			t.Fatal(err)
		}
	}
	withToolLog(t, s)
	return s, code
}

// TestSummarizeToolLogPerSpace is SP-016.1.a2: the weekly summary reports,
// per space, tool calls, directory moves and moves into another space.
func TestSummarizeToolLogPerSpace(t *testing.T) {
	s, code := summaryEnv(t)
	app := code + "/company1/app"
	now := time.Now()
	acme, me := sessionOwner("sess-acme"), sessionOwner("sess-me")
	for i := range 3 {
		logFixture(t, s, acme, now, callFixture(fmt.Sprint("make ", i)))
	}
	logFixture(t, s, acme, now, toollog.Record{Event: toollog.EventPreToolUse, Tool: "Bash", Command: "cat x", Decision: toollog.DecisionRefused})
	logFixture(t, s, acme, now, moveFixture(app, app+"/sub"))          // within acme
	logFixture(t, s, acme, now, moveFixture(app, code+"/willesq"))     // into personal
	logFixture(t, s, acme, now, moveFixture(app, code+"/willesq"))     // again
	logFixture(t, s, acme, now, moveFixture(app, code+"/lib"))         // public root: sessions there are personal's
	logFixture(t, s, me, now.Add(-8*24*time.Hour), callFixture("old")) // outside the week
	logFixture(t, s, me, now.Add(-8*24*time.Hour), moveFixture(code+"/willesq", app))
	logFixture(t, s, me, now, callFixture("ls"))
	logFixture(t, s, me, now, moveFixture(code+"/willesq", code+"/willesq/notes")) // within personal
	logFixture(t, s, me, now, moveFixture(code+"/willesq", app))                   // into acme
	logFixture(t, s, taskOwner("t-1"), now, callFixture("go test"))
	logFixture(t, s, taskOwner("t-1"), now, moveFixture("/a", app))
	logFixture(t, s, sessionOwner("sess-gone"), now, callFixture("echo")) // no pin
	flushToolLog(t, s)

	rep, err := s.SummarizeToolLog(operatorCtx, &orchdv1.SummarizeToolLogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Duration(rep.GetUntil()-rep.GetSince()) * time.Second; d != 7*24*time.Hour {
		t.Errorf("window = %v, want a week", d)
	}
	got := map[string]string{}
	for _, r := range rep.GetSpaces() {
		got[r.GetSpace()] = fmt.Sprintf("sessions=%d calls=%d moves=%d cross=%d refusals=%d",
			r.GetSessions(), r.GetToolCalls(), r.GetDirectoryMoves(), r.GetCrossSpaceMoves(), r.GetRefusals())
	}
	want := map[string]string{
		"acme":     "sessions=1 calls=4 moves=4 cross=3 refusals=1",
		"personal": "sessions=1 calls=1 moves=2 cross=1 refusals=0",
		"":         "sessions=2 calls=2 moves=1 cross=0 refusals=0",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || rep.GetUnplacedMoves() != 0 {
		t.Errorf("summary = %v (unplaced %d), want %v", got, rep.GetUnplacedMoves(), want)
	}
	if rep.GetSpaces()[0].GetSpace() != "" || rep.GetSpaces()[1].GetSpace() != "acme" {
		t.Errorf("spaces not sorted: %v", rep.GetSpaces())
	}

	// A wider window takes the older events in.
	rep, err = s.SummarizeToolLog(operatorCtx, &orchdv1.SummarizeToolLogRequest{Since: now.Add(-9 * 24 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.GetSpaces() {
		if r.GetSpace() == "personal" && (r.GetToolCalls() != 2 || r.GetCrossSpaceMoves() != 2) {
			t.Errorf("personal over 9 days = %v", r)
		}
	}

	// Past the directory budget, moves are counted but not placed.
	defer func(n int) { maxSummaryDirs = n }(maxSummaryDirs)
	maxSummaryDirs = 1
	rep, err = s.SummarizeToolLog(operatorCtx, &orchdv1.SummarizeToolLogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.GetUnplacedMoves() == 0 {
		t.Errorf("no move past the budget: %v", rep)
	}
}

// TestSummarizeToolLogAccess: the summary is the operator's alone, needs a
// past window, and with no policy applied puts everything under "".
func TestSummarizeToolLogAccess(t *testing.T) {
	s, dirs := pinsServer(t)
	if _, err := s.SummarizeToolLog(operatorCtx, &orchdv1.SummarizeToolLogRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("without a command log = %v, want Unavailable", err)
	}
	withToolLog(t, s)
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	for name, ctx := range map[string]context.Context{"session": inClaude, "task": queued} {
		if _, err := s.SummarizeToolLog(ctx, &orchdv1.SummarizeToolLogRequest{}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("%s caller = %v, want PermissionDenied", name, err)
		}
	}
	if operatorActions[orchdv1.Orchd_SummarizeToolLog_FullMethodName] == "" {
		t.Error("SummarizeToolLog is not operator-only")
	}
	for _, since := range []int64{-1, time.Now().Add(time.Hour).Unix()} {
		if _, err := s.SummarizeToolLog(operatorCtx, &orchdv1.SummarizeToolLogRequest{Since: since}); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("since %d = %v, want InvalidArgument", since, err)
		}
	}
	if _, err := s.PinSession(inClaude, pinReq("sess-x", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	logFixture(t, s, sessionOwner("sess-x"), time.Now(), callFixture("ls"))
	logFixture(t, s, sessionOwner("sess-x"), time.Now(), moveFixture("/a", "/b"))
	flushToolLog(t, s)
	rep, err := s.SummarizeToolLog(operatorCtx, &orchdv1.SummarizeToolLogRequest{})
	if err != nil || len(rep.GetSpaces()) != 1 || rep.GetSpaces()[0].GetSpace() != "" ||
		rep.GetSpaces()[0].GetDirectoryMoves() != 1 || rep.GetSpaces()[0].GetCrossSpaceMoves() != 0 {
		t.Errorf("summary with no policy = %v, %v", rep, err)
	}
}

// serveAsOperator serves s on a temporary socket that ORCH_SOCKET names,
// with every caller seen as the operator. Reason: the test may itself run
// under an agent harness, which orchd would rightly refuse; the CLI here
// stands for the operator's terminal.
func serveAsOperator(t *testing.T, s *server) {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "orch-logread-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	asOperator := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(peer.NewContext(ctx, &peer.Peer{AuthInfo: callerAuth{pid: 42}}), req)
	}
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(asOperator))
	orchdv1.RegisterOrchdServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	t.Setenv("ORCH_SOCKET", sock)
}

func orchCommand(t *testing.T, args ...string) string {
	t.Helper()
	root := cli.NewRootCmd(orchdclient.Dial)
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("orch %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	t.Logf("orch %s:\n%s", strings.Join(args, " "), out.String())
	return out.String()
}

// TestSpacesLogCommands runs 'orch spaces log <session>' and
// 'orch spaces log summary' against orchd with the summary's fixtures.
func TestSpacesLogCommands(t *testing.T) {
	s, code := summaryEnv(t)
	app := code + "/company1/app"
	logFixture(t, s, sessionOwner("sess-acme"), time.Now(), callFixture("git status"))
	logFixture(t, s, sessionOwner("sess-acme"), time.Now(), moveFixture(app, code+"/willesq"))
	flushToolLog(t, s)
	serveAsOperator(t, s)

	out := orchCommand(t, "spaces", "log", "sess-acme")
	for _, w := range []string{"pre-tool-use", "git status", app + " -> " + code + "/willesq"} {
		if !strings.Contains(out, w) {
			t.Errorf("log lacks %q", w)
		}
	}
	out = orchCommand(t, "spaces", "log", "summary")
	if !strings.Contains(out, "acme   1         1           1                1                   0") {
		t.Errorf("summary lacks acme's line:\n%s", out)
	}
}

// TestListToolEventsByteBudget is the audit's M1: a chain of large records
// comes back in pages under gRPC's 4 MiB limit, never one oversized reply.
func TestListToolEventsByteBudget(t *testing.T) {
	s, _ := pinsServer(t)
	withToolLog(t, s)
	o := sessionOwner("sess-big")
	big := strings.Repeat("t", maxTargetsJSON)
	const n = 300 // about 5 MiB of targets
	for i := range n {
		r := callFixture(fmt.Sprint("c", i))
		r.Targets = big
		logFixture(t, s, o, time.Now(), r)
	}
	var after int64
	pages, total := 0, 0
	for {
		rep, err := s.ListToolEvents(operatorCtx, &orchdv1.ListToolEventsRequest{Owner: "sess-big", AfterSeq: after})
		if err != nil {
			t.Fatal(err)
		}
		if size := proto.Size(rep); size > 4<<20 {
			t.Fatalf("page %d is %d bytes", pages, size)
		}
		pages++
		total += len(rep.GetEvents())
		after = rep.GetEvents()[len(rep.GetEvents())-1].GetSeq()
		if !rep.GetMore() {
			break
		}
	}
	if pages < 2 || total != n {
		t.Fatalf("%d records in %d pages, want %d in at least 2", total, pages, n)
	}
	// An agent's pages are smaller.
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-a", e.dirs[0])
	withToolLog(t, e.s)
	for i := range 40 {
		r := callFixture(fmt.Sprint("c", i))
		r.Targets = big
		logFixture(t, e.s, sessionOwner("sess-a"), time.Now(), r)
	}
	rep, err := e.s.ListToolEvents(e.session("claude", 400, "sess-a"), &orchdv1.ListToolEventsRequest{})
	if err != nil || !rep.GetMore() || proto.Size(rep) > agentToolEventsBytes+64<<10 {
		t.Fatalf("agent page: %d bytes, more %v, %v", proto.Size(rep), rep.GetMore(), err)
	}
}

// TestListToolEventsAfterClear is the audit's L1: a connection that resolved
// its pin before /clear no longer reads that session once the harness
// process has a newer one; a fresh connection reads the new session.
func TestListToolEventsAfterClear(t *testing.T) {
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-old", e.dirs[0])
	withToolLog(t, e.s)
	logFixture(t, e.s, sessionOwner("sess-old"), time.Now(), callFixture("before clear"))
	old := e.session("claude", 400, "sess-old")
	if cmds, err := listEvents(e.s, old, ""); err != nil || strings.Join(cmds, ",") != "before clear" {
		t.Fatalf("before /clear: %v, %v", cmds, err)
	}
	if _, err := e.s.EndSession(e.hook("claude", 400), &orchdv1.EndSessionRequest{SessionId: "sess-old"}); err != nil {
		t.Fatal(err)
	}
	e.pin(t, e.hook("claude", 400), "sess-new", e.dirs[0])
	logFixture(t, e.s, sessionOwner("sess-new"), time.Now(), callFixture("after clear"))
	for _, owner := range []string{"", "sess-old"} {
		if _, err := listEvents(e.s, old, owner); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("stale connection reading %q = %v, want PermissionDenied", owner, err)
		}
	}
	if cmds, err := listEvents(e.s, e.session("claude", 400, "sess-new"), ""); err != nil || strings.Join(cmds, ",") != "after clear" {
		t.Errorf("after /clear: %v, %v", cmds, err)
	}
	// Its events and the end record SessionEnd wrote.
	if cmds, err := listEvents(e.s, operatorCtx, "sess-old"); err != nil || len(cmds) != 2 || cmds[0] != "before clear" {
		t.Errorf("operator reading the cleared session: %v, %v", cmds, err)
	}
}

// TestListToolEventsOneReadPerChain is the second audit's M1: an agent
// reads its chain one page at a time and further calls are refused, not
// queued; the operator's reads never wait on agents'.
func TestListToolEventsOneReadPerChain(t *testing.T) {
	e := newLiveEnv(t)
	e.pin(t, e.hook("claude", 400), "sess-a", e.dirs[0])
	withToolLog(t, e.s)
	logFixture(t, e.s, sessionOwner("sess-a"), time.Now(), callFixture("ls"))
	inA := e.session("claude", 400, "sess-a")
	if !e.s.toolLog.startRead(sessionOwner("sess-a")) {
		t.Fatal("no read was running")
	}
	if _, err := listEvents(e.s, inA, ""); grpcstatus.Code(err) != codes.ResourceExhausted {
		t.Errorf("second agent read = %v, want ResourceExhausted", err)
	}
	if cmds, err := listEvents(e.s, operatorCtx, "sess-a"); err != nil || len(cmds) != 1 {
		t.Errorf("operator read during an agent's: %v, %v", cmds, err)
	}
	e.s.toolLog.endRead(sessionOwner("sess-a"))
	if cmds, err := listEvents(e.s, inA, ""); err != nil || len(cmds) != 1 {
		t.Errorf("agent read after the first ended: %v, %v", cmds, err)
	}
	if !e.s.toolLog.startRead(sessionOwner("sess-a")) {
		t.Error("a finished read kept its claim")
	}

	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.s.toolLog.flushShared(done); err == nil {
		t.Error("a cancelled flush succeeded")
	}
	if err := e.s.toolLog.flushShared(context.Background()); err != nil {
		t.Errorf("flush: %v", err)
	}
}

// TestSummaryPlacingDeadline is the second audit's L1: once placing runs
// out of time, the summary still answers and counts the rest as unplaced.
func TestSummaryPlacingDeadline(t *testing.T) {
	s, code := summaryEnv(t)
	logFixture(t, s, sessionOwner("sess-acme"), time.Now(), moveFixture(code+"/company1/app", code+"/willesq"))
	logFixture(t, s, sessionOwner("sess-me"), time.Now(), moveFixture(code+"/willesq", code+"/lib"))
	flushToolLog(t, s)
	defer func(d time.Duration) { summaryPlaceTime = d }(summaryPlaceTime)
	summaryPlaceTime = 0
	rep, err := s.SummarizeToolLog(operatorCtx, &orchdv1.SummarizeToolLogRequest{})
	if err != nil || rep.GetUnplacedMoves() != 2 || len(rep.GetSpaces()) != 2 {
		t.Fatalf("summary past the placing deadline = %v, %v", rep, err)
	}
}

// TestSummaryPlacesFromCache is the audit's L2: placing the directories
// sessions reported asks GitHub nothing, and stops at its deadline.
func TestSummaryPlacesFromCache(t *testing.T) {
	s, code, gh := placementServer(t)
	withToolLog(t, s)
	before := gh.calls
	for _, dir := range []string{code + "/company1/app", code + "/company1/fork", code + "/lib"} {
		if _, _, err := s.placeSession(withCachedRepoLookups(context.Background()), dir); err != nil {
			t.Fatal(err)
		}
	}
	if gh.calls != before {
		t.Errorf("cache-only placement called gh %d times", gh.calls-before)
	}
	sum := &toolSummary{s: s, rows: map[string]*orchdv1.SpaceToolActivity{}, placed: map[string]placedDir{}}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := sum.place(expired, code+"/lib"); ok {
		t.Error("placed a directory after the deadline")
	}
	if _, ok := sum.place(context.Background(), "relative/dir"); ok {
		t.Error("placed a relative directory")
	}
}
