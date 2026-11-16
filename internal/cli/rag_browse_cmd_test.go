package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// browseClient answers ListCollections and BrowseChunks, recording the
// browse request.
type browseClient struct {
	*fakeClient
	cols      *orchdv1.ListCollectionsResponse
	page      *orchdv1.BrowseChunksResponse
	browseReq *orchdv1.BrowseChunksRequest
	err       error
}

func (b *browseClient) ListCollections(context.Context, *orchdv1.ListCollectionsRequest, ...grpc.CallOption) (*orchdv1.ListCollectionsResponse, error) {
	return b.cols, b.err
}

func (b *browseClient) BrowseChunks(_ context.Context, req *orchdv1.BrowseChunksRequest, _ ...grpc.CallOption) (*orchdv1.BrowseChunksResponse, error) {
	b.browseReq = req
	return b.page, b.err
}

func TestRAGCollections(t *testing.T) {
	tests := []struct {
		name    string
		resp    *orchdv1.ListCollectionsResponse
		err     error
		wantSub []string
		wantNot []string
	}{
		{
			name: "a table, spaces on",
			resp: &orchdv1.ListCollectionsResponse{SpacesOn: true, Collections: []*orchdv1.CollectionInfo{
				{Name: "orch", Chunks: 12, Hybrid: true},
				{Name: "orch__work\x1b[2J", Space: "work", Chunks: 3},
			}},
			wantSub: []string{"NAME", "orch ", "12  yes", "work", "3  no", "orch__work?[2J"},
			wantNot: []string{"spaces are off", "\x1b"},
		},
		{
			name:    "spaces off",
			resp:    &orchdv1.ListCollectionsResponse{Collections: []*orchdv1.CollectionInfo{{Name: "orch", Chunks: 1}}},
			wantSub: []string{"memory spaces are off", "orch", " -  "},
		},
		{name: "none", resp: &orchdv1.ListCollectionsResponse{SpacesOn: true}, wantSub: []string{"no collections readable"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bc := &browseClient{fakeClient: &fakeClient{}, cols: tt.resp}
			out, err := run(factory(bc, nil), "", "rag", "collections")
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.wantSub {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(out, s) {
					t.Errorf("output has %q:\n%s", s, out)
				}
			}
		})
	}
	bc := &browseClient{fakeClient: &fakeClient{}, err: errors.New("store down")}
	if _, err := run(factory(bc, nil), "", "rag", "collections"); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("err = %v", err)
	}
	if _, err := run(factory(nil, errors.New("no orchd")), "", "rag", "collections"); err == nil {
		t.Fatal("a failed dial succeeded")
	}
}

func TestRAGBrowse(t *testing.T) {
	page := &orchdv1.BrowseChunksResponse{Total: 120, NextCursor: "abc", Hits: []*orchdv1.SearchHit{
		{SourceUri: "note://a", SourceType: "manual", Collection: "orch", PointId: "p1", ChunkIndex: proto.Int32(2), Text: "first\nline \x1b[31mred"},
		{SourceUri: "note://b", SourceType: "repo_doc", Collection: "orch__work", Text: strings.Repeat("x", 300)},
	}}
	tests := []struct {
		name    string
		args    []string
		page    *orchdv1.BrowseChunksResponse
		wantSub []string
		wantNot []string
	}{
		{
			name:    "the first page",
			args:    []string{"--type", "manual", "--scope", "work", "--uri", "note://a", "--tag", "t1", "--require-tag", "t2", "--collection", "orch", "--collection", "orch__work", "--limit", "2", "--since", "2026-01-01", "--until", "2026-02-01"},
			page:    page,
			wantSub: []string{"120 chunk(s) match", `1. "note://a" #2 ("manual") in orch`, "first line ?[31mred", `2. "note://b" ("repo_doc") in orch__work`, "…", "more: --cursor abc"},
			wantNot: []string{"\x1b", "end of listing"},
		},
		{
			name:    "a later, last page",
			args:    []string{"--cursor", "abc"},
			page:    &orchdv1.BrowseChunksResponse{},
			wantSub: []string{"no chunks", "end of listing"},
			wantNot: []string{"match"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bc := &browseClient{fakeClient: &fakeClient{}, page: tt.page}
			out, err := run(factory(bc, nil), "", append([]string{"rag", "browse"}, tt.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.wantSub {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(out, s) {
					t.Errorf("output has %q:\n%s", s, out)
				}
			}
		})
	}
	bc := &browseClient{fakeClient: &fakeClient{}, page: page}
	if _, err := run(factory(bc, nil), "", "rag", "browse", "--type", "manual", "--scope", "work", "--uri", "u", "--tag", "a,b",
		"--require-tag", "c", "--collection", "x", "--limit", "7", "--cursor", "cur", "--since", "2026-01-01"); err != nil {
		t.Fatal(err)
	}
	r := bc.browseReq
	if r.GetSourceType() != "manual" || r.GetScope() != "work" || r.GetSourceUri() != "u" || !slices.Equal(r.GetTags(), []string{"a", "b"}) ||
		!slices.Equal(r.GetAllTags(), []string{"c"}) || !slices.Equal(r.GetCollections(), []string{"x"}) || r.GetLimit() != 7 ||
		r.GetCursor() != "cur" || r.GetSince() == 0 || r.GetUntil() != 0 {
		t.Fatalf("request = %v", r)
	}
	if _, err := run(factory(bc, nil), "", "rag", "browse", "--since", "never"); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("a bad --since: err = %v", err)
	}
	bc.err = errors.New("the browse cursor is not valid")
	if _, err := run(factory(bc, nil), "", "rag", "browse", "--cursor", "bad"); err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("err = %v", err)
	}
	if _, err := run(factory(nil, errors.New("no orchd")), "", "rag", "browse"); err == nil {
		t.Fatal("a failed dial succeeded")
	}
}

func TestCollectionFlagNarrowsSearchAndSources(t *testing.T) {
	fc := &fakeClient{search: &orchdv1.SearchResponse{}, listSources: &orchdv1.ListSourcesResponse{}}
	if _, err := run(factory(fc, nil), "", "search", "q", "--collection", "orch__work", "--collection", "orch"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fc.searchReq.GetCollections(), []string{"orch__work", "orch"}) {
		t.Fatalf("search collections = %v", fc.searchReq.GetCollections())
	}
	if _, err := run(factory(fc, nil), "", "rag", "sources", "--collection", "orch"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fc.listReq.GetCollections(), []string{"orch"}) {
		t.Fatalf("sources collections = %v", fc.listReq.GetCollections())
	}
}
