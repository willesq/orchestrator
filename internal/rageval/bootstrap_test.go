package rageval

import (
	"context"
	"fmt"
	"math"
	"testing"
)

func report(ranks ...int) Report {
	r := Report{Version: ReportVersion}
	for i, k := range ranks {
		r.Results = append(r.Results, Result{Query: fmt.Sprint("q", i), Rank: k})
	}
	return r
}

func TestBootstrap(t *testing.T) {
	// Every case improves: the interval is above zero.
	before, after := report(0, 3, 2, 0, 5, 4, 0, 2, 3, 4, 0, 6), report(1, 1, 1, 2, 1, 1, 3, 1, 1, 2, 1, 1)
	p := Bootstrap("MRR", before, after, RR)
	if p.N != 12 || p.Delta <= 0 || p.Lo <= 0 || p.Hi < p.Lo || p.P != 0 || !p.Significant() {
		t.Fatalf("improvement = %+v", p)
	}
	// Same twice: no change, not significant.
	same := Bootstrap("MRR", before, before, RR)
	if same.Delta != 0 || same.Significant() || same.P != 1 {
		t.Fatalf("no change = %+v", same)
	}
	// A mixed change straddles zero.
	mixed := Bootstrap("recall@3", report(1, 9, 1, 9), report(9, 1, 9, 1), RecallAt3)
	if mixed.Delta != 0 || mixed.Lo >= 0 || mixed.Hi <= 0 || mixed.Significant() {
		t.Fatalf("mixed = %+v", mixed)
	}
	// Only shared queries are paired; none shared is empty.
	if none := Bootstrap("MRR", report(1), Report{Results: []Result{{Query: "other", Rank: 1}}}, RR); none.N != 0 || none.Significant() {
		t.Fatalf("disjoint = %+v", none)
	}
	// Deterministic: the seed is fixed.
	if again := Bootstrap("MRR", before, after, RR); again != p {
		t.Fatalf("not deterministic: %+v vs %+v", again, p)
	}
	if got := CompareMetrics(before, after); len(got) != 3 || got[0].Metric != "MRR" || got[2].Metric != "nDCG@10" {
		t.Fatalf("CompareMetrics = %+v", got)
	}
}

func TestBootstrapPairsEachCaseOnce(t *testing.T) {
	// One before result against the same query three times after: one pair.
	before := Report{Results: []Result{{Query: "q", Rank: 0}}}
	after := Report{Results: []Result{{Query: "q", Rank: 1}, {Query: "q", Rank: 1}, {Query: "q", Rank: 1}}}
	if p := Bootstrap("MRR", before, after, RR); p.N != 1 || p.Significant() {
		t.Fatalf("duplicates = %+v", p)
	}
	// Same query, different filters: two cases.
	a, b := Case{Query: "q", Scope: "work"}, Case{Query: "q"}
	if a.Key() == b.Key() {
		t.Fatal("filters do not distinguish cases")
	}
	before = Report{Results: []Result{{Query: "q", Key: a.Key(), Rank: 1}, {Query: "q", Key: b.Key(), Rank: 0}}}
	after = Report{Results: []Result{{Query: "q", Key: a.Key(), Rank: 1}, {Query: "q", Key: b.Key(), Rank: 1}}}
	if p := Bootstrap("MRR", before, after, RR); p.N != 2 || p.Delta != 0.5 {
		t.Fatalf("keyed = %+v", p)
	}
	if ch := Compare(before, after); len(ch) != 1 || ch[0].From != 0 {
		t.Fatalf("compare = %+v", ch)
	}
	// Corrupt saved ranks score nothing rather than negative.
	if RR(Result{Rank: -1}) != 0 || NDCG10(Result{Rank: -1}) != 0 || within(-1, 10) != 0 {
		t.Fatal("negative rank scored")
	}
	nan := Bootstrap("x", report(1, 2), report(1, 2), func(Result) float64 { return math.NaN() })
	if nan.N != 0 {
		t.Fatalf("NaN metric paired: %+v", nan)
	}
}

func TestSplits(t *testing.T) {
	var s Set
	for i := range 40 {
		s.Cases = append(s.Cases, Case{Query: fmt.Sprint("query ", i), Expect: []string{"u"}, Section: "hard"})
	}
	s.Cases = append(s.Cases, Case{Query: "pinned", Expect: []string{"u"}, Section: "hard", Split: SplitHoldout})
	r := Run(context.Background(), s, 5, func(context.Context, Case, int) ([]Hit, error) { return nil, nil }, nil)
	tune, hold := r.Splits[SplitTune].N, r.Splits[SplitHoldout].N
	if tune+hold != 41 || tune < 10 || hold < 10 || r.SectionSplits["hard/"+SplitTune].N != tune {
		t.Fatalf("splits = %d/%d, %+v", tune, hold, r.SectionSplits)
	}
	if SplitOf(Case{Query: "pinned", Split: SplitHoldout}) != SplitHoldout || SplitOf(Case{Query: "x"}) != SplitOf(Case{Query: " X "}) {
		t.Fatal("SplitOf")
	}
}

func TestPairingAcrossVersionsAndDuplicates(t *testing.T) {
	c := Case{Query: "q"}
	// A run saved before keys existed pairs by query.
	before := Report{Results: []Result{{Query: "q", Rank: 3}}}
	after := Report{Results: []Result{{Query: "q", Key: c.Key(), Rank: 1}, {Query: "q", Key: c.Key(), Rank: 1}}}
	if p := Bootstrap("MRR", before, after, RR); p.N != 1 {
		t.Fatalf("legacy pairing N = %d", p.N)
	}
	if ch := Compare(before, after); len(ch) != 1 || ch[0].From != 3 || ch[0].To != 1 {
		t.Fatalf("compare = %+v", ch)
	}
	if (Case{Query: "x", Tags: []string{"a,b"}}).Key() == (Case{Query: "x", Tags: []string{"a", "b"}}).Key() {
		t.Fatal("tag lists collide")
	}
}

func TestLoadSetRejectsUnknownSplits(t *testing.T) {
	path := t.TempDir() + "/s.jsonl"
	if err := SaveSet(path, Set{Cases: []Case{{Query: "q", Expect: []string{"u"}, Split: "Holdout"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSet(path); err == nil {
		t.Fatal("a mistyped split was accepted")
	}
}

func TestPairingWhenKeysAreSpelledAnotherWay(t *testing.T) {
	before := Report{Results: []Result{{Query: "q", Key: "q\x00\x00\x00", Rank: 3}, {Query: "dup", Key: "a", Rank: 1}, {Query: "dup", Key: "b", Rank: 1}}}
	after := Report{Results: []Result{{Query: "q", Key: Case{Query: "q"}.Key(), Rank: 1}, {Query: "dup", Key: "c", Rank: 2}}}
	if p := Bootstrap("MRR", before, after, RR); p.N != 1 {
		t.Fatalf("N = %d; a unique query pairs, an ambiguous one does not", p.N)
	}
}
