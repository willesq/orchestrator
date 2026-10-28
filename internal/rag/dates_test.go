package rag

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRepeatedChunksAreStoredOnce(t *testing.T) {
	svc, f := datedService(t)
	svc.MarkdownSections = false
	block := strings.Repeat("same words here ", 6)
	n, err := svc.IngestText(context.Background(), block+"\n\n"+block+"\n\nsomething else entirely", Meta{SourceURI: "manual://dup"})
	if err != nil {
		t.Fatal(err)
	}
	texts := f.collection("c").texts()["manual://dup"]
	seen := map[string]bool{}
	for _, tx := range texts {
		if seen[tx] {
			t.Fatalf("chunk %q stored twice", tx)
		}
		seen[tx] = true
	}
	if n != len(texts) || n == 0 {
		t.Fatalf("n = %d, stored %d", n, len(texts))
	}
	// Indices stay contiguous, so a shrink prunes the rest correctly.
	for i, p := range dedupeChunks([]chunk{{text: "a"}, {text: "b"}, {text: "a"}, {text: "c"}}) {
		if p.text != string("abc"[i]) {
			t.Fatalf("dedupe order wrong at %d: %q", i, p.text)
		}
	}
}

func TestSourceSpanDatesUndatedChunks(t *testing.T) {
	svc, f := datedService(t)
	commit := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := svc.IngestText(context.Background(), "# Doc\n\nbody", Meta{SourceURI: "repo://o/r/a.md", SourceType: "repo_doc",
		StartedAt: commit, EndedAt: commit}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IngestText(context.Background(), "kept undated", Meta{SourceURI: "manual://copy", KeepUndated: true}); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.collection("c").points {
		switch str(p.Payload["source_uri"]) {
		case "repo://o/r/a.md":
			if !unixTime(p.Payload[fieldStartedAt]).Equal(commit) || !unixTime(p.Payload[fieldEndedAt]).Equal(commit) {
				t.Fatalf("doc not dated by its commit: %+v", p.Payload)
			}
		case "manual://copy":
			if _, ok := p.Payload[fieldStartedAt]; ok {
				t.Fatalf("KeepUndated content was dated: %+v", p.Payload)
			}
		}
	}
}
