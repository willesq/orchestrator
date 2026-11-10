package toollog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

// t0 is when the retention tests' sessions end.
var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// stamped is an event orchd took at time at, with the given decision.
func stamped(cmd, decision string, at time.Time) func() Record {
	return func() Record {
		r := bashEvent(cmd)()
		r.TS, r.Decision = at.UnixNano(), decision
		return r
	}
}

// logAt stores events for o, taken at at: logged ones, then refused ones;
// with end set, an end record at at follows.
func logAt(t *testing.T, s *Store, o Owner, at time.Time, logged, refused int, end bool) {
	t.Helper()
	w := NewWriter(s, DefaultQueue, nil)
	defer w.Close()
	w.now = func() time.Time { return at }
	for i := range logged {
		w.Enqueue(key(o), 100, stamped(fmt.Sprintf("ls %d", i), "logged", at))
	}
	for i := range refused {
		w.Enqueue(key(o), 100, stamped(fmt.Sprintf("cat /other/%d", i), DecisionRefused, at))
	}
	if end {
		if err := w.End(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	flush(t, w)
}

// states is a StateFunc from a map; an owner not in it is live.
type states map[Owner]ChainState

func (m states) of(_ context.Context, o Owner) (ChainState, error) {
	if st, ok := m[o]; ok {
		return st, nil
	}
	return ChainState{Live: true}, nil
}

func sweep(t *testing.T, s *Store, now time.Time, p Policy, st states) SweepResult {
	t.Helper()
	res, err := s.Sweep(context.Background(), now, p, st.of)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return res
}

// kinds counts o's stored records: logged events, refusals, and others
// (gap and end records).
func kinds(t *testing.T, s *Store, o Owner) (logged, refused, other int) {
	t.Helper()
	recs, err := s.Records(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		switch {
		case r.Kind != KindEvent:
			other++
		case r.Decision == DecisionRefused:
			refused++
		default:
			logged++
		}
	}
	return
}

func intact(t *testing.T, s *Store, o Owner) Report {
	t.Helper()
	rep := verify(t, s, o)
	if !rep.OK() {
		t.Fatalf("%s does not verify after retention: %+v", o, rep.Problems)
	}
	return rep
}

// TestRetentionEndedSession is SP-015.1.a1 at the store, for each retention
// an operator may choose: a session that ended keeps everything until N days
// after its end, then only its refusals, which go 90 days after they were
// taken. Its chain verifies throughout.
func TestRetentionEndedSession(t *testing.T) {
	for _, n := range RetentionDays {
		t.Run(fmt.Sprintf("%dd", n), func(t *testing.T) {
			s, _ := openTemp(t)
			o := session("sess-ended")
			logAt(t, s, o, t0.Add(-time.Hour), 5, 2, true)
			p := DefaultPolicy()
			p.Retention = time.Duration(n) * day
			st := states{o: {Ended: t0}}

			if res := sweep(t, s, t0.Add(p.Retention-time.Second), p, st); res.Records != 0 {
				t.Fatalf("a second before day %d the sweep deleted %d records", n, res.Records)
			}
			if l, r, x := kinds(t, s, o); l != 5 || r != 2 || x != 1 {
				t.Fatalf("before day %d: %d logged, %d refused, %d other; want 5, 2, 1", n, l, r, x)
			}
			if res := sweep(t, s, t0.Add(p.Retention), p, st); res.Records != 6 || res.Expired != 1 {
				t.Fatalf("at day %d the sweep = %+v, want 6 records of 1 chain", n, res)
			}
			if l, r, x := kinds(t, s, o); l != 0 || r != 2 || x != 0 {
				t.Fatalf("at day %d: %d logged, %d refused, %d other; want only the 2 refusals", n, l, r, x)
			}
			intact(t, s, o)

			// Refusals were taken an hour before the end: kept to day 90.
			taken := t0.Add(-time.Hour)
			sweep(t, s, taken.Add(RefusalRetention-time.Second), p, st)
			if _, r, _ := kinds(t, s, o); r != 2 {
				t.Fatalf("refusals before day 90: %d, want 2", r)
			}
			sweep(t, s, taken.Add(RefusalRetention), p, st)
			if l, r, x := kinds(t, s, o); l+r+x != 0 {
				t.Fatalf("at day 90: %d logged, %d refused, %d other left", l, r, x)
			}
			if rep := intact(t, s, o); rep.Records != 0 || rep.Anchors != 1 || rep.Pruned != 8 {
				t.Fatalf("deleted chain = %+v, want 0 records, its anchor kept, pruned to 8", rep)
			}
		})
	}
}

// TestRetentionCountsFromDetection is SP-015.1.a1's process-exit case at the
// store: the clock starts when orchd noticed the end (the pin's ended_at),
// not at the chain's last record.
func TestRetentionCountsFromDetection(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess-exited")
	logAt(t, s, o, t0, 3, 0, false)
	detected := t0.Add(2 * day)
	st := states{o: {Ended: detected}}
	p := DefaultPolicy()
	if res := sweep(t, s, detected.Add(p.Retention-time.Second), p, st); res.Records != 0 {
		t.Fatalf("deleted %d records before N days after detection", res.Records)
	}
	if res := sweep(t, s, detected.Add(p.Retention), p, st); res.Records != 3 {
		t.Fatalf("N days after detection deleted %d records, want 3", res.Records)
	}
	// With no end time the chain ended at its last record.
	o2 := session("sess-unknown")
	logAt(t, s, o2, t0, 2, 0, false)
	st[o2] = ChainState{}
	if res := sweep(t, s, t0.Add(p.Retention), p, st); res.Records != 2 {
		t.Fatalf("a chain with no end time: deleted %d, want 2", res.Records)
	}
}

// TestRetentionKeepsIdleLiveSession is SP-015.1.a1's idle case: a session
// whose process still runs keeps its records however long it is idle.
func TestRetentionKeepsIdleLiveSession(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess-idle")
	logAt(t, s, o, t0, 4, 1, false)
	for _, after := range []time.Duration{14 * day, 120 * day} {
		if res := sweep(t, s, t0.Add(after), DefaultPolicy(), states{}); res.Records != 0 {
			t.Fatalf("idle %v: deleted %d records of a live session", after, res.Records)
		}
	}
	if l, r, _ := kinds(t, s, o); l != 4 || r != 1 {
		t.Fatalf("idle session kept %d logged, %d refused; want 4, 1", l, r)
	}
}

// bigEvent is an event whose stored record is about size bytes.
func bigEvent(i int, decision string, size int) func() Record {
	return func() Record {
		r := stamped(fmt.Sprintf("echo %d", i), decision, t0)()
		r.Targets = strings.Repeat("t", size)
		return r
	}
}

func chainSize(t *testing.T, s *Store, o Owner) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(`+recordSize+`), 0) FROM events WHERE owner_kind = ? AND owner_id = ?`,
		o.Kind, o.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestLiveSessionCappedOldestFirst is SP-015.1.a2's first half with the
// default 50 MB cap: a live session over it loses its oldest records other
// than refusals, every refusal stays, and the chain still verifies and
// continues.
func TestLiveSessionCappedOldestFirst(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess-big")
	const n, size = 5600, 10 << 10 // about 56 MB
	w := NewWriter(s, DefaultQueue, nil)
	for i := range n {
		decision := "logged"
		if i%500 == 0 {
			decision = DecisionRefused
		}
		w.Enqueue(key(o), 100, bigEvent(i, decision, size))
		if i%1000 == 999 {
			flush(t, w)
		}
	}
	flush(t, w)
	w.Close()
	before := chainSize(t, s, o)
	if before <= DefaultSessionCap {
		t.Fatalf("set-up: chain is %d bytes, want over %d", before, DefaultSessionCap)
	}
	_, refusedBefore, _ := kinds(t, s, o)

	res := sweep(t, s, t0.Add(400*day), DefaultPolicy(), states{})
	after := chainSize(t, s, o)
	t.Logf("live chain %d -> %d bytes (cap %d); %d records deleted", before, after, DefaultSessionCap, res.Records)
	if res.Trimmed != 1 || after > DefaultSessionCap || after < DefaultSessionCap-2*size {
		t.Fatalf("after the sweep: %+v, %d bytes; want cut to just under %d", res, after, DefaultSessionCap)
	}
	recs, err := s.Records(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var refused, logged int
	firstKept := int64(0)
	for _, r := range recs {
		if r.Decision == DecisionRefused {
			refused++
			continue
		}
		logged++
		if firstKept == 0 {
			firstKept = r.Seq
		}
	}
	// Oldest first: the logged records left are exactly the newest ones.
	newer := 0
	for seq := firstKept; seq <= n; seq++ {
		if (seq-1)%500 != 0 {
			newer++
		}
	}
	if refused != refusedBefore || logged != newer {
		t.Fatalf("kept %d refusals (want %d) and %d logged from seq %d; want all %d logged records from there on",
			refused, refusedBefore, logged, firstKept, newer)
	}
	if rep := intact(t, s, o); rep.Pruned != firstKept-1 {
		t.Fatalf("pruned head %d, want %d", rep.Pruned, firstKept-1)
	}
	// The chain continues past the cut.
	logAt(t, s, o, t0.Add(400*day), 1, 0, true)
	if rep := intact(t, s, o); rep.Ends != 1 {
		t.Fatalf("continued chain = %+v", rep)
	}
}

// TestLiveSessionOnlyRefusalsOverCap: refusals are never cut from a live
// session, even when they alone are over the cap.
func TestLiveSessionOnlyRefusalsOverCap(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess-refusing")
	w := NewWriter(s, DefaultQueue, nil)
	for i := range 10 {
		w.Enqueue(key(o), 100, bigEvent(i, DecisionRefused, 4<<10))
	}
	w.Enqueue(key(o), 100, bigEvent(10, "logged", 4<<10))
	flush(t, w)
	w.Close()
	p := DefaultPolicy()
	p.SessionCap = 8 << 10
	if res := sweep(t, s, t0, p, states{}); res.Records != 1 {
		t.Fatalf("deleted %d, want only the logged record", res.Records)
	}
	if l, r, _ := kinds(t, s, o); l != 0 || r != 10 {
		t.Fatalf("kept %d logged, %d refused; want 0, 10", l, r)
	}
	intact(t, s, o)
}

// TestTotalCapDropsOldestEnded is SP-015.1.a2's second half: over the total
// cap, ended chains lose their records in the order they ended, until the
// file fits, keeping refusals younger than 90 days;
// a live chain is never touched, even when only live chains are left.
func TestTotalCapDropsOldestEnded(t *testing.T) {
	s, _ := openTemp(t)
	live := session("sess-live")
	oldest, middle, newest := session("sess-old"), session("sess-mid"), task("t-new")
	logAt(t, s, live, t0, 400, 2, false)
	taken := map[Owner]time.Time{oldest: t0.Add(-4 * time.Hour), middle: t0.Add(-4 * time.Hour), newest: t0}
	for o, at := range taken {
		w := NewWriter(s, DefaultQueue, nil)
		for i := range 201 {
			decision := "logged"
			if i == 200 {
				decision = DecisionRefused
			}
			w.Enqueue(key(o), 100, func() Record {
				r := bigEvent(i, decision, 2<<10)()
				r.TS = at.UnixNano()
				return r
			})
		}
		flush(t, w)
		w.Close()
	}
	// Ended in this order, all within retention; the task ended at its
	// last record, which is after the sessions' ends.
	st := states{
		middle: {Ended: t0.Add(-2 * time.Hour)},
		oldest: {Ended: t0.Add(-3 * time.Hour)},
		newest: {},
	}
	used, err := s.used(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()
	p.TotalCap = used - 100<<10 // one chain's worth fits it
	res := sweep(t, s, t0, p, st)
	if res.Capped != 1 || res.Used > p.TotalCap {
		t.Fatalf("sweep = %+v, want exactly one chain cut and %d bytes or fewer in use", res, p.TotalCap)
	}
	if len(res.Cuts) != 1 || res.Cuts[0].Owner != oldest || res.Cuts[0].Reason != "capped" || res.Cuts[0].Deleted != 200 {
		t.Fatalf("cuts = %+v, want sess-old capped by 200 records", res.Cuts)
	}
	for o, want := range map[Owner][2]int{oldest: {0, 1}, middle: {200, 1}, newest: {200, 1}, live: {400, 2}} {
		if l, r, _ := kinds(t, s, o); l != want[0] || r != want[1] {
			t.Errorf("%s keeps %d logged, %d refused; want %v", o, l, r, want)
		}
	}
	// A cap below what the live chain alone needs cuts every ended chain
	// to its refusals and leaves the live one; the file stays over it.
	p.TotalCap = 1
	res = sweep(t, s, t0, p, st)
	if res.Capped != 2 || !res.OverCap {
		t.Fatalf("sweep = %+v, want the other two ended chains cut and the file still over the cap", res)
	}
	for _, o := range []Owner{oldest, middle, newest} {
		if l, r, _ := kinds(t, s, o); l != 0 || r != 1 {
			t.Errorf("%s keeps %d logged, %d refused; want only its refusal", o, l, r)
		}
	}
	if l, r, _ := kinds(t, s, live); l != 400 || r != 2 {
		t.Fatalf("live chain keeps %d logged, %d refused; want 400, 2", l, r)
	}
	for _, o := range []Owner{oldest, middle, newest, live} {
		intact(t, s, o)
	}
}

func task(id string) Owner { return Owner{Kind: OwnerTask, ID: id} }

// dump is every row of every table, in order.
func dump(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range []string{"events", "anchors", "pruned"} {
		rows, err := s.db.Query(`SELECT * FROM ` + table + ` ORDER BY 1, 2, 3`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], fmt.Sprint(vals...))
		}
		_ = rows.Close()
	}
	return out
}

// TestSweepTwiceChangesNothing is SP-015.1.a3: a second sweep at the same
// time deletes nothing and leaves every table as the first left it; the
// chain heads anchored for deleted sessions are kept, so a resumed session
// continues its chain.
func TestSweepTwiceChangesNothing(t *testing.T) {
	s, _ := openTemp(t)
	gone, refusing, big, live := session("sess-gone"), session("sess-refusing"), session("sess-big"), session("sess-live")
	logAt(t, s, gone, t0.Add(-time.Hour), 5, 0, true)
	logAt(t, s, refusing, t0.Add(-time.Hour), 5, 3, true)
	logAt(t, s, live, t0, 5, 1, false)
	w := NewWriter(s, DefaultQueue, nil)
	for i := range 20 {
		w.Enqueue(key(big), 100, bigEvent(i, "logged", 1<<10))
	}
	flush(t, w)
	w.Close()
	st := states{gone: {Ended: t0}, refusing: {Ended: t0}}
	p := DefaultPolicy()
	p.SessionCap = 10 << 10
	now := t0.Add(8 * day)

	first := sweep(t, s, now, p, st)
	if first.Expired != 2 || first.Trimmed != 1 {
		t.Fatalf("first sweep = %+v, want 2 expired and 1 trimmed", first)
	}
	before := dump(t, s)
	if second := sweep(t, s, now, p, st); second.Records != 0 || second.Expired+second.Trimmed+second.Capped != 0 {
		t.Fatalf("second sweep = %+v, want nothing deleted", second)
	}
	if after := dump(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("second sweep changed the tables:\nbefore %v\nafter  %v", before, after)
	}
	if len(before["anchors"]) != 2 {
		t.Fatalf("anchors = %v, want both deleted sessions' heads kept", before["anchors"])
	}
	for _, o := range []Owner{gone, refusing, big, live} {
		intact(t, s, o)
	}
	// sess-gone resumes: its chain goes on from the pruned head.
	logAt(t, s, gone, now, 2, 0, true)
	recs, err := s.Records(context.Background(), gone)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].Seq != 7 {
		t.Fatalf("resumed chain = %d records from seq %d, want 3 from 7", len(recs), recs[0].Seq)
	}
	if rep := intact(t, s, gone); rep.Anchors != 2 {
		t.Fatalf("resumed chain = %+v, want both anchors", rep)
	}
}

// TestVerifyAfterRetentionStillDetects: retention's cut does not hide
// tampering after it, nor a forged record before it.
func TestVerifyAfterRetentionStillDetects(t *testing.T) {
	// Each tamper is given the pruned head's sequence number.
	cases := map[string]string{
		"modified past the cut": `UPDATE events SET command = 'x' WHERE owner_id = 'sess' AND seq = ? + 2`,
		"deleted past the cut":  `DELETE FROM events WHERE owner_id = 'sess' AND seq = ? + 2`,
		"first past the cut":    `DELETE FROM events WHERE owner_id = 'sess' AND seq = ? + 1`,
		"refusal before it":     `UPDATE events SET command = 'x' WHERE owner_id = 'sess' AND seq = 1 AND ? > 0`,
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := openTemp(t)
			o := session("sess")
			w := NewWriter(s, DefaultQueue, nil)
			w.Enqueue(key(o), 100, bigEvent(0, DecisionRefused, 10))
			for i := range 10 {
				w.Enqueue(key(o), 100, bigEvent(i+1, "logged", 1<<10))
			}
			flush(t, w)
			w.Close()
			p := DefaultPolicy()
			p.SessionCap = 5 << 10
			sweep(t, s, t0, p, states{})
			cut := intact(t, s, o).Pruned
			if cut < 2 || cut > 9 {
				t.Fatalf("pruned to %d, want a cut inside the chain", cut)
			}
			if res, err := s.db.Exec(tamper, cut); err != nil {
				t.Fatal(err)
			} else if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("tamper touched %d rows", n)
			}
			if rep := verify(t, s, o); rep.OK() {
				t.Fatalf("%s: not detected", name)
			}
		})
	}
}

// TestSweepSkipsUnknownState: a chain whose owner's state cannot be read is
// left alone.
func TestSweepSkipsUnknownState(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess")
	logAt(t, s, o, t0, 3, 0, true)
	failing := func(context.Context, Owner) (ChainState, error) { return ChainState{}, fmt.Errorf("orch.db busy") }
	p := DefaultPolicy()
	p.TotalCap = 1
	res, err := s.Sweep(context.Background(), t0.Add(365*day), p, failing)
	if err == nil || !strings.Contains(err.Error(), "orch.db busy") || res.Records != 0 {
		t.Fatalf("Sweep = %+v, %v; want nothing deleted and the state error reported", res, err)
	}
}

// TestSweepWhileWriting: the audit's round-1 case. Sweeps that cut a live
// chain while the writer keeps appending to it all succeed (each takes the
// write lock first, so neither fails its upgrade), the writer loses
// nothing, and the chain verifies.
func TestSweepWhileWriting(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess-busy")
	w := newWriter(t, s, DefaultQueue)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// About 5,000 events a second, more than sessions send; the
			// race detector slows the writer below that, so 500 there.
			w.Enqueue(key(o), 100, bigEvent(i, "logged", 2<<10))
			if every := 5 - 4*boolInt(raceEnabled); i%every == every-1 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	p := DefaultPolicy()
	p.SessionCap = 64 << 10
	var trimmed int
	for i := range 30 {
		time.Sleep(5 * time.Millisecond)
		res, err := s.Sweep(context.Background(), t0, p, states{}.of)
		if err != nil {
			t.Fatalf("sweep %d while writing: %v", i, err)
		}
		trimmed += res.Trimmed
	}
	close(stop)
	<-done
	flush(t, w)
	if trimmed == 0 || w.Dropped() != 0 {
		t.Fatalf("%d sweeps trimmed, writer dropped %d; want trims and no drops", trimmed, w.Dropped())
	}
	intact(t, s, o)
}

// TestVerifyRejectsForgedPrunedHead: the audit's round-1 case. A pruned
// head written by hand does not excuse records deleted from a chain whose
// earlier records are still there, nor a head whose record is kept with
// another hash; a chain deleted whole under a forged head shows the cut.
func TestVerifyRejectsForgedPrunedHead(t *testing.T) {
	forge := func(t *testing.T, s *Store, seq int64, hash string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO pruned VALUES ('session', 'victim', ?, ?, 'end')`, seq, hash); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("tail deleted", func(t *testing.T) {
		s, _ := openTemp(t)
		o := session("victim")
		writeChain(t, s, o, 10)
		if _, err := s.db.Exec(`DELETE FROM events WHERE owner_id = 'victim' AND seq >= 2`); err != nil {
			t.Fatal(err)
		}
		forge(t, s, 11, "x")
		if rep := verify(t, s, o); rep.OK() {
			t.Fatal("deleting records 2-11 under a forged pruned head went undetected")
		}
	})
	t.Run("head hash", func(t *testing.T) {
		s, _ := openTemp(t)
		o := session("victim")
		logAt(t, s, o, t0, 0, 3, false)
		forge(t, s, 2, "x")
		if rep := verify(t, s, o); rep.OK() {
			t.Fatal("a pruned head whose kept record has another hash went undetected")
		}
	})
	t.Run("whole chain", func(t *testing.T) {
		s, _ := openTemp(t)
		o := session("victim")
		writeChain(t, s, o, 10)
		if _, err := s.db.Exec(`DELETE FROM events WHERE owner_id = 'victim'`); err != nil {
			t.Fatal(err)
		}
		forge(t, s, 11, "x")
		// Not detectable from the file alone; the report and orchd's log
		// of each real cut show it.
		if rep := verify(t, s, o); rep.Pruned != 11 {
			t.Fatalf("report = %+v, want the cut to record 11 shown", rep)
		}
	})
}

func TestSweepClosedStore(t *testing.T) {
	s, _ := openTemp(t)
	_ = s.Close()
	if _, err := s.Sweep(context.Background(), t0, DefaultPolicy(), states{}.of); err == nil {
		t.Fatal("Sweep on a closed store succeeded")
	}
}

// TestRetentionShrinksOldFile: a log made before retention, with
// auto_vacuum off, is converted at open, and deleting records then makes
// the file smaller.
func TestRetentionShrinksOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toollog.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schema + `; PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var mode, version int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum = %d, %v; want 2 (incremental)", mode, err)
	}
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	o := session("sess")
	w := NewWriter(s, DefaultQueue, nil)
	for i := range 300 {
		w.Enqueue(key(o), 100, bigEvent(i, "logged", 8<<10))
	}
	flush(t, w)
	w.Close()
	full, err := s.used(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pages := func() int64 {
		var n int64
		if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := pages()
	res := sweep(t, s, t0.Add(30*day), DefaultPolicy(), states{o: {Ended: t0}})
	if res.Records != 300 || res.Used >= full {
		t.Fatalf("sweep = %+v (in use before %d)", res, full)
	}
	if after := pages(); after*4 > before {
		t.Fatalf("file pages %d -> %d; want the freed pages returned", before, after)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestReadOnNewConnectionDuringWrite: the round-2 audit's case. With
// auto_vacuum set in the connection string every new connection took the
// write lock, so a read waited out the busy timeout while a write was held.
// A read on a fresh connection now returns at once.
func TestReadOnNewConnectionDuringWrite(t *testing.T) {
	s, _ := openTemp(t)
	o := session("sess")
	logAt(t, s, o, t0, 3, 0, true)
	s.db.SetMaxIdleConns(0) // every read below opens a new connection
	err := s.write(context.Background(), func(*sql.Tx) error {
		start := time.Now()
		rep, err := s.Verify(context.Background(), o)
		if took := time.Since(start); err != nil || !rep.OK() || took > time.Second {
			return fmt.Errorf("verify during a write took %v: %+v, %v", took, rep, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("a new log's auto_vacuum = %d, %v; want 2 (incremental)", mode, err)
	}
}

// TestWriteTurnHonoursContext: the round-3 audit's case. A sweep waiting
// for the writer's turn gives up when its context ends instead of waiting
// for the holder.
func TestWriteTurnHonoursContext(t *testing.T) {
	s, _ := openTemp(t)
	logAt(t, s, session("sess"), t0, 3, 0, true)
	if err := s.lockWrite(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.unlockWrite()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Sweep(ctx, t0.Add(365*day), DefaultPolicy(), states{session("sess"): {Ended: t0}}.of)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("Sweep while the turn was held = %v after %v; want a prompt context error", err, time.Since(start))
	}
}
