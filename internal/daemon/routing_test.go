package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/models"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/routing"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/workflows"
)

type fakeRouter struct {
	rec      routing.Recommendation
	err      error
	runnable *func(string) bool
}

func (f fakeRouter) Recommend(_ context.Context, kind string, runnable func(string) bool) (routing.Recommendation, error) {
	if f.runnable != nil {
		*f.runnable = runnable
	}
	rec := f.rec
	rec.Kind = kind
	return rec, f.err
}

func (f fakeRouter) Pin(_ context.Context, ref string, runnable func(string) bool) (routing.Recommendation, error) {
	if f.runnable != nil {
		*f.runnable = runnable
	}
	if f.err != nil {
		return routing.Recommendation{}, f.err
	}
	p, s, err := models.ParseRef(ref)
	return routing.Recommendation{Route: routing.Route{Provider: p, Model: s, Rationale: "chosen by the caller"}, Confidence: "low"}, err
}

func TestDispatchRouting(t *testing.T) {
	opus := routing.Recommendation{Route: routing.Route{Provider: "claude-code", Model: "opus", Rationale: "declared"}, Confidence: "low", FromTable: true}
	tests := []struct {
		name      string
		req       *orchdv1.DispatchRequest
		router    modelRouter
		wantCode  codes.Code
		wantRoute workflows.Route
		wantPB    bool
	}{
		{name: "unrouted", req: &orchdv1.DispatchRequest{Body: "x"}, router: fakeRouter{rec: opus}},
		{name: "kind routes", req: &orchdv1.DispatchRequest{Body: "x", TaskKind: "code"}, router: fakeRouter{rec: opus},
			wantRoute: workflows.Route{Kind: "code", Provider: "claude-code", Model: "opus"}, wantPB: true},
		{name: "model wins over kind", req: &orchdv1.DispatchRequest{Body: "x", TaskKind: "docs", Model: "claude-code/sonnet"}, router: fakeRouter{rec: opus},
			wantRoute: workflows.Route{Kind: "docs", Provider: "claude-code", Model: "sonnet"}, wantPB: true},
		{name: "no routable model runs unrouted", req: &orchdv1.DispatchRequest{Body: "x", TaskKind: "code"},
			router: fakeRouter{err: routing.ErrNoRoute}, wantRoute: workflows.Route{Kind: "code"}, wantPB: true},
		{name: "unknown kind", req: &orchdv1.DispatchRequest{Body: "x", TaskKind: "poetry"}, router: fakeRouter{}, wantCode: codes.InvalidArgument},
		{name: "unroutable pin", req: &orchdv1.DispatchRequest{Body: "x", Model: "codex/gpt"}, router: fakeRouter{err: errors.New("not routable")}, wantCode: codes.InvalidArgument},
		{name: "router failure", req: &orchdv1.DispatchRequest{Body: "x", TaskKind: "code"}, router: fakeRouter{err: errors.New("db gone")}, wantCode: codes.Unknown},
		{name: "routing off", req: &orchdv1.DispatchRequest{Body: "x", TaskKind: "code"}, wantCode: codes.FailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rt workflows.Route
			var runnable func(string) bool
			if fr, ok := tt.router.(fakeRouter); ok {
				fr.runnable = &runnable
				tt.router = fr
			}
			s := &server{start: time.Now(), disp: fakeDispatcher{id: "task-1", route: &rt}, router: tt.router,
				routerErr: errors.New("bad policy")}
			resp, err := s.Dispatch(context.Background(), tt.req)
			if tt.wantCode != codes.OK {
				if grpcstatus.Code(err) != tt.wantCode {
					t.Fatalf("err = %v, want %v", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if rt != tt.wantRoute {
				t.Fatalf("dispatched route = %+v, want %+v", rt, tt.wantRoute)
			}
			if (resp.GetRoute() != nil) != tt.wantPB {
				t.Fatalf("response route = %v", resp.GetRoute())
			}
			if tt.wantPB && resp.GetRoute().GetTaskKind() != tt.req.GetTaskKind() {
				t.Fatalf("response kind = %q", resp.GetRoute().GetTaskKind())
			}
			if runnable != nil && (runnable("codex") || !runnable("claude-code")) {
				t.Fatal("dispatch must route only to providers the worker runs")
			}
		})
	}
}

func TestRecommendModel(t *testing.T) {
	opus := routing.Recommendation{Route: routing.Route{Provider: "claude-code", Model: "opus", EvidenceN: 3}, Confidence: "low", FromTable: true}
	tests := []struct {
		name     string
		kind     string
		router   modelRouter
		wantCode codes.Code
	}{
		{name: "answers", kind: "code", router: fakeRouter{rec: opus}},
		{name: "unknown kind", kind: "poetry", router: fakeRouter{rec: opus}, wantCode: codes.InvalidArgument},
		{name: "no route", kind: "code", router: fakeRouter{err: routing.ErrNoRoute}, wantCode: codes.NotFound},
		{name: "failure", kind: "code", router: fakeRouter{err: errors.New("db gone")}, wantCode: codes.Unknown},
		{name: "routing off", kind: "code", wantCode: codes.FailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{router: tt.router, routerErr: errors.New("bad policy")}
			resp, err := s.RecommendModel(context.Background(), &orchdv1.RecommendModelRequest{TaskKind: tt.kind})
			if tt.wantCode != codes.OK {
				if grpcstatus.Code(err) != tt.wantCode {
					t.Fatalf("err = %v, want %v", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := resp.GetRoute()
			if r.GetModel() != "opus" || r.GetTaskKind() != "code" || r.GetEvidenceN() != 3 || !resp.GetFromTable() {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}
}

func TestNewRouter(t *testing.T) {
	if _, err := newRouter(nil, routing.Policy{Ranks: 0}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	r, err := newRouter(nil, routing.Policy{Ranks: 1})
	if err != nil || r == nil {
		t.Fatalf("router = %v, %v", r, err)
	}
}

func TestAgentTaskRoutesOnlyClaudeModels(t *testing.T) {
	tests := []struct {
		route workflows.Route
		want  string
	}{
		{workflows.Route{Kind: "code", Provider: "claude-code", Model: "opus"}, "opus"},
		{workflows.Route{Kind: "code", Provider: "codex", Model: "gpt-sol"}, ""},
		{workflows.Route{Kind: "code"}, ""},
	}
	for _, tt := range tests {
		if got := agentTask(workflows.AgentTaskInput{ID: "t", Agent: "dev", Route: tt.route}).Model; got != tt.want {
			t.Errorf("route %+v → model %q, want %q", tt.route, got, tt.want)
		}
	}
}

func TestTaskRoutePB(t *testing.T) {
	if taskRoutePB(queueTask("", "")) != nil {
		t.Fatal("unrouted task should have no route")
	}
	if r := taskRoutePB(queueTask("code", "opus")); r.GetTaskKind() != "code" || r.GetModel() != "opus" {
		t.Fatalf("route = %+v", r)
	}
}

func queueTask(kind, model string) queue.Task {
	return queue.Task{ID: "t", Kind: kind, Provider: "claude-code", Model: model}
}

func TestTaskRunnerRechecksRoutedModel(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	reg := models.NewRegistry(conn)
	ctx := context.Background()
	if err := reg.RecordDiscovery(ctx, models.Provider{Name: "claude-code", State: models.StateEnabled}, models.StateEnabled, []models.Model{
		{Slug: "opus", Source: "alias", State: models.StateEnabled},
		{Slug: "sonnet", Source: "alias", State: models.StateEnabled},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetModelState(ctx, "claude-code", "sonnet", models.StateDisabled); err != nil {
		t.Fatal(err)
	}
	in := func(model string) workflows.AgentTaskInput {
		return workflows.AgentTaskInput{ID: "t", Agent: "dev", Route: workflows.Route{Kind: "code", Provider: "claude-code", Model: model}}
	}
	tr := taskRunner{models: reg}
	for model, want := range map[string]string{"opus": "opus", "sonnet": "", "gone": "", "": ""} {
		if got := tr.task(ctx, in(model)).Model; got != want {
			t.Errorf("routed %q → %q, want %q", model, got, want)
		}
	}
	if got := (taskRunner{}).task(ctx, in("sonnet")).Model; got != "sonnet" {
		t.Errorf("without a registry the route passes through, got %q", got)
	}
}
