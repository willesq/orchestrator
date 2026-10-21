package daemon

import (
	"context"
	"fmt"
	"strings"
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

// datedRag records the options searches and listings asked for.
type datedRag struct {
	fakeRag
	search *rag.SearchOpts
	list   *rag.ListSourcesOpts
	meta   *rag.Meta
}

func (d datedRag) Search(_ context.Context, _ string, o rag.SearchOpts) ([]rag.Hit, string, error) {
	*d.search = o
	return []rag.Hit{{SourceURI: "session://s/record", StartedAt: time.Unix(100, 0), EndedAt: time.Unix(200, 0), IngestedAt: time.Unix(300, 0)}}, "", nil
}

func (d datedRag) ListSources(_ context.Context, o rag.ListSourcesOpts) ([]rag.SourceSummary, int, int, error) {
	*d.list = o
	return []rag.SourceSummary{{SourceURI: "session://s/record", StartedAt: time.Unix(100, 0)}}, 1, 1, nil
}

func (d datedRag) IngestText(_ context.Context, _ string, m rag.Meta) (int, error) {
	*d.meta = m
	return 1, nil
}

func TestDatesThroughTheDaemon(t *testing.T) {
	ctx := context.Background()
	var so rag.SearchOpts
	var lo rag.ListSourcesOpts
	var meta rag.Meta
	s := &server{start: time.Now(), rag: datedRag{search: &so, list: &lo, meta: &meta}}

	resp, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q", Since: 50, Until: 250})
	if err != nil || so.Range.Since.Unix() != 50 || so.Range.Until.Unix() != 250 {
		t.Fatalf("search range = %+v, %v", so.Range, err)
	}
	if h := resp.GetHits()[0]; h.GetStartedAt() != 100 || h.GetEndedAt() != 200 || h.GetIngestedAt() != 300 {
		t.Fatalf("hit = %+v", h)
	}
	if _, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q", Since: 300, Until: 200}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("inverted range err = %v", err)
	}
	if _, err := s.Search(ctx, &orchdv1.SearchRequest{Query: "q"}); err != nil || !so.Range.IsZero() {
		t.Fatalf("no range = %+v, %v", so.Range, err)
	}

	lresp, err := s.ListSources(ctx, &orchdv1.ListSourcesRequest{Until: 400})
	if err != nil || lo.Range.Until.Unix() != 400 || !lo.Range.Since.IsZero() || lresp.GetSources()[0].GetStartedAt() != 100 ||
		lresp.GetSources()[0].GetEndedAt() != 0 {
		t.Fatalf("list = %+v, %+v, %v", lo.Range, lresp, err)
	}
	if _, err := s.ListSources(ctx, &orchdv1.ListSourcesRequest{Since: -1}); grpcstatus.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative since err = %v", err)
	}

	if _, err := s.Ingest(ctx, &orchdv1.IngestRequest{Text: "t", SourceUri: "u", StartedAt: 100, EndedAt: 200, Timeline: true}); err != nil ||
		meta.StartedAt.Unix() != 100 || meta.EndedAt.Unix() != 200 || !meta.Timeline {
		t.Fatalf("ingest meta = %+v, %v", meta, err)
	}
	if _, err := s.Ingest(ctx, &orchdv1.IngestRequest{Text: "t", SourceUri: "u"}); err != nil || !meta.StartedAt.IsZero() {
		t.Fatalf("undated ingest meta = %+v, %v", meta, err)
	}
}
