package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/cli"
	"github.com/willesq/orchestrator/internal/orchdclient"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// withToolLog attaches a command log in a temporary directory to s, the way
// Run does, and returns its path.
func withToolLog(t *testing.T, s *server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toollog.db")
	l, err := openToolLog(path, s.resolveClaim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s.toolLog = l
	s.toolEvents.setSink(s.logToolEvent)
	return path
}

func flushToolLog(t *testing.T, s *server) {
	t.Helper()
	// Generous: the race detector slows reading targets many times over.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.toolLog.writer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func records(t *testing.T, s *server, o toollog.Owner) []toollog.Record {
	t.Helper()
	recs, err := s.toolLog.store.Records(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func sessionOwner(id string) toollog.Owner {
	return toollog.Owner{Kind: toollog.OwnerSession, ID: id}
}

// pinnedCtx is a hook call resolved to session id's pin.
func pinnedCtx(t *testing.T, s *server, id string) context.Context {
	t.Helper()
	p, ok, err := s.pins.get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("pin %s: %v %v", id, ok, err)
	}
	return context.WithValue(context.Background(), resolvedKey{}, resolvedCaller{kind: callerSession, pin: p})
}

func bashReq(session, cwd, cmd string) *orchdv1.RecordToolEventRequest {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return &orchdv1.RecordToolEventRequest{Event: "pre-tool-use", SessionId: session, Cwd: cwd,
		ToolName: "Bash", ToolInput: in, ToolInputSize: int64(len(in)), ToolUseId: "tu", At: time.Now().UnixNano()}
}

// TestToolEventsLoggedInOrder is SP-014.1.a1 end to end: events the hooks
// hand to RecordToolEvent are stored per session in the order they came,
// with the harness, targets and decision, in a 0600 file.
func TestToolEventsLoggedInOrder(t *testing.T) {
	s, dirs := pinsServer(t)
	path := withToolLog(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-a", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinSession(inCodex, pinReq("sess-b", dirs[1], "")); err != nil {
		t.Fatal(err)
	}
	ctxA, ctxB := pinnedCtx(t, s, "sess-a"), pinnedCtx(t, s, "sess-b")
	for i := range 20 {
		if _, err := s.RecordToolEvent(ctxA, bashReq("sess-a", dirs[0], fmt.Sprintf("cat %s/f%d", dirs[0], i))); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordToolEvent(ctxB, bashReq("sess-b", dirs[1], fmt.Sprintf("ls %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	cwd := &orchdv1.RecordToolEventRequest{Event: "cwd-changed", SessionId: "sess-a", OldCwd: dirs[0], NewCwd: dirs[1]}
	if _, err := s.RecordToolEvent(ctxA, cwd); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, s)

	a := records(t, s, sessionOwner("sess-a"))
	if len(a) != 21 {
		t.Fatalf("sess-a: %d records, want 21", len(a))
	}
	for i, r := range a[:20] {
		want := fmt.Sprintf("cat %s/f%d", dirs[0], i)
		var tg spaces.ToolTargets
		if err := json.Unmarshal([]byte(r.Targets), &tg); err != nil {
			t.Fatalf("record %d targets %q: %v", i, r.Targets, err)
		}
		if r.Seq != int64(i+1) || r.Command != want || r.Harness != "claude" || r.Tool != "Bash" ||
			r.Decision != decisionLogged || r.Cwd != dirs[0] || !slices.Contains(tg.Paths, dirs[0]+fmt.Sprintf("/f%d", i)) {
			t.Errorf("record %d = %+v", i, r)
		}
	}
	if last := a[20]; last.Event != "cwd-changed" || last.OldCwd != dirs[0] || last.NewCwd != dirs[1] || last.Targets != "" {
		t.Errorf("cwd-changed record = %+v", last)
	}
	b := records(t, s, sessionOwner("sess-b"))
	if len(b) != 20 || b[0].Harness != "codex" || b[19].Command != "ls 19" {
		t.Errorf("sess-b: %d records, first %+v", len(b), b[0])
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("toollog.db mode = %v, %v; want 600", fi.Mode().Perm(), err)
	}
}

// TestToolLogAttribution: a session's event goes to the session the hook
// names only when that session is pinned to the same harness process; a
// task's to its task; and an event orchd cannot attribute is not stored.
func TestToolLogAttribution(t *testing.T) {
	s, dirs := pinsServer(t)
	path := withToolLog(t, s)
	for _, p := range []struct {
		ctx context.Context
		id  string
		dir string
	}{{inClaude, "sess-1", dirs[0]}, {inClaude, "sess-cleared", dirs[0]}, {inCodex, "sess-other", dirs[1]}} {
		if _, err := s.PinSession(p.ctx, pinReq(p.id, p.dir, "")); err != nil {
			t.Fatal(err)
		}
	}
	// The claude process's newest pin is sess-cleared (after /clear).
	cleared := pinnedCtx(t, s, "sess-cleared")
	send := func(ctx context.Context, claim, cmd string) {
		t.Helper()
		if _, err := s.RecordToolEvent(ctx, bashReq(claim, dirs[0], cmd)); err != nil {
			t.Fatal(err)
		}
	}
	send(cleared, "sess-1", "same process, earlier session")
	send(cleared, "sess-other", "another process's session")
	send(cleared, "sess-unknown", "unpinned claim")
	send(cleared, "", "no claim")

	task := context.WithValue(context.Background(), resolvedKey{},
		resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-7", Provider: "codex"}})
	send(task, "sess-1", "task call")
	untracked := context.WithValue(context.Background(), resolvedKey{}, resolvedCaller{kind: callerTask})
	send(untracked, "sess-1", "task orchd cannot find")
	send(context.WithValue(context.Background(), resolvedKey{}, resolvedCaller{kind: callerOperator}), "sess-1", "operator")
	send(context.Background(), "sess-1", "unresolved")
	flushToolLog(t, s)

	cmds := func(o toollog.Owner) []string {
		var out []string
		for _, r := range records(t, s, o) {
			out = append(out, r.Command+" <- "+r.ClaimedSession)
		}
		return out
	}
	tests := []struct {
		owner toollog.Owner
		want  []string
	}{
		{sessionOwner("sess-1"), []string{"same process, earlier session <- sess-1"}},
		{sessionOwner("sess-cleared"), []string{"another process's session <- sess-other", "unpinned claim <- sess-unknown", "no claim <- "}},
		{sessionOwner("sess-other"), nil},
		{toollog.Owner{Kind: toollog.OwnerTask, ID: "task-7"}, []string{"task call <- sess-1"}},
	}
	for _, tt := range tests {
		if got := cmds(tt.owner); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %q, want %q", tt.owner, got, tt.want)
		}
	}
	if r := records(t, s, toollog.Owner{Kind: toollog.OwnerTask, ID: "task-7"}); len(r) != 1 || r[0].Harness != "codex" {
		t.Errorf("task record = %+v", r)
	}
	var total int
	if err := withFile(path, func(db *sql.DB) error {
		return db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&total)
	}); err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("%d records stored in all, want 5: operator, unresolved and untracked task events must not be", total)
	}
}

// TestToolRecordFields: credentials in the input are redacted, an input too
// large to send is noted, and the applied policy revision is recorded.
func TestToolRecordFields(t *testing.T) {
	s := spacesServer(t)
	pin := sessionPin{SessionID: "sess-f", Harness: "claude"}
	ev := func(req *orchdv1.RecordToolEventRequest) toolEvent {
		return toolEvent{caller: resolvedCaller{kind: callerSession, pin: pin}, req: req, received: time.Now()}
	}
	r := s.toolRecord(ev(bashReq("sess-f", "/w", "GH_TOKEN=ghp_R4nd0mT0kenValue1234567890abcdEFGH gh pr list")), "claude")
	if strings.Contains(r.Command, "ghp_") || !strings.Contains(r.Command, "[REDACTED:github-token]") {
		t.Errorf("command = %q", r.Command)
	}
	big := &orchdv1.RecordToolEventRequest{Event: "pre-tool-use", ToolName: "Write", ToolInputSize: 5 << 20, Cwd: "/w"}
	if r := s.toolRecord(ev(big), "claude"); r.Command != "[tool input of 5242880 bytes was not sent]" || r.InputSize != 5<<20 {
		t.Errorf("unsent input record = %+v", r)
	}
	long := &orchdv1.RecordToolEventRequest{Event: "cwd-changed", Cwd: strings.Repeat("d", 10000)}
	if r := s.toolRecord(ev(long), "claude"); len(r.Cwd) > toollog.MaxField {
		t.Errorf("cwd stored at %d bytes, over the %d cap", len(r.Cwd), toollog.MaxField)
	}
	d := draft(t, s, `{"spaces":[{"name":"personal","kind":"private"}]}`)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	if r := s.toolRecord(ev(bashReq("sess-f", "/w", "ls")), "claude"); r.PolicyRev != d.GetRevision() {
		t.Errorf("policy_rev = %d, want %d", r.PolicyRev, d.GetRevision())
	}
}

func TestTargetsJSONTooLarge(t *testing.T) {
	var many spaces.ToolTargets
	for i := range 2000 {
		many.Paths = append(many.Paths, fmt.Sprintf("/some/long/path/number/%d", i))
	}
	got := targetsJSON(many)
	var tg spaces.ToolTargets
	if err := json.Unmarshal([]byte(got), &tg); err != nil || !tg.Opaque || len(tg.Paths) != 0 ||
		!strings.Contains(tg.OpaqueReasons[0], "were not stored") {
		t.Errorf("targets = %s (%v)", got, err)
	}
}

// TestEndSessionAnchorsToolLog: SessionEnd writes an end record after the
// session's events and anchors the chain head.
func TestEndSessionAnchorsToolLog(t *testing.T) {
	s, dirs := pinsServer(t)
	withToolLog(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-end", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	ctx := pinnedCtx(t, s, "sess-end")
	for i := range 3 {
		if _, err := s.RecordToolEvent(ctx, bashReq("sess-end", dirs[0], fmt.Sprint("echo ", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "sess-end"}); err != nil {
		t.Fatal(err)
	}
	var rep *orchdv1.VerifyToolLogResponse
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var err error
		if rep, err = s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "sess-end"}); err != nil {
			t.Fatal(err)
		}
		if rep.GetAnchors() == 1 {
			break
		}
	}
	if rep.GetRecords() != 4 || rep.GetEnds() != 1 || rep.GetAnchors() != 1 || len(rep.GetProblems()) != 0 {
		t.Errorf("after SessionEnd: %v", rep)
	}
	if recs := records(t, s, sessionOwner("sess-end")); recs[3].Kind != toollog.KindEnd {
		t.Errorf("last record = %+v, want the end record", recs[3])
	}
	// Without a command log SessionEnd still works.
	s.toolLog = nil
	if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "sess-end"}); err != nil {
		t.Errorf("EndSession without a command log = %v", err)
	}
}

// TestVerifyToolLogRPC is SP-014.2.a1 through orchd: VerifyToolLog reports
// a modified, a deleted and a reordered record, and only the operator may
// call it.
func TestVerifyToolLogRPC(t *testing.T) {
	tampers := map[string]string{
		"modified":  `UPDATE events SET command = 'something else' WHERE seq = 2`,
		"deleted":   `DELETE FROM events WHERE seq = 2`,
		"reordered": `UPDATE events SET seq = -seq WHERE seq IN (1, 3); UPDATE events SET seq = 3 WHERE seq = -1; UPDATE events SET seq = 1 WHERE seq = -3`,
	}
	for name, tamper := range tampers {
		t.Run(name, func(t *testing.T) {
			s := spacesServer(t)
			path := withToolLog(t, s)
			ctx := context.WithValue(context.Background(), resolvedKey{},
				resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-v", Harness: "claude"}})
			for i := range 4 {
				if _, err := s.RecordToolEvent(ctx, bashReq("sess-v", "/w", fmt.Sprint("echo ", i))); err != nil {
					t.Fatal(err)
				}
			}
			rep, err := s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "sess-v"})
			if err != nil || rep.GetRecords() != 4 || len(rep.GetProblems()) != 0 {
				t.Fatalf("intact chain: %v, %v", rep, err)
			}
			if err := tamperFile(path, tamper); err != nil {
				t.Fatal(err)
			}
			rep, err = s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "session:sess-v"})
			if err != nil || len(rep.GetProblems()) == 0 {
				t.Errorf("%s record not detected: %v, %v", name, rep, err)
			}
		})
	}

	s := spacesServer(t)
	if _, err := s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "x"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("without a command log = %v, want Unavailable", err)
	}
	withToolLog(t, s)
	if _, err := s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "task:../x"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("bad owner = %v, want InvalidArgument", err)
	}
	queued := metadata.NewIncomingContext(operatorCtx, metadata.Pairs(orchdclient.TaskMetadataKey, "task-1"))
	for name, ctx := range map[string]context.Context{"session": inClaude, "task": queued} {
		if _, err := s.VerifyToolLog(ctx, &orchdv1.VerifyToolLogRequest{Owner: "x"}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("%s caller = %v, want PermissionDenied", name, err)
		}
	}
}

// withFile opens the command log file directly, the way someone holding the
// file would.
func withFile(path string, fn func(*sql.DB) error) error {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(db)
}

func tamperFile(path, stmts string) error {
	return withFile(path, func(db *sql.DB) error {
		_, err := db.Exec(stmts)
		return err
	})
}

// TestToolLogThroughput is SP-014.2.a2: events per second from
// RecordToolEvent until they are stored, with each call's targets read.
func TestToolLogThroughput(t *testing.T) {
	s := spacesServer(t)
	withToolLog(t, s)
	dir := t.TempDir()
	// Four harness processes, since one may hold only a quarter of the queue.
	var ctxs [4]context.Context
	for i := range ctxs {
		ctxs[i] = context.WithValue(context.Background(), resolvedKey{},
			resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: fmt.Sprint("sess-t", i), Harness: "claude",
				Process: procstat.Identity{PID: 1000 + i, Started: claudeStart}}})
	}
	const n = 3000
	start := time.Now()
	for i := range n {
		cmd := fmt.Sprintf("cd %s && git -C sub%d status --short && cat notes/%d.md | grep -n TODO", dir, i%20, i)
		if _, err := s.RecordToolEvent(ctxs[i%4], bashReq(fmt.Sprint("sess-t", i%4), dir, cmd)); err != nil {
			t.Fatal(err)
		}
	}
	flushToolLog(t, s)
	rate := float64(n) / time.Since(start).Seconds()
	stored := 0
	for i := range ctxs {
		stored += len(records(t, s, sessionOwner(fmt.Sprint("sess-t", i))))
	}
	t.Logf("command log throughput: %.0f events/s (%d events stored)", rate, stored)
	if rate < 1000 && !raceEnabled {
		t.Errorf("throughput %.0f events/s, want >= 1000", rate)
	}
	// The queue holds every event sent, so none may be dropped.
	if stored != n || s.toolLog.writer.Dropped() != 0 {
		t.Errorf("stored %d of %d events, %d dropped", stored, n, s.toolLog.writer.Dropped())
	}
}

// TestSpacesLogVerifyCommand is SP-014.2.a1 end to end: 'orch spaces log
// verify <session>' run against orchd detects a modified, a deleted and a
// reordered record, and passes an untouched chain.
func TestSpacesLogVerifyCommand(t *testing.T) {
	s := spacesServer(t)
	path := withToolLog(t, s)
	sessCtx := func(id string) context.Context {
		return context.WithValue(context.Background(), resolvedKey{},
			resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: id, Harness: "claude"}})
	}
	cases := []struct{ session, tamper string }{
		{"sess-intact", ""},
		{"sess-modified", `UPDATE events SET command = 'curl evil' WHERE owner_id = 'sess-modified' AND seq = 2`},
		{"sess-deleted", `DELETE FROM events WHERE owner_id = 'sess-deleted' AND seq = 2`},
		{"sess-reordered", `UPDATE events SET seq = -seq WHERE owner_id = 'sess-reordered' AND seq IN (1, 3);
			UPDATE events SET seq = 3 WHERE owner_id = 'sess-reordered' AND seq = -1;
			UPDATE events SET seq = 1 WHERE owner_id = 'sess-reordered' AND seq = -3`},
	}
	for _, c := range cases {
		for i := range 4 {
			if _, err := s.RecordToolEvent(sessCtx(c.session), bashReq(c.session, "/w", fmt.Sprint("echo ", i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	flushToolLog(t, s)
	for _, c := range cases {
		if c.tamper != "" {
			if err := tamperFile(path, c.tamper); err != nil {
				t.Fatal(err)
			}
		}
	}

	dir, err := os.MkdirTemp(os.TempDir(), "orch-logverify-")
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
	// would rightly refuse; the CLI here stands for the operator's terminal.
	asOperator := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(peer.NewContext(ctx, &peer.Peer{AuthInfo: callerAuth{pid: 42}}), req)
	}
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(asOperator))
	orchdv1.RegisterOrchdServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	t.Setenv("ORCH_SOCKET", sock)

	for _, c := range cases {
		root := cli.NewRootCmd(orchdclient.Dial)
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"spaces", "log", "verify", c.session})
		err := root.Execute()
		switch {
		case c.tamper == "" && (err != nil || !strings.Contains(out.String(), "chain intact")):
			t.Errorf("%s: %v\n%s", c.session, err, out.String())
		case c.tamper != "" && (err == nil || !strings.Contains(out.String(), "chain broken")):
			t.Errorf("%s: tampering not detected: %v\n%s", c.session, err, out.String())
		}
		t.Logf("orch spaces log verify %s:\n%s", c.session, out.String())
	}
}

// TestRecordToolEventFieldCap is the audit's H1: a field other than
// tool_input over 4 KiB is refused before anything is queued.
func TestRecordToolEventFieldCap(t *testing.T) {
	s := spacesServer(t)
	withToolLog(t, s)
	ctx := context.WithValue(context.Background(), resolvedKey{},
		resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-cap", Harness: "claude"}})
	long := strings.Repeat("a", maxToolEventField+1)
	for name, req := range map[string]*orchdv1.RecordToolEventRequest{
		"cwd":        {Event: "pre-tool-use", Cwd: long},
		"tool_name":  {Event: "pre-tool-use", ToolName: long},
		"new_cwd":    {Event: "cwd-changed", NewCwd: long},
		"agent_type": {Event: "pre-tool-use", AgentType: long},
	} {
		if _, err := s.RecordToolEvent(ctx, req); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("%s over 4 KiB = %v, want InvalidArgument", name, err)
		}
	}
	ok := &orchdv1.RecordToolEventRequest{Event: "pre-tool-use", Cwd: strings.Repeat("a", maxToolEventField)}
	if _, err := s.RecordToolEvent(ctx, ok); err != nil {
		t.Errorf("cwd of exactly 4 KiB = %v", err)
	}
	flushToolLog(t, s)
	if n := len(records(t, s, sessionOwner("sess-cap"))); n != 1 {
		t.Errorf("%d records stored, want 1", n)
	}
}

// TestRepeatedEndSessionAnchorsOnce is the audit's M3: repeated SessionEnds
// write one end record.
func TestRepeatedEndSessionAnchorsOnce(t *testing.T) {
	s, dirs := pinsServer(t)
	withToolLog(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-once", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordToolEvent(pinnedCtx(t, s, "sess-once"), bashReq("sess-once", dirs[0], "ls")); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := s.EndSession(inClaude, &orchdv1.EndSessionRequest{SessionId: "sess-once"}); err != nil {
			t.Fatal(err)
		}
	}
	var rep *orchdv1.VerifyToolLogResponse
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var err error
		if rep, err = s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "sess-once"}); err != nil {
			t.Fatal(err)
		}
		if rep.GetEnds() > 0 {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	rep, err := s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "sess-once"})
	if err != nil || rep.GetEnds() != 1 || rep.GetAnchors() != 1 {
		t.Errorf("after 5 SessionEnds: %v, %v; want one end record", rep, err)
	}
}

// TestRecordToolEventUnknownFields is the round-2 audit's H1: a request
// carrying fields this orchd does not know is refused, since they would be
// held in memory without being counted.
func TestRecordToolEventUnknownFields(t *testing.T) {
	s := spacesServer(t)
	withToolLog(t, s)
	ctx := context.WithValue(context.Background(), resolvedKey{},
		resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: "sess-u", Harness: "claude"}})
	req := bashReq("sess-u", "/w", "ls")
	// Field 99, length-delimited, 1 KiB of padding.
	req.ProtoReflect().SetUnknown(append([]byte{0x9a, 0x06, 0x80, 0x08}, make([]byte, 1024)...))
	if _, err := s.RecordToolEvent(ctx, req); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown fields = %v, want InvalidArgument", err)
	}
	if n := eventSize(bashReq("sess-u", "/w", strings.Repeat("x", 4000))); n < 4000 {
		t.Errorf("eventSize = %d, want at least the request's size", n)
	}
}

// TestToolLogQuotaPerProcess is the round-2 audit's M-b: every session of
// one harness process shares the process's quota key, and tasks have their
// own.
func TestToolLogQuotaPerProcess(t *testing.T) {
	s := spacesServer(t)
	proc := procstat.Identity{PID: 400, Started: claudeStart}
	ev := func(id string) toolEvent {
		return toolEvent{caller: resolvedCaller{kind: callerSession, pin: sessionPin{SessionID: id, Harness: "claude", Process: proc}},
			req: bashReq(id, "/w", "ls")}
	}
	a, _, _ := s.toolLogKey(ev("sess-a"))
	b, _, _ := s.toolLogKey(ev("sess-b"))
	if a.Quota != b.Quota || a.Owner == b.Owner {
		t.Errorf("keys %+v and %+v: want one quota, two chains", a, b)
	}
	task := toolEvent{caller: resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-1"}}, req: bashReq("", "/w", "ls")}
	if k, _, ok := s.toolLogKey(task); !ok || k.Quota != "task:task-1" || k.Claim != "" {
		t.Errorf("task key = %+v, %v", k, ok)
	}
}
