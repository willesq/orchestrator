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
