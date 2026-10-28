package rag

import (
	"fmt"
	"strings"
	"testing"
)

func TestChunkTextByTokens(t *testing.T) {
	// "big" costs 5 tokens, every other word 1.
	m := func(w string) int {
		if w == "big" {
			return 5
		}
		return 1
	}
	tests := []struct {
		name          string
		text          string
		size, overlap int
		want          []string
	}{
		{name: "fits", text: "a b c", size: 5, want: []string{"a b c"}},
		// Overlap is held to a quarter of the window: 2 becomes 1.
		{name: "windows with overlap", text: "a b c d e f", size: 4, overlap: 2, want: []string{"a b c d", "d e f"}},
		{name: "overlap within a quarter", text: "a b c d e f g h i j", size: 8, overlap: 2, want: []string{"a b c d e f g h", "g h i j"}},
		{name: "weighted words", text: "a big b c", size: 6, want: []string{"a big", "b c"}},
		{name: "overlap never stalls", text: "big big big", size: 5, overlap: 4, want: []string{"big", "big", "big"}},
		{name: "empty", text: "  \n ", size: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkText(tt.text, tt.size, tt.overlap, m)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChunkTextCapsAtWhatTheModelEmbeds(t *testing.T) {
	text := strings.Repeat("w ", 1000)
	for _, size := range []int{0, 400, maxChunkTokens + 1} {
		for _, c := range chunkText(text, size, 0, perWord) {
			if n := tokens(perWord, c); n > maxChunkTokens {
				t.Fatalf("size %d made a %d-token chunk", size, n)
			}
		}
	}
	for _, c := range markdownChunks("# T\n\n"+text, 400, 0, perWord) {
		if n := tokens(perWord, c.text); n > maxChunkTokens {
			t.Fatalf("markdown chunk of %d tokens", n)
		}
	}
}

func TestApproxTokens(t *testing.T) {
	for w, want := range map[string]int{
		"the": 1, "orchestrator": 4, "x": 1, "(": 1, "foo()": 3, "a.b": 3,
		"日本語": 3, "2026": 2, "[section:": 5, "--force-with-lease": 10,
	} {
		if got := approxTokens(w); got != want {
			t.Errorf("approxTokens(%q) = %d, want %d", w, got, want)
		}
	}
}

type fixedCounter struct {
	n     int
	ok    bool
	calls *int
}

func (f fixedCounter) CountTokens(string) (int, bool) { *f.calls++; return f.n, f.ok }

func TestNewMeasure(t *testing.T) {
	calls := 0
	m := newMeasure(fixedCounter{n: 7, ok: true, calls: &calls})
	if m("word") != 7 || m("word") != 7 || calls != 1 {
		t.Fatalf("counter not used once per word: calls=%d", calls)
	}
	fallback := newMeasure(fixedCounter{ok: false, calls: &calls})
	if fallback("orchestrator") != approxTokens("orchestrator") {
		t.Fatal("an unavailable counter did not fall back to the estimate")
	}
	if newMeasure(nil)("a.b") != 3 {
		t.Fatal("nil counter did not estimate")
	}
}

type countingEmbedder struct {
	fakeEmbedder
	calls *int
}

func (c countingEmbedder) CountTokens(w string) (int, bool) { *c.calls++; return 2, true }

func TestServiceCountsWithTheEmbeddersTokenizer(t *testing.T) {
	calls := 0
	store, payloads := payloadStub(t)
	svc := NewService(countingEmbedder{fakeEmbedder{dim: 2, vec: []float32{1, 0}}, &calls}, store, 10, 0)
	svc.MarkdownSections = false
	words := make([]string, 20)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i) // distinct, so no window repeats another
	}
	n, err := svc.IngestText(t.Context(), strings.Join(words, " "), Meta{SourceURI: "manual://x"})
	if err != nil {
		t.Fatal(err)
	}
	// 20 words at 2 tokens each in 10-token windows.
	if n != 4 || len(payloads()) != 4 || calls == 0 {
		t.Fatalf("chunks = %d, counter calls = %d", n, calls)
	}
}

func TestSplitWord(t *testing.T) {
	perRune := func(w string) int { return len([]rune(w)) }
	if got := splitWord("abcdefghij", 4, perRune); strings.Join(got, "|") != "abcd|efgh|ij" {
		t.Fatalf("got %q", got)
	}
	if got := splitWord("abc", 4, perRune); len(got) != 1 {
		t.Fatalf("a fitting word was split: %q", got)
	}
	if got := splitWord("日本語テキスト", 2, perRune); strings.Join(got, "") != "日本語テキスト" {
		t.Fatalf("multibyte split lost text: %q", got)
	}
	// A long rule inside text is split so every window fits.
	long := strings.Repeat("-", 300)
	for _, c := range chunkText("before "+long+" after", 20, 0, perRune) {
		if n := perRune(strings.ReplaceAll(c, " ", "")); n > 20 {
			t.Fatalf("window of %d units: %q", n, c)
		}
	}
}

func TestSplitWordStaysCheap(t *testing.T) {
	calls, units := 0, 0
	perRune := func(w string) int { calls++; units += len(w); return len([]rune(w)) }
	huge := strings.Repeat("x", 1<<20)
	pieces := splitWord(huge, 126, perRune)
	if strings.Join(pieces, "") != huge {
		t.Fatal("split lost text")
	}
	// Each piece costs a handful of measured prefixes of about its own length.
	if units > 16*len(huge) {
		t.Fatalf("measured %d units for a %d-unit word in %d calls", units, len(huge), calls)
	}
}
