package hook

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestRelevance(t *testing.T) {
	tests := []struct {
		hit  *orchdv1.SearchHit
		want int
	}{
		{&orchdv1.SearchHit{Score: 0.5}, byMeaning},
		{&orchdv1.SearchHit{Score: 0.5, Exact: true}, byMeaning},
		{&orchdv1.SearchHit{Score: 0.1}, notRelevant},
		{&orchdv1.SearchHit{Score: 0.1, KeywordScore: 4}, notRelevant}, // a keyword hit alone does not pass
		{&orchdv1.SearchHit{Score: 0.15, Exact: true}, byIdentifier},   // an identifier lowers the floor
		{&orchdv1.SearchHit{Score: 0.1, Exact: true}, notRelevant},     // but does not remove it
	}
	for _, tt := range tests {
		if got := relevance(tt.hit, 0.25); got != tt.want {
			t.Errorf("relevance(%+v) = %v, want %v", tt.hit, got, tt.want)
		}
	}
}

// hitsClient answers every search with the same hits.
type hitsClient struct {
	orchdv1.OrchdClient
	hits []*orchdv1.SearchHit
}

func (c hitsClient) Search(context.Context, *orchdv1.SearchRequest, ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	return &orchdv1.SearchResponse{Hits: c.hits}, nil
}

func TestProjectFirstHitsCapsIdentifierOnlyHits(t *testing.T) {
	client := hitsClient{hits: []*orchdv1.SearchHit{
		{SourceUri: "a", Text: "a", Score: 0.15, Exact: true},
		{SourceUri: "b", Text: "b", Score: 0.15, Exact: true},
		{SourceUri: "c", Text: "c", Score: 0.4},
	}}
	got := projectFirstHits(context.Background(), client, "where is sync_meta", nil,
		config.AutoRAGSettings{TopK: 3, MinScore: 0.25}, nil)
	var uris []string
	for _, h := range got {
		uris = append(uris, h.GetSourceUri())
	}
	if len(uris) != 2 || uris[0] != "a" || uris[1] != "c" {
		t.Fatalf("kept %v, want one identifier-only hit plus the meaningful one", uris)
	}
}

// sizeClient records the top_k of each search it answers.
type sizeClient struct {
	orchdv1.OrchdClient
	asked *[]int32
}

func (c sizeClient) Search(_ context.Context, req *orchdv1.SearchRequest, _ ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	*c.asked = append(*c.asked, req.GetTopK())
	return &orchdv1.SearchResponse{}, nil
}

func TestProjectFirstHitsClampsTopK(t *testing.T) {
	var asked []int32
	projectFirstHits(context.Background(), sizeClient{asked: &asked}, "q", []string{"p"},
		config.AutoRAGSettings{TopK: 1000, MinScore: 0.25}, nil)
	if len(asked) != 2 || asked[0] != maxAutoRAGTopK || asked[1] != maxAutoRAGTopK {
		t.Fatalf("asked top_k %v, want %d", asked, maxAutoRAGTopK)
	}
}
