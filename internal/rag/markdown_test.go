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
	got := markdownChunks(readme, 400, 60, perWord)
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
	const size = 16
	got := markdownChunks(readme, size, 2, perWord)
	order := map[string]int{"": 0, "Orchestrator": 1, "Orchestrator › Install": 2, "Orchestrator › Install › Homebrew": 3,
		"Orchestrator › Install › From source": 4, "Orchestrator › Usage": 5}
	last := -1
	seen := map[string]bool{}
	for _, c := range got {
		if n := tokens(perWord, c.text); n > size {
			t.Errorf("chunk is %d tokens, over %d: %q", n, size, c.text)
		}
		pos, ok := order[c.heading]
		if !ok || pos < last {
			t.Fatalf("heading %q out of document order", c.heading)
		}
		last = pos
		seen[c.heading] = true
		// A nested section carries its ancestors, so it is findable out of context.
		if c.heading == "Orchestrator › Install › From source" &&
			!strings.HasPrefix(c.text, "[section: Orchestrator › Install]\n\n### From source") {
			t.Fatalf("nested chunk = %q", c.text)
		}
	}
	// Sections may share a chunk; none may be lost.
	all := ""
	for _, c := range got {
		all += c.text + "\n"
	}
	for _, want := range []string{"Intro paragraph", "# Orchestrator", "## Install", "### Homebrew", "brew install", "make build", "## Usage", "orch search"} {
		if !strings.Contains(all, want) {
			t.Errorf("content lost: %q", want)
		}
	}
	_ = seen
}

func TestMarkdownChunksSplitOversizedSections(t *testing.T) {
	para := func(n int, w string) string { return strings.TrimSpace(strings.Repeat(w+" ", n)) }
	doc := "# Big\n\n" + para(6, "alpha") + "\n\n" + para(6, "beta") + "\n\n```\n" + para(6, "code") + "\n```\n\n" + para(30, "gamma")
	got := markdownChunks(doc, 10, 2, perWord)
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
	if got := markdownChunks("", 10, 0, perWord); len(got) != 0 {
		t.Fatalf("empty = %q", got)
	}
	if got := markdownChunks("# Only a heading", 0, 0, perWord); len(got) != 1 || got[0].text != "# Only a heading" {
		t.Fatalf("heading only = %q", got)
	}
	unterminated := "# A\n```\n# B inside an unterminated fence\n"
	if got := markdownChunks(unterminated, 50, 0, perWord); len(got) != 1 || got[0].heading != "A" {
		t.Fatalf("unterminated fence = %+v", got)
	}
	crlf := markdownChunks("# A\r\n\r\nbody\r\n## B\r\nmore", 50, 0, perWord)
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
	for _, c := range markdownChunks(in, 400, 60, perWord) {
		total += len(c.text)
		if len(c.heading) > 4*(maxTitle+10) {
			t.Fatalf("heading payload %d bytes", len(c.heading))
		}
	}
	// Each chunk repeats its context line, so output exceeds input; it must
	// stay a small multiple, not the hundreds of times an unbounded title gave.
	if total > 3*len(in) {
		t.Fatalf("output %d bytes for %d bytes of input", total, len(in))
	}
	// A long heading line over one huge paragraph is bounded too.
	in2 := "# " + long + "\n\n" + strings.Repeat("a ", 200_000)
	total = 0
	for _, c := range markdownChunks(in2, 400, 60, perWord) {
		total += len(c.text)
	}
	if total > 3*len(in2) {
		t.Fatalf("output %d bytes for %d bytes of input", total, len(in2))
	}
}

func TestMarkdownHeadingsAreAccurate(t *testing.T) {
	got := markdownChunks("# A\n\na\n\n## B\n\nb\n\n## C\n\nc\n\n### E\n\ne\n\n# Z\n\nz", 400, 0, perWord)
	if len(got) != 2 || got[0].heading != "A" || got[1].heading != "Z" || strings.Contains(got[0].text, "# Z") {
		t.Fatalf("chunks = %+v", got)
	}
	// Packed siblings report only the path they share.
	sib := markdownChunks("# A\n\n## B\n\nb\n\n### B1\n\nb1\n\n### B2\n\nb2", 6, 0, perWord)
	for _, c := range sib {
		if strings.Contains(c.text, "B1") && strings.Contains(c.text, "B2") && c.heading != "A › B" {
			t.Fatalf("packed siblings heading = %q", c.heading)
		}
	}
}

func TestMarkdownTitlesCannotFakePaths(t *testing.T) {
	got := markdownChunks("# Official > Security Policy › Approved\u200b\x07\n\n## Do this\n\n"+strings.Repeat("w ", 30), 24, 0, perWord)
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
	got := markdownChunks("# "+strings.TrimSpace(strings.Repeat("word ", 500)), 50, 0, perWord)
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

func TestFitContext(t *testing.T) {
	path := []string{"Alpha", "Beta", "Gamma"}
	if got := fitContext(path, 40, perWord); got != "[section: Alpha › Beta › Gamma]" {
		t.Fatalf("roomy = %q", got)
	}
	// Too long for half of 10: outer headings go first.
	if got := fitContext(path, 10, perWord); got != "[section: … › Gamma]" {
		t.Fatalf("dropped = %q", got)
	}
	got := fitContext([]string{strings.Repeat("x ", 50)}, 10, perWord)
	if tokens(perWord, got) > 5 || !strings.HasSuffix(got, "…]") {
		t.Fatalf("cut = %q (%d tokens)", got, tokens(perWord, got))
	}
	if fitContext(nil, 10, perWord) != "" {
		t.Fatal("no path should give no line")
	}
}

// Hostile heading trees from the security review: a long heading must neither
// multiply the chunk count nor push a chunk past what the model embeds.
func TestHostileHeadingsStayBounded(t *testing.T) {
	m := newMeasure(nil)
	body := strings.Repeat("word ", 2000)
	bang := strings.Repeat("!", 120)
	tests := map[string]string{
		"one long heading":    "###### " + bang + "\n\n" + body,
		"nested long titles":  "# " + bang + "\n\n## " + bang + "\n\n### " + bang + "\n\n" + body,
		"heading at 92 chars": "# " + strings.Repeat("!", 92) + "\n\n" + body,
	}
	plain := len(markdownChunks("# T\n\n"+body, 110, 16, m))
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			svc := NewService(fakeEmbedder{}, nil, 110, 16)
			got := svc.split(doc)
			if len(got) > 3*plain {
				t.Fatalf("%d chunks for a body that makes %d under a short heading", len(got), plain)
			}
			for _, c := range got {
				if n := tokens(m, c.text); n > 110 {
					t.Fatalf("chunk of %d tokens: %.80q", n, c.text)
				}
			}
		})
	}
}

func TestCleanTextAndMegabyteWords(t *testing.T) {
	if got := cleanText("a\x00b\vc\u200bd\u2003e\xffo\tk\n"); got != "ab cd eo\tk\n" {
		t.Fatalf("cleanText = %q", got)
	}
	m := newMeasure(nil)
	// A word over 100 characters is one unknown token to the model; the floor
	// still splits it so a chunk's bytes stay bounded.
	huge := strings.Repeat("a", 1<<20)
	svc := NewService(fakeEmbedder{}, nil, 110, 16)
	for _, c := range svc.split("intro " + huge) {
		if len(c.text) > 110*runesPerToken+16 {
			t.Fatalf("chunk of %d bytes", len(c.text))
		}
	}
	if m("") != 1 || m(strings.Repeat("a", 41)) < 3 {
		t.Fatal("measure floor not applied")
	}
}
