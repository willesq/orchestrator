package toollog

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// anchorSink collects what a writer hands to its onAnchor, the way orchd
// copies anchors into orch.db.
type anchorSink struct {
	mu  sync.Mutex
	got []Anchor
}

func (a *anchorSink) save(as []Anchor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, as...)
}

func (a *anchorSink) of(o Owner) []Anchor {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []Anchor{}
	for _, x := range a.got {
		if x.Owner == o {
			out = append(out, x)
		}
	}
	return out
}

// copyingChain writes two runs of n events, each ended (anchored), through a
// writer that copies its anchors into sink.
func copyingChain(t *testing.T, s *Store, o Owner, n int, sink *anchorSink) {
	t.Helper()
	w := NewWriterWithAnchors(s, DefaultQueue, nil, sink.save)
	defer w.Close()
	for run := range 2 {
		for i := range n {
			w.Enqueue(key(o), 100, bashEvent(fmt.Sprintf("run %d cmd %d", run, i)))
		}
		if err := w.End(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
}

func verifyCopies(t *testing.T, s *Store, o Owner, copies []Anchor) Report {
	t.Helper()
	return verifyWithCut(t, s, o, copies, Cut{})
}

func verifyWithCut(t *testing.T, s *Store, o Owner, copies []Anchor, cut Cut) Report {
	t.Helper()
	rep, err := s.VerifyCopies(context.Background(), o, Copies{Anchors: copies, Cut: cut})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func hasProblem(rep Report, text string) bool {
	for _, p := range rep.Problems {
		if strings.Contains(p.Detail, text) {
			return true
		}
	}
	return false
}

// rewriteChain rewrites o's chain from its first record, as someone holding
// the file could: every record's command changes, every hash and link is
// recomputed, and the file's anchors are moved to the new hashes.
func rewriteChain(t *testing.T, s *Store, o Owner) {
	t.Helper()
	ctx := context.Background()
	recs, err := s.Records(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	prev := ""
	for _, r := range recs {
		if r.Kind == KindEvent {
			r.Command = "echo innocent"
		}
		r.PrevHash = prev
		r.Hash = r.computeHash()
		prev = r.Hash
		if _, err := s.db.Exec(`UPDATE events SET command = ?, prev_hash = ?, hash = ? WHERE owner_kind = ? AND owner_id = ? AND seq = ?`,
			r.Command, r.PrevHash, r.Hash, o.Kind, o.ID, r.Seq); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE anchors SET hash = ? WHERE owner_kind = ? AND owner_id = ? AND seq = ?`,
			r.Hash, o.Kind, o.ID, r.Seq); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAnchorCopiesCatchWhatTheFileCannot is SP-073.1.a1 at the store: every
// anchor the writer stores is handed on for orch.db; a chain cut after its
// last anchor in the file, or rewritten from its first record, still
// verifies from the file alone but not against the copies; an anchor in
// only one store is reported.
func TestAnchorCopiesCatchWhatTheFileCannot(t *testing.T) {
	cases := []struct {
		name string
		// tamper changes the file; copies changes orch.db's copies.
		tamper func(t *testing.T, s *Store, o Owner)
		copies func([]Anchor) []Anchor
		want   string
		// fileOK says the file alone still verifies.
		fileOK bool
	}{
		{name: "cut after the last anchor in the file", fileOK: true,
			tamper: func(t *testing.T, s *Store, o Owner) {
				if _, err := s.db.Exec(`DELETE FROM events WHERE seq > 4; DELETE FROM anchors WHERE seq > 4`); err != nil {
					t.Fatal(err)
				}
			}, want: "records after it were removed"},
		{name: "rewritten from the first record", fileOK: true,
			tamper: rewriteChain, want: "the chain was rewritten"},
		{name: "anchor only in toollog.db", fileOK: true,
			copies: func(c []Anchor) []Anchor { return c[:1] }, want: "orch.db has no copy"},
		{name: "anchor only in orch.db", fileOK: true,
			copies: func(c []Anchor) []Anchor {
				return append(c, Anchor{Owner: c[0].Owner, Seq: 2, Hash: "x"})
			}, want: "missing from toollog.db"},
		{name: "a copy disagrees", fileOK: true,
			copies: func(c []Anchor) []Anchor {
				c[1].Hash = strings.Repeat("0", 64)
				return c
			}, want: "disagree"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := openTemp(t)
			o := session("anchored")
			sink := &anchorSink{}
			copyingChain(t, s, o, 3, sink)
			copies := sink.of(o)
			inFile, err := s.Anchors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(copies) != 2 || len(inFile) != 2 || copies[0] != inFile[0] || copies[1] != inFile[1] {
				t.Fatalf("copies %+v, file anchors %+v; want the same two", copies, inFile)
			}
			if rep := verifyCopies(t, s, o, copies); !rep.OK() || rep.Copies != 2 || rep.Anchors != 2 {
				t.Fatalf("untouched chain against its copies: %+v", rep)
			}
			if c.tamper != nil {
				c.tamper(t, s, o)
			}
			if c.copies != nil {
				copies = c.copies(copies)
			}
			if rep := verify(t, s, o); rep.OK() != c.fileOK {
				t.Fatalf("the file alone: %+v; want OK %v", rep, c.fileOK)
			}
			rep := verifyCopies(t, s, o, copies)
			if rep.OK() || !hasProblem(rep, c.want) {
				t.Fatalf("against the copies: %+v; want a problem saying %q", rep.Problems, c.want)
			}
		})
	}
}

// TestAnchorCopiesWithRetention: retention hands every cut to the cut
// hook; an anchored head retention deleted still matches its copy through
// the pruned head; a copy of a deleted head that disagrees with it is
// reported.
func TestAnchorCopiesWithRetention(t *testing.T) {
	s, _ := openTemp(t)
	o := session("pruned")
	sink := &anchorSink{}
	copyingChain(t, s, o, 2, sink)
	ctx := context.Background()
	var cuts []Cut
	s.SetCutHook(func(k Cut) { cuts = append(cuts, k) })
	// Expire everything: the chain ended at its last record.
	res, err := s.Sweep(ctx, time.Now().Add(DefaultRetention+time.Hour), DefaultPolicy(),
		func(context.Context, Owner) (ChainState, error) { return ChainState{}, nil })
	if err != nil || len(cuts) != 1 || cuts[0] != res.Cuts[0] || cuts[0].Seq != 6 {
		t.Fatalf("cuts handed over = %+v (sweep %+v, %v)", cuts, res, err)
	}
	copies := sink.of(o)
	if rep := verifyWithCut(t, s, o, copies, cuts[0]); !rep.OK() || rep.Records != 0 {
		t.Fatalf("expired chain: %+v", rep)
	}
	copies[1].Hash = "y"
	if rep := verifyWithCut(t, s, o, copies, cuts[0]); rep.OK() {
		t.Fatal("a copy that disagrees with the pruned head was not reported")
	}
	if got, err := s.Cuts(ctx); err != nil || len(got) != 1 || got[0].Seq != 6 || got[0].Hash != cuts[0].Hash {
		t.Fatalf("Cuts = %+v, %v", got, err)
	}
}

// TestForgedPrunedHeadReported: records deleted by hand under a pruned head
// written into the file pass the file's own check, but not orch.db's copy
// of the pruned head (the SP-073 audit's first finding).
func TestForgedPrunedHeadReported(t *testing.T) {
	s, _ := openTemp(t)
	o := session("forged")
	sink := &anchorSink{}
	copyingChain(t, s, o, 3, sink)
	if _, err := s.db.Exec(`DELETE FROM events; INSERT INTO pruned (owner_kind, owner_id, seq, hash, kind) VALUES ('session', 'forged', 9, 'x', 'event')`); err != nil {
		t.Fatal(err)
	}
	if rep := verify(t, s, o); !rep.OK() {
		t.Fatalf("the file alone: %+v; want it to pass, as the audit found", rep)
	}
	rep := verifyCopies(t, s, o, sink.of(o))
	if rep.OK() || !hasProblem(rep, "does not match orch.db's copy") || !hasProblem(rep, "records after it were removed") {
		t.Fatalf("against orch.db: %+v", rep.Problems)
	}
}

// TestOwnerKindsAndProcessIDs: process chains parse and print like the
// other owner kinds; "process:<pid>" alone is accepted for lookup.
func TestOwnerKindsAndProcessIDs(t *testing.T) {
	id := ProcessID(501, 1700000000123456789)
	if id != "501-1700000000123456789" {
		t.Fatalf("ProcessID = %q", id)
	}
	if pid, started, ok := ParseProcessID(id); !ok || pid != 501 || started != 1700000000123456789 {
		t.Fatalf("ParseProcessID(%q) = %d %d %v", id, pid, started, ok)
	}
	for _, bad := range []string{"501", "x-1", "501-", "-1", "501-1-2", "99999999999-1"} {
		if _, _, ok := ParseProcessID(bad); ok {
			t.Errorf("ParseProcessID(%q) accepted", bad)
		}
	}
	for in, want := range map[string]Owner{
		"process:" + id: {Kind: OwnerProcess, ID: id},
		"process:501":   {Kind: OwnerProcess, ID: "501"},
	} {
		got, err := ParseOwner(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("ParseOwner(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"process:abc", "process:", "process:1-2-3", "process:1-99999999999999999999", "process:1-9999999999999999999", "process:007-5", "process:0"} {
		if _, err := ParseOwner(bad); err == nil {
			t.Errorf("ParseOwner(%q) accepted", bad)
		}
	}
}

// TestChainQueries: the open chains of a kind, a PID's latest process
// chain, and every process chain's activity.
func TestChainQueries(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	older := Owner{Kind: OwnerProcess, ID: ProcessID(501, 1)}
	newer := Owner{Kind: OwnerProcess, ID: ProcessID(501, 2)}
	other := Owner{Kind: OwnerProcess, ID: ProcessID(5010, 1)}
	w := NewWriter(s, DefaultQueue, nil)
	defer w.Close()
	for i, o := range []Owner{older, other, newer} {
		w.Enqueue(key(o), 100, func() Record {
			return Record{TS: int64(i + 1), Harness: "claude", Event: EventPreToolUse, Tool: "Bash"}
		})
	}
	w.Enqueue(key(newer), 100, func() Record {
		return Record{TS: 9, Harness: "claude", Event: EventPreToolUse, Tool: "Bash", Decision: DecisionRefused}
	})
	if err := w.End(ctx, older); err != nil {
		t.Fatal(err)
	}
	writeChain(t, s, Owner{Kind: OwnerTask, ID: "t1"}, 1)
	open, err := s.OpenChains(ctx, OwnerProcess)
	if err != nil || len(open) != 2 || open[0] != newer || open[1] != other {
		t.Fatalf("open process chains = %v, %v; want %v and %v", open, err, newer, other)
	}
	if tasks, err := s.OpenChains(ctx, OwnerTask); err != nil || len(tasks) != 0 {
		t.Fatalf("open task chains = %v, %v; want none", tasks, err)
	}
	if got, ok, err := s.LatestProcessChain(ctx, 501); err != nil || !ok || got != newer {
		t.Fatalf("LatestProcessChain(501) = %v %v %v; want %v", got, ok, err, newer)
	}
	if _, ok, err := s.LatestProcessChain(ctx, 50); err != nil || ok {
		t.Fatalf("LatestProcessChain(50) = %v %v; want none", ok, err)
	}
	chains, err := s.ProcessChains(ctx)
	if err != nil || len(chains) != 3 {
		t.Fatalf("ProcessChains = %+v, %v", chains, err)
	}
	for _, c := range chains {
		if c.Owner == newer && (c.Activity.Events != 2 || c.Activity.Refused != 1 || c.Activity.Last != 9 || c.Harness != "claude") {
			t.Errorf("newer chain = %+v", c)
		}
	}
}
