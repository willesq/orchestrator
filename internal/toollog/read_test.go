package toollog

import (
	"context"
	"testing"
	"time"
)

// TestPage reads a chain a page at a time, and reports the retention cut.
func TestPage(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess-1")
	writeChain(t, s, o, 4) // records 1-4 and an end record, 5
	writeChain(t, s, session("sess-2"), 2)
	ctx := context.Background()
	recs, more, pruned, err := s.Page(ctx, o, 0, 3, 1<<20)
	if err != nil || len(recs) != 3 || recs[0].Seq != 1 || recs[2].Seq != 3 || !more || pruned != 0 {
		t.Fatalf("first page = %v, %v, %d, %v", recs, more, pruned, err)
	}
	two := recs[0].textSize() + recs[1].textSize()
	recs, more, _, err = s.Page(ctx, o, 3, 10, 1<<20)
	if err != nil || len(recs) != 2 || recs[1].Kind != KindEnd || recs[0].OwnerID != "sess-1" || more {
		t.Fatalf("second page = %v, %v, %v", recs, more, err)
	}
	if recs, more, _, err = s.Page(ctx, session("nobody"), 0, 10, 1<<20); err != nil || len(recs) != 0 || more {
		t.Fatalf("unknown owner = %v, %v, %v", recs, more, err)
	}
	// The byte budget ends a page early, but never before its first record.
	if recs, more, _, err = s.Page(ctx, o, 0, 10, 1); err != nil || len(recs) != 1 || !more {
		t.Fatalf("budget below one record = %v, %v, %v", recs, more, err)
	}
	if recs, more, _, err = s.Page(ctx, o, 0, 10, two+1); err != nil || len(recs) != 2 || !more {
		t.Fatalf("budget of two records = %d records, %v, %v", len(recs), more, err)
	}
	if _, err := s.db.Exec(`INSERT INTO pruned VALUES ('session', 'sess-1', 2, 'h', 'event')`); err != nil {
		t.Fatal(err)
	}
	if _, _, pruned, err = s.Page(ctx, o, 0, 1, 1<<20); err != nil || pruned != 2 {
		t.Fatalf("pruned = %d, %v", pruned, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := s.Page(cctx, o, 0, 1, 1); err == nil {
		t.Fatal("a cancelled read succeeded")
	}
}

// TestActivitySince sums each owner's calls, refusals and moves by
// directory, leaving out gap and end records and events before the window.
func TestActivitySince(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	now := time.Now()
	ev := func(o Owner, at time.Time, r Record) {
		r.TS = at.UnixNano()
		w.Enqueue(key(o), 100, func() Record { return r })
	}
	a, b := session("sess-a"), Owner{Kind: OwnerTask, ID: "t-1"}
	ev(a, now, Record{Event: EventPreToolUse, Tool: "Bash"})
	ev(a, now, Record{Event: EventPreToolUse, Tool: "Bash", Decision: DecisionRefused})
	ev(a, now, Record{Event: EventCwdChanged, NewCwd: "/x"})
	ev(a, now, Record{Event: EventCwdChanged, NewCwd: "/x"})
	ev(a, now, Record{Event: EventCwdChanged, NewCwd: "/y"})
	ev(a, now.Add(-time.Hour), Record{Event: EventPreToolUse, Tool: "Bash"})
	ev(b, now, Record{Event: EventPreToolUse, Tool: "Read"})
	ev(b, now, Record{Event: "unknown"})
	flush(t, w)
	if err := w.End(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	acts, err := s.ActivitySince(context.Background(), now.Add(-time.Minute).UnixNano())
	if err != nil || len(acts) != 2 {
		t.Fatalf("ActivitySince = %v, %v", acts, err)
	}
	got := map[Owner]OwnerActivity{acts[0].Owner: acts[0], acts[1].Owner: acts[1]}
	if g := got[a]; g.Calls != 2 || g.Refused != 1 || g.Moves["/x"] != 2 || g.Moves["/y"] != 1 || len(g.Moves) != 2 {
		t.Errorf("sess-a = %+v", g)
	}
	if g := got[b]; g.Calls != 1 || g.Refused != 0 || len(g.Moves) != 0 {
		t.Errorf("t-1 = %+v", g)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ActivitySince(cctx, 0); err == nil {
		t.Fatal("a cancelled read succeeded")
	}
}
