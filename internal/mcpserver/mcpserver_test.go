package mcpserver

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeClient struct {
	listSources  *orchdv1.ListSourcesResponse
	deleteSource *orchdv1.DeleteSourceResponse
	listTasks    *orchdv1.ListTasksResponse
	getTask      *orchdv1.GetTaskResponse
	search       *orchdv1.SearchResponse
	ingest       *orchdv1.IngestResponse
	dispatch     *orchdv1.DispatchResponse
	dispatchReq  *orchdv1.DispatchRequest
	err          error
}

func (f *fakeClient) Ping(context.Context, *orchdv1.PingRequest, ...grpc.CallOption) (*orchdv1.PingResponse, error) {
	return nil, f.err
}

func (f *fakeClient) Ingest(context.Context, *orchdv1.IngestRequest, ...grpc.CallOption) (*orchdv1.IngestResponse, error) {
	return f.ingest, f.err
}

func (f *fakeClient) Search(context.Context, *orchdv1.SearchRequest, ...grpc.CallOption) (*orchdv1.SearchResponse, error) {
	return f.search, f.err
}

func (f *fakeClient) Dispatch(_ context.Context, req *orchdv1.DispatchRequest, _ ...grpc.CallOption) (*orchdv1.DispatchResponse, error) {
	f.dispatchReq = req
	return f.dispatch, f.err
}

func (f *fakeClient) Status(context.Context, *orchdv1.StatusRequest, ...grpc.CallOption) (*orchdv1.StatusResponse, error) {
	return nil, f.err
}

func (f *fakeClient) Restart(context.Context, *orchdv1.RestartRequest, ...grpc.CallOption) (*orchdv1.RestartResponse, error) {
	return nil, f.err
}

func toolReq(args map[string]any) mcp.CallToolRequest {
	var r mcp.CallToolRequest
	r.Params.Arguments = args
	return r
}

func resultText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestRAGSearch(t *testing.T) {
	tests := []struct {
		name         string
		args         map[string]any
		client       *fakeClient
		wantErr      bool
		wantContains string
	}{
		{
			name:         "missing query is an error result",
			args:         map[string]any{},
			client:       &fakeClient{},
			wantErr:      true,
			wantContains: "query is required",
		},
		{
			name: "success formats hits",
			args: map[string]any{"query": "hi", "top_k": float64(2)},
			client: &fakeClient{search: &orchdv1.SearchResponse{Hits: []*orchdv1.SearchHit{
				{Text: "t", SourceUri: "manual://x", Score: 0.9},
			}}},
			wantContains: "manual://x",
		},
		{
			name:         "client error is an error result",
			args:         map[string]any{"query": "hi"},
			client:       &fakeClient{err: errors.New("boom")},
			wantErr:      true,
			wantContains: "search failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := ragTools{client: tt.client}
			res, err := rt.search(context.Background(), toolReq(tt.args))
			if err != nil {
				t.Fatalf("handler returned go error: %v", err)
			}
			if res.IsError != tt.wantErr {
				t.Errorf("IsError = %v, want %v", res.IsError, tt.wantErr)
			}
			if !strings.Contains(resultText(res), tt.wantContains) {
				t.Errorf("result %q missing %q", resultText(res), tt.wantContains)
			}
		})
	}
}

func TestRAGIngest(t *testing.T) {
	tests := []struct {
		name         string
		args         map[string]any
		client       *fakeClient
		wantErr      bool
		wantContains string
	}{
		{name: "missing fields", args: map[string]any{"text": "x"}, client: &fakeClient{}, wantErr: true, wantContains: "required"},
		{
			name:         "success",
			args:         map[string]any{"text": "hello", "source_uri": "manual://x"},
			client:       &fakeClient{ingest: &orchdv1.IngestResponse{Chunks: 3}},
			wantContains: "ingested 3 chunk(s)",
		},
		{
			name:         "client error",
			args:         map[string]any{"text": "hello", "source_uri": "manual://x"},
			client:       &fakeClient{err: errors.New("boom")},
			wantErr:      true,
			wantContains: "ingest failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := ragTools{client: tt.client}
			res, err := rt.ingest(context.Background(), toolReq(tt.args))
			if err != nil {
				t.Fatalf("handler go error: %v", err)
			}
			if res.IsError != tt.wantErr {
				t.Errorf("IsError = %v, want %v", res.IsError, tt.wantErr)
			}
			if !strings.Contains(resultText(res), tt.wantContains) {
				t.Errorf("result %q missing %q", resultText(res), tt.wantContains)
			}
		})
	}
}

func TestQueueDispatch(t *testing.T) {
	wd, _ := os.Getwd()
	tests := []struct {
		name         string
		args         map[string]any
		client       *fakeClient
		wantErr      bool
		wantContains string
		wantCwd      string
		taskID       string
		wantNoCall   bool
	}{
		{name: "missing body", args: map[string]any{}, client: &fakeClient{}, wantErr: true, wantContains: "body is required"},
		{
			name:         "success defaults cwd to the session directory",
			args:         map[string]any{"body": "fix it"},
			client:       &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-1", WorkerEnabled: true}},
			wantContains: "task-1",
			wantCwd:      wd,
		},
		{
			name:         "explicit cwd passes through",
			args:         map[string]any{"body": "fix it", "cwd": "/src/widget"},
			client:       &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-1", WorkerEnabled: true}},
			wantContains: "runs in /src/widget",
			wantCwd:      "/src/widget",
		},
		{
			name:         "disabled worker is called out",
			args:         map[string]any{"body": "fix it"},
			client:       &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-1"}},
			wantContains: "worker is disabled",
		},
		{
			name:         "a queued task cannot dispatch more tasks",
			args:         map[string]any{"body": "fix it"},
			taskID:       "task-parent",
			client:       &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-1", WorkerEnabled: true}},
			wantErr:      true,
			wantContains: "cannot dispatch further tasks",
			wantNoCall:   true,
		},
		{
			name:         "client error",
			args:         map[string]any{"body": "fix it"},
			client:       &fakeClient{err: errors.New("boom")},
			wantErr:      true,
			wantContains: "dispatch failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(agentrun.TaskIDEnv, tt.taskID)
			qt := queueTools{client: tt.client}
			res, err := qt.dispatch(context.Background(), toolReq(tt.args))
			if tt.wantNoCall && tt.client.dispatchReq != nil {
				t.Errorf("orchd was called despite the refusal")
			}
			if tt.wantCwd != "" && tt.client.dispatchReq.GetCwd() != tt.wantCwd {
				t.Errorf("cwd = %q, want %q", tt.client.dispatchReq.GetCwd(), tt.wantCwd)
			}
			if err != nil {
				t.Fatalf("handler go error: %v", err)
			}
			if res.IsError != tt.wantErr {
				t.Errorf("IsError = %v, want %v", res.IsError, tt.wantErr)
			}
			if !strings.Contains(resultText(res), tt.wantContains) {
				t.Errorf("result %q missing %q", resultText(res), tt.wantContains)
			}
		})
	}
}

func TestGetInt(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		def  int32
		want int32
	}{
		{name: "float64 json number", args: map[string]any{"k": float64(7)}, def: 5, want: 7},
		{name: "int value", args: map[string]any{"k": 9}, def: 5, want: 9},
		{name: "missing uses default", args: map[string]any{}, def: 5, want: 5},
		{name: "wrong type uses default", args: map[string]any{"k": "nope"}, def: 5, want: 5},
		// Bounded so the int32 narrowing downstream cannot wrap — a wrapped 0
		// reads as "no limit" on some paths, which is the opposite of intent.
		{name: "huge float is clamped", args: map[string]any{"k": float64(1e18)}, def: 5, want: maxArgInt},
		{name: "huge int is clamped", args: map[string]any{"k": 1 << 40}, def: 5, want: maxArgInt},
		{name: "negative falls back to the default", args: map[string]any{"k": float64(-3)}, def: 5, want: 5},
		{name: "negative int falls back", args: map[string]any{"k": -3}, def: 5, want: 5},
		{name: "NaN falls back", args: map[string]any{"k": math.NaN()}, def: 5, want: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getInt(toolReq(tt.args), "k", tt.def); got != tt.want {
				t.Errorf("getInt = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatHits(t *testing.T) {
	tests := []struct {
		name         string
		hits         []*orchdv1.SearchHit
		wantContains string
	}{
		{name: "empty", hits: nil, wantContains: "no results"},
		{name: "one hit", hits: []*orchdv1.SearchHit{{SourceUri: "manual://x", Score: 0.5, Text: "t"}}, wantContains: "manual://x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatHits(tt.hits); !strings.Contains(got, tt.wantContains) {
				t.Errorf("formatHits = %q, missing %q", got, tt.wantContains)
			}
		})
	}
}

func TestServersConstruct(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "rag and queue servers build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if NewRAG(&fakeClient{}) == nil || NewQueue(&fakeClient{}) == nil {
				t.Errorf("nil MCP server")
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

func (f *fakeClient) SummarizeSession(context.Context, *orchdv1.SummarizeSessionRequest, ...grpc.CallOption) (*orchdv1.SummarizeSessionResponse, error) {
	return &orchdv1.SummarizeSessionResponse{}, f.err
}
