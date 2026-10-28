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
		{Query: "first", Expect: []string{"repo://a/README.md"}, Section: "hard"},
		{Query: "dup chunks rank by position", Expect: []string{"repo://c"}},
		{Query: "prefix", Expect: []string{"session://*"}},
		{Query: "any of several", Expect: []string{"x://nope", "repo://b"}},
		{Query: "missed", Expect: []string{"repo://never"}},
		{Query: "boom", Expect: []string{"repo://a"}},
	}}
	r := Run(context.Background(), s, 10, fixed(map[string][]string{
		"first":                       {"repo://a/README.md", "repo://b"},
		"dup chunks rank by position": {"repo://a", "repo://a", "repo://a", "repo://c"},
		"prefix":                      {"repo://a", "session://123/summary"},
		"any of several":              {"repo://z", "repo://y", "repo://b"},
		"missed":                      {"repo://a"},
	}), nil)
	want := []int{1, 4, 2, 3, 0, 0}
	for i, res := range r.Results {
		if res.Rank != want[i] || res.SourceRank != want[i] {
			t.Errorf("%q rank = %d/%d, want %d (top %v)", res.Query, res.Rank, res.SourceRank, want[i], res.Top)
		}
	}
	if r.Results[1].Top[0] != "repo://a" || len(r.Results[1].Top) != 2 {
		t.Errorf("duplicates not collapsed in Top: %v", r.Results[1].Top)
	}
	if r.Results[5].Err == "" || r.Errors != 1 {
		t.Error("search error not recorded")
	}
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	if !near(r.RecallAt1, 1.0/6) || !near(r.RecallAt3, 3.0/6) || !near(r.RecallAt10, 4.0/6) ||
		!near(r.MRR, (1+0.25+0.5+1.0/3)/6) || !near(r.SourceHit, 4.0/6) {
		t.Fatalf("metrics %+v", r.Metrics)
	}
	if r.Version != ReportVersion || r.Sections["hard"].N != 1 || r.Sections["hard"].RecallAt1 != 1 {
		t.Fatalf("version/sections = %d %+v", r.Version, r.Sections)
	}
	if empty := Run(context.Background(), Set{}, 5, fixed(nil), nil); empty.MRR != 0 || empty.TopK != 5 || empty.Sections != nil {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestSpanAndInjection(t *testing.T) {
	s := Set{Cases: []Case{{Query: "q", Expect: []string{"u://a"}, Span: "The Decision"}}}
	search := func(context.Context, Case, int) ([]Hit, error) {
		return []Hit{{SourceURI: "u://a", Text: "intro"}, {SourceURI: "u://b"}, {SourceURI: "u://a", Text: "so the decision was made"}}, nil
	}
	calls := 0
	r := Run(context.Background(), s, 5, search, func(c Case, hits []Hit) int { calls++; return 41 })
	res := r.Results[0]
	if res.SourceRank != 1 || res.Rank != 3 || res.InjectedTokens != 11 || calls != 1 || r.InjectedTokens != 11 {
		t.Fatalf("result = %+v", res)
	}
	if NDCG10(res) != 0.5 || RR(res) != 1.0/3 || RecallAt3(res) != 1 || NDCG10(Result{Rank: 11}) != 0 {
		t.Fatal("per-case metrics wrong")
	}
	if p := percentile([]float64{1, 2, 3, 4}, 0.95); p != 4 {
		t.Fatalf("p95 = %v", p)
	}
	if percentile(nil, 0.5) != 0 {
		t.Fatal("empty percentile")
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
	r := Report{TopK: 5, Results: []Result{{Query: "q", Rank: 2}}, Metrics: Metrics{MRR: 0.5}}
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
	}, nil)
	if calls != 1 || len(r.Results) != 1 {
		t.Fatalf("calls = %d, results = %d; want the run to stop after the cancel", calls, len(r.Results))
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eval", "golden.jsonl")
	in := Set{Cases: []Case{
		{Query: "what did we decide about <x> & y", Expect: []string{"session://s/*"}, Section: SectionHard, Span: "x"},
		{Query: "ident_name", Expect: []string{"repo://o/r/a.md"}, Tags: []string{"repo:r"}},
	}}
	if err := SaveSet(path, in); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "\n") != 2 || strings.Contains(string(b), "\\u003c") {
		t.Fatalf("not one readable case per line:\n%s", b)
	}
	// Comments and blank lines are skipped.
	if err := os.WriteFile(path, append([]byte("# hand-written\n\n"), b...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := LoadSet(path)
	if err != nil || len(out.Cases) != 2 || out.Cases[0].Span != "x" || out.Cases[1].Tags[0] != "repo:r" {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
	for name, body := range map[string]string{
		"bad json":      "{",
		"unknown field": `{"query":"q","expect":["u"],"bogus":1}`,
		"no expect":     `{"query":"q"}`,
	} {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSet(path); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	var many strings.Builder
	for i := 0; i <= maxCases; i++ {
		many.WriteString(`{"query":"q","expect":["u"]}` + "\n")
	}
	if err := os.WriteFile(path, []byte(many.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSet(path); err == nil || !strings.Contains(err.Error(), "limit is") {
		t.Fatalf("too many jsonl cases err = %v", err)
	}
	if !IsJSONL("a.JSONL") || IsJSONL("a.yaml") {
		t.Fatal("IsJSONL")
	}
}
