package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// modeRag records the mode a search asked for and returns one hybrid hit.
type modeRag struct {
	fakeRag
	mode     *string
	fallback string
	err      error
}

func (m modeRag) Search(_ context.Context, _ string, o rag.SearchOpts) ([]rag.Hit, string, error) {
	*m.mode = o.Mode
	if m.err != nil {
		return nil, "", m.err
	}
	return []rag.Hit{{SourceURI: "u", Score: 0.2, Keyword: 3.5, Exact: true}}, m.fallback, nil
}

func TestSearchModeAndHybridFields(t *testing.T) {
	var mode string
	s := &server{start: time.Now(), rag: modeRag{mode: &mode}}
	resp, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q", Mode: "dense"})
	if err != nil || mode != "dense" {
		t.Fatalf("mode = %q, err = %v", mode, err)
	}
	h := resp.GetHits()[0]
	if h.GetKeywordScore() != 3.5 || !h.GetExact() || h.GetScore() != 0.2 {
		t.Fatalf("hit = %+v", h)
	}
	if _, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q", Mode: "fuzzy"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad mode err = %v", err)
	}
	for _, k := range []int32{-1, maxSearchTopK + 1} {
		if _, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q", TopK: k}); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Fatalf("top_k %d err = %v", k, err)
		}
	}
}

func TestSearchReportsFallbackAndUnavailableHybrid(t *testing.T) {
	var mode string
	s := &server{start: time.Now(), rag: modeRag{mode: &mode, fallback: "no keyword index"}}
	resp, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q"})
	if err != nil || resp.GetFallback() != "no keyword index" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	s.rag = modeRag{mode: &mode, err: fmt.Errorf("%w: test", rag.ErrHybridUnavailable)}
	if _, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q", Mode: "hybrid"}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v", err)
	}
}
