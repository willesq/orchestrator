package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// browseRag records what browsing and searching asked for.
type browseRag struct {
	fakeRag
	cols      []rag.CollectionInfo
	browse    *rag.BrowseOpts
	listCols  *rag.ListCollectionsOpts
	search    *rag.SearchOpts
	sources   *rag.ListSourcesOpts
	browseErr error
}

func (b browseRag) ListCollections(_ context.Context, o rag.ListCollectionsOpts) ([]rag.CollectionInfo, error) {
	*b.listCols = o
	return b.cols, b.browseErr
}

func (b browseRag) Browse(_ context.Context, o rag.BrowseOpts) ([]rag.Hit, string, int64, error) {
	*b.browse = o
	if b.browseErr != nil {
		return nil, "", 0, b.browseErr
	}
	return b.hits, "next", 7, nil
}

func (b browseRag) Search(_ context.Context, _ string, o rag.SearchOpts) ([]rag.Hit, string, error) {
	*b.search = o
	return b.hits, "", nil
}

func (b browseRag) ListSources(_ context.Context, o rag.ListSourcesOpts) ([]rag.SourceSummary, int, int, error) {
	*b.sources = o
	return nil, 0, 0, nil
}

func newBrowseRag(hits []rag.Hit, cols []rag.CollectionInfo) browseRag {
	return browseRag{fakeRag: fakeRag{hits: hits}, cols: cols, browse: &rag.BrowseOpts{}, listCols: &rag.ListCollectionsOpts{},
		search: &rag.SearchOpts{}, sources: &rag.ListSourcesOpts{}}
}

func withBackup(t *testing.T, name string) {
	t.Helper()
	prev := backupCollection
	backupCollection = func() string { return name }
	t.Cleanup(func() { backupCollection = prev })
}

func TestBrowseChunksValidatesAndMaps(t *testing.T) {
	idx := 3
	hits := []rag.Hit{
		{Text: "t", SourceURI: "u", Space: "acme", Collection: "c__acme", PointID: "p1", ChunkIndex: &idx, IngestedAt: time.Unix(300, 0)},
		{Text: "t2", SourceURI: "u2", Collection: "c", PointID: "p2"},
	}
	fr := newBrowseRag(hits, nil)
	s := &server{start: time.Now(), rag: fr}
	ctx := context.Background()
	tests := []struct {
		name string
		req  *orchdv1.BrowseChunksRequest
		code codes.Code
	}{
		{"defaults", &orchdv1.BrowseChunksRequest{}, codes.OK},
		{"the largest page", &orchdv1.BrowseChunksRequest{Limit: maxBrowseLimit}, codes.OK},
		{"a negative limit", &orchdv1.BrowseChunksRequest{Limit: -1}, codes.InvalidArgument},
		{"too large a page", &orchdv1.BrowseChunksRequest{Limit: maxBrowseLimit + 1}, codes.InvalidArgument},
		{"too long a tag", &orchdv1.BrowseChunksRequest{Tags: []string{strings.Repeat("x", 300)}}, codes.InvalidArgument},
		{"since after until", &orchdv1.BrowseChunksRequest{Since: 20, Until: 10}, codes.InvalidArgument},
		{"a negative time", &orchdv1.BrowseChunksRequest{Since: -1}, codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.BrowseChunks(ctx, tt.req)
			if grpcstatus.Code(err) != tt.code {
				t.Fatalf("err = %v, want %s", err, tt.code)
			}
		})
	}
	req := &orchdv1.BrowseChunksRequest{Collections: []string{"c__acme"}, SourceType: "manual", Scope: "work", SourceUri: "u",
		Tags: []string{"a"}, AllTags: []string{"b"}, Since: 10, Until: 20, Limit: 5, Cursor: "cur"}
	rep, err := s.BrowseChunks(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	o := *fr.browse
	if !slices.Equal(o.Collections, []string{"c__acme"}) || o.SourceType != "manual" || o.Scope != "work" || o.SourceURI != "u" ||
		!slices.Equal(o.Tags, []string{"a"}) || !slices.Equal(o.AllTags, []string{"b"}) || o.Limit != 5 || o.Cursor != "cur" ||
		o.Range.Since.Unix() != 10 || o.Range.Until.Unix() != 20 || o.Spaces != nil {
		t.Fatalf("browse opts = %+v", o)
	}
	if rep.GetNextCursor() != "next" || rep.GetTotal() != 7 || len(rep.GetHits()) != 2 {
		t.Fatalf("reply = %+v", rep)
	}
	h := rep.GetHits()[0]
	if h.GetCollection() != "c__acme" || h.GetPointId() != "p1" || h.ChunkIndex == nil || h.GetChunkIndex() != 3 || h.GetIngestedAt() != 300 {
		t.Fatalf("hit = %+v", h)
	}
	if h.GetSpace() != "" {
		t.Fatalf("with no policy the hit names space %q", h.GetSpace())
	}
	if rep.GetHits()[1].ChunkIndex != nil {
		t.Fatal("a hit with no chunk index reports one")
	}
}

func TestBrowseChunksErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := (&server{rag: fakeRag{}}).BrowseChunks(ctx, &orchdv1.BrowseChunksRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("a store that cannot browse: err = %v", err)
	}
	if _, err := (&server{rag: fakeRag{}}).ListCollections(ctx, &orchdv1.ListCollectionsRequest{}); grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("a store that cannot list collections: err = %v", err)
	}
	fr := newBrowseRag(nil, nil)
	fr.browseErr = rag.ErrBadCursor
	if _, err := (&server{rag: fr}).BrowseChunks(ctx, &orchdv1.BrowseChunksRequest{Cursor: "x"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("a bad cursor: err = %v", err)
	}
	fr.browseErr = errors.New(`collection "c__acme" is down`)
	if _, err := (&server{rag: fr}).BrowseChunks(ctx, &orchdv1.BrowseChunksRequest{}); err == nil || !strings.Contains(err.Error(), "c__acme") {
		t.Fatalf("an unfiltered reader's store error = %v, want it as it is", err)
	}
	withBackup(t, "")
	if _, err := (&server{rag: fr}).ListCollections(ctx, &orchdv1.ListCollectionsRequest{}); err == nil {
		t.Fatal("a failed listing succeeded")
	}
	// A filtered reader is never told a collection's name (RC-350).
	e := newReadEnv(t)
	e.s.rag = fr
	for name, call := range map[string]func() error{
		"browse": func() error {
			_, err := e.s.BrowseChunks(inSpace("acme"), &orchdv1.BrowseChunksRequest{})
			return err
		},
		"collections": func() error {
			_, err := e.s.ListCollections(inSpace("acme"), &orchdv1.ListCollectionsRequest{})
			return err
		},
	} {
		err := call()
		if grpcstatus.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "acme") {
			t.Fatalf("%s: a filtered reader's store error = %v", name, err)
		}
	}
}

func TestCollectionFilterAndCursorAreBounded(t *testing.T) {
	ctx := context.Background()
	s := &server{rag: newBrowseRag(nil, nil)}
	many := make([]string, maxFilterCollections+1)
	for i := range many {
		many[i] = "c"
	}
	long := strings.Repeat("c", maxFilterCollectionLen+1)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"browse: too many collections", func() error {
			_, err := s.BrowseChunks(ctx, &orchdv1.BrowseChunksRequest{Collections: many})
			return err
		}},
		{"browse: a long collection name", func() error {
			_, err := s.BrowseChunks(ctx, &orchdv1.BrowseChunksRequest{Collections: []string{long}})
			return err
		}},
		{"browse: a long cursor", func() error {
			_, err := s.BrowseChunks(ctx, &orchdv1.BrowseChunksRequest{Cursor: strings.Repeat("a", maxBrowseCursorLen+1)})
			return err
		}},
		{"search: too many collections", func() error {
			_, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q", Collections: many})
			return err
		}},
		{"sources: a long collection name", func() error {
			_, err := s.ListSources(ctx, &orchdv1.ListSourcesRequest{Collections: []string{long}})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); grpcstatus.Code(err) != codes.InvalidArgument {
				t.Fatalf("err = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestListCollectionsMapsAndHidesTheBackup(t *testing.T) {
	withBackup(t, "old")
	fr := newBrowseRag(nil, []rag.CollectionInfo{{Name: "c", Chunks: 4, Hybrid: true}, {Name: "c__acme", Space: "acme", Chunks: 2}})
	s := &server{rag: fr}
	rep, err := s.ListCollections(context.Background(), &orchdv1.ListCollectionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fr.listCols.Hide, []string{"old"}) || fr.listCols.Spaces != nil {
		t.Fatalf("list opts = %+v", *fr.listCols)
	}
	if rep.GetSpacesOn() {
		t.Fatal("spaces reported on with no switch")
	}
	got := rep.GetCollections()
	if len(got) != 2 || got[0].GetName() != "c" || got[0].GetChunks() != 4 || !got[0].GetHybrid() || got[1].GetName() != "c__acme" || got[1].GetChunks() != 2 {
		t.Fatalf("collections = %v", got)
	}
	if got[1].GetSpace() != "" {
		t.Fatalf("with no policy a collection names space %q", got[1].GetSpace())
	}
}

func TestSearchAndListSourcesPassCollections(t *testing.T) {
	fr := newBrowseRag([]rag.Hit{{SourceURI: "u", Collection: "c__a", PointID: "p"}}, nil)
	s := &server{rag: fr}
	ctx := context.Background()
	rep, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q", Collections: []string{"c__a"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fr.search.Collections, []string{"c__a"}) {
		t.Fatalf("search collections = %v", fr.search.Collections)
	}
	if h := rep.GetHits()[0]; h.GetCollection() != "c__a" || h.GetPointId() != "p" || h.ChunkIndex != nil {
		t.Fatalf("hit = %+v", h)
	}
	if _, err := s.ListSources(ctx, &orchdv1.ListSourcesRequest{Collections: []string{"c", "c__a"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fr.sources.Collections, []string{"c", "c__a"}) {
		t.Fatalf("list collections = %v", fr.sources.Collections)
	}
	if _, err := s.ListSources(ctx, &orchdv1.ListSourcesRequest{Tags: []string{"any"}, AllTags: []string{"repo:r", "owner:o"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fr.sources.Tags, []string{"any"}) || !slices.Equal(fr.sources.AllTags, []string{"repo:r", "owner:o"}) {
		t.Fatalf("list tags = %v, all tags = %v", fr.sources.Tags, fr.sources.AllTags)
	}
	tooMany := make([]string, maxFilterTags+1)
	if _, err := s.ListSources(ctx, &orchdv1.ListSourcesRequest{AllTags: tooMany}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("too many tags: err = %v, want InvalidArgument", err)
	}
}

// TestBrowseKeepsSpacesApart: a session browses and lists only its readable
// spaces' collections, page by page to the end; naming another space's
// collection reads nothing.
func TestBrowseKeepsSpacesApart(t *testing.T) {
	e := newReadEnv(t)
	globex := inSpace("globex")
	cols, err := e.s.ListCollections(globex, &orchdv1.ListCollectionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cols.GetCollections() {
		names = append(names, c.GetName()+"/"+c.GetSpace())
		if c.GetChunks() != chunksPerSpace+1 {
			t.Errorf("%s holds %d chunks, want %d", c.GetName(), c.GetChunks(), chunksPerSpace+1)
		}
	}
	if !slices.Equal(names, []string{"c__globex/globex", "c__oss/oss"}) {
		t.Fatalf("globex lists %v", names)
	}
	seen := map[string]bool{}
	req := &orchdv1.BrowseChunksRequest{Limit: 150}
	for page := 0; ; page++ {
		rep, err := e.s.BrowseChunks(globex, req)
		if err != nil {
			t.Fatal(err)
		}
		if page == 0 && rep.GetTotal() != 2*(chunksPerSpace+1) {
			t.Fatalf("total = %d", rep.GetTotal())
		}
		for _, h := range rep.GetHits() {
			if l := spaceLeaks([]string{"globex", "oss"}, h.GetText(), h.GetSourceUri(), h.GetCollection()); len(l) > 0 {
				t.Fatalf("globex browsed %q", l)
			}
			seen[h.GetCollection()+"/"+h.GetPointId()] = true
		}
		if rep.GetNextCursor() == "" {
			break
		}
		if page > 10 {
			t.Fatal("browse never ended")
		}
		req.Cursor = rep.GetNextCursor()
	}
	if len(seen) != 2*(chunksPerSpace+1) {
		t.Fatalf("browsed %d distinct chunks, want %d", len(seen), 2*(chunksPerSpace+1))
	}
	rep, err := e.s.BrowseChunks(globex, &orchdv1.BrowseChunksRequest{Collections: []string{"c__acme", "c__quarantine", "c"}})
	if err != nil || len(rep.GetHits()) != 0 || rep.GetTotal() != 0 || rep.GetNextCursor() != "" {
		t.Fatalf("naming other spaces' collections = %v, %v", rep, err)
	}
	for _, c := range []string{"c__acme", "c__quarantine", "c", "c__personal"} {
		if n := e.q.readsOf(c); n != 0 {
			t.Errorf("%s was read %d times", c, n)
		}
	}
}
