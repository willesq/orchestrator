package daemon

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// TestUnpinnedProcessChain is SP-073.2.a1: a harness process holding no pin
// logs into a chain keyed by its PID and start time; the operator sees it in
// the command log and the session list; a session pinned later in that
// process cannot read it; once the process exits its chain is ended and
// anchored, and retention expires it.
func TestUnpinnedProcessChain(t *testing.T) {
	e := newLiveEnv(t)
	withCopyingToolLog(t, e.s)
	ctx := context.Background()
	started := e.base.Add(-2 * time.Minute)
	e.tbl.started[501], e.tbl.cwd[501] = started, e.dirs[0]
	proc := procstat.Identity{PID: 501, Started: started}
	o := toollog.Owner{Kind: toollog.OwnerProcess, ID: toollog.ProcessID(501, started.UnixNano())}
	e.s.pins.procs = func() ([]procstat.Process, error) {
		if _, ok := e.tbl.started[501]; !ok {
			return nil, nil
		}
		return []procstat.Process{{Identity: proc, Name: "claude"}}, nil
	}

	// Caller resolution keeps the unpinned harness the hook runs in.
	resolver := &callerResolver{pins: e.s.pins}
	unpinned := resolver.resolve(hookIn("claude", proc))
	if unpinned.kind != callerNone || unpinned.harness != "claude" || !unpinned.harnessProc.Same(proc) {
		t.Fatalf("unpinned hook resolved to %+v", unpinned)
	}
	hookCtx := context.WithValue(ctx, resolvedKey{}, unpinned)
	for _, cmd := range []string{"ls", "make"} {
		if _, err := e.s.RecordToolEvent(hookCtx, bashReq("sess-unknown", e.dirs[0], cmd)); err != nil {
			t.Fatal(err)
		}
	}
	// A caller under no harness at all is still not logged.
	if _, err := e.s.RecordToolEvent(context.WithValue(ctx, resolvedKey{}, none("x")), bashReq("", "/", "id")); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, e.s)
	recs := records(t, e.s, o)
	if len(recs) != 2 || recs[0].Harness != "claude" || recs[1].Command != "make" || recs[0].ClaimedSession != "sess-unknown" {
		t.Fatalf("process chain = %+v", recs)
	}

	// The operator reads it by PID or by its full ID, and sees it listed.
	for _, owner := range []string{"process:501", o.String()} {
		if cmds, err := listEvents(e.s, operatorCtx, owner); err != nil || !slices.Equal(cmds, []string{"ls", "make"}) {
			t.Fatalf("operator reading %s = %v, %v", owner, cmds, err)
		}
	}
	if _, err := listEvents(e.s, operatorCtx, "process:502"); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("a PID with no chain = %v, want NotFound", err)
	}
	if rep, err := e.s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: "process:501"}); err != nil || rep.GetRecords() != 2 {
		t.Fatalf("verify by PID = %v, %v", rep, err)
	}
	row := listSessions(t, e.s, operatorCtx, false)["pid:501"]
	if row.GetState() != stateUnpinned || row.GetToolEvents() != 2 || row.GetHarness() != "claude" || row.GetLastActivity() == 0 {
		t.Fatalf("unpinned row = %v", row)
	}

	// A session pinned later in the process reads its own log, never the
	// process's; its events go to its own chain.
	e.pin(t, e.hook("claude", 501), "sess-later", e.dirs[0])
	later := e.session("claude", 501, "sess-later")
	if _, err := e.s.RecordToolEvent(later, bashReq("sess-later", e.dirs[0], "pwd")); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, e.s)
	for _, owner := range []string{"process:501", o.String()} {
		if _, err := listEvents(e.s, later, owner); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Fatalf("the later session reading %s = %v, want PermissionDenied", owner, err)
		}
	}
	if cmds, err := listEvents(e.s, later, ""); err != nil || !slices.Equal(cmds, []string{"pwd"}) {
		t.Fatalf("the later session's own log = %v, %v", cmds, err)
	}
	if n := len(records(t, e.s, o)); n != 2 {
		t.Fatalf("the process chain grew to %d after the pin", n)
	}
	// With --all its earlier chain is still listed for the operator.
	if row := listSessions(t, e.s, operatorCtx, true)["pid:501"]; row.GetToolEvents() != 2 || row.GetState() != stateUnpinned {
		t.Fatalf("--all row of the pinned process's chain = %v", row)
	}

	// The liveness sweep (ListSessions ran one) ended and anchored the
	// chain once the process was pinned, in orch.db too, freeing its place.
	if rep := waitAnchors(t, e.s, o, 1); rep.Ends != 1 || !rep.OK() || rep.Records != 3 {
		t.Fatalf("after the pin: %+v", rep)
	}
	e.s.toolLog.procMu.Lock()
	held := e.s.toolLog.procs[o]
	e.s.toolLog.procMu.Unlock()
	if held {
		t.Fatal("a pinned process still holds a process chain's place")
	}
	// While the process runs, retention keeps the chain.
	far := time.Now().Add(toollog.DefaultRetention + time.Hour)
	e.s.sweepToolLog(ctx, far)
	if _, n := counts(t, e.s, o); n != 3 {
		t.Fatalf("a running process's chain lost records: %d left", n)
	}
	// Once it exits nothing more is written and retention may expire it.
	delete(e.tbl.started, 501)
	e.s.sweepSessions(ctx)
	if rep := waitAnchors(t, e.s, o, 1); rep.Ends != 1 || !rep.OK() || rep.Anchors != 1 {
		t.Fatalf("after the exit: %+v", rep)
	}
	if c := copiesOf(t, e.s, o); len(c) != 1 || c[0].Seq != 3 {
		t.Fatalf("orch.db copies = %+v", c)
	}
	if st, err := e.s.chainState(ctx, o); err != nil || st.Live {
		t.Fatalf("chain state after the exit = %+v, %v", st, err)
	}
	row = listSessions(t, e.s, operatorCtx, true)["pid:501"]
	if row.GetState() != stateEnded || row.GetToolEvents() != 2 {
		t.Fatalf("--all row after the exit = %v", row)
	}
	if _, listed := listSessions(t, e.s, operatorCtx, false)["pid:501"]; listed {
		t.Fatal("an exited process's chain is listed without --all")
	}
	// Another sweep adds nothing; retention then expires the chain.
	e.s.sweepSessions(ctx)
	e.s.sweepToolLog(ctx, far)
	if _, n := counts(t, e.s, o); n != 0 {
		t.Fatalf("an exited process's chain outlived retention: %d left", n)
	}
	// An unparsable process ID counts as ended.
	if st := e.s.processState(toollog.Owner{Kind: toollog.OwnerProcess, ID: "501"}); st.Live {
		t.Fatal("a process chain with no start time runs")
	}
}

// TestSessionCapSetting is SP-073.2.a2: sessions.command_log.max_session_mb
// (default 50, sensitive, 10 to 1000) sets the per-session cap retention
// applies to a running session; a value outside the range keeps 50.
func TestSessionCapSetting(t *testing.T) {
	f, ok := config.FieldFor("sessions.command_log.max_session_mb")
	if !ok || !f.Sensitive || !f.HasRange || f.Min != minSessionMB || f.Max != maxSessionMB || f.Type != config.TypeInt {
		t.Fatalf("schema field = %+v, %v", f, ok)
	}
	d, err := config.Unmarshal(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if d.Sessions.CommandLog.MaxSessionMB != 50 || logPolicy(d.Sessions.CommandLog).SessionCap != 50<<20 {
		t.Fatalf("default = %d MB", d.Sessions.CommandLog.MaxSessionMB)
	}
	for mb, want := range map[int]int64{0: 50, 10: 10, 200: 200, 1000: 1000, 9: 50, 1001: 50, -5: 50} {
		if got := logPolicy(config.CommandLogSettings{MaxSessionMB: mb}).SessionCap; got != want<<20 {
			t.Errorf("max_session_mb %d gives a cap of %d MB, want %d", mb, got>>20, want)
		}
	}

	// Retention cuts a running session to the configured cap.
	e := newLiveEnv(t)
	withToolLog(t, e.s)
	e.pin(t, e.hook("claude", 400), "sess-big", e.dirs[0])
	o := sessionOwner("sess-big")
	big := strings.Repeat("t", 16<<10)
	for i := range 800 { // about 12.8 MB
		e.s.toolLog.writer.Enqueue(toollog.Key{Quota: "q", Owner: o}, 100, func() toollog.Record {
			return toollog.Record{TS: time.Now().UnixNano() + int64(i), Event: "pre-tool-use", Tool: "Bash", Targets: big}
		})
	}
	flushToolLog(t, e.s)
	e.s.toolLog.policy = logPolicy(config.CommandLogSettings{MaxSessionMB: 50})
	e.s.sweepToolLog(context.Background(), time.Now())
	if _, n := counts(t, e.s, o); n != 800 {
		t.Fatalf("under the 50 MB cap %d of 800 records are left", n)
	}
	e.s.toolLog.policy = logPolicy(config.CommandLogSettings{MaxSessionMB: 10})
	e.s.sweepToolLog(context.Background(), time.Now())
	_, n := counts(t, e.s, o)
	if n == 800 || n*(16<<10) > 10<<20 || n < 500 {
		t.Fatalf("with a 10 MB cap %d records are left; want about 10 MB of them", n)
	}
	if rep, err := e.s.toolLog.store.Verify(context.Background(), o); err != nil || !rep.OK() {
		t.Fatalf("the cut chain: %+v, %v", rep, err)
	}
}

// TestProcessChainCap: at most maxProcessChains unpinned harness processes
// have a chain open at once, so processes named like a harness cannot grow
// the log without bound (the SP-073 audit's second finding); a process that
// exits frees its place, and the open chains are counted again at open.
func TestProcessChainCap(t *testing.T) {
	e := newLiveEnv(t)
	path := withToolLog(t, e.s)
	ctx := context.Background()
	call := func(pid int) {
		t.Helper()
		proc := procstat.Identity{PID: pid, Started: e.tbl.started[pid]}
		c := resolvedCaller{kind: callerNone, harness: "codex", harnessProc: proc}
		if _, err := e.s.RecordToolEvent(context.WithValue(ctx, resolvedKey{}, c), bashReq("", "/", "ls")); err != nil {
			t.Fatal(err)
		}
	}
	owner := func(pid int) toollog.Owner {
		return toollog.Owner{Kind: toollog.OwnerProcess, ID: toollog.ProcessID(pid, e.tbl.started[pid].UnixNano())}
	}
	for i := range maxProcessChains + 1 {
		e.tbl.started[600+i] = e.base.Add(-time.Hour)
		call(600 + i)
	}
	call(600) // a process with a chain open keeps logging
	flushToolLog(t, e.s)
	if n := len(records(t, e.s, owner(600))); n != 2 {
		t.Fatalf("the first process logged %d events, want 2", n)
	}
	over := 600 + maxProcessChains
	if n := len(records(t, e.s, owner(over))); n != 0 {
		t.Fatalf("a process past the cap logged %d events", n)
	}
	// A process exits; its place goes to the next.
	exited := owner(601)
	delete(e.tbl.started, 601)
	e.s.sweepSessions(ctx)
	waitAnchors(t, e.s, exited, 1)
	call(over)
	flushToolLog(t, e.s)
	if n := len(records(t, e.s, owner(over))); n != 1 {
		t.Fatalf("after an exit the waiting process logged %d events, want 1", n)
	}
	// Reopened, orchd counts the chains still open.
	_ = e.s.toolLog.Close()
	l, err := openToolLog(path, e.s.resolveClaim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if n := len(l.procs); n != maxProcessChains {
		t.Fatalf("reopened with %d open process chains, want %d", n, maxProcessChains)
	}
}
