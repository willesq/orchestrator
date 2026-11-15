package rag

import (
	"context"
	"slices"
	"testing"

	"github.com/willesq/orchestrator/internal/spaces"
)

// TestCountSpacesCountsEachCollection: SP-031.1.a1. Each collection's
// chunks are counted, and those not labelled by the applied revision are
// counted as stale, as a relabel sees them.
func TestCountSpacesCountsEachCollection(t *testing.T) {
	svc, f := datedService(t)
	f.seed("c", map[string]any{"source_uri": "a"}, map[string]any{"source_uri": "b", "labels_v": 4})
	f.seed("c__acme", map[string]any{"labels_v": 4}, map[string]any{"labels_v": 3}, map[string]any{"labels_v": 4})
	f.seed("c__quarantine", map[string]any{"labels_v": 4})
	f.seed("c__oss")
	f.seed("other", map[string]any{"labels_v": 1}) // not one of c's collections

	got, err := svc.CountSpaces(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []CollectionCount{
		{Space: "", Chunks: 2, Stale: 1},
		{Space: "acme", Chunks: 3, Stale: 1},
		{Space: "oss"},
		{Space: spaces.Quarantine, Chunks: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}

	got, err = svc.CountSpaces(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Stale != 0 {
			t.Fatalf("with no revision nothing is stale: %+v", got)
		}
	}
}

// TestCountSpacesStoreFailure: a store that cannot be listed is an error,
// never a partial count.
func TestCountSpacesStoreFailure(t *testing.T) {
	svc := NewService(&hookEmbedder{}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 12, 0)
	if _, err := svc.CountSpaces(context.Background(), 1); err == nil {
		t.Fatal("want an error from an unreachable store")
	}
}

// TestSearchCollectionsNamesWhatSearchReads: rag why's collections are the
// ones a search for the same spaces reads.
func TestSearchCollectionsNamesWhatSearchReads(t *testing.T) {
	svc, f := datedService(t)
	f.seed("c")
	f.seed("c__acme")
	f.seed("c__oss")
	ctx := context.Background()
	for _, tc := range []struct {
		spaces []string
		want   []string
	}{
		{nil, []string{"c", "c__acme", "c__oss"}},
		{[]string{}, []string{}},
		{[]string{"oss", "acme", "oss", "Not A Space"}, []string{"c__oss", "c__acme"}},
		{[]string{spaces.Shared}, []string{"c"}},
	} {
		got, err := svc.SearchCollections(ctx, tc.spaces)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("SearchCollections(%v) = %v, want %v", tc.spaces, got, tc.want)
		}
	}
}
