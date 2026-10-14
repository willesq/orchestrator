package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestQueueDispatchRouting(t *testing.T) {
	t.Setenv(agentrun.TaskIDEnv, "")
	client := &fakeClient{dispatch: &orchdv1.DispatchResponse{InstanceId: "task-1", WorkerEnabled: true,
		Route: &orchdv1.Route{TaskKind: "code", Provider: "claude-code", Model: "opus", Confidence: "low"}}}
	res, err := queueTools{client: client}.dispatch(context.Background(),
		toolReq(map[string]any{"body": "fix it", "task_kind": "code", "model": "claude-code/opus"}))
	if err != nil || res.IsError {
		t.Fatalf("res = %v, %v", resultText(res), err)
	}
	if client.dispatchReq.GetTaskKind() != "code" || client.dispatchReq.GetModel() != "claude-code/opus" {
		t.Fatalf("request = %+v", client.dispatchReq)
	}
	if !strings.Contains(resultText(res), "route: code → claude-code/opus") {
		t.Fatalf("result = %q", resultText(res))
	}
}

func TestModelRecommend(t *testing.T) {
	orig := fakeRecommend
	t.Cleanup(func() { fakeRecommend = orig })
	tests := []struct {
		name      string
		args      map[string]any
		resp      *orchdv1.RecommendModelResponse
		err       error
		wantErr   bool
		wantParts []string
	}{
		{name: "missing kind", args: map[string]any{}, wantErr: true, wantParts: []string{"task_kind is required"}},
		{name: "from table", args: map[string]any{"task_kind": "code"},
			resp: &orchdv1.RecommendModelResponse{FromTable: true, Route: &orchdv1.Route{TaskKind: "code", Provider: "claude-code",
				Model: "opus", Confidence: "low", Rationale: "declared frontier"}},
			wantParts: []string{"provider: claude-code", "model: opus", "effort: model default", "confidence: low", "evidence_n: 0", "source: routing table", "declared frontier"}},
		{name: "live proposal", args: map[string]any{"task_kind": "code"},
			resp:      &orchdv1.RecommendModelResponse{Route: &orchdv1.Route{Provider: "codex", Model: "gpt-sol", Effort: "high"}},
			wantParts: []string{"effort: high", "live proposal"}},
		{name: "error", args: map[string]any{"task_kind": "code"}, err: errors.New("no route"), wantErr: true, wantParts: []string{"recommend failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *orchdv1.RecommendModelRequest
			fakeRecommend = func(req *orchdv1.RecommendModelRequest) (*orchdv1.RecommendModelResponse, error) {
				got = req
				return tt.resp, tt.err
			}
			res, err := queueTools{client: &fakeClient{}}.recommend(context.Background(), toolReq(tt.args))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError != tt.wantErr {
				t.Fatalf("IsError = %v: %s", res.IsError, resultText(res))
			}
			for _, p := range tt.wantParts {
				if !strings.Contains(resultText(res), p) {
					t.Errorf("missing %q in %q", p, resultText(res))
				}
			}
			if k, _ := tt.args["task_kind"].(string); k != "" && got.GetTaskKind() != k {
				t.Errorf("request kind = %q", got.GetTaskKind())
			}
		})
	}
}

func TestQueueServerExposesRoutingTools(t *testing.T) {
	s := NewQueue(&fakeClient{})
	for _, name := range []string{"queue_dispatch", "queue_list", "queue_read", "model_recommend"} {
		if s.GetTool(name) == nil {
			t.Errorf("queue server lacks %s", name)
		}
	}
	props := s.GetTool("queue_dispatch").Tool.InputSchema.Properties
	for _, p := range []string{"task_kind", "model"} {
		if _, ok := props[p]; !ok {
			t.Errorf("queue_dispatch lacks %s", p)
		}
	}
}
