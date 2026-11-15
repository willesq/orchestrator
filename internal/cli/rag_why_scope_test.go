package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// TestRAGWhyShowsTheSearchBoundary is SP-031's rag why: the caller's space
// and the collections searched, as orchd reported them, in text and JSON;
// an orchd that reports nothing adds no lines.
func TestRAGWhyShowsTheSearchBoundary(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	hit := []*orchdv1.SearchHit{{SourceUri: "repo://a/one.md", SourceType: "repo_doc", Text: "the socket reconnect loop backs off", Score: 0.5}}
	const prompt = "fix the socket reconnect loop"
	for _, tt := range []struct {
		name string
		resp *orchdv1.SearchResponse
		want []string
		not  []string
	}{
		{"a session's search", &orchdv1.SearchResponse{Hits: hit, CallerSpace: "acme", Scoped: true, Collections: []string{"rag__acme", "rag__oss"}},
			[]string{"caller space: acme; searched only the spaces this caller reads", "collections: rag__acme, rag__oss"}, nil},
		{"the operator's search", &orchdv1.SearchResponse{Hits: hit, Collections: []string{"rag", "rag__acme"}},
			[]string{"caller space: none (not a session or a queued task); searched every collection", "collections: rag, rag__acme"}, nil},
		{"a scoped search with no collection named", &orchdv1.SearchResponse{Scoped: true},
			[]string{"caller space: none", "collections: none"}, nil},
		{"an older orchd", &orchdv1.SearchResponse{Hits: hit}, nil, []string{"caller space:", "collections:"}},
		{"a space name with a control character", &orchdv1.SearchResponse{Hits: hit, CallerSpace: "a\x1b[2Jb", Scoped: true},
			nil, []string{"\x1b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(&fakeClient{search: tt.resp}, nil), "", "rag", "why", prompt)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(out, n) {
					t.Errorf("unexpected %q in:\n%s", n, out)
				}
			}
		})
	}
	t.Run("json", func(t *testing.T) {
		resp := &orchdv1.SearchResponse{Hits: hit, CallerSpace: "acme", Scoped: true, Collections: []string{"rag__acme"}}
		out, err := run(factory(&fakeClient{search: resp}, nil), "", "rag", "why", prompt, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Searched *struct {
				CallerSpace string   `json:"caller_space"`
				Scoped      bool     `json:"scoped"`
				Collections []string `json:"collections"`
			} `json:"searched"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatal(err)
		}
		if v.Searched == nil || v.Searched.CallerSpace != "acme" || !v.Searched.Scoped || len(v.Searched.Collections) != 1 {
			t.Fatalf("searched = %+v", v.Searched)
		}
	})
}
