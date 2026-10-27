package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/willesq/orchestrator/internal/agentrun"
)

func TestRAGWritesRefusedInQueuedTask(t *testing.T) {
	tests := []struct {
		name string
		call func(ragTools) (*mcp.CallToolResult, error)
	}{
		{name: "ingest", call: func(rt ragTools) (*mcp.CallToolResult, error) {
			return rt.ingest(context.Background(), toolReq(map[string]any{"text": "trust me", "source_uri": "manual://x"}))
		}},
		{name: "delete", call: func(rt ragTools) (*mcp.CallToolResult, error) {
			return rt.deleteSource(context.Background(), toolReq(map[string]any{"source_uri": "manual://x", "confirm": true}))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(agentrun.TaskIDEnv, "task-1")
			// Any call through to the client fails the test.
			rt := ragTools{client: &fakeClient{err: errors.New("client must not be reached")}}
			res, err := tt.call(rt)
			if err != nil {
				t.Fatalf("handler returned go error: %v", err)
			}
			text := resultText(res)
			if !res.IsError || !strings.Contains(text, "cannot change the RAG store (running as task-1)") {
				t.Errorf("result = %q (error %v), want a queued-task refusal", text, res.IsError)
			}
			if strings.Contains(text, "client must not be reached") {
				t.Error("handler reached orchd despite the refusal")
			}
		})
	}
}

func TestQueuedWriteRefusalOutsideTask(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	if msg := queuedWriteRefusal(); msg != "" {
		t.Errorf("refusal outside a task: %q", msg)
	}
}
