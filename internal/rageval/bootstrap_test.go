package rageval

import (
	"fmt"
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
	before, after := report(0, 3, 2, 0, 5, 4, 0, 2), report(1, 1, 1, 2, 1, 1, 3, 1)
	p := Bootstrap("MRR", before, after, RR)
	if p.N != 8 || p.Delta <= 0 || p.Lo <= 0 || p.Hi < p.Lo || p.P != 0 || !p.Significant() {
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
