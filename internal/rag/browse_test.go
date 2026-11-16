package rag

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// browseFixture is a service over a fake store holding the base collection
// "c" (3 chunks) and the space collections c__a (4 chunks of space a, plus
// a stray of space b, one under an old labels revision and a direct
// message) and c__b (2 chunks).
func browseFixture(t *testing.T) (*Service, *fakeQdrant) {
	t.Helper()
	f, url := newFakeQdrant(t)
	chunk := func(uri string, i int, extra map[string]any) map[string]any {
		p := map[string]any{"text": fmt.Sprintf("%s chunk %d", uri, i), "source_uri": uri, "source_type": "manual", "chunk_index": i}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	f.seed("c", chunk("note://base", 0, nil), chunk("note://base", 1, nil), chunk("note://base", 2, map[string]any{"source_type": "repo_doc"}))
	f.seedLayout("c__a", LayoutHybrid,
		chunk("note://a1", 0, map[string]any{fieldSpace: "a", fieldLabelsV: float64(2), "tags": []any{"x"}}),
		chunk("note://a1", 1, map[string]any{fieldSpace: "a", fieldLabelsV: float64(2)}),
		chunk("note://a2", 0, map[string]any{fieldSpace: "a", fieldLabelsV: float64(2), "tags": []any{"x", "y"}}),
		chunk("note://a3", 0, map[string]any{fieldSpace: "a", fieldLabelsV: float64(2)}),
		chunk("note://stray", 0, map[string]any{fieldSpace: "b"}),
		chunk("chat://dm", 0, map[string]any{fieldSpace: "a", fieldChatKind: ChatDM}),
	)
	f.seed("c__b", chunk("note://b1", 0, map[string]any{fieldSpace: "b"}), chunk("note://b1", 1, map[string]any{fieldSpace: "b"}))
	return NewService(fixedEmbedder{vec: []float32{1, 0}}, NewQdrantStore(url, "c", ""), 200, 0), f
}

// browseAll pages through opts to the end, checking each page's total.
func browseAll(t *testing.T, s *Service, opts BrowseOpts) (uris []string, pages int, total int64) {
	t.Helper()
	opts.Cursor = ""
	for {
		hits, next, tot, err := s.Browse(context.Background(), opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		pages++
		if opts.Cursor == "" {
			total = tot
		} else if tot != 0 {
			t.Fatalf("page %d total = %d, want 0 after the first page", pages, tot)
		}
		if opts.Limit > 0 && len(hits) > opts.Limit {
			t.Fatalf("page %d has %d hits, limit %d", pages, len(hits), opts.Limit)
		}
		for _, h := range hits {
			if h.Collection == "" || h.PointID == "" || h.ChunkIndex == nil || h.Score != 0 {
				t.Fatalf("hit %+v lacks its collection, point or chunk index, or has a score", h)
			}
			uris = append(uris, fmt.Sprintf("%s#%d", h.SourceURI, *h.ChunkIndex))
		}
		if next == "" {
			return uris, pages, total
		}
		if pages > 50 {
			t.Fatal("browse never ended")
		}
		opts.Cursor = next
	}
}

func TestBrowsePagesAcrossCollections(t *testing.T) {
	s, _ := browseFixture(t)
	every := []string{"note://base#0", "note://base#1", "note://base#2", "note://a1#0", "note://a1#1", "note://a2#0", "note://a3#0", "note://stray#0", "note://b1#0", "note://b1#1"}
	tests := []struct {
		name      string
		opts      BrowseOpts
		want      []string
		wantPages int
	}{
		// 3 + 5 + 2 readable chunks (the direct message is never read).
		{"pages of three span collections", BrowseOpts{Limit: 3}, every, 4},
		{"a page ending at a collection's end", BrowseOpts{Limit: 5}, every, 2},
		{"the default page holds everything", BrowseOpts{}, every, 1},
		{"one page per chunk", BrowseOpts{Limit: 1}, every, 10},
		{"type filter", BrowseOpts{Limit: 2, SourceType: "repo_doc"}, []string{"note://base#2"}, 1},
		{"tag filter", BrowseOpts{Limit: 2, Tags: []string{"y"}}, []string{"note://a2#0"}, 1},
		{"source filter", BrowseOpts{SourceURI: "note://b1"}, []string{"note://b1#0", "note://b1#1"}, 1},
		{"collections narrow", BrowseOpts{Limit: 2, Collections: []string{"c__b", "c"}}, []string{"note://base#0", "note://base#1", "note://base#2", "note://b1#0", "note://b1#1"}, 3},
		{"unknown collection reads nothing", BrowseOpts{Collections: []string{"elsewhere"}}, nil, 1},
		{"a space reads its own chunks", BrowseOpts{Spaces: []string{"a"}, Limit: 2}, []string{"note://a1#0", "note://a1#1", "note://a2#0", "note://a3#0"}, 2},
		{"collections never widen spaces", BrowseOpts{Spaces: []string{"a"}, Collections: []string{"c__b", "c"}}, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, pages, total := browseAll(t, s, tt.opts)
			if sorted := slices.Sorted(slices.Values(got)); !slices.Equal(sorted, slices.Sorted(slices.Values(tt.want))) {
				t.Fatalf("browsed %v, want %v", got, tt.want)
			}
			if len(slices.Compact(slices.Sorted(slices.Values(got)))) != len(got) {
				t.Fatalf("a chunk was listed twice: %v", got)
			}
			if total != int64(len(tt.want)) {
				t.Fatalf("total = %d, want %d", total, len(tt.want))
			}
			if pages != tt.wantPages {
				t.Fatalf("pages = %d, want %d", pages, tt.wantPages)
			}
		})
	}
}

func TestBrowseOrderAndLabels(t *testing.T) {
	s, f := browseFixture(t)
	hits, _, _, err := s.Browse(context.Background(), BrowseOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, h := range hits {
		if len(order) == 0 || order[len(order)-1] != h.Collection {
			order = append(order, h.Collection)
		}
	}
	if !slices.Equal(order, []string{"c", "c__a", "c__b"}) {
		t.Fatalf("collection order = %v, want the base first, then by name", order)
	}
	f.requests()
	if _, _, _, err := s.Browse(context.Background(), BrowseOpts{Spaces: []string{"a"}, LabelsV: 2}); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.requests() {
		if r.action != "points/scroll" && r.action != "points/count" {
			continue
		}
		filter, _ := r.body["filter"].(map[string]any)
		must, _ := filter["must"].([]any)
		found := slices.ContainsFunc(must, func(c any) bool {
			m := c.(map[string]any)
			return m["key"] == fieldLabelsV && fmt.Sprint(m["match"].(map[string]any)["value"]) == "2"
		})
		if !found || filter["must_not"] == nil {
			t.Fatalf("%s filter %v lacks the labels revision or the direct message exclusion", r.action, filter)
		}
	}
}

func TestBrowseRejectsBadCursors(t *testing.T) {
	s, _ := browseFixture(t)
	for name, cursor := range map[string]string{
		"not base64":         "!!!",
		"not json":           base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"no collection":      browseCursor{Offset: "x"}.encode(),
		"unread collection":  browseCursor{Collection: "c__b"}.encode(),
		"unknown collection": browseCursor{Collection: "other"}.encode(),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := s.Browse(context.Background(), BrowseOpts{Spaces: []string{"a"}, Cursor: cursor})
			if !errors.Is(err, ErrBadCursor) {
				t.Fatalf("err = %v, want ErrBadCursor", err)
			}
		})
	}
}

func TestBrowseFailsWithTheStore(t *testing.T) {
	s, _ := browseFixture(t)
	// A store that cannot be listed fails a whole-store browse.
	broken := NewService(fixedEmbedder{vec: []float32{1, 0}}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 200, 0)
	if _, _, _, err := broken.Browse(context.Background(), BrowseOpts{}); err == nil {
		t.Fatal("a browse of an unreachable store succeeded")
	}
	if _, _, _, err := s.Browse(context.Background(), BrowseOpts{Limit: 500}); err != nil {
		t.Fatal(err)
	}
}

func TestListCollections(t *testing.T) {
	s, f := browseFixture(t)
	f.seed("c__backup", map[string]any{"source_uri": "old"})
	f.seed("c__next", map[string]any{"source_uri": "shadow"})
	f.seed("other", map[string]any{"source_uri": "elsewhere"})
	s.shadow = s.store.WithCollection("c__next")
	tests := []struct {
		name string
		opts ListCollectionsOpts
		want []CollectionInfo
	}{
		{"every collection, backup and shadow hidden", ListCollectionsOpts{Hide: []string{"c__backup"}}, []CollectionInfo{
			{Name: "c", Chunks: 3},
			{Name: "c__a", Space: "a", Chunks: 6 - 1, Hybrid: true},
			{Name: "c__b", Space: "b", Chunks: 2},
		}},
		{"a scoped reader, with a space that has no collection", ListCollectionsOpts{Spaces: []string{"zz", "a", "shared"}, LabelsV: 2, Hide: []string{"c__backup"}}, []CollectionInfo{
			{Name: "c", Chunks: 3},
			{Name: "c__a", Space: "a", Chunks: 4, Hybrid: true},
		}},
		{"no spaces read nothing", ListCollectionsOpts{Spaces: []string{}}, []CollectionInfo{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListCollections(context.Background(), tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	broken := NewService(fixedEmbedder{vec: []float32{1, 0}}, NewQdrantStore("http://127.0.0.1:1", "c", ""), 200, 0)
	if _, err := broken.ListCollections(context.Background(), ListCollectionsOpts{Spaces: []string{"a"}}); err == nil {
		t.Fatal("listing an unreachable store succeeded")
	}
}

func TestHidden(t *testing.T) {
	for _, tt := range []struct {
		name string
		hide []string
		want bool
	}{
		{"c", []string{"c"}, false},
		{"old", []string{"", "old"}, true},
		{"old__a", []string{"old"}, true},
		{"older", []string{"old"}, false},
		{"c__a", nil, false},
	} {
		if got := hidden(tt.name, "c", tt.hide); got != tt.want {
			t.Errorf("hidden(%q, %v) = %v, want %v", tt.name, tt.hide, got, tt.want)
		}
	}
}

func TestSearchAndListSourcesNarrowToCollections(t *testing.T) {
	s, _ := browseFixture(t)
	ctx := context.Background()
	for _, tt := range []struct {
		name        string
		spaces      []string
		collections []string
		want        []string
	}{
		{"one collection", nil, []string{"c__b"}, []string{"c__b"}},
		{"two collections", nil, []string{"c__b", "c"}, []string{"c", "c__b"}},
		{"a name outside the spaces is ignored", []string{"a"}, []string{"c__b", "c__a"}, []string{"c__a"}},
		{"nothing readable", []string{"a"}, []string{"c__b"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hits, _, err := s.Search(ctx, "q", SearchOpts{TopK: 50, Mode: ModeDense, PerSource: -1, Ranking: RankingPlain, Spaces: tt.spaces, Collections: tt.collections})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range hits {
				if h.PointID == "" || h.ChunkIndex == nil {
					t.Fatalf("hit %+v lacks its point or chunk index", h)
				}
				if !slices.Contains(got, h.Collection) {
					got = append(got, h.Collection)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("searched %v, want %v", got, tt.want)
			}
			srcs, _, _, err := s.ListSources(ctx, ListSourcesOpts{Spaces: tt.spaces, Collections: tt.collections})
			if err != nil {
				t.Fatal(err)
			}
			if (len(srcs) == 0) != (len(tt.want) == 0) {
				t.Fatalf("listed %d sources for collections %v", len(srcs), tt.want)
			}
			for _, src := range srcs {
				if tt.want != nil && !slices.Contains(tt.want, "c__b") && src.SourceURI == "note://b1" {
					t.Fatalf("listed %s outside the collections", src.SourceURI)
				}
			}
		})
	}
}

func TestHybridHitsCarryTheirCollection(t *testing.T) {
	s, _ := browseFixture(t)
	hits, _, err := s.Search(context.Background(), "q", SearchOpts{TopK: 10, Mode: ModeHybrid, Collections: []string{"c__a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range hits {
		if h.Collection != "c__a" || h.PointID == "" {
			t.Fatalf("hit %+v", h)
		}
	}
}
