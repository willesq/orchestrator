package rageval

import (
	"strings"
	"testing"
)

const record = `Session 2026-10-26

## Outcome

Shipped it.

## Decisions

- Use ` + "`GITHUB_TOKEN`" + ` for releases instead of the old token: the tap has its own.
- Keep **only** macOS arm64 builds in [Homebrew](https://brew.sh) - Accept red test-bot runs
- ok

## Files and changes

- not a decision at all here
`

func TestDecisions(t *testing.T) {
	got := decisions(record, 5)
	want := []string{
		"Use GITHUB_TOKEN for releases instead of the old token",
		"Keep only macOS arm64 builds in Homebrew",
		"Accept red test-bot runs",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("decisions = %q", got)
	}
	if got := decisions(record, 1); len(got) != 1 {
		t.Fatalf("limit ignored: %q", got)
	}
	if got := decisions("## Decisions\n\n- "+strings.Repeat("word ", 20), 3); len(strings.Fields(got[0])) != maxQueryWords {
		t.Fatalf("not cut to %d words: %q", maxQueryWords, got)
	}
	if decisions("no headings here\n- a b c", 3) != nil {
		t.Fatal("bullets outside a Decisions section taken")
	}
}

func TestHeadingsAndIdentifiers(t *testing.T) {
	doc := "# Title\n\n## Overview\n\n## 1. How the sync protocol works\n\n```\n## not a heading in code\n```\n" +
		"### Two words\n\n## ⚙️ Keyword vectors and fusion\n\nSet `rag.search.keyword_weight` or `ProcessActivity`, not `plain` or `v1.2` or `https://x.y/z`.\n"
	if got := headings(doc); strings.Join(got, "|") != "How the sync protocol works|Keyword vectors and fusion" {
		t.Fatalf("headings = %q", got)
	}
	if got := identifiers(doc); strings.Join(got, "|") != "rag.search.keyword_weight|ProcessActivity" {
		t.Fatalf("identifiers = %q", got)
	}
}

func TestSeed(t *testing.T) {
	records := []SeedDoc{
		{URI: "session://a/record", Text: record},
		{URI: "session://b/record", Text: "no decisions"},
	}
	docs := []SeedDoc{
		{URI: "repo://o/r/a.md", Text: "## Shared heading for both\n\n## Unique heading of doc a\n\nUses `alpha_only_ident` and `shared_ident`."},
		{URI: "repo://o/r/b.md", Text: "## Shared heading for both\n\nUses `shared_ident`, and mentions beta_only_ident in prose."},
		{URI: "repo://o/r/c.md", Text: "Also names `beta_only_ident`."},
	}
	cases := Seed(records, docs, SeedOpts{Decisions: 2, Headings: 5, Identifiers: 5})
	by := map[string][]Case{}
	for _, c := range cases {
		by[c.Section] = append(by[c.Section], c)
	}
	if len(by[SectionDecision]) != 2 || by[SectionDecision][0].Expect[0] != "session://a/*" ||
		!strings.HasPrefix(by[SectionDecision][0].Query, "what did we decide about ") {
		t.Fatalf("decision cases = %+v", by[SectionDecision])
	}
	if len(by[SectionHeading]) != 1 || by[SectionHeading][0].Query != "Unique heading of doc a" {
		t.Fatalf("heading cases = %+v", by[SectionHeading])
	}
	// shared_ident is in two docs; beta_only_ident is backticked in c but
	// named in b's prose, so it is not distinctive either.
	if len(by[SectionIdentifier]) != 1 || by[SectionIdentifier][0].Query != "alpha_only_ident" {
		t.Fatalf("identifier cases = %+v", by[SectionIdentifier])
	}
	// The same store seeds the same set.
	again := Seed(records, docs, SeedOpts{Decisions: 2, Headings: 5, Identifiers: 5})
	if len(again) != len(cases) || again[0].String() != cases[0].String() {
		t.Fatal("seed is not deterministic")
	}
	if none := Seed(records, docs, SeedOpts{}); len(none) != 0 {
		t.Fatalf("zero limits seeded %d", len(none))
	}
}

func (c Case) String() string { return c.Section + ":" + c.Query }

func TestMergeSeedKeepsHandWritten(t *testing.T) {
	s := Set{Cases: []Case{
		{Query: "hand", Expect: []string{"u"}, Section: SectionHard},
		{Query: "old seed", Expect: []string{"u"}, Section: SectionHeading},
		{Query: "mine", Expect: []string{"u"}},
	}}
	got := MergeSeed(s, []Case{{Query: "new seed", Expect: []string{"v"}, Section: SectionHeading}, {Query: "HAND", Expect: []string{"v"}, Section: SectionHeading}})
	var qs []string
	for _, c := range got.Cases {
		qs = append(qs, c.Query)
	}
	if strings.Join(qs, "|") != "hand|mine|new seed" {
		t.Fatalf("merged = %q", qs)
	}
	if !IsSeedSection(SectionIdentifier) || IsSeedSection(SectionHard) || IsSeedSection("") {
		t.Fatal("IsSeedSection")
	}
}
