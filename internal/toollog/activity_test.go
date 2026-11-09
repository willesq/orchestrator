package toollog

import (
	"context"
	"testing"
	"time"
)

// TestSessionActivity: the session list's counts take only tool events of
// the session asked for; refusals are counted, gap and end records are not,
// and a session with no events is absent.
func TestSessionActivity(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, DefaultQueue)
	a, b := session("sess-a"), session("sess-b")
	// orchd's record builder stamps TS when it takes the event.
	stamped := func(cmd, decision string) func() Record {
		return func() Record {
			r := bashEvent(cmd)()
			r.TS, r.Decision = time.Now().UnixNano(), decision
			return r
		}
	}
	before := time.Now().UnixNano()
	for range 3 {
		w.Enqueue(key(a), 100, stamped("ls", "logged"))
	}
	w.Enqueue(key(a), 100, stamped("rm -rf ~/other", DecisionRefused))
	w.Enqueue(key(b), 100, bashEvent("pwd"))
	w.Enqueue(Key{Quota: "task:t1", Owner: Owner{Kind: OwnerTask, ID: "sess-a"}}, 100, bashEvent("task"))
	if err := w.End(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	flush(t, w)

	got, err := s.SessionActivity(context.Background(), []string{"sess-a", "sess-b", "sess-none"})
	if err != nil {
		t.Fatal(err)
	}
	if ga := got["sess-a"]; ga.Events != 4 || ga.Refused != 1 || ga.Last < before {
		t.Errorf("sess-a = %+v, want 4 events, 1 refused, last >= %d", ga, before)
	}
	if gb := got["sess-b"]; gb.Events != 1 || gb.Refused != 0 {
		t.Errorf("sess-b = %+v, want 1 event", gb)
	}
	if _, ok := got["sess-none"]; ok || len(got) != 2 {
		t.Errorf("activity = %+v, want only sess-a and sess-b", got)
	}
}

func TestSessionActivityClosedStore(t *testing.T) {
	s, _ := openTemp(t)
	_ = s.Close()
	if _, err := s.SessionActivity(context.Background(), []string{"x"}); err == nil {
		t.Fatal("SessionActivity on a closed store succeeded")
	}
}
