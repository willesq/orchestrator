package rag

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitBlockKeepsLines(t *testing.T) {
	var lines []string
	for i := range 12 {
		lines = append(lines, fmt.Sprintf("- item %d has four words", i)) // 6 words
	}
	pieces := splitBlock(lines, 20, 6, perWord)
	if len(pieces) < 2 {
		t.Fatalf("pieces = %d, want the block split", len(pieces))
	}
	for i, p := range pieces {
		if n := perWordCount(p); n > 20 {
			t.Errorf("piece %d is %d tokens, over 20", i, n)
		}
		for _, l := range strings.Split(p, "\n") {
			if !strings.HasPrefix(l, "- item ") {
				t.Errorf("piece %d line %q is not a whole list line", i, l)
			}
		}
	}
	// Each piece after the first starts with the previous piece's last line.
	for i := 1; i < len(pieces); i++ {
		prev := strings.Split(pieces[i-1], "\n")
		if !strings.HasPrefix(pieces[i], prev[len(prev)-1]) {
			t.Errorf("piece %d does not overlap with the line before it", i)
		}
	}
	// Every line appears.
	all := strings.Join(pieces, "\n")
	for _, l := range lines {
		if !strings.Contains(all, l) {
			t.Errorf("line %q lost", l)
		}
	}
}

func TestSplitBlockEdges(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 50))
	tests := []struct {
		name    string
		lines   []string
		size    int
		overlap int
		check   func(t *testing.T, pieces []string)
	}{
		{
			name: "a single overlong line falls back to word windows", lines: []string{"short line", long, "after"}, size: 20,
			check: func(t *testing.T, pieces []string) {
				if pieces[0] != "short line" || pieces[len(pieces)-1] != "after" || len(pieces) < 4 {
					t.Errorf("pieces = %q", pieces)
				}
			},
		},
		{
			name: "no overlap carries nothing", lines: []string{"a b c", "d e f", "g h i"}, size: 6,
			check: func(t *testing.T, pieces []string) {
				if strings.Join(pieces, "|") != "a b c\nd e f|g h i" {
					t.Errorf("pieces = %q", pieces)
				}
			},
		},
		{
			name: "an overlap larger than the budget never stalls", lines: []string{"a b c d", "e f g h", "i j k l"}, size: 5, overlap: 100,
			check: func(t *testing.T, pieces []string) {
				if len(pieces) != 3 {
					t.Errorf("pieces = %q", pieces)
				}
			},
		},
		{name: "empty", lines: nil, size: 10, check: func(t *testing.T, pieces []string) {
			if len(pieces) != 0 {
				t.Errorf("pieces = %q", pieces)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.check(t, splitBlock(tt.lines, tt.size, tt.overlap, perWord)) })
	}
}

func TestOversizedCodeFenceRebuildsWithItsLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Install\n\n```sh\n")
	for i := range 30 {
		fmt.Fprintf(&b, "brew install thing%d   # step %d\n", i, i)
	}
	b.WriteString("```\n")
	chunks := markdownChunks(b.String(), 40, 8, perWord)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want the fence split", len(chunks))
	}
	var stored []storedChunk
	for i, c := range chunks {
		stored = append(stored, storedChunk{id: fmt.Sprint(i), index: i, text: c.text})
	}
	text, malformed := rebuild(stored)
	if malformed {
		t.Fatal("rebuild flagged malformed")
	}
	for i := range 30 {
		line := fmt.Sprintf("brew install thing%d   # step %d", i, i)
		if !strings.Contains(text, line+"\n") && !strings.HasSuffix(strings.TrimSpace(text), line) {
			t.Errorf("rebuilt text lost the line %q:\n%s", line, text)
			break
		}
	}
}

func perWordCount(s string) int { return len(strings.Fields(s)) }

func TestRebuildContinuesASplitBlock(t *testing.T) {
	stored := []storedChunk{
		{id: "0", index: 0, text: "## List\n\n- **Overview:**\n- second item\n- third item"},
		{id: "1", index: 1, text: "## List\n\n- third item\n- fourth item"},
		// A new block that only shares a fence line with the one before it.
		{id: "2", index: 2, text: "```sh\nmake\n```"},
		{id: "3", index: 3, text: "```sh\nmake test\n```"},
	}
	text, malformed := rebuild(stored)
	if malformed {
		t.Fatal("malformed")
	}
	want := "## List\n\n- **Overview:**\n- second item\n- third item\n- fourth item\n\n```sh\nmake\n```\n\n```sh\nmake test\n```"
	if text != want {
		t.Errorf("rebuilt =\n%s\nwant\n%s", text, want)
	}
}

func TestLineOverlap(t *testing.T) {
	tests := []struct {
		prev, cur []string
		want      int
	}{
		{[]string{"a", "- shared line"}, []string{"- shared line", "next"}, 1},
		{[]string{"x", "- one line", "- two line"}, []string{"- one line", "- two line", "z"}, 2},
		{[]string{"code", "```"}, []string{"```", "more"}, 0},
		{[]string{"", ""}, []string{"", "x"}, 0},
		{[]string{"## Title"}, []string{"## Title"}, 0},
		{nil, []string{"x"}, 0},
	}
	for _, tt := range tests {
		if got := lineOverlap(tt.prev, tt.cur); got != tt.want {
			t.Errorf("lineOverlap(%q, %q) = %d, want %d", tt.prev, tt.cur, got, tt.want)
		}
	}
}
