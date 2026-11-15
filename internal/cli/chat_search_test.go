package cli

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestSearchDirectMessagesFlag: --direct-messages sends the search through
// SearchDirectMessages, with every filter; without it, Search is used and
// DMs are never asked for (SP-060).
func TestSearchDirectMessagesFlag(t *testing.T) {
	lastDMSearch = nil
	fc := &fakeClient{search: &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
		{Text: "can you review my patch", SourceUri: "slack://T1/D1", SourceType: "chat", Score: 0.8},
	}}}
	out, err := run(factory(fc, nil), "", "search", "review", "patch", "--direct-messages", "--top-k", "7", "--type", "chat")
	if err != nil {
		t.Fatal(err)
	}
	req := lastDMSearch.GetSearch()
	if req.GetQuery() != "review patch" || req.GetTopK() != 7 || req.GetSourceType() != "chat" {
		t.Errorf("DM search request = %v", lastDMSearch)
	}
	if fc.searchReq != nil {
		t.Errorf("Search was called too: %v", fc.searchReq)
	}
	if !strings.Contains(out, "slack://T1/D1") {
		t.Errorf("output = %q", out)
	}
	lastDMSearch = nil
	if _, err := run(factory(fc, nil), "", "search", "review"); err != nil {
		t.Fatal(err)
	}
	if lastDMSearch != nil || fc.searchReq.GetQuery() != "review" {
		t.Errorf("a plain search asked for DMs: %v", lastDMSearch)
	}
}
