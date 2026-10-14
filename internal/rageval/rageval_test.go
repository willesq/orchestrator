package rageval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixed(byQuery map[string][]string) Searcher {
	return func(_ context.Context, c Case, _ int) ([]Hit, error) {
		if c.Query == "boom" {
			return nil, errors.New("orchd down")
		}
		var hits []Hit
		for _, u := range byQuery[c.Query] {
			hits = append(hits, Hit{SourceURI: u})
		}
		return hits, nil
	}
}

func TestRunScores(t *testing.T) {
	s := Set{Cases: []Case{
		{Query: "first", Expect: []string{"repo://a/README.md"}},
		{Query: "dup chunks count once", Expect: []string{"repo://c"}},
		{Query: "prefix", Expect: []string{"session://*"}},
		{Query: "any of several", Expect: []string{"x://nope", "repo://b"}},
		{Query: "missed", Expect: []string{"repo://never"}},
		{Query: "boom", Expect: []string{"repo://a"}},
	}}
	r := Run(context.Background(), s, 10, fixed(map[string][]string{
		"first":                 {"repo://a/README.md", "repo://b"},
		"dup chunks count once": {"repo://a", "repo://a", "repo://a", "repo://c"},
		"prefix":                {"repo://a", "session://123/summary"},
		"any of several":        {"repo://z", "repo://y", "repo://b"},
		"missed":                {"repo://a"},
	}))
	want := []int{1, 2, 2, 3, 0, 0}
	for i, res := range r.Results {
		if res.Rank != want[i] {
			t.Errorf("%q rank = %d, want %d (top %v)", res.Query, res.Rank, want[i], res.Top)
		}
	}
	if r.Results[1].Top[0] != "repo://a" || len(r.Results[1].Top) != 2 {
		t.Errorf("duplicates not collapsed: %v", r.Results[1].Top)
	}
	if r.Results[5].Err == "" {
		t.Error("search error not recorded")
	}
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	if !near(r.HitAt1, 1.0/6) || !near(r.Recall, 4.0/6) || !near(r.MRR, (1+0.5+0.5+1.0/3)/6) {
		t.Fatalf("hit@1 %v recall %v mrr %v", r.HitAt1, r.Recall, r.MRR)
	}
	if empty := Run(context.Background(), Set{}, 5, fixed(nil)); empty.MRR != 0 || empty.TopK != 5 {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestCompare(t *testing.T) {
	before := Report{Results: []Result{{Query: "a", Rank: 3}, {Query: "b", Rank: 1}, {Query: "c", Rank: 0}, {Query: "gone", Rank: 1}}}
	after := Report{Results: []Result{{Query: "a", Rank: 1}, {Query: "b", Rank: 0}, {Query: "c", Rank: 0}, {Query: "new", Rank: 2}}}
	got := Compare(before, after)
	if len(got) != 2 || got[0] != (Change{"a", 3, 1}) || got[1] != (Change{"b", 1, 0}) {
		t.Fatalf("changes = %+v", got)
	}
	if !got[0].Better() || got[1].Better() || !(Change{From: 0, To: 4}).Better() {
		t.Fatal("Better wrong")
	}
}

func TestSetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "set.yaml")
	if s, err := LoadSet(path); err != nil || len(s.Cases) != 0 {
		t.Fatalf("missing file = %+v, %v", s, err)
	}
	in := Set{Cases: []Case{{Query: "q", Expect: []string{"u"}, Scope: "work", Tags: []string{"repo:x"}}}}
	if err := SaveSet(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadSet(path)
	if err != nil || len(out.Cases) != 1 || out.Cases[0].Scope != "work" || out.Cases[0].Tags[0] != "repo:x" {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
	for name, body := range map[string]string{
		"bad yaml":  "cases: [",
		"no expect": "cases:\n  - query: q\n",
		"no query":  "cases:\n  - expect: [u]\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSet(path); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRuns(t *testing.T) {
	dir := t.TempDir()
	r := Report{TopK: 5, Results: []Result{{Query: "q", Rank: 2}}, MRR: 0.5}
	if err := SaveRun(dir, "baseline-1", r); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRun(dir, "baseline-1")
	if err != nil || got.MRR != 0.5 || got.Results[0].Rank != 2 {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	if _, err := LoadRun(dir, "absent"); err == nil || !strings.Contains(err.Error(), "no saved run") {
		t.Fatalf("absent err = %v", err)
	}
	for _, bad := range []string{"../x", "a/b", "", ".hidden", "a..b"} {
		if err := SaveRun(dir, bad, r); err == nil {
			t.Errorf("run name %q accepted", bad)
		}
		if _, err := LoadRun(dir, bad); err == nil {
			t.Errorf("run name %q loaded", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRun(dir, "broken"); err == nil {
		t.Fatal("corrupt run accepted")
	}
	info, err := os.Stat(filepath.Join(dir, "baseline-1.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("run file mode = %v, %v", info.Mode().Perm(), err)
	}
}

func TestLimits(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, []byte("cases: []\n#"+strings.Repeat("x", maxFileBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSet(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized set err = %v", err)
	}
	var b strings.Builder
	b.WriteString("cases:\n")
	for i := 0; i <= maxCases; i++ {
		b.WriteString("  - {query: q, expect: [u]}\n")
	}
	many := filepath.Join(dir, "many.yaml")
	if err := os.WriteFile(many, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSet(many); err == nil || !strings.Contains(err.Error(), "limit is") {
		t.Fatalf("too many cases err = %v", err)
	}
	laughs := filepath.Join(dir, "laughs.yaml")
	bomb := "a: &a [x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\nd: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\ncases: [*d,*d,*d,*d,*d,*d,*d,*d,*d]\n"
	if err := os.WriteFile(laughs, []byte(bomb), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSet(laughs); err == nil {
		t.Fatal("alias bomb accepted")
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	s := Set{Cases: []Case{{Query: "a", Expect: []string{"u"}}, {Query: "b", Expect: []string{"u"}}, {Query: "c", Expect: []string{"u"}}}}
	r := Run(ctx, s, 5, func(context.Context, Case, int) ([]Hit, error) {
		calls++
		cancel()
		return nil, nil
	})
	if calls != 1 || len(r.Results) != 1 {
		t.Fatalf("calls = %d, results = %d; want the run to stop after the cancel", calls, len(r.Results))
	}
}
