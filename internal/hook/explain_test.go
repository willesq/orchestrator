package hook

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/memctx"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func gatedAutoRAG() config.AutoRAGSettings {
	c := defaultAutoRAG()
	c.Gate = GateEvidence
	return c
}

// countingClient counts searches, answers from hits, or fails.
type countingClient struct {
	orchdv1.OrchdClient
	hits     []*orchdv1.SearchHit
	err      error
	requests []*orchdv1.SearchRequest
}

func (c *countingClient) Search(_ context.Context, req *orchdv1.SearchRequest, _ ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	c.requests = append(c.requests, req)
	return &orchdv1.SearchResponse{Hits: c.hits}, c.err
}

func TestGatedPromptGetsOnlyEvidencedHits(t *testing.T) {
	st := &sessionState{}
	client := &countingClient{hits: []*orchdv1.SearchHit{
		{SourceUri: "session://a/transcript", SourceType: "session_transcript", Heading: "gitStatus", Text: "socket reconnect ?? docs/x.md", Score: 0.9},
		{SourceUri: "repo://a/noise.md", SourceType: "repo_doc", Text: "kiln firing schedules", Score: 0.85},
		{SourceUri: "repo://a/one.md", SourceType: "repo_doc", Text: "the socket reconnect loop backs off", Score: 0.5},
		{SourceUri: "session://b/transcript", SourceType: "session_transcript", Text: "the socket reconnect loop backs off", Score: 0.5},
	}}
	var out strings.Builder
	cf := func() (orchdv1.OrchdClient, func(), error) { return client, func() {}, nil }
	enrichPrompt(gatedAutoRAG(), Payload{Prompt: "fix the socket reconnect loop"}, &out, cf, st)
	got := out.String()
	if !strings.Contains(got, "repo://a/one.md") || strings.Contains(got, "noise") || strings.Contains(got, "session://") {
		t.Fatalf("injected:\n%s", got)
	}
	if client.requests[0].GetTopK() != 12 {
		t.Fatalf("gated search asked for %d candidates", client.requests[0].GetTopK())
	}
}

func TestGatedFollowUpNeverDials(t *testing.T) {
	var out strings.Builder
	cf := func() (orchdv1.OrchdClient, func(), error) {
		t.Fatal("dialed orchd for a prompt with no topic")
		return nil, nil, nil
	}
	enrichPrompt(gatedAutoRAG(), Payload{Prompt: "Let's merge and cut a release."}, &out, cf, &sessionState{})
	if out.Len() != 0 {
		t.Fatalf("injected %q", out.String())
	}
}

// With the gate off, the score floors alone decide, as before the gate existed.
func TestGateOffKeepsScoreFloorsOnly(t *testing.T) {
	var out strings.Builder
	client := &countingClient{hits: []*orchdv1.SearchHit{{SourceUri: "repo://a/noise.md", SourceType: "repo_doc", Text: "kiln firing schedules", Score: 0.85}}}
	cf := func() (orchdv1.OrchdClient, func(), error) { return client, func() {}, nil }
	enrichPrompt(defaultAutoRAG(), Payload{Prompt: "Let's merge it right now."}, &out, cf, &sessionState{})
	if !strings.Contains(out.String(), "noise.md") || client.requests[0].GetTopK() != 3 {
		t.Fatalf("gate off: %q, %+v", out.String(), client.requests)
	}
}

func TestGatedSearchFailureInjectsNothing(t *testing.T) {
	var out strings.Builder
	client := &countingClient{err: errors.New("qdrant is down")}
	cf := func() (orchdv1.OrchdClient, func(), error) { return client, func() {}, nil }
	enrichPrompt(gatedAutoRAG(), Payload{Prompt: "fix the socket reconnect loop"}, &out, cf, &sessionState{})
	if out.Len() != 0 {
		t.Fatalf("injected %q", out.String())
	}
}

func TestExplain(t *testing.T) {
	ctx := context.Background()
	t.Run("skips a follow-up without searching", func(t *testing.T) {
		client := &countingClient{}
		ex := Explain(ctx, gatedAutoRAG(), client, "Let's merge it.", "")
		if ex.Skipped == "" || !ex.Gated || len(client.requests) != 0 {
			t.Fatalf("%+v", ex)
		}
	})
	t.Run("too short", func(t *testing.T) {
		if ex := Explain(ctx, gatedAutoRAG(), &countingClient{}, "ok", ""); !strings.Contains(ex.Skipped, "min_prompt_chars") {
			t.Fatalf("%+v", ex)
		}
	})
	t.Run("says why each candidate went where it did", func(t *testing.T) {
		client := &countingClient{hits: []*orchdv1.SearchHit{
			{SourceUri: "repo://a/one.md", SourceType: "repo_doc", Text: "the socket reconnect loop backs off", Score: 0.5},
			{SourceUri: "repo://a/one.md", SourceType: "repo_doc", Text: "the socket reconnect loop again", Score: 0.5},
			{SourceUri: "repo://a/low.md", SourceType: "repo_doc", Text: "socket reconnect loop", Score: 0.1},
			{SourceUri: "repo://a/noise.md", SourceType: "repo_doc", Text: "kiln firing", Score: 0.8},
			{SourceUri: "repo://a/same.md", SourceType: "repo_doc", Text: "The socket reconnect loop backs off!", Score: 0.5},
			{SourceUri: "repo://a/tail.md", SourceType: "repo_doc", Text: "the socket hangs", Score: 0.7},
		}}
		ex := Explain(ctx, gatedAutoRAG(), client, "fix the socket reconnect loop", "")
		if ex.Written != 1 || !strings.Contains(ex.Block, "one.md") {
			t.Fatalf("written %d:\n%s", ex.Written, ex.Block)
		}
		reasons := map[string]string{}
		for _, d := range ex.Decisions {
			if _, ok := reasons[d.Hit.GetSourceUri()]; !ok {
				reasons[d.Hit.GetSourceUri()] = d.Reason
			}
		}
		for uri, want := range map[string]string{
			"repo://a/one.md": "", "repo://a/low.md": reasonBelowFloor, "repo://a/same.md": reasonDuplicate,
		} {
			if reasons[uri] != want {
				t.Errorf("%s: %q, want %q (all %v)", uri, reasons[uri], want, reasons)
			}
		}
		if !slices.Contains(ex.Terms, "socket") {
			t.Errorf("terms %v", ex.Terms)
		}
	})
	t.Run("reports a failed search rather than hiding it", func(t *testing.T) {
		ex := Explain(ctx, gatedAutoRAG(), &countingClient{err: errors.New("down")}, "fix the socket reconnect loop", "")
		if len(ex.Errors) == 0 || ex.Block != "" {
			t.Fatalf("%+v", ex)
		}
	})
	t.Run("gate off and disabled are visible", func(t *testing.T) {
		cfg := defaultAutoRAG()
		cfg.Enabled = false
		ex := Explain(ctx, cfg, &countingClient{hits: []*orchdv1.SearchHit{{SourceUri: "m://x", Text: "x", Score: 0.9}}}, "Let's merge it right now.", "")
		if ex.Gated || !ex.Disabled || ex.Written != 1 {
			t.Fatalf("%+v", ex)
		}
	})
}

// A decision note has no project tag, so it must not lose its slot to project docs.
func TestCuratedNoteOutranksProjectHits(t *testing.T) {
	project := []*orchdv1.SearchHit{
		{SourceUri: "repo://a/one.md", SourceType: "repo_doc", Text: "the socket reconnect loop", Score: 0.6},
		{SourceUri: "repo://a/two.md", SourceType: "repo_doc", Text: "socket reconnect loop again", Score: 0.6},
		{SourceUri: "repo://a/three.md", SourceType: "repo_doc", Text: "reconnect loop socket once more", Score: 0.6},
	}
	note := &orchdv1.SearchHit{SourceUri: "decision://a/socket", SourceType: "decision", Text: "decided: the socket retries forever", Score: 0.4}
	client := &taggedClient{project: project, global: []*orchdv1.SearchHit{note}}
	got := selectHits(context.Background(), client, "fix the socket reconnect loop", []string{"repo:a"}, gatedAutoRAG(), nil, ptrCheck("fix the socket reconnect loop"), nil)
	if len(got) != 3 || got[0] != note {
		t.Fatalf("kept %v", uris(got))
	}
}

// One failed search must not cost the other's results.
func TestGatedSearchSurvivesOneFailure(t *testing.T) {
	hit := &orchdv1.SearchHit{SourceUri: "repo://a/one.md", SourceType: "repo_doc", Text: "the socket reconnect loop", Score: 0.6}
	client := &taggedClient{projectErr: errors.New("down"), global: []*orchdv1.SearchHit{hit}}
	var failures []string
	trace := func(h *orchdv1.SearchHit, reason string) {
		if h == nil {
			failures = append(failures, reason)
		}
	}
	got := selectHits(context.Background(), client, "fix the socket reconnect loop", []string{"repo:a"}, gatedAutoRAG(), nil, ptrCheck("fix the socket reconnect loop"), trace)
	if len(got) != 1 || len(failures) != 1 {
		t.Fatalf("kept %v, failures %v", uris(got), failures)
	}
}

func ptrCheck(prompt string) *memctx.PromptCheck {
	c := memctx.CheckPrompt(prompt)
	return &c
}

// taggedClient answers a tagged search from project and an untagged one from global.
type taggedClient struct {
	orchdv1.OrchdClient
	project, global []*orchdv1.SearchHit
	projectErr      error
}

func (c *taggedClient) Search(_ context.Context, req *orchdv1.SearchRequest, _ ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	if len(req.GetTags()) > 0 {
		if c.projectErr != nil {
			return nil, c.projectErr
		}
		return &orchdv1.SearchResponse{Hits: c.project}, nil
	}
	return &orchdv1.SearchResponse{Hits: c.global}, nil
}

// A page stuffed with the project's words, and copying a note's line, must not
// shadow the note or set the bar for the user's own docs.
func TestStuffedPageCannotDisplaceTrustedHits(t *testing.T) {
	const line = "the socket reconnect loop backs off exponentially after a stall"
	page := &orchdv1.SearchHit{SourceUri: "https://x.io/a", SourceType: "web", Text: line + "\nsocket reconnect loop stall proxy retry", Score: 0.9}
	note := &orchdv1.SearchHit{SourceUri: "decision://a", SourceType: "decision", Text: line, Score: 0.5}
	doc := &orchdv1.SearchHit{SourceUri: "repo://a/b.md", SourceType: "repo_doc", Text: "socket reconnect guide", Score: 0.5}
	client := &taggedClient{global: []*orchdv1.SearchHit{page, doc, note}}
	got := selectHits(context.Background(), client, "fix the socket reconnect loop stall", nil, gatedAutoRAG(), nil, ptrCheck("fix the socket reconnect loop stall"), nil)
	if len(got) != 2 || got[0] != note || got[1] != doc {
		t.Fatalf("kept %v", uris(got))
	}
}

// Only one note may enter below the floor, however many claim to be decisions.
func TestOneCuratedNoteBelowTheFloor(t *testing.T) {
	a := &orchdv1.SearchHit{SourceUri: "decision://a", SourceType: "decision", Text: "the socket a", Score: 0.4}
	b := &orchdv1.SearchHit{SourceUri: "decision://b", SourceType: "decision", Text: "the socket b", Score: 0.4}
	cfg := gatedAutoRAG()
	cfg.MinScore = 0.45
	got := selectHits(context.Background(), &taggedClient{global: []*orchdv1.SearchHit{a, b}}, "add a socket reconnect loop now", nil, cfg, nil, ptrCheck("add a socket reconnect loop now"), nil)
	if len(got) != 1 || got[0] != a {
		t.Fatalf("kept %v", uris(got))
	}
}

func TestRunRecoversFromAPanic(t *testing.T) {
	var out strings.Builder
	if err := Run([]string{"user-prompt-submit"}, panickingReader{}, &out); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

type panickingReader struct{}

func (panickingReader) Read([]byte) (int, error) { panic("boom") }
