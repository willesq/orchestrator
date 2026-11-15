package hook

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// scopedClient answers every search with resp, or fails.
type scopedClient struct {
	orchdv1.OrchdClient
	resp *orchdv1.SearchResponse
	err  error
}

func (c scopedClient) Search(context.Context, *orchdv1.SearchRequest, ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	return c.resp, c.err
}

// TestExplainRecordsWhatWasSearched: SP-031's rag why keeps the boundary
// orchd reported; nothing when orchd said nothing or the search failed.
func TestExplainRecordsWhatWasSearched(t *testing.T) {
	ctx := context.Background()
	const prompt = "fix the socket reconnect loop"
	resp := &orchdv1.SearchResponse{CallerSpace: "acme", Scoped: true, Collections: []string{"rag__acme", "rag__oss"}}
	ex := Explain(ctx, gatedAutoRAG(), scopedClient{resp: resp}, prompt, "")
	if s := ex.Searched; s == nil || s.CallerSpace != "acme" || !s.Scoped || !slices.Equal(s.Collections, resp.Collections) {
		t.Fatalf("searched = %+v", ex.Searched)
	}
	if ex := Explain(ctx, gatedAutoRAG(), scopedClient{resp: &orchdv1.SearchResponse{}}, prompt, ""); ex.Searched != nil {
		t.Fatalf("an older orchd's answer recorded %+v", ex.Searched)
	}
	if ex := Explain(ctx, gatedAutoRAG(), scopedClient{err: errors.New("down")}, prompt, ""); ex.Searched != nil {
		t.Fatalf("a failed search recorded %+v", ex.Searched)
	}
}
