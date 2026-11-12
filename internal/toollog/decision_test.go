package toollog

import (
	"context"
	"testing"
)

// TestEnqueueDecisionUsesReserve (D-37): once a quota key's ordinary share
// is full, guard decisions still queue in the reserve, which has shares of
// its own; ordinary events keep dropping with a gap record.
func TestEnqueueDecisionUsesReserve(t *testing.T) {
	s, _ := openTemp(t)
	w := newWriter(t, s, 16) // share 4, reserve 4, reserve share 1
	release, started := make(chan struct{}), make(chan struct{})
	o := session("flood")
	w.Enqueue(key(o), 1, func() Record { close(started); <-release; return Record{Event: "pre-tool-use"} })
	<-started
	ordinary := 0
	for range 10 {
		if w.Enqueue(key(o), 1, bashEvent("ls")) {
			ordinary++
		}
	}
	if ordinary != 3 {
		t.Fatalf("%d ordinary events queued past the held one, want 3 (share of 4)", ordinary)
	}
	if !w.EnqueueDecision(key(o), 1, func() Record { return Record{Event: "pre-tool-use", Decision: DecisionRefused} }) {
		t.Fatal("a refusal was dropped with the share full")
	}
	if w.EnqueueDecision(key(o), 1, bashEvent("second refusal")) {
		t.Error("one quota key took more than its share of the reserve")
	}
	other := session("other")
	if !w.EnqueueDecision(key(other), 1, bashEvent("fits ordinarily")) {
		t.Error("a decision of another key was dropped")
	}
	close(release)
	flush(t, w)
	recs, err := s.Records(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var refused, gaps int
	for _, r := range recs {
		if r.Decision == DecisionRefused {
			refused++
		}
		if r.Kind == KindGap {
			gaps++
		}
	}
	if refused != 1 || gaps == 0 {
		t.Errorf("%d refusals and %d gap records stored, want 1 and some", refused, gaps)
	}
	w.Close()
	if w.EnqueueDecision(key(o), 1, bashEvent("after close")) {
		t.Error("queued after close")
	}
}
