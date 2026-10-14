package rag

import (
	"strings"
	"testing"
)

const readme = `Intro paragraph before any heading.

# Orchestrator

Local-first memory for agents.

## Install

Use Homebrew.

### Homebrew

brew install willesq/tap/orch

### From source

` + "```sh\n# not a heading\nmake build\n```" + `

## Usage

Run orch search.
`

func TestSections(t *testing.T) {
	secs := sections(readme)
	var got []string
	for _, s := range secs {
		got = append(got, strings.Join(s.full(), pathSep))
	}
	want := []string{"", "Orchestrator", "Orchestrator › Install", "Orchestrator › Install › Homebrew",
		"Orchestrator › Install › From source", "Orchestrator › Usage"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sections = %q", got)
	}
	// The "# not a heading" line inside the fence stays in its section.
	if !strings.Contains(strings.Join(secs[4].lines, "\n"), "# not a heading") {
		t.Fatalf("fenced line lost: %q", secs[4].lines)
	}
}

func TestIsMarkdown(t *testing.T) {
	for text, want := range map[string]bool{
		readme:                            true,
		"just words here":                 false,
		"```\n# inside a fence\n```":      false,
		"#hashtag is not a heading":       false,
		"   ## indented three is fine":    true,
		"    ## indented four is code":    false,
		"Title\n=====\nsetext ignored":    false,
		"## Closing hashes ##\nbody":      true,
		"~~~\n# tilde fence\n~~~\n# Real": true,
	} {
		if got := isMarkdown(text); got != want {
			t.Errorf("isMarkdown(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestMarkdownChunksPackSubtrees(t *testing.T) {
	got := markdownChunks(readme, 400, 60)
	// Everything fits in one chunk, but the preface and each top-level subtree
	// stay apart from unrelated parts.
	var heads []string
	for _, c := range got {
		heads = append(heads, c.heading)
	}
	if strings.Join(heads, "|") != "|Orchestrator" {
		t.Fatalf("headings = %q", heads)
	}
	body := got[1].text
	for _, want := range []string{"# Orchestrator", "## Install", "### Homebrew", "```sh\n# not a heading\nmake build\n```", "## Usage"} {
		if !strings.Contains(body, want) {
			t.Errorf("chunk lost %q:\n%s", want, body)
		}
	}
}

func TestMarkdownChunksSplitAtSections(t *testing.T) {
	got := markdownChunks(readme, 8, 2)
	var heads []string
	for _, c := range got {
		heads = append(heads, c.heading)
		if words(c.text) > 8+6 { // the size plus a path prefix
			t.Errorf("chunk too large (%d words): %q", words(c.text), c.text)
		}
	}
	// From source no longer fits once its context line counts against the
	// size, so it splits in two; its fenced block stays whole.
	want := "|Orchestrator|Orchestrator › Install|Orchestrator › Install › Homebrew|Orchestrator › Install › From source|Orchestrator › Install › From source|Orchestrator › Usage"
	if strings.Join(heads, "|") != want {
		t.Fatalf("headings = %q", strings.Join(heads, "|"))
	}
	// A nested section carries its ancestors, so it is findable out of context.
	for _, c := range got {
		if c.heading == "Orchestrator › Install › From source" &&
			!strings.HasPrefix(c.text, "[section: Orchestrator › Install]\n\n### From source") {
			t.Fatalf("nested chunk = %q", c.text)
		}
	}
}

func TestMarkdownChunksSplitOversizedSections(t *testing.T) {
	para := func(n int, w string) string { return strings.TrimSpace(strings.Repeat(w+" ", n)) }
	doc := "# Big\n\n" + para(6, "alpha") + "\n\n" + para(6, "beta") + "\n\n```\n" + para(6, "code") + "\n```\n\n" + para(30, "gamma")
	got := markdownChunks(doc, 10, 2)
	if len(got) < 4 {
		t.Fatalf("got %d chunks: %q", len(got), got)
	}
	for _, c := range got {
		if !strings.HasPrefix(c.text, "# Big") || c.heading != "Big" {
			t.Errorf("piece lost its heading: %q", c.text)
		}
		if strings.Contains(c.text, "alpha") && strings.Contains(c.text, "beta") {
			t.Errorf("paragraphs that do not fit together were joined: %q", c.text)
		}
	}
	var fenced string
	for _, c := range got {
		if strings.Contains(c.text, "code") {
			fenced = c.text
		}
	}
	if !strings.Contains(fenced, "```\n"+para(6, "code")+"\n```") {
		t.Fatalf("code block was split: %q", fenced)
	}
	var gamma int
	for _, c := range got {
		gamma += strings.Count(c.text, "gamma")
	}
	if gamma < 30 {
		t.Fatalf("oversized paragraph lost words: %d", gamma)
	}
}

func TestMarkdownChunksEdgeCases(t *testing.T) {
	if got := markdownChunks("", 10, 0); len(got) != 0 {
		t.Fatalf("empty = %q", got)
	}
	if got := markdownChunks("# Only a heading", 0, 0); len(got) != 1 || got[0].text != "# Only a heading" {
		t.Fatalf("heading only = %q", got)
	}
	unterminated := "# A\n```\n# B inside an unterminated fence\n"
	if got := markdownChunks(unterminated, 50, 0); len(got) != 1 || got[0].heading != "A" {
		t.Fatalf("unterminated fence = %+v", got)
	}
	crlf := markdownChunks("# A\r\n\r\nbody\r\n## B\r\nmore", 50, 0)
	if len(crlf) != 1 || strings.Contains(crlf[0].text, "\r") {
		t.Fatalf("crlf = %q", crlf)
	}
	if got := tidy([]string{"a  ", "", "", "", "b", ""}); got != "a\n\nb" {
		t.Fatalf("tidy = %q", got)
	}
}

func TestMarkdownChunksBoundAmplification(t *testing.T) {
	long := strings.Repeat("x", 200_000)
	var b strings.Builder
	b.WriteString("# " + long + "\n\n")
	for i := 0; i < 1000; i++ {
		b.WriteString("## s\n\n" + strings.Repeat("w ", 250) + "\n\n")
	}
	in := b.String()
	total := 0
	for _, c := range markdownChunks(in, 400, 60) {
		total += len(c.text)
		if len(c.heading) > 4*(maxTitle+10) {
			t.Fatalf("heading payload %d bytes", len(c.heading))
		}
	}
	if total > 2*len(in) {
		t.Fatalf("output %d bytes for %d bytes of input", total, len(in))
	}
	// A long heading line over one huge paragraph is bounded too.
	in2 := "# " + long + "\n\n" + strings.Repeat("a ", 200_000)
	total = 0
	for _, c := range markdownChunks(in2, 400, 60) {
		total += len(c.text)
	}
	if total > 3*len(in2) {
		t.Fatalf("output %d bytes for %d bytes of input", total, len(in2))
	}
}

func TestMarkdownHeadingsAreAccurate(t *testing.T) {
	got := markdownChunks("# A\n\na\n\n## B\n\nb\n\n## C\n\nc\n\n### E\n\ne\n\n# Z\n\nz", 400, 0)
	if len(got) != 2 || got[0].heading != "A" || got[1].heading != "Z" || strings.Contains(got[0].text, "# Z") {
		t.Fatalf("chunks = %+v", got)
	}
	// Packed siblings report only the path they share.
	sib := markdownChunks("# A\n\n## B\n\nb\n\n### B1\n\nb1\n\n### B2\n\nb2", 6, 0)
	for _, c := range sib {
		if strings.Contains(c.text, "B1") && strings.Contains(c.text, "B2") && c.heading != "A › B" {
			t.Fatalf("packed siblings heading = %q", c.heading)
		}
	}
}

func TestMarkdownTitlesCannotFakePaths(t *testing.T) {
	got := markdownChunks("# Official > Security Policy › Approved\u200b\x07\n\n## Do this\n\n"+strings.Repeat("w ", 30), 10, 0)
	for _, c := range got {
		if strings.Count(c.heading, pathSep) > 1 || strings.ContainsAny(c.heading, ">\u200b\x07") {
			t.Fatalf("heading = %q", c.heading)
		}
	}
	if !strings.HasPrefix(got[len(got)-1].text, "[section: Official - Security Policy - Approved]") {
		t.Fatalf("context line = %q", got[len(got)-1].text)
	}
}

func TestMarkdownLongHeadingOnlyKeepsWords(t *testing.T) {
	got := markdownChunks("# "+strings.TrimSpace(strings.Repeat("word ", 500)), 50, 0)
	if len(got) == 0 {
		t.Fatal("heading-only section dropped")
	}
}

func TestFences(t *testing.T) {
	doc := "# Real\n\n````md\n```\n# NotAHeading\n```\n````\n\n# After"
	var heads []string
	for _, s := range sections(doc) {
		heads = append(heads, s.title)
	}
	if strings.Join(heads, ",") != "Real,After" {
		t.Fatalf("headings = %q", heads)
	}
	if _, ok := openFence("```foo`bar"); ok {
		t.Fatal("a backtick in a backtick fence's info string opened a fence")
	}
	if _, ok := openFence("~~~ `ok`"); !ok {
		t.Fatal("tilde fence with backticks in its info string should open")
	}
	f, _ := openFence("```go")
	for line, want := range map[string]bool{"```": true, "````": true, "```bash": false, "    ```": false, "``": false} {
		if got := f.closes(line); got != want {
			t.Errorf("closes(%q) = %v", line, got)
		}
	}
	if got := tidy([]string{"```", "a  ", "", "", "b", "```", "", "", "c"}); got != "```\na  \n\n\nb\n```\n\nc" {
		t.Fatalf("tidy changed fenced code: %q", got)
	}
}
