package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/toollog"
)

// refuse stores a refused call in o's chain, as the enforcing guard (SP-041)
// will.
func refuse(t *testing.T, s *server, o toollog.Owner) {
	t.Helper()
	s.toolLog.writer.Enqueue(toollog.Key{Quota: o.String(), Owner: o}, 100, func() toollog.Record {
		return toollog.Record{TS: time.Now().UnixNano(), Event: "pre-tool-use", Tool: "Bash",
			Command: "cat /other/secret", Decision: toollog.DecisionRefused}
	})
	flushToolLog(t, s)
}

// counts is o's stored records: refusals and all others.
func counts(t *testing.T, s *server, o toollog.Owner) (refused, other int) {
	t.Helper()
	for _, r := range records(t, s, o) {
		if r.Decision == toollog.DecisionRefused {
			refused++
		} else {
			other++
		}
	}
	return
}

// endedAt is when retention counts o as ended: the latest of its pin's end,
// when the liveness sweep saw its process gone, and its last record (the
// end record orchd writes just after the end).
func endedAt(t *testing.T, s *server, o toollog.Owner) time.Time {
	t.Helper()
	p, ok, err := s.pins.get(context.Background(), o.ID)
	if err != nil || !ok || p.EndedAt.IsZero() {
		t.Fatalf("pin %s = %+v, %v, %v; want ended", o.ID, p, ok, err)
	}
	end := p.EndedAt
	if p.ExitedAt.After(end) {
		end = p.ExitedAt
	}
	recs := records(t, s, o)
	if last := time.Unix(0, recs[len(recs)-1].TS); last.After(end) {
		end = last
	}
	return end
}

// TestLogRetentionThroughSessions is SP-015.1.a1 end to end, with the
// default 7 days, a settable clock and a fake process table: a session
// ended by SessionEnd (its process exits and the liveness sweep sees it 30 s
// later) loses its log at day 7, not before; one whose process exited without SessionEnd at day 7 after the
// liveness sweep noticed; one idle but alive for 14 days keeps everything;
// refusals stay to day 90.
func TestLogRetentionThroughSessions(t *testing.T) {
	e := newLiveEnv(t)
	withToolLog(t, e.s)
	e.tbl.started[420], e.tbl.cwd[420] = e.base.Add(-time.Minute), e.dirs[0]
	ended, exited, idle := sessionOwner("sess-ended"), sessionOwner("sess-exited"), sessionOwner("sess-idle")
	e.pin(t, e.hook("claude", 400), ended.ID, e.dirs[0])
	e.pin(t, e.hook("codex", 410), exited.ID, e.dirs[1])
	e.pin(t, e.hook("claude", 420), idle.ID, e.dirs[0])
	for _, c := range []struct {
		harness string
		pid     int
		id      string
		dir     string
	}{{"claude", 400, ended.ID, e.dirs[0]}, {"codex", 410, exited.ID, e.dirs[1]}, {"claude", 420, idle.ID, e.dirs[0]}} {
		for range 3 {
			if _, err := e.s.RecordToolEvent(e.session(c.harness, c.pid, c.id), bashReq(c.id, c.dir, "ls")); err != nil {
				t.Fatal(err)
			}
		}
	}
	flushToolLog(t, e.s)
	refuse(t, e.s, ended)

	// sess-ended: SessionEnd, then its process exits.
	e.clock.set(time.Now())
	if _, err := e.s.EndSession(e.hook("claude", 400), &orchdv1.EndSessionRequest{SessionId: ended.ID}); err != nil {
		t.Fatal(err)
	}
	waitForEndRecord(t, e.s, ended.ID)
	delete(e.tbl.started, 400)
	e.clock.set(e.clock.now().Add(30 * time.Second))
	e.s.sweepSessions(context.Background())
	// sess-exited: codex 410 exits with no SessionEnd; the sweep notices
	// two days later.
	delete(e.tbl.started, 410)
	detected := e.base.Add(2 * 24 * time.Hour)
	e.clock.set(detected)
	e.s.sweepSessions(context.Background())
	waitForEndRecord(t, e.s, exited.ID)

	retention := toollog.DefaultRetention
	endEnded, endExited := endedAt(t, e.s, ended), endedAt(t, e.s, exited)
	if p, _, _ := e.s.pins.get(context.Background(), ended.ID); endEnded.Sub(p.EndedAt) > time.Minute {
		t.Fatalf("sess-ended counts from %v, more than a minute after its SessionEnd at %v", endEnded, p.EndedAt)
	}
	if endExited.Sub(detected) > time.Minute {
		t.Fatalf("sess-exited counts from %v, want the detection at %v", endExited, detected)
	}
	sweepAt := func(at time.Time) { e.s.sweepToolLog(context.Background(), at) }

	sweepAt(endEnded.Add(retention - time.Second))
	if r, o := counts(t, e.s, ended); r != 1 || o != 4 {
		t.Fatalf("before day 7 sess-ended keeps %d refused, %d other; want 1, 4", r, o)
	}
	sweepAt(endEnded.Add(retention))
	if r, o := counts(t, e.s, ended); r != 1 || o != 0 {
		t.Fatalf("at day 7 sess-ended keeps %d refused, %d other; want only the refusal", r, o)
	}
	if _, o := counts(t, e.s, exited); o != 4 {
		t.Fatalf("sess-exited lost records %v after it ended, before 7 days after detection", endEnded.Add(retention).Sub(endExited))
	}
	sweepAt(endExited.Add(retention - time.Second))
	if _, o := counts(t, e.s, exited); o != 4 {
		t.Fatalf("sess-exited lost records a second before day 7 after detection")
	}
	sweepAt(endExited.Add(retention))
	if _, o := counts(t, e.s, exited); o != 0 {
		t.Fatalf("sess-exited keeps %d records 7 days after detection", o)
	}
	sweepAt(e.base.Add(14 * 24 * time.Hour))
	if r, o := counts(t, e.s, idle); r != 0 || o != 3 {
		t.Fatalf("idle session alive for 14 days keeps %d refused, %d other; want 3 events", r, o)
	}
	if rows := listSessions(t, e.s, operatorCtx, false); rows[idle.ID].GetState() != stateIdle {
		t.Fatalf("sess-idle state = %v, want idle", rows[idle.ID])
	}
	sweepAt(e.base.Add(90*24*time.Hour - time.Minute))
	if r, _ := counts(t, e.s, ended); r != 1 {
		t.Fatal("the refusal was deleted before day 90")
	}
	sweepAt(e.base.Add(90*24*time.Hour + time.Minute))
	if r, _ := counts(t, e.s, ended); r != 0 {
		t.Fatal("the refusal outlived day 90")
	}
	for _, o := range []toollog.Owner{ended, exited, idle} {
		rep, err := e.s.VerifyToolLog(operatorCtx, &orchdv1.VerifyToolLogRequest{Owner: o.String()})
		if err != nil || len(rep.GetProblems()) != 0 || (o != idle) != (rep.GetPruned() > 0) {
			t.Fatalf("verify %s after retention = %v, %v; want intact, with the cut shown when one was made", o.ID, rep, err)
		}
	}
}

// TestEarlyEndKeepsLiveLog: a session whose pin was ended while its process
// runs (any process in it can call EndSession, and /clear does) is not
// expired while the process runs, and gets its full retention from when the
// liveness sweep saw the process exit (the audit's round-1 case: it was
// deleted at the next sweep after the exit).
func TestEarlyEndKeepsLiveLog(t *testing.T) {
	e := newLiveEnv(t)
	withToolLog(t, e.s)
	o := sessionOwner("sess-early")
	e.pin(t, e.hook("claude", 400), o.ID, e.dirs[0])
	if _, err := e.s.RecordToolEvent(e.session("claude", 400, o.ID), bashReq(o.ID, e.dirs[0], "pwd")); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, e.s)
	if _, err := e.s.EndSession(e.hook("claude", 400), &orchdv1.EndSessionRequest{SessionId: o.ID}); err != nil {
		t.Fatal(err)
	}
	waitForEndRecord(t, e.s, o.ID)
	if _, err := e.s.RecordToolEvent(e.session("claude", 400, o.ID), bashReq(o.ID, e.dirs[0], "ls")); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, e.s)
	month := e.base.Add(30 * 24 * time.Hour)
	e.s.sweepToolLog(context.Background(), month)
	if _, n := counts(t, e.s, o); n == 0 {
		t.Fatal("a running process's log was deleted after an early SessionEnd")
	}
	delete(e.tbl.started, 400)
	e.s.sweepToolLog(context.Background(), month)
	if _, n := counts(t, e.s, o); n == 0 {
		t.Fatal("deleted when the process exited, before the sweep saw it")
	}
	e.clock.set(month)
	e.s.sweepSessions(context.Background())
	e.s.sweepToolLog(context.Background(), month.Add(toollog.DefaultRetention-time.Second))
	if _, n := counts(t, e.s, o); n == 0 {
		t.Fatal("deleted less than 7 days after the process exited")
	}
	e.s.sweepToolLog(context.Background(), month.Add(toollog.DefaultRetention))
	if _, n := counts(t, e.s, o); n != 0 {
		t.Fatalf("%d records left 7 days after the process exited", n)
	}
	p, _, _ := e.s.pins.get(context.Background(), o.ID)
	if p.ExitedAt.IsZero() {
		t.Fatal("the sweep did not record the exit")
	}
}

// TestLogRetentionTasks: a task's chain is kept while the runner runs it and
// expires N days after its last record once it does not.
func TestLogRetentionTasks(t *testing.T) {
	s, _ := pinsServer(t)
	withToolLog(t, s)
	running := map[string]bool{"t-run": true}
	s.toolLog.running = func(id string) bool { return running[id] }
	for _, id := range []string{"t-run", "t-done"} {
		o := toollog.Owner{Kind: toollog.OwnerTask, ID: id}
		s.toolLog.writer.Enqueue(toollog.Key{Quota: o.String(), Owner: o}, 100, func() toollog.Record {
			return toollog.Record{TS: time.Now().UnixNano(), Event: "pre-tool-use", Tool: "Bash", Command: "ls"}
		})
	}
	flushToolLog(t, s)
	s.sweepToolLog(context.Background(), time.Now().Add(toollog.DefaultRetention+time.Minute))
	if _, n := counts(t, s, toollog.Owner{Kind: toollog.OwnerTask, ID: "t-run"}); n != 1 {
		t.Fatal("a running task's log was deleted")
	}
	if _, n := counts(t, s, toollog.Owner{Kind: toollog.OwnerTask, ID: "t-done"}); n != 0 {
		t.Fatal("a finished task's log outlived retention")
	}
	// With no seam the current runner is asked; there is none.
	s.toolLog.running = nil
	if s.taskRunning("t-run") {
		t.Fatal("a task runs with no runner")
	}
	s.live = &liveRunner{}
	if s.taskRunning("t-run") {
		t.Fatal("a task runs with an empty runner slot")
	}
}

// TestLogRetentionUnpinnedAndUnknown: a session chain with no pin counts as
// ended at its last record; a sweep with the log off does nothing.
func TestLogRetentionUnpinnedAndUnknown(t *testing.T) {
	s, _ := pinsServer(t)
	withToolLog(t, s)
	o := sessionOwner("sess-nopin")
	s.toolLog.writer.Enqueue(toollog.Key{Quota: o.String(), Owner: o}, 100, func() toollog.Record {
		return toollog.Record{TS: time.Now().UnixNano(), Event: "pre-tool-use", Tool: "Bash", Command: "ls"}
	})
	flushToolLog(t, s)
	if st, err := s.chainState(context.Background(), toollog.Owner{Kind: "other", ID: "x"}); err == nil || st.Live {
		t.Fatalf("unknown owner kind = %+v, %v; want an error", st, err)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	s.sweepToolLog(context.Background(), time.Now().Add(toollog.DefaultRetention+time.Minute))
	log.SetOutput(os.Stderr)
	if _, n := counts(t, s, o); n != 0 {
		t.Fatal("an unpinned session's log outlived retention")
	}
	// Each cut is logged outside the file, with its pruned head.
	if out := logged.String(); !strings.Contains(out, "expired sess-nopin: 1 record(s) up to 1 deleted, pruned head ") {
		t.Fatalf("orchd's log = %q, want the cut named", out)
	}
	pins := s.pins
	s.pins = nil
	if st, err := s.chainState(context.Background(), o); err != nil || st.Live {
		t.Fatalf("with no pin registry = %+v, %v; want ended", st, err)
	}
	s.pins = pins
	s.toolLog = nil
	s.sweepToolLog(context.Background(), time.Now()) // no log: nothing to do
}

// TestLogPolicyFromSettings: the settings choose 3, 7 or 30 days and the
// total cap; anything else keeps the defaults.
func TestLogPolicyFromSettings(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		in        config.CommandLogSettings
		retention time.Duration
		total     int64
	}{
		{config.CommandLogSettings{}, 7 * day, 500 << 20},
		{config.CommandLogSettings{Retention: "3d", MaxTotalMB: 100}, 3 * day, 100 << 20},
		{config.CommandLogSettings{Retention: "30d", MaxTotalMB: 2000}, 30 * day, 2000 << 20},
		{config.CommandLogSettings{Retention: "1d", MaxTotalMB: 5}, 7 * day, 500 << 20},
		{config.CommandLogSettings{Retention: "7d", MaxTotalMB: 1 << 30}, 7 * day, 500 << 20},
	} {
		p := logPolicy(c.in)
		if p.Retention != c.retention || p.TotalCap != c.total || p.SessionCap != toollog.DefaultSessionCap {
			t.Errorf("logPolicy(%+v) = %+v; want %v, %d", c.in, p, c.retention, c.total)
		}
	}
	d, err := config.Unmarshal(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if p := logPolicy(d.Sessions.CommandLog); p != toollog.DefaultPolicy() {
		t.Errorf("default settings give %+v, want %+v", p, toollog.DefaultPolicy())
	}
}

// TestToolLogSweepLoop: retention runs at start and on every tick until
// orchd stops.
func TestToolLogSweepLoop(t *testing.T) {
	s, _ := pinsServer(t)
	withToolLog(t, s)
	o := sessionOwner("sess-nopin")
	s.toolLog.writer.Enqueue(toollog.Key{Quota: o.String(), Owner: o}, 100, func() toollog.Record {
		return toollog.Record{TS: time.Now().Add(-30 * 24 * time.Hour).UnixNano(), Event: "pre-tool-use", Tool: "Bash"}
	})
	flushToolLog(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runToolLogSweeps(ctx, time.Millisecond); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, n := counts(t, s, o); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop never swept")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
