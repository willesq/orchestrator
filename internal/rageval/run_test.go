package rageval

import "testing"

func TestPairResultsUsesABeforeResultOnce(t *testing.T) {
	before := Report{Results: []Result{{Query: "deploy", Key: "a", Rank: 1}}}
	after := Report{Results: []Result{{Query: "deploy", Key: "a", Rank: 1}, {Query: "deploy", Key: "b", Rank: 5}}}
	got := pairResults(before, after)
	if len(got) != 1 || got[0][1].Key != "a" {
		t.Fatalf("pairs = %+v, want only the key match", got)
	}
	// The query fallback also counts as a use.
	after = Report{Results: []Result{{Query: "deploy", Key: "x", Rank: 2}, {Query: "deploy", Key: "y", Rank: 3}}}
	if got := pairResults(before, after); len(got) != 1 || got[0][1].Key != "x" {
		t.Fatalf("pairs = %+v, want one query-fallback pair", got)
	}
}

// QA r4-6: the pairing does not depend on row order.
func TestPairResultsIgnoresRowOrder(t *testing.T) {
	before := Report{Results: []Result{{Query: "deploy", Key: "a", Rank: 1}}}
	for _, after := range [][]Result{
		{{Query: "deploy", Key: "b", Rank: 5}, {Query: "deploy", Key: "a", Rank: 1}},
		{{Query: "deploy", Key: "a", Rank: 1}, {Query: "deploy", Key: "b", Rank: 5}},
	} {
		got := pairResults(before, Report{Results: after})
		if len(got) != 1 || got[0][1].Key != "a" {
			t.Fatalf("pairs for %+v = %+v, want only a", after, got)
		}
	}
	// Two fallback rows for one before-result: the lower key wins, either way.
	for _, after := range [][]Result{
		{{Query: "deploy", Key: "y", Rank: 3}, {Query: "deploy", Key: "x", Rank: 2}},
		{{Query: "deploy", Key: "x", Rank: 2}, {Query: "deploy", Key: "y", Rank: 3}},
	} {
		if got := pairResults(before, Report{Results: after}); len(got) != 1 || got[0][1].Key != "x" {
			t.Fatalf("fallback pairs for %+v = %+v", after, got)
		}
	}
}
