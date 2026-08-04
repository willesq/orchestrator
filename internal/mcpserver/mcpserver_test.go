package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeClient struct {
	search   *orchdv1.SearchResponse
	ingest   *orchdv1.IngestResponse
	dispatch *orchdv1.DispatchResponse
	err      error
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

func (f *fakeClient) Dispatch(context.Context, *orchdv1.DispatchRequest, ...grpc.CallOption) (*orchdv1.DispatchResponse, error) {
	return f.dispatch, f.err
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
	tests := []struct {
		name         string
		args         map[string]any
		client       *fakeClient
		wantErr      bool
		wantContains string
	}{
		{name: "missing body", args: map[string]any{}, client: &fakeClient{}, wantErr: true, wantContains: "body is required"},
		{
			name:         "success",
			args:         map[string]any{"body": "fix it"},
			client:       &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-1"}},
			wantContains: "task-1",
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
			qt := queueTools{client: tt.client}
			res, err := qt.dispatch(context.Background(), toolReq(tt.args))
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
		def  int
		want int
	}{
		{name: "float64 json number", args: map[string]any{"k": float64(7)}, def: 5, want: 7},
		{name: "int value", args: map[string]any{"k": 9}, def: 5, want: 9},
		{name: "missing uses default", args: map[string]any{}, def: 5, want: 5},
		{name: "wrong type uses default", args: map[string]any{"k": "nope"}, def: 5, want: 5},
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
