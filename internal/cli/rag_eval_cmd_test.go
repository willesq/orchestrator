package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// evalClient answers Search by query.
type evalClient struct {
	*fakeClient
	byQuery map[string][]string
	reqs    []*orchdv1.SearchRequest
}

func (e *evalClient) Search(_ context.Context, req *orchdv1.SearchRequest, _ ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	e.reqs = append(e.reqs, req)
	var hits []*orchdv1.SearchHit
	for _, u := range e.byQuery[req.GetQuery()] {
		hits = append(hits, &orchdv1.SearchHit{SourceUri: u})
	}
	return &orchdv1.SearchResponse{Hits: hits}, nil
}

func TestRAGEval(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	ec := &evalClient{fakeClient: &fakeClient{}, byQuery: map[string][]string{
		"install orch": {"repo://willesq/orchestrator/README.md"},
		"routing":      {"repo://x", "repo://willesq/orchestrator/docs/MODEL_ROUTING_PLAN.md"},
	}}
	cf := func() (orchdv1.OrchdClient, func(), error) { return ec, func() {}, nil }

	if _, err := run(cf, "", "rag", "eval"); err == nil || !strings.Contains(err.Error(), "no cases") {
		t.Fatalf("empty set err = %v", err)
	}
	for _, args := range [][]string{
		{"rag", "eval", "add", "install orch", "repo://willesq/orchestrator/README.md"},
		{"rag", "eval", "add", "routing", "repo://willesq/orchestrator/docs/*", "--source-type", "repo_doc", "--tag", "repo:orchestrator"},
		{"rag", "eval", "add", "never found", "repo://nope"},
	} {
		if _, err := run(cf, "", args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := run(cf, "", "rag", "eval", "add", "routing", "x"); err == nil {
		t.Fatal("duplicate case accepted")
	}
	out, err := run(cf, "", "rag", "eval", "--top-k", "5", "--save", "before")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"✓   1  install orch", "✓   2  routing", "✗   -  never found", "3 cases, top 5: hit@1 0.33  recall@5 0.67  MRR 0.500", `saved as "before"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	last := ec.reqs[len(ec.reqs)-2] // the routing case's request
	if last.GetSourceType() != "repo_doc" || last.GetTags()[0] != "repo:orchestrator" || last.GetTopK() != 5 {
		t.Fatalf("filters not passed: %+v", last)
	}

	// Routing improves: compare reports the change.
	ec.byQuery["routing"] = []string{"repo://willesq/orchestrator/docs/MODEL_ROUTING_PLAN.md"}
	out, err = run(cf, "", "rag", "eval", "--top-k", "5", "--compare", "before")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "vs saved:       hit@1 +0.33") || !strings.Contains(out, "better 2 → 1  routing") {
		t.Fatalf("compare output:\n%s", out)
	}
	out, err = run(cf, "", "rag", "eval", "--json", "--top-k", "5")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		HitAt1 float64 `json:"hit_at_1"`
	}
	if json.Unmarshal([]byte(out), &rep) != nil || rep.HitAt1 < 0.66 {
		t.Fatalf("json = %s", out)
	}
	for _, args := range [][]string{
		{"rag", "eval", "--top-k", "0"},
		{"rag", "eval", "--compare", "absent"},
		{"rag", "eval", "--save", "../escape"},
		{"rag", "eval", "add", " ", "u"},
	} {
		if _, err := run(cf, "", args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestTermSafe(t *testing.T) {
	if got := termSafe("repo://x\x1b]52;c;cGF5\x07\x1b[2K\u009bok"); strings.ContainsAny(got, "\x1b\x07\u009b") || !strings.HasSuffix(got, "ok") {
		t.Fatalf("termSafe = %q", got)
	}
}

func TestRAGEvalEscapesHitText(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	ec := &evalClient{fakeClient: &fakeClient{}, byQuery: map[string][]string{"q": {"repo://evil\x1b[2Kfake"}}}
	cf := func() (orchdv1.OrchdClient, func(), error) { return ec, func() {}, nil }
	if _, err := run(cf, "", "rag", "eval", "add", "q", "repo://wanted"); err != nil {
		t.Fatal(err)
	}
	out, err := run(cf, "", "rag", "eval")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "repo://evil?[2Kfake") {
		t.Fatalf("escape reached the terminal: %q", out)
	}
}

func TestRAGEvalPassesMode(t *testing.T) {
	t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	ec := &evalClient{fakeClient: &fakeClient{}, byQuery: map[string][]string{"q": {"u://a"}}}
	cf := func() (orchdv1.OrchdClient, func(), error) { return ec, func() {}, nil }
	if _, err := run(cf, "", "rag", "eval", "add", "q", "u://a"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(cf, "", "rag", "eval", "--mode", "dense"); err != nil {
		t.Fatal(err)
	}
	if got := ec.reqs[len(ec.reqs)-1].GetMode(); got != "dense" {
		t.Fatalf("mode = %q", got)
	}
}

func TestSearchShowsMatchKind(t *testing.T) {
	client := &fakeClient{search: &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
		{SourceUri: "u://exact", Score: 0.2, Exact: true, KeywordScore: 4},
		{SourceUri: "u://kw", Score: 0.3, KeywordScore: 2},
		{SourceUri: "u://dense", Score: 0.6},
	}}}
	out, err := run(factory(client, nil), "", "search", "--mode", "hybrid", "q")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[0.200 exact] u://exact", "[0.300 keyword] u://kw", "[0.600] u://dense"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
