package hook

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeClient struct {
	listSources  *orchdv1.ListSourcesResponse
	deleteSource *orchdv1.DeleteSourceResponse
	listTasks    *orchdv1.ListTasksResponse
	getTask      *orchdv1.GetTaskResponse
	search       *orchdv1.SearchResponse
	// projectSearch answers tag-filtered searches when set; projectErr fails them.
	projectSearch *orchdv1.SearchResponse
	projectErr    error
	searchReqs    []*orchdv1.SearchRequest
	err           error
	ingestCalls   int
	ingested      []*orchdv1.IngestRequest
}

func (f *fakeClient) Ping(context.Context, *orchdv1.PingRequest, ...grpc.CallOption) (*orchdv1.PingResponse, error) {
	return nil, f.err
}

func (f *fakeClient) Ingest(_ context.Context, req *orchdv1.IngestRequest, _ ...grpc.CallOption) (*orchdv1.IngestResponse, error) {
	f.ingestCalls++
	f.ingested = append(f.ingested, req)
	return &orchdv1.IngestResponse{}, f.err
}

func (f *fakeClient) Search(_ context.Context, req *orchdv1.SearchRequest, _ ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	f.searchReqs = append(f.searchReqs, req)
	if len(req.GetTags()) > 0 {
		if f.projectErr != nil {
			return nil, f.projectErr
		}
		if f.projectSearch != nil {
			return f.projectSearch, f.err
		}
	}
	return f.search, f.err
}

func (f *fakeClient) Dispatch(context.Context, *orchdv1.DispatchRequest, ...grpc.CallOption) (*orchdv1.DispatchResponse, error) {
	return nil, f.err
}

func (f *fakeClient) Status(context.Context, *orchdv1.StatusRequest, ...grpc.CallOption) (*orchdv1.StatusResponse, error) {
	return nil, f.err
}

func (f *fakeClient) Restart(context.Context, *orchdv1.RestartRequest, ...grpc.CallOption) (*orchdv1.RestartResponse, error) {
	return nil, f.err
}

func factory(c orchdv1.OrchdClient, dialErr error) clientFactory {
	return func() (orchdv1.OrchdClient, func(), error) {
		if dialErr != nil {
			return nil, nil, dialErr
		}
		return c, func() {}, nil
	}
}

func defaultAutoRAG() config.AutoRAGSettings {
	return config.AutoRAGSettings{Enabled: true, TopK: 3, MinScore: 0.25, MaxChars: 2000, MinPromptChars: 12}
}

func hitsResp(score float32) *orchdv1.SearchResponse {
	return &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
		{Text: "prior knowledge", SourceUri: "manual://x", SourceType: "manual", Score: score},
	}}
}

func TestUserPromptSubmit(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.AutoRAGSettings
		prompt     string
		client     *fakeClient
		dialErr    error
		wantInject bool
	}{
		{name: "question with strong hit injects", cfg: defaultAutoRAG(), prompt: "how did we handle retries?", client: &fakeClient{search: hitsResp(0.8)}, wantInject: true},
		{name: "non-question no inject", cfg: defaultAutoRAG(), prompt: "please refactor this now", client: &fakeClient{search: hitsResp(0.8)}, wantInject: false},
		{name: "too short no inject", cfg: defaultAutoRAG(), prompt: "hi?", client: &fakeClient{search: hitsResp(0.8)}, wantInject: false},
		{name: "weak hit filtered out", cfg: defaultAutoRAG(), prompt: "what is our policy?", client: &fakeClient{search: hitsResp(0.1)}, wantInject: false},
		{name: "disabled no inject", cfg: config.AutoRAGSettings{Enabled: false}, prompt: "what is our policy?", client: &fakeClient{search: hitsResp(0.9)}, wantInject: false},
		{name: "dial error degrades gracefully", cfg: defaultAutoRAG(), prompt: "what is our policy?", dialErr: errors.New("down"), wantInject: false},
		{name: "search error degrades gracefully", cfg: defaultAutoRAG(), prompt: "what is our policy?", client: &fakeClient{err: errors.New("boom")}, wantInject: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := userPromptSubmit(tt.cfg, Payload{Prompt: tt.prompt}, &out, factory(tt.client, tt.dialErr))
			if err != nil {
				t.Fatalf("handler must never error: %v", err)
			}
			injected := strings.Contains(out.String(), "Auto-injected RAG context")
			if injected != tt.wantInject {
				t.Errorf("injected = %v, want %v (out=%q)", injected, tt.wantInject, out.String())
			}
		})
	}
}

func TestLooksLikeQuestion(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   bool
	}{
		{name: "trailing question mark", prompt: "we good?", want: true},
		{name: "how prefix", prompt: "how do we do X", want: true},
		{name: "find phrase", prompt: "find the config for Y", want: true},
		{name: "remind me phrase", prompt: "remind me what we decided", want: true},
		{name: "imperative", prompt: "refactor the parser", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeQuestion(tt.prompt); got != tt.want {
				t.Errorf("looksLikeQuestion(%q) = %v, want %v", tt.prompt, got, tt.want)
			}
		})
	}
}

func TestFormatInjection(t *testing.T) {
	tests := []struct {
		name     string
		maxChars int
		hits     int
		wantOne  bool
	}{
		{name: "renders hits", maxChars: 2000, hits: 2, wantOne: true},
		{name: "tiny cap truncates body", maxChars: 10, hits: 2, wantOne: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits []*orchdv1.SearchHit
			for i := 0; i < tt.hits; i++ {
				hits = append(hits, &orchdv1.SearchHit{SourceUri: "manual://x", SourceType: "manual", Score: 0.9, Text: "some text"})
			}
			got := formatInjection(hits, tt.maxChars)
			has := strings.Contains(got, "manual://x")
			if has != tt.wantOne {
				t.Errorf("contains hit = %v, want %v", has, tt.wantOne)
			}
		})
	}
}

func TestDispatchUserPromptSubmit(t *testing.T) {
	tests := []struct {
		name       string
		prompt     string
		client     *fakeClient
		wantInject bool
	}{
		{name: "routes to auto-rag and injects", prompt: "what did we decide?", client: &fakeClient{search: hitsResp(0.9)}, wantInject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir()) // defaults: auto_rag enabled
			var out bytes.Buffer
			if err := dispatch("user-prompt-submit", Payload{Prompt: tt.prompt}, &out, factory(tt.client, nil)); err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if strings.Contains(out.String(), "Auto-injected RAG context") != tt.wantInject {
				t.Errorf("inject = %v, want %v", !tt.wantInject, tt.wantInject)
			}
		})
	}
}

func TestDispatchAndRun(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		event string
	}{
		{name: "empty args no-op", args: nil},
		{name: "unknown event no-op", args: []string{"nope"}},
		{name: "known no-op event", args: []string{"stop"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", t.TempDir())
			if err := Run(tt.args, strings.NewReader(`{"session_id":"s"}`), &bytes.Buffer{}); err != nil {
				t.Errorf("Run must never error: %v", err)
			}
		})
	}
}

func (f *fakeClient) ListSources(context.Context, *orchdv1.ListSourcesRequest, ...grpc.CallOption) (*orchdv1.ListSourcesResponse, error) {
	return f.listSources, f.err
}

func (f *fakeClient) DeleteSource(context.Context, *orchdv1.DeleteSourceRequest, ...grpc.CallOption) (*orchdv1.DeleteSourceResponse, error) {
	return f.deleteSource, f.err
}

func (f *fakeClient) ListTasks(context.Context, *orchdv1.ListTasksRequest, ...grpc.CallOption) (*orchdv1.ListTasksResponse, error) {
	return f.listTasks, f.err
}

func (f *fakeClient) GetTask(context.Context, *orchdv1.GetTaskRequest, ...grpc.CallOption) (*orchdv1.GetTaskResponse, error) {
	return f.getTask, f.err
}
