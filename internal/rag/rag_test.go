package rag

import (
	"testing"
	"time"
)

func TestChunkText(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		size      int
		overlap   int
		wantCount int
	}{
		{name: "empty", text: "", size: 400, overlap: 60, wantCount: 0},
		{name: "whitespace only", text: "   \n\t ", size: 400, overlap: 60, wantCount: 0},
		{name: "single small chunk", text: "one two three", size: 400, overlap: 60, wantCount: 1},
		{name: "exact size is one chunk", text: "a b c d", size: 4, overlap: 1, wantCount: 1},
		{name: "two chunks with overlap", text: "a b c d e f", size: 4, overlap: 2, wantCount: 2},
		{name: "overlap >= size falls back", text: "a b c d e f", size: 3, overlap: 5, wantCount: 2},
		{name: "non-positive size defaults", text: "a b c", size: 0, overlap: 0, wantCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkText(tt.text, tt.size, tt.overlap, perWord)
			if len(got) != tt.wantCount {
				t.Errorf("chunkText(%q, %d, %d, perWord) = %d chunks, want %d",
					tt.text, tt.size, tt.overlap, len(got), tt.wantCount)
			}
		})
	}
}

func TestPointIDDeterministic(t *testing.T) {
	tests := []struct {
		name   string
		uriA   string
		idxA   int
		uriB   string
		idxB   int
		wantEq bool
	}{
		{name: "same uri+index equal", uriA: "manual://x", idxA: 0, uriB: "manual://x", idxB: 0, wantEq: true},
		{name: "different index differ", uriA: "manual://x", idxA: 0, uriB: "manual://x", idxB: 1, wantEq: false},
		{name: "different uri differ", uriA: "manual://x", idxA: 0, uriB: "manual://y", idxB: 0, wantEq: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := pointID(tt.uriA, tt.idxA)
			b := pointID(tt.uriB, tt.idxB)
			if (a == b) != tt.wantEq {
				t.Errorf("pointID equality = %v, want %v (%s vs %s)", a == b, tt.wantEq, a, b)
			}
		})
	}
}

func TestBuildFilter(t *testing.T) {
	tests := []struct {
		name      string
		must      map[string]string
		tags      []string
		wantNil   bool
		wantConds int
		tr        TimeRange
	}{
		{name: "empty is nil", must: nil, tags: nil, wantNil: true},
		{name: "skips empty values", must: map[string]string{"scope": "work", "source_type": ""}, tags: nil, wantConds: 1},
		{name: "scope plus tags", must: map[string]string{"scope": "work"}, tags: []string{"go"}, wantConds: 2},
		{name: "tags only", must: nil, tags: []string{"a", "b"}, wantConds: 1},
		{name: "a date range adds both ends", tr: TimeRange{Since: time.Unix(100, 0), Until: time.Unix(200, 0)}, wantConds: 2},
		{name: "an open-ended range adds one", tr: TimeRange{Since: time.Unix(100, 0)}, wantConds: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := buildFilter(tt.must, tt.tags, tt.tr)
			if tt.wantNil {
				if f != nil {
					t.Errorf("expected nil filter, got %v", f)
				}
				return
			}
			conds, ok := f["must"].([]any)
			if !ok {
				t.Fatalf("must clause missing: %v", f)
			}
			if len(conds) != tt.wantConds {
				t.Errorf("conditions = %d, want %d", len(conds), tt.wantConds)
			}
		})
	}
}

// perWord counts each word as one token, so tests can reason in words.
func perWord(string) int { return 1 }
